//go:build !windows

package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/szhekpisov/gomutants/internal/coverage"
	"github.com/szhekpisov/gomutants/internal/mutator"
)

// TestPgroupRSSBytesSelf exercises pgroupRSSBytes against our own process
// group. Kills:
//   - CONDITIONALS_NEGATION on `err != nil` (line 32): mutated `err == nil`
//     would short-circuit to return 0 on the success path.
//   - STATEMENT_REMOVE on `line = strings.TrimSpace(line)` (line 37):
//     without trimming, `ps` output lines (" 12345") fail strconv.ParseInt.
//   - CONDITIONALS_NEGATION on `line == ""` (line 38): mutated `!=` skips
//     every non-empty line, never reaching ParseInt.
//   - CONDITIONALS_NEGATION on `err != nil` (line 42): mutated `==` skips
//     successful parses, total stays 0.
//   - ARITHMETIC_BASE on `n * 1024` (line 45): mutated `n / 1024` produces
//     byte totals roughly 1e6× too small.
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

// TestPgroupRSSBytesInvalidPgid kills the err-path return: passing an
// invalid PGID makes `ps -g` emit either an error or empty output. The
// original returns 0; a mutation that keeps the loop running still
// returns 0 on empty output (no lines to parse), so this specifically
// verifies the call doesn't panic or return garbage.
func TestPgroupRSSBytesInvalidPgid(t *testing.T) {
	// Very large pgid unlikely to exist.
	bytes := pgroupRSSBytes(99999999)
	if bytes < 0 {
		t.Errorf("pgroupRSSBytes(invalid) = %d, want >= 0", bytes)
	}
}

// TestPgroupRSSBytesParsing covers the parsing path with a stubbed
// psOutputFunc. Real ps output may or may not have leading whitespace
// depending on column width; injecting a known string lets us pin the
// behavior. Kills:
//   - BRANCH_IF on the err-return (psOutputFunc returns err with non-empty
//     output; original returns 0, the elided body parses the output).
//   - STATEMENT_REMOVE on the inner TrimSpace (now removed since the trim
//     is inlined into ParseInt's argument; mutating the inlined call breaks
//     parses on whitespace-prefixed lines).
//   - REMOVE_SELF_ASSIGNMENTS on `total += n * 1024` (sum vs last-line).
func TestPgroupRSSBytesParsing(t *testing.T) {
	orig := psOutputFunc
	defer func() { psOutputFunc = orig }()

	t.Run("err returns 0 even with output", func(t *testing.T) {
		psOutputFunc = func(int) ([]byte, error) {
			return []byte("12345\n"), errors.New("inject")
		}
		if got := pgroupRSSBytes(0); got != 0 {
			t.Errorf("got %d, want 0 — BRANCH_IF on err-return body lets the parse through", got)
		}
	})

	t.Run("sums all lines", func(t *testing.T) {
		psOutputFunc = func(int) ([]byte, error) {
			return []byte("  100\n  200\n  50\n"), nil
		}
		got := pgroupRSSBytes(0)
		want := int64((100 + 200 + 50) * 1024)
		if got != want {
			t.Errorf("got %d, want %d — REMOVE_SELF_ASSIGNMENTS on `total += n*1024` (or untrimmed-line parse failure) collapses the sum", got, want)
		}
	})

	t.Run("trims leading whitespace", func(t *testing.T) {
		psOutputFunc = func(int) ([]byte, error) {
			return []byte("    7\n"), nil
		}
		got := pgroupRSSBytes(0)
		want := int64(7 * 1024)
		if got != want {
			t.Errorf("got %d, want %d — without TrimSpace, ParseInt rejects whitespace-prefixed lines", got, want)
		}
	})

	t.Run("skips malformed lines", func(t *testing.T) {
		psOutputFunc = func(int) ([]byte, error) {
			return []byte("100\nnotanumber\n200\n"), nil
		}
		got := pgroupRSSBytes(0)
		want := int64((100 + 200) * 1024)
		if got != want {
			t.Errorf("got %d, want %d — malformed line should be skipped, sum should still total valid lines", got, want)
		}
	})
}

