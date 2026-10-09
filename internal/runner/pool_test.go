package runner

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gomutants/gomutants/internal/coverage"
	"github.com/gomutants/gomutants/internal/mutator"
)

// runWithDeadline runs fn in a goroutine and fails the test if it doesn't
// return within d. Catches mutations that turn the result-collection loop
// into a hang (e.g. dropping `close(results)`), classifying them as KILLED
// instead of letting them ride out the per-mutant TIMED OUT timeout.
func runWithDeadline(t *testing.T, d time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("deadlocked: function exceeded %s", d)
	}
}

// setupTestProject creates a minimal Go project in a temp directory.
func setupTestProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	goMod := "module testmod\n\ngo 1.26\n"
	src := "package testmod\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n"
	testSrc := "package testmod\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"wrong\")\n\t}\n}\n"

	for name, content := range map[string]string{
		"go.mod":      goMod,
		"add.go":      src,
		"add_test.go": testSrc,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestChildGOMAXPROCSFor covers each branch of the helper, locking down
// CONDITIONALS_BOUNDARY (`<=` ↔ `<`), CONDITIONALS_NEGATION, the BRANCH_IF
// on the early-return body, and ARITHMETIC_BASE on `NumCPU()/workers`.
func TestChildGOMAXPROCSFor(t *testing.T) {
	tests := []struct {
		workers  int
		want     int
		wantPass func(int) bool
	}{
		// workers <= 1 must return 0 (inherit). The BRANCH_IF on the early
		// return is the only thing keeping workers=0 from triggering a
		// divide-by-zero in `NumCPU/workers`.
		{0, 0, nil},
		{1, 0, nil},
	}
	for _, tt := range tests {
		got := childGOMAXPROCSFor(tt.workers)
		if got != tt.want {
			t.Errorf("childGOMAXPROCSFor(%d) = %d, want %d", tt.workers, got, tt.want)
		}
	}

	// workers >= 2: result must be ≤ NumCPU. Mutating `/` to `*` blows past
	// NumCPU even for small worker counts. Mutating `<=` to `<` returns
	// max(1, NumCPU) when workers==1, which is also > NumCPU/2 here.
	if got := childGOMAXPROCSFor(2); got > runtime.NumCPU() {
		t.Errorf("childGOMAXPROCSFor(2) = %d, must be ≤ NumCPU=%d (ARITHMETIC_BASE on / would inflate it)",
			got, runtime.NumCPU())
	}
	// At least 1 — the max(1, ...) clamp is also load-bearing on
	// single-CPU CI hosts.
	if got := childGOMAXPROCSFor(8); got < 1 {
		t.Errorf("childGOMAXPROCSFor(8) = %d, must be ≥ 1", got)
	}
}

func TestPoolRunNoPending(t *testing.T) {
	p := NewPool(2, ExecOpts{}, TimeoutPolicy{Global: 30 * time.Second}, t.TempDir(), nil, ".", nil)
	mutants := []mutator.Mutant{
		{ID: 1, Status: mutator.StatusNotCovered},
		{ID: 2, Status: mutator.StatusKilled},
	}

	p.Run(context.Background(), mutants, nil)
	if len(mutants) != 2 {
		t.Fatalf("expected 2 mutants returned, got %d", len(mutants))
	}
	if mutants[0].Status != mutator.StatusNotCovered {
		t.Errorf("mutant 1 status=%v, want NOT_COVERED", mutants[0].Status)
	}
	if mutants[1].Status != mutator.StatusKilled {
		t.Errorf("mutant 2 status=%v, want KILLED", mutants[1].Status)
	}
}

// TestPoolRunNoPendingDoesNotCreateWorkers kills BRANCH_IF on the
// `if len(pending) == 0 { return mutants }` body. Without the early return,
// Run would still construct workers (NewWorker writes worker-N.go and
// overlay-N.json into tmpDir) before falling through to the empty results
// loop and producing the same final mutant slice. Asserting the tmpDir is
// untouched is the only observable handle we have on the early-return.
func TestPoolRunNoPendingDoesNotCreateWorkers(t *testing.T) {
	tmpDir := t.TempDir()
	p := NewPool(2, ExecOpts{}, TimeoutPolicy{Global: time.Second}, tmpDir, nil, ".", nil)
	mutants := []mutator.Mutant{
		{ID: 1, Status: mutator.StatusKilled},
	}
	p.Run(context.Background(), mutants, nil)
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("tmpDir has %d entries after empty-pending Run; expected 0 — early-return on len(pending)==0 was elided, workers were created", len(entries))
	}
}

