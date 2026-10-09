package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gomutants/gomutants/internal/report"
)

func TestIntegrationSimple(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	dir, _ := os.Getwd()
	outPath := filepath.Join(t.TempDir(), "report.json")

	err := run(context.Background(), []string{
		"-w", "4",
		"-o", outPath,
		"./testdata/simple/",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	r := loadReport(t, outPath)

	// Expected: 68 total, 0 not covered (all positions in tested
	// files are testable thanks to the FilterByCoverage relaxation
	// that runs uninstrumented positions in tested files), 68 tested.
	// Every function in the fixture returns a value, so the return-value
	// mutators contribute 17 of those on their own.
	if r.MutantsTotal != 68 {
		t.Errorf("total=%d, want 68", r.MutantsTotal)
	}
	if r.MutantsNotCovered != 0 {
		t.Errorf("not_covered=%d, want 0", r.MutantsNotCovered)
	}

	// All mutants should be either killed, lived, or not viable.
	tested := r.MutantsKilled + r.MutantsLived + r.MutantsNotViable
	if tested != 68 {
		t.Errorf("tested=%d (killed=%d lived=%d not_viable=%d), want 68 total",
			tested, r.MutantsKilled, r.MutantsLived, r.MutantsNotViable)
	}

	// Efficacy should be > 80% (strong tests).
	if r.TestEfficacy < 80 {
		t.Errorf("efficacy=%.1f%%, want >= 80%%", r.TestEfficacy)
	}

	_ = dir
}

func TestIntegrationUntested(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	outPath := filepath.Join(t.TempDir(), "report.json")

	err := run(context.Background(), []string{
		"-w", "4",
		"-o", outPath,
		"./testdata/untested/",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	r := loadReport(t, outPath)

	// Expected: 17 total, 9 not covered. IsEven has no test, so all seven
	// of its mutants are uncovered — its two literals each emit
	// IntegerIncrement+IntegerDecrement alongside the original
	// ARITHMETIC_BASE / CONDITIONALS_NEGATION, plus RETURN_TRUE and
	// RETURN_FALSE on the returned comparison. The ninth is the RETURN_ZERO
	// on Max's `return b`, which TestMax's single direction never reaches.
	if r.MutantsTotal != 17 {
		t.Errorf("total=%d, want 17", r.MutantsTotal)
	}
	if r.MutantsNotCovered != 9 {
		t.Errorf("not_covered=%d, want 9", r.MutantsNotCovered)
	}

	// Weak tests — some mutants should survive.
	if r.MutantsLived == 0 {
		t.Error("expected some lived mutants with weak tests")
	}

	// Efficacy should be < 100% (intentionally weak tests).
	if r.TestEfficacy >= 100 {
		t.Errorf("efficacy=%.1f%%, want < 100%% with weak tests", r.TestEfficacy)
	}
}

func TestIntegrationDirectives(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	outPath := filepath.Join(t.TempDir(), "report.json")

	err := run(context.Background(), []string{
		"-w", "4",
		"--cache=off",
		"-o", outPath,
		"./testdata/directives/",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	r := loadReport(t, outPath)

	// Eight mutants are suppressed across the four directive forms:
	// 1× same-line on Add (scoped to ARITHMETIC_BASE);
	// 3× next-line on Sub (wildcard suppresses ARITHMETIC + INVERT_NEGATIVES
	//    + RETURN_ZERO);
	// 2× disable-func on Magic body (ARITHMETIC_BASE + RETURN_ZERO);
	// 2× disable-regexp on Mul's `return a * b` (ARITHMETIC_BASE + RETURN_ZERO).
	if r.MutantsSuppressed != 8 {
		t.Errorf("MutantsSuppressed=%d, want 8", r.MutantsSuppressed)
	}

	// Suppressed mutants must not roll into MutantsTotal. Add's directive
	// names ARITHMETIC_BASE only, so its RETURN_ZERO survives suppression
	// alongside both of Plain's.
	if r.MutantsTotal != 3 {
		t.Errorf("MutantsTotal=%d, want 3 (Plain's `+` and RETURN_ZERO, plus Add's RETURN_ZERO)", r.MutantsTotal)
	}
	if r.MutantsKilled != 3 {
		t.Errorf("MutantsKilled=%d, want 3", r.MutantsKilled)
	}

	// No suppressed mutant should appear in Files[].Mutations.
	for _, f := range r.Files {
		for _, m := range f.Mutations {
			// Sub's `a - b` is on line 11; Magic's `a + b` is on line 18;
			// Mul's `a * b` is on line 28. Add's line-5 ARITHMETIC_BASE
			// is suppressed but a non-arithmetic mutator on the same line
			// (if any) would be allowed — the fixture has none.
			if m.Line == 5 && m.Type == "ARITHMETIC_BASE" {
				t.Errorf("Add line 5 ARITHMETIC_BASE should be suppressed: %+v", m)
			}
			if m.Line == 11 {
				t.Errorf("Sub line 11 should be suppressed: %+v", m)
			}
			if m.Line == 18 {
				t.Errorf("Magic line 18 (inside disable-func) should be suppressed: %+v", m)
			}
			if m.Line == 28 {
				t.Errorf("Mul line 28 (regexp match) should be suppressed: %+v", m)
			}
		}
	}
}

func loadReport(t *testing.T, path string) *report.Report {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading report: %v", err)
	}
	var r report.Report
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatalf("parsing report: %v", err)
	}
	assertStableIDs(t, &r)
	return &r
}

// assertStableIDs holds the report-wide invariant every consumer of the
// `id` field depends on: each entry carries one, and no two entries share
// it. Run on every report the integration tests load, so any mutator or
// anchoring change that lets two mutants collapse onto one ID fails here
// rather than silently in a consumer.
func assertStableIDs(t *testing.T, r *report.Report) {
	t.Helper()
	seen := make(map[string]string)
	for _, f := range r.Files {
		for _, m := range f.Mutations {
			if m.ID == "" {
				t.Errorf("%s:%d:%d (%s) has an empty id", f.FileName, m.Line, m.Column, m.Type)
				continue
			}
			if prev, dup := seen[m.ID]; dup {
				t.Errorf("duplicate id %q: %s and %s:%d:%d", m.ID, prev, f.FileName, m.Line, m.Column)
			}
			seen[m.ID] = fmt.Sprintf("%s:%d:%d", f.FileName, m.Line, m.Column)
		}
	}
}

// The --exclude-calls tests below run over testdata/excludecalls, where
// the same arithmetic appears both inside and outside excluded calls.
// Together they cover the three states a project can be in: the built-in
// set alone, the built-in set plus a project pattern, and the built-ins
// switched off.
//
// Fixture source lines, named so a fixture edit fails loudly here rather
// than silently weakening an assertion.
const (
	fxLogPrintfLine = 21 // log.Printf("...", done*100/total)
	fxReturnLine    = 22 // return done * 100 / total
	fxFatalfLine    = 30 // log.Fatalf("negative: %d", n*2)
	fxDebugLine     = 39 // l.Debug("sum", a+b)
	fxSubLine       = 40 // return a - b
)

// runExcludeCallsFixture runs the fixture package with extraArgs and
// returns the parsed report.
func runExcludeCallsFixture(t *testing.T, extraArgs ...string) *report.Report {
	t.Helper()
	outPath := filepath.Join(t.TempDir(), "report.json")
	args := append([]string{"-w", "4", "--cache=off", "-o", outPath}, extraArgs...)
	if err := run(context.Background(), append(args, "./testdata/excludecalls/")); err != nil {
		t.Fatalf("run: %v", err)
	}
	return loadReport(t, outPath)
}

// mutatedLines is the set of source lines still present in the report.
func mutatedLines(r *report.Report) map[int]bool {
	out := make(map[int]bool)
	for _, f := range r.Files {
		for _, m := range f.Mutations {
			out[m.Line] = true
		}
	}
	return out
}

// TestIntegrationExcludeCallsDefaults pins the out-of-the-box behaviour:
// stdlib logging covered with no configuration, and the two families the
// built-in set deliberately leaves alone still mutated.
func TestIntegrationExcludeCallsDefaults(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	r := runExcludeCallsFixture(t)

	// Five mutants live inside Ratio's log.Printf call: the two arithmetic
	// operators, both literal steps on 100, and STATEMENT_REMOVE of the
	// call itself.
	if r.MutantsSuppressed != 5 {
		t.Errorf("MutantsSuppressed=%d, want 5", r.MutantsSuppressed)
	}
	// The fixture carries no directives, so the whole bucket is ours.
	if r.MutantsSuppressedByCalls != r.MutantsSuppressed {
		t.Errorf("MutantsSuppressedByCalls=%d, want all %d suppressions attributed to exclude-calls",
			r.MutantsSuppressedByCalls, r.MutantsSuppressed)
	}
	if r.MutantsTotal != 19 {
		t.Errorf("MutantsTotal=%d, want 19 (suppressed mutants leave the total)", r.MutantsTotal)
	}

	lines := mutatedLines(r)
	if lines[fxLogPrintfLine] {
		t.Errorf("line %d (log.Printf) should be suppressed by the built-in set", fxLogPrintfLine)
	}
	// The identical expression one line down is ordinary code.
	if !lines[fxReturnLine] {
		t.Errorf("line %d (return) must still be mutated", fxReturnLine)
	}
	// log.Fatal* is deliberately absent from the built-in set.
	if !lines[fxFatalfLine] {
		t.Errorf("line %d (log.Fatalf) must still be mutated — deleting it changes behaviour", fxFatalfLine)
	}
	// So are method-shaped globs like *.Debug.
	if !lines[fxDebugLine] {
		t.Errorf("line %d (l.Debug) must still be mutated without an opt-in pattern", fxDebugLine)
	}
}

// TestIntegrationExcludeCallsUserPatternExtends pins that a user list adds
// to the built-in set rather than replacing it.
func TestIntegrationExcludeCallsUserPatternExtends(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	r := runExcludeCallsFixture(t, "--exclude-calls", "*.Debug")

	// Two more from l.Debug: the `+` in its argument and STATEMENT_REMOVE
	// of the call.
	if r.MutantsSuppressed != 7 {
		t.Errorf("MutantsSuppressed=%d, want 7", r.MutantsSuppressed)
	}
	if r.MutantsTotal != 17 {
		t.Errorf("MutantsTotal=%d, want 17", r.MutantsTotal)
	}

	lines := mutatedLines(r)
	if lines[fxDebugLine] {
		t.Errorf("line %d (l.Debug) should be suppressed by the user pattern", fxDebugLine)
	}
	// The user list must not have replaced the built-in one.
	if lines[fxLogPrintfLine] {
		t.Errorf("line %d (log.Printf) should still be suppressed by the built-in set", fxLogPrintfLine)
	}
	if !lines[fxSubLine] {
		t.Errorf("line %d (return) must still be mutated", fxSubLine)
	}
}

// TestIntegrationExcludeCallsDefaultsOff pins the escape hatch: with the
// built-ins off and no user list, every mutant is back in the run.
func TestIntegrationExcludeCallsDefaultsOff(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	r := runExcludeCallsFixture(t, "--exclude-calls-defaults=false")

	if r.MutantsSuppressed != 0 {
		t.Errorf("MutantsSuppressed=%d, want 0 with the built-ins off", r.MutantsSuppressed)
	}
	if r.MutantsTotal != 24 {
		t.Errorf("MutantsTotal=%d, want 24 (every mutant back in the run)", r.MutantsTotal)
	}
	lines := mutatedLines(r)
	if !lines[fxLogPrintfLine] || !lines[fxDebugLine] {
		t.Errorf("both call lines must be mutated with the built-ins off, got lines %v", lines)
	}
}
