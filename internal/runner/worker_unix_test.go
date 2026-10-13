//go:build !windows

package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gomutants/gomutants/internal/coverage"
	"github.com/gomutants/gomutants/internal/mutator"
	"github.com/gomutants/gomutants/internal/proctree"
)

// TestPgroupRSSBytesSelf runs the real probe (/proc on Linux, ps
// elsewhere) against our own process group. Kills, in the platform's
// groupRSSFunc, any mutant that loses the group's rows or its unit
// conversion: a dropped match, a wrong field index, or `/` for the page or
// KB multiply all collapse the total toward 0.
//
// We assert the returned value exceeds 1 MiB — real Go test processes use
// tens of MiB, but all the mutations above collapse the result toward 0.
func TestPgroupRSSBytesSelf(t *testing.T) {
	pgid, err := syscall.Getpgid(os.Getpid())
	if err != nil {
		t.Fatalf("Getpgid: %v", err)
	}
	bytes := pgroupRSSBytes(pgid)
	if bytes < 1<<20 {
		t.Errorf("pgroupRSSBytes(self pgid=%d) = %d bytes, want >= 1 MiB (a running Go test process is typically tens of MiB)", pgid, bytes)
	}
	// Sanity upper bound — catches mutations that inflate the total.
	if bytes > 100*(1<<30) {
		t.Errorf("pgroupRSSBytes(self) = %d bytes, implausibly large", bytes)
	}
}

// TestPgroupRSSBytesNewProcessGroup probes a process group shaped like a
// mutant's: a child started with Setpgid (see applyProcessGroup), so its
// pgid is its own pid while its session stays ours. TestPgroupRSSBytesSelf
// can't tell a process-group lookup from a session lookup when the two ids
// coincide, as they do under a shell without job control; here they never
// do. GNU ps reads a numeric `-g` as a session list, so on Linux the
// lookup matched nothing and the RSS monitor never tripped (issue #107).
func TestPgroupRSSBytesNewProcessGroup(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	applyProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("Getpgid: %v", err)
	}
	if got := pgroupRSSBytes(pgid); got <= 0 {
		t.Errorf("pgroupRSSBytes(child pgid=%d) = %d, want > 0", pgid, got)
	}
}

// deadPgid is a pgid no process can have: it is above pid_max on Linux
// (at most 2^22) and on macOS (99999).
const deadPgid = 99999999

// resetRSSProbe clears the probe's shared health now and after the test,
// so another test's failures can't count toward this one's outage.
func resetRSSProbe(t *testing.T) {
	t.Helper()
	reset := func() {
		rssProbe.fails.Store(0)
		rssProbe.down.Store(false)
	}
	reset()
	t.Cleanup(reset)
}

// probeThroughOutage polls pgid one more time than an outage takes to
// report, so a test can assert both that it was reported and that it was
// reported once. It returns the stderr and every poll's result.
func probeThroughOutage(t *testing.T, pgid int) (stderr string, got []int64) {
	t.Helper()
	stderr = captureStderr(t, func() {
		for range rssProbeOutageAfter + 1 {
			got = append(got, pgroupRSSBytes(pgid))
		}
	})
	return stderr, got
}

// TestPgroupRSSBytesInvalidPgid probes a pgid no process has. The real
// probe lists no row for it, and the group is gone rather than hidden, so
// the result must be exactly 0 with no warning, however often it's polled.
func TestPgroupRSSBytesInvalidPgid(t *testing.T) {
	resetRSSProbe(t)
	out, got := probeThroughOutage(t, deadPgid)
	if slices.ContainsFunc(got, func(n int64) bool { return n != 0 }) {
		t.Errorf("pgroupRSSBytes(invalid) = %v, want all 0", got)
	}
	if out != "" {
		t.Errorf("stderr = %q, want nothing — the group doesn't exist, the probe didn't fail", out)
	}
}

// TestPgroupRSSBytesProbeError pins how pgroupRSSBytes handles its probe.
// A failed probe (no ps binary, no /proc) leaves the RSS cap unable to
// fire, which once went unreported. Kills:
//   - CONDITIONALS_NEGATION / BRANCH_IF on `err == nil`: no warning on
//     failure, or one on success.
//   - INVERT_LOGICAL on the `&&` of the not-listed check: a plain failure
//     on a group that is gone stays silent.
//   - an error path that returns the probe's n: a partial total reaches
//     the cap check.
func TestPgroupRSSBytesProbeError(t *testing.T) {
	orig := groupRSSFunc
	t.Cleanup(func() { groupRSSFunc = orig })

	t.Run("success passes the total through, silently", func(t *testing.T) {
		resetRSSProbe(t)
		groupRSSFunc = func(int) (int64, error) { return 7 * 1024, nil }
		var got int64
		out := captureStderr(t, func() { got = pgroupRSSBytes(0) })
		if got != 7*1024 {
			t.Errorf("got %d, want %d", got, 7*1024)
		}
		if out != "" {
			t.Errorf("stderr = %q, want nothing on success", out)
		}
	})

	t.Run("failure warns once and reads 0", func(t *testing.T) {
		resetRSSProbe(t)
		// A partial total alongside the error, and a group that is gone:
		// neither may matter once the probe itself has failed.
		groupRSSFunc = func(int) (int64, error) {
			return 5 * 1024, errors.New("fork/exec /bin/ps: no such file or directory")
		}
		out, got := probeThroughOutage(t, deadPgid)
		if slices.ContainsFunc(got, func(n int64) bool { return n != 0 }) {
			t.Errorf("got %v, want all 0", got)
		}
		if n := strings.Count(out, "RSS cap is off"); n != 1 {
			t.Errorf("warned %d times, want once; stderr:\n%s", n, out)
		}
		if !strings.Contains(out, "no such file or directory") {
			t.Errorf("warning doesn't name the cause; stderr:\n%s", out)
		}
	})
}

