package runner

import (
	"testing"
	"time"

	"github.com/gomutants/gomutants/internal/coverage"
	"github.com/gomutants/gomutants/internal/mutator"
)

// newTestMapWithDurations builds a TestMap fixture from raw timing data,
// using only exported APIs that mutate state through public records. We
// use coverage.NewTestMapForTesting (added below) to avoid exporting
// internal map shape from the coverage package.
//
// Falling back to the BuildTestMap pipeline would require spinning up
// real `go test` invocations, which is the opposite of what a unit test
// for the timeout selector should do.
func newTestMapWithDurations(t *testing.T, perTest map[[2]string]time.Duration, coverIndex map[string][]coverage.TestRef) *coverage.TestMap {
	t.Helper()
	tm := coverage.NewTestMapForTesting(perTest, coverIndex)
	return tm
}

func TestTimeoutPolicyForAdaptiveDisabled(t *testing.T) {
	p := TimeoutPolicy{Global: 30 * time.Second, Margin: 3, Min: time.Second, Adaptive: false}
	got := p.For(nil, mutator.Mutant{Pkg: "p", CoverageFile: "f.go", Line: 1})
	if got != 30*time.Second {
		t.Errorf("Adaptive=false should return Global; got %v", got)
	}
}

// Adaptive=false must short-circuit even when the TestMap has timing
// data that would otherwise drive a much shorter per-mutant timeout.
// Drives the Adaptive=false branch with a populated map; if the early
// `if !p.Adaptive` return is elided (BRANCH_IF), the function falls
// through and returns the Min-clamped 1s instead of Global 30s.
func TestTimeoutPolicyForAdaptiveDisabledIgnoresTestMap(t *testing.T) {
	tm := newTestMapWithDurations(t,
		map[[2]string]time.Duration{
			{"p", "TestA"}: 100 * time.Millisecond,
		},
		map[string][]coverage.TestRef{
			"f.go:1": {{Pkg: "p", Name: "TestA"}},
		},
	)
	p := TimeoutPolicy{Global: 30 * time.Second, Margin: 3, Min: time.Second, Adaptive: false}
	got := p.For(tm, mutator.Mutant{Pkg: "p", CoverageFile: "f.go", Line: 1})
	if got != 30*time.Second {
		t.Errorf("Adaptive=false must ignore TestMap and return Global; got %v", got)
	}
}

// TestTimeoutPolicyForSumsAcrossPackages: a mutant's covering tests in
// several packages get one deadline from all their durations, however
// many packages their binaries are built in: the builds happen before the
// deadline starts (see Worker.runGroups).
func TestTimeoutPolicyForSumsAcrossPackages(t *testing.T) {
	tm := newTestMapWithDurations(t,
		map[[2]string]time.Duration{
			{"p", "TestA"}: 100 * time.Millisecond,
			{"p", "TestB"}: 100 * time.Millisecond,
			{"q", "TestC"}: 300 * time.Millisecond,
		},
		map[string][]coverage.TestRef{
			"f.go:1": {{Pkg: "p", Name: "TestA"}, {Pkg: "p", Name: "TestB"}, {Pkg: "q", Name: "TestC"}},
		},
	)
	p := TimeoutPolicy{Global: 30 * time.Second, Margin: 2, Min: 100 * time.Millisecond, Adaptive: true}
	if got := p.For(tm, mutator.Mutant{Pkg: "p", CoverageFile: "f.go", Line: 1}); got != time.Second {
		t.Errorf("For = %v, want 1s: (100ms+100ms+300ms) × 2", got)
	}
}

func TestTimeoutPolicyForUsesPerTestSum(t *testing.T) {
	tm := newTestMapWithDurations(t,
		map[[2]string]time.Duration{
			{"p", "TestA"}: 100 * time.Millisecond,
			{"p", "TestB"}: 200 * time.Millisecond,
		},
		map[string][]coverage.TestRef{
			"f.go:10": {{Pkg: "p", Name: "TestA"}, {Pkg: "p", Name: "TestB"}},
		},
	)
	p := TimeoutPolicy{Global: 30 * time.Second, Margin: 3, Min: time.Second, Adaptive: true}
	got := p.For(tm, mutator.Mutant{Pkg: "p", CoverageFile: "f.go", Line: 10})
	want := 900 * time.Millisecond // (100ms+200ms) * 3
	// 900ms is below the 1s floor → clamps to 1s.
	if got != time.Second {
		t.Errorf("got %v; want 1s (per-test sum 300ms × 3 = 900ms, clamped to Min 1s) — got base %v", got, want)
	}
}

func TestTimeoutPolicyForFloorClampsBelowMin(t *testing.T) {
	tm := newTestMapWithDurations(t,
		map[[2]string]time.Duration{
			{"p", "TestA"}: 5 * time.Millisecond,
		},
		map[string][]coverage.TestRef{
			"f.go:1": {{Pkg: "p", Name: "TestA"}},
		},
	)
	p := TimeoutPolicy{Global: 30 * time.Second, Margin: 3, Min: 2 * time.Second, Adaptive: true}
	got := p.For(tm, mutator.Mutant{Pkg: "p", CoverageFile: "f.go", Line: 1})
	if got != 2*time.Second {
		t.Errorf("scaled 15ms must clamp up to Min 2s; got %v", got)
	}
}

