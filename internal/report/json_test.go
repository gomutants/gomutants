package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/szhekpisov/gomutants/internal/mutator"
)

func TestGenerate(t *testing.T) {
	mutants := []mutator.Mutant{
		{ID: 1, Type: mutator.ArithmeticBase, RelFile: "a.go", Line: 10, Col: 5, Status: mutator.StatusKilled},
		{ID: 2, Type: mutator.ArithmeticBase, RelFile: "a.go", Line: 20, Col: 3, Status: mutator.StatusKilled},
		{ID: 3, Type: mutator.ConditionalsBoundary, RelFile: "b.go", Line: 5, Col: 1, Status: mutator.StatusLived},
		{ID: 4, Type: mutator.BranchIf, RelFile: "b.go", Line: 8, Col: 2, Status: mutator.StatusNotCovered},
		{ID: 5, Type: mutator.BranchElse, RelFile: "a.go", Line: 30, Col: 1, Status: mutator.StatusNotViable},
		{ID: 6, Type: mutator.StatementRemove, RelFile: "b.go", Line: 12, Col: 1, Status: mutator.StatusTimedOut},
	}

	r := Generate(mutants, "example.com/mod", 120*time.Second, 0)

	if r.GoModule != "example.com/mod" {
		t.Errorf("GoModule=%q", r.GoModule)
	}
	if r.MutantsTotal != 6 {
		t.Errorf("Total=%d, want 6", r.MutantsTotal)
	}
	if r.MutantsKilled != 2 {
		t.Errorf("Killed=%d, want 2", r.MutantsKilled)
	}
	if r.MutantsLived != 1 {
		t.Errorf("Lived=%d, want 1", r.MutantsLived)
	}
	if r.MutantsNotCovered != 1 {
		t.Errorf("NotCovered=%d, want 1", r.MutantsNotCovered)
	}
	if r.MutantsNotViable != 1 {
		t.Errorf("NotViable=%d, want 1", r.MutantsNotViable)
	}
	if r.MutantsTimedOut != 1 {
		t.Errorf("TimedOut=%d, want 1", r.MutantsTimedOut)
	}

	// Efficacy: 2 killed / (2 killed + 1 lived) = 66.67%
	wantEfficacy := float64(2) / float64(3) * 100
	if r.TestEfficacy != wantEfficacy {
		t.Errorf("TestEfficacy=%f, want %f", r.TestEfficacy, wantEfficacy)
	}

	// Coverage: (6 - 1 not covered) / 6 = 83.33%
	wantCoverage := float64(5) / float64(6) * 100
	if r.MutationsCoverage != wantCoverage {
		t.Errorf("MutationsCoverage=%f, want %f", r.MutationsCoverage, wantCoverage)
	}

	if r.ElapsedTime != 120.0 {
		t.Errorf("ElapsedTime=%f, want 120.0", r.ElapsedTime)
	}

	// Files should preserve insertion order.
	if len(r.Files) != 2 {
		t.Fatalf("Files count=%d, want 2", len(r.Files))
	}
	if r.Files[0].FileName != "a.go" {
		t.Errorf("Files[0]=%q, want a.go", r.Files[0].FileName)
	}
	if r.Files[1].FileName != "b.go" {
		t.Errorf("Files[1]=%q, want b.go", r.Files[1].FileName)
	}
	if len(r.Files[0].Mutations) != 3 {
		t.Errorf("Files[0] mutations=%d, want 3", len(r.Files[0].Mutations))
	}
	if len(r.Files[1].Mutations) != 3 {
		t.Errorf("Files[1] mutations=%d, want 3", len(r.Files[1].Mutations))
	}

	// Mutator statistics should exclude NOT_COVERED.
	if r.MutatorStatistics["arithmetic_base"] != 2 {
		t.Errorf("arithmetic_base stat=%d, want 2", r.MutatorStatistics["arithmetic_base"])
	}
	if r.MutatorStatistics["conditionals_boundary"] != 1 {
		t.Errorf("conditionals_boundary stat=%d, want 1", r.MutatorStatistics["conditionals_boundary"])
	}
	if _, ok := r.MutatorStatistics["branch_if"]; ok {
		t.Error("branch_if should not appear in stats (NOT_COVERED)")
	}
}

