package runner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/szhekpisov/gomutants/internal/coverage"
	"github.com/szhekpisov/gomutants/internal/mutator"
	"github.com/szhekpisov/gomutants/internal/schemata"
)

// ResultCallback is called for each completed mutant.
type ResultCallback func(m mutator.Mutant)

// ExecOpts bundles the inner `go test` knobs forwarded to each worker:
// TestCPU (0 omits -cpu, letting go test default to GOMAXPROCS), Tags
// (empty omits -tags), and TestFlags (user-supplied flags appended
// verbatim; nil appends nothing). Grouped into one struct so NewPool stays
// within a sane parameter count.
type ExecOpts struct {
	TestCPU   int
	Tags      string
	TestFlags []string

	// Schemata, when non-nil, is the plan naming the mutants compiled into
	// prebuilt test binaries. Nil means every mutant takes the per-mutant
	// overlay path, which is the default.
	Schemata *schemata.Plan
}

// Pool coordinates parallel mutation testing.
type Pool struct {
	workers    int
	exec       ExecOpts
	policy     TimeoutPolicy
	tmpDir     string
	srcCache   map[string][]byte
	projectDir string
	testMap    *coverage.TestMap
}

// childGOMAXPROCSFor returns the GOMAXPROCS cap for each child `go test`
// when running with the given outer worker count. Goal: spread NumCPU cores
// across workers so the host doesn't oversubscribe (50 threads on a 10-core
// machine when each worker spawns a parallel-compiling subprocess).
// Returns 0 (= inherit) when there's only one worker — no contention to fix.
func childGOMAXPROCSFor(workers int) int {
	if workers <= 1 {
		return 0
	}
	return max(1, runtime.NumCPU()/workers)
}

// NewPool creates a worker pool. See ExecOpts for the inner `go test`
// knobs (TestCPU, Tags).
func NewPool(workers int, exec ExecOpts, policy TimeoutPolicy, tmpDir string, srcCache map[string][]byte, projectDir string, testMap *coverage.TestMap) *Pool {
	return &Pool{
		workers:    workers,
		exec:       exec,
		policy:     policy,
		tmpDir:     tmpDir,
		srcCache:   srcCache,
		projectDir: projectDir,
		testMap:    testMap,
	}
}

// Run tests all pending mutants in parallel, calling onResult for each
// completion. Mutant statuses are updated in place on the input slice.
func (p *Pool) Run(ctx context.Context, mutants []mutator.Mutant, onResult ResultCallback) {
	// Filter to only pending mutants.
	var pending []int
	for i, m := range mutants {
		if m.Status == mutator.StatusPending {
			pending = append(pending, i)
		}
	}

	if len(pending) == 0 {
		return
	}

	// EXP4: sort pending mutants by (Pkg, File, StartOffset) so consecutive
	// mutants on the work channel target the same package + file. The first
	// mutant in a package pays the cold compile; subsequent ones in that
	// package hit the build cache for deps + stdlib.
	sort.SliceStable(pending, func(a, b int) bool {
		return mutantLess(mutants[pending[a]], mutants[pending[b]])
	})

	work := make(chan int, len(pending))
	results := make(chan mutator.Mutant, len(pending))

	workers := p.createWorkers()

	// If no worker could be created, abort cleanly rather than deadlocking
	// on a feeder blocked forever sending into `work` with no readers.
	if len(workers) == 0 {
		fmt.Fprintln(os.Stderr, "gomutants: no workers could be started; no mutant could be tested")
		failPending(mutants, pending, onResult)
		return
	}

	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Add(1)
		go func(w *Worker) {
			defer wg.Done()
			for idx := range work {
				if ctx.Err() != nil {
					return
				}
				result := w.Test(ctx, mutants[idx])
				results <- result
			}
		}(w)
	}

	// Feed work.
	go func() {
		defer close(work)
		for _, idx := range pending {
			select {
			case work <- idx:
			case <-ctx.Done():
				return
			}
		}
	}()

	// Collect results.
	go func() {
		wg.Wait()
		close(results)
	}()

	// Build ID → index lookup so we don't rely on IDs being a dense
	// 1-based contiguous range. Future filter steps that drop mutants
	// before Pool won't silently corrupt results with an off-by-one.
	idToIdx := make(map[int]int, len(mutants))
	for i, m := range mutants {
		idToIdx[m.ID] = i
	}

	for result := range results {
		if idx, ok := idToIdx[result.ID]; ok {
			mutants[idx] = result
		}
		if onResult != nil {
			onResult(result)
		}
	}
}