// TestPgroupRSSBytesOutage scripts the probe's health poll by poll and pins
// what each poll prints: an outage is reported once it is
// rssProbeOutageAfter failures long, once per outage, and its end once; a
// group that exited between tick and scan neither ends an outage nor adds
// to one. Kills:
//   - CONDITIONALS_BOUNDARY / INTEGER_* on `>= rssProbeOutageAfter`: the
//     report moves off the third failure in a row.
//   - STATEMENT_REMOVE on `fails.Store(0)`: failures either side of a
//     success add up to an outage.
//   - either CompareAndSwap guard dropped or negated: an outage reported
//     per poll, its end reported per success, or one or the other never.
func TestPgroupRSSBytesOutage(t *testing.T) {
	orig := groupRSSFunc
	t.Cleanup(func() { groupRSSFunc = orig })
	resetRSSProbe(t)

	const (
		ok   = "ok"
		fail = "fail"
		gone = "gone" // unlisted, and deadPgid has no live group
	)
	var next string
	groupRSSFunc = func(int) (int64, error) {
		switch next {
		case ok:
			return 1024, nil
		case fail:
			return 0, errors.New("fork/exec /bin/ps: resource temporarily unavailable")
		default:
			return 0, errGroupNotListed
		}
	}

	const (
		silent = ""
		off    = "RSS cap is off"
		backOn = "RSS cap is back on"
	)
	steps := []struct{ probe, prints string }{
		{fail, silent},
		{fail, silent},
		{gone, silent}, // not a third failure
		{ok, silent},   // no outage to end; the count starts over
		{fail, silent},
		{fail, silent},
		{gone, silent},
		{fail, off}, // third failure in a row: gone didn't reset the count
		{fail, silent},
		{ok, backOn},
		{ok, silent},
		{fail, silent},
		{fail, silent},
		{fail, off}, // a new outage is reported too
	}
	for i, s := range steps {
		next = s.probe
		out := captureStderr(t, func() { pgroupRSSBytes(deadPgid) })
		if s.prints == silent && out != "" || s.prints != silent && !strings.Contains(out, s.prints) {
			t.Fatalf("step %d (%s) printed %q, want %q", i+1, s.probe, out, s.prints)
		}
	}
}

// TestPgroupRSSBytesGroupNotListed pins the probe that runs but lists no
// row for the group, the shape issue #107 hid behind: a live group the
// probe can't see must warn, while one that has exited must not. Kills
// CONDITIONALS_NEGATION / BRANCH_IF on the errors.Is and on `!groupAlive`,
// and in groupAlive `!=` flipped to `==`, INVERT_NEGATIVES on -pgid, and
// INTEGER_INCREMENT on the 0 signal.
func TestPgroupRSSBytesGroupNotListed(t *testing.T) {
	orig := groupRSSFunc
	t.Cleanup(func() { groupRSSFunc = orig })
	groupRSSFunc = func(int) (int64, error) {
		return 0, fmt.Errorf("/proc: %w", errGroupNotListed)
	}

	t.Run("live group warns once", func(t *testing.T) {
		resetRSSProbe(t)
		cmd := exec.Command("sleep", "60")
		applyProcessGroup(cmd)
		if err := cmd.Start(); err != nil {
			t.Fatalf("start sleep: %v", err)
		}
		t.Cleanup(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		})
		pgid, err := syscall.Getpgid(cmd.Process.Pid)
		if err != nil {
			t.Fatalf("Getpgid: %v", err)
		}

		out, got := probeThroughOutage(t, pgid)
		if slices.ContainsFunc(got, func(n int64) bool { return n != 0 }) {
			t.Errorf("got %v, want all 0", got)
		}
		if n := strings.Count(out, "RSS cap is off"); n != 1 {
			t.Errorf("warned %d times, want once; stderr:\n%s", n, out)
		}
		if !strings.Contains(out, errGroupNotListed.Error()) {
			t.Errorf("warning doesn't name the cause; stderr:\n%s", out)
		}

		// The liveness check must only probe: any real signal it sent
		// (signal 1, SIGHUP) would end sleep before our SIGKILL does.
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		_ = cmd.Wait()
		if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || ws.Signal() != syscall.SIGKILL {
			t.Errorf("sleep ended with %v, want SIGKILL — the liveness check signalled the group", cmd.ProcessState)
		}
	})

	// A mutant's group outlives its leader when `go test` exits before the
	// test binary it started: the leader's pid is gone but the group is not,
	// so the check must address the group (-pgid), not the pid.
	t.Run("group whose leader exited still counts as alive", func(t *testing.T) {
		resetRSSProbe(t)
		cmd := exec.Command("sh", "-c", "sleep 60 &")
		applyProcessGroup(cmd)
		if err := cmd.Start(); err != nil {
			t.Fatalf("start sh: %v", err)
		}
		pgid := cmd.Process.Pid // sh leads its own group (Setpgid)
		t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
		if err := cmd.Wait(); err != nil {
			t.Fatalf("sh: %v", err)
		}

		out, _ := probeThroughOutage(t, pgid)
		if n := strings.Count(out, "RSS cap is off"); n != 1 {
			t.Errorf("warned %d times, want once — the group's sleep is alive; stderr:\n%s", n, out)
		}
	})

	t.Run("exited group is silent", func(t *testing.T) {
		resetRSSProbe(t)
		out, got := probeThroughOutage(t, deadPgid)
		if slices.ContainsFunc(got, func(n int64) bool { return n != 0 }) {
			t.Errorf("got %v, want all 0", got)
		}
		if out != "" {
			t.Errorf("stderr = %q, want nothing — the group exited, the probe didn't fail", out)
		}
	})
}

// TestMakeCmdSetpgid kills STATEMENT_REMOVE on `cmd.SysProcAttr =
// &syscall.SysProcAttr{Setpgid: true}` (now wrapped in applyProcessGroup).
// Without it, the child runs in the parent's process group; the RSS
// monitor would mistakenly include the parent and SIGKILL the entire
// test process.
func TestMakeCmdSetpgid(t *testing.T) {
	w := &Worker{projectDir: ".", policy: TimeoutPolicy{Global: time.Second}}
	cmd, _, _ := w.makeCmd(context.Background(), "go", w.projectDir, []string{"version"})
	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr is nil — STATEMENT_REMOVE strips process-group isolation")
	}
	if !cmd.SysProcAttr.Setpgid {
		t.Errorf("Setpgid=false; want true")
	}
}

