//go:build !windows

package runner

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gomutants/gomutants/internal/config"
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
//
// The probe is polled, not read once: a sleep sampled the instant Start
// returns has barely faulted in a page (VmRSS ~12 kB), and Linux's stat rss
// can still read 0 then, about 1 run in 500 under load. A lookup that
// can't find the group reads 0 for the whole deadline.
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
	got := pgroupRSSBytes(pgid)
	for deadline := time.Now().Add(2 * time.Second); got <= 0 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
		got = pgroupRSSBytes(pgid)
	}
	if got <= 0 {
		t.Errorf("pgroupRSSBytes(child pgid=%d) = %d for 2s, want > 0", pgid, got)
	}
}

// deadPgid is a pgid no process can have: it is above pid_max on Linux
// (at most 2^22) and on macOS (99999).
const deadPgid = 99999999

// cleanRSSProbe clears the probe's shared health now and after the test,
// so another test's failures can't count toward this one's outage.
func cleanRSSProbe(t *testing.T) {
	t.Helper()
	resetRSSProbe()
	t.Cleanup(resetRSSProbe)
}

// TestPoolRunResetsRSSProbe starts a run while an earlier run's outage is
// still recorded: three failures would be needed to report it, but this
// run must report its own outage from scratch, and one never reported in
// it must not read as already reported. Kills STATEMENT_REMOVE on the
// resetRSSProbe call in Pool.Run and on each of its Stores, and INTEGER_*
// on the 0.
func TestPoolRunResetsRSSProbe(t *testing.T) {
	cleanRSSProbe(t)
	rssProbe.fails.Store(2)
	rssProbe.down.Store(true)

	// No mutants: Run returns before starting a worker, after the reset.
	NewPool(1, ExecOpts{}, TimeoutPolicy{}, t.TempDir(), nil, t.TempDir(), nil).Run(context.Background(), nil, nil)

	if fails, down := rssProbe.fails.Load(), rssProbe.down.Load(); fails != 0 || down {
		t.Errorf("after Pool.Run: fails=%d down=%v, want 0 false — the earlier run's outage carried over", fails, down)
	}
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

// scanStub stands in for the process-table scan under cachedGroupRSS: it
// counts scans and returns a scripted table or error, after an optional
// delay.
type scanStub struct {
	scans atomic.Int32
	table rssTable
	err   error
	delay time.Duration
}

// stubScan installs a scanStub that returns table, starting from a clean
// probe and snapshot, and restores the real scan after the test.
func stubScan(t *testing.T, table rssTable) *scanStub {
	t.Helper()
	cleanRSSProbe(t)
	s := &scanStub{table: table}
	orig := rssScanFunc
	t.Cleanup(func() { rssScanFunc = orig })
	rssScanFunc = func() (rssTable, error) {
		s.scans.Add(1)
		time.Sleep(s.delay)
		if s.err != nil {
			return nil, s.err
		}
		return maps.Clone(s.table), nil
	}
	return s
}

// lookup asserts that cachedGroupRSS reads want for pgid.
func (s *scanStub) lookup(t *testing.T, pgid int, want int64) {
	t.Helper()
	if got, err := cachedGroupRSS(pgid); err != nil || got != want {
		t.Fatalf("cachedGroupRSS(%d) = (%d, %v), want (%d, nil)", pgid, got, err, want)
	}
}

// wantScans asserts how many scans have run.
func (s *scanStub) wantScans(t *testing.T, want int32) {
	t.Helper()
	if got := s.scans.Load(); got != want {
		t.Fatalf("%d scans, want %d", got, want)
	}
}

// ageSnapshot moves the snapshot back by fraction of a poll interval.
func ageSnapshot(fraction float64) {
	rssSnapshot.at = rssSnapshot.at.Add(-time.Duration(fraction * float64(monitorPollInterval)))
}

// TestCachedGroupRSSSharesAScan pins the snapshot's max age: one 0.4 of an
// interval old is reused, one 0.75 old is not. Kills CONDITIONALS_NEGATION,
// ARITHMETIC_BASE and INTEGER_* on `> monitorPollInterval/2`.
func TestCachedGroupRSSSharesAScan(t *testing.T) {
	s := stubScan(t, rssTable{7: 100})
	s.lookup(t, 7, 100)
	s.lookup(t, 7, 100)
	s.wantScans(t, 1)
	ageSnapshot(0.4)
	s.lookup(t, 7, 100)
	s.wantScans(t, 1)
	ageSnapshot(0.35) // 0.75 of an interval in all
	s.lookup(t, 7, 100)
	s.wantScans(t, 2)
}

// TestCachedGroupRSSListedAtZero: a group whose processes all read 0 (a
// zombie leader) is listed, not missing.
func TestCachedGroupRSSListedAtZero(t *testing.T) {
	s := stubScan(t, rssTable{10: 0})
	s.lookup(t, 10, 0)
}

// TestCachedGroupRSSRescansForANewGroup: a group missing from a scan that
// started before the call may have started since, so it gets a fresh scan.
// Kills the rescan dropped, or its `Before` negated.
func TestCachedGroupRSSRescansForANewGroup(t *testing.T) {
	s := stubScan(t, rssTable{7: 1})
	s.lookup(t, 7, 1)
	s.table = rssTable{7: 1, 8: 2} // group 8 started after the scan
	s.lookup(t, 8, 2)
	s.wantScans(t, 2)
}

// TestCachedGroupRSSUnlisted: a group missing from a scan made for this
// call is unlisted, at 0, with no second scan. Kills INTEGER_* on the 0.
func TestCachedGroupRSSUnlisted(t *testing.T) {
	s := stubScan(t, rssTable{7: 1})
	if n, err := cachedGroupRSS(9); !errors.Is(err, errGroupNotListed) || n != 0 {
		t.Fatalf("cachedGroupRSS(9) = (%d, %v), want (0, errGroupNotListed)", n, err)
	}
	s.wantScans(t, 1)
}

// TestCachedGroupRSSFailedScanNotKept: a scan's own error comes back from
// both rescans, and the next call scans again rather than reading a kept
// failure, which several workers would count as an outage. Kills either
// rescan's error check dropped, and rescanRSS keeping a failed scan.
func TestCachedGroupRSSFailedScanNotKept(t *testing.T) {
	s := stubScan(t, rssTable{7: 1})
	s.lookup(t, 7, 1)
	boom := errors.New("fork/exec /bin/ps: resource temporarily unavailable")
	s.err = boom
	// A fresh snapshot without group 8: the rescan for the miss fails.
	if n, err := cachedGroupRSS(8); !errors.Is(err, boom) || n != 0 {
		t.Fatalf("miss, failing rescan: (%d, %v), want (0, %v)", n, err, boom)
	}
	// A stale snapshot: the rescan for its age fails.
	ageSnapshot(0.75)
	if n, err := cachedGroupRSS(7); !errors.Is(err, boom) || n != 0 {
		t.Fatalf("stale snapshot, failing rescan: (%d, %v), want (0, %v)", n, err, boom)
	}
	s.err = nil
	s.lookup(t, 7, 1)
	s.wantScans(t, 4)
}

// TestCachedGroupRSSResetDropsSnapshot kills the snapshot clear in
// resetRSSProbe dropped.
func TestCachedGroupRSSResetDropsSnapshot(t *testing.T) {
	s := stubScan(t, rssTable{7: 1})
	s.lookup(t, 7, 1)
	resetRSSProbe()
	s.lookup(t, 7, 1)
	s.wantScans(t, 2)
}

// TestCachedGroupRSSConcurrentPollers: pollers that arrive mid-scan wait
// for it rather than scanning again. Kills the mutex dropped.
func TestCachedGroupRSSConcurrentPollers(t *testing.T) {
	s := stubScan(t, rssTable{7: 1})
	s.delay = 20 * time.Millisecond
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			if n, err := cachedGroupRSS(7); err != nil || n != 1 {
				t.Errorf("cachedGroupRSS(7) = (%d, %v), want (1, nil)", n, err)
			}
		})
	}
	wg.Wait()
	s.wantScans(t, 1)
}

