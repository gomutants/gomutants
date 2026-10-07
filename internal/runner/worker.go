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
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/szhekpisov/gomutants/internal/coverage"
	"github.com/szhekpisov/gomutants/internal/mutator"
	"github.com/szhekpisov/gomutants/internal/patch"
	"github.com/szhekpisov/gomutants/internal/schemata"
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

// shortFlagFromEnv reports whether the inner `go test` should be invoked
// with -short. Extracted from Worker.Test so the env-string equality check
// is reachable without spinning up a subprocess.
func shortFlagFromEnv() bool {
	return os.Getenv("GOMUTANTS_TEST_SHORT") == "1"
}

// overlay is the JSON structure for `go test -overlay`.
type overlay struct {
	Replace map[string]string `json:"Replace"`
}

// Worker tests a single mutant using go test with overlay.
type Worker struct {
	id          int
	tmpSrcPath  string // Stable temp source file for this worker.
	overlayPath string // Stable overlay JSON file for this worker.
	policy      TimeoutPolicy
	sourceCache map[string][]byte // Read-only, shared across workers.
	projectDir  string            // Working directory for go test.
	testMap     *coverage.TestMap // Per-test coverage map (may be nil).

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

	// testFlags are the user's --test-flags, appended verbatim to every
	// inner `go test` argv. Empty appends nothing. They land last of all —
	// after the flags we set ourselves *and* after the package argument, so
	// where both spell the same flag the user's value wins (Go's flag
	// parsing takes the last occurrence) and a flag `go test` does not
	// recognize cannot demote the package we meant to test into a
	// positional argument for the test binary.
	//
	// That "last one wins" rule is argv-level only, and it is not a
	// general override guarantee: -timeout is also enforced out-of-band by
	// the context deadline in Worker.Test, so lengthening it via argv
	// alone would not work. Flags in that class, along with the ones
	// gomutants depends on (-overlay, -run, …), are rejected at the
	// CLI boundary and cannot reach here. Set by the pool after
	// construction, mirroring tags.
	testFlags []string

	// schemata, when non-nil, holds the mutants compiled into prebuilt test
	// binaries. Those run straight from the binary with the mutant selected
	// by environment variable, skipping the per-mutant compile and link.
	// Mutants the plan declined take the overlay path below unchanged. Set
	// by the pool after construction, mirroring tags.
	schemata *schemata.Plan
}

// invocation is one command that decides a mutant's fate: either a
// `go test` run against a per-mutant overlay, or a prebuilt schema test
// binary with the mutant selected through the environment.
type invocation struct {
	path string // Executable; "go" on the overlay path.
	args []string
	dir  string   // Working directory.
	env  []string // Extra environment entries, appended last.
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
		policy:      policy,
		sourceCache: sourceCache,
		projectDir:  projectDir,
		testMap:     testMap,
	}, nil
}

// Test applies a mutation and runs the tests, returning the updated mutant.
//
// A mutant the schemata plan compiled into a prebuilt test binary skips the
// staging below entirely: the mutation is already in the binary, inert until
// its id is named in the environment. Everything else — routing, the
// per-mutant deadline, the RSS watchdog, classification — is shared, so the
// two paths cannot disagree about a verdict.
func (w *Worker) Test(ctx context.Context, m mutator.Mutant) mutator.Mutant {
	start := time.Now()

	if w.schemata.Schematized(m.ID) {
		// Computed once and threaded into both the invocations' own
		// -test.timeout and the context deadline, for the same reason the
		// overlay path below does it: two calls could not desync today, but
		// nothing would catch it if they ever did.
		timeout := w.computeTimeout(m)
		if invs, ok := w.schemaInvocations(ctx, m, timeout); ok {
			return w.runInvocations(ctx, m, invs, timeout, start)
		}
		// The binary could not be built (a covering package with no tests,
		// a compile that only fails when linked). Fall through: the overlay
		// path still produces an honest verdict.
	}

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

	// 5. Compute the per-mutant timeout once and reuse it for both the
	// outer context deadline (which feeds exec.CommandContext's
	// SIGKILL-on-expiry) and the inner -timeout flag (which lets `go test`
	// exit cleanly with its own timeout error). Computing once means an
	// odd-shaped TimeoutPolicy can't desync the two. The single deadline is
	// shared across every per-package invocation: computeTimeout already
	// sizes it from the sum of all covering tests' durations (across
	// packages), so the whole cross-package run is bounded as one unit.
	timeout := w.computeTimeout(m)

	// 6. Run each covering package's invocation in turn, short-circuiting on
	// the first non-Lived outcome (a kill, timeout, or compile failure). A
	// mutant is Lived only if every covering package's tests pass. A
	// cmd.Start failure surfaces as NotViable or InfraError from
	// runMutantTest, which the non-Lived check below returns just like any
	// other terminal outcome.
	return w.runInvocations(ctx, m, w.testInvocations(m, shortFlagFromEnv(), timeout), timeout, start)
}