// TestWorkerTestGetpgidFallback kills CONDITIONALS_NEGATION and BRANCH_IF
// on the `if err != nil { return pid }` fallback inside processGroup.
// Stubbing syscallGetpgidFunc to fail forces the body to fire; we then
// assert that the kill the RSS monitor issues went to a non-zero pid (the
// original fallback). With the body elided, processGroup would return
// Getpgid's zero, killPgroup would send -0 == 0, and no real process is
// signalled.
func TestWorkerTestGetpgidFallback(t *testing.T) {
	dir := setupTestProject(t)
	srcPath := filepath.Join(dir, "add.go")
	src, _ := os.ReadFile(srcPath)
	cache := map[string][]byte{srcPath: src}
	plusIdx := strings.IndexByte(string(src), '+')

	origCap := maxSubprocRSSBytes
	origPoll := monitorPollInterval
	origRSS := groupRSSFunc
	origKill := syscallKillFunc
	origGetpgid := syscallGetpgidFunc
	defer func() {
		maxSubprocRSSBytes = origCap
		monitorPollInterval = origPoll
		groupRSSFunc = origRSS
		syscallKillFunc = origKill
		syscallGetpgidFunc = origGetpgid
	}()
	maxSubprocRSSBytes = 1024
	monitorPollInterval = 50 * time.Millisecond
	groupRSSFunc = func(int) (int64, error) { return 999999 * 1024, nil }

	// Force the Getpgid path to fail so the fallback body must execute.
	syscallGetpgidFunc = func(int) (int, error) {
		return 0, errors.New("inject")
	}

	var killedPid atomic.Int32
	syscallKillFunc = func(pid int, _ syscall.Signal) error {
		// Capture the first non-zero kill pid we see (later polls may also
		// fire before the goroutine returns).
		if killedPid.Load() == 0 {
			killedPid.Store(int32(pid))
		}
		return nil
	}

	w, err := NewWorker(0, t.TempDir(), TimeoutPolicy{Global: 30 * time.Second}, cache, dir, nil)
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	m := mutator.Mutant{
		ID: 1, File: srcPath, Pkg: "testmod",
		StartOffset: plusIdx, EndOffset: plusIdx + 1,
		Replacement: "-", Status: mutator.StatusPending,
	}
	w.Test(context.Background(), m)

	got := killedPid.Load()
	if got == 0 {
		t.Fatalf("syscallKillFunc never invoked")
	}
	// killPgroup negates pgid, so a nonzero result here means the fallback
	// `return pid` did run. Body elision (BRANCH_IF) leaves pgid=0,
	// killPgroup sends -0 == 0; mutating `!=` to `==` makes the body fire
	// on the success path with the same value, but the success path is
	// gated out by our Getpgid stub.
	if got == 0 {
		t.Errorf("kill targeted pgid=0 — BRANCH_IF on the fallback elides `return pid`")
	}
}

// TestKillPgroupSendsNegativePgid kills INVERT_NEGATIVES on `-pgid`. The
// negative pgid is what makes syscall.Kill target the entire process
// group; with `+pgid` only the leader gets the signal, leaving children
// alive — defeating the RSS-runaway containment.
func TestKillPgroupSendsNegativePgid(t *testing.T) {
	orig := syscallKillFunc
	defer func() { syscallKillFunc = orig }()
	var got int
	syscallKillFunc = func(pid int, sig syscall.Signal) error {
		got = pid
		return nil
	}
	killPgroup(123)
	if got != -123 {
		t.Errorf("syscallKillFunc called with pid=%d, want -123 — INVERT_NEGATIVES on -pgid flips the sign", got)
	}
}

// TestWorkerTestRSSKillsRunaway kills BRANCH_IF on the
// `if pgroupRSSBytes(pgid) > maxSubprocRSSBytes` body and STATEMENT_REMOVE
// on `killPgroup(pgid)`. Stubbing syscallKillFunc lets us assert the kill
// was actually issued without sending a real signal that would tear down
// the test process tree.
//
// The test binary is a stand-in that runs for 0.5s, so the monitor polls
// it several times over. A real one can exit before the first poll: a
// build's sample alone, which a failed build reads, decides nothing when
// the build succeeds, as it does here with the kill stubbed out.
func TestWorkerTestRSSKillsRunaway(t *testing.T) {
	origCap := maxSubprocRSSBytes
	origPoll := monitorPollInterval
	origRSS := groupRSSFunc
	origKill := syscallKillFunc
	defer func() {
		maxSubprocRSSBytes = origCap
		monitorPollInterval = origPoll
		groupRSSFunc = origRSS
		syscallKillFunc = origKill
	}()
	maxSubprocRSSBytes = 1024 // 1 KB — well below any real process
	monitorPollInterval = 50 * time.Millisecond

	groupRSSFunc = func(int) (int64, error) {
		// Way above the tiny cap so the kill path always fires.
		return 999999 * 1024, nil
	}
	var killCalls atomic.Int32
	syscallKillFunc = func(pid int, sig syscall.Signal) error {
		killCalls.Add(1)
		// Don't actually kill — let the cmd run to natural completion.
		return nil
	}

	w, m := fakeWorker(t, routeMap("f.go:1", coverage.TestRef{Pkg: "m/calc", Name: "TestCalc"}), 30*time.Second)
	fakeBuildsAndRuns(t, func(string) []string { return []string{"true"} },
		func(string) []string { return []string{"sleep", "0.5"} })
	result := w.Test(context.Background(), m)
	if result.Status != mutator.StatusTimedOut {
		t.Errorf("Status=%v, want TimedOut — BRANCH_IF on `pgroupRSSBytes > cap` body skips the kill", result.Status)
	}
	if killCalls.Load() == 0 {
		t.Errorf("syscallKillFunc never invoked — STATEMENT_REMOVE on killPgroup elides the kill call")
	}
}

