package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/szhekpisov/gomutants/internal/config"
	"github.com/szhekpisov/gomutants/internal/report"
)

// runTarget runs one target and returns the parsed report. extra carries
// the flags under test.
func runTarget(t *testing.T, target string, extra ...string) report.Report {
	t.Helper()
	outPath := filepath.Join(t.TempDir(), "report.json")
	args := append([]string{"-w", "4", "--cache=off", "-o", outPath}, extra...)
	if err := run(context.Background(), append(args, target)); err != nil {
		t.Fatalf("run %s %v: %v", target, extra, err)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	var r report.Report
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func verdicts(r report.Report) map[string]string {
	out := map[string]string{}
	for _, f := range r.Files {
		for _, m := range f.Mutations {
			out[f.FileName+" "+m.ID] = m.Status
		}
	}
	return out
}

// TestSchemataVerdictParity is the correctness gate for --schemata.
//
// Mutant schemata changes how a mutant is executed, never which mutants
// exist or what they mean, so a run with the flag has to reach exactly the
// verdict the per-mutant overlay path reaches. Anything the rewriter cannot
// prove safe is supposed to fall back to that path rather than guess, and
// this test is what holds it to that: the two runs are compared mutant by
// mutant, not just on the summary counts, because the interesting failures
// (a mutation that only compiles because the original survives in an else
// branch) move a single NOT VIABLE into a live verdict and leave the totals
// looking plausible.
func TestSchemataVerdictParity(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping schemata parity run in short mode")
	}
	for _, target := range []string{"./testdata/simple/", "./testdata/untested/"} {
		t.Run(target, func(t *testing.T) {
			overlay := runTarget(t, target)
			schema := runTarget(t, target, "--schemata")

			want, got := verdicts(overlay), verdicts(schema)
			if len(want) != len(got) {
				t.Fatalf("mutant count differs: overlay %d, schemata %d", len(want), len(got))
			}
			for id, wantStatus := range want {
				gotStatus, ok := got[id]
				if !ok {
					t.Errorf("%s: missing from the schemata run", id)
					continue
				}
				if gotStatus != wantStatus {
					t.Errorf("%s: overlay says %s, schemata says %s", id, wantStatus, gotStatus)
				}
			}
			if overlay.TestEfficacy != schema.TestEfficacy {
				t.Errorf("efficacy differs: overlay %v, schemata %v", overlay.TestEfficacy, schema.TestEfficacy)
			}
			for _, c := range []struct {
				name      string
				want, got int
			}{
				{"total", overlay.MutantsTotal, schema.MutantsTotal},
				{"killed", overlay.MutantsKilled, schema.MutantsKilled},
				{"lived", overlay.MutantsLived, schema.MutantsLived},
				{"not viable", overlay.MutantsNotViable, schema.MutantsNotViable},
				{"not covered", overlay.MutantsNotCovered, schema.MutantsNotCovered},
			} {
				if c.want != c.got {
					t.Errorf("%s differs: overlay %d, schemata %d", c.name, c.want, c.got)
				}
			}
		})
	}
}

// TestSchemataStandsDownForIncompatibleFlags pins the guards. None of them
// is visible in the report, so they are asserted on the predicate the run
// phase consults.
func TestSchemataStandsDownForIncompatibleFlags(t *testing.T) {
	tests := []struct {
		name    string
		cfg     config.Config
		pending int
		want    bool
	}{
		{"enabled", config.Config{Schemata: true}, 5, true},
		{"off by default", config.Config{}, 5, false},
		{"nothing pending", config.Config{Schemata: true}, 0, false},
		{"test flags stand it down", config.Config{Schemata: true, TestFlags: "-short"}, 5, false},
		{"single mutant stands it down", config.Config{Schemata: true, RunMutantID: "x:y:T#1"}, 5, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := useSchemataFor(&tt.cfg, tt.pending); got != tt.want {
				t.Errorf("useSchemataFor = %v, want %v", got, tt.want)
			}
		})
	}
}
