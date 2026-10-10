package runner

import (
	"time"

	"github.com/gomutants/gomutants/internal/coverage"
	"github.com/gomutants/gomutants/internal/mutator"
)

// TimeoutPolicy decides the per-mutant `go test` deadline. Without
// adaptive sizing every mutant inherits the same baseline×coefficient
// timeout, which on multi-package projects pins workers on infinite-loop
// mutants for whole-suite-sized intervals.
//
//   - Adaptive=false → every mutant gets Global. Behavior matches pre-
//     adaptive gomutants exactly; used as the kill switch.
//   - Adaptive=true  → per-mutant timeout = clamp(baseSum*Margin, Min,
//     Global) where baseSum is the sum of the mutant's covering tests'
//     durations from the coverage map. A mutant without it gets Global.
//
// The deadline bounds running the mutant's tests, not building their test
// binaries, which happens first, outside it (see Worker.runGroups).
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
// actual tests this mutant will run via -test.run=^(TestA|TestB)$, in
// whichever packages they are — scaled by Margin. It covers no build: the
// tests' binaries are built with the mutant in place before the deadline
// starts.
//
// A mutant without covering tests runs its whole package, which gets
// Global: the map's timings don't describe that run, as a test that
// failed or hung when run alone has none, and a mutant on a line no test
// covers rarely hangs.
//
// The output is clamped: max(scaled, Min), then min(that, Global).
// Both clamps fail safe — too-tight measurements widen to Min, and a
// pathological multiplication can never escape Global.
func (p TimeoutPolicy) For(tm *coverage.TestMap, m mutator.Mutant) time.Duration {
	if !p.Adaptive {
		return p.Global
	}

	// Sum the per-test durations by (pkg, name) reference, as a test's
	// name is unique only within its package. SumDurationsForRefs returns
	// complete=false on no refs or any missing entry, and its contract
	// guarantees complete=true ⇒ base>0 (sums of strictly-positive
	// recordDuration entries), so a single `!complete` guard handles every
	// "no data" case.
	base, complete := tm.SumDurationsForRefs(tm.TestRefsFor(m.CoverageFile, m.Line))
	if !complete {
		return p.Global
	}

	// Use max/min builtins so the clamps don't surface as
	// CONDITIONALS_BOUNDARY mutation targets (the previous if-form had
	// equivalent mutants on the equality cases). Same idiom as Worker.Test
	// uses for its capped-buffer clamp.
	return min(p.Global, max(p.Min, time.Duration(float64(base)*p.Margin)))
}
