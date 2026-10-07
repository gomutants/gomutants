package runner

import (
	"time"

	"github.com/szhekpisov/gomutants/internal/coverage"
	"github.com/szhekpisov/gomutants/internal/mutator"
)

// TimeoutPolicy decides the per-mutant `go test` deadline. Without
// adaptive sizing every mutant inherits the same baseline×coefficient
// timeout, which on multi-package projects pins workers on infinite-loop
// mutants for whole-suite-sized intervals.
//
//   - Adaptive=false → every mutant gets Global. Behavior matches pre-
//     adaptive gomutants exactly; used as the kill switch.
//   - Adaptive=true  → per-mutant timeout =
//     clamp((baseSum+rebuild)*Margin, Min, Global) where baseSum is the
//     sum of the mutant's covering tests' durations from the coverage map
//     and rebuild is what its `go test` runs spend rebuilding their test
//     binaries. A mutant without both gets Global.
//
// All clamps point in the safe direction: a missing measurement falls
// back to a longer timeout, never a shorter one. Worst-case the user
// sees pre-adaptive wait times; they never see false TIMED_OUT inflation.
type TimeoutPolicy struct {
	// Global is the absolute upper bound (baseline*coefficient). Also
	// the value used when Adaptive is false.
	Global time.Duration

	// Margin scales the observed per-test/per-package sum. 1.0 means
	// "trust the measurement exactly"; values >1 add headroom for GC,
	// scheduler jitter, mutated-code slowdowns, and contention under
	// the parallel mutation phase (which runs at higher concurrency
	// than the coverage build that produced the measurements).
	Margin float64

	// Min is the floor — the smallest value For() will return when
	// adaptive computation produces something tiny. Absorbs cold-start,
	// child fork, and GC-pause overhead that doesn't scale with the
	// underlying test work. Without it a 5ms test gets a 15ms deadline
	// (with Margin=3) and flakes on a busy machine.
	Min time.Duration

	// Adaptive is the master switch. When false, For() always returns
	// Global; the per-mutant tm/m arguments are ignored.
	Adaptive bool
}

// For returns the deadline for the run of m's covering tests (see
// Worker.Test), consulting `tm` for per-test timings.
//
// When adaptive, it is the sum of the selected per-test durations — the
// actual tests this mutant will run via -run=^(TestA|TestB)$ — plus the
// time the mutant's `go test` invocations spend rebuilding their test
// binaries with the mutant in place, which the deadline covers too: the
// measured rebuild of every package they run in (see rebuildCost).
//
// A mutant without covering tests runs its whole package, which gets
// Global: the map's timings don't describe that run, as a test that
// failed or hung when run alone has none, and a mutant on a line no test
// covers rarely hangs. So does a package whose rebuild wasn't measured —
// the floor alone can be shorter than a link under load, and a deadline
// that runs out mid-build turns the mutant TIMED_OUT, which drops it from
// the efficacy denominator.
//
// The output is clamped: max(scaled, Min), then min(that, Global).
// Both clamps fail safe — too-tight measurements widen to Min, and a
// pathological multiplication can never escape Global.
func (p TimeoutPolicy) For(tm *coverage.TestMap, m mutator.Mutant) time.Duration {
	if !p.Adaptive {
		return p.Global
	}

	// Sum the per-test durations by (pkg, name) reference so a mutant
	// routed to covering tests in importing packages (integration mode) is
	// sized from those tests' real durations. SumDurationsForRefs returns
	// complete=false on no refs or any missing entry, and its contract
	// guarantees complete=true ⇒ base>0 (sums of strictly-positive
	// recordDuration entries), so a single `!complete` guard handles every
	// "no data" case.
	refs := tm.TestRefsFor(m.CoverageFile, m.Line)
	base, complete := tm.SumDurationsForRefs(refs)
	if !complete {
		return p.Global
	}
	rebuild, measured := rebuildCost(tm, refs)
	if !measured {
		return p.Global
	}

	// Use max/min builtins so the clamps don't surface as
	// CONDITIONALS_BOUNDARY mutation targets (the previous if-form had
	// equivalent mutants on the equality cases). Same idiom as Worker.Test
	// uses for its capped-buffer clamp.
	return min(p.Global, max(p.Min, time.Duration(float64(base+rebuild)*p.Margin)))
}

// rebuildCost sums the measured rebuild durations (see
// coverage.TestMap.RebuildDuration) of the packages a mutant's covering
// tests run in. measured is false when any of them has no measurement.
func rebuildCost(tm *coverage.TestMap, refs []coverage.TestRef) (total time.Duration, measured bool) {
	pkgs := map[string]bool{}
	for _, r := range refs {
		pkgs[r.Pkg] = true
	}
	for pkg := range pkgs {
		d, ok := tm.RebuildDuration(pkg)
		if !ok {
			return 0, false
		}
		total += d
	}
	return total, true
}
