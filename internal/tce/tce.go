// Package tce implements Trivial Compiler Equivalence detection: a
// surviving (LIVED) mutant is provably equivalent to the original when
// the Go compiler produces identical generated code for the mutated and
// the original source. Such a mutant can never change behavior, so it can
// never be killed by any test — it's noise, not a test gap.
//
// The check compiles the package twice with assembly emission scoped to
// that package (`go build -gcflags=<importpath>=-S`), once for the
// original (the reference) and once for the mutant (via `-overlay`), and
// compares the normalized assembly. Identical assembly ⇒ identical
// machine code ⇒ provably equivalent.
//
// Soundness: the failure mode is one-sided. Any real difference in the
// generated code (including data symbols and string/numeric constants,
// which `-S` dumps) diverges the hash, so a killable mutant is never
// declared equivalent. A truly-equivalent mutant whose assembly happens to
// differ is simply left LIVED (under-detection, the safe direction).
//
// The verdict is meaningful only for the host build configuration
// (toolchain, GOOS/GOARCH/GOEXPERIMENT) — the same one the tests run
// under — which is why the cache gates equivalence reuse on the Go
// toolchain.
package tce

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"

	"github.com/gomutants/gomutants/internal/mutator"
	"github.com/gomutants/gomutants/internal/patch"
)

// execCommandContext, writeFileFunc, and marshalFunc are package-level
// indirections to exec.CommandContext, os.WriteFile, and json.Marshal so
// tests can drive the subprocess and I/O failure paths without contriving
// filesystem or PATH state. Mirrors the runner/cache packages.
var (
	execCommandContext = exec.CommandContext
	writeFileFunc      = os.WriteFile
	marshalFunc        = json.Marshal
)

// Detector memoizes per-package reference assembly hashes and checks
// mutants for compiler equivalence. Safe for concurrent use: the
// per-package reference memo is guarded by mu, and each Check writes to
// caller-owned temp files.
type Detector struct {
	projectDir string
	tags       string            // forwarded as -tags= (empty omits)
	srcCache   map[string][]byte // shared, read-only production sources

	mu   sync.Mutex
	refs map[string]*refResult // import path -> memoized reference hash
}

// refResult memoizes one package's reference assembly hash. once ensures
// the reference is compiled exactly once even under concurrent Checks.
type refResult struct {
	once sync.Once
	hash string
	err  error
}

// NewDetector creates a Detector. srcCache is the in-memory production
// source map (absolute path -> bytes) reused from the run; tags mirrors
// the run's --tags so build-constrained packages compile identically to
// how they were tested.
func NewDetector(projectDir, tags string, srcCache map[string][]byte) *Detector {
	return &Detector{
		projectDir: projectDir,
		tags:       tags,
		srcCache:   srcCache,
		refs:       make(map[string]*refResult),
	}
}

// referenceHash returns the normalized reference-assembly hash for
// importPath, compiling the original (unmutated) package once and
// memoizing the result. A sticky error means we never retry a package
// whose reference build fails (e.g. cgo / hand-written .s) — its
// survivors are simply left LIVED.
func (d *Detector) referenceHash(ctx context.Context, importPath string) (string, error) {
	d.mu.Lock()
	r := d.refs[importPath]
	if r == nil {
		r = &refResult{}
		d.refs[importPath] = r
	}
	d.mu.Unlock()

	r.once.Do(func() {
		r.hash, r.err = d.compileHash(ctx, importPath, "")
	})
	return r.hash, r.err
}