// TestCachedGroupRSSRealScanUnlisted runs the platform's real scan for a
// group nobody has.
func TestCachedGroupRSSRealScanUnlisted(t *testing.T) {
	cleanRSSProbe(t)
	if _, err := cachedGroupRSS(deadPgid); !errors.Is(err, errGroupNotListed) {
		t.Errorf("cachedGroupRSS(deadPgid) err = %v, want errGroupNotListed", err)
	}
}

// wantAllZero asserts that every poll read 0.
func wantAllZero(t *testing.T, got []int64) {
	t.Helper()
	if slices.ContainsFunc(got, func(n int64) bool { return n != 0 }) {
		t.Errorf("got %v, want all 0", got)
	}
}

// wantWarnedOnce asserts that stderr holds exactly one "cap is off"
// warning, and that it names cause.
func wantWarnedOnce(t *testing.T, stderr, cause string) {
	t.Helper()
	if n := strings.Count(stderr, "RSS cap is off"); n != 1 {
		t.Errorf("warned %d times, want once; stderr:\n%s", n, stderr)
	}
	if !strings.Contains(stderr, cause) {
		t.Errorf("warning doesn't name the cause %q; stderr:\n%s", cause, stderr)
	}
}

// startGroup starts argv in its own process group, as makeCmd starts a
// mutant, and returns it with its pgid. The group is killed after the test.
func startGroup(t *testing.T, argv ...string) (*exec.Cmd, int) {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	applyProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", argv[0], err)
	}
	pgid := cmd.Process.Pid // with Setpgid the child leads its own group
	t.Cleanup(func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	return cmd, pgid
}

