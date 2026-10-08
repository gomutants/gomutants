package coverage

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// groupIndex is a coverage index whose lines exercise testGroups and
// dropGroups: a group seen on two lines in different orders, a single
// test, a line with tests in two packages, and a line that shares a test
// with a group without being that group.
func groupIndex() map[string]map[testKey]bool {
	return map[string]map[testKey]bool{
		"f.go:1": {{pkg: "p", name: "TestA"}: true, {pkg: "p", name: "TestB"}: true},
		"f.go:2": {{pkg: "p", name: "TestB"}: true, {pkg: "p", name: "TestA"}: true},
		"f.go:3": {{pkg: "p", name: "TestC"}: true},
		"f.go:4": {{pkg: "p", name: "TestA"}: true, {pkg: "q", name: "TestY"}: true, {pkg: "q", name: "TestX"}: true},
		"f.go:5": {{pkg: "p", name: "TestA"}: true, {pkg: "p", name: "TestC"}: true},
	}
}

func equalGroups(a, b []testGroup) bool {
	return slices.EqualFunc(a, b, func(x, y testGroup) bool { return compareGroups(x, y) == 0 })
}

// TestTestGroups: each distinct group of two or more tests comes back
// once, per package, with its tests sorted, the groups sorted — and only
// from the listed lines when lines is non-nil.
func TestTestGroups(t *testing.T) {
	tm := &TestMap{index: groupIndex()}
	for _, tc := range []struct {
		name  string
		lines map[string]bool
		want  []testGroup
	}{
		{"every line", nil, []testGroup{
			{pkg: "p", tests: []string{"TestA", "TestB"}},
			{pkg: "p", tests: []string{"TestA", "TestC"}},
			{pkg: "q", tests: []string{"TestX", "TestY"}},
		}},
		{"listed lines", map[string]bool{"f.go:3": true, "f.go:4": true, "f.go:9": true}, []testGroup{
			{pkg: "q", tests: []string{"TestX", "TestY"}},
		}},
		{"no lines", map[string]bool{}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tm.testGroups(tc.lines); !equalGroups(got, tc.want) {
				t.Errorf("testGroups = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCompareGroups orders groups by package, then by their tests.
func TestCompareGroups(t *testing.T) {
	a := testGroup{pkg: "p", tests: []string{"TestA", "TestB"}}
	for _, tc := range []struct {
		b    testGroup
		want int
	}{
		{testGroup{pkg: "p", tests: []string{"TestA", "TestB"}}, 0},
		{testGroup{pkg: "p", tests: []string{"TestA", "TestC"}}, -1},
		{testGroup{pkg: "p", tests: []string{"TestA"}}, 1},
		{testGroup{pkg: "o", tests: []string{"TestZ", "TestZZ"}}, 1},
		{testGroup{pkg: "q", tests: []string{"Test0", "Test1"}}, -1},
	} {
		if got := compareGroups(a, tc.b); got != tc.want {
			t.Errorf("compareGroups(%v, %v) = %d, want %d", a, tc.b, got, tc.want)
		}
	}
}

// TestTestGroupKeyAndString: the key tells a group's package apart from
// its tests, and the string names both for the warning.
func TestTestGroupKeyAndString(t *testing.T) {
	a := testGroup{pkg: "p", tests: []string{"TestA", "TestB"}}
	if a.key() == (testGroup{pkg: "pTestA", tests: []string{"TestB"}}).key() {
		t.Errorf("%v shares its key with a group of another package", a)
	}
	if got, want := a.String(), "p: TestA, TestB"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

// TestDropGroups: a failed group leaves every line whose tests in its
// package are exactly that group, and only those; a line left without
// tests leaves the index, so the mutants on it run their whole package.
func TestDropGroups(t *testing.T) {
	tm := &TestMap{index: groupIndex()}
	tm.dropGroups([]testGroup{
		{pkg: "p", tests: []string{"TestA", "TestB"}},
		{pkg: "q", tests: []string{"TestX", "TestY"}},
	})
	want := map[string]map[testKey]bool{
		"f.go:3": {{pkg: "p", name: "TestC"}: true},
		"f.go:4": {{pkg: "p", name: "TestA"}: true},
		"f.go:5": {{pkg: "p", name: "TestA"}: true, {pkg: "p", name: "TestC"}: true},
	}
	if !maps.EqualFunc(tm.index, want, maps.Equal) {
		t.Errorf("index = %v, want %v", tm.index, want)
	}
}

// TestCheckGroups: each group runs once against its own package's binary
// with the timeout, at most `workers` at a time, and the failed groups
// come back sorted whatever order the runs finish in.
func TestCheckGroups(t *testing.T) {
	orig := groupPassesFunc
	t.Cleanup(func() { groupPassesFunc = orig })
	var (
		mu       sync.Mutex
		ran      []string
		inFlight atomic.Int32
		peak     atomic.Int32
	)
	groupPassesFunc = func(_ context.Context, cp *compiledPkg, tests []string, timeout time.Duration) bool {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		time.Sleep(20 * time.Millisecond)
		if timeout != 7*time.Second {
			t.Errorf("timeout = %s, want 7s", timeout)
		}
		mu.Lock()
		ran = append(ran, testGroup{pkg: cp.importPath, tests: tests}.String())
		mu.Unlock()
		return !slices.Contains(tests, "TestBad")
	}
	bins := map[string]*compiledPkg{"p": {importPath: "p"}, "q": {importPath: "q"}}
	groups := []testGroup{
		{pkg: "q", tests: []string{"TestBad", "TestX"}},
		{pkg: "p", tests: []string{"TestA", "TestB"}},
		{pkg: "p", tests: []string{"TestA", "TestBad"}},
		{pkg: "q", tests: []string{"TestX", "TestY"}},
	}
	failed := checkGroups(context.Background(), groups, bins, 7*time.Second, 2)

	if want := []testGroup{groups[2], groups[0]}; !equalGroups(failed, want) {
		t.Errorf("failed = %v, want %v", failed, want)
	}
	slices.Sort(ran)
	if want := []string{"p: TestA, TestB", "p: TestA, TestBad", "q: TestBad, TestX", "q: TestX, TestY"}; !slices.Equal(ran, want) {
		t.Errorf("ran %q, want each group once against its package: %q", ran, want)
	}
	if p := peak.Load(); p != 2 {
		t.Errorf("%d groups ran at once, want 2 workers' worth", p)
	}
}

// stubGroupBuild stubs BuildTestMap so that TestA and TestB both cover
// p/f.go:3 and pass alone, and every group run together passes or fails
// as `passes` says. It returns the groups run, as strings.
func stubGroupBuild(t *testing.T, passes bool) *[]string {
	t.Helper()
	stubBuildTestMapDeps(t,
		[]testEntry{{name: "TestA", pkg: "p"}, {name: "TestB", pkg: "p"}},
		[]resolvedPkg{{importPath: "p", dir: t.TempDir()}})
	runCompiledTestFunc = func(context.Context, *compiledPkg, string, string, time.Duration) ([]Block, time.Duration, error) {
		return []Block{{File: "p/f.go", StartLine: 3, EndLine: 3, Count: 1}}, time.Millisecond, nil
	}
	var mu sync.Mutex
	ran := &[]string{}
	groupPassesFunc = func(_ context.Context, cp *compiledPkg, tests []string, _ time.Duration) bool {
		mu.Lock()
		defer mu.Unlock()
		*ran = append(*ran, testGroup{pkg: cp.importPath, tests: tests}.String())
		return passes
	}
	return ran
}

// TestBuildTestMapDropsFailingGroup: tests that pass alone but fail
// together stop being routed to, and the warning says why and names them.
func TestBuildTestMapDropsFailingGroup(t *testing.T) {
	ran := stubGroupBuild(t, false)
	tm, err := BuildTestMap(context.Background(), t.TempDir(), []string{"./..."}, BuildOptions{TmpDir: t.TempDir(), Workers: 2})
	if err != nil {
		t.Fatalf("BuildTestMap: %v", err)
	}
	if want := []string{"p: TestA, TestB"}; !slices.Equal(*ran, want) {
		t.Errorf("groups run = %q, want %q", *ran, want)
	}
	if tests := tm.TestsFor("p/f.go", 3); tests != nil {
		t.Errorf("TestsFor(p/f.go, 3) = %v, want none", tests)
	}
	want := "test groups that fail when run together without any mutant: 1, so mutants on their lines run whole suites instead; first: p: TestA, TestB"
	if !slices.Equal(tm.Warnings(), []string{want}) {
		t.Errorf("Warnings() = %q, want [%q]", tm.Warnings(), want)
	}
}

// TestBuildTestMapKeepsPassingGroup: a group that passes together stays
// routed, without a warning.
func TestBuildTestMapKeepsPassingGroup(t *testing.T) {
	ran := stubGroupBuild(t, true)
	tm, err := BuildTestMap(context.Background(), t.TempDir(), []string{"./..."}, BuildOptions{TmpDir: t.TempDir(), Workers: 2})
	if err != nil {
		t.Fatalf("BuildTestMap: %v", err)
	}
	if len(*ran) != 1 {
		t.Errorf("groups run = %q, want the one group", *ran)
	}
	tests := tm.TestsFor("p/f.go", 3)
	slices.Sort(tests)
	if !slices.Equal(tests, []string{"TestA", "TestB"}) {
		t.Errorf("TestsFor(p/f.go, 3) = %v, want [TestA TestB]", tests)
	}
	if w := tm.Warnings(); len(w) != 0 {
		t.Errorf("Warnings() = %q, want none", w)
	}
}

// TestBuildTestMapChecksOnlyListedLines: with Lines set, a group covering
// no listed line isn't run, and stays in the map.
func TestBuildTestMapChecksOnlyListedLines(t *testing.T) {
	ran := stubGroupBuild(t, false)
	tm, err := BuildTestMap(context.Background(), t.TempDir(), []string{"./..."}, BuildOptions{
		TmpDir: t.TempDir(), Workers: 2, Lines: map[string]bool{LineKey("p/f.go", 4): true},
	})
	if err != nil {
		t.Fatalf("BuildTestMap: %v", err)
	}
	if len(*ran) != 0 {
		t.Errorf("groups run = %q, want none", *ran)
	}
	if tests := tm.TestsFor("p/f.go", 3); len(tests) != 2 {
		t.Errorf("TestsFor(p/f.go, 3) = %v, want both tests", tests)
	}
}

// TestBuildTestMapCancelledDuringGroupCheck reports the cancellation, not
// a map missing the groups that didn't get to run.
func TestBuildTestMapCancelledDuringGroupCheck(t *testing.T) {
	stubGroupBuild(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	groupPassesFunc = func(context.Context, *compiledPkg, []string, time.Duration) bool {
		cancel()
		return false
	}
	tm, err := BuildTestMap(ctx, t.TempDir(), []string{"./..."}, BuildOptions{TmpDir: t.TempDir(), Workers: 1})
	if !errors.Is(err, context.Canceled) || tm != nil {
		t.Errorf("BuildTestMap = (%v, %v), want (nil, context.Canceled)", tm, err)
	}
}

func TestLineKey(t *testing.T) {
	if got := LineKey("m/p/f.go", 12); got != "m/p/f.go:12" {
		t.Errorf("LineKey = %q, want m/p/f.go:12", got)
	}
}