// TestWorkerTestRSSExactlyAtCapDoesNotKill kills CONDITIONALS_BOUNDARY on
// `pgroupRSSBytes(pgid) > maxSubprocRSSBytes`. With the probe reporting
// exactly the cap, the original `>` evaluates to false and no kill fires;
// the boundary mutant `>=` would trigger the kill on the equality.
func TestWorkerTestRSSExactlyAtCapDoesNotKill(t *testing.T) {
	dir := setupTestProject(t)
	srcPath := filepath.Join(dir, "add.go")
	src, _ := os.ReadFile(srcPath)
	cache := map[string][]byte{srcPath: src}
	plusIdx := strings.IndexByte(string(src), '+')

	origCap := maxSubprocRSSBytes
	origPoll := monitorPollInterval
	origRSS := groupRSSFunc
	origKill := syscallKillFunc
	defer func() {
		maxSubprocRSSBytes = origCap
		monitorPollInterval = origPoll
		groupRSSFunc = origRSS
		syscallKillFunc = origKill
	}()
	maxSubprocRSSBytes = 1024
	monitorPollInterval = 50 * time.Millisecond

	// Exactly equal to the cap.
	groupRSSFunc = func(int) (int64, error) { return 1024, nil }
	var killCalls atomic.Int32
	syscallKillFunc = func(int, syscall.Signal) error {
		killCalls.Add(1)
		return nil
	}

	w, err := NewWorker(0, t.TempDir(), TimeoutPolicy{Global: 30 * time.Second}, cache, dir, nil)
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	m := mutator.Mutant{
		ID: 1, File: srcPath, Pkg: "testmod",
		StartOffset: plusIdx, EndOffset: plusIdx + 1,
		Replacement: "-", Status: mutator.StatusPending,
	}
	result := w.Test(context.Background(), m)
	// The mutated `+`→`-` makes TestAdd fail, so original status is Killed.
	// Boundary mutation `>=` would trigger the RSS kill at the equality
	// boundary, classifying the result as TimedOut instead.
	if result.Status == mutator.StatusTimedOut {
		t.Errorf("RSS == cap should not trigger kill (`>` boundary); CONDITIONALS_BOUNDARY mutation `>=` triggers here")
	}
	if killCalls.Load() != 0 {
		t.Errorf("syscallKillFunc called %d times — boundary `>=` lets equality fire", killCalls.Load())
	}
}

// TestWorkerTestMonitorGoroutineExits kills STATEMENT_REMOVE on
// `close(monitorDone)`. With RSS well below the cap the kill path never
// fires, so the goroutine relies on `<-monitorDone` to exit. Without the
// close, it keeps polling the probe after Worker.Test returns.
func TestWorkerTestMonitorGoroutineExits(t *testing.T) {
	dir := setupTestProject(t)
	srcPath := filepath.Join(dir, "add.go")
	src, _ := os.ReadFile(srcPath)
	cache := map[string][]byte{srcPath: src}
	plusIdx := strings.IndexByte(string(src), '+')

	origPoll := monitorPollInterval
	origRSS := groupRSSFunc
	defer func() {
		monitorPollInterval = origPoll
		groupRSSFunc = origRSS
	}()
	monitorPollInterval = 50 * time.Millisecond

	var probeCalls atomic.Int32
	groupRSSFunc = func(int) (int64, error) {
		probeCalls.Add(1)
		return 0, nil // far below cap; no kill
	}

	w, err := NewWorker(0, t.TempDir(), TimeoutPolicy{Global: 30 * time.Second}, cache, dir, nil)
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	m := mutator.Mutant{
		ID: 1, File: srcPath, Pkg: "testmod",
		StartOffset: plusIdx, EndOffset: plusIdx + 1,
		Replacement: "-", Status: mutator.StatusPending,
	}
	w.Test(context.Background(), m)

	atReturn := probeCalls.Load()
	time.Sleep(4 * monitorPollInterval)
	if growth := probeCalls.Load() - atReturn; growth > 1 {
		t.Errorf("probe polled %d more times after Test returned — STATEMENT_REMOVE on close(monitorDone) leaks the goroutine", growth)
	}
}

// recheckWorker returns a worker whose map routes a mutant in m/calc to
// m/app's TestApp, with m/app's tests linking m/calc, so a survivor is
// re-checked against m/calc and m/app in full, and that mutant. Every
// test binary it builds is built at once, and every run of one runs
// cmdFor(its package) instead (see fakeBuildsAndRuns); the packages run
// are recorded in the order they start.
func recheckWorker(t *testing.T, global time.Duration, cmdFor func(pkg string) []string) (*Worker, mutator.Mutant, *[]string) {
	t.Helper()
	const calc, app = "m/calc", "m/app"
	tm := routeMap("f.go:1", coverage.TestRef{Pkg: app, Name: "TestApp"}).WithSuitesForTesting(true,
		map[string]map[string]bool{calc: {}, app: {calc: true}},
		coverage.Package{ImportPath: calc}, coverage.Package{ImportPath: app})
	w, m := fakeWorker(t, tm, global)
	runs := fakeBuildsAndRuns(t, func(string) []string { return []string{"true"} }, cmdFor)
	return w, m, &runs.runs
}

// fakeWorker returns a worker around tm whose policy is Global alone, and
// a mutant in m/calc at f.go:1.
func fakeWorker(t *testing.T, tm *coverage.TestMap, global time.Duration) (*Worker, mutator.Mutant) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "f.go")
	w, err := NewWorker(0, t.TempDir(), TimeoutPolicy{Global: global}, map[string][]byte{file: []byte("package calc\n")}, t.TempDir(), tm)
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	return w, mutator.Mutant{Pkg: "m/calc", File: file, CoverageFile: "f.go", Line: 1, Status: mutator.StatusPending}
}