func TestTimeoutPolicyForCeilingClampsAboveGlobal(t *testing.T) {
	tm := newTestMapWithDurations(t,
		map[[2]string]time.Duration{
			{"p", "TestSlow"}: 60 * time.Second,
		},
		map[string][]coverage.TestRef{
			"f.go:1": {{Pkg: "p", Name: "TestSlow"}},
		},
	)
	p := TimeoutPolicy{Global: 30 * time.Second, Margin: 3, Min: time.Second, Adaptive: true}
	got := p.For(tm, mutator.Mutant{Pkg: "p", CoverageFile: "f.go", Line: 1})
	if got != 30*time.Second {
		t.Errorf("scaled 180s must clamp down to Global 30s; got %v", got)
	}
}

// TestTimeoutPolicyForMissingPerTestUsesGlobal: a covering test with no
// recorded duration gives no basis for the deadline, so the mutant gets
// the ceiling rather than one sized from the package's other tests.
func TestTimeoutPolicyForMissingPerTestUsesGlobal(t *testing.T) {
	tm := newTestMapWithDurations(t,
		map[[2]string]time.Duration{
			{"p", "TestA"}: 50 * time.Millisecond,
			{"p", "TestB"}: 50 * time.Millisecond,
		},
		map[string][]coverage.TestRef{
			"f.go:1": {{Pkg: "p", Name: "TestUnseen"}},
		},
	)
	p := TimeoutPolicy{Global: 30 * time.Second, Margin: 4, Min: 0, Adaptive: true}
	if got := p.For(tm, mutator.Mutant{Pkg: "p", CoverageFile: "f.go", Line: 1}); got != p.Global {
		t.Errorf("missing per-test entry: got %v, want the 30s ceiling", got)
	}
}

func TestTimeoutPolicyForFallsBackToGlobalWhenNoData(t *testing.T) {
	tm := newTestMapWithDurations(t, nil, nil)
	p := TimeoutPolicy{Global: 30 * time.Second, Margin: 3, Min: time.Second, Adaptive: true}
	got := p.For(tm, mutator.Mutant{Pkg: "unknown", CoverageFile: "f.go", Line: 1})
	if got != 30*time.Second {
		t.Errorf("no data → Global; got %v", got)
	}
}

// TestWorkerComputeTimeoutWiresPolicyAndTestMap binds the field plumbing
// in Worker.computeTimeout to the policy's semantics. With Adaptive=true
// and a populated TestMap, the resolved timeout must reflect the per-test
// sum × Margin (clamped to Min/Global), not the Global ceiling. Catches
// the regression where a refactor passes nil — or the wrong field —
// into policy.For and silently downgrades every mutant.
func TestWorkerComputeTimeoutWiresPolicyAndTestMap(t *testing.T) {
	tm := newTestMapWithDurations(t,
		map[[2]string]time.Duration{
			{"p", "TestA"}: time.Second, // 1s × 3 = 3s, between Min(2s) and Global(30s)
		},
		map[string][]coverage.TestRef{
			"f.go:1": {{Pkg: "p", Name: "TestA"}},
		},
	)
	w := &Worker{
		policy: TimeoutPolicy{
			Global: 30 * time.Second, Margin: 3, Min: 2 * time.Second, Adaptive: true,
		},
		testMap: tm,
	}
	got := w.computeTimeout(mutator.Mutant{Pkg: "p", CoverageFile: "f.go", Line: 1})
	want := 3 * time.Second
	if got != want {
		t.Errorf("computeTimeout = %v, want %v — Worker isn't passing its TestMap into policy.For; the Global ceiling (%v) would surface here instead", got, want, w.policy.Global)
	}
}

// TestTimeoutPolicyForNoCoveringSetUsesGlobal: a mutant on a line no
// test covers runs its whole package, whose timings the map doesn't hold
// in full (a test that failed or hung alone has none), so it gets the
// ceiling.
func TestTimeoutPolicyForNoCoveringSetUsesGlobal(t *testing.T) {
	tm := newTestMapWithDurations(t,
		map[[2]string]time.Duration{
			{"p", "TestA"}: 250 * time.Millisecond,
		},
		map[string][]coverage.TestRef{
			"other.go:5": {{Pkg: "p", Name: "TestA"}},
		},
	)
	p := TimeoutPolicy{Global: 30 * time.Second, Margin: 2, Min: 0, Adaptive: true}
	if got := p.For(tm, mutator.Mutant{Pkg: "p", CoverageFile: "f.go", Line: 1}); got != p.Global {
		t.Errorf("uncovered line: got %v, want the 30s ceiling", got)
	}
}
