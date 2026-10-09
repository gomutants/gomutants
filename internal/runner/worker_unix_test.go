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
				fake := startCommandFunc
				startCommandFunc = func(cmd *exec.Cmd) error {
					if _, ok := builtBin(cmd.Args); ok {
						return fake(cmd)
					}
					return tc.start
				}
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

// TestKillGroupOnCancelFallsBack: when the group can't be signalled (on
// macOS, just after Start, the child may not have joined it yet), the
// cancel kills the process alone.
func TestKillGroupOnCancelFallsBack(t *testing.T) {
	orig := syscallKillFunc
	t.Cleanup(func() { syscallKillFunc = orig })
	syscallKillFunc = func(int, syscall.Signal) error { return syscall.ESRCH }
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sleep", "30")
	applyProcessGroup(cmd)
	killGroupOnCancel(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	cancel()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		_ = cmd.Process.Kill()
		t.Fatal("the cancel left the process running")
	}
}

// TestWorkerTestDeadlineKillsProcessGroup: a run cut off at its deadline
// takes down every process its tests started, so one that holds the
// output open can't keep the mutant waiting. With the binary alone
// killed, the wait would last until the started process exits (3s), as
// the drain delay is longer.
func TestWorkerTestDeadlineKillsProcessGroup(t *testing.T) {
	origDrain := pipeDrainDelay
	t.Cleanup(func() { pipeDrainDelay = origDrain })
	pipeDrainDelay = 10 * time.Second
	w, m := fakeWorker(t, routeMap("f.go:1", coverage.TestRef{Pkg: "m/calc", Name: "TestCalc"}), time.Second)
	fakeBuildsAndRuns(t, func(string) []string { return []string{"true"} },
		func(string) []string { return []string{"sh", "-c", "sleep 3 & sleep 3"} })
	start := time.Now()
	if got := w.runGroups(context.Background(), m, w.routeGroups(m), false, 300*time.Millisecond); got != mutator.StatusTimedOut {
		t.Errorf("runGroups = %v, want TIMED OUT", got)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("runGroups took %v, want the group killed at the 300ms deadline", took)
	}
}

// TestWorkerTestBuildDeadline: a build that hangs without growing — on a
// module proxy, a stuck -toolexec — is cut off at buildTimeout with its
// whole process group, so it can't hold its worker forever, and reads as
// INFRA ERROR: a host problem, re-run next time rather than cached. With
// the build unbounded, or `go` alone killed, it would last 3s.
func TestWorkerTestBuildDeadline(t *testing.T) {
	origBuild, origDrain := buildTimeout, pipeDrainDelay
	t.Cleanup(func() { buildTimeout, pipeDrainDelay = origBuild, origDrain })
	buildTimeout, pipeDrainDelay = 300*time.Millisecond, 10*time.Second
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
// waiting no longer than pipeDrainDelay past the deadline.
func TestWorkerTestDeadlineDrainsBounded(t *testing.T) {
	origDrain, origKill := pipeDrainDelay, syscallKillFunc
	t.Cleanup(func() { pipeDrainDelay, syscallKillFunc = origDrain, origKill })
	pipeDrainDelay = 200 * time.Millisecond
	syscallKillFunc = func(int, syscall.Signal) error { return syscall.ESRCH }
	w, m := fakeWorker(t, routeMap("f.go:1", coverage.TestRef{Pkg: "m/calc", Name: "TestCalc"}), time.Second)
	fakeBuildsAndRuns(t, func(string) []string { return []string{"true"} },
		func(string) []string { return []string{"sh", "-c", "sleep 3 & sleep 3"} })
	start := time.Now()
	if got := w.runGroups(context.Background(), m, w.routeGroups(m), false, 300*time.Millisecond); got != mutator.StatusTimedOut {
		t.Errorf("runGroups = %v, want TIMED OUT", got)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("runGroups took %v, want the wait cut off 200ms past the 300ms deadline", took)
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
func addWorker(t *testing.T, body string) (*Worker, TimeoutPolicy, mutator.Mutant) {
	t.Helper()
	dir := t.TempDir()
	// The comment keeps the mutant's build, link included, out of the
	// build cache of an earlier run.
	src := fmt.Sprintf("package testpkg\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n\n// %d\n", time.Now().UnixNano())
	testSrc := "package testpkg\n\nimport (\n\t\"testing\"\n\t\"time\"\n)\n\nvar _ = time.Second\n\n" +
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
