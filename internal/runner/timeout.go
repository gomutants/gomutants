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
//     sum of selected per-test durations from the coverage map
//     (preferred), or the per-package total (fallback when no per-test
//     set is known), or 0 (degrade to Global), and rebuild is what the
//     mutant's `go test` runs spend rebuilding their test binaries.
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

// For returns the per-mutant timeout for `m`, consulting `tm` for
// per-test and per-package timings.
//
// Selection order (when adaptive):
//  1. Sum of selected per-test durations (the actual tests this mutant
//     will run via -run=^(TestA|TestB)$). Tightest, most accurate.
//  2. Per-package total (sum of all timed tests in the mutant's package).
//     Used when no per-test set is known — typically when the mutated
//     line isn't in any covered block, so the runner falls back to
//     running the whole package.
//  3. Global. Last resort: no measurements available at all.
//
// Added to it is the time the mutant's `go test` invocations spend
// rebuilding their test binaries with the mutant in place, which the
// deadline covers too: the measured rebuild of every package they run in
// (see rebuildCost). A package whose rebuild wasn't measured means Global
// as well — the floor alone can be shorter than a link under load, and a
// deadline that runs out mid-build turns the mutant TIMED_OUT, which
// drops it from the efficacy denominator.
//
// The output is clamped: max(scaled, Min), then min(that, Global).
// Both clamps fail safe — too-tight measurements widen to Min, and a
// pathological multiplication can never escape Global.
func (p TimeoutPolicy) For(tm *coverage.TestMap, m mutator.Mutant) time.Duration {
	if !p.Adaptive {
		return p.Global
	}

	// A package run in full because the map couldn't cover it test by test
	// has no per-test timings that describe the run.
	if len(tm.FullRunPkgs(m.Pkg)) > 0 {
		return p.Global
	}

	// Try the per-test sum first, by (pkg, name) reference so a mutant
	// routed to covering tests in importing packages (integration mode) is
	// sized from those tests' real durations. SumDurationsForRefs returns
	// complete=false on any missing entry; in that case fall through to
	// package-level data. Its contract guarantees complete=true ⇒ base>0
	// (sums of strictly-positive recordDuration entries), so a single
	// `!complete` guard handles both "no data" cases — checking `base <= 0`
	// here as well would be dead and shows up as four equivalent mutants.
	refs := tm.TestRefsFor(m.CoverageFile, m.Line)
	base, complete := tm.SumDurationsForRefs(refs)
	if !complete {
		base = tm.PackageDuration(m.Pkg)
	}
	if base <= 0 {
		// No data at all — fall back to global. Conservative: longer is safer.
		return p.Global
	}
	rebuild, measured := rebuildCost(tm, refs, m.Pkg)
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
// coverage.TestMap.RebuildDuration) of the packages a mutant's `go test`
// invocations run in: those of its covering tests, or its own package
// when it has none, as in Worker.testInvocations. measured is false when
// any of them has no measurement.
func rebuildCost(tm *coverage.TestMap, refs []coverage.TestRef, ownPkg string) (total time.Duration, measured bool) {
	pkgs := map[string]bool{}
	for _, r := range refs {
		pkgs[r.Pkg] = true
	}
	if len(pkgs) == 0 {
		pkgs[ownPkg] = true
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