// runInvocations executes a mutant's invocations under one shared deadline,
// short-circuiting on the first non-Lived outcome.
func (w *Worker) runInvocations(ctx context.Context, m mutator.Mutant, invs []invocation, timeout time.Duration, start time.Time) mutator.Mutant {
	testCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for _, inv := range invs {
		status := w.runMutantTest(testCtx, inv)
		// Parent-context cancel (Ctrl-C, upstream deadline) propagates via
		// exec.CommandContext as a non-nil cmd.Wait error that is neither
		// memKilled nor the test's own timeout, which classifyTestOutcome
		// would mistake for StatusKilled — silently marking cancelled mutants
		// as tested and inflating efficacy. Detect parent cancel here and
		// preserve the incoming Status + zero Duration so the pool surfaces
		// the mutant as Pending (not tested), keeping Pending ⇒ Duration==0.
		if ctx.Err() != nil {
			return m
		}
		if status != mutator.StatusLived {
			m.Duration = time.Since(start)
			m.Status = status
			return m
		}
	}

	m.Duration = time.Since(start)
	m.Status = mutator.StatusLived
	return m
}

// runMutantTest runs one `go test` invocation under the RSS monitor and
// returns its classified status. A cmd.Start failure (an infrastructure
// problem — exec/fork failure, PATH misconfig, rlimit — not a mutant-viability
// signal) is reported as InfraError when it carries a recognized signature,
// or NotViable otherwise. The caller treats either as a terminal outcome.
//
// Extracted from Worker.Test so the integration path can drive it once per
// covering package while sharing a single per-mutant deadline.
func (w *Worker) runMutantTest(testCtx context.Context, inv invocation) mutator.MutantStatus {
	cmd, stdout, stderr := w.makeCmd(testCtx, inv)

	if err := startCommandFunc(cmd); err != nil {
		status := setupErrorStatus(err)
		fmt.Fprintf(os.Stderr, "gomutants: worker %d: cmd.Start failed, treating as %s: %v\n", w.id, status, err)
		return status
	}

	// Resolve the process-group "handle" we'll later kill if RSS runs away.
	// On Unix this is the child's pgid (with Setpgid:true the parent has
	// already issued setpgid before Start returns on Linux; on macOS it
	// happens in the child post-fork, leaving a brief window where Getpgid
	// may transiently differ from the leader's pid — processGroup falls
	// back to cmd.Process.Pid then). On Windows there is no pgid concept
	// and processGroup returns pid unchanged.
	pgid := processGroup(cmd.Process.Pid)

	var memKilled atomic.Bool
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
					memKilled.Store(true)
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

	// Only stdout's truncation flag is passed on: the `--- FAIL: ` marker that
	// vetoes infra classification is printed on stdout, so a dropped tail
	// there is what makes its absence unsound. Losing the tail of stderr can
	// only hide a signature, which fails safe as KILLED.
	return classifyTestOutcome(err, memKilled.Load(), testCtx.Err(), stdout.String(), stderr.String(), stdout.truncated)
}

// makeTestCmd builds the *exec.Cmd that runs the mutated `go test` plus
// its capped stdout/stderr buffers. Extracted from Worker.Test so each
// piece of cmd configuration (process group, GOMAXPROCS env, capped
// buffers) can be asserted on directly. Without extraction the cmd is
// local to Test and the SysProcAttr / Env mutations are invisible to tests.
func (w *Worker) makeTestCmd(ctx context.Context, args []string) (*exec.Cmd, *cappedBuffer, *cappedBuffer) {
	return w.makeCmd(ctx, invocation{path: "go", args: args, dir: w.projectDir})
}