// TestMutantLessSortStability covers the (pkg, file, offset) ordering
// directly. Using paired `<`/`>` checks (no `!=` guard) — see the
// candidateLess refactor in internal/discover — keeps `<` ↔ `<=` mutations
// observable on equal-key inputs.
func TestMutantLessSortStability(t *testing.T) {
	mk := func(pkg, file string, off int) mutator.Mutant {
		return mutator.Mutant{Pkg: pkg, File: file, StartOffset: off}
	}
	cases := []struct {
		name string
		a, b mutator.Mutant
		want bool
	}{
		{"pkg less", mk("a", "f", 0), mk("b", "f", 0), true},
		{"pkg greater", mk("b", "f", 0), mk("a", "f", 0), false},
		{"eqPkg fileLt", mk("p", "a", 0), mk("p", "b", 0), true},
		{"eqPkg fileGt", mk("p", "b", 0), mk("p", "a", 0), false},
		{"eqFile offLt", mk("p", "f", 1), mk("p", "f", 9), true},
		{"eqFile offGt", mk("p", "f", 9), mk("p", "f", 1), false},
		// Equal across the board — < returns false (not less than itself).
		{"all equal", mk("p", "f", 5), mk("p", "f", 5), false},
		// Pkg `>` decisive: BRANCH_IF on its `{ return false }` body falls
		// through to file-`<` and returns true here. Original returns false.
		{"aPkgGt bFileLt", mk("z", "a", 0), mk("a", "z", 0), false},
		{"eqPkg aFileGt bOffLt", mk("p", "z", 0), mk("p", "a", 99), false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := mutantLess(tt.a, tt.b); got != tt.want {
				t.Errorf("mutantLess = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestPoolCreateWorkersAppliesGOMAXPROCS kills STATEMENT_REMOVE on
// `w.childGOMAXPROCS = childGOMAXPROCSFor(p.workers)`. With the assignment
// elided, every worker is created with childGOMAXPROCS=0 (the zero value),
// so the GOMAXPROCS env override never gets propagated to inner go test.
func TestPoolCreateWorkersAppliesGOMAXPROCS(t *testing.T) {
	p := NewPool(4, ExecOpts{}, TimeoutPolicy{Global: time.Second}, t.TempDir(), nil, ".", nil)
	workers := p.createWorkers()
	if len(workers) == 0 {
		t.Fatal("expected workers; createWorkers returned none")
	}
	want := childGOMAXPROCSFor(4)
	for i, w := range workers {
		if w.childGOMAXPROCS != want {
			t.Errorf("worker[%d].childGOMAXPROCS = %d, want %d (STATEMENT_REMOVE drops the assignment)",
				i, w.childGOMAXPROCS, want)
		}
	}
}

// TestPoolCreateWorkersPropagatesExecOpts kills STATEMENT_REMOVE on the
// `w.testCPU = p.exec.TestCPU`, `w.tags = p.exec.Tags`, and
// `w.testFlags = p.exec.TestFlags` assignments in createWorkers. With any
// of them elided the worker keeps the zero value, so the configured
// --test-cpu / --tags / --test-flags never reaches the per-mutant
// `go test` even though the pool was given a value.
func TestPoolCreateWorkersPropagatesExecOpts(t *testing.T) {
	p := NewPool(2, ExecOpts{TestCPU: 3, Tags: "integration", TestFlags: []string{"-short"}}, TimeoutPolicy{Global: time.Second}, t.TempDir(), nil, ".", nil)
	workers := p.createWorkers()
	if len(workers) == 0 {
		t.Fatal("expected workers; createWorkers returned none")
	}
	for i, w := range workers {
		if w.testCPU != 3 {
			t.Errorf("worker[%d].testCPU = %d, want 3 (STATEMENT_REMOVE drops `w.testCPU = p.exec.TestCPU`)", i, w.testCPU)
		}
		if w.tags != "integration" {
			t.Errorf("worker[%d].tags = %q, want %q (STATEMENT_REMOVE drops `w.tags = p.exec.Tags`)", i, w.tags, "integration")
		}
		// Length and element checked separately so a failure says which
		// half broke: dropping the assignment leaves testFlags nil (len 0),
		// while a wrong source would keep the length but change the value.
		if len(w.testFlags) != 1 || w.testFlags[0] != "-short" {
			t.Errorf("worker[%d].testFlags = %v, want [-short] (STATEMENT_REMOVE drops `w.testFlags = p.exec.TestFlags`)", i, w.testFlags)
		}
	}
}

// TestPoolCreateWorkersContinuesPastFailure kills INVERT_LOOP_CTRL on
// the `continue` after a NewWorker failure and STATEMENT_REMOVE on the
// stderr log. With newWorkerFunc stubbed to fail on the first call and
// succeed on subsequent calls, the original returns 3 workers and logs a
// diagnostic to stderr; the `continue` → `break` mutation returns 0;
// removing the Fprintf drops the diagnostic.
func TestPoolCreateWorkersContinuesPastFailure(t *testing.T) {
	orig := newWorkerFunc
	defer func() { newWorkerFunc = orig }()
	var calls atomic.Int32
	newWorkerFunc = func(id int, tmpDir string, policy TimeoutPolicy, srcCache map[string][]byte, projectDir string, testMap *coverage.TestMap) (*Worker, error) {
		n := calls.Add(1)
		if n == 1 {
			return nil, errors.New("first failure")
		}
		return &Worker{id: id, tmpSrcPath: filepath.Join(tmpDir, "src"), overlayPath: filepath.Join(tmpDir, "ovl"), policy: policy}, nil
	}

	p := NewPool(4, ExecOpts{}, TimeoutPolicy{Global: time.Second}, t.TempDir(), nil, ".", nil)
	var workers []*Worker
	captured := captureStderr(t, func() {
		workers = p.createWorkers()
	})
	if len(workers) != 3 {
		t.Errorf("got %d workers, want 3 (1 failure + 3 successes); INVERT_LOOP_CTRL turns the err-continue into break", len(workers))
	}
	if !strings.Contains(captured, "NewWorker 0 failed") {
		t.Errorf("stderr missing the per-failure diagnostic; got: %q — STATEMENT_REMOVE on the Fprintf elides the log", captured)
	}
}

// TestPoolNoWorkersLogsToStderr kills BRANCH_IF and STATEMENT_REMOVE on
// the `if len(workers) == 0 { Fprintln(os.Stderr, ...) }` block. Without
// the log call, callers downstream lose the diagnostic — the only signal
// that the pool ran but did nothing.
func TestPoolNoWorkersLogsToStderr(t *testing.T) {
	captured := captureStderr(t, func() {
		orig := newWorkerFunc
		defer func() { newWorkerFunc = orig }()
		newWorkerFunc = func(int, string, TimeoutPolicy, map[string][]byte, string, *coverage.TestMap) (*Worker, error) {
			return nil, errors.New("always fail")
		}
		p := NewPool(2, ExecOpts{}, TimeoutPolicy{Global: time.Second}, t.TempDir(), nil, ".", nil)
		mutants := []mutator.Mutant{
			{ID: 1, File: "/abs/f.go", Pkg: "p", Status: mutator.StatusPending},
		}
		p.Run(context.Background(), mutants, nil)
	})
	if !strings.Contains(captured, "no workers could be started") {
		t.Errorf("stderr missing the diagnostic; got: %q", captured)
	}
}

// TestPoolNoWorkersMarksPendingInfra pins what the stderr line above cannot
// carry on its own. A mutant left Pending is counted in the report's total
// but in none of its buckets, so a run where every worker failed to start
// reports no infrastructure errors, warns about nothing, and evaluates its
// thresholds over cached results alone — a green gate on a run that tested
// nothing. Every pending mutant must come back as an INFRA ERROR, and must
// be reported through onResult so the progress display and the report agree.
func TestPoolNoWorkersMarksPendingInfra(t *testing.T) {
	orig := newWorkerFunc
	defer func() { newWorkerFunc = orig }()
	newWorkerFunc = func(int, string, TimeoutPolicy, map[string][]byte, string, *coverage.TestMap) (*Worker, error) {
		return nil, errors.New("write /tmp/gomutants-0.go: no space left on device")
	}

	mutants := []mutator.Mutant{
		{ID: 1, File: "/abs/f.go", Pkg: "p", Status: mutator.StatusPending},
		{ID: 2, File: "/abs/f.go", Pkg: "p", Status: mutator.StatusPending},
		// A mutant the cache already settled must be left exactly as it is.
		{ID: 3, File: "/abs/f.go", Pkg: "p", Status: mutator.StatusKilled, FromCache: true},
	}
	var reported []mutator.Mutant
	captureStderr(t, func() {
		p := NewPool(2, ExecOpts{}, TimeoutPolicy{Global: time.Second}, t.TempDir(), nil, ".", nil)
		p.Run(context.Background(), mutants, func(m mutator.Mutant) {
			reported = append(reported, m)
		})
	})

	for _, i := range []int{0, 1} {
		if mutants[i].Status != mutator.StatusInfraError {
			t.Errorf("mutant %d = %v, want InfraError; Pending would vanish from every report bucket",
				mutants[i].ID, mutants[i].Status)
		}
	}
	if mutants[2].Status != mutator.StatusKilled {
		t.Errorf("cached mutant = %v, want its Killed verdict untouched", mutants[2].Status)
	}
	if len(reported) != 2 {
		t.Fatalf("onResult called %d times, want 2 (once per pending mutant)", len(reported))
	}
	for _, m := range reported {
		if m.Status != mutator.StatusInfraError {
			t.Errorf("reported mutant %d as %v, want InfraError", m.ID, m.Status)
		}
	}
}

// captureStderr redirects os.Stderr through a pipe for the duration of fn
// and returns whatever was written. Used to assert on log lines that have
// no other observable handle.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	done := make(chan struct{})
	var captured strings.Builder
	go func() {
		defer close(done)
		_, _ = io.Copy(&captured, r)
	}()
	fn()
	w.Close()
	<-done
	return captured.String()
}

// TestPoolRunWorkerCtxErrCheckHitDeterministic deterministically exercises
// the worker goroutine's `if ctx.Err() != nil { return }` body (pool.go
// lines 99–101). The pre-cancelled-ctx variant below is probabilistic on
// Linux: with ctx already cancelled when Run starts, the feeder's `select`
// between `case work <- idx:` and `case <-ctx.Done():` picks ctx.Done on
// the first iteration with probability 0.5, closing the work channel
// before any send. The worker's `for idx := range work` then exits without
// entering the body, leaving lines 99–101 uncovered on ~50% of CI runs.
//
// Strategy: feed the worker N mutants and cancel ctx synchronously inside
// the very first w.Test call (via the writeFileFunc indirection, which
// w.Test invokes before any subprocess work). When that first w.Test
// returns, ctx is already cancelled; the worker's next `for idx := range
// work` receive succeeds (the work channel was filled by the feeder before
// ctx was cancelled, since the feeder ran first), the `if ctx.Err() != nil`
// check fires true, and the early-return path is taken — no race on
// callback ordering or scheduler timing.
func TestPoolRunWorkerCtxErrCheckHitDeterministic(t *testing.T) {
	dir := setupTestProject(t)
	srcPath := filepath.Join(dir, "add.go")
	src, _ := os.ReadFile(srcPath)
	cache := map[string][]byte{srcPath: src}
	plusIdx := strings.IndexByte(string(src), '+')

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Hook writeFileFunc to cancel ctx during the very first w.Test call.
	// NewWorker's two preflight writes (the empty worker-N.go and
	// overlay-N.json) come first; w.Test's first write is the patched
	// source file, which is the third writeFileFunc invocation. Cancelling
	// at that point guarantees the first w.Test still completes (the
	// patched file is written before cancel takes effect on subprocess
	// start) but ctx is cancelled by the time the worker loops back to
	// `for idx := range work`, so the pre-w.Test `if ctx.Err() != nil`
	// check fires on the second iteration.
	origWrite := writeFileFunc
	defer func() { writeFileFunc = origWrite }()
	var writeCalls atomic.Int32
	writeFileFunc = func(name string, data []byte, perm os.FileMode) error {
		if writeCalls.Add(1) == 3 {
			cancel()
		}
		return origWrite(name, data, perm)
	}

	// Single worker so the two mutants serialize through one goroutine —
	// keeps the test's iteration-ordering guarantee tight.
	p := NewPool(1, ExecOpts{}, TimeoutPolicy{Global: 30 * time.Second}, t.TempDir(), cache, dir, nil)
	mk := func(id int, repl string) mutator.Mutant {
		return mutator.Mutant{
			ID: id, File: srcPath, Pkg: "testmod",
			StartOffset: plusIdx, EndOffset: plusIdx + 1,
			Replacement: repl, Status: mutator.StatusPending,
		}
	}
	mutants := []mutator.Mutant{mk(1, "-"), mk(2, "*")}

	var calls atomic.Int32
	runWithDeadline(t, 60*time.Second, func() {
		p.Run(ctx, mutants, func(mutator.Mutant) { calls.Add(1) })
	})

	// At most one result should reach the callback: the first mutant ran
	// (its writeFileFunc call cancelled ctx mid-flight, but w.Test still
	// finishes and emits a result), and the second was skipped via the
	// ctx-err early-return (its result never makes it onto the results
	// channel). BRANCH_IF on the early-return body would let the second
	// w.Test fire and bump calls to 2.
	if got := calls.Load(); got > 1 {
		t.Errorf("callback fired %d times, want at most 1 — BRANCH_IF on the worker's `if ctx.Err() != nil { return }` lets the post-cancel mutant slip through", got)
	}
}

// TestPoolRunCancelledNoCallback kills BRANCH_IF on the worker goroutine's
// `if ctx.Err() != nil { return }`. With the body elided, w.Test runs
// (returns Pending via its own ctx-cancel handling) and the result still
// makes it onto the results channel — so the user-supplied callback fires
// for a mutant the user expected to be skipped.
func TestPoolRunCancelledNoCallback(t *testing.T) {
	dir := setupTestProject(t)
	srcPath := filepath.Join(dir, "add.go")
	src, _ := os.ReadFile(srcPath)
	cache := map[string][]byte{srcPath: src}
	plusIdx := strings.IndexByte(string(src), '+')

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	p := NewPool(1, ExecOpts{}, TimeoutPolicy{Global: 30 * time.Second}, t.TempDir(), cache, dir, nil)
	mutants := []mutator.Mutant{
		{ID: 1, File: srcPath, Pkg: "testmod", StartOffset: plusIdx, EndOffset: plusIdx + 1, Replacement: "-", Status: mutator.StatusPending},
	}

	var calls atomic.Int32
	p.Run(ctx, mutants, func(mutator.Mutant) { calls.Add(1) })

	if got := calls.Load(); got != 0 {
		t.Errorf("callback fired %d times for cancelled ctx; BRANCH_IF on the worker's ctx.Err() check lets results through", got)
	}
}

// TestRunCoverageStdoutGoesToStderr kills STATEMENT_REMOVE on
// `cmd.Stdout = os.Stderr` in RunCoverage. `go test` buffers stdout from
// passing tests, so we make the test fail; the failure message embeds a
// unique marker that go test always prints. With the assignment in place
// the marker surfaces in the parent's stderr; without it, it disappears.
func TestRunCoverageStdoutGoesToStderr(t *testing.T) {
	const marker = "MARKER_8z2qPxV"
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":      "module testmod\n\ngo 1.26\n",
		"add.go":      "package testmod\n\nfunc Add(a, b int) int { return a + b }\n",
		"add_test.go": "package testmod\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tt.Fatal(\"" + marker + "\")\n}\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	captured := captureStderr(t, func() {
		// RunCoverage returns an error because the test fails; we don't
		// care about the error itself, only the test output that should
		// have been routed to os.Stderr.
		_, _ = RunCoverage(context.Background(), dir, []string{"testmod"}, "", "", t.TempDir(), nil)
	})
	if !strings.Contains(captured, marker) {
		t.Errorf("marker %q not found in stderr capture; STATEMENT_REMOVE on `cmd.Stdout = os.Stderr` discards test output. Captured: %q", marker, captured)
	}
}

// TestPoolRunEmpty pins the early-return when no mutants are pending:
// nil input must not panic, and an all-terminal slice must not have any
// statuses overwritten by the pending-mutant pipeline.
func TestPoolRunEmpty(t *testing.T) {
	tmpDir := t.TempDir()
	p := NewPool(2, ExecOpts{}, TimeoutPolicy{Global: 30 * time.Second}, tmpDir, nil, ".", nil)

	// nil input — Run must early-exit cleanly.
	p.Run(context.Background(), nil, nil)

	// Non-nil but no Pending — statuses must be preserved verbatim.
	mutants := []mutator.Mutant{
		{ID: 1, Status: mutator.StatusKilled},
		{ID: 2, Status: mutator.StatusLived},
	}
	called := false
	p.Run(context.Background(), mutants, func(mutator.Mutant) { called = true })
	if mutants[0].Status != mutator.StatusKilled || mutants[1].Status != mutator.StatusLived {
		t.Errorf("statuses changed on no-pending Run: %v", mutants)
	}
	if called {
		t.Error("onResult called with no pending mutants — pending filter is wrong")
	}
}

func TestPoolRunWithPending(t *testing.T) {
	dir := setupTestProject(t)
	srcPath := filepath.Join(dir, "add.go")
	src, _ := os.ReadFile(srcPath)
	cache := map[string][]byte{srcPath: src}

	// Find the "+" offset.
	plusIdx := 0
	for i, c := range string(src) {
		if c == '+' && i > 30 {
			plusIdx = i
			break
		}
	}

	tmpDir := t.TempDir()
	p := NewPool(2, ExecOpts{}, TimeoutPolicy{Global: 30 * time.Second}, tmpDir, cache, dir, nil)

	mutants := []mutator.Mutant{
		{
			ID:          1,
			File:        srcPath,
			Pkg:         "testmod",
			StartOffset: plusIdx,
			EndOffset:   plusIdx + 1,
			Replacement: "-",
			Status:      mutator.StatusPending,
		},
		{
			ID:          2,
			File:        srcPath,
			Pkg:         "testmod",
			StartOffset: plusIdx,
			EndOffset:   plusIdx + 1,
			Replacement: "*",
			Status:      mutator.StatusPending,
		},
		{
			ID:     3,
			Status: mutator.StatusNotCovered,
		},
	}

	var callbackCount int
	runWithDeadline(t, 60*time.Second, func() {
		p.Run(context.Background(), mutants, func(m mutator.Mutant) {
			callbackCount++
		})
	})

	if len(mutants) != 3 {
		t.Fatalf("expected 3 mutants, got %d", len(mutants))
	}

	// Mutants 1 and 2 should be tested (KILLED since tests check result).
	if mutants[0].Status != mutator.StatusKilled {
		t.Errorf("mutant 1: status=%v, want KILLED", mutants[0].Status)
	}
	if mutants[1].Status != mutator.StatusKilled {
		t.Errorf("mutant 2: status=%v, want KILLED", mutants[1].Status)
	}
	// Mutant 3 unchanged.
	if mutants[2].Status != mutator.StatusNotCovered {
		t.Errorf("mutant 3: status=%v, want NOT_COVERED", mutants[2].Status)
	}
	if callbackCount != 2 {
		t.Errorf("callback called %d times, want 2", callbackCount)
	}
}

// TestPoolRunOnResultSerial pins the contract that Run invokes onResult
// from a single goroutine, serially. main.go's mid-run cache checkpointing
// hooks into this callback and reads the shared mutants slice without a
// lock — that's only sound if onResult never runs concurrently with
// itself. Multiple workers + a re-entrancy guard (a plain bool, plus a
// non-atomic counter) would trip under `-race` or fail outright if the
// pool ever dispatched results concurrently.
func TestPoolRunOnResultSerial(t *testing.T) {
	dir := setupTestProject(t)
	srcPath := filepath.Join(dir, "add.go")
	src, _ := os.ReadFile(srcPath)
	cache := map[string][]byte{srcPath: src}

	plusIdx := 0
	for i, c := range string(src) {
		if c == '+' && i > 30 {
			plusIdx = i
			break
		}
	}

	tmpDir := t.TempDir()
	p := NewPool(4, ExecOpts{}, TimeoutPolicy{Global: 30 * time.Second}, tmpDir, cache, dir, nil)

	replacements := []string{"-", "*", "/", "%"}
	mutants := make([]mutator.Mutant, len(replacements))
	for i, r := range replacements {
		mutants[i] = mutator.Mutant{
			ID:          i + 1,
			File:        srcPath,
			Pkg:         "testmod",
			StartOffset: plusIdx,
			EndOffset:   plusIdx + 1,
			Replacement: r,
			Status:      mutator.StatusPending,
		}
	}

	var inCallback bool
	var count int
	runWithDeadline(t, 90*time.Second, func() {
		p.Run(context.Background(), mutants, func(m mutator.Mutant) {
			if inCallback {
				t.Error("onResult invoked concurrently — checkpoint hook would need a lock")
			}
			inCallback = true
			count++ // non-atomic on purpose: races here are the bug we're guarding against
			inCallback = false
		})
	})

	if count != len(replacements) {
		t.Errorf("onResult called %d times, want %d", count, len(replacements))
	}
}

// TestPoolRunNoWorkersAvailable verifies that when every NewWorker call
// fails, Run surfaces the condition cleanly and does not hang on a feeder
// goroutine stuck sending into a channel that no one reads.
func TestPoolRunNoWorkersAvailable(t *testing.T) {
	// tmpDir that NewWorker can't write into — pass a path that is a
	// regular file. os.WriteFile(worker-N.go) fails with "not a directory".
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := NewPool(4, ExecOpts{}, TimeoutPolicy{Global: 30 * time.Second}, blocker, nil, ".", nil)
	mutants := []mutator.Mutant{
		{ID: 1, File: "/abs/f.go", Pkg: "p", Status: mutator.StatusPending},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(context.Background(), mutants, nil)
	}()
	select {
	case <-done:
		// OK — Run returned rather than deadlocking.
	case <-time.After(5 * time.Second):
		t.Fatal("Pool.Run hung when all workers failed to start")
	}
}

// TestPoolRunNonDenseIDs kills the assumption that mutant IDs are a
// dense 1-based contiguous range. With sparse IDs the old
// `mutants[result.ID-1] = result` corrupted arbitrary slots.
func TestPoolRunNonDenseIDs(t *testing.T) {
	dir := setupTestProject(t)
	srcPath := filepath.Join(dir, "add.go")
	src, _ := os.ReadFile(srcPath)
	cache := map[string][]byte{srcPath: src}

	plusIdx := 0
	for i, c := range string(src) {
		if c == '+' && i > 30 {
			plusIdx = i
			break
		}
	}

	p := NewPool(1, ExecOpts{}, TimeoutPolicy{Global: 30 * time.Second}, t.TempDir(), cache, dir, nil)
	mutants := []mutator.Mutant{
		// Sparse IDs: 100 and 500. Not 1 and 2.
		{ID: 100, File: srcPath, Pkg: "testmod",
			StartOffset: plusIdx, EndOffset: plusIdx + 1, Replacement: "-",
			Status: mutator.StatusPending},
		{ID: 500, File: srcPath, Pkg: "testmod",
			StartOffset: plusIdx, EndOffset: plusIdx + 1, Replacement: "*",
			Status: mutator.StatusPending},
	}
	p.Run(context.Background(), mutants, nil)
	if len(mutants) != 2 {
		t.Fatalf("len(mutants)=%d, want 2", len(mutants))
	}
	for _, m := range mutants {
		if m.Status == mutator.StatusPending {
			t.Errorf("mutant ID=%d still Pending — ID→index lookup failed", m.ID)
		}
	}
}

func TestPoolRunCancelled(t *testing.T) {
	dir := setupTestProject(t)
	srcPath := filepath.Join(dir, "add.go")
	src, _ := os.ReadFile(srcPath)
	cache := map[string][]byte{srcPath: src}

	plusIdx := 0
	for i, c := range string(src) {
		if c == '+' && i > 30 {
			plusIdx = i
			break
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	p := NewPool(1, ExecOpts{}, TimeoutPolicy{Global: 30 * time.Second}, t.TempDir(), cache, dir, nil)

	mutants := []mutator.Mutant{
		{ID: 1, File: srcPath, Pkg: "testmod", StartOffset: plusIdx, EndOffset: plusIdx + 1, Replacement: "-", Status: mutator.StatusPending},
	}

	// Should not hang — cancelled context means workers exit early.
	p.Run(ctx, mutants, nil)
	if len(mutants) != 1 {
		t.Fatalf("expected 1 mutant, got %d", len(mutants))
	}
}

// TestPoolRunCancelledFeederExitsViaCtxDone drives the work-feeder
// goroutine's `case <-ctx.Done()` branch in Run. The work channel is
// buffered to len(pending), so sends never block — both select cases
// are ready every iteration once ctx is cancelled. With N=64 pending
// mutants and ctx pre-cancelled, Go's uniform pseudo-random select
// picks ctx.Done at least once with probability 1 - 0.5^64 (~5e-20),
// which is well below any cosmic-ray flake threshold.
func TestPoolRunCancelledFeederExitsViaCtxDone(t *testing.T) {
	dir := setupTestProject(t)
	srcPath := filepath.Join(dir, "add.go")
	src, _ := os.ReadFile(srcPath)
	cache := map[string][]byte{srcPath: src}
	plusIdx := strings.IndexByte(string(src), '+')

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	p := NewPool(1, ExecOpts{}, TimeoutPolicy{Global: 30 * time.Second}, t.TempDir(), cache, dir, nil)

	const n = 64
	mutants := make([]mutator.Mutant, n)
	for i := range mutants {
		mutants[i] = mutator.Mutant{
			ID:          i + 1,
			File:        srcPath,
			Pkg:         "testmod",
			StartOffset: plusIdx,
			EndOffset:   plusIdx + 1,
			Replacement: "-",
			Status:      mutator.StatusPending,
		}
	}

	runWithDeadline(t, 30*time.Second, func() {
		p.Run(ctx, mutants, nil)
	})
}

func TestMeasureBaseline(t *testing.T) {
	dir := setupTestProject(t)
	duration, err := MeasureBaseline(context.Background(), dir, []string{"testmod"}, "", nil)
	if err != nil {
		t.Fatalf("MeasureBaseline: %v", err)
	}
	if duration <= 0 {
		t.Errorf("duration=%v, want > 0", duration)
	}
}

func TestMeasureBaselineFailure(t *testing.T) {
	_, err := MeasureBaseline(context.Background(), t.TempDir(), []string{"definitely/nonexistent/pkg/zzz"}, "", nil)
	if err == nil {
		t.Fatal("expected error for nonexistent package")
	}
	// Error must wrap stderr. Kills STATEMENT_REMOVE on `cmd.Stderr = &stderr`.
	msg := err.Error()
	if !strings.Contains(msg, "definitely/nonexistent/pkg/zzz") && !strings.Contains(msg, "no required module") && !strings.Contains(msg, "cannot find") {
		t.Errorf("error should include stderr content, got: %q", msg)
	}
}

func TestRunCoverage(t *testing.T) {
	dir := setupTestProject(t)
	tmpDir := t.TempDir()
	profilePath, err := RunCoverage(context.Background(), dir, []string{"testmod"}, "", "", tmpDir, nil)
	if err != nil {
		t.Fatalf("RunCoverage: %v", err)
	}

	// Verify profile file was created and is non-empty.
	info, err := os.Stat(profilePath)
	if err != nil {
		t.Fatalf("profile not found: %v", err)
	}
	if info.Size() == 0 {
		t.Error("profile file is empty")
	}
}

func TestRunCoverageWithCoverpkg(t *testing.T) {
	dir := setupTestProject(t)
	tmpDir := t.TempDir()
	_, err := RunCoverage(context.Background(), dir, []string{"testmod"}, "testmod", "", tmpDir, nil)
	if err != nil {
		t.Fatalf("RunCoverage with coverpkg: %v", err)
	}
}

func TestRunCoverageFailure(t *testing.T) {
	_, err := RunCoverage(context.Background(), t.TempDir(), []string{"definitely/nonexistent/pkg/zzz"}, "", "", t.TempDir(), nil)
	if err == nil {
		t.Fatal("expected error for nonexistent package")
	}
	// Error must wrap stderr. Kills STATEMENT_REMOVE on `cmd.Stderr = &stderr`.
	msg := err.Error()
	if !strings.Contains(msg, "definitely/nonexistent/pkg/zzz") && !strings.Contains(msg, "no required module") && !strings.Contains(msg, "cannot find") {
		t.Errorf("error should include stderr content, got: %q", msg)
	}
}

// TestRunCoverageCoverPkgApplied verifies that the -coverpkg flag is passed
// through. Kills CONDITIONALS_NEGATION, BRANCH_IF, and STATEMENT_REMOVE
// mutations on `if coverPkg != "" { args = append(args, "-coverpkg="+...) }`.
//
// Strategy: pass a coverpkg pattern that matches no package. Go still runs
// the tests successfully but records "[no statements]" — the profile file
// ends up containing only the "mode:" header. Without the flag passed
// through, the tested package's own code is instrumented and the profile
// contains block lines (e.g., "testmod/add.go:2.24,2.40 1 1").
func TestRunCoverageCoverPkgApplied(t *testing.T) {
	dir := setupTestProject(t)
	tmpDir := t.TempDir()
	profilePath, err := RunCoverage(context.Background(), dir, []string{"testmod"}, "completely/nonexistent/zzz", "", tmpDir, nil)
	if err != nil {
		t.Fatalf("RunCoverage: %v", err)
	}
	data, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// A profile produced with a no-match -coverpkg has only the "mode:" header.
	// Block lines (".go:") appearing means the flag was dropped by mutation.
	if strings.Contains(string(data), ".go:") {
		t.Errorf("coverpkg=nonexistent should produce empty coverage, but profile has block lines:\n%s", data)
	}
}

// setupTaggedTestProject builds a project whose only failing test sits
// behind `//go:build mytag`. The tagged test runs (and fails) only when
// `-tags=mytag` reaches the inner `go test`, making tag forwarding
// observable through pass/fail.
func setupTaggedTestProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	taggedTest := "//go:build mytag\n\npackage testmod\n\nimport \"testing\"\n\nfunc TestTagged(t *testing.T) {\n\tt.Fatal(\"tagged test ran\")\n}\n"
	for name, content := range map[string]string{
		"go.mod":         "module testmod\n\ngo 1.26\n",
		"add.go":         "package testmod\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n",
		"add_test.go":    "package testmod\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"wrong\")\n\t}\n}\n",
		"tagged_test.go": taggedTest,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestMeasureBaselineForwardsTags kills BRANCH_IF / CONDITIONALS_NEGATION /
// STATEMENT_REMOVE on the `if tags != "" { args = append(args, "-tags="+...) }`
// guard in MeasureBaseline: the tagged failing test compiles in only when
// the tag is forwarded, flipping the result from pass to error.
func TestMeasureBaselineForwardsTags(t *testing.T) {
	dir := setupTaggedTestProject(t)

	if _, err := MeasureBaseline(context.Background(), dir, []string{"testmod"}, "", nil); err != nil {
		t.Fatalf("no tags: tagged test must be excluded, got error: %v", err)
	}
	if _, err := MeasureBaseline(context.Background(), dir, []string{"testmod"}, "mytag", nil); err == nil {
		t.Fatal("tags=mytag: tagged failing test must run and fail, but baseline succeeded")
	}
}

// setupShortSensitiveProject writes a module whose test fails only when
// `-short` is in effect. That inverted polarity is what makes forwarding
// observable: the flag reaching the inner `go test` is the difference
// between a passing and a failing run, with no timing heuristics involved.
func setupShortSensitiveProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	shortTest := "package testmod\n\nimport \"testing\"\n\nfunc TestShortSensitive(t *testing.T) {\n\tif testing.Short() {\n\t\tt.Fatal(\"ran in short mode\")\n\t}\n}\n"
	for name, content := range map[string]string{
		"go.mod":      "module testmod\n\ngo 1.26\n",
		"add.go":      "package testmod\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n",
		"add_test.go": shortTest,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestMeasureBaselineForwardsTestFlags kills STATEMENT_REMOVE on
// `args = append(args, testFlags...)` in MeasureBaseline. The baseline must
// measure the same work the per-mutant runs do; if the flags are dropped
// here, a --test-flags=-short run is sized against a full-cost baseline.
func TestMeasureBaselineForwardsTestFlags(t *testing.T) {
	dir := setupShortSensitiveProject(t)

	if _, err := MeasureBaseline(context.Background(), dir, []string{"testmod"}, "", nil); err != nil {
		t.Fatalf("no test flags: suite must pass, got error: %v", err)
	}
	if _, err := MeasureBaseline(context.Background(), dir, []string{"testmod"}, "", []string{"-short"}); err == nil {
		t.Fatal("testFlags=[-short]: the short-sensitive test must run and fail, but baseline succeeded")
	}
}

// TestRunCoverageForwardsTestFlags is the RunCoverage analogue. Forwarding
// here is what keeps the profile honest: a test skipped under -short must
// not be recorded as covering, or every mutant on its lines is reported as
// a survivor rather than NOT_COVERED.
func TestRunCoverageForwardsTestFlags(t *testing.T) {
	dir := setupShortSensitiveProject(t)

	if _, err := RunCoverage(context.Background(), dir, []string{"testmod"}, "", "", t.TempDir(), nil); err != nil {
		t.Fatalf("no test flags: suite must pass, got error: %v", err)
	}
	if _, err := RunCoverage(context.Background(), dir, []string{"testmod"}, "", "", t.TempDir(), []string{"-short"}); err == nil {
		t.Fatal("testFlags=[-short]: the short-sensitive test must run and fail, but coverage run succeeded")
	}
}

// TestRunCoverageForwardsTags is the RunCoverage analogue of
// TestMeasureBaselineForwardsTags, pinning the same `-tags=` append guard.
func TestRunCoverageForwardsTags(t *testing.T) {
	dir := setupTaggedTestProject(t)

	if _, err := RunCoverage(context.Background(), dir, []string{"testmod"}, "", "", t.TempDir(), nil); err != nil {
		t.Fatalf("no tags: tagged test must be excluded, got error: %v", err)
	}
	if _, err := RunCoverage(context.Background(), dir, []string{"testmod"}, "", "mytag", t.TempDir(), nil); err == nil {
		t.Fatal("tags=mytag: tagged failing test must run and fail, but coverage run succeeded")
	}
}

// setupTestBinaryFlagProject writes the layout from issue #75: a module
// root that is a valid package with *no test files*, and a `sub` package
// whose test declares a flag of its own. subFails picks whether that test
// passes or fails.
//
// The root package is the whole point. The first flag `go test` does not
// recognize marks the package list as already seen, so a package name
// after it is forwarded to the test binary as a positional argument and
// the package list falls back to `.`. A test-binary flag placed ahead of
// the package argument therefore tests `.` — and because `.` is a
// buildable package with no tests, that exits 0. No diagnostic, wrong
// package. Without the root package the same bug fails loudly ("no Go
// files in ..."), which is the variant that would have been caught
// already.
func setupTestBinaryFlagProject(t *testing.T, subFails bool) string {
	t.Helper()
	dir := t.TempDir()
	body := "if Add(1, 2) != 3 {\n\t\tt.Fatal(\"wrong\")\n\t}"
	if subFails {
		body = "t.Fatal(\"sub test ran\")"
	}
	subTest := "package sub\n\nimport (\n\t\"flag\"\n\t\"testing\"\n)\n\n" +
		"var iterations = flag.Int(\"custom.iterations\", 100, \"a test-binary flag\")\n\n" +
		"var customShort = flag.Bool(\"short\", false, \"a flag whose name collides with go test\")\n\n" +
		"func TestAdd(t *testing.T) {\n\t_ = *iterations\n" +
		"\tif *customShort {\n\t\tt.Fatal(\"custom -short reached the test binary\")\n\t}\n\t" + body + "\n}\n"
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"go.mod":          "module testmod\n\ngo 1.26\n",
		"root.go":         "package testmod\n\nfunc Root() int {\n\treturn 1\n}\n",
		"sub/sub.go":      "package sub\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n",
		"sub/sub_test.go": subTest,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestRunCoverageProfilesTargetPackageWithTestBinaryFlags is the issue #75
// regression: `args = append(args, packages...)` must precede
// `args = append(args, testFlags...)`, or a test-binary flag swallows the
// package argument.
//
// The assertion has to be on the profile's *contents*, not on the error:
// the pre-fix run exits 0 and writes a perfectly well-formed profile — for
// the wrong package. That silence is the bug. Swapping the two appends back
// yields a profile holding root.go and no sub/sub.go, which is what every
// downstream mutant sees as NOT_COVERED.
func TestRunCoverageProfilesTargetPackageWithTestBinaryFlags(t *testing.T) {
	dir := setupTestBinaryFlagProject(t, false)

	path, err := RunCoverage(context.Background(), dir, []string{"testmod/sub"},
		"", "", t.TempDir(), []string{"-custom.iterations=5"})
	if err != nil {
		t.Fatalf("coverage run failed: %v", err)
	}
	profile, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading profile: %v", err)
	}
	if !strings.Contains(string(profile), "testmod/sub/sub.go") {
		t.Errorf("profile has no testmod/sub/sub.go — the test-binary flag swallowed "+
			"the package argument and the module root was profiled instead.\nprofile:\n%s", profile)
	}
}

// TestMeasureBaselineRunsTargetPackageWithTestBinaryFlags is the
// MeasureBaseline half of the same regression, with the inverted polarity
// setupShortSensitiveProject uses: the sub package's test fails, so an
// error proves it ran. Pre-fix this returns nil — the module root was
// tested, has no test files, and exits 0 — which is exactly the silent
// wrongness being pinned.
//
// The root-package control is what makes the error discriminating. On its
// own "baseline failed" would also be satisfied by the flag breaking the
// run outright; targeting the root with the same flag has to keep
// succeeding, so the only difference left between the two calls is which
// package the argument actually selected.
func TestMeasureBaselineRunsTargetPackageWithTestBinaryFlags(t *testing.T) {
	dir := setupTestBinaryFlagProject(t, true)
	flags := []string{"-custom.iterations=5"}

	if _, err := MeasureBaseline(context.Background(), dir, []string{"testmod"}, "", flags); err != nil {
		t.Fatalf("root package has no tests and must pass, got error: %v", err)
	}
	if _, err := MeasureBaseline(context.Background(), dir, []string{"testmod/sub"}, "", flags); err == nil {
		t.Fatal("baseline succeeded: the failing sub test never ran, so the " +
			"test-binary flag swallowed the package argument and the module root was tested")
	}
}

// TestMeasureBaselineArgsForwardsCollidingTestBinaryFlag covers why -args
// remains useful after moving user fields behind the package. The go command
// owns the unprefixed -short spelling, while a compiled test binary can
// independently register its own -short flag (testing's built-in is named
// -test.short). Without -args the go command consumes and rewrites the flag;
// after -args it reaches the custom flag verbatim.
func TestMeasureBaselineArgsForwardsCollidingTestBinaryFlag(t *testing.T) {
	dir := setupTestBinaryFlagProject(t, false)

	if _, err := MeasureBaseline(context.Background(), dir, []string{"testmod/sub"}, "", []string{"-short"}); err != nil {
		t.Fatalf("go test's -short must not set the custom test-binary flag: %v", err)
	}
	if _, err := MeasureBaseline(context.Background(), dir, []string{"testmod/sub"}, "", []string{"-args", "-short"}); err == nil {
		t.Fatal("baseline succeeded: -args did not forward the colliding custom -short flag to the test binary")
	}
}