// fakeRuns records what fakeBuildsAndRuns' stand-ins did: the package of
// each build and of each run, in the order they started.
type fakeRuns struct {
	builds, runs []string
}

// fakeBuildsAndRuns replaces every command a worker starts with a
// stand-in. A build of a test binary writes an empty one, unless
// buildFor(its package) is nil, which stands for a package without tests,
// then runs buildFor(its package). A run of the binary runs runFor(its
// package). The binaries' arguments read as none.
func fakeBuildsAndRuns(t *testing.T, buildFor, runFor func(pkg string) []string) *fakeRuns {
	t.Helper()
	origArgs := testBinaryArgsFunc
	t.Cleanup(func() { testBinaryArgsFunc = origArgs })
	testBinaryArgsFunc = func(context.Context, string, string, string, []string) ([]string, error) { return nil, nil }
	got := &fakeRuns{}
	binPkg := map[string]string{}
	origStart := startCommandFunc
	t.Cleanup(func() { startCommandFunc = origStart })
	startCommandFunc = func(cmd *exec.Cmd) error {
		var argv []string
		if bin, ok := builtBin(cmd.Args); ok {
			pkg := lastArg(cmd.Args)
			got.builds = append(got.builds, pkg)
			argv = buildFor(pkg)
			if argv == nil {
				argv = []string{"true"}
			} else {
				binPkg[bin] = pkg
				if err := os.WriteFile(bin, nil, 0o755); err != nil {
					t.Fatal(err)
				}
			}
		} else {
			pkg := binPkg[cmd.Path]
			got.runs = append(got.runs, pkg)
			argv = runFor(pkg)
		}
		path, err := exec.LookPath(argv[0])
		if err != nil {
			t.Fatalf("LookPath(%s): %v", argv[0], err)
		}
		cmd.Path, cmd.Args, cmd.Err = path, argv, nil
		return cmd.Start()
	}
	return got
}

// TestWorkerTestRecheckReusesBuilds: the re-check runs the binaries the
// routed run built, so each package is built once per mutant, and the
// binaries are gone once the mutant is done.
func TestWorkerTestRecheckReusesBuilds(t *testing.T) {
	const calc, app = "m/calc", "m/app"
	tm := routeMap("f.go:1", coverage.TestRef{Pkg: app, Name: "TestApp"}).WithSuitesForTesting(true,
		map[string]map[string]bool{calc: {}, app: {calc: true}},
		coverage.Package{ImportPath: calc}, coverage.Package{ImportPath: app})
	w, m := fakeWorker(t, tm, 30*time.Second)
	got := fakeBuildsAndRuns(t, func(string) []string { return []string{"true"} }, func(string) []string { return []string{"true"} })
	if r := w.Test(context.Background(), m); r.Status != mutator.StatusLived || !r.Rechecked {
		t.Fatalf("Status=%v Rechecked=%v, want a re-checked LIVED", r.Status, r.Rechecked)
	}
	if want := []string{app, calc}; !slices.Equal(got.builds, want) {
		t.Errorf("builds = %v, want %v: m/app's routed build reused by the re-check", got.builds, want)
	}
	if want := []string{app, calc, app}; !slices.Equal(got.runs, want) {
		t.Errorf("runs = %v, want %v", got.runs, want)
	}
	if left, _ := filepath.Glob(filepath.Join(w.binDir, "*.test")); len(left) != 0 {
		t.Errorf("binaries left behind: %v", left)
	}
}

// TestWorkerTestPackageWithoutTests: a package of which `go test -c`
// builds no binary has no tests, and passes, as its `go test` reported
// "no test files"; the other packages still run.
func TestWorkerTestPackageWithoutTests(t *testing.T) {
	const calc, app = "m/calc", "m/app"
	tm := routeMap("f.go:1", coverage.TestRef{Pkg: app, Name: "TestApp"}).WithSuitesForTesting(true,
		map[string]map[string]bool{calc: {}, app: {calc: true}},
		coverage.Package{ImportPath: calc}, coverage.Package{ImportPath: app})
	w, m := fakeWorker(t, tm, 30*time.Second)
	got := fakeBuildsAndRuns(t, func(pkg string) []string {
		if pkg == calc {
			return nil
		}
		return []string{"true"}
	}, func(string) []string { return []string{"true"} })
	if r := w.Test(context.Background(), m); r.Status != mutator.StatusLived {
		t.Fatalf("Status=%v, want LIVED", r.Status)
	}
	if want := []string{app, app}; !slices.Equal(got.runs, want) {
		t.Errorf("runs = %v, want %v: nothing to run for m/calc", got.runs, want)
	}
}

// TestWorkerTestRunDeadline: the routed run's deadline is shared by the
// runs of its packages, and is not spent on their builds, or on reading
// the binaries' arguments, which can wait on another worker's read. Each
// run takes 0.6s of a 1s deadline: shared, the second is cut off. Each
// build takes 1.2s: counted, even one would be. A 0.6s read and two 0.3s
// runs: counted, the second run would get 0.1s.
func TestWorkerTestRunDeadline(t *testing.T) {
	const calc, app = "m/calc", "m/app"
	both := []coverage.TestRef{{Pkg: calc, Name: "TestCalc"}, {Pkg: app, Name: "TestApp"}}
	cases := []struct {
		name  string
		refs  []coverage.TestRef
		build []string
		args  time.Duration
		run   []string
		want  mutator.MutantStatus
	}{
		{"builds don't count", []coverage.TestRef{{Pkg: calc, Name: "TestCalc"}}, []string{"sleep", "1.2"}, 0, []string{"true"}, mutator.StatusLived},
		{"reading args doesn't count", both, []string{"true"}, 600 * time.Millisecond, []string{"sleep", "0.3"}, mutator.StatusLived},
		{"runs share it", both, []string{"true"}, 0, []string{"sleep", "0.6"}, mutator.StatusTimedOut},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Suites in scope that are only the routed packages: no re-check
			// beyond them.
			tm := routeMap("f.go:1", tc.refs...).WithSuitesForTesting(false, nil,
				coverage.Package{ImportPath: calc}, coverage.Package{ImportPath: app})
			w, m := fakeWorker(t, tm, time.Second)
			fakeBuildsAndRuns(t, func(string) []string { return tc.build }, func(string) []string { return tc.run })
			testBinaryArgsFunc = func(context.Context, string, string, string, []string) ([]string, error) {
				time.Sleep(tc.args)
				return nil, nil
			}
			// The re-check of a survivor gets a deadline of its own; only
			// the routed run is under test.
			if got := w.runGroups(context.Background(), m, w.routeGroups(m), false, w.computeTimeout(m)); got != tc.want {
				t.Errorf("runGroups = %v, want %v", got, tc.want)
			}
		})
	}
}