func TestGenerateEmpty(t *testing.T) {
	r := Generate(nil, "example.com/mod", 0, 0)
	if r.MutantsTotal != 0 {
		t.Errorf("Total=%d, want 0", r.MutantsTotal)
	}
	if r.TestEfficacy != 0 {
		t.Errorf("Efficacy=%f, want 0 (no tested mutants)", r.TestEfficacy)
	}
	if r.MutationsCoverage != 0 {
		t.Errorf("Coverage=%f, want 0 (no mutants)", r.MutationsCoverage)
	}
}

func TestGenerateCarriesOriginalAndReplacement(t *testing.T) {
	mutants := []mutator.Mutant{
		{
			ID: 1, Type: mutator.ConditionalsNegation, RelFile: "a.go",
			Line: 10, Col: 5, Original: "==", Replacement: "!=",
			Status: mutator.StatusLived,
		},
	}
	r := Generate(mutants, "mod", time.Second, 0)
	if len(r.Files) != 1 || len(r.Files[0].Mutations) != 1 {
		t.Fatalf("expected 1 mutation, got files=%v", r.Files)
	}
	got := r.Files[0].Mutations[0]
	if got.Original != "==" || got.Replacement != "!=" {
		t.Errorf("Original/Replacement = %q/%q, want ==/!=", got.Original, got.Replacement)
	}

	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var loaded Report
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	got2 := loaded.Files[0].Mutations[0]
	if got2.Original != "==" || got2.Replacement != "!=" {
		t.Errorf("after round-trip: Original/Replacement = %q/%q", got2.Original, got2.Replacement)
	}
}

func TestGenerateOmitsEmptyOriginalAndReplacement(t *testing.T) {
	mutants := []mutator.Mutant{
		{ID: 1, Type: mutator.ArithmeticBase, RelFile: "a.go", Line: 1, Col: 1, Status: mutator.StatusKilled},
	}
	r := Generate(mutants, "mod", time.Second, 0)
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), `"original"`) || strings.Contains(string(data), `"replacement"`) {
		t.Errorf("expected omitempty for empty original/replacement, got: %s", data)
	}
}

func TestGenerateAllKilled(t *testing.T) {
	mutants := []mutator.Mutant{
		{ID: 1, Type: mutator.ArithmeticBase, RelFile: "a.go", Line: 1, Col: 1, Status: mutator.StatusKilled},
		{ID: 2, Type: mutator.ArithmeticBase, RelFile: "a.go", Line: 2, Col: 1, Status: mutator.StatusKilled},
	}
	r := Generate(mutants, "mod", time.Second, 0)
	if r.TestEfficacy != 100 {
		t.Errorf("Efficacy=%f, want 100", r.TestEfficacy)
	}
	if r.MutationsCoverage != 100 {
		t.Errorf("Coverage=%f, want 100", r.MutationsCoverage)
	}
}

func TestGenerateCountsMutantsCached(t *testing.T) {
	mutants := []mutator.Mutant{
		{ID: 1, Type: mutator.ArithmeticBase, RelFile: "a.go", Line: 1, Col: 1, Status: mutator.StatusKilled, FromCache: true},
		{ID: 2, Type: mutator.ArithmeticBase, RelFile: "a.go", Line: 2, Col: 1, Status: mutator.StatusLived, FromCache: true},
		{ID: 3, Type: mutator.ArithmeticBase, RelFile: "a.go", Line: 3, Col: 1, Status: mutator.StatusKilled},
	}
	r := Generate(mutants, "mod", time.Second, 0)
	if r.MutantsCached != 2 {
		t.Errorf("MutantsCached=%d, want 2", r.MutantsCached)
	}
	if r.MutantsKilled != 2 {
		t.Errorf("MutantsKilled=%d, want 2 (FromCache must not change status counts)", r.MutantsKilled)
	}
}