// makeCmd is makeTestCmd generalized over the executable, so a prebuilt
// schema test binary gets the same process-group, GOMAXPROCS and
// output-capping treatment as `go test`.
func (w *Worker) makeCmd(ctx context.Context, inv invocation) (*exec.Cmd, *cappedBuffer, *cappedBuffer) {
	cmd := execCommandContext(ctx, inv.path, inv.args...)
	cmd.Dir = inv.dir
	// Put go test + its compiler + test-binary descendants in their own
	// process group so we can kill the whole tree if RSS runs away.
	// applyProcessGroup is platform-specific (Setpgid on Unix,
	// CREATE_NEW_PROCESS_GROUP on Windows).
	applyProcessGroup(cmd)
	if w.childGOMAXPROCS > 0 || len(inv.env) > 0 {
		// exec auto-sets PWD=cmd.Dir only when cmd.Env is nil (see Go's
		// exec.go ~L1220). When we set Env explicitly the child inherits the
		// parent's stale PWD, which breaks module-relative paths. Mirror the
		// auto-PWD behavior plus our GOMAXPROCS cap.
		cmd.Env = append(os.Environ(), "PWD="+cmd.Dir)
		if w.childGOMAXPROCS > 0 {
			cmd.Env = append(cmd.Env, fmt.Sprintf("GOMAXPROCS=%d", w.childGOMAXPROCS))
		}
		cmd.Env = append(cmd.Env, inv.env...)
	}
	stdout := &cappedBuffer{}
	stderr := &cappedBuffer{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd, stdout, stderr
}

// baseTestArgs builds the flag-only prefix shared by every `go test`
// invocation for a mutant — everything except the `-run` filter, the
// package argument, and the user's --test-flags that follow it. Split out
// so both the single-package builder and the per-package integration
// builder share one source of truth for the flag wiring (and so each flag
// stays unit-testable).
//
// `timeout` is the resolved per-mutant deadline (computed by the caller
// from TimeoutPolicy), threaded into both the outer context and the
// `-timeout=` flag.
func (w *Worker) baseTestArgs(short bool, timeout time.Duration) []string {
	// -vet=off: vet runs in the user's CI on clean source; re-running it
	// per mutant is pure overhead. Measured ~17–39% per-mutant wall-clock
	// reduction on representative packages.
	args := []string{"test", "-count=1", "-failfast", "-vet=off",
		fmt.Sprintf("-timeout=%s", timeout),
		fmt.Sprintf("-overlay=%s", w.overlayPath),
	}
	if w.testCPU > 0 {
		args = append(args, fmt.Sprintf("-cpu=%d", w.testCPU))
	}
	if w.tags != "" {
		args = append(args, "-tags="+w.tags)
	}
	// GOMUTANTS_TEST_SHORT=1 propagates -short to inner go test, letting the
	// target suite skip heavy integration tests. Used for gomutants self-testing
	// to avoid recursive worker-pool fanout.
	if short {
		args = append(args, "-short")
	}
	return args
}

// buildTestArgs constructs the `go test` argv for the single-package case:
// the mutant's own package, optionally filtered to its covering tests. Used
// when no cross-package routing applies (the common path). Kept as a
// distinct builder so callers can verify the -short, -run, and package arg
// wiring without spinning up a subprocess.
//
// The user's --test-flags go last, after the package. `go test` goes on
// parsing its own flags past one it does not recognize, but that first
// unrecognized flag marks the package list as already seen, so a package
// name after it is forwarded to the test binary as a positional argument
// and `go test` falls back to `.`. A test-binary flag ahead of the
// package therefore leaves the working directory to be tested and every
// mutant reported LIVED. Trailing placement also preserves the override
// rule — Go takes the last occurrence of a repeated flag, so a user value
// still beats ours.
func (w *Worker) buildTestArgs(m mutator.Mutant, short bool, timeout time.Duration) []string {
	args := w.baseTestArgs(short, timeout)
	// Use per-test coverage map to run only relevant tests.
	if w.testMap != nil {
		if tests := w.testMap.TestsFor(m.CoverageFile, m.Line); len(tests) > 0 {
			args = append(args, fmt.Sprintf("-run=%s", coverage.RunPattern(tests)))
		}
	}
	args = append(args, m.Pkg)
	return append(args, w.testFlags...)
}

// testInvocations returns the ordered set of `go test` argv lists to run for
// one mutant. In the common case this is a single invocation against the
// mutant's own package (identical to buildTestArgs). When the per-test
// coverage map routes the mutant to covering tests in *other* packages
// (integration mode), it returns one invocation per covering package, each
// filtered to that package's covering tests.
//
// Per-package invocations are required because `go test -run` applies its
// regex independently per package and `-failfast` does not short-circuit
// across packages; a single multi-package invocation would mis-route
// same-named tests and run every package even after one already killed the
// mutant. The mutant's own package is ordered first so the cheapest, most
// likely killer runs before any cross-package suite.
func (w *Worker) testInvocations(m mutator.Mutant, short bool, timeout time.Duration) []invocation {
	groups := map[string][]string{}
	if w.testMap != nil {
		for _, ref := range w.testMap.TestRefsFor(m.CoverageFile, m.Line) {
			groups[ref.Pkg] = append(groups[ref.Pkg], ref.Name)
		}
	}

	// No routing info: run the whole of the mutant's own package. (When the
	// only covering package is the mutant's own, the general loop below
	// produces the same single invocation, so no special-case is needed.)
	if len(groups) == 0 {
		return []invocation{w.goInvocation(w.buildTestArgs(m, short, timeout))}
	}

	base := w.baseTestArgs(short, timeout)
	invs := make([]invocation, 0, len(groups))
	for _, pkg := range orderRoutePackages(groups, m.Pkg) {
		args := append(slices.Clone(base),
			fmt.Sprintf("-run=%s", coverage.RunPattern(groups[pkg])), pkg)
		// User flags trail the package here for the same reason as in
		// buildTestArgs.
		args = append(args, w.testFlags...)
		invs = append(invs, w.goInvocation(args))
	}
	return invs
}

// goInvocation wraps `go test` arguments as an invocation.
func (w *Worker) goInvocation(args []string) invocation {
	return invocation{path: "go", args: args, dir: w.projectDir}
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
	if groups[ownPkg] != nil {
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

// schemaInvocations builds the commands for a mutant that lives in a
// prebuilt test binary. Routing is identical to the overlay path — one
// invocation per covering package, the mutant's own package first — but
// each one execs the package's schema binary directly instead of `go test`,
// with the mutant selected through GOMUTANTS_ACTIVE.
//
// It reports false when any needed binary is missing, so the caller can
// fall back rather than lose a verdict.
func (w *Worker) schemaInvocations(ctx context.Context, m mutator.Mutant, timeout time.Duration) ([]invocation, bool) {
	groups := map[string][]string{}
	if w.testMap != nil {
		for _, ref := range w.testMap.TestRefsFor(m.CoverageFile, m.Line) {
			groups[ref.Pkg] = append(groups[ref.Pkg], ref.Name)
		}
	}
	pkgs := orderRoutePackages(groups, m.Pkg)
	if len(groups) == 0 {
		// No routing information: run the whole of the mutant's own package.
		pkgs = []string{m.Pkg}
	}

	invs := make([]invocation, 0, len(pkgs))
	for _, pkg := range pkgs {
		bin, dir, err := w.schemata.Binary(ctx, pkg)
		if err != nil {
			return nil, false
		}
		invs = append(invs, invocation{
			path: bin,
			args: schemaTestArgs(groups[pkg], shortFlagFromEnv(), timeout, w.testCPU),
			dir:  dir,
			env:  []string{schemata.ActiveEnv + "=" + strconv.Itoa(m.ID)},
		})
	}
	return invs, true
}

// schemaTestArgs mirrors baseTestArgs for a prebuilt binary. The `go test`
// spellings become the testing package's own `-test.` flags; the build-time
// ones (-vet, -tags, -overlay) were already applied when the binary was
// compiled.
//
// The user's --test-flags are deliberately absent: they are `go test`
// arguments with no general translation to binary flags, which is why
// schemata is switched off whenever they are set.
func schemaTestArgs(tests []string, short bool, timeout time.Duration, testCPU int) []string {
	args := []string{
		"-test.count=1",
		"-test.failfast",
		fmt.Sprintf("-test.timeout=%s", timeout),
	}
	if pattern := coverage.RunPattern(tests); pattern != "" {
		args = append(args, "-test.run="+pattern)
	}
	if testCPU > 0 {
		args = append(args, fmt.Sprintf("-test.cpu=%d", testCPU))
	}
	if short {
		args = append(args, "-test.short")
	}
	return args
}