// failRunStarts makes every start of a test binary fail with err, on top
// of fakeBuildsAndRuns' stand-ins, which still build.
func failRunStarts(err error) {
	fake := startCommandFunc
	startCommandFunc = func(cmd *exec.Cmd) error {
		if _, ok := builtBin(cmd.Args); ok {
			return fake(cmd)
		}
		return err
	}
}

// TestWorkerTestRunFailures: a run that can't start ends the mutant
// without a verdict on its tests: a package with no known directory, or
// binary arguments that can't be read, is an infrastructure error, and a
// failed start is classified as any other (see startFailure), unless the
// deadline had already run out by then, which is a timeout.
func TestWorkerTestRunFailures(t *testing.T) {
	refs := []coverage.TestRef{{Pkg: "m/calc", Name: "TestCalc"}}
	cases := []struct {
		name    string
		tm      *coverage.TestMap
		args    error
		start   error
		timeout time.Duration
		want    mutator.MutantStatus
		log     string // what the worker reports on stderr
	}{
		{"no directory", routeMap("f.go:1", coverage.TestRef{Pkg: "m/other", Name: "TestOther"}), nil, nil, time.Second, mutator.StatusInfraError,
			"worker 0: no directory known for package m/other, treating as INFRA ERROR"},
		{"arguments unreadable", routeMap("f.go:1", refs...), errors.New("go test -n: boom"), nil, time.Second, mutator.StatusInfraError,
			"worker 0: go test -n: boom, treating as INFRA ERROR"},
		{"start fails", routeMap("f.go:1", refs...), nil, errors.New("injected start failure"), time.Second, mutator.StatusNotViable,
			"cmd.Start failed"},
		{"deadline spent by the start", routeMap("f.go:1", refs...), nil, nil, 0, mutator.StatusTimedOut, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, m := fakeWorker(t, tc.tm, time.Second)
			fakeBuildsAndRuns(t, func(string) []string { return []string{"true"} }, func(string) []string { return []string{"true"} })
			if tc.args != nil {
				testBinaryArgsFunc = func(context.Context, string, string, string, []string) ([]string, error) { return nil, tc.args }
			}
			if tc.start != nil {
				failRunStarts(tc.start)
			}
			var got mutator.MutantStatus
			captured := captureStderr(t, func() {
				got = w.runGroups(context.Background(), m, w.routeGroups(m), false, tc.timeout)
			})
			if got != tc.want {
				t.Errorf("runGroups = %v, want %v", got, tc.want)
			}
			if !strings.Contains(captured, tc.log) {
				t.Errorf("stderr = %q, want it to report %q", captured, tc.log)
			}
		})
	}
}

// TestWorkerTestDeadlineKillsProcessGroup: a run cut off at its deadline
// takes down every process its tests started, so one that holds the
// output open can't keep the mutant waiting. With the binary alone
// killed, the wait would last until the started process exits (3s), as
// the drain delay is longer.
func TestWorkerTestDeadlineKillsProcessGroup(t *testing.T) {
	origDrain := proctree.DrainDelay
	t.Cleanup(func() { proctree.DrainDelay = origDrain })
	proctree.DrainDelay = 10 * time.Second
	runPastDeadlineWithHeldOutput(t, "the group killed at the 300ms deadline")
}

// runPastDeadlineWithHeldOutput runs a mutant whose tests start a process
// that holds the output open for 3s, against a 300ms deadline, and wants
// TIMED OUT well before that process exits.
func runPastDeadlineWithHeldOutput(t *testing.T, want string) {
	t.Helper()
	w, m := fakeWorker(t, routeMap("f.go:1", coverage.TestRef{Pkg: "m/calc", Name: "TestCalc"}), time.Second)
	fakeBuildsAndRuns(t, func(string) []string { return []string{"true"} },
		func(string) []string { return []string{"sh", "-c", "sleep 3 & sleep 3"} })
	start := time.Now()
	if got := w.runGroups(context.Background(), m, w.routeGroups(m), false, 300*time.Millisecond); got != mutator.StatusTimedOut {
		t.Errorf("runGroups = %v, want TIMED OUT", got)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("runGroups took %v, want %s", took, want)
	}
}

// TestWorkerTestBuildDeadline: a build that hangs without growing — on a
// module proxy, a stuck -toolexec — is cut off at buildTimeout with its
// whole process group, so it can't hold its worker forever, and reads as
// INFRA ERROR: a host problem, re-run next time rather than cached. With
// the build unbounded, or `go` alone killed, it would last 3s.
func TestWorkerTestBuildDeadline(t *testing.T) {
	origBuild, origDrain := buildTimeout, proctree.DrainDelay
	t.Cleanup(func() { buildTimeout, proctree.DrainDelay = origBuild, origDrain })
	buildTimeout, proctree.DrainDelay = 300*time.Millisecond, 10*time.Second
	w, m := fakeWorker(t, routeMap("f.go:1", coverage.TestRef{Pkg: "m/calc", Name: "TestCalc"}), time.Minute)
	fakeBuildsAndRuns(t, func(string) []string { return []string{"sh", "-c", "sleep 3 & sleep 3"} },
		func(string) []string { return []string{"true"} })
	start := time.Now()
	if got := w.runGroups(context.Background(), m, w.routeGroups(m), false, time.Minute); got != mutator.StatusInfraError {
		t.Errorf("runGroups = %v, want INFRA ERROR", got)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("runGroups took %v, want the build's group killed at the 300ms build deadline", took)
	}
}

