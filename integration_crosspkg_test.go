package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/szhekpisov/gomutants/internal/report"
)

// writeCrossPkgModule synthesizes a self-contained two-package module:
//
//	calc.Add  — arithmetic target with a weak in-package test (covers the
//	            line, asserts nothing → cannot kill the mutation).
//	app.Total — imports calc; its test asserts a result the mutation breaks
//	            (the only killer of calc's mutant, in a different package).
//
// It must be a real module (not a testdata fixture): the integration closure
// enumerates importers via `go list <module>/...`, and the go tool excludes
// directories named testdata from that wildcard.
func writeCrossPkgModule(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"go.mod": "module crosspkg\n\ngo 1.26\n",
		"calc/calc.go": "package calc\n\n" +
			"func Add(a, b int) int {\n\treturn a + b\n}\n",
		"calc/calc_test.go": "package calc\n\nimport \"testing\"\n\n" +
			"// Covers Add's line but asserts nothing — cannot kill the mutant.\n" +
			"func TestAddRuns(t *testing.T) {\n\t_ = Add(1, 2)\n}\n",
		"app/app.go": "package app\n\nimport \"crosspkg/calc\"\n\n" +
			"func Total(xs ...int) int {\n\tsum := 0\n\tfor _, x := range xs {\n" +
			"\t\tsum = calc.Add(sum, x)\n\t}\n\treturn sum\n}\n",
		"app/app_test.go": "package app\n\nimport \"testing\"\n\n" +
			"// Asserts a concrete result, so calc.Add's `+ → -` mutation fails it.\n" +
			"func TestTotalKillsCalcMutant(t *testing.T) {\n" +
			"\tif got := Total(2, 3); got != 5 {\n\t\tt.Fatalf(\"Total(2,3)=%d, want 5\", got)\n\t}\n}\n",
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestIntegrationCrossPackageRouting is the acceptance gate for --integration:
// a mutant in package calc whose only killer is a test in the importing
// package app must SURVIVE under default per-package routing and be KILLED
// only when --integration widens routing across the package boundary.
//
// Scoped to ./calc/ with --only ARITHMETIC_BASE so exactly one mutant
// (calc.Add's `+`) is under test.
func TestIntegrationCrossPackageRouting(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	dir := writeCrossPkgModule(t)
	t.Chdir(dir)

	runCalc := func(t *testing.T, extraArgs ...string) *report.Report {
		t.Helper()
		out := filepath.Join(t.TempDir(), "report.json")
		args := append([]string{
			"-o", out,
			"--only", "ARITHMETIC_BASE",
			"--cache=off",
		}, extraArgs...)
		args = append(args, "./calc/")
		if err := run(context.Background(), args); err != nil {
			t.Fatalf("run: %v", err)
		}
		return loadReport(t, out)
	}

	// Default per-package routing: calc's weak test covers the line but can't
	// kill the mutant, and the killer in app is never run → LIVED.
	base := runCalc(t)
	if base.MutantsKilled != 0 || base.MutantsLived != 1 {
		t.Fatalf("without --integration: killed=%d lived=%d, want killed=0 lived=1 (the cross-package mutant must survive)",
			base.MutantsKilled, base.MutantsLived)
	}

	// --integration routes the calc mutant to app's covering test, which kills it.
	integ := runCalc(t, "--integration")
	if integ.MutantsKilled != 1 || integ.MutantsLived != 0 {
		t.Fatalf("with --integration: killed=%d lived=%d, want killed=1 lived=0 (the mutant must be killed cross-package)",
			integ.MutantsKilled, integ.MutantsLived)
	}
}

// TestIntegrationKillsWithoutRoutes: under --integration, a per-test map
// that can route nothing still decides which suites run. calc here has no
// tests of its own, and app's tests can't be listed (its TestMain refuses
// -test.list), so no mutant routes anywhere. The map was dropped, and each
// calc mutant then ran only `go test ./calc/`, which has no test files and
// passes, reading LIVED although app's test kills it.
func TestIntegrationKillsWithoutRoutes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	dir := writeCrossPkgModule(t)
	if err := os.Remove(filepath.Join(dir, "calc", "calc_test.go")); err != nil {
		t.Fatal(err)
	}
	mainTest := "package app\n\nimport (\n\t\"flag\"\n\t\"os\"\n\t\"testing\"\n)\n\n" +
		"func TestMain(m *testing.M) {\n\tflag.Parse()\n" +
		"\tif flag.Lookup(\"test.list\").Value.String() != \"\" {\n\t\tos.Exit(3)\n\t}\n" +
		"\tos.Exit(m.Run())\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "app", "main_test.go"), []byte(mainTest), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	out := filepath.Join(t.TempDir(), "report.json")
	if err := run(context.Background(), []string{
		"-o", out,
		"--only", "ARITHMETIC_BASE",
		"--cache=off",
		"--integration",
		"./calc/",
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if r := loadReport(t, out); r.MutantsKilled != 1 || r.MutantsLived != 0 {
		t.Fatalf("killed=%d lived=%d, want killed=1 lived=0 (app's suite must still run)", r.MutantsKilled, r.MutantsLived)
	}
}

// TestIntegrationCoverpkgConflict pins the guard that --integration and
// --coverpkg cannot be combined: integration mode computes -coverpkg itself.
func TestIntegrationCoverpkgConflict(t *testing.T) {
	dir := writeCrossPkgModule(t)
	t.Chdir(dir)

	err := run(context.Background(), []string{
		"--integration",
		"--coverpkg", "./...",
		"./calc/",
	})
	if err == nil {
		t.Fatal("expected an error when --integration and --coverpkg are both set, got nil")
	}
}
