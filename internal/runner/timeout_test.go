package runner

import (
	"testing"
	"time"

	"github.com/szhekpisov/gomutants/internal/coverage"
	"github.com/szhekpisov/gomutants/internal/mutator"
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

// TestTimeoutPolicyForAddsRebuilds: the deadline also covers rebuilding
// the test binary of every package the mutant's tests run in — each once,
// however many of its tests run. The floor alone can be shorter than a
// link under load.
func TestTimeoutPolicyForAddsRebuilds(t *testing.T) {
	tm := newTestMapWithDurations(t,
		map[[2]string]time.Duration{
			{"p", "TestA"}: 100 * time.Millisecond,
			{"p", "TestB"}: 100 * time.Millisecond,
			{"q", "TestC"}: 100 * time.Millisecond,
		},
		map[string][]coverage.TestRef{
			"f.go:1": {{Pkg: "p", Name: "TestA"}, {Pkg: "p", Name: "TestB"}},
			"f.go:2": {{Pkg: "p", Name: "TestA"}, {Pkg: "q", Name: "TestC"}},
		},
	).WithRebuildsForTesting(map[string]time.Duration{"p": 500 * time.Millisecond, "q": time.Second})
	p := TimeoutPolicy{Global: 30 * time.Second, Margin: 2, Min: time.Second, Adaptive: true}
	cases := []struct {
		name string
		m    mutator.Mutant
		want time.Duration
	}{
		{"one package", mutator.Mutant{Pkg: "p", CoverageFile: "f.go", Line: 1}, 1400 * time.Millisecond},
		{"two packages", mutator.Mutant{Pkg: "p", CoverageFile: "f.go", Line: 2}, 3400 * time.Millisecond},
	}
	for _, tc := range cases {
		if got := p.For(tm, tc.m); got != tc.want {
			t.Errorf("%s: For = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestTimeoutPolicyForUnmeasuredRebuild: a package whose rebuild wasn't
// measured gives no basis for the build part of the deadline, so the
// mutant gets Global.
func TestTimeoutPolicyForUnmeasuredRebuild(t *testing.T) {
	tm := newTestMapWithDurations(t,
		map[[2]string]time.Duration{{"p", "TestA"}: 100 * time.Millisecond, {"q", "TestC"}: 100 * time.Millisecond},
		map[string][]coverage.TestRef{"f.go:1": {{Pkg: "p", Name: "TestA"}, {Pkg: "q", Name: "TestC"}}},
	).WithRebuildsForTesting(map[string]time.Duration{"p": 0})
	p := TimeoutPolicy{Global: 30 * time.Second, Margin: 3, Min: time.Second, Adaptive: true}
	if got := p.For(tm, mutator.Mutant{Pkg: "p", CoverageFile: "f.go", Line: 1}); got != 30*time.Second {
		t.Errorf("For = %v, want Global with q's rebuild unmeasured", got)
	}
}

// TestRebuildCost: a measured rebuild sum for the packages run, and none
// at all — not a partial sum — once one of them is unmeasured.
func TestRebuildCost(t *testing.T) {
	tm := coverage.NewTestMapForTesting(nil, nil).WithRebuildsForTesting(map[string]time.Duration{"p": time.Second, "q": 2 * time.Second})
	refs := []coverage.TestRef{{Pkg: "p", Name: "TestA"}, {Pkg: "q", Name: "TestB"}, {Pkg: "p", Name: "TestC"}}
	if total, ok := rebuildCost(tm, refs); total != 3*time.Second || !ok {
		t.Errorf("rebuildCost(p, q) = (%v, %v), want (3s, true)", total, ok)
	}
	if total, ok := rebuildCost(tm, append(refs, coverage.TestRef{Pkg: "r", Name: "TestD"})); total != 0 || ok {
		t.Errorf("rebuildCost with r unmeasured = (%v, %v), want (0, false)", total, ok)
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

// TestWorkerTestInvocationsUsesAdaptiveTimeout closes the loop end-to-end:
// the per-mutant timeout chosen by computeTimeout must thread into the
// `-timeout=` flag that `go test` actually receives. A refactor that
// reverts to threading w.policy.Global directly would still pass
// TestWorkerComputeTimeoutWiresPolicyAndTestMap; this test catches that
// by asserting the args carry the adaptive value.
func TestWorkerTestInvocationsUsesAdaptiveTimeout(t *testing.T) {
	tm := newTestMapWithDurations(t,
		map[[2]string]time.Duration{
			{"p", "TestA"}: 500 * time.Millisecond,
		},
		map[string][]coverage.TestRef{
			"f.go:7": {{Pkg: "p", Name: "TestA"}},
		},
	)
	w := &Worker{
		policy: TimeoutPolicy{
			Global: 30 * time.Second, Margin: 4, Min: 0, Adaptive: true,
		},
		testMap:     tm,
		overlayPath: "/tmp/overlay.json",
	}
	m := mutator.Mutant{Pkg: "p", CoverageFile: "f.go", Line: 7}

	timeout := w.computeTimeout(m)
	wantTimeout := 2 * time.Second // 500ms × 4 = 2s, no clamp
	if timeout != wantTimeout {
		t.Fatalf("computeTimeout = %v, want %v (precondition for arg test)", timeout, wantTimeout)
	}

	args := onlyInvocation(t, w, m, false, timeout)
	wantArg := "-timeout=" + wantTimeout.String()
	found := false
	for _, a := range args {
		if a == wantArg {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("args missing %q; got: %v — the routed run must thread the resolved adaptive timeout, not w.policy.Global", wantArg, args)
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