// failPending marks every still-pending mutant as an infrastructure error and
// reports it through onResult, for the case where the run could not start at
// all. Leaving them Pending is what makes a dead host look like a clean run:
// Pending falls into no bucket in the report, so the count is invisible, the
// stderr warning never fires, and the thresholds are evaluated over whatever
// the cache happened to supply — a green gate on a run that tested nothing.
//
// InfraError rather than NotViable because the mutants themselves were never
// in question: gomutants could not stage its own scratch files. NotViable is
// cacheable and would freeze that verdict into the next run.
func failPending(mutants []mutator.Mutant, pending []int, onResult ResultCallback) {
	for _, idx := range pending {
		mutants[idx].Status = mutator.StatusInfraError
		if onResult != nil {
			onResult(mutants[idx])
		}
	}
}

// createWorkers spins up p.workers workers, logs and skips ones that fail
// to construct, and applies the per-worker GOMAXPROCS cap. Extracted from
// Run so the loop can be unit-tested directly: with a stub
// newWorkerFunc, the test sees how many workers were created and what
// childGOMAXPROCS each ended up with — neither of which is observable
// through the pool's mutant return value.
func (p *Pool) createWorkers() []*Worker {
	workers := make([]*Worker, 0, p.workers)
	cap := childGOMAXPROCSFor(p.workers)
	for i := range p.workers {
		w, err := newWorkerFunc(i, p.tmpDir, p.policy, p.srcCache, p.projectDir, p.testMap)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gomutants: NewWorker %d failed: %v\n", i, err)
			continue
		}
		w.childGOMAXPROCS = cap
		w.testCPU = p.exec.TestCPU
		w.tags = p.exec.Tags
		w.testFlags = p.exec.TestFlags
		w.schemata = p.exec.Schemata
		workers = append(workers, w)
	}
	return workers
}

// newWorkerFunc is the constructor used by createWorkers; swappable for tests.
var newWorkerFunc = NewWorker

// mutantLess orders mutants by (Pkg, File, StartOffset) using paired `<`/`>`
// comparisons so each `<` mutation flips behavior on the equality path. A
// guarded `if a != b { return a < b }` would render `<` ↔ `<=` mutations
// equivalent because the guard rules out the only case where the boundary
// matters.
func mutantLess(a, b mutator.Mutant) bool {
	if a.Pkg < b.Pkg {
		return true
	}
	if a.Pkg > b.Pkg {
		return false
	}
	if a.File < b.File {
		return true
	}
	if a.File > b.File {
		return false
	}
	return a.StartOffset < b.StartOffset
}

// MeasureBaseline runs the test suite once to determine baseline duration.
// testFlags are the user's --test-flags, forwarded here too so the baseline
// measures the same work the per-mutant runs will do — otherwise a
// `-short`-ed mutation run would be sized against a full-cost baseline.
//
// They land *after* the package arguments. `go test` keeps parsing its
// own flags past one it does not recognize, but that first unrecognized
// flag marks the package list as already seen, so any package name after
// it is no longer read as a package — it is forwarded to the test binary
// as a positional argument and `go test` falls back to `.`. A test-binary
// flag (`-rapid.checks=N`) ahead of the packages therefore measures the
// working directory instead of them. See buildTestArgs for the same
// ordering on the mutant runs.
func MeasureBaseline(ctx context.Context, projectDir string, packages []string, tags string, testFlags []string) (time.Duration, error) {
	args := []string{"test", "-count=1"}
	if tags != "" {
		args = append(args, "-tags="+tags)
	}
	args = append(args, packages...)
	args = append(args, testFlags...)
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = projectDir

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = nil

	start := time.Now()
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("baseline test failed: %w\n%s", err, stderr.String())
	}
	return time.Since(start), nil
}

// RunCoverage runs go test with coverage and returns the profile path.
// testFlags are the user's --test-flags. Forwarding them here keeps the
// profile consistent with what the per-mutant runs execute: a test that
// `-short` skips must not be recorded as covering, or every mutant on its
// lines would be reported as a survivor instead of NOT_COVERED.
//
// As in MeasureBaseline, the flags land after the package arguments so
// that an unrecognized one cannot demote them to positional arguments for
// the test binary — which here would profile the working directory and
// report every mutant NOT_COVERED.
func RunCoverage(ctx context.Context, projectDir string, packages []string, coverPkg, tags string, tmpDir string, testFlags []string) (string, error) {
	profilePath := filepath.Join(tmpDir, "coverage.out")

	args := []string{"test", "-count=1", "-coverprofile=" + profilePath}
	if coverPkg != "" {
		args = append(args, "-coverpkg="+coverPkg)
	}
	if tags != "" {
		args = append(args, "-tags="+tags)
	}
	args = append(args, packages...)
	args = append(args, testFlags...)

	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = projectDir

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = os.Stderr // Show test output.

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("coverage run failed: %w\n%s", err, stderr.String())
	}

	return profilePath, nil
}
