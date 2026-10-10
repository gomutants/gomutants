package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gomutants/gomutants/internal/coverage"
	"github.com/gomutants/gomutants/internal/mutator"
	"github.com/gomutants/gomutants/internal/patch"
	"github.com/gomutants/gomutants/internal/proctree"
)

// maxSubprocRSSBytes caps per-mutant subprocess group memory. A mutation that
// flips a loop termination or allocation bound can make go test (or its test
// binary) balloon to tens of GB within seconds. We SIGKILL the whole process
// group at this cap; a killed mutant is classified as TimedOut.
//
// var (not const) so tests can lower it to a tiny value and force the
// monitor goroutine's kill path on a normal-sized test process.
//
// On Windows pgroupRSSBytes is a no-op (returns 0) so this cap never trips;
// the per-mutant context timeout is the backstop there.
var maxSubprocRSSBytes int64 = 2 * 1024 * 1024 * 1024 // 2 GiB

// monitorPollInterval is the cadence at which the RSS monitor probes
// `ps -g`. var so tests can shrink it to make the kill path fire quickly.
var monitorPollInterval = 1 * time.Second

// nonZeroSince returns time.Since(start) but guarantees a strictly positive
// result, so callers can use Duration==0 as a "never set" sentinel. Without
// this, rapid early-return paths can yield a zero Duration on some clocks.
func nonZeroSince(start time.Time) time.Duration {
	return clampPositive(time.Since(start))
}

// clampPositive returns d if it is strictly positive, otherwise the smallest
// positive Duration. Extracted from nonZeroSince so the d == 0 boundary can
// be tested directly — driving nonZeroSince is racy because time.Since on a
// just-captured `time.Now()` returns a tiny but nonzero positive duration
// on real clocks, hiding the `<` vs `<=` mutation.
func clampPositive(d time.Duration) time.Duration {
	if d <= 0 {
		return time.Nanosecond
	}
	return d
}

// maxCapturedOutput caps per-stream subprocess capture. A misbehaving mutant
// (panic-loop, infinite logging) can otherwise balloon RSS by gigabytes before
// the timeout fires.
const maxCapturedOutput = 1 << 20 // 1 MiB