// TestPgroupRSSBytesInvalidPgid probes a pgid no process has. The real
// probe lists no row for it, and the group is gone rather than hidden, so
// the result must be exactly 0 with no warning, however often it's polled.
func TestPgroupRSSBytesInvalidPgid(t *testing.T) {
	cleanRSSProbe(t)
	out, got := probeThroughOutage(t, deadPgid)
	wantAllZero(t, got)
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
		cleanRSSProbe(t)
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
		cleanRSSProbe(t)
		// A partial total alongside the error, and a group that is gone:
		// neither may matter once the probe itself has failed.
		groupRSSFunc = func(int) (int64, error) {
			return 5 * 1024, errors.New("fork/exec /bin/ps: no such file or directory")
		}
		out, got := probeThroughOutage(t, deadPgid)
		wantAllZero(t, got)
		wantWarnedOnce(t, out, "no such file or directory")
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
	cleanRSSProbe(t)

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
		cleanRSSProbe(t)
		cmd, pgid := startGroup(t, "sleep", "60")
		out, got := probeThroughOutage(t, pgid)
		wantAllZero(t, got)
		wantWarnedOnce(t, out, errGroupNotListed.Error())

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
		cleanRSSProbe(t)
		cmd, pgid := startGroup(t, "sh", "-c", "sleep 60 &")
		if err := cmd.Wait(); err != nil {
			t.Fatalf("sh: %v", err)
		}
		out, _ := probeThroughOutage(t, pgid)
		wantWarnedOnce(t, out, errGroupNotListed.Error())
	})

	t.Run("exited group is silent", func(t *testing.T) {
		cleanRSSProbe(t)
		out, got := probeThroughOutage(t, deadPgid)
		wantAllZero(t, got)
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

// TestWorkerTestRSSKillsRunaway kills BRANCH_IF on the `if rss > rssCap`
// body and STATEMENT_REMOVE on `killPgroup(pgid)`. Stubbing syscallKillFunc
// lets us assert the kill was actually issued without sending a real
// signal that would tear down the test process tree.
//
// The test binary is a stand-in that runs for 0.5s, so the monitor polls
// it several times over. A real one can exit before the first poll: a
// build's sample alone, which a failed build reads, decides nothing when
// the build succeeds, as it does here with the kill stubbed out.
//
// Its package's suite, measured without the mutant once the mutant's run
// goes over the cap (see suiteCaps), stays well under it, so the cap
// stays and the mutant reads TIMED OUT.
func TestWorkerTestRSSKillsRunaway(t *testing.T) {
	// 2 MiB, well below the stubbed probe, and a whole number of MiB so the
	// log line's cap reads back as set.
	mon := stubMemCap(t, 2<<20)
	w, m := fakeWorker(t, routeMap("f.go:1", coverage.TestRef{Pkg: "m/calc", Name: "TestCalc"}), 30*time.Second)
	n := 0
	runs := fakeBuildsAndRuns(t, func(string) []string { return []string{"true"} },
		func(string) []string {
			// The mutant's run goes way over the cap, the suite's without
			// it doesn't.
			n++
			if n == 1 {
				mon.rss.Store(999999 * 1024)
			} else {
				mon.rss.Store(1024)
			}
			return []string{"sleep", "0.5"}
		})
	var result mutator.Mutant
	stderr := captureStderr(t, func() { result = w.Test(context.Background(), m) })
	if result.Status != mutator.StatusTimedOut {
		t.Errorf("Status=%v, want TimedOut — BRANCH_IF on `rss > rssCap` body skips the kill", result.Status)
	}
	if mon.kills.Load() == 0 {
		t.Errorf("syscallKillFunc never invoked — STATEMENT_REMOVE on killPgroup elides the kill call")
	}
	if want := []string{"m/calc", "m/calc"}; !slices.Equal(runs.runs, want) {
		t.Errorf("runs = %v, want %v: the mutant's, then its suite's without it", runs.runs, want)
	}
	want := "gomutants: worker 0: testing m/calc went over the 2 MiB memory cap and was killed, treating as TIMED OUT\n"
	if stderr != want {
		t.Errorf("stderr = %q, want %q: a memory kill must say so, or it reads as a deadline", stderr, want)
	}
}

// TestWorkerTestRSSKillsRunawayBuild is TestWorkerTestRSSKillsRunaway for
// a build: the monitor's kill of a compile or link reads TIMED OUT too, and
// says which it stopped, once the package's build without any mutant is
// seen to stay under the cap. That check is made once: a second mutant's
// build over the cap isn't followed by another. The stand-in build fails
// on its own after the monitor has polled it, as one the stubbed-out kill
// had stopped would. A build keeps the default cap whatever its tests use,
// so nothing is measured.
func TestWorkerTestRSSKillsRunawayBuild(t *testing.T) {
	mon := stubMemCap(t, 3<<20)
	w, m := fakeWorker(t, routeMap("f.go:1", coverage.TestRef{Pkg: "m/calc", Name: "TestCalc"}), 30*time.Second)
	_, runs := fakeBuildsOverCap(t, mon, []string{"sleep", "0.3"}, 1024)
	const want = "gomutants: worker 0: building m/calc went over the 3 MiB memory cap and was killed, treating as TIMED OUT\n"
	for range 2 {
		var result mutator.Mutant
		stderr := captureStderr(t, func() { result = w.Test(context.Background(), m) })
		if result.Status != mutator.StatusTimedOut {
			t.Errorf("Status=%v, want TimedOut", result.Status)
		}
		if stderr != want {
			t.Errorf("stderr = %q, want %q", stderr, want)
		}
	}
	if want := []string{"m/calc", "m/calc", "m/calc"}; len(runs.runs) != 0 || !slices.Equal(runs.builds, want) {
		t.Errorf("builds = %v, runs = %v, want %v and no run: each mutant's, and one without a mutant for the first", runs.builds, runs.runs, want)
	}
}

// TestWorkerTestBuildOverCapWithoutMutant: a build over the cap whose
// package's build without any mutant goes over it too, or can't tell, is
// INFRA ERROR, not TIMED OUT, which would drop every mutant of the package
// from the efficacy denominator unsaid. INFRA ERROR isn't cached, and the
// check is kept for the rest of the run: a later mutant's build over the
// cap isn't followed by another.
func TestWorkerTestBuildOverCapWithoutMutant(t *testing.T) {
	const overCap = "gomutants: worker 0: building m/calc went over the 3 MiB memory cap, and building it without a mutant didn't stay under it, treating as INFRA ERROR\n"
	cases := []struct {
		name string
		// unmutated is the build without a mutant, and rss what it uses.
		unmutated []string
		rss       int64
		startErr  bool // its start fails instead
		want      string
	}{
		{"over cap too", []string{"sh", "-c", "sleep 0.5; exit 1"}, 999999 * 1024, false,
			"gomutants: worker 0: building m/calc without a mutant goes over the 3 MiB memory cap too; raise --memory-cap to test its mutants\n"},
		{"fails", []string{"sh", "-c", "echo undefined: x >&2; exit 1"}, 1024, false,
			"gomutants: worker 0: can't tell whether m/calc's build fits under the memory cap without a mutant: building it: exit status 1\nundefined: x\n"},
		{"fails to start", []string{"true"}, 1024, true,
			"gomutants: worker 0: can't tell whether m/calc's build fits under the memory cap without a mutant: fork failed\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mon := stubMemCap(t, 3<<20)
			w, m := fakeWorker(t, nil, 30*time.Second)
			n, runs := fakeBuildsOverCap(t, mon, tc.unmutated, tc.rss)
			if tc.startErr {
				failSecondStart(func() { *n++ }) // the stand-in isn't asked for it
			}
			wantInfraErrors(t, w, m, tc.want+overCap, overCap)
			if *n != 3 || len(runs.runs) != 0 {
				t.Errorf("%d builds, runs = %v, want 3 and none: each mutant's, and one without a mutant for the first", *n, runs.runs)
			}
		})
	}
}

// fakeBuildsOverCap fakes every build as one over the cap that fails on
// its own after the monitor has polled it, but the second, the package's
// build without a mutant, which runs unmutated and uses rss. Each run
// passes. It returns how many builds were asked for, and what ran.
func fakeBuildsOverCap(t *testing.T, mon *memCapStub, unmutated []string, rss int64) (*int, *fakeRuns) {
	t.Helper()
	n := 0
	runs := fakeBuildsAndRuns(t, func(string) []string {
		n++
		if n == 2 {
			mon.rss.Store(rss)
			return unmutated
		}
		mon.rss.Store(999999 * 1024)
		return []string{"sh", "-c", "sleep 0.5; exit 1"}
	}, func(string) []string { return []string{"true"} })
	return &n, runs
}

// failSecondStart fails the second command started, the build without a
// mutant, with "fork failed", after calling failed. It wraps the stand-in
// already in place, whose cleanup puts the original back.
func failSecondStart(failed func()) {
	start := startCommandFunc
	calls := 0
	startCommandFunc = func(cmd *exec.Cmd) error {
		calls++
		if calls == 2 {
			failed()
			return errors.New("fork failed")
		}
		return start(cmd)
	}
}

// wantInfraErrors tests m with w once per entry of wants, and wants each
// test INFRA ERROR, saying that entry.
func wantInfraErrors(t *testing.T, w *Worker, m mutator.Mutant, wants ...string) {
	t.Helper()
	for _, want := range wants {
		var got mutator.Mutant
		stderr := captureStderr(t, func() { got = w.Test(context.Background(), m) })
		if got.Status != mutator.StatusInfraError {
			t.Errorf("Status=%v, want InfraError", got.Status)
		}
		if stderr != want {
			t.Errorf("stderr = %q, want %q", stderr, want)
		}
	}
}

// TestWorkerTestCancelledBuildCheckSaysNothing: a cancel of ctx during the
// build without any mutant says nothing of it, nor of the mutant's build
// over the cap: the run is ending, and the mutant is discarded.
func TestWorkerTestCancelledBuildCheckSaysNothing(t *testing.T) {
	mon := stubMemCap(t, 3<<20)
	mon.rss.Store(999999 * 1024)
	w, m := fakeWorker(t, nil, 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := 0
	fakeBuildsAndRuns(t, func(string) []string {
		n++
		if n == 2 {
			mon.rss.Store(1024)
			time.AfterFunc(100*time.Millisecond, cancel)
			return []string{"sleep", "5"}
		}
		return []string{"sh", "-c", "sleep 0.5; exit 1"}
	}, func(string) []string { return []string{"true"} })
	var got mutator.Mutant
	if stderr := captureStderr(t, func() { got = w.Test(ctx, m) }); stderr != "" {
		t.Errorf("stderr = %q, want nothing said", stderr)
	}
	if got.Status != mutator.StatusPending || n != 2 {
		t.Errorf("Status=%v after %d builds, want the mutant Pending after 2", got.Status, n)
	}
}

// TestWorkerTestRSSExactlyAtCapDoesNotKill kills CONDITIONALS_BOUNDARY on
// `rss > rssCap`. With the probe reporting exactly the cap, the original
// `>` evaluates to false and no kill fires; the boundary mutant `>=` would
// trigger the kill on the equality.
func TestWorkerTestRSSExactlyAtCapDoesNotKill(t *testing.T) {
	dir := setupTestProject(t)
	srcPath := filepath.Join(dir, "add.go")
	src, _ := os.ReadFile(srcPath)
	cache := map[string][]byte{srcPath: src}
	plusIdx := strings.IndexByte(string(src), '+')

	// The probe reads exactly the cap.
	mon := stubMemCap(t, 1024)

	w, err := NewWorker(0, t.TempDir(), TimeoutPolicy{Global: 30 * time.Second}, cache, dir, nil)
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	m := mutator.Mutant{
		ID: 1, File: srcPath, Pkg: "testmod",
		StartOffset: plusIdx, EndOffset: plusIdx + 1,
		Replacement: "-", Status: mutator.StatusPending,
	}
	var result mutator.Mutant
	stderr := captureStderr(t, func() { result = w.Test(context.Background(), m) })
	if strings.Contains(stderr, "memory cap") {
		t.Errorf("stderr = %q, want no memory-kill line for a run the monitor let finish", stderr)
	}
	// The mutated `+`→`-` makes TestAdd fail, so original status is Killed.
	// Boundary mutation `>=` would trigger the RSS kill at the equality
	// boundary, classifying the result as TimedOut instead.
	if result.Status == mutator.StatusTimedOut {
		t.Errorf("RSS == cap should not trigger kill (`>` boundary); CONDITIONALS_BOUNDARY mutation `>=` triggers here")
	}
	if mon.kills.Load() != 0 {
		t.Errorf("syscallKillFunc called %d times — boundary `>=` lets equality fire", mon.kills.Load())
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
// runs: counted, the second run would get 0.1s. A 0.1s run of m/calc, the
// mutant's own package and so the first, leaves m/app's 0.5s run 0.9s,
// where the first run's time alone would leave it 0.1s: 0.4s to spare
// either way, as the cases above are near ties for that.
func TestWorkerTestRunDeadline(t *testing.T) {
	const calc, app = "m/calc", "m/app"
	both := []coverage.TestRef{{Pkg: calc, Name: "TestCalc"}, {Pkg: app, Name: "TestApp"}}
	cases := []struct {
		name   string
		refs   []coverage.TestRef
		build  []string
		args   time.Duration
		run    []string
		appRun []string // m/app's run, if not run
		want   mutator.MutantStatus
	}{
		{"builds don't count", []coverage.TestRef{{Pkg: calc, Name: "TestCalc"}}, []string{"sleep", "1.2"}, 0, []string{"true"}, nil, mutator.StatusLived},
		{"reading args doesn't count", both, []string{"true"}, 600 * time.Millisecond, []string{"sleep", "0.3"}, nil, mutator.StatusLived},
		{"runs share it", both, []string{"true"}, 0, []string{"sleep", "0.6"}, nil, mutator.StatusTimedOut},
		{"a run leaves the rest", both, []string{"true"}, 0, []string{"sleep", "0.1"}, []string{"sleep", "0.5"}, mutator.StatusLived},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Suites in scope that are only the routed packages: no re-check
			// beyond them.
			tm := routeMap("f.go:1", tc.refs...).WithSuitesForTesting(false, nil,
				coverage.Package{ImportPath: calc}, coverage.Package{ImportPath: app})
			w, m := fakeWorker(t, tm, time.Second)
			fakeBuildsAndRuns(t, func(string) []string { return tc.build }, func(pkg string) []string {
				if pkg == app && tc.appRun != nil {
					return tc.appRun
				}
				return tc.run
			})
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

// TestWorkerTestHeavySuiteRaisesCap: a package whose suite goes over the
// memory cap without any mutant gets twice what it uses as its cap (#124).
// The cap sums every process a suite starts, so a suite whose tests run a
// linter or an analyzer goes over 2 GiB with any mutant at all; read as
// TIMED OUT, its mutants drop out of the efficacy denominator, and
// --threshold-efficacy passes on survivors. The suite is measured once,
// from a binary built without the overlay, and a mutant that runs away in
// it still reads TIMED OUT.
func TestWorkerTestHeavySuiteRaisesCap(t *testing.T) {
	mon := stubMemCap(t, 2<<20)
	// The suite uses 3 MiB whatever the mutant.
	mon.rss.Store(3 << 20)
	w, m := fakeWorker(t, nil, 30*time.Second)
	runs := fakeBuildsAndRuns(t, func(string) []string { return []string{"true"} },
		func(string) []string { return []string{"sleep", "0.3"} })
	// Every build and run is bounded like a mutant's (see proctree.Bound),
	// and every run gets the environment `go test` gives a test binary.
	var overlays []bool
	fake := startCommandFunc
	startCommandFunc = func(cmd *exec.Cmd) error {
		if cmd.WaitDelay != proctree.DrainDelay {
			t.Errorf("%v: WaitDelay = %v, want proctree.DrainDelay", cmd.Args, cmd.WaitDelay)
		}
		if _, ok := builtBin(cmd.Args); ok {
			overlays = append(overlays, slices.ContainsFunc(cmd.Args, func(a string) bool { return strings.HasPrefix(a, "-overlay=") }))
		} else if cmd.Env == nil {
			t.Errorf("%v: Env is nil, want the test binary's", cmd.Args)
		}
		return fake(cmd)
	}

	var got mutator.Mutant
	stderr := captureStderr(t, func() { got = w.Test(context.Background(), m) })
	if got.Status != mutator.StatusLived {
		t.Errorf("Status=%v, want LIVED: the mutant's run used what its suite uses without it", got.Status)
	}
	if want := []string{"m/calc", "m/calc", "m/calc"}; !slices.Equal(runs.runs, want) {
		t.Errorf("runs = %v, want %v: the mutant's, the suite's without it, the mutant's under the raised cap", runs.runs, want)
	}
	if want := []bool{true, false}; !slices.Equal(overlays, want) {
		t.Errorf("builds with an overlay = %v, want %v: the suite is measured without the mutant", overlays, want)
	}
	if want := "gomutants: worker 0: m/calc's tests use 3 MiB without a mutant, so their memory cap is raised to 6 MiB\n"; stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}

	// The package's later mutants run under the raised cap from the start.
	stderr = captureStderr(t, func() { got = w.Test(context.Background(), m) })
	if got.Status != mutator.StatusLived || len(runs.runs) != 4 || stderr != "" {
		t.Errorf("Status=%v, runs = %v, stderr = %q, want LIVED from one more run, measuring nothing", got.Status, runs.runs, stderr)
	}

	// A mutant that runs away in it still goes over the raised cap.
	mon.rss.Store(999999 * 1024)
	stderr = captureStderr(t, func() { got = w.Test(context.Background(), m) })
	if got.Status != mutator.StatusTimedOut || len(runs.runs) != 5 {
		t.Errorf("Status=%v, runs = %v, want TIMED OUT from one more run", got.Status, runs.runs)
	}
	if want := "gomutants: worker 0: testing m/calc went over the 6 MiB memory cap and was killed, treating as TIMED OUT\n"; stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
}

// TestWorkerTestRecheckHeavySuitesStayLived is TestWorkerTestHeavySuiteRaisesCap
// for the re-check, where #124 showed: each package whose suite goes over
// the cap alone gets its own cap, so a survivor stays a re-checked LIVED.
func TestWorkerTestRecheckHeavySuitesStayLived(t *testing.T) {
	mon := stubMemCap(t, 2<<20)
	mon.rss.Store(3 << 20)
	w, m, started := recheckWorker(t, 30*time.Second, func(string) []string { return []string{"sleep", "0.3"} })
	var got mutator.Mutant
	stderr := captureStderr(t, func() { got = w.Test(context.Background(), m) })
	if got.Status != mutator.StatusLived || !got.Rechecked {
		t.Errorf("Status=%v Rechecked=%v, want a re-checked LIVED", got.Status, got.Rechecked)
	}
	// Each package's first run goes over the default cap, its suite is
	// measured, and the run is made again; m/app's re-check already has
	// its raised cap.
	if want := []string{"m/app", "m/app", "m/app", "m/calc", "m/calc", "m/calc", "m/app"}; !slices.Equal(*started, want) {
		t.Errorf("runs = %v, want %v", *started, want)
	}
	want := "gomutants: worker 0: m/app's tests use 3 MiB without a mutant, so their memory cap is raised to 6 MiB\n" +
		"gomutants: worker 0: m/calc's tests use 3 MiB without a mutant, so their memory cap is raised to 6 MiB\n"
	if stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
}

// TestWorkerTestSuiteOverCeilingKeepsCap: a suite that goes over
// suiteRSSCeilingFactor times the cap without any mutant is stopped there
// and keeps the cap, so its mutants that go over it read TIMED OUT. It is
// measured once all the same.
func TestWorkerTestSuiteOverCeilingKeepsCap(t *testing.T) {
	mon := stubMemCap(t, 2<<20)
	mon.rss.Store(999999 * 1024)
	w, m := fakeWorker(t, nil, 30*time.Second)
	runs := fakeBuildsAndRuns(t, func(string) []string { return []string{"true"} },
		func(string) []string { return []string{"sleep", "0.3"} })
	var got mutator.Mutant
	stderr := captureStderr(t, func() { got = w.Test(context.Background(), m) })
	if got.Status != mutator.StatusTimedOut || len(runs.runs) != 2 {
		t.Errorf("Status=%v, runs = %v, want TIMED OUT from the mutant's run and the suite's", got.Status, runs.runs)
	}
	want := "gomutants: worker 0: m/calc's tests go over 8 MiB without a mutant, so they keep the 2 MiB memory cap\n" +
		"gomutants: worker 0: testing m/calc went over the 2 MiB memory cap and was killed, treating as TIMED OUT\n"
	if stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
	captureStderr(t, func() { got = w.Test(context.Background(), m) })
	if got.Status != mutator.StatusTimedOut || len(runs.runs) != 3 {
		t.Errorf("Status=%v, runs = %v, want TIMED OUT from one more run, measuring nothing", got.Status, runs.runs)
	}
}

// TestWorkerTestMemCapIsTheWorkers: a worker's own cap (see
// ExecOpts.MemoryCap), not the default, caps its builds and test runs, and
// is the base its packages' raised caps and the measuring run's ceiling
// are made from.
func TestWorkerTestMemCapIsTheWorkers(t *testing.T) {
	t.Run("build", func(t *testing.T) {
		mon := stubMemCap(t, 2<<20)
		mon.rss.Store(999999 * 1024)
		w, m := fakeWorker(t, nil, 30*time.Second)
		w.memCap = 5 << 20
		fakeBuildsAndRuns(t, func(string) []string { return []string{"sh", "-c", "sleep 0.3; exit 1"} },
			func(string) []string { return []string{"true"} })
		stderr := captureStderr(t, func() { w.Test(context.Background(), m) })
		want := "gomutants: worker 0: building m/calc without a mutant goes over the 5 MiB memory cap too; raise --memory-cap to test its mutants\n" +
			"gomutants: worker 0: building m/calc went over the 5 MiB memory cap, and building it without a mutant didn't stay under it, treating as INFRA ERROR\n"
		if stderr != want {
			t.Errorf("stderr = %q, want %q", stderr, want)
		}
	})
	t.Run("raised", func(t *testing.T) {
		mon := stubMemCap(t, 2<<20)
		// Over the worker's 4 MiB, under twice it.
		mon.rss.Store(5 << 20)
		w, m := fakeWorker(t, nil, 30*time.Second)
		w.memCap = 4 << 20
		fakeBuildsAndRuns(t, func(string) []string { return []string{"true"} },
			func(string) []string { return []string{"sleep", "0.3"} })
		var got mutator.Mutant
		stderr := captureStderr(t, func() { got = w.Test(context.Background(), m) })
		if got.Status != mutator.StatusLived {
			t.Errorf("Status=%v, want LIVED under the raised cap", got.Status)
		}
		if want := "gomutants: worker 0: m/calc's tests use 5 MiB without a mutant, so their memory cap is raised to 10 MiB\n"; stderr != want {
			t.Errorf("stderr = %q, want %q", stderr, want)
		}
	})
	t.Run("over ceiling", func(t *testing.T) {
		mon := stubMemCap(t, 2<<20)
		mon.rss.Store(999999 * 1024)
		w, m := fakeWorker(t, nil, 30*time.Second)
		w.memCap = 3 << 20
		fakeBuildsAndRuns(t, func(string) []string { return []string{"true"} },
			func(string) []string { return []string{"sleep", "0.3"} })
		stderr := captureStderr(t, func() { w.Test(context.Background(), m) })
		want := "gomutants: worker 0: m/calc's tests go over 12 MiB without a mutant, so they keep the 3 MiB memory cap\n" +
			"gomutants: worker 0: testing m/calc went over the 3 MiB memory cap and was killed, treating as TIMED OUT\n"
		if stderr != want {
			t.Errorf("stderr = %q, want %q", stderr, want)
		}
	})
}

// TestWorkerTestNoMemCapNeverKills: a worker whose cap is off
// (config.NoMemoryCap, or 0, which runMonitored reads as none too) has
// nothing killed whatever its runs use, and nothing polls its memory: a
// probe that read the process table every second would cost each build and
// run for a peak nothing reads.
func TestWorkerTestNoMemCapNeverKills(t *testing.T) {
	for _, memCap := range []int64{0, config.NoMemoryCap} {
		t.Run(fmt.Sprint(memCap), func(t *testing.T) {
			mon := stubMemCap(t, 2<<20)
			mon.rss.Store(999999 * 1024)
			var polls atomic.Int32
			probe := groupRSSFunc
			groupRSSFunc = func(pgid int) (int64, error) {
				polls.Add(1)
				return probe(pgid)
			}
			w, m := fakeWorker(t, nil, 30*time.Second)
			w.memCap = memCap
			runs := fakeBuildsAndRuns(t, func(string) []string { return []string{"sleep", "0.3"} },
				func(string) []string { return []string{"sleep", "0.3"} })
			var got mutator.Mutant
			stderr := captureStderr(t, func() { got = w.Test(context.Background(), m) })
			if got.Status != mutator.StatusLived || mon.kills.Load() != 0 || len(runs.runs) != 1 || stderr != "" {
				t.Errorf("Status=%v, kills = %d, runs = %v, stderr = %q, want LIVED from one run, nothing killed or said", got.Status, mon.kills.Load(), runs.runs, stderr)
			}

			cmd, _, _ := w.makeCmd(context.Background(), "sleep", t.TempDir(), []string{"0.3"})
			res, err := w.runMonitored(cmd, memCap)
			if err != nil {
				t.Fatalf("runMonitored: %v", err)
			}
			if res.memKilled || res.peak != 0 || polls.Load() != 0 {
				t.Errorf("memKilled = %v, peak = %d, polls = %d, want no kill and nothing polled", res.memKilled, res.peak, polls.Load())
			}
		})
	}

	// The smallest cap there is still caps.
	stubMemCap(t, 2<<20).rss.Store(999999 * 1024)
	w, _ := fakeWorker(t, nil, 30*time.Second)
	cmd, _, _ := w.makeCmd(context.Background(), "sleep", t.TempDir(), []string{"0.3"})
	res, err := w.runMonitored(cmd, 1)
	if err != nil {
		t.Fatalf("runMonitored: %v", err)
	}
	if !res.memKilled {
		t.Error("a 1-byte cap killed nothing, want the run over it killed")
	}
}

// TestRunMonitoredNoCapReportsFailure: with no cap, nothing polls, and a
// run that fails still returns its exit error.
func TestRunMonitoredNoCapReportsFailure(t *testing.T) {
	w, _ := fakeWorker(t, nil, 30*time.Second)
	cmd, _, _ := w.makeCmd(context.Background(), "false", t.TempDir(), nil)
	if res, err := w.runMonitored(cmd, config.NoMemoryCap); err != nil || res.runErr == nil {
		t.Errorf("runMonitored(false) = %+v, %v, want its exit error", res, err)
	}
}

// TestWorkerTestSuiteMeasureFailureKept: a run over the cap whose
// package's suite can't be measured is INFRA ERROR, not TIMED OUT: there
// is no telling whether the mutant or its suite went over, and INFRA
// ERROR isn't cached, so the next run measures again. The failure is kept
// for the rest of this one: a later mutant of the package that goes over
// the cap isn't built again without the overlay.
func TestWorkerTestSuiteMeasureFailureKept(t *testing.T) {
	mon := stubMemCap(t, 2<<20)
	mon.rss.Store(999999 * 1024)
	w, m := fakeWorker(t, nil, 30*time.Second)
	n := 0
	runs := fakeBuildsAndRuns(t, func(string) []string {
		// The first mutant's build succeeds, the unmutated one after it
		// fails.
		n++
		if n == 2 {
			return []string{"false"}
		}
		return []string{"true"}
	}, func(string) []string { return []string{"sleep", "0.3"} })
	const overCap = "gomutants: worker 0: testing m/calc went over the 2 MiB memory cap, and its tests couldn't be measured without a mutant, treating as INFRA ERROR\n"
	wants := []string{
		"gomutants: worker 0: can't measure the memory m/calc's tests use without a mutant: building it: exit status 1\n" + overCap,
		overCap,
	}
	for _, want := range wants {
		var got mutator.Mutant
		stderr := captureStderr(t, func() { got = w.Test(context.Background(), m) })
		if got.Status != mutator.StatusInfraError {
			t.Errorf("Status=%v, want InfraError", got.Status)
		}
		if stderr != want {
			t.Errorf("stderr = %q, want %q", stderr, want)
		}
	}
	if want := []string{"m/calc", "m/calc", "m/calc"}; !slices.Equal(runs.builds, want) {
		t.Errorf("builds = %v, want %v: each mutant's, and one unmutated for the first", runs.builds, want)
	}
}

// TestWorkerTestCancelledRunSaysNothing: a run over the cap that ctx
// cancels says nothing of a memory kill, whether ctx cancels its
// package's measurement or, the package measured already, the run itself:
// the mutant is left untested, and its result thrown away.
func TestWorkerTestCancelledRunSaysNothing(t *testing.T) {
	cases := []struct {
		name string
		// mutants is how many are tested; ctx cancels 0.1s into the
		// cancel'th run.
		mutants, cancel int
	}{
		{"measuring", 1, 2},
		// The suite goes over the ceiling, so it keeps the cap and the
		// second mutant's run isn't made again.
		{"measured", 2, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mon := stubMemCap(t, 2<<20)
			mon.rss.Store(999999 * 1024)
			w, m := fakeWorker(t, nil, 30*time.Second)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			n := 0
			fakeBuildsAndRuns(t, func(string) []string { return []string{"true"} }, func(string) []string {
				n++
				if n == tc.cancel {
					time.AfterFunc(100*time.Millisecond, cancel)
				}
				return []string{"sleep", "0.3"}
			})
			for range tc.mutants - 1 {
				captureStderr(t, func() { w.Test(ctx, m) })
			}
			if stderr := captureStderr(t, func() { w.Test(ctx, m) }); stderr != "" {
				t.Errorf("stderr = %q, want nothing said", stderr)
			}
			if n != tc.cancel {
				t.Errorf("%d runs, want %d: ctx cancelled another run", n, tc.cancel)
			}
		})
	}
}

// TestMeasureSuiteCap: the cap a measured suite's runs get, and what is
// said of it. A run that fails or runs past its deadline is kept, but may
// have stopped short of what the suite uses. The cap is never raised past
// the measuring run's ceiling. A probe that reads nothing of a run past
// two of its polls is an outage, and measures nothing; a run over sooner
// can have ended before the first poll. Those two are polled every 0.5s,
// and run 2.5 and 1.5 polls, so a threshold of one or three polls reads
// them the other way, with a quarter second to spare.
func TestMeasureSuiteCap(t *testing.T) {
	const tests = "gomutants: worker 0: m/calc's tests "
	cases := []struct {
		name   string
		rss    int64
		run    []string
		global time.Duration
		poll   time.Duration // stubMemCap's if 0
		want   int64
		stderr string
	}{
		{name: "failed", rss: 3 << 20, run: []string{"sh", "-c", "sleep 0.3; exit 1"}, global: 30 * time.Second, want: 6 << 20,
			stderr: tests + "failed without a mutant, so the 3 MiB they were seen to use may be less than they need\n" +
				tests + "use 3 MiB without a mutant, so their memory cap is raised to 6 MiB\n"},
		{name: "past the deadline", rss: 3 << 20, run: []string{"sleep", "5"}, global: 300 * time.Millisecond, want: 6 << 20,
			stderr: tests + "ran past their deadline without a mutant, so the 3 MiB they were seen to use may be less than they need\n" +
				tests + "use 3 MiB without a mutant, so their memory cap is raised to 6 MiB\n"},
		{name: "limited to the ceiling", rss: 5 << 20, run: []string{"sleep", "0.3"}, global: 30 * time.Second, want: 8 << 20,
			stderr: tests + "use 5 MiB without a mutant, so their memory cap is raised to 8 MiB\n"},
		{name: "probe reads nothing", rss: 0, run: []string{"sleep", "1.25"}, global: 30 * time.Second, poll: 500 * time.Millisecond, want: 0,
			stderr: "gomutants: worker 0: can't measure the memory m/calc's tests use without a mutant: no reading of its memory came back\n"},
		{name: "over within two polls", rss: 0, run: []string{"sleep", "0.75"}, global: 30 * time.Second, poll: 500 * time.Millisecond, want: 2 << 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mon := stubMemCap(t, 2<<20)
			if tc.poll != 0 {
				monitorPollInterval = tc.poll
			}
			mon.rss.Store(tc.rss)
			w, _ := fakeWorker(t, nil, tc.global)
			fakeBuildsAndRuns(t, func(string) []string { return []string{"true"} }, func(string) []string { return tc.run })
			var got int64
			stderr := captureStderr(t, func() { got = w.measureSuite(context.Background(), "m/calc", t.TempDir(), nil) })
			if got != tc.want {
				t.Errorf("measureSuite = %d MiB, want %d MiB", got>>20, tc.want>>20)
			}
			if stderr != tc.stderr {
				t.Errorf("stderr = %q, want %q", stderr, tc.stderr)
			}
		})
	}
}

// TestMeasureSuiteFails: a suite whose binary can't be built or started,
// or whose run ctx cancels, measures nothing, and says why unless ctx was
// cancelled, as the run's result is thrown away then. The binary it built
// is gone either way.
func TestMeasureSuiteFails(t *testing.T) {
	const why = "gomutants: worker 0: can't measure the memory m/calc's tests use without a mutant: "
	startErr := errors.New("inject")
	cases := []struct {
		name string
		// start replaces the nth start (1 for the build, 2 for the run)
		// with a failure; 0 replaces none.
		start      int
		build, run []string
		cancel     time.Duration // cancels ctx this long into the run, if set
		want       string
	}{
		{name: "build fails to start", start: 1, build: []string{"true"}, run: []string{"true"}, want: why + "inject\n"},
		{name: "run fails to start", start: 2, build: []string{"true"}, run: []string{"true"}, want: why + "inject\n"},
		{name: "build fails", build: []string{"sh", "-c", "echo 'undefined: x' >&2; exit 1"}, run: []string{"true"},
			want: why + "building it: exit status 1\nundefined: x\n"},
		{name: "run cancelled", build: []string{"true"}, run: []string{"sleep", "5"}, cancel: 100 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubMemCap(t, 2<<20)
			w, _ := fakeWorker(t, nil, 30*time.Second)
			fakeBuildsAndRuns(t, func(string) []string { return tc.build }, func(string) []string { return tc.run })
			failStart(tc.start, startErr)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel > 0 {
				time.AfterFunc(tc.cancel, cancel)
			}
			var got int64
			stderr := captureStderr(t, func() { got = w.measureSuite(ctx, "m/calc", t.TempDir(), nil) })
			if got != 0 {
				t.Errorf("measureSuite = %d, want 0 for a failure", got)
			}
			if stderr != tc.want {
				t.Errorf("stderr = %q, want %q", stderr, tc.want)
			}
			if _, err := os.Stat(filepath.Join(w.binDir, "worker-0-suite.test")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the suite's binary is left behind: %v", err)
			}
		})
	}
}

// failStart makes the nth command a worker starts fail with err, on top of
// fakeBuildsAndRuns' stand-ins, whose cleanup restores startCommandFunc;
// 0 fails none.
func failStart(nth int, err error) {
	fake := startCommandFunc
	n := 0
	startCommandFunc = func(cmd *exec.Cmd) error {
		n++
		if n == nth {
			return err
		}
		return fake(cmd)
	}
}

// TestWorkerTestDeadlineMeasuresNothing: a run past its deadline reads
// TIMED OUT without the monitor having stopped it, so its package's suite
// isn't measured, and no memory kill is reported.
func TestWorkerTestDeadlineMeasuresNothing(t *testing.T) {
	stubMemCap(t, 2<<20)
	w, m := fakeWorker(t, nil, 200*time.Millisecond)
	runs := fakeBuildsAndRuns(t, func(string) []string { return []string{"true"} },
		func(string) []string { return []string{"sleep", "5"} })
	var got mutator.Mutant
	stderr := captureStderr(t, func() { got = w.Test(context.Background(), m) })
	if got.Status != mutator.StatusTimedOut {
		t.Errorf("Status=%v, want TimedOut", got.Status)
	}
	if len(runs.runs) != 1 || stderr != "" {
		t.Errorf("runs = %v, stderr = %q, want the mutant's run alone, and nothing said", runs.runs, stderr)
	}
}

// TestWorkerTestMemKillAfterFailureIsKilled: a run the monitor stops after
// a test has reported a failure is KILLED, not TIMED OUT. A test can fail
// and its binary exit while a process it started still holds the output
// open and grows; the mutant was detected before the kill. Nothing is
// measured for it.
func TestWorkerTestMemKillAfterFailureIsKilled(t *testing.T) {
	mon := stubMemCap(t, 2<<20)
	mon.rss.Store(999999 * 1024)
	w, m := fakeWorker(t, nil, 30*time.Second)
	runs := fakeBuildsAndRuns(t, func(string) []string { return []string{"true"} },
		func(string) []string { return []string{"sh", "-c", "echo '--- FAIL: TestCalc'; sleep 0.5; exit 1"} })
	var got mutator.Mutant
	stderr := captureStderr(t, func() { got = w.Test(context.Background(), m) })
	if got.Status != mutator.StatusKilled {
		t.Errorf("Status=%v, want KILLED", got.Status)
	}
	if mon.kills.Load() == 0 {
		t.Errorf("the monitor never killed the run, so this tests nothing")
	}
	if len(runs.runs) != 1 || stderr != "" {
		t.Errorf("runs = %v, stderr = %q, want the mutant's run alone, and nothing said", runs.runs, stderr)
	}
}

// memCapStub is what stubMemCap stubs the RSS monitor with: the probe
// reads rss for every group, and kills counts the monitor's kills, which
// do nothing.
type memCapStub struct {
	rss   atomic.Int64
	kills atomic.Int32
}

// stubMemCap sets the memory cap to rssCap, polled every 50ms, and stubs
// the monitor's probe and kill (see memCapStub). The probe reads 1 KiB
// until the test sets rss.
func stubMemCap(t *testing.T, rssCap int64) *memCapStub {
	t.Helper()
	origCap := maxSubprocRSSBytes
	origPoll := monitorPollInterval
	origRSS := groupRSSFunc
	origKill := syscallKillFunc
	t.Cleanup(func() {
		maxSubprocRSSBytes = origCap
		monitorPollInterval = origPoll
		groupRSSFunc = origRSS
		syscallKillFunc = origKill
	})
	maxSubprocRSSBytes = rssCap
	monitorPollInterval = 50 * time.Millisecond
	mon := &memCapStub{}
	mon.rss.Store(1024)
	groupRSSFunc = func(int) (int64, error) { return mon.rss.Load(), nil }
	syscallKillFunc = func(int, syscall.Signal) error {
		mon.kills.Add(1)
		return nil
	}
	return mon
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