// TestGenerateCountsRechecks: re-checked mutants are counted whatever
// their status, and those of them killed separately; neither count
// changes the status counts.
func TestGenerateCountsRechecks(t *testing.T) {
	mutants := []mutator.Mutant{
		{ID: 1, Type: mutator.ArithmeticBase, RelFile: "a.go", Line: 1, Col: 1, Status: mutator.StatusKilled, Rechecked: true},
		{ID: 2, Type: mutator.ArithmeticBase, RelFile: "a.go", Line: 2, Col: 1, Status: mutator.StatusLived, Rechecked: true},
		{ID: 3, Type: mutator.ArithmeticBase, RelFile: "a.go", Line: 3, Col: 1, Status: mutator.StatusTimedOut, Rechecked: true},
		{ID: 4, Type: mutator.ArithmeticBase, RelFile: "a.go", Line: 4, Col: 1, Status: mutator.StatusKilled},
	}
	r := Generate(mutants, "mod", time.Second, 0)
	if r.MutantsRechecked != 3 || r.MutantsRecheckKilled != 1 {
		t.Errorf("MutantsRechecked=%d MutantsRecheckKilled=%d, want 3 and 1", r.MutantsRechecked, r.MutantsRecheckKilled)
	}
	if r.MutantsKilled != 2 || r.MutantsLived != 1 {
		t.Errorf("MutantsKilled=%d MutantsLived=%d, want 2 and 1", r.MutantsKilled, r.MutantsLived)
	}
	data, err := json.Marshal(Generate(mutants[3:], "mod", time.Second, 0))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), `"mutants_rechecked"`) || strings.Contains(string(data), `"mutants_recheck_killed"`) {
		t.Errorf("re-check counts should be omitted when 0, got: %s", data)
	}
}

func TestGenerateRecordsMutantsSuppressed(t *testing.T) {
	mutants := []mutator.Mutant{
		{ID: 1, Type: mutator.ArithmeticBase, RelFile: "a.go", Line: 1, Col: 1, Status: mutator.StatusKilled},
	}
	r := Generate(mutants, "mod", time.Second, 4)
	if r.MutantsSuppressed != 4 {
		t.Errorf("MutantsSuppressed=%d, want 4", r.MutantsSuppressed)
	}
	if r.MutantsTotal != 1 {
		t.Errorf("MutantsTotal=%d, want 1 (suppressed must not roll into total)", r.MutantsTotal)
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(data), `"mutants_suppressed":4`) {
		t.Errorf("expected mutants_suppressed:4 in JSON, got: %s", data)
	}
}

func TestGenerateOmitsMutantsSuppressedWhenZero(t *testing.T) {
	mutants := []mutator.Mutant{
		{ID: 1, Type: mutator.ArithmeticBase, RelFile: "a.go", Line: 1, Col: 1, Status: mutator.StatusKilled},
	}
	r := Generate(mutants, "mod", time.Second, 0)
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), `"mutants_suppressed"`) {
		t.Errorf("mutants_suppressed should be omitted when 0, got: %s", data)
	}
}

func TestGenerateOmitsMutantsTimedOutWhenZero(t *testing.T) {
	mutants := []mutator.Mutant{
		{ID: 1, Type: mutator.ArithmeticBase, RelFile: "a.go", Line: 1, Col: 1, Status: mutator.StatusKilled},
	}
	r := Generate(mutants, "mod", time.Second, 0)
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), `"mutants_timed_out"`) {
		t.Errorf("mutants_timed_out should be omitted when 0, got: %s", data)
	}
}

func TestGenerateEmitsMutantsTimedOutWhenNonZero(t *testing.T) {
	mutants := []mutator.Mutant{
		{ID: 1, Type: mutator.ArithmeticBase, RelFile: "a.go", Line: 1, Col: 1, Status: mutator.StatusTimedOut},
	}
	r := Generate(mutants, "mod", time.Second, 0)
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(data), `"mutants_timed_out":1`) {
		t.Errorf("expected mutants_timed_out:1 in JSON, got: %s", data)
	}
}