// cappedBuffer accumulates writes up to cap bytes and silently drops the rest.
// Compile-error detection only needs early output; later noise is useless.
//
// Dropping is recorded because the infrastructure classifier reasons about
// what is *absent* from the output: a missing `--- FAIL: ` line is read as
// "no test reported a failure", which is only sound if the whole stream was
// seen. See infraFromOutput.
type cappedBuffer struct {
	buf       []byte
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	// Compute take with min/max so neither comparison is an AST `<`/`<=`
	// operator the boundary mutator can target. The previous if-else form
	// produced equivalent boundary mutants because both branches collapsed
	// to the same observable result on the equality cases.
	take := min(len(p), max(0, maxCapturedOutput-len(c.buf)))
	c.buf = append(c.buf, p[:take]...)
	// take is capped at len(p), so != is exactly "some bytes were dropped"
	// without introducing an ordering comparison for the boundary mutator.
	if take != len(p) {
		c.truncated = true
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string { return string(c.buf) }

var compileErrorRe = regexp.MustCompile(`\.go:\d+:\d+:`)

// testPhaseInfraSignatures are host resource failures recognizable in output
// that may have been produced while the test binary was running.
//
// Everything here has to survive a hostile reading, because `go test` merges
// the test binary's output into its own stdout: the phrase could have been
// printed by the code under test rather than by a failing host, and matching
// it would launder a genuine kill into a non-result. Generic wordings are
// therefore qualified with the prefix only the real failure produces ("out of
// memory" alone is an ordinary application error message; "fatal error: out
// of memory" is the Go runtime dying). A bare "signal: killed" is absent for
// a different reason: `go test` does write it to this stream when the test
// binary is signalled, but always alone on a line, so it is matched with the
// line anchoring a substring scan cannot express — see unexplainedKill.
//
// The runtime-abort wordings stay despite a mutation being able to provoke
// them in principle, which is a deliberate call rather than an oversight. To
// reach one, a mutated value has to feed a single oversized allocation: an
// allocation that grows across iterations trips the RSS monitor and returns
// TIMED OUT before the runtime gives up. The failure on the other side is
// wider — N parallel `go test` children exhausting a small runner is the
// exact report behind this status — and it lands as a KILLED that gets
// cached, so it outlives the bad host. Between a rare non-result that is
// re-run every time and a common false kill that is never revisited, this
// list prefers the non-result.
var testPhaseInfraSignatures = []string{
	"no space left on device",
	"disk quota exceeded",
	"cannot allocate memory",
	"fatal error: out of memory",
	"runtime: out of memory",
	"runtime: failed to create new os thread",
	"too many open files",
	"read-only file system",
	"input/output error",
	"text file busy",
}

// buildPhaseInfraSignatures are wordings too generic to trust from a running
// test, but unambiguous before one starts. `go: fork/exec …/compile:
// resource temporarily unavailable` (the toolchain unable to spawn the
// compiler) and `ld: out of memory allocating …` are host failures with no
// other reading; both are invisible to the qualified list above.
//
// They are matched only in the build/setup phase, where every byte on the
// captured streams was written by the Go toolchain because the code under
// test has not run yet.
var buildPhaseInfraSignatures = []string{
	"out of memory",
	"resource temporarily unavailable",
}

// infrastructureErrnos are the same class of failures as typed syscall
// errors, for the paths where gomutants itself holds an error value (its
// per-mutant temp-file writes, cmd.Start) instead of a subprocess's text.
// errors.Is on the errno is exact, survives wrapping that rewrites the
// message, and cannot be spoofed by the tested code, so it carries the
// signatures that are too generic to match as free text.
var infrastructureErrnos = []error{
	syscall.ENOSPC,  // no space left on device
	syscall.EDQUOT,  // disk quota exceeded (how a quota'd CI host runs out)
	syscall.ENOMEM,  // cannot allocate memory
	syscall.EMFILE,  // too many open files (per-process limit)
	syscall.ENFILE,  // too many open files (system-wide limit)
	syscall.EROFS,   // read-only file system
	syscall.EIO,     // input/output error
	syscall.EAGAIN,  // fork/thread exhaustion
	syscall.ETXTBSY, // text file busy
}

// testFailureMarker is the per-test line `go test` prints when a test
// function itself reported a failure. Its presence means a test ran and
// detected the mutation, so an infrastructure signature elsewhere in the same
// output is the test's own text rather than the host failing — the mutant
// stays KILLED. A genuine host failure aborts the binary with a runtime
// `fatal error:` or a toolchain message and no per-test failure line. It is
// not the only shape a detected mutation takes, though — see panicMarker.
const testFailureMarker = "--- FAIL: "

// panicMarker begins the runtime's report for a panic that reached the top of
// a goroutine. It settles a mutant as killed for the same reason
// testFailureMarker does, and covers what that marker misses: a panic outside
// the test's own goroutine — a background worker, TestMain, package init —
// aborts the binary on the spot, so `go test` never reaches the point where
// it would print a per-test failure line, and the whole run ends with nothing
// but `FAIL\tpkg\t0.3s`.
//
// Without this, a mutation that provokes such a panic *through* a host
// resource is scored as an environment problem rather than the kill it is:
// drop a `defer f.Close()` and a background goroutine panics with
// "too many open files", which the test-phase list matches. Genuine host
// failures never arrive in this shape — the Go runtime aborts with
// `fatal error:` and an OOM-killer leaves `signal: killed`, neither of which
// starts a line with `panic: `.
const panicMarker = "panic: "

// sigkillMessage is what os/exec reports when the process died on SIGKILL,
// and — because `go test` reports its child the same way — also the line
// `go test` prints when the test binary alone was signalled. Matching the
// text keeps this portable: reading the signal off ProcessState needs a
// syscall.WaitStatus, which does not exist on every platform we build for.
const sigkillMessage = "signal: killed"

// killVetoed reports whether the captured output settles the mutant as a kill
// before any host-failure reasoning runs. Both host-failure paths — a
// signature in the output and an unexplained SIGKILL — consult it, because
// both are claims about a stream the code under test also writes to.
//
// A `--- FAIL: ` line or a `panic: ` line means the mutation was detected,
// which is a kill whatever happened to the process afterwards. Truncation
// makes their absence meaningless (see infraFromOutput), so a clipped stream
// declines to classify as well.
func killVetoed(stdout string, truncated bool) bool {
	return truncated ||
		strings.Contains(stdout, testFailureMarker) ||
		hasLineWithPrefix(stdout, panicMarker)
}

// hasLineWithPrefix reports whether any line of s starts with prefix.
//
// Line anchoring is what separates the two authors of `go test`'s stdout for
// the phrases that carry no qualifying prefix of their own. The toolchain and
// the runtime write theirs at column zero; a test quoting the same words puts
// them inside a message that `go test` indents under a `--- FAIL: ` header,
// or mid-line in its own logging. A plain substring scan cannot tell those
// apart, which is why the phrases matched here are absent from the signature
// lists above.
func hasLineWithPrefix(s, prefix string) bool {
	for line := range strings.Lines(s) {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// unexplainedKill reports whether the test process died on a SIGKILL that
// gomutants did not send. Both signals it does send are classified earlier:
// the RSS monitor's kill arrives as memKilled and the per-mutant deadline as
// context.DeadlineExceeded, and each returns TIMED OUT before this is
// reached. What is left came from outside the run — a cgroup or kernel
// OOM-killer reaping the test binary, or the CI runner tearing the job down.
//
// It takes two readings because the signal lands on one of two processes and
// only one of them is the process gomutants waits on:
//
//   - The `go` parent is signalled (a teardown that sweeps the tree). cmd.Wait
//     reports the death directly, so runErr carries os/exec's "signal: killed".
//   - The test binary alone is signalled. This is the usual shape, because the
//     kernel OOM-killer picks the largest RSS in the cgroup and that is the
//     binary, not the `go` process supervising it. `go test` survives, prints
//     "signal: killed" on a line of its own, and exits 1 — leaving runErr as a
//     bare "exit status 1" with all of the evidence on stdout.
//
// The second is the failure issue #79 reported, and it is the one the RSS
// monitor cannot see: the monitor SIGKILLs at its own 2 GiB ceiling on a 1 s
// poll, so a container whose limit is lower is always killed by the kernel
// first, well before memKilled is ever set. Classified as KILLED it would be
// cached, and cached kills are never revisited — the false score outlives the
// bad host.
//
// Reading stdout means reading a stream the tested code also writes to, so
// the phrase counts only at the start of a line, where `go test` puts it (see
// hasLineWithPrefix). The caller pairs that with killVetoed for the rest.
//
// runErr is never nil here: classifyTestOutcome returns LIVED on a nil error
// long before this is reached, so a nil guard would be dead code (and an
// equivalent mutant).
func unexplainedKill(runErr error, stdout string) bool {
	return strings.Contains(runErr.Error(), sigkillMessage) ||
		hasLineWithPrefix(stdout, sigkillMessage)
}

// matchesAnySignature reports whether already-lowercased text contains any of
// the signatures. Subprocess output and OS errors vary in capitalization, so
// callers lowercase once and match case-insensitively.
func matchesAnySignature(lower string, signatures []string) bool {
	for _, signature := range signatures {
		if strings.Contains(lower, signature) {
			return true
		}
	}
	return false
}

// buildOrSetupFailed reports whether `go test` gave up before running any
// test, which is also what tells us who wrote the captured output.
func buildOrSetupFailed(stdout string) bool {
	return strings.Contains(stdout, "[build failed]") || strings.Contains(stdout, "[setup failed]")
}

// infraFromOutput reports whether subprocess output carries a recognized host
// failure. Which signatures count depends on the phase the output came from,
// because the two phases have different authors:
//
//   - Build/setup: the test binary never ran, so every byte came from the Go
//     toolchain and even the generic wordings are unambiguous.
//   - Test run: `go test` merges the test binary's output into its own
//     stdout, so the tested code could have printed the phrase itself. Only
//     the qualified signatures count.
//
// A reported test failure settles it before either tier: a test detected the
// mutation, which is a kill no matter what else the output holds. That check
// has to come first rather than only in the test-run branch, because the
// build/setup markers are themselves just text on stdout — a project whose
// tests process `go test` output (gomutants' own suite does) prints
// `[build failed]` as fixture data, and reading that as "the toolchain wrote
// this" would hand test-authored text to the wide list. Checking the marker
// before lowercasing also keeps the common killed path from paying for a scan
// of up to 2 MiB of captured output.
//
// truncated says stdout lost bytes to maxCapturedOutput, which makes the
// marker check unsound in the one direction that matters: a verbose suite
// (`-v`, or a mutation that turns the code under test chatty) can push the
// `--- FAIL: ` line past the cap while an infra-looking phrase the test
// printed earlier survives in the retained head, laundering a genuine kill
// into a non-result. Absence of the marker only means "no test failed" when
// the whole stream was seen, so a truncated stream declines to classify and
// the caller falls through to KILLED.
func infraFromOutput(stdout, stderr string, truncated bool) bool {
	if killVetoed(stdout, truncated) {
		return false
	}
	lower := strings.ToLower(stdout + "\n" + stderr)
	if buildOrSetupFailed(stdout) && matchesAnySignature(lower, buildPhaseInfraSignatures) {
		return true
	}
	return matchesAnySignature(lower, testPhaseInfraSignatures)
}

// isInfrastructureErr reports whether err is, or wraps, a recognized host
// resource failure. The errno comparison is authoritative; the message scan
// is a fallback for errors that lost their errno on the way up (a wrapper
// that reformatted the text rather than chaining it). Both signature lists
// apply: an error value from gomutants' own syscalls is never authored by the
// code under test, so the generic wordings are safe here too.
func isInfrastructureErr(err error) bool {
	for _, errno := range infrastructureErrnos {
		if errors.Is(err, errno) {
			return true
		}
	}
	lower := strings.ToLower(err.Error())
	return matchesAnySignature(lower, buildPhaseInfraSignatures) ||
		matchesAnySignature(lower, testPhaseInfraSignatures)
}

// setupErrorStatus classifies a failure in one of gomutants' own per-mutant
// setup steps (staging the patched source, the overlay, or starting the test
// process). A recognized host failure becomes InfraError — the mutation was
// never actually tested, so it must not be cached or scored. Anything else
// keeps the historical NotViable: the mutant could not be staged either way,
// and NotViable already means "no verdict, excluded from efficacy".
func setupErrorStatus(err error) mutator.MutantStatus {
	if isInfrastructureErr(err) {
		return mutator.StatusInfraError
	}
	return mutator.StatusNotViable
}

// writeFileFunc, execCommandContext, and startCommandFunc are package-level
// indirections around process/file operations. Swapping them in tests lets us
// hit the unhappy paths in NewWorker / Worker.Test (write failure, fork/exec
// failure) without contriving filesystem, PATH, or host resource state.
var (
	writeFileFunc      = os.WriteFile
	execCommandContext = exec.CommandContext
	startCommandFunc   = func(cmd *exec.Cmd) error { return cmd.Start() }
)

// ShortFlagFromEnv reports whether the inner `go test` should be invoked
// with -short. Extracted from Worker.Test so the env-string equality check
// is reachable without spinning up a subprocess.
func ShortFlagFromEnv() bool {
	return os.Getenv("GOMUTANTS_TEST_SHORT") == "1"
}

// overlay is the JSON structure for `go test -overlay`.
type overlay struct {
	Replace map[string]string `json:"Replace"`
}

// buildTimeout bounds building a mutant's test binary (see
// Worker.buildBin). It is no deadline sized from anything, only a stop
// for a build that hangs without growing, which the RSS monitor can't
// see — on a module proxy, a stuck -toolexec: far past any real compile
// and link, under any --workers contention. A var so tests can shorten
// it.
var buildTimeout = 10 * time.Minute

// testBinaryArgsFunc reads the arguments `go test` passes a test binary
// (see binArgsCache); a var so tests can stub it.
var testBinaryArgsFunc = coverage.TestBinaryArgs

// binArgsCache holds the arguments every run of a mutant's test binary
// gets: those `go test` would pass it for the run's flags (see
// Worker.binFlags), read once and shared by a pool's workers. They depend
// only on the flags, which are fixed for a run, but reading them takes a
// package with tests, so the first package a worker builds a binary of is
// the one they are read for.
type binArgsCache struct {
	mu   sync.Mutex
	args []string
	read bool
}

// get returns the cached arguments, reading them for pkg on first use. A
// failed read isn't cached, so the next mutant tries again.
func (c *binArgsCache) get(ctx context.Context, projectDir, tags, pkg string, flags []string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.read {
		return c.args, nil
	}
	args, err := testBinaryArgsFunc(ctx, projectDir, tags, pkg, flags)
	if err != nil {
		return nil, err
	}
	c.args, c.read = args, true
	return args, nil
}

// Worker tests a single mutant: it builds the test binary of each package
// whose tests it runs, with the mutant in place through an overlay, then
// runs the binary.
type Worker struct {
	id          int
	tmpSrcPath  string // Stable temp source file for this worker.
	overlayPath string // Stable overlay JSON file for this worker.
	binDir      string // Where the worker's test binaries are built.
	policy      TimeoutPolicy
	sourceCache map[string][]byte // Read-only, shared across workers.
	projectDir  string            // Working directory for go test.
	testMap     *coverage.TestMap // Per-test coverage map (may be nil).

	// bins holds the current mutant's test binaries by package, built on
	// first use (see buildBin) so its routed run and re-check share them;
	// "" marks a package without tests. Test removes them when it returns.
	bins map[string]string

	// binArgs reads and caches what every run of a test binary is passed;
	// a pool's workers share one.
	binArgs *binArgsCache

	// childGOMAXPROCS, if > 0, caps the GOMAXPROCS of each `go test` child.
	// Limits compile + test runtime parallelism per child so N parallel workers
	// don't oversubscribe a NumCPU-core host. Zero means inherit from parent.
	childGOMAXPROCS int

	// testCPU, if > 0, is forwarded to the inner `go test` as `-cpu=N`.
	// Zero omits the flag so go test defaults to GOMAXPROCS. Note: when
	// childGOMAXPROCS > 0, go test silently caps -cpu at that value, so
	// the intended pairing is --workers=1 --test-cpu=N (or any combo
	// where --test-cpu <= NumCPU/--workers).
	testCPU int

	// tags, if non-empty, is forwarded to the inner `go test` as
	// `-tags=<value>` so mutants in build-tag-gated files compile and run.
	// Set by the pool after construction, mirroring testCPU.
	tags string

	// testFlags are the user's --test-flags. Their build flags go to every
	// `go test -c` (see buildArgs), and the test-binary arguments `go test`
	// makes of them to every run of a binary (see binFlags). Empty adds
	// nothing. They land after the flags we set ourselves, so where both
	// spell the same flag the user's value wins (Go's flag parsing takes
	// the last occurrence).
	//
	// That "last one wins" rule is argv-level only, and it is not a
	// general override guarantee: the deadline is enforced out-of-band by
	// the context each run gets in Worker.Test, so a -timeout would not
	// lengthen it. Flags in that class, along with the ones gomutants
	// depends on (-overlay, -run, …), are rejected at the CLI boundary and
	// cannot reach here. Set by the pool after construction, mirroring
	// tags.
	testFlags []string
}

// NewWorker creates a worker with stable temp file paths.
func NewWorker(id int, tmpDir string, policy TimeoutPolicy, sourceCache map[string][]byte, projectDir string, testMap *coverage.TestMap) (*Worker, error) {
	tmpSrc := filepath.Join(tmpDir, fmt.Sprintf("worker-%d.go", id))
	overlayFile := filepath.Join(tmpDir, fmt.Sprintf("overlay-%d.json", id))

	// Create empty files so paths exist.
	if err := writeFileFunc(tmpSrc, nil, 0o644); err != nil {
		return nil, err
	}
	if err := writeFileFunc(overlayFile, nil, 0o644); err != nil {
		return nil, err
	}

	return &Worker{
		id:          id,
		tmpSrcPath:  tmpSrc,
		overlayPath: overlayFile,
		binDir:      tmpDir,
		policy:      policy,
		sourceCache: sourceCache,
		projectDir:  projectDir,
		testMap:     testMap,
		binArgs:     &binArgsCache{},
	}, nil
}

// Test applies a mutation and runs go test, returning the updated mutant.
func (w *Worker) Test(ctx context.Context, m mutator.Mutant) mutator.Mutant {
	start := time.Now()

	// 1. Get original source.
	original, ok := w.sourceCache[m.File]
	if !ok {
		m.Status = mutator.StatusNotViable
		m.Duration = nonZeroSince(start)
		return m
	}

	// 2. Apply patch.
	patched, err := patch.Apply(original, m.StartOffset, m.EndOffset, m.Replacement)
	if err != nil {
		m.Status = mutator.StatusNotViable
		m.Duration = nonZeroSince(start)
		return m
	}

	// 3. Write patched source to worker's temp file.
	if err := writeFileFunc(w.tmpSrcPath, patched, 0o644); err != nil {
		m.Status = setupErrorStatus(err)
		m.Duration = nonZeroSince(start)
		return m
	}

	// 4. Write overlay JSON (absolute paths required).
	ov := overlay{Replace: map[string]string{m.File: w.tmpSrcPath}}
	ovBytes, _ := json.Marshal(ov)
	if err := writeFileFunc(w.overlayPath, ovBytes, 0o644); err != nil {
		m.Status = setupErrorStatus(err)
		m.Duration = nonZeroSince(start)
		return m
	}

	// 5. Run the mutant's covering tests, then, if they all pass, the
	// whole suites its verdict rests on (see recheckGroups). The covering
	// tests are only the likeliest killers: a test that covers the line in
	// package order but not alone is missing from them, so a mutant they
	// miss lives only once the full suites pass too. That costs a second
	// run for survivors alone; killed mutants keep the speed of routing.
	//
	// The routed run gets the adaptive deadline, sized from its tests'
	// timings. Each package of the re-check gets the global ceiling: their
	// timings aren't all known, and a mutant the covering tests pass
	// rarely hangs. Either bounds running the tests only: each test binary
	// is built first, outside it (see runGroups).
	short := ShortFlagFromEnv()
	defer w.removeBins()
	routed := w.routeGroups(m)
	status := w.runGroups(ctx, m, routed, short, w.computeTimeout(m))
	rechecked := false
	if status == mutator.StatusLived {
		if full := w.recheckGroups(m, routed); len(full) > 0 {
			status = w.recheck(ctx, m, full, short)
			rechecked = true
		}
	}
	// Parent-context cancel (Ctrl-C, upstream deadline) propagates via
	// exec.CommandContext as a non-nil cmd.Wait error that is neither
	// memKilled nor the test's own timeout, which classifyTestOutcome
	// would mistake for StatusKilled — silently marking cancelled mutants
	// as tested and inflating efficacy. Preserve the incoming Status + zero
	// Duration so the pool surfaces the mutant as Pending (not tested),
	// keeping Pending ⇒ Duration==0.
	if ctx.Err() != nil {
		return m
	}
	m.Duration = time.Since(start)
	m.Status = status
	m.Rechecked = rechecked
	return m
}

// runGroups runs the tests of each package in groups in turn (see
// invocations), and returns the first outcome that isn't Lived, or Lived
// when every package passes.
//
// Each package's test binary is built with m in place (see buildBin)
// before its tests run, outside the deadline: `timeout` is sized from the
// tests' run time and bounds running them alone, shared by all of groups.
// A rebuild takes no time the tests' timings show — a recompile and a
// relink of the package, which on a heavy package under --workers
// contention outlasts any deadline sized from a fast test — so a
// deadline that covered it would turn a mutant the tests kill or survive
// at once TIMED_OUT, which drops it from the efficacy denominator (#106).
// A build gets only buildTimeout's stop for a hang instead.
//
// A failed build or start ends the run like any other terminal outcome.
// So does a cancelled ctx, which kills the build or run in flight or
// fails the next one's start; the caller discards that outcome.
func (w *Worker) runGroups(ctx context.Context, m mutator.Mutant, groups map[string][]string, short bool, timeout time.Duration) mutator.MutantStatus {
	left := timeout
	for _, run := range invocations(groups, m.Pkg) {
		bin, status := w.buildBin(ctx, run.pkg)
		if status != mutator.StatusLived {
			return status
		}
		// A package without tests has no binary to run: its `go test`
		// reports "no test files" and passes.
		if bin != "" {
			status, ran := w.runBin(ctx, m, run, bin, short, left)
			if status != mutator.StatusLived {
				return status
			}
			left -= ran
		}
	}
	return mutator.StatusLived
}

// recheck runs each package in full in turn (see recheckGroups), the
// mutant's own first, and returns the first outcome that isn't Lived, or
// Lived when every package passes. Each package gets the global ceiling
// to itself. Global is sized from one `go test` of every package at once,
// in parallel, so one deadline shared by suites run one after another
// would run out on a mutant many suites link, turning a survivor
// TIMED_OUT, which drops it from the efficacy denominator. A package the
// routed run built a binary of runs that binary again.
func (w *Worker) recheck(ctx context.Context, m mutator.Mutant, full map[string][]string, short bool) mutator.MutantStatus {
	for _, pkg := range orderRoutePackages(full, m.Pkg) {
		if status := w.runGroups(ctx, m, map[string][]string{pkg: nil}, short, w.policy.Global); status != mutator.StatusLived {
			return status
		}
	}
	return mutator.StatusLived
}

// buildBin returns pkg's test binary with the mutant in place, building it
// on first use, and Lived, or the outcome that ends the mutant's run
// instead. The path is "" for a package without tests, of which `go test
// -c` builds no binary.
//
// The build is outside the mutant's deadline (see runGroups). The RSS
// monitor stops a runaway compile or link, and buildTimeout one that
// hangs: it kills the build's whole process group, so a tool `go` started
// goes with it, and reads as InfraError, as a hung build is the host's
// failure, not the mutant's verdict. ctx still cancels the build.
func (w *Worker) buildBin(ctx context.Context, pkg string) (string, mutator.MutantStatus) {
	if bin, ok := w.bins[pkg]; ok {
		return bin, mutator.StatusLived
	}
	// A file name flattened from the import path can collide (m/api_v1 and
	// m/api/v1 both flatten to m_api_v1), so binaries are numbered instead.
	bin := filepath.Join(w.binDir, fmt.Sprintf("worker-%d-%d.test", w.id, len(w.bins)))
	// One an earlier mutant left here must not stand in for a package
	// without tests, of which the build writes none.
	_ = os.Remove(bin)
	buildCtx, cancel := context.WithTimeout(ctx, buildTimeout)
	defer cancel()
	cmd, stdout, stderr := w.makeCmd(buildCtx, "go", w.projectDir, w.buildArgs(pkg, bin))
	proctree.Bound(cmd)
	runErr, memKilled, err := w.runMonitored(cmd)
	if err != nil {
		return "", w.startFailure(err)
	}
	// The monitor's kill fails the build too, so a nil error is a build
	// that finished.
	if runErr != nil {
		if buildCtx.Err() == context.DeadlineExceeded {
			fmt.Fprintf(os.Stderr, "gomutants: worker %d: building %s took over %v, treating as %s\n", w.id, pkg, buildTimeout, mutator.StatusInfraError)
			return "", mutator.StatusInfraError
		}
		return "", classifyBuildFailure(runErr, memKilled, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(bin); err != nil {
		bin = ""
	}
	if w.bins == nil {
		w.bins = make(map[string]string)
	}
	w.bins[pkg] = bin
	return bin, mutator.StatusLived
}

// removeBins removes the current mutant's test binaries; the next mutant
// builds its own.
func (w *Worker) removeBins() {
	for _, bin := range w.bins {
		if bin != "" {
			_ = os.Remove(bin)
		}
	}
	w.bins = nil
}

// runBin runs bin, the test binary of run.pkg, as `go test` runs it: from
// the package's directory, filtered to run.tests, with the arguments `go
// test` passes for the run's flags (see binArgsCache). The run is cut off
// TIMED_OUT after timeout. It returns the run's outcome and how long the
// run took against timeout: from its deadline's start, after the
// arguments are read, which can wait on another worker's read and is no
// time the tests took.
//
// The binary gets no -test.timeout: it would start its own clock along
// with the deadline's, and its timeout panic, winning the race, would
// read as a kill. The deadline kills the binary's whole process group
// instead (see proctree.Bound), as a process a test started would
// otherwise hold the output open past it.
//
// Its stderr goes to the same buffer as its stdout, as `go test` merges
// them: the classifier reads one stream for a test's own failure markers,
// and a panic is reported on stderr.
func (w *Worker) runBin(ctx context.Context, m mutator.Mutant, run pkgRun, bin string, short bool, timeout time.Duration) (mutator.MutantStatus, time.Duration) {
	dir, ok := w.pkgDir(m, run.pkg)
	if !ok {
		fmt.Fprintf(os.Stderr, "gomutants: worker %d: no directory known for package %s, treating as %s\n", w.id, run.pkg, mutator.StatusInfraError)
		return mutator.StatusInfraError, 0
	}
	binArgs, err := w.binArgs.get(ctx, w.projectDir, w.tags, run.pkg, w.binFlags(short))
	if err != nil {
		fmt.Fprintf(os.Stderr, "gomutants: worker %d: %v, treating as %s\n", w.id, err, mutator.StatusInfraError)
		return mutator.StatusInfraError, 0
	}
	start := time.Now()
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd, output, _ := w.makeCmd(runCtx, bin, dir, runArgs(run.tests, binArgs))
	cmd.Stderr = output
	proctree.Bound(cmd)
	runErr, memKilled, err := w.runMonitored(cmd)
	if err != nil {
		// A deadline that ran out by the start (the routed run's earlier
		// packages took all of it) fails the start with it.
		if runCtx.Err() == context.DeadlineExceeded {
			return mutator.StatusTimedOut, time.Since(start)
		}
		return w.startFailure(err), time.Since(start)
	}
	return classifyTestOutcome(runErr, memKilled, runCtx.Err(), output.String(), "", output.truncated), time.Since(start)
}

// pkgDir returns the directory pkg's test binary runs from: the mutated
// file's for m's own package, the coverage map's for a package of its
// suites. Every package a mutant runs the tests of is one of those, as
// the map routes and re-checks only to its suites.
func (w *Worker) pkgDir(m mutator.Mutant, pkg string) (string, bool) {
	if pkg == m.Pkg {
		return filepath.Dir(m.File), true
	}
	return w.testMap.SuiteDir(pkg)
}

// startFailure classifies a command that failed to start: an
// infrastructure problem (exec/fork failure, PATH misconfig, rlimit), not
// a mutant-viability signal. It is InfraError when it carries a
// recognized signature, NotViable otherwise; either ends the mutant's run.
func (w *Worker) startFailure(err error) mutator.MutantStatus {
	status := setupErrorStatus(err)
	fmt.Fprintf(os.Stderr, "gomutants: worker %d: cmd.Start failed, treating as %s: %v\n", w.id, status, err)
	return status
}

// runMonitored runs cmd (see makeCmd) under the RSS monitor and returns
// how it ended: its Wait error, and whether the monitor killed it. A
// failure to start is returned as startErr, with nothing run (see
// startFailure).
func (w *Worker) runMonitored(cmd *exec.Cmd) (runErr error, memKilled bool, startErr error) {
	if startErr = startCommandFunc(cmd); startErr != nil {
		return
	}

	// Resolve the process-group "handle" we'll later kill if RSS runs away.
	// On Unix this is the child's pgid (with Setpgid:true the parent has
	// already issued setpgid before Start returns on Linux; on macOS it
	// happens in the child post-fork, leaving a brief window where Getpgid
	// may transiently differ from the leader's pid — processGroup falls
	// back to cmd.Process.Pid then). On Windows there is no pgid concept
	// and processGroup returns pid unchanged.
	pgid := processGroup(cmd.Process.Pid)

	var killed atomic.Bool
	monitorDone := make(chan struct{})
	monitorExited := make(chan struct{})
	go func() {
		// 1s cadence: 5 workers × 1 poll/s = 5 ps/sec aggregate (was 25 at
		// 200ms). The 2 GiB cap is loose enough that a 800 ms-later kill is
		// still safe — even on M-series RAM bandwidth a runaway alloc takes
		// ≥1s to add 2 GiB resident. testTimeout (10× baseline) is the
		// outer backstop.
		defer close(monitorExited)
		t := time.NewTicker(monitorPollInterval)
		defer t.Stop()
		for {
			select {
			case <-monitorDone:
				return
			case <-t.C:
				if pgroupRSSBytes(pgid) > maxSubprocRSSBytes {
					killed.Store(true)
					killPgroup(pgid)
					return
				}
			}
		}
	}()

	err := cmd.Wait()
	close(monitorDone)
	// Wait for the monitor goroutine to fully exit before returning. Without
	// this barrier its still-pending reads of psOutputFunc / syscallKillFunc
	// race with the next test's swap of those package-level vars (caught by
	// `go test -race`).
	<-monitorExited
	return err, killed.Load(), nil
}

// makeCmd builds the *exec.Cmd that runs name with args from dir — a
// mutant's `go test -c`, or the test binary it built — plus its capped
// stdout/stderr buffers. Extracted from Worker.Test so each piece of cmd
// configuration (process group, GOMAXPROCS env, capped buffers) can be
// asserted on directly. Without extraction the cmd is local to Test and
// the SysProcAttr / Env mutations are invisible to tests.
func (w *Worker) makeCmd(ctx context.Context, name, dir string, args []string) (*exec.Cmd, *cappedBuffer, *cappedBuffer) {
	cmd := execCommandContext(ctx, name, args...)
	cmd.Dir = dir
	// Put go test -c + its compiler, or the test binary + what its tests
	// start, in their own process group so we can kill the whole tree if
	// RSS runs away.
	// applyProcessGroup is platform-specific (Setpgid on Unix,
	// CREATE_NEW_PROCESS_GROUP on Windows).
	applyProcessGroup(cmd)
	if w.childGOMAXPROCS > 0 {
		// exec auto-sets PWD=cmd.Dir only when cmd.Env is nil (see Go's
		// exec.go ~L1220). When we set Env explicitly the child inherits the
		// parent's stale PWD, which breaks module-relative paths. Mirror the
		// auto-PWD behavior plus our GOMAXPROCS cap.
		cmd.Env = append(os.Environ(),
			"PWD="+cmd.Dir,
			fmt.Sprintf("GOMAXPROCS=%d", w.childGOMAXPROCS),
		)
	}
	stdout := &cappedBuffer{}
	stderr := &cappedBuffer{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd, stdout, stderr
}

// buildArgs constructs the `go test -c` argv that builds pkg's test binary
// to bin with the mutant's overlay in place. Kept as a distinct builder so
// callers can verify the flag and package wiring without spinning up a
// subprocess.
//
// Of the user's --test-flags only the build flags apply (see
// coverage.BuildFlags): `go test -c` rejects any flag it doesn't know,
// such as a property framework's -rapid.checks=100, where `go test` would
// hand it to the test binary; those reach the binary through binFlags.
// They go last, after the package, as `go test` reads them there too (see
// binFlags), and Go takes the last occurrence of a repeated flag, so a
// user value still beats ours.
func (w *Worker) buildArgs(pkg, bin string) []string {
	// -vet=off: vet runs in the user's CI on clean source; re-running it
	// per mutant is pure overhead. Measured ~17–39% per-mutant wall-clock
	// reduction on representative packages.
	args := []string{"test", "-c", "-o", bin, "-vet=off", "-overlay=" + w.overlayPath}
	if w.tags != "" {
		args = append(args, "-tags="+w.tags)
	}
	args = append(args, pkg)
	return append(args, coverage.BuildFlags(w.testFlags)...)
}

// binFlags returns the `go test` flags a mutant's runs of its test
// binaries are run as: ours, then the user's --test-flags. binArgsCache
// reads, through `go test -n`, the binary arguments `go test` makes of
// them, so test flags are rewritten to -test.X and -args split exactly
// as `go test` would.
func (w *Worker) binFlags(short bool) []string {
	flags := []string{"-failfast"}
	if w.testCPU > 0 {
		flags = append(flags, fmt.Sprintf("-cpu=%d", w.testCPU))
	}
	// GOMUTANTS_TEST_SHORT=1 propagates -short to the tests, letting the
	// target suite skip heavy integration tests. Used for gomutants
	// self-testing to avoid recursive worker-pool fanout.
	if short {
		flags = append(flags, "-short")
	}
	return append(flags, w.testFlags...)
}

// runArgs returns the argv of one run of a test binary: the -test.run
// filter for tests, or none to run them all when tests is nil, then
// binArgs (see binArgsCache). Ours go first: a positional argument among
// binArgs (after a user's -args) ends the binary's flag parsing.
func runArgs(tests, binArgs []string) []string {
	var args []string
	if tests != nil {
		args = append(args, "-test.run="+coverage.RunPattern(tests))
	}
	return append(args, binArgs...)
}

// routeGroups returns the packages to run first for m, each mapped to the
// covering tests to run in it: the tests the per-test coverage map
// attributes to m's line, grouped by package, or, when it has none, m's
// whole own package (a nil entry runs a package in full). With the map
// built across packages (integration mode) the covering tests can sit in
// other packages than m's.
func (w *Worker) routeGroups(m mutator.Mutant) map[string][]string {
	// TestRefsFor is nil-safe, so a nil map leaves only the fallback.
	groups := map[string][]string{}
	for _, ref := range w.testMap.TestRefsFor(m.CoverageFile, m.Line) {
		groups[ref.Pkg] = append(groups[ref.Pkg], ref.Name)
	}
	if len(groups) == 0 {
		groups[m.Pkg] = nil
	}
	return groups
}

// recheckGroups returns the packages to run in full for m once the run of
// `routed` (see routeGroups) has passed: every package whose suite can
// kill it (see coverage.TestMap.SuitePkgs) and every package `routed` ran
// only some tests of, less those `routed` already ran in full. Empty means
// the routed run already was the full verdict. m's own package is among
// them only when it has tests: a package tested only by its importers
// would cost a build and link for "no test files".
func (w *Worker) recheckGroups(m mutator.Mutant, routed map[string][]string) map[string][]string {
	full := map[string][]string{}
	for pkg, tests := range routed {
		if tests != nil {
			full[pkg] = nil
		}
	}
	for _, p := range w.testMap.SuitePkgs(m.Pkg) {
		if tests, ran := routed[p.ImportPath]; !ran || tests != nil {
			full[p.ImportPath] = nil
		}
	}
	return full
}

// pkgRun is one package's part of a mutant's run: the tests to run in it,
// or nil to run all of them.
type pkgRun struct {
	pkg   string
	tests []string
}

// invocations returns the ordered runs of groups, one per package.
//
// Per-package runs are required because a -run filter names tests
// independently per package and a failing test ends only its own
// package's binary; one filter across packages would mis-route same-named
// tests and run every package even after one already killed the mutant.
// The mutant's own package is ordered first so the cheapest, most likely
// killer runs before any cross-package suite.
func invocations(groups map[string][]string, ownPkg string) []pkgRun {
	runs := make([]pkgRun, 0, len(groups))
	for _, pkg := range orderRoutePackages(groups, ownPkg) {
		runs = append(runs, pkgRun{pkg: pkg, tests: groups[pkg]})
	}
	return runs
}

// orderRoutePackages returns the covering packages with the mutant's own
// package first (when present), then the rest in deterministic sorted order.
// Same-package tests are the cheapest and likeliest to kill, so running them
// first minimizes wasted cross-package work before a short-circuit.
func orderRoutePackages(groups map[string][]string, ownPkg string) []string {
	rest := make([]string, 0, len(groups))
	for pkg := range groups {
		if pkg != ownPkg {
			rest = append(rest, pkg)
		}
	}
	slices.Sort(rest)
	if _, ok := groups[ownPkg]; ok {
		return append([]string{ownPkg}, rest...)
	}
	return rest
}

// computeTimeout resolves the per-mutant deadline for `m` from the
// worker's policy and testMap. Extracted from Worker.Test so the wiring
// — that the policy's TestMap argument is in fact w.testMap, not nil —
// is unit-testable without spinning up a real subprocess. A regression
// where a refactor passed nil here would silently downgrade every
// mutant to the global ceiling and pass the existing tests.
func (w *Worker) computeTimeout(m mutator.Mutant) time.Duration {
	return w.policy.For(w.testMap, m)
}

// classifyTestOutcome decides a mutant's terminal status from the raw
// subprocess outcome. Pure function so the branching can be unit-tested
// without staging actual test failures.
//
// Priority order:
//  1. memKilled → TimedOut (RSS monitor SIGKILL'd the tree).
//  2. runErr == nil → Lived (tests all passed with the mutant applied).
//  3. testCtxErr == DeadlineExceeded → TimedOut.
//  4. stderr carries a `file.go:N:N:` compile error AND stdout shows
//     `[build failed]` / `[setup failed]` → NotViable.
//  5. output carries a recognized infrastructure signature for the phase it
//     came from, and was captured whole (see infraFromOutput) → InfraError.
//     Step 4 running first is what makes the build-phase tier safe: a build
//     that failed *with* a compile diagnostic is the mutation's doing and has
//     already returned, so what reaches step 5 is a build that broke with
//     nothing to say about the code.
//  6. the `go` process or the test binary under it died on a SIGKILL
//     gomutants did not send (see unexplainedKill) → InfraError. Steps 1 and
//     3 have already taken both signals it does send, so this is an outside
//     hand.
//  7. Otherwise → Killed.
func classifyTestOutcome(runErr error, memKilled bool, testCtxErr error, stdout, stderr string, truncated bool) mutator.MutantStatus {
	if memKilled {
		return mutator.StatusTimedOut
	}
	if runErr == nil {
		return mutator.StatusLived
	}
	if testCtxErr == context.DeadlineExceeded {
		return mutator.StatusTimedOut
	}
	if compileErrorRe.MatchString(stderr) && buildOrSetupFailed(stdout) {
		return mutator.StatusNotViable
	}
	if infraFromOutput(stdout, stderr, truncated) {
		return mutator.StatusInfraError
	}
	if unexplainedKill(runErr, stdout) && !killVetoed(stdout, truncated) {
		return mutator.StatusInfraError
	}
	return mutator.StatusKilled
}

// classifyBuildFailure decides the status of a mutant whose test binary
// failed to build (see Worker.buildBin). No test has run, so every byte of
// the output is the toolchain's: a compile diagnostic is the mutation's
// doing, and both signature lists, the generic build-phase wordings too,
// mean the host failed (see infraFromOutput for the narrower reading a
// test run's output gets). So does a SIGKILL gomutants didn't send.
//
// That SIGKILL usually lands on a tool rather than on `go`: the kernel
// OOM-killer picks the largest RSS, a compiler or linker. `go` survives
// it, exits 1 with nothing on stdout, and reports the tool's death on
// stderr mid-line ("pkg.test: …/link: signal: killed"), so stderr is read
// for it anywhere, not only at the start of a line as unexplainedKill
// reads a test's output, which the tested code can also write.
//
// Priority order:
//  1. memKilled → TimedOut (RSS monitor SIGKILL'd the build).
//  2. stderr carries a `file.go:N:N:` compile error → NotViable.
//  3. a recognized infrastructure signature, or an unexplained SIGKILL of
//     `go` or a tool it ran → InfraError.
//  4. Otherwise → Killed, as such a failure of the whole `go test` read.
func classifyBuildFailure(runErr error, memKilled bool, stdout, stderr string) mutator.MutantStatus {
	if memKilled {
		return mutator.StatusTimedOut
	}
	if compileErrorRe.MatchString(stderr) {
		return mutator.StatusNotViable
	}
	lower := strings.ToLower(stdout + "\n" + stderr)
	if matchesAnySignature(lower, buildPhaseInfraSignatures) ||
		matchesAnySignature(lower, testPhaseInfraSignatures) ||
		unexplainedKill(runErr, stdout) ||
		strings.Contains(stderr, sigkillMessage) {
		return mutator.StatusInfraError
	}
	return mutator.StatusKilled
}