// Check reports whether mutant m is equivalent to the original. It applies
// m's byte patch to tmpSrcPath, writes an overlay mapping the original
// source path to that temp file, compiles the package with the same -S
// flag plus -overlay, and compares the normalized assembly to the package
// reference. tmpSrcPath and overlayPath are the caller's stable temp files
// (one set per worker goroutine) so concurrent Checks don't collide.
func (d *Detector) Check(ctx context.Context, m mutator.Mutant, tmpSrcPath, overlayPath string) (bool, error) {
	// Cheap local prep first, so a malformed mutant never pays for the
	// (memoized) reference compile.
	original, ok := d.srcCache[m.File]
	if !ok {
		return false, fmt.Errorf("tce: source not cached: %s", m.File)
	}
	patched, err := patch.Apply(original, m.StartOffset, m.EndOffset, m.Replacement)
	if err != nil {
		return false, err
	}
	// gomutants:disable-next-line INTEGER_INCREMENT,INTEGER_DECREMENT reason="the mode of this throwaway temp source is observably irrelevant: it is written and then immediately read back by `go build -overlay` in the same process/user, so any owner-readable bits behave identically"
	if err := writeFileFunc(tmpSrcPath, patched, 0o644); err != nil {
		return false, err
	}
	if err := writeOverlay(overlayPath, m.File, tmpSrcPath); err != nil {
		return false, err
	}
	ref, err := d.referenceHash(ctx, m.Pkg)
	if err != nil {
		return false, err
	}
	h, err := d.compileHash(ctx, m.Pkg, overlayPath)
	if err != nil {
		return false, err
	}
	return h == ref, nil
}

// compileHash runs `go build` with package-scoped `-S` (plus -overlay when
// overlayPath is non-empty) and returns the normalized assembly hash.
func (d *Detector) compileHash(ctx context.Context, importPath, overlayPath string) (string, error) {
	cmd := execCommandContext(ctx, "go", buildArgs(importPath, overlayPath, d.tags)...)
	cmd.Dir = d.projectDir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	// gomutants:disable-next-line STATEMENT_REMOVE reason="a nil cmd.Stdout is already connected to the null device by os/exec, and `go build -S` emits assembly to stderr (stdout stays empty), so dropping this discard is observably identical"
	cmd.Stdout = io.Discard
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("tce: go build -S %s: %w\n%s", importPath, err, stderr.String())
	}
	return normalizeAsm(stderr.Bytes(), importPath), nil
}

// buildArgs assembles the `go build` argv. `-S` is emitted to stderr and
// scoped to importPath so dependencies/stdlib produce no assembly noise.
// `-o os.DevNull` discards any output binary (a main package would
// otherwise litter the working directory).
func buildArgs(importPath, overlayPath, tags string) []string {
	args := []string{"build", "-o", os.DevNull, "-gcflags=" + importPath + "=-S"}
	if overlayPath != "" {
		args = append(args, "-overlay="+overlayPath)
	}
	if tags != "" {
		args = append(args, "-tags="+tags)
	}
	return append(args, importPath)
}