func TestGenerateCountsAndSerializesMutantsInfraError(t *testing.T) {
	r := Generate([]mutator.Mutant{
		{Type: mutator.ArithmeticBase, Status: mutator.StatusKilled, RelFile: "x.go"},
		{Type: mutator.ArithmeticBase, Status: mutator.StatusLived, RelFile: "x.go"},
		{Type: mutator.ArithmeticBase, Status: mutator.StatusNotCovered, RelFile: "x.go"},
		{Type: mutator.ArithmeticBase, Status: mutator.StatusInfraError, RelFile: "x.go"},
	}, "m", 0, 0)

	if r.MutantsInfraError != 1 {
		t.Errorf("MutantsInfraError=%d, want 1", r.MutantsInfraError)
	}
	if r.MutantsTotal != 4 {
		t.Errorf("MutantsTotal=%d, want 4", r.MutantsTotal)
	}
	if r.TestEfficacy != 50 {
		t.Errorf("TestEfficacy=%v, want 50 (infra error excluded)", r.TestEfficacy)
	}
	if r.MutationsCoverage != 75 {
		t.Errorf("MutationsCoverage=%v, want 75 (infra error included)", r.MutationsCoverage)
	}
	if got := r.Files[0].Mutations[3].Status; got != "INFRA ERROR" {
		t.Errorf("per-mutant status=%q, want INFRA ERROR", got)
	}

	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(data), `"mutants_infra_error":1`) {
		t.Errorf("expected mutants_infra_error:1 in JSON, got: %s", data)
	}
}

func TestGenerateOmitsMutantsInfraErrorWhenZero(t *testing.T) {
	r := Generate([]mutator.Mutant{
		{Type: mutator.ArithmeticBase, Status: mutator.StatusKilled, RelFile: "x.go"},
	}, "m", 0, 0)
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), `"mutants_infra_error"`) {
		t.Errorf("mutants_infra_error should be omitted when 0, got: %s", data)
	}
}

func TestGenerateOmitsMutantsCachedWhenZero(t *testing.T) {
	mutants := []mutator.Mutant{
		{ID: 1, Type: mutator.ArithmeticBase, RelFile: "a.go", Line: 1, Col: 1, Status: mutator.StatusKilled},
	}
	r := Generate(mutants, "mod", time.Second, 0)
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), `"mutants_cached"`) {
		t.Errorf("mutants_cached should be omitted when 0, got: %s", data)
	}
}

func TestWriteJSON(t *testing.T) {
	r := &Report{
		GoModule:          "example.com/mod",
		MutantsTotal:      10,
		MutantsKilled:     8,
		TestEfficacy:      100,
		MutatorStatistics: map[string]int{"arithmetic_base": 5},
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "report.json")

	if err := WriteJSON(r, path); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	var loaded Report
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if loaded.GoModule != "example.com/mod" {
		t.Errorf("GoModule=%q", loaded.GoModule)
	}
	if loaded.MutantsTotal != 10 {
		t.Errorf("Total=%d", loaded.MutantsTotal)
	}
}

// TestGenerateCountsMutantsEquivalent pins the EQUIVALENT bucket: it is
// counted, stays in MutantsTotal, and is excluded from the efficacy
// denominator (an equivalent mutant is neither killed nor a surviving gap).
func TestGenerateCountsMutantsEquivalent(t *testing.T) {
	r := Generate([]mutator.Mutant{
		{Type: mutator.ArithmeticBase, Status: mutator.StatusKilled, RelFile: "x.go"},
		{Type: mutator.ArithmeticBase, Status: mutator.StatusEquivalent, RelFile: "x.go"},
	}, "m", 0, 0)
	if r.MutantsEquivalent != 1 {
		t.Errorf("MutantsEquivalent=%d, want 1", r.MutantsEquivalent)
	}
	if r.MutantsTotal != 2 {
		t.Errorf("MutantsTotal=%d, want 2 (equivalent stays in total)", r.MutantsTotal)
	}
	if r.MutantsLived != 0 {
		t.Errorf("MutantsLived=%d, want 0 (equivalent is not lived)", r.MutantsLived)
	}
	if r.TestEfficacy != 100 {
		t.Errorf("TestEfficacy=%v, want 100 (1 killed / (1 killed + 0 lived))", r.TestEfficacy)
	}
}