// TestMakeTestCmdSetpgid kills STATEMENT_REMOVE on `cmd.SysProcAttr =
// &syscall.SysProcAttr{Setpgid: true}` (now wrapped in applyProcessGroup).
// Without it, the child runs in the parent's process group; the RSS
// monitor would mistakenly include the parent and SIGKILL the entire
// test process.
func TestMakeTestCmdSetpgid(t *testing.T) {
	w := &Worker{projectDir: ".", policy: TimeoutPolicy{Global: time.Second}}
	cmd, _, _ := w.makeTestCmd(context.Background(), []string{"version"})
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
	origPS := psOutputFunc
	origKill := syscallKillFunc
	origGetpgid := syscallGetpgidFunc
	defer func() {
		maxSubprocRSSBytes = origCap
		monitorPollInterval = origPoll
		psOutputFunc = origPS
		syscallKillFunc = origKill
		syscallGetpgidFunc = origGetpgid
	}()
	maxSubprocRSSBytes = 1024
	monitorPollInterval = 50 * time.Millisecond
	psOutputFunc = func(int) ([]byte, error) { return []byte("999999\n"), nil }

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
func TestWorkerTestRSSKillsRunaway(t *testing.T) {
	dir := setupTestProject(t)
	srcPath := filepath.Join(dir, "add.go")
	src, _ := os.ReadFile(srcPath)
	cache := map[string][]byte{srcPath: src}
	plusIdx := strings.IndexByte(string(src), '+')

	origCap := maxSubprocRSSBytes
	origPoll := monitorPollInterval
	origPS := psOutputFunc
	origKill := syscallKillFunc
	defer func() {
		maxSubprocRSSBytes = origCap
		monitorPollInterval = origPoll
		psOutputFunc = origPS
		syscallKillFunc = origKill
	}()
	maxSubprocRSSBytes = 1024 // 1 KB — well below any real process
	monitorPollInterval = 50 * time.Millisecond

	psOutputFunc = func(int) ([]byte, error) {
		// Way above the tiny cap so the kill path always fires.
		return []byte("999999\n"), nil
	}
	var killCalls atomic.Int32
	syscallKillFunc = func(pid int, sig syscall.Signal) error {
		killCalls.Add(1)
		// Don't actually kill — let the cmd run to natural completion.
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
	if result.Status != mutator.StatusTimedOut {
		t.Errorf("Status=%v, want TimedOut — BRANCH_IF on `pgroupRSSBytes > cap` body skips the kill", result.Status)
	}
	if killCalls.Load() == 0 {
		t.Errorf("syscallKillFunc never invoked — STATEMENT_REMOVE on killPgroup elides the kill call")
	}
}

// TestWorkerTestRSSExactlyAtCapDoesNotKill kills CONDITIONALS_BOUNDARY on
// `pgroupRSSBytes(pgid) > maxSubprocRSSBytes`. With ps reporting exactly
// the cap, the original `>` evaluates to false and no kill fires; the
// boundary mutant `>=` would trigger the kill on the equality.
func TestWorkerTestRSSExactlyAtCapDoesNotKill(t *testing.T) {
	dir := setupTestProject(t)
	srcPath := filepath.Join(dir, "add.go")
	src, _ := os.ReadFile(srcPath)
	cache := map[string][]byte{srcPath: src}
	plusIdx := strings.IndexByte(string(src), '+')

	origCap := maxSubprocRSSBytes
	origPoll := monitorPollInterval
	origPS := psOutputFunc
	origKill := syscallKillFunc
	defer func() {
		maxSubprocRSSBytes = origCap
		monitorPollInterval = origPoll
		psOutputFunc = origPS
		syscallKillFunc = origKill
	}()
	maxSubprocRSSBytes = 1024
	monitorPollInterval = 50 * time.Millisecond

	// ps RSS column is in KB, multiplied by 1024 inside pgroupRSSBytes.
	// "1\n" ⇒ total = 1024 bytes, exactly equal to cap.
	psOutputFunc = func(int) ([]byte, error) { return []byte("1\n"), nil }
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
// close, it keeps polling ps after Worker.Test returns.
func TestWorkerTestMonitorGoroutineExits(t *testing.T) {
	dir := setupTestProject(t)
	srcPath := filepath.Join(dir, "add.go")
	src, _ := os.ReadFile(srcPath)
	cache := map[string][]byte{srcPath: src}
	plusIdx := strings.IndexByte(string(src), '+')

	origPoll := monitorPollInterval
	origPS := psOutputFunc
	defer func() {
		monitorPollInterval = origPoll
		psOutputFunc = origPS
	}()
	monitorPollInterval = 50 * time.Millisecond

	var psCalls atomic.Int32
	psOutputFunc = func(int) ([]byte, error) {
		psCalls.Add(1)
		return []byte("0\n"), nil // far below cap; no kill
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

	atReturn := psCalls.Load()
	time.Sleep(4 * monitorPollInterval)
	if growth := psCalls.Load() - atReturn; growth > 1 {
		t.Errorf("ps polled %d more times after Test returned — STATEMENT_REMOVE on close(monitorDone) leaks the goroutine", growth)
	}
}

// recheckWorker returns a worker whose map routes a mutant in m/calc to
// m/app's TestApp, with m/app's tests linking m/calc, so a survivor is
// re-checked against m/calc and m/app in full, and that mutant. Every
// `go test` it starts runs cmdFor(its package) instead, and the packages
// are recorded in the order they start.
func recheckWorker(t *testing.T, global time.Duration, cmdFor func(pkg string) []string) (*Worker, mutator.Mutant, *[]string) {
	t.Helper()
	const calc, app = "m/calc", "m/app"
	tm := routeMap("f.go:1", coverage.TestRef{Pkg: app, Name: "TestApp"}).WithSuitesForTesting(true,
		map[string]map[string]bool{calc: {}, app: {calc: true}},
		coverage.Package{ImportPath: calc}, coverage.Package{ImportPath: app})
	file := filepath.Join(t.TempDir(), "f.go")
	w, err := NewWorker(0, t.TempDir(), TimeoutPolicy{Global: global}, map[string][]byte{file: []byte("package calc\n")}, t.TempDir(), tm)
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	var started []string
	origStart := startCommandFunc
	t.Cleanup(func() { startCommandFunc = origStart })
	startCommandFunc = func(cmd *exec.Cmd) error {
		pkg := lastArg(cmd.Args)
		started = append(started, pkg)
		argv := cmdFor(pkg)
		path, err := exec.LookPath(argv[0])
		if err != nil {
			t.Fatalf("LookPath(%s): %v", argv[0], err)
		}
		cmd.Path, cmd.Args, cmd.Err = path, argv, nil
		return cmd.Start()
	}
	return w, mutator.Mutant{Pkg: calc, File: file, CoverageFile: "f.go", Line: 1, Status: mutator.StatusPending}, &started
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