// normalizeAsm hashes the `-S` assembly after dropping the single
// `# <importpath>` package-header line and trailing whitespace. The header
// is identical between the original and the mutant builds of the same
// package, so stripping it is purely defensive against incidental churn —
// and anchoring on the *exact* header (rather than any '#'-prefixed line)
// keeps every signal-bearing line in the hash, including data dumps that
// could legitimately begin with '#'. The file:line refs, symbol hashes, and
// object byte dumps that encode real differences are all kept.
func normalizeAsm(b []byte, importPath string) string {
	header := append([]byte("# "), importPath...)
	h := sha256.New()
	for line := range bytes.SplitSeq(b, []byte("\n")) {
		line = bytes.TrimRight(line, " \t\r")
		if bytes.Equal(line, header) {
			continue
		}
		h.Write(line)
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// overlay is the JSON structure for `go build -overlay`, mirroring the
// shape the runner uses for `go test -overlay`.
type overlay struct {
	Replace map[string]string `json:"Replace"`
}

func writeOverlay(path, srcAbs, tmpSrc string) error {
	b, err := marshalFunc(overlay{Replace: map[string]string{srcAbs: tmpSrc}})
	if err != nil {
		return err
	}
	// gomutants:disable-next-line INTEGER_INCREMENT,INTEGER_DECREMENT reason="overlay temp-file mode is observably irrelevant; it is read back by `go build -overlay` in the same process, so any owner-readable bits behave identically"
	return writeFileFunc(path, b, 0o644)
}

// runShared bundles the per-run state every worker goroutine reads, so the
// worker helpers stay below a sane parameter count.
type runShared struct {
	mutants  []mutator.Mutant
	work     <-chan int
	onResult func(mutator.Mutant)
	outMu    *sync.Mutex // serializes the status write + onResult (and its reads)
}

// Run checks every StatusLived mutant for compiler equivalence in
// parallel, flipping matches to StatusEquivalent in place and returning
// how many it flipped. Mutants that error or aren't equivalent stay LIVED.
// onResult (may be nil) is invoked once per checked mutant, serialized with
// the status writes so a non-concurrent sink that reads the whole slice
// (e.g. a cache checkpoint) is safe. Honors ctx cancellation.
func (d *Detector) Run(ctx context.Context, mutants []mutator.Mutant, workers int, tmpDir string, onResult func(mutator.Mutant)) int {
	lived := collectLived(mutants)
	// No early return for an empty `lived`: the loop below feeds an empty
	// work channel, so the workers spawn and exit immediately — keeping a
	// guard here only adds a mutation-equivalent fast path.
	//
	// Sort by (Pkg, File, StartOffset) so consecutive survivors share the
	// per-package reference compile and the dependency build cache.
	sort.SliceStable(lived, func(a, b int) bool {
		return mutantLess(mutants[lived[a]], mutants[lived[b]])
	})

	// gomutants:disable-next-line INTEGER_INCREMENT reason="raising the floor from 1 to 2 still clamps to a worker count >=1 that processes the same survivors, so it's observably equivalent; the killable decrement (floor 0 → no workers) is pinned by the workers=0 test"
	workers = max(1, workers)
	work := make(chan int, len(lived))
	rs := &runShared{mutants: mutants, work: work, onResult: onResult, outMu: &sync.Mutex{}}

	var wg sync.WaitGroup
	for w := range workers {
		tmpSrc := filepath.Join(tmpDir, fmt.Sprintf("tce-%d.go", w))
		overlayPath := filepath.Join(tmpDir, fmt.Sprintf("tce-overlay-%d.json", w))
		wg.Go(func() {
			d.worker(ctx, rs, tmpSrc, overlayPath)
		})
	}
	for _, idx := range lived {
		work <- idx // buffered to len(lived); never blocks
	}
	close(work)
	wg.Wait()

	// Count the flips here (over the survivor indices only) so the caller
	// doesn't re-scan the whole mutants slice for the same number.
	equivalent := 0
	for _, idx := range lived {
		if mutants[idx].Status == mutator.StatusEquivalent {
			equivalent++
		}
	}
	return equivalent
}

// collectLived returns the indices of the StatusLived mutants.
func collectLived(mutants []mutator.Mutant) []int {
	var lived []int
	for i := range mutants {
		if mutants[i].Status == mutator.StatusLived {
			lived = append(lived, i)
		}
	}
	return lived
}

// worker drains the work channel, checking each survivor for equivalence
// until the channel closes or ctx is cancelled.
func (d *Detector) worker(ctx context.Context, rs *runShared, tmpSrc, overlayPath string) {
	for idx := range rs.work {
		if ctx.Err() != nil {
			return
		}
		d.checkAndReport(ctx, rs, idx, tmpSrc, overlayPath)
	}
}

// checkAndReport flips an equivalent survivor to StatusEquivalent and then
// reports it via onResult. The compile (Check) runs unlocked, but the
// status write and onResult are both held under outMu: a non-nil onResult
// (e.g. a cache checkpoint) reads the whole mutants slice, so every worker's
// Status write must take the same lock to stay race-free with that reader.
func (d *Detector) checkAndReport(ctx context.Context, rs *runShared, idx int, tmpSrc, overlayPath string) {
	eq, err := d.Check(ctx, rs.mutants[idx], tmpSrc, overlayPath)
	rs.outMu.Lock()
	if err == nil && eq {
		rs.mutants[idx].Status = mutator.StatusEquivalent
	}
	if rs.onResult != nil {
		rs.onResult(rs.mutants[idx])
	}
	rs.outMu.Unlock()
}

// mutantLess orders mutants by (Pkg, File, StartOffset). Paired `<`/`>`
// comparisons keep each boundary observable to the mutation tester.
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