// TestWorkerTestDeadlineDrainsBounded: when the group can't be killed, a
// process the tests started that holds the output open keeps the mutant
// waiting no longer than proctree.DrainDelay past the deadline.
func TestWorkerTestDeadlineDrainsBounded(t *testing.T) {
	origDrain := proctree.DrainDelay
	t.Cleanup(func() { proctree.DrainDelay = origDrain })
	proctree.DrainDelay = 200 * time.Millisecond
	t.Cleanup(proctree.FailGroupKillForTesting())
	runPastDeadlineWithHeldOutput(t, "the wait cut off 200ms past the 300ms deadline")
}

// TestWorkerTestRunOutputIsNoBuild: a test binary's output is the tests'
// own, never the toolchain's, as the build runs on its own. A test that
// prints `go test` output as fixture data, `[build failed]` included, and
// then exits through log.Fatal, with no `--- FAIL: ` line, detected the
// mutation: a generic phrase in its output must not read as the host
// failing a build, which would drop the kill as INFRA ERROR.
func TestWorkerTestRunOutputIsNoBuild(t *testing.T) {
	w, m := fakeWorker(t, routeMap("f.go:1", coverage.TestRef{Pkg: "m/calc", Name: "TestCalc"}), time.Minute)
	fakeBuildsAndRuns(t, func(string) []string { return []string{"true"} },
		func(string) []string {
			return []string{"sh", "-c", `printf 'FAIL\tm [build failed]\nresource temporarily unavailable\n'; exit 1`}
		})
	if got := w.runGroups(context.Background(), m, w.routeGroups(m), false, time.Minute); got != mutator.StatusKilled {
		t.Errorf("runGroups = %v, want KILLED: the output is the test's, not a build's", got)
	}
}

// builtBin returns the binary a `go test -c -o <bin>` argv builds, and
// whether args is one.
func builtBin(args []string) (string, bool) {
	if len(args) < 5 || args[1] != "test" || args[2] != "-c" || args[3] != "-o" {
		return "", false
	}
	return args[4], true
}

// TestWorkerTestRecheckDeadlinePerPackage: each package of the re-check
// gets the global ceiling to itself. Global is sized from every package's
// tests run at once, so with one deadline shared by suites run in turn, a
// survivor many suites link would run out of time and read as TIMED_OUT,
// dropping out of the efficacy denominator. Here each run takes 0.6s of a
// 1s ceiling: shared, m/app's re-check would be killed 0.4s in.
func TestWorkerTestRecheckDeadlinePerPackage(t *testing.T) {
	w, m, started := recheckWorker(t, time.Second, func(string) []string { return []string{"sleep", "0.6"} })
	got := w.Test(context.Background(), m)
	if got.Status != mutator.StatusLived || !got.Rechecked {
		t.Errorf("Status=%v Rechecked=%v, want a re-checked LIVED", got.Status, got.Rechecked)
	}
	if want := []string{"m/app", "m/calc", "m/app"}; !slices.Equal(*started, want) {
		t.Errorf("runs = %v, want %v: routed m/app, then the re-check, own package first", *started, want)
	}
}

// TestWorkerTestRecheckStopsAtKill: the re-check stops at the first
// package whose suite kills the mutant.
func TestWorkerTestRecheckStopsAtKill(t *testing.T) {
	runs := 0
	w, m, started := recheckWorker(t, 30*time.Second, func(string) []string {
		// The routed run passes; the re-check's first package fails.
		runs++
		if runs == 1 {
			return []string{"true"}
		}
		return []string{"false"}
	})
	got := w.Test(context.Background(), m)
	if got.Status != mutator.StatusKilled || !got.Rechecked {
		t.Errorf("Status=%v Rechecked=%v, want KILLED by the re-check", got.Status, got.Rechecked)
	}
	if want := []string{"m/app", "m/calc"}; !slices.Equal(*started, want) {
		t.Errorf("runs = %v, want %v: nothing after m/calc's kill", *started, want)
	}
}

// slowLinkDelay is how long slowLinkGoflags makes each test binary's link
// take: well past the 1s deadline TestWorkerTestDeadlineExcludesBuild's
// mutants get, as a heavy package's link under --workers contention is.
const slowLinkDelay = 3 * time.Second

// onLinkGoflags returns a GOFLAGS value whose -toolexec runs the shell
// command onLink before every link. `go` also runs `link -V=full` to
// identify the tool, which doesn't run it. Compiles are untouched, so the
// build cache already holds every package but the module's own.
func onLinkGoflags(t *testing.T, onLink string) string {
	t.Helper()
	wrapper := filepath.Join(t.TempDir(), "onlink.sh")
	script := "#!/bin/sh\n" +
		"if [ \"$(basename \"$1\")\" = link ] && [ \"$2\" != -V=full ]; then " + onLink + "; fi\n" +
		"exec \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return "-toolexec=" + wrapper
}

// slowLinkGoflags returns a GOFLAGS value that makes every link sleep
// slowLinkDelay first (see onLinkGoflags).
func slowLinkGoflags(t *testing.T) string {
	t.Helper()
	return onLinkGoflags(t, "sleep "+strconv.Itoa(int(slowLinkDelay/time.Second)))
}