// TestReportMutantsSuppressedByCallsSerialization pins the breakdown
// field: present when the caller sets it, omitted otherwise so a run
// without --exclude-calls suppressions keeps producing the exact JSON
// gremlins consumers already parse.
func TestReportMutantsSuppressedByCallsSerialization(t *testing.T) {
	mutants := []mutator.Mutant{
		{ID: 1, Type: mutator.ArithmeticBase, RelFile: "a.go", Line: 1, Col: 1, Status: mutator.StatusKilled},
	}

	r := Generate(mutants, "mod", time.Second, 5)
	r.MutantsSuppressedByCalls = 2
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(data), `"mutants_suppressed_by_calls":2`) {
		t.Errorf("expected mutants_suppressed_by_calls:2 in JSON, got: %s", data)
	}
	if !strings.Contains(string(data), `"mutants_suppressed":5`) {
		t.Errorf("the breakdown must not replace the shared bucket, got: %s", data)
	}

	plain := Generate(mutants, "mod", time.Second, 5)
	data, err = json.Marshal(plain)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "mutants_suppressed_by_calls") {
		t.Errorf("mutants_suppressed_by_calls should be omitted when 0, got: %s", data)
	}
}

func TestGenerateCarriesStableID(t *testing.T) {
	mutants := []mutator.Mutant{
		{
			ID: 1, StableID: "a.go:Add:ARITHMETIC_BASE#1", Type: mutator.ArithmeticBase,
			RelFile: "a.go", Line: 10, Col: 5, Status: mutator.StatusLived,
		},
		{
			ID: 2, StableID: "a.go:Add:ARITHMETIC_BASE#2", Type: mutator.ArithmeticBase,
			RelFile: "a.go", Line: 12, Col: 5, Status: mutator.StatusKilled,
		},
	}

	r := Generate(mutants, "mod", time.Second, 0)
	if len(r.Files) != 1 || len(r.Files[0].Mutations) != 2 {
		t.Fatalf("expected 1 file with 2 mutations, got files=%v", r.Files)
	}
	// The stable ID must ride along per entry, not be derived from the
	// sequential ID or shared across the file.
	for i, want := range []string{"a.go:Add:ARITHMETIC_BASE#1", "a.go:Add:ARITHMETIC_BASE#2"} {
		if got := r.Files[0].Mutations[i].ID; got != want {
			t.Errorf("Mutations[%d].ID=%q, want %q", i, got, want)
		}
	}

	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(data), `"id":"a.go:Add:ARITHMETIC_BASE#1"`) {
		t.Errorf("expected the id field under the `id` JSON key, got: %s", data)
	}

	var loaded Report
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if loaded.Files[0].Mutations[1].ID != "a.go:Add:ARITHMETIC_BASE#2" {
		t.Errorf("after round-trip: ID=%q", loaded.Files[0].Mutations[1].ID)
	}
}

func TestGenerateEmitsIDEvenWhenEmpty(t *testing.T) {
	// `id` carries no omitempty: a consumer indexing on it should see the
	// key on every entry rather than have it vanish.
	mutants := []mutator.Mutant{
		{ID: 1, Type: mutator.ArithmeticBase, RelFile: "a.go", Line: 1, Col: 1, Status: mutator.StatusKilled},
	}
	data, err := json.Marshal(Generate(mutants, "mod", time.Second, 0))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(data), `"id":""`) {
		t.Errorf("expected an empty id key to be present, got: %s", data)
	}
}