// addWorker writes a module testmod whose Add is covered by TestAdd, with
// body as TestAdd's body, and returns a worker on it, its policy, and the
// mutant that turns Add's + into -. TestAdd ran in 10ms when the map was
// built, so the mutant's deadline is the policy's 1s floor.
func addWorker(t *testing.T, body string, imports ...string) (*Worker, TimeoutPolicy, mutator.Mutant) {
	t.Helper()
	dir := t.TempDir()
	// The comment keeps the mutant's build, link included, out of the
	// build cache of an earlier run.
	src := fmt.Sprintf("package testpkg\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n\n// %d\n", time.Now().UnixNano())
	var extra string
	for _, imp := range imports {
		extra += "\t\"" + imp + "\"\n"
	}
	testSrc := "package testpkg\n\nimport (\n" + extra + "\t\"testing\"\n\t\"time\"\n)\n\nvar _ = time.Second\n\n" +
		"func TestAdd(t *testing.T) {\n\t" + body + "\n}\n"
	for name, content := range map[string]string{
		"go.mod": "module testmod\n\ngo 1.26\n", "add.go": src, "add_test.go": testSrc,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tm := coverage.NewTestMapForTesting(
		map[[2]string]time.Duration{{"testmod", "TestAdd"}: 10 * time.Millisecond},
		map[string][]coverage.TestRef{"testmod/add.go:4": {{Pkg: "testmod", Name: "TestAdd"}}},
	)
	policy := TimeoutPolicy{Global: time.Minute, Margin: 3, Min: time.Second, Adaptive: true}
	file := filepath.Join(dir, "add.go")
	w, err := NewWorker(0, t.TempDir(), policy, map[string][]byte{file: []byte(src)}, dir, tm)
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	plus := strings.Index(src, "+")
	m := mutator.Mutant{
		ID: 1, File: file, Pkg: "testmod", CoverageFile: "testmod/add.go", Line: 4,
		StartOffset: plus, EndOffset: plus + 1, Replacement: "-",
		Status: mutator.StatusPending,
	}
	return w, policy, m
}

// TestWorkerTestDeadlineExcludesBuild (#106): the adaptive deadline is
// sized from the covering tests' run time, so it must bound running them,
// not building their binary too. A mutant's package is recompiled and
// relinked first, and a heavy package's link under --workers contention
// outlasts the --timeout-min floor. A mutant its tests kill, or survive,
// at once must not be reported TIMED_OUT for that, which drops it from
// the efficacy denominator. A test that hangs still times out, at the
// floor rather than the minute-long ceiling.
func TestWorkerTestDeadlineExcludesBuild(t *testing.T) {
	t.Setenv("GOFLAGS", slowLinkGoflags(t))
	cases := []struct {
		name string
		test string
		want mutator.MutantStatus
	}{
		{"killed", "if Add(1, 2) != 3 {\n\t\tt.Fatal(\"add\")\n\t}", mutator.StatusKilled},
		{"lived", "_ = Add(1, 2)", mutator.StatusLived},
		{"hangs", "time.Sleep(time.Minute)", mutator.StatusTimedOut},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, policy, m := addWorker(t, tc.test)
			if got := w.computeTimeout(m); got != time.Second {
				t.Fatalf("computeTimeout = %v, want the 1s floor", got)
			}
			got := w.Test(context.Background(), m)
			if got.Status != tc.want {
				t.Errorf("Status=%v after %v, want %v: the %v link must not count against the 1s deadline",
					got.Status, got.Duration.Round(time.Millisecond), tc.want, slowLinkDelay)
			}
			// The hang is cut off at the floor, not the minute-long
			// ceiling, which a run given Global instead would wait out.
			if limit := policy.Global / 2; got.Duration > limit {
				t.Errorf("Test took %v, want under %v: the run's deadline must be the 1s floor", got.Duration.Round(time.Millisecond), limit)
			}
		})
	}
}

// TestWorkerTestKilledLinkIsInfraError (#106): a linker SIGKILLed from
// outside — the kernel OOM-killer under a cgroup limit below the RSS
// monitor's ceiling — fails the build, not the mutant. `go` survives it,
// exits 1 with nothing on stdout, and reports the tool's death on stderr
// ("testmod.test: …/link: signal: killed"). Read as KILLED, the false kill
// would be cached and never re-run.
func TestWorkerTestKilledLinkIsInfraError(t *testing.T) {
	t.Setenv("GOFLAGS", onLinkGoflags(t, "kill -KILL $$"))
	w, _, m := addWorker(t, "_ = Add(1, 2)")
	if got := w.Test(context.Background(), m); got.Status != mutator.StatusInfraError {
		t.Errorf("Status=%v, want %v: an outside kill of the linker is no verdict on the mutant", got.Status, mutator.StatusInfraError)
	}
}

// fakeGoFirstOnPath puts first on PATH a `go` that reports itself as
// "fake" to `go version` and hands every other command to the real `go`,
// so builds still work: a test that sees "fake" ran a `go` other than the
// toolchain's.
func fakeGoFirstOnPath(t *testing.T) {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = version ]; then echo fake; exit 0; fi\nexec '" + goBin + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestWorkerTestRunsToolchainGo: a test that runs `go` gets the toolchain
// that built it, as under `go test`, which puts GOROOT/bin first on PATH,
// not whichever `go` is first there. A different one fails a test the
// mutant can't reach: a false KILLED the baseline, run by `go test`, never
// shows.
func TestWorkerTestRunsToolchainGo(t *testing.T) {
	fakeGoFirstOnPath(t)
	goroot, err := coverage.GOROOT(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	body := "_ = Add(1, 2)\n\tout, err := exec.Command(\"go\", \"version\").Output()\n" +
		"\tif err != nil || strings.TrimSpace(string(out)) == \"fake\" {\n\t\tt.Fatalf(\"go version = (%q, %v)\", out, err)\n\t}"
	for _, tc := range []struct {
		goroot string
		want   mutator.MutantStatus
	}{
		// Without GOROOT the fake go is found, which shows it is first on
		// PATH, so the LIVED below is GOROOT's doing.
		{"", mutator.StatusKilled},
		{goroot, mutator.StatusLived},
	} {
		w, _, m := addWorker(t, body, "os/exec", "strings")
		w.goroot = tc.goroot
		if got := w.Test(context.Background(), m); got.Status != tc.want {
			t.Errorf("goroot=%q: Status=%v, want %v", tc.goroot, got.Status, tc.want)
		}
	}
}
