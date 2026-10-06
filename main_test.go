package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/szhekpisov/gomutants/internal/cache"
	"github.com/szhekpisov/gomutants/internal/config"
	"github.com/szhekpisov/gomutants/internal/coverage"
	"github.com/szhekpisov/gomutants/internal/discover"
	"github.com/szhekpisov/gomutants/internal/mutator"
	"github.com/szhekpisov/gomutants/internal/report"
	"github.com/szhekpisov/gomutants/internal/runner"
)

// captureOutput swaps the package-level stdout writer with a bytes.Buffer
// and returns the captured text plus the function's error.
func captureOutput(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	orig := stdout
	stdout = &buf
	defer func() { stdout = orig }()
	err := fn()
	return buf.String(), err
}

// captureStderr swaps the package-level stderr writer for the duration of
// fn so tests can assert against warnings/notes (e.g. the "no testable
// mutants discovered; --threshold-efficacy not evaluated" message).
func captureStderr(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	orig := stderr
	stderr = &buf
	defer func() { stderr = orig }()
	err := fn()
	return buf.String(), err
}

func requireExitCode(t *testing.T, err error, want int) {
	t.Helper()
	if err == nil {
		t.Fatalf("exit code = 0, want %d", want)
	}
	if got := exitCodeForError(err); got != want {
		t.Fatalf("exit code = %d, want %d (error: %v)", got, want, err)
	}
}

func TestRunVersion(t *testing.T) {
	out, err := captureOutput(t, func() error {
		return run(context.Background(), []string{"--version"})
	})
	if err != nil {
		t.Fatalf("run --version: %v", err)
	}
	// Under `go test`, version falls back to "dev" (Main.Version is
	// "(devel)"); commit/buildDate may be either the sentinel defaults
	// or vcs.revision/vcs.time depending on how the test was built, so
	// only assert structure, not exact values.
	if !strings.HasPrefix(out, "gomutants vdev (commit: ") {
		t.Errorf("version output prefix: got %q", out)
	}
	if !strings.Contains(out, ", built: ") || !strings.HasSuffix(out, ")\n") {
		t.Errorf("version output structure: got %q", out)
	}
}

func TestRunUnleash(t *testing.T) {
	// "unleash" should be stripped — then --version runs normally.
	out, err := captureOutput(t, func() error {
		return run(context.Background(), []string{"unleash", "--version"})
	})
	if err != nil {
		t.Fatalf("run unleash --version: %v", err)
	}
	if !strings.Contains(out, "gomutants vdev") {
		t.Errorf("unleash: expected version output, got %q", out)
	}
}

func TestRunListMutators(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "broken.yml"), []byte("only: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(orig); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	}()

	var out string
	stderrText, err := captureStderr(t, func() error {
		var runErr error
		out, runErr = captureOutput(t, func() error {
			return run(context.Background(), []string{
				"--config", "broken.yml",
				"--list-mutators",
				"not-a-package",
			})
		})
		return runErr
	})
	if err != nil {
		t.Fatalf("run --list-mutators: %v", err)
	}
	if stderrText != "" {
		t.Errorf("--list-mutators wrote stderr: %q", stderrText)
	}

	catalog := mutator.NewRegistry().Catalog()
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if got, want := len(lines), len(catalog); got != want {
		t.Fatalf("got %d catalog lines, want %d:\n%s", got, want, out)
	}
	for i, entry := range catalog {
		line := lines[i]
		if !strings.HasPrefix(line, string(entry.Type)+" ") {
			t.Errorf("line %d = %q, want type %q first", i, line, entry.Type)
			continue
		}
		descriptionAt := strings.Index(line, entry.Description)
		exampleAt := strings.Index(line, entry.Example)
		if descriptionAt < 0 || exampleAt < descriptionAt+len(entry.Description) {
			t.Errorf("line %d = %q, want description %q followed by example %q",
				i, line, entry.Description, entry.Example)
		}
	}

	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Name() != "broken.yml" {
		t.Errorf("--list-mutators created project artifacts: %v", files)
	}
}

func TestRunListMutatorsWithUnleashPrefix(t *testing.T) {
	plain, err := captureOutput(t, func() error {
		return run(context.Background(), []string{"--list-mutators"})
	})
	if err != nil {
		t.Fatalf("run --list-mutators: %v", err)
	}
	prefixed, err := captureOutput(t, func() error {
		return run(context.Background(), []string{"unleash", "--list-mutators"})
	})
	if err != nil {
		t.Fatalf("run unleash --list-mutators: %v", err)
	}
	if prefixed != plain {
		t.Errorf("unleash catalog differs\nplain:\n%s\nprefixed:\n%s", plain, prefixed)
	}
}

func TestRunInvalidFlag(t *testing.T) {
	_, err := captureStderr(t, func() error {
		return run(context.Background(), []string{"--invalid-flag"})
	})
	requireExitCode(t, err, exitCodeUsageError)
}

func TestRunInvalidFlagValuesExitUsage(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"integer", []string{"-w", "auto"}},
		{"boolean", []string{"--exclude-calls-defaults=maybe"}},
		{"duration", []string{"--checkpoint-interval=eventually"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := captureStderr(t, func() error {
				return run(context.Background(), tc.args)
			})
			requireExitCode(t, err, exitCodeUsageError)
		})
	}
}

func TestRunHelpExitsSuccessfully(t *testing.T) {
	output, err := captureStderr(t, func() error {
		return run(context.Background(), []string{"--help"})
	})
	if err != nil {
		t.Fatalf("run --help: %v", err)
	}
	if !strings.Contains(output, "Usage of gomutants:") {
		t.Errorf("help output missing usage header: %q", output)
	}
	if !strings.Contains(output, "-list-mutators") {
		t.Errorf("help output missing --list-mutators: %q", output)
	}
}

func TestRunNegativeTestCPU(t *testing.T) {
	err := run(context.Background(), []string{"--test-cpu", "-1"})
	requireExitCode(t, err, exitCodeUsageError)
	if !strings.Contains(err.Error(), "test-cpu") {
		t.Errorf("error should mention test-cpu, got: %v", err)
	}
}

func TestRunSemanticConfigErrorsExitUsage(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"conflicting verbosity", []string{"--quiet", "--verbose"}},
		{"unsupported annotations", []string{"--annotations=gitlab"}},
		{"integration coverpkg conflict", []string{"--integration", "--coverpkg=./..."}},
		{"managed test flag", []string{"--test-flags=-overlay=x"}},
		{"invalid exclude-files pattern", []string{"--exclude-files=["}},
		{"invalid exclude-calls pattern", []string{"--exclude-calls=*"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := run(context.Background(), tc.args)
			requireExitCode(t, err, exitCodeUsageError)
		})
	}
}

func runCLIExitCodeHelper(t *testing.T, helperEnv string) bool {
	t.Helper()
	if os.Getenv(helperEnv) != "1" {
		return false
	}

	separator := slices.Index(os.Args, "--")
	if separator < 0 {
		t.Fatal("helper invocation missing -- separator")
	}
	os.Args = append([]string{"gomutants"}, os.Args[separator+1:]...)
	main()
	return true
}

func TestCLIExitCodes(t *testing.T) {
	const helperEnv = "GOMUTANTS_TEST_MAIN_HELPER"
	if runCLIExitCodeHelper(t, helperEnv) {
		return
	}

	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "go.mod"), []byte("module testmod\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		args []string
		want int
	}{
		{"help", []string{"--help"}, 0},
		{"unknown flag", []string{"--totally-bad-flag", "./pkg"}, exitCodeUsageError},
		{"invalid flag value", []string{"-w", "auto", "./pkg"}, exitCodeUsageError},
		{"unbuildable target", []string{"./pkg-that-does-not-exist"}, exitCodeRuntimeError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"-test.run=^TestCLIExitCodes$", "--"}, tc.args...)
			cmd := exec.Command(os.Args[0], args...)
			cmd.Dir = projectDir
			cmd.Env = append(os.Environ(), helperEnv+"=1")
			output, err := cmd.CombinedOutput()
			got := 0
			if err != nil {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) {
					t.Fatalf("running helper: %v", err)
				}
				got = exitErr.ExitCode()
			}
			if got != tc.want {
				t.Fatalf("exit code = %d, want %d\noutput:\n%s", got, tc.want, output)
			}
		})
	}
}

func TestReadModuleName(t *testing.T) {
	dir := t.TempDir()
	goMod := `module github.com/example/project

go 1.26
`
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatal(err)
	}

	name, err := readModuleName(dir)
	if err != nil {
		t.Fatalf("readModuleName: %v", err)
	}
	if name != "github.com/example/project" {
		t.Errorf("module name=%q, want %q", name, "github.com/example/project")
	}
}

func TestReadModuleNameMissing(t *testing.T) {
	_, err := readModuleName("/nonexistent")
	if err == nil {
		t.Fatal("expected error for missing go.mod")
	}
}

func TestReadModuleNameNoModule(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("go 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := readModuleName(dir)
	if err == nil {
		t.Fatal("expected error for go.mod without module line")
	}
}

// TestRunAllLongFlags exercises each long-form flag so removing any
// fs.XxxVar registration breaks the parse. Uses --dry-run to avoid the
// slow mutation phase.
func TestRunAllLongFlags(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":      "module testmod\n\ngo 1.26\n",
		"add.go":      "package testmod\n\nfunc Add(a, b int) int { return a + b }\n",
		"add_test.go": "package testmod\nimport \"testing\"\nfunc TestAdd(t *testing.T) { if Add(1,2) != 3 { t.Fatal(\"wrong\") } }\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	outPath := filepath.Join(dir, "report.json")
	args := []string{
		"--workers", "1",
		"--timeout-coefficient", "5",
		"--coverpkg", "testmod",
		"--output", outPath,
		"--config", ".gomutants.yml",
		"--disable", "BRANCH_IF",
		"--checkpoint-interval", "5s",
		"--dry-run",
		"--verbose",
		"testmod",
	}
	out, err := captureOutput(t, func() error {
		return run(context.Background(), args)
	})
	if err != nil {
		t.Fatalf("run with all long flags: %v", err)
	}
	// Dry-run prints "[PENDING]" or "[NOT COVERED]" markers per mutant.
	if !strings.Contains(out, "gomutants vdev") {
		t.Errorf("missing header: %q", out)
	}
	if !strings.Contains(out, "Target: [testmod]") {
		t.Errorf("missing target line: %q", out)
	}
	if !strings.Contains(out, "Workers: 1") {
		t.Errorf("missing workers line: %q", out)
	}
	// Phase banners AND their paired PhaseDone outputs, as joined strings.
	// Checking only the prefix lets STATEMENT_REMOVE on PhaseDone(...) calls
	// survive (the overall output still has "done (" strings from other
	// phases). We check the full "Phase... PhaseDone" pair.
	joined := []string{
		"Resolving packages... done (1 packages)",
		"Collecting coverage... done (", // "done (Ns)" — duration varies
		"Measuring baseline... done (",  // "done (Ns, timeout: Ns)"
		"Discovering mutants... ",       // "N found (N not covered, N to test)"
	}
	for _, s := range joined {
		if !strings.Contains(out, s) {
			t.Errorf("missing output %q: full output:\n%s", s, out)
		}
	}
}

// TestRunWarnsOnUnknownMutatorName asserts a typo in --only / --disable
// surfaces as a stderr warning instead of silently filtering nothing
// (--disable) or everything (--only). The "ignored" wording is part of
// the contract — the warning has to communicate that the run continues
// with the remaining valid names.
func TestRunWarnsOnUnknownMutatorName(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":      "module testmod\n\ngo 1.26\n",
		"add.go":      "package testmod\n\nfunc Add(a, b int) int { return a + b }\n",
		"add_test.go": "package testmod\nimport \"testing\"\nfunc TestAdd(t *testing.T) { if Add(1,2) != 3 { t.Fatal(\"wrong\") } }\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	stderrText, err := captureStderr(t, func() error {
		return run(context.Background(), []string{
			"--only", "ARITHMETIC_BASE,ARTIHMETIC_BASE",
			"--disable", "BOGUS_MUTATOR",
			"--dry-run", "testmod",
		})
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(stderrText, `unknown mutator "ARTIHMETIC_BASE" in --only`) {
		t.Errorf("expected --only typo warning, got: %q", stderrText)
	}
	if !strings.Contains(stderrText, `unknown mutator "BOGUS_MUTATOR" in --disable`) {
		t.Errorf("expected --disable typo warning, got: %q", stderrText)
	}
	const hint = `gomutants: run "gomutants --list-mutators" to see valid mutator names`
	if got := strings.Count(stderrText, hint); got != 1 {
		t.Errorf("expected one catalog hint, got %d in: %q", got, stderrText)
	}
}

// TestRunOnlyFlag exercises --only (separate test because it disables all
// other mutators).
func TestRunOnlyFlag(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":      "module testmod\n\ngo 1.26\n",
		"add.go":      "package testmod\n\nfunc Add(a, b int) int { return a + b }\n",
		"add_test.go": "package testmod\nimport \"testing\"\nfunc TestAdd(t *testing.T) { if Add(1,2) != 3 { t.Fatal(\"wrong\") } }\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	_, err := captureOutput(t, func() error {
		return run(context.Background(), []string{"--only", "ARITHMETIC_BASE", "--dry-run", "testmod"})
	})
	if err != nil {
		t.Fatalf("run --only: %v", err)
	}
}

// TestRunShortFlags exercises -w, -o, -v short-form flags, killing
// STATEMENT_REMOVE mutants on those fs.XxxVar shorthand registrations.
func TestRunShortFlags(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":      "module testmod\n\ngo 1.26\n",
		"add.go":      "package testmod\n\nfunc Add(a, b int) int { return a + b }\n",
		"add_test.go": "package testmod\nimport \"testing\"\nfunc TestAdd(t *testing.T) { if Add(1,2) != 3 { t.Fatal(\"wrong\") } }\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	outPath := filepath.Join(dir, "r.json")
	out, err := captureOutput(t, func() error {
		return run(context.Background(), []string{
			"-w", "1", "-o", outPath, "-v", "--dry-run", "testmod",
		})
	})
	if err != nil {
		t.Fatalf("run with short flags: %v", err)
	}
	if !strings.Contains(out, "Workers: 1") {
		t.Errorf("short -w didn't set workers: %q", out)
	}
	// Verbose mode triggers per-mutant markers like "[PENDING]" or "[NOT COVERED]".
	if !strings.Contains(out, "[") || !strings.Contains(out, "]") {
		t.Errorf("short -v verbose output missing brackets: %q", out)
	}
}

// TestRunPendingCountExact asserts the exact "N found (N not covered, N to test)"
// output for a known-shape testmod. Kills INCREMENT_DECREMENT on pendingCount++
// and notCoveredCount++, STATEMENT_REMOVE on those assignments, and
// CONDITIONALS_NEGATION on the status-comparison branches.
func TestRunPendingCountExact(t *testing.T) {
	dir := t.TempDir()
	// Two functions: Add is covered by TestAdd; Unused is not covered.
	files := map[string]string{
		"go.mod":      "module testmod\n\ngo 1.26\n",
		"lib.go":      "package testmod\n\nfunc Add(a, b int) int { return a + b }\n\nfunc Unused(x, y int) int { return x + y }\n",
		"lib_test.go": "package testmod\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) { if Add(1,2) != 3 { t.Fatal(\"wrong\") } }\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	out, err := captureOutput(t, func() error {
		return run(context.Background(), []string{
			"--only", "ARITHMETIC_BASE",
			"--dry-run",
			"testmod",
		})
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// Expected: 2 ARITHMETIC_BASE mutants total (Add's +, Unused's +).
	// Add is covered -> 1 to test. Unused is not covered -> 1 not covered.
	want := "2 found (1 not covered, 1 to test)"
	if !strings.Contains(out, want) {
		t.Errorf("expected counts %q in output, got: %q", want, out)
	}
}

// TestRunConfigLoadError kills BRANCH_IF on the config.Load error check.
// config.Load returns the *default* Config alongside its error, so the
// elided body lets cfg.ApplyFlags work fine and downstream calls run
// normally — the only signal that the error wasn't honored is that the
// returned err originates from a later step (resolve/coverage) rather
// than from config parsing. We assert the error message wraps config
// parsing to lock that distinction in.
func TestRunConfigLoadError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module testmod\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Config with invalid YAML.
	cfgPath := filepath.Join(dir, "bad.yml")
	if err := os.WriteFile(cfgPath, []byte("not: valid: yaml: at: all:\n  : : :"), 0o644); err != nil {
		t.Fatal(err)
	}
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)
	err := run(context.Background(), []string{"--config", cfgPath, "--dry-run", "testmod"})
	requireExitCode(t, err, exitCodeUsageError)
	if !strings.Contains(err.Error(), "config") && !strings.Contains(err.Error(), "yaml") {
		t.Errorf("error should originate from config.Load, got: %v — BRANCH_IF on the err-return body lets config errors fall through to a later step", err)
	}
}

// TestRunResolvePackagesError kills BRANCH_IF on the discover.ResolvePackages
// error check by pointing at a package pattern go-list can't resolve.
func TestRunResolvePackagesError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module testmod\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)
	err := run(context.Background(), []string{"--dry-run", "completely/nonexistent/package/xyz"})
	requireExitCode(t, err, exitCodeRuntimeError)
}

// TestRunCoverageError kills BRANCH_IF on the runner.RunCoverage error check
// by providing a package with a failing test.
func TestRunCoverageError(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":      "module testmod\n\ngo 1.26\n",
		"add.go":      "package testmod\n\nfunc Add(a, b int) int { return a + b }\n",
		"add_test.go": "package testmod\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) { t.Fatal(\"always fail\") }\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)
	// Non-dry-run so coverage collection actually runs.
	err := run(context.Background(), []string{
		"--only", "ARITHMETIC_BASE",
		"-w", "1",
		"-o", filepath.Join(dir, "report.json"),
		"testmod",
	})
	requireExitCode(t, err, exitCodeRuntimeError)
}

func TestRunDryRun(t *testing.T) {
	// Create a minimal Go project.
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":      "module testmod\n\ngo 1.26\n",
		"add.go":      "package testmod\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n",
		"add_test.go": "package testmod\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"wrong\")\n\t}\n}\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Change to the temp dir so go list works.
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	err := run(context.Background(), []string{"--dry-run", "--only", "ARITHMETIC_BASE", "testmod"})
	if err != nil {
		t.Fatalf("run --dry-run: %v", err)
	}
}

func TestRunFullPipeline(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess-spawning test in short mode (self-mutation guard)")
	}
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":      "module testmod\n\ngo 1.26\n",
		"add.go":      "package testmod\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n",
		"add_test.go": "package testmod\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"wrong\")\n\t}\n}\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	outPath := filepath.Join(dir, "report.json")
	out, err := captureOutput(t, func() error {
		return run(context.Background(), []string{
			"--only", "ARITHMETIC_BASE",
			"-w", "1",
			"-o", outPath,
			"testmod",
		})
	})
	if err != nil {
		t.Fatalf("run full pipeline: %v", err)
	}

	// Verify every phase-banner line and the final report line.
	mustContain := []string{
		"gomutants vdev",
		"Target: [testmod]",
		"Workers: 1 | Mutations: 1 types enabled",
		"Resolving packages...",
		"done (1 packages)",
		"Collecting coverage...",
		"Measuring baseline...",
		"Discovering mutants...",
		"Building per-test coverage map...",
		"Killed:",
		"Lived:",
		"Efficacy:",
		"Report: " + outPath,
	}
	for _, s := range mustContain {
		if !strings.Contains(out, s) {
			t.Errorf("output missing %q; full output:\n%s", s, out)
		}
	}

	// Verify report was written.
	if _, err := os.Stat(outPath); err != nil {
		t.Fatalf("report not written: %v", err)
	}
}

// TestRunQuietConflictsWithVerbose pins the early-return on the
// --quiet --verbose conflict. The check runs before any work, so the
// error must come back without any setup or stdout side effects.
func TestRunQuietConflictsWithVerbose(t *testing.T) {
	out, err := captureOutput(t, func() error {
		return run(context.Background(), []string{"--quiet", "--verbose"})
	})
	if err == nil {
		t.Fatal("expected --quiet --verbose to error")
	}
	requireExitCode(t, err, exitCodeUsageError)
	if !strings.Contains(err.Error(), "--quiet and --verbose") {
		t.Errorf("error should call out the conflicting flags, got: %v", err)
	}
	if out != "" {
		t.Errorf("conflict check must short-circuit before any stdout, got %q", out)
	}
}

// The --test-flags guard is pinned by three tests rather than one, split
// along the three things it has to get right: reject every spelling that
// can override a managed flag, accept everything else (including raw
// test-binary fields after -args), and name the flag the way the user wrote
// it. They share the flagCases shape below.
type flagCases = []struct {
	name  string
	flags []string
}

// TestCheckTestFlagsRejects covers the guard's reason for existing: a
// managed flag must be caught in every spelling that reaches the go command
// or its generated test flags, because each one fails silently rather than
// loudly.
func TestCheckTestFlagsRejects(t *testing.T) {
	rejected := flagCases{
		{"single dash with value", []string{"-overlay=/tmp/x.json"}},
		{"double dash", []string{"--overlay=/tmp/x.json"}},
		{"space-separated value", []string{"-overlay", "/tmp/x.json"}},
		{"bare flag", []string{"-c"}},
		{"not first", []string{"-short", "-run=TestFoo"}},
		{"coverprofile", []string{"-coverprofile=/tmp/c.out"}},
		{"coverpkg", []string{"-coverpkg=./..."}},
		{"binary output", []string{"-o=/tmp/bin"}},
		{"exec wrapper", []string{"-exec=wine"}},
		// A non-flag field *before* the managed one, so the scan has to
		// carry on past it. INVERT_LOOP_CTRL (an early `break` or
		// `return`) would abandon the loop at "all=-N" and let -overlay
		// through; only an ordering like this catches that, since the
		// accept-side `{"-gcflags", "c"}` case ends on the non-flag field.
		{"managed flag after a non-flag value", []string{"-gcflags", "all=-N", "-overlay=x"}},
		// -timeout is enforced twice: on the argv and by the context
		// deadline in Worker.Test. A longer user value is capped by the
		// context and still lands as TIMED_OUT, so honoring it on argv
		// alone would be a lie.
		{"timeout", []string{"-timeout=30s"}},
		// `go test` forwards a `-test.`-prefixed flag straight to the test
		// binary, where it beats the one gomutants set: `go test -run=A
		// -test.run=B` runs B, and `-test.coverprofile` writes the profile
		// somewhere RunCoverage never looks. Reading these as flags named
		// "test.run" / "test.coverprofile" would wave through exactly the
		// override the un-prefixed names are rejected for.
		{"test-prefixed run", []string{"-test.run=TestFoo"}},
		{"test-prefixed run, double dash", []string{"--test.run=TestFoo"}},
		{"test-prefixed coverprofile", []string{"-test.coverprofile=/tmp/c.out"}},
		{"test-prefixed timeout", []string{"-test.timeout=30s"}},
		// -args makes unprefixed fields belong to the test binary, but the
		// direct binary spelling still overrides gomutants' generated flag.
		{"test-prefixed run after args", []string{"-args", "-test.run=TestFoo"}},
		{"test-prefixed coverprofile after args", []string{"--args", "-test.coverprofile=/tmp/c.out"}},
		// A boundary token is only a boundary if the go command read it as
		// one. `go test pkg -bench -args -overlay=x` binds "-args" to -bench,
		// so -overlay is parsed by the go command after all and replaces the
		// mutation overlay: no mutant is applied and every one "survives".
		// Relaxing behind a boundary that was never there would wave through
		// exactly what this guard exists to stop, so an ambiguous preceding
		// flag keeps the scan strict.
		{"managed flag behind a consumable args", []string{"-bench", "-args", "-overlay=/tmp/x.json"}},
		{"managed flag behind a consumable terminator", []string{"-bench", "--", "-overlay=/tmp/x.json"}},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			err := checkTestFlags(tc.flags)
			if err == nil {
				t.Fatalf("checkTestFlags(%v) = nil, want an error", tc.flags)
			}
			if !strings.Contains(err.Error(), "--test-flags") {
				t.Errorf("error should name the flag that carried the value, got: %v", err)
			}
		})
	}
}

// TestCheckTestFlagsErrorEchoesSpelling pins the wording: the message must
// quote the flag as typed. Being told "-run is managed" after passing
// `-test.run` reads like the wrong flag was rejected, and sends the reader
// looking for a `-run` they never wrote.
func TestCheckTestFlagsErrorEchoesSpelling(t *testing.T) {
	for _, spelled := range []string{"-test.run", "--overlay", "-coverprofile"} {
		t.Run(spelled, func(t *testing.T) {
			err := checkTestFlags([]string{spelled + "=x"})
			if err == nil {
				t.Fatalf("checkTestFlags(%q) = nil, want an error", spelled)
			}
			if !strings.Contains(err.Error(), spelled) {
				t.Errorf("error must echo %q as typed, got: %v", spelled, err)
			}
		})
	}
}

// TestCheckTestFlagsAccepts matters as much as the rejections: an
// over-eager match would block the exact use case --test-flags exists for.
func TestCheckTestFlagsAccepts(t *testing.T) {
	accepted := flagCases{
		{"nil", nil},
		{"the motivating case", []string{"-rapid.checks=20"}},
		{"short", []string{"-short"}},
		{"race and count", []string{"-race", "-count=2"}},
		// Once the package has already been parsed, -args is the only way to
		// pass a test-binary flag whose name also belongs to the go command.
		// Neither name here is one gomutants manages, so both are accepted
		// whether or not the boundary itself was read as one.
		{"args with colliding go flag", []string{"-args", "-x"}},
		{"double-dash args with colliding test flag", []string{"-race", "--args", "-short"}},
		// Managed-looking unprefixed names after -args belong to a custom test
		// binary flag and cannot override gomutants' already-parsed settings.
		// This is the relaxation the boundary buys, so the boundary has to be
		// unambiguous: nothing precedes it here that could have consumed it.
		{"custom managed-looking flag after args", []string{"-args", "-run=custom"}},
		// The same relaxation behind a preceding flag, which only stays
		// available because the inline value makes it plain that -bench did
		// not consume the -args. Without the `=` this is rejected; that pair
		// is what pins the syntactic test rather than a blanket allow.
		{"inline value keeps the args boundary", []string{"-bench=.", "-args", "-run=custom"}},
		// The flag terminator makes all remaining fields positional arguments
		// to the test binary, so they cannot override gomutants either.
		{"terminator", []string{"--", "-test.run=positional"}},
		// A managed name appearing as a *value* rather than a flag: the
		// non-flag skip must let it through, or `-gcflags c` and friends
		// would be rejected for spelling a managed flag by coincidence.
		{"managed name as a value", []string{"-gcflags", "c"}},
		// Prefix-adjacent to managed names: matching is on the whole flag
		// name, so these must not be caught by a sloppy HasPrefix.
		{"prefix-adjacent to a managed name", []string{"-count=2", "-cpu=4", "-run.notreal"}},
		{"parallel", []string{"-parallel=4"}},
		// Only a leading `test.` is stripped, and only as a whole segment.
		// Over-trimming here would reject an unrelated third-party flag.
		{"test-prefixed but unmanaged", []string{"-test.short", "-test.v"}},
		{"name merely starting with test", []string{"-testdata=x", "-testify.m=y"}},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkTestFlags(tc.flags); err != nil {
				t.Errorf("checkTestFlags(%v) = %v, want nil", tc.flags, err)
			}
		})
	}
}

// TestTimeoutPolicyFor covers the config→policy mapping, and in
// particular the one field that isn't a straight read: Unmeasured must
// track whether --test-flags is in effect. The timing phase never sees
// those flags, so with them set the recorded durations describe different
// work than the deadline is being sized for — a `-race` run given a
// no-race deadline turns survivors into TIMED_OUT, which drops them out
// of the efficacy denominator instead of into it. STATEMENT_REMOVE on any
// assignment here leaves that field zero.
func TestTimeoutPolicyFor(t *testing.T) {
	const (
		global = 42 * time.Second
		margin = 2.5
		floor  = 3 * time.Second
	)
	// want is the expected policy with only the two derived switches left
	// to vary; the three pass-through fields are the same every time.
	// Compared as a whole struct so a STATEMENT_REMOVE on any assignment
	// in timeoutPolicyFor shows up, without a per-field if apiece.
	want := func(adaptive, unmeasured bool) runner.TimeoutPolicy {
		return runner.TimeoutPolicy{
			Global: global, Margin: margin, Min: floor,
			Adaptive: adaptive, Unmeasured: unmeasured,
		}
	}
	off := false
	cases := []struct {
		name string
		cfg  config.Config
		want runner.TimeoutPolicy
	}{
		{"no test flags", config.Config{}, want(true, false)},
		{"test flags set", config.Config{TestFlags: "-race"}, want(true, true)},
		// Whitespace-only is not a flag: the runner appends nothing, so
		// giving up adaptive sizing here would cost speed for nothing.
		{"whitespace-only test flags", config.Config{TestFlags: "   "}, want(true, false)},
		// The two switches are independent — a --test-flags run with
		// adaptive already off must not read as adaptive.
		{"adaptive off with test flags", config.Config{TestFlags: "-short", AdaptiveTimeout: &off}, want(false, true)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.cfg.TimeoutMargin = margin
			tc.cfg.TimeoutMin = floor
			if got := timeoutPolicyFor(&tc.cfg, global); got != tc.want {
				t.Errorf("timeoutPolicyFor(--test-flags=%q) = %+v, want %+v", tc.cfg.TestFlags, got, tc.want)
			}
		})
	}
}

// TestRunRejectsManagedTestFlags pins the guard at the run() boundary,
// not just on the helper: the check must sit after ApplyFlags and abort
// before any work happens.
func TestRunRejectsManagedTestFlags(t *testing.T) {
	out, err := captureOutput(t, func() error {
		return run(context.Background(), []string{"--test-flags", "-overlay=/tmp/x.json", "testmod"})
	})
	if err == nil {
		t.Fatal("expected --test-flags -overlay to error")
	}
	requireExitCode(t, err, exitCodeUsageError)
	if !strings.Contains(err.Error(), "-overlay") {
		t.Errorf("error should name the rejected flag, got: %v", err)
	}
	if out != "" {
		t.Errorf("the check must short-circuit before any stdout, got %q", out)
	}
}

// TestRunRejectsManagedTestFlagsFromYAML pins the *placement* of the
// guard: it runs after ApplyFlags, so a value that arrived from
// .gomutants.yml is screened too. Hoisting the check above the merge
// would pass this project straight through.
func TestRunRejectsManagedTestFlagsFromYAML(t *testing.T) {
	dir := setupTinyProject(t)
	cfgPath := filepath.Join(dir, ".gomutants.yml")
	if err := os.WriteFile(cfgPath, []byte("test-flags: \"-overlay=x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := run(context.Background(), []string{"--config", cfgPath, "testmod"})
	requireExitCode(t, err, exitCodeUsageError)
	if !strings.Contains(err.Error(), "-overlay") {
		t.Fatalf("YAML-supplied managed flag must be rejected, got err: %v", err)
	}
}

// TestRunTestFlagsRepeatAccumulate pins the fs.Func accumulation: a second
// --test-flags must add to the first, not replace it. Verified through the
// config the CLI layer builds, since that is what every consumer reads.
func TestRunTestFlagsRepeatAccumulate(t *testing.T) {
	// A managed flag in the *second* occurrence only errors if that
	// occurrence survived the join — a last-one-wins bug would drop the
	// first, and an overwrite bug would drop the second.
	err := run(context.Background(), []string{"--test-flags", "-short", "--test-flags", "-overlay=x", "testmod"})
	if err == nil || !strings.Contains(err.Error(), "-overlay") {
		t.Fatalf("second --test-flags value must survive the join, got err: %v", err)
	}
	err = run(context.Background(), []string{"--test-flags", "-overlay=x", "--test-flags", "-short", "testmod"})
	if err == nil || !strings.Contains(err.Error(), "-overlay") {
		t.Fatalf("first --test-flags value must survive the join, got err: %v", err)
	}
}

// TestRunQuietSuppressesProgressKeepsSummary mirrors TestRunFullPipeline's
// fixture so any drift in non-quiet output is caught by that test, not this one.
func TestRunQuietSuppressesProgressKeepsSummary(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess-spawning test in short mode (self-mutation guard)")
	}
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":      "module testmod\n\ngo 1.26\n",
		"add.go":      "package testmod\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n",
		"add_test.go": "package testmod\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"wrong\")\n\t}\n}\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	outPath := filepath.Join(dir, "report.json")
	out, err := captureOutput(t, func() error {
		return run(context.Background(), []string{
			"--only", "ARITHMETIC_BASE",
			"-w", "1",
			"-o", outPath,
			"--cache=off",
			"--quiet",
			"testmod",
		})
	})
	if err != nil {
		t.Fatalf("run --quiet: %v", err)
	}

	mustNotContain := []string{
		"gomutants v",
		"Target:",
		"Workers:",
		"Resolving packages...",
		"Collecting coverage...",
		"Measuring baseline...",
		"Discovering mutants...",
		"Building per-test coverage map...",
		"Report:",
	}
	for _, s := range mustNotContain {
		if strings.Contains(out, s) {
			t.Errorf("--quiet stdout must not contain %q; full output:\n%s", s, out)
		}
	}

	mustContain := []string{
		"Killed:",
		"Lived:",
		"Efficacy:",
	}
	for _, s := range mustContain {
		if !strings.Contains(out, s) {
			t.Errorf("--quiet stdout must still contain %q; full output:\n%s", s, out)
		}
	}

	// Report file is still written — quiet trims chatter, not artifacts.
	if _, err := os.Stat(outPath); err != nil {
		t.Fatalf("report not written under --quiet: %v", err)
	}
}

// TestRunQuietShorthand kills STATEMENT_REMOVE on the `-q` fs.BoolVar
// shorthand registration. The end-to-end behavior is exhaustively
// covered by TestRunQuietSuppressesProgressKeepsSummary; here we only
// need a single smoking-gun assertion that `-q` plumbed cfg.Quiet
// through to the Terminal.
func TestRunQuietShorthand(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":      "module testmod\n\ngo 1.26\n",
		"add.go":      "package testmod\n\nfunc Add(a, b int) int { return a + b }\n",
		"add_test.go": "package testmod\nimport \"testing\"\nfunc TestAdd(t *testing.T) { if Add(1,2) != 3 { t.Fatal(\"wrong\") } }\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	out, err := captureOutput(t, func() error {
		return run(context.Background(), []string{"-q", "--dry-run", "--only", "ARITHMETIC_BASE", "testmod"})
	})
	if err != nil {
		t.Fatalf("run -q --dry-run: %v", err)
	}
	if strings.Contains(out, "gomutants v") {
		t.Errorf("-q should suppress header, got: %q", out)
	}
}

// TestRunThresholdEfficacy asserts that --threshold-efficacy=100 turns a
// surviving mutant into exit code 10 (gremlins-compat), and that the
// report is still written before that error fires. The "test" here calls
// the SUT but never asserts anything, so any ARITHMETIC_BASE mutation
// lives.
func TestRunThresholdEfficacy(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess-spawning test in short mode (self-mutation guard)")
	}
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":      "module testmod\n\ngo 1.26\n",
		"add.go":      "package testmod\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n",
		"add_test.go": "package testmod\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\t_ = Add(1, 2)\n}\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	outPath := filepath.Join(dir, "report.json")
	_, err := captureOutput(t, func() error {
		return run(context.Background(), []string{
			"--only", "ARITHMETIC_BASE",
			"-w", "1",
			"-o", outPath,
			"--threshold-efficacy=100",
			"testmod",
		})
	})
	if err == nil {
		t.Fatal("expected --threshold-efficacy=100 to return an error when LIVED > 0")
	}
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != exitCodeEfficacy {
		t.Errorf("expected exitError code 10 (gremlins-compat), got: %v", err)
	}
	// Report must be written even when the gate fires — the action depends
	// on the JSON/Stryker outputs being available for upload after a fail.
	if _, statErr := os.Stat(outPath); statErr != nil {
		t.Errorf("report should be written before the gate fires: %v", statErr)
	}
}

// TestRunThresholdEfficacySilentWhenClean is the inverse: with no LIVED
// mutants (test asserts the result), --threshold-efficacy=100 must NOT
// return an error. Pins the `r.TestEfficacy < thresholdEfficacy` guard so a
// mutation that flips the comparison or drops the guard is observable.
func TestRunThresholdEfficacySilentWhenClean(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess-spawning test in short mode (self-mutation guard)")
	}
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":      "module testmod\n\ngo 1.26\n",
		"add.go":      "package testmod\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n",
		"add_test.go": "package testmod\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"wrong\")\n\t}\n\tif Add(5, 7) != 12 {\n\t\tt.Fatal(\"wrong\")\n\t}\n}\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	_, err := captureOutput(t, func() error {
		return run(context.Background(), []string{
			"--only", "ARITHMETIC_BASE",
			"-w", "1",
			"-o", filepath.Join(dir, "report.json"),
			"--threshold-efficacy=100",
			"testmod",
		})
	})
	if err != nil {
		t.Fatalf("--threshold-efficacy=100 must not error when LIVED == 0: %v", err)
	}
}

// TestRunThresholdMcover pins the second gate: a function whose mutants
// are all NOT_COVERED (no test exercises it) drops mutant coverage to 0%,
// which must surface as exit code 11.
func TestRunThresholdMcover(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess-spawning test in short mode (self-mutation guard)")
	}
	dir := t.TempDir()
	files := map[string]string{
		"go.mod": "module testmod\n\ngo 1.26\n",
		// No test file references Add at all -> ARITHMETIC_BASE mutant on
		// `+` is NOT_COVERED. KILLED+LIVED == 0, NOT_COVERED == 1, so
		// gremlins-formula mcover = 0/1 = 0%.
		"add.go":      "package testmod\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n",
		"add_test.go": "package testmod\n\nimport \"testing\"\n\nfunc TestNoop(t *testing.T) {}\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	_, err := captureOutput(t, func() error {
		return run(context.Background(), []string{
			"--only", "ARITHMETIC_BASE",
			"-w", "1",
			"-o", filepath.Join(dir, "report.json"),
			"--threshold-mcover=50",
			"testmod",
		})
	})
	if err == nil {
		t.Fatal("expected --threshold-mcover=50 to error when coverage is 0%")
	}
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != exitCodeMutantCoverage {
		t.Errorf("expected exitError code 11 (gremlins-compat), got: %v", err)
	}
}

// TestWarnInfraErrors pins the stderr note that keeps a partial run from
// looking complete in CI, where the terminal summary (stdout) is often
// discarded and only the exit code and the JSON report survive.
func TestWarnInfraErrors(t *testing.T) {
	var buf bytes.Buffer
	warnInfraErrors(&buf, &report.Report{MutantsInfraError: 3, MutantsTotal: 40})
	got := buf.String()
	if !strings.Contains(got, "3 of 40 mutants ended in INFRA ERROR") {
		t.Errorf("expected the infra-error warning with both counts, got %q", got)
	}

	buf.Reset()
	warnInfraErrors(&buf, &report.Report{MutantsTotal: 40})
	if buf.Len() != 0 {
		t.Errorf("expected silence when no mutant hit an infrastructure error, got %q", buf.String())
	}
}

// TestRunThresholdSkipsOnEmptyDiscovery pins the deviation from gremlins:
// when a threshold's denominator is zero (no mutants to evaluate), the
// gate is *skipped* with a stderr note rather than failing with a
// misleading "0% below N%" message. A function with no arithmetic
// operators yields zero ARITHMETIC_BASE mutants, so both K+L and
// K+L+NC are zero and both gates skip.
func TestRunThresholdSkipsOnEmptyDiscovery(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess-spawning test in short mode (self-mutation guard)")
	}
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":        "module testmod\n\ngo 1.26\n",
		"greet.go":      "package testmod\n\nfunc Greet() string {\n\treturn \"hello\"\n}\n",
		"greet_test.go": "package testmod\n\nimport \"testing\"\n\nfunc TestGreet(t *testing.T) {\n\tif Greet() != \"hello\" {\n\t\tt.Fatal(\"wrong\")\n\t}\n}\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	stderrText, err := captureStderr(t, func() error {
		_, runErr := captureOutput(t, func() error {
			return run(context.Background(), []string{
				"--only", "ARITHMETIC_BASE",
				"-w", "1",
				"-o", filepath.Join(dir, "report.json"),
				"--threshold-efficacy=80",
				"--threshold-mcover=60",
				"testmod",
			})
		})
		return runErr
	})
	if err != nil {
		t.Fatalf("threshold gates must skip (not error) on empty discovery: %v", err)
	}
	if !strings.Contains(stderrText, "--threshold-efficacy not evaluated") {
		t.Errorf("expected stderr to note the skipped efficacy gate, got: %q", stderrText)
	}
	// mcoverDenom == 0 only when KILLED+LIVED+NOT_COVERED == 0; here all
	// are zero because there are no mutants at all, so the mcover skip
	// note must also appear.
	if !strings.Contains(stderrText, "--threshold-mcover not evaluated") {
		t.Errorf("expected stderr to note the skipped mcover gate, got: %q", stderrText)
	}
}

// TestRunDryRunOutput asserts the exact dry-run line format, which kills
// STATEMENT_REMOVE on the dry-run Printf.
func TestRunDryRunOutput(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":      "module testmod\n\ngo 1.26\n",
		"add.go":      "package testmod\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n",
		"add_test.go": "package testmod\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"wrong\")\n\t}\n}\n",
	}
	for name, content := range files {
		os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
	}

	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	out, err := captureOutput(t, func() error {
		return run(context.Background(), []string{"--dry-run", "--only", "ARITHMETIC_BASE", "testmod"})
	})
	if err != nil {
		t.Fatalf("run dry-run: %v", err)
	}
	// Dry-run should print at least one "[PENDING]" line for the + mutation.
	if !strings.Contains(out, "[PENDING]") {
		t.Errorf("expected [PENDING] marker in dry-run output: %q", out)
	}
	if !strings.Contains(out, "+ → -") {
		t.Errorf("expected '+ → -' in dry-run output: %q", out)
	}
}

// TestRunDefaultsToCurrentDirOnEmptyPackages kills BRANCH_IF on the
// `if len(packages) == 0 { packages = []string{"./..."} }` body. Without
// the default assignment, packages stays empty and ResolvePackages fails
// (or returns empty), so the test crashes or runs against zero packages.
func TestRunDefaultsToCurrentDirOnEmptyPackages(t *testing.T) {
	dir := setupTinyProject(t)
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	out, err := captureOutput(t, func() error {
		return run(context.Background(), []string{"--dry-run", "--only", "ARITHMETIC_BASE"})
	})
	if err != nil {
		t.Fatalf("expected dry-run with default ./... to succeed: %v — BRANCH_IF on the empty-packages default leaves the pattern empty", err)
	}
	// Resolving packages should find the local module.
	if !strings.Contains(out, "Resolving packages... done (1 packages)") {
		t.Errorf("expected default ./... to resolve to 1 package, got: %q", out)
	}
}

// TestRunCoverageErrorMessage kills BRANCH_IF on the runner.RunCoverage
// err return by stubbing the call and asserting the err propagates with
// its original wrapping.
func TestRunCoverageErrorMessage(t *testing.T) {
	dir := setupTinyProject(t)
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	origCov := runCoverageFunc
	defer func() { runCoverageFunc = origCov }()
	runCoverageFunc = func(_ context.Context, _ string, _ []string, _, _, _ string, _ []string) (string, error) {
		return "", errors.New("inject coverage failure: marker_xyz")
	}

	err := run(context.Background(), []string{"--only", "ARITHMETIC_BASE", "-w", "1", "-o", filepath.Join(dir, "r.json"), "testmod"})
	if err == nil {
		t.Fatal("expected error from RunCoverage stub")
	}
	if !strings.Contains(err.Error(), "marker_xyz") {
		t.Errorf("err lost the underlying RunCoverage message; got: %v — BRANCH_IF on the err-return body lets a different (later) error surface", err)
	}
}

// TestRunMeasureBaselineErrorMessage kills BRANCH_IF on the runner.MeasureBaseline
// err return.
func TestRunMeasureBaselineErrorMessage(t *testing.T) {
	dir := setupTinyProject(t)
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	origM := measureBaselineFunc
	defer func() { measureBaselineFunc = origM }()
	measureBaselineFunc = func(_ context.Context, _ string, _ []string, _ string, _ []string) (time.Duration, error) {
		return 0, errors.New("inject baseline failure: marker_pdq")
	}

	err := run(context.Background(), []string{"--only", "ARITHMETIC_BASE", "-w", "1", "-o", filepath.Join(dir, "r.json"), "testmod"})
	if err == nil {
		t.Fatal("expected error from MeasureBaseline stub")
	}
	if !strings.Contains(err.Error(), "marker_pdq") {
		t.Errorf("err lost the MeasureBaseline message; got: %v", err)
	}
}

// TestRunParseProfileErrorMessage kills BRANCH_IF on the coverage.ParseBytes
// err return for the fresh-coverage branch (no --cache).
func TestRunParseProfileErrorMessage(t *testing.T) {
	dir := setupTinyProject(t)
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	origParse := parseBytesFunc
	defer func() { parseBytesFunc = origParse }()
	parseBytesFunc = func([]byte) (*coverage.Profile, error) {
		return nil, errors.New("inject parse failure: marker_abc")
	}

	err := run(context.Background(), []string{"--only", "ARITHMETIC_BASE", "-w", "1", "-o", filepath.Join(dir, "r.json"), "testmod"})
	if err == nil {
		t.Fatal("expected error from ParseBytes stub")
	}
	if !strings.Contains(err.Error(), "marker_abc") {
		t.Errorf("err lost the ParseBytes message; got: %v", err)
	}
}

// TestRunPreReadFilesErrorMessage kills BRANCH_IF on the discover.PreReadFiles
// err return. The wrap text is "pre-reading source files: ...".
func TestRunPreReadFilesErrorMessage(t *testing.T) {
	dir := setupTinyProject(t)
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	origRead := preReadFilesFunc
	defer func() { preReadFilesFunc = origRead }()
	preReadFilesFunc = func([]discover.Package) (map[string][]byte, error) {
		return nil, errors.New("inject pre-read failure: marker_klm")
	}

	err := run(context.Background(), []string{"--only", "ARITHMETIC_BASE", "-w", "1", "-o", filepath.Join(dir, "r.json"), "testmod"})
	if err == nil {
		t.Fatal("expected error from PreReadFiles stub")
	}
	if !strings.Contains(err.Error(), "pre-reading source files") {
		t.Errorf("err should wrap with 'pre-reading source files'; got: %v — BRANCH_IF on the err-return strips the wrap", err)
	}
	if !strings.Contains(err.Error(), "marker_klm") {
		t.Errorf("err lost the PreReadFiles message; got: %v", err)
	}
}

// TestRunUnleashStripGuardSafeOnEmptyArgs kills EXPRESSION_REMOVE on the
// `len(args) > 0` operand and CONDITIONALS_BOUNDARY on the same `> 0`.
// Both mutations let `args[0]` be evaluated when args is empty, producing
// an out-of-bounds panic.
func TestRunUnleashStripGuardSafeOnEmptyArgs(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("run([]) panicked: %v — guard on `len(args) > 0` was relaxed", r)
		}
	}()
	// Empty args: the unleash strip must short-circuit, not index args[0].
	// run() will fail later (no go.mod, etc.); we only care that the strip
	// guard didn't blow up.
	_ = run(context.Background(), []string{})
}

// TestRunMissingGoMod kills BRANCH_IF on the readModuleName error return
// in run(). The wrap text "reading go.mod" is what distinguishes the
// readModuleName failure from the downstream `go list` failure that fires
// when the err-return is elided — both contain "go.mod" verbatim, so the
// test must pin on the more specific prefix.
func TestRunMissingGoMod(t *testing.T) {
	dir := t.TempDir()
	// Create a Go file but no go.mod.
	if err := os.WriteFile(filepath.Join(dir, "x.go"),
		[]byte("package x\nfunc F() int { return 1 + 2 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)
	err := run(context.Background(), []string{"--dry-run", "."})
	requireExitCode(t, err, exitCodeRuntimeError)
	if !strings.Contains(err.Error(), "reading go.mod") {
		t.Errorf("error should be wrapped 'reading go.mod', got: %v — BRANCH_IF on the err-return lets the go.mod failure fall through to a `go list` error that also mentions go.mod", err)
	}
}

// TestRunResolvePackagesErrorMessage upgrades TestRunResolvePackagesError
// with an error-content assertion. The BRANCH_IF on the resolve err-return
// only surfaces if we observe that the returned error came from resolve,
// not from a later step that would also error.
func TestRunResolvePackagesErrorMessage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module testmod\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)
	err := run(context.Background(), []string{"--dry-run", "completely/nonexistent/package/xyz"})
	if err == nil {
		t.Fatal("expected error")
	}
	// resolvePackages wraps with "go list:" prefix. The downstream RunCoverage
	// would also error on the same package but with "coverage run failed:"
	// prefix, so pinning the prefix forces the test through the correct branch.
	if !strings.Contains(err.Error(), "go list") {
		t.Errorf("error should be wrapped with 'go list', got: %v — BRANCH_IF on the err-return lets the failure resurface from a later step", err)
	}
}

// TestRunGetwdError kills BRANCH_IF on the os.Getwd err return. Stub
// getwdFunc to fail; the wrap "getting working directory" is the
// distinguishing signature for this branch.
func TestRunGetwdError(t *testing.T) {
	dir := setupTinyProject(t)
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	origGetwd := getwdFunc
	defer func() { getwdFunc = origGetwd }()
	getwdFunc = func() (string, error) {
		return "", errors.New("inject getwd failure")
	}

	err := run(context.Background(), []string{"--only", "ARITHMETIC_BASE", "-w", "1", "-o", filepath.Join(dir, "r.json"), "testmod"})
	if err == nil {
		t.Fatal("expected error from Getwd stub")
	}
	if !strings.Contains(err.Error(), "getting working directory") {
		t.Errorf("error should be wrapped 'getting working directory', got: %v — BRANCH_IF on the err-return strips the wrap", err)
	}
}

// TestRunMkdirTempError kills BRANCH_IF on the os.MkdirTemp err return.
// We swap mkdirTempFunc rather than munging TMPDIR because TMPDIR also
// breaks `go list` (which runs before MkdirTemp), causing the test to
// fail at the wrong step.
func TestRunMkdirTempError(t *testing.T) {
	dir := setupTinyProject(t)
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	origMkdir := mkdirTempFunc
	defer func() { mkdirTempFunc = origMkdir }()
	mkdirTempFunc = func(string, string) (string, error) {
		return "", errors.New("inject mkdirtemp failure: marker_efg")
	}

	err := run(context.Background(), []string{"--only", "ARITHMETIC_BASE", "-w", "1", "-o", filepath.Join(dir, "r.json"), "testmod"})
	if err == nil {
		t.Fatal("expected error from MkdirTemp stub")
	}
	if !strings.Contains(err.Error(), "creating temp dir") {
		t.Errorf("error should be wrapped 'creating temp dir', got: %v — BRANCH_IF on the err-return strips the wrap", err)
	}
}

// TestRunDefaultsToRecursivePattern kills STATEMENT_REMOVE on the
// `packages = []string{"./..."}` default. We set up a project with a
// sub-package; the default `./...` finds both packages, while an empty
// pattern only resolves the cwd package. Asserting "done (2 packages)"
// pins the difference.
func TestRunDefaultsToRecursivePattern(t *testing.T) {
	dir := setupTinyProject(t)
	subDir := filepath.Join(dir, "sub")
	if err := os.Mkdir(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "x.go"),
		[]byte("package sub\nfunc F() int { return 1 + 2 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "x_test.go"),
		[]byte("package sub\nimport \"testing\"\nfunc TestF(t *testing.T) { if F() != 3 { t.Fatal(\"\") } }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	out, err := captureOutput(t, func() error {
		return run(context.Background(), []string{"--dry-run", "--only", "ARITHMETIC_BASE"})
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "Resolving packages... done (2 packages)") {
		t.Errorf("expected 2 packages with default ./... pattern, got: %q — STATEMENT_REMOVE on the default-assignment leaves packages empty so only the cwd package resolves", out)
	}
}

// TestRunCoverageProfileParseError kills BRANCH_IF on the coverage.ParseFile
// error check. We force RunCoverage to succeed and write a profile, then
// nuke the profile right before ParseFile reads it. Achieved indirectly
// via coverpkg=nomatch (produces an empty profile that still parses) —
// the surest path is to swap the test by pointing tmpDir into a directory
// that gets removed; instead we trust that an empty/malformed profile
// parses successfully and rely on the fallthrough mutation surfacing
// further. Skipping this in favor of the broader pipeline test.
//
// Instead: assert that the success path's "Collecting coverage..." line
// pairs with "done (Xs)". The phaseDuration helper handles the ARITHMETIC
// mutation directly.
func TestPhaseDurationDisplay(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want time.Duration
	}{
		// Round-to-100ms boundary cases.
		{0, 0},
		{49 * time.Millisecond, 0},
		{50 * time.Millisecond, 100 * time.Millisecond},
		{234 * time.Millisecond, 200 * time.Millisecond},
		{1234 * time.Millisecond, 1200 * time.Millisecond},
	}
	for _, c := range cases {
		got := phaseDurationDisplay(c.in)
		if got != c.want {
			t.Errorf("phaseDurationDisplay(%v) = %v, want %v — ARITHMETIC mutation on `100*time.Millisecond` collapses the rounding", c.in, got, c.want)
		}
	}
}

// TestRunBuildTestMapWarningOnError kills BRANCH_IF / BRANCH_ELSE /
// STATEMENT_REMOVE / CONDITIONALS_NEGATION on the BuildTestMap error
// branch. We swap buildTestMapFunc to return an error and assert the
// stderr warning + the "skipped" PhaseDone line both appear.
func TestRunBuildTestMapWarningOnError(t *testing.T) {
	dir := setupTinyProject(t)
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	origBuild := buildTestMapFunc
	defer func() { buildTestMapFunc = origBuild }()
	buildTestMapFunc = func(_ context.Context, _ string, _ []string, _, _, _ string, _ int, _ time.Duration) (*coverage.TestMap, error) {
		return nil, errors.New("inject build-test-map failure")
	}

	var out, errBuf bytes.Buffer
	origStdout := stdout
	origStderr := stderr
	stdout = &out
	stderr = &errBuf
	defer func() {
		stdout = origStdout
		stderr = origStderr
	}()

	err := run(context.Background(), []string{
		"--only", "ARITHMETIC_BASE",
		"-w", "1",
		"-o", filepath.Join(dir, "report.json"),
		"testmod",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(errBuf.String(), "warning: per-test coverage map failed") {
		t.Errorf("stderr missing warning; got: %q — BRANCH_IF / STATEMENT_REMOVE strip the diagnostic", errBuf.String())
	}
	if !strings.Contains(out.String(), "Building per-test coverage map... skipped") {
		t.Errorf("stdout missing 'skipped' PhaseDone; got: %q — CONDITIONALS_NEGATION on `err != nil` flips the branch", out.String())
	}
}

// TestRunBuildTestMapDoneOnSuccess kills BRANCH_ELSE and STATEMENT_REMOVE
// on the success arm of the BuildTestMap branch — without "done" being
// printed, the user has no signal the per-test map is in use.
func TestRunBuildTestMapDoneOnSuccess(t *testing.T) {
	dir := setupTinyProject(t)
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	out, err := captureOutput(t, func() error {
		return run(context.Background(), []string{
			"--only", "ARITHMETIC_BASE",
			"-w", "1",
			"-o", filepath.Join(dir, "report.json"),
			"testmod",
		})
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "Building per-test coverage map... done") {
		t.Errorf("output missing 'Building per-test coverage map... done'; got: %q", out)
	}
}

// TestRunPoolResultsApplied kills STATEMENT_REMOVE on the
// `mutants = pool.Run(...)` assignment. Without the assignment the
// returned mutants are still all Pending, so the report shows zero
// killed/lived/notViable counts — the asserted "Killed: 1" pin would
// fail.
func TestRunPoolResultsApplied(t *testing.T) {
	dir := setupTinyProject(t)
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	out, err := captureOutput(t, func() error {
		return run(context.Background(), []string{
			"--only", "ARITHMETIC_BASE",
			"-w", "1",
			"-o", filepath.Join(dir, "report.json"),
			"testmod",
		})
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// Tiny project has exactly one ARITHMETIC_BASE mutant on `+`.
	// The mutated `+`→`-` makes TestAdd fail, so it should be killed.
	if !strings.Contains(out, "Killed:       1") {
		t.Errorf("output missing 'Killed:       1'; got: %q — STATEMENT_REMOVE on `mutants = pool.Run(...)` drops the result assignment", out)
	}
}

// TestRunWriteJSONError kills BRANCH_IF on the report.WriteJSON error
// return. We point --output at a path that can't be created (a directory)
// so WriteJSON fails; the original wraps the error, the mutant lets it
// fall through silently and `run` returns nil.
func TestRunWriteJSONError(t *testing.T) {
	dir := setupTinyProject(t)
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	// Make `report.json` a directory so WriteJSON's open fails.
	badPath := filepath.Join(dir, "report.json")
	if err := os.Mkdir(badPath, 0o755); err != nil {
		t.Fatal(err)
	}

	err := run(context.Background(), []string{
		"--only", "ARITHMETIC_BASE",
		"-w", "1",
		"-o", badPath,
		"testmod",
	})
	if err == nil {
		t.Fatal("expected WriteJSON error when output path is a directory")
	}
	if !strings.Contains(err.Error(), "writing report") && !strings.Contains(err.Error(), "report") {
		t.Errorf("error should wrap WriteJSON failure, got: %v", err)
	}
}

// TestReadModuleNameWrappedReadError upgrades TestReadModuleNameMissing
// with an error-message check that locks down BRANCH_IF on the ReadFile
// err return inside readModuleName.
func TestReadModuleNameWrappedReadError(t *testing.T) {
	_, err := readModuleName("/definitely/not/a/real/dir/xyz")
	if err == nil {
		t.Fatal("expected error reading nonexistent go.mod")
	}
	if !strings.Contains(err.Error(), "reading go.mod") {
		t.Errorf("error should wrap with 'reading go.mod', got: %v — BRANCH_IF on the read-error return elides the wrap", err)
	}
}

// TestReadModuleNameBlankLines kills EXPRESSION_REMOVE on the
// `len(fields) >= 2 && fields[0] == "module"` guard. The left operand is
// what guards `fields[0]` from indexing an empty slice. We feed a go.mod
// with leading blank/whitespace lines so the loop encounters
// strings.Fields output of length 0; the original short-circuits, the
// mutant panics on fields[0].
func TestReadModuleNameBlankLines(t *testing.T) {
	dir := t.TempDir()
	// Blank line, then whitespace-only, then the module directive.
	goMod := "\n   \nmodule example.com/m\n\ngo 1.26\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("readModuleName panicked on blank lines: %v — EXPRESSION_REMOVE on the len-check guard relaxed it", r)
		}
	}()
	got, err := readModuleName(dir)
	if err != nil {
		t.Fatalf("readModuleName: %v", err)
	}
	if got != "example.com/m" {
		t.Errorf("got %q, want example.com/m", got)
	}
}

// TestReadModuleNameScannerError pins the `sc.Err()` propagation: a
// pre-`module` line longer than bufio.Scanner's 64 KiB cap surfaces as a
// "scanning go.mod" error instead of the misleading "module name not
// found". Without the sc.Err() check, this test fails with the wrong
// error message.
func TestReadModuleNameScannerError(t *testing.T) {
	dir := t.TempDir()
	// 70 KiB single line ahead of `module …` → bufio.ErrTooLong on the
	// first Scan; module directive never reached.
	long := strings.Repeat("x", 70*1024)
	goMod := "// " + long + "\nmodule example.com/m\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := readModuleName(dir)
	if err == nil {
		t.Fatal("expected error for too-long line, got nil")
	}
	if !strings.Contains(err.Error(), "scanning go.mod") {
		t.Errorf("expected scanner-error wrapping, got: %v", err)
	}
}

// TestRun_CoverageCacheHit_SkipsRunCoverage stubs runCoverageFunc to
// fail; the only way `run` succeeds is if the cached coverage profile
// short-circuits the call. We pre-seed the cache by running once with
// --cache enabled (real runCoverageFunc), then swap in a poisoned stub
// for the second run.
func TestRun_CoverageCacheHit_SkipsRunCoverage(t *testing.T) {
	dir := setupTinyProject(t)
	orig, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(orig)

	cachePath := filepath.Join(dir, ".gomutants-cache.json")
	args := []string{
		"--only", "ARITHMETIC_BASE",
		"-w", "1",
		"-o", filepath.Join(dir, "r.json"),
		"--cache", cachePath,
		"testmod",
	}

	// First run: real runCoverageFunc populates the cache.
	if _, err := captureOutput(t, func() error { return run(context.Background(), args) }); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// Sanity-check: cache file exists and has a coverage profile in it.
	cacheBytes, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	if !strings.Contains(string(cacheBytes), `"coverage_key":`) {
		t.Fatalf("cache missing coverage_key after first run: %s", cacheBytes)
	}

	// Second run: any call to runCoverageFunc would surface as an error.
	origRC := runCoverageFunc
	defer func() { runCoverageFunc = origRC }()
	runCoverageFunc = func(context.Context, string, []string, string, string, string, []string) (string, error) {
		return "", errors.New("runCoverageFunc must NOT be called on a cache hit")
	}

	out, err := captureOutput(t, func() error { return run(context.Background(), args) })
	if err != nil {
		t.Fatalf("second run: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "cached") {
		t.Errorf("second-run output should show 'cached' suffix on coverage phase; got: %s", out)
	}
}

// TestRun_CoverageCacheMiss_RunsCoverage seeds the cache with a stale
// CoverageKey, then runs gomutants. The stub captures whether
// runCoverageFunc was invoked — the miss path must invoke it.
func TestRun_CoverageCacheMiss_RunsCoverage(t *testing.T) {
	dir := setupTinyProject(t)
	orig, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(orig)

	cachePath := filepath.Join(dir, ".gomutants-cache.json")
	args := []string{
		"--only", "ARITHMETIC_BASE",
		"-w", "1",
		"-o", filepath.Join(dir, "r.json"),
		"--cache", cachePath,
		"testmod",
	}

	// First seed: a hand-written cache file with a *wrong* CoverageKey
	// but a syntactically valid (yet content-wrong) cached profile. The
	// key mismatch should force a fresh coverage run; if the key check
	// were missing or inverted, the stale profile would be used and
	// FilterByCoverage would mark every mutant NOT_COVERED, producing
	// "0 to test" — which would still pass run() but with wrong stats.
	// We detect the miss by stubbing runCoverageFunc to set a flag.
	called := false
	origRC := runCoverageFunc
	defer func() { runCoverageFunc = origRC }()
	runCoverageFunc = func(ctx context.Context, projectDir string, packages []string, coverPkg, tags, tmpDir string, testFlags []string) (string, error) {
		called = true
		return origRC(ctx, projectDir, packages, coverPkg, tags, tmpDir, testFlags)
	}

	// Pin the toolchain string so the hand-written cache below passes the
	// metadata gate (otherwise the whole cache is discarded on go_toolchain
	// mismatch and the coverage_key check this test targets never runs).
	origGV := goVersionFunc
	defer func() { goVersionFunc = origGV }()
	goVersionFunc = func(context.Context) string { return "go-test-toolchain" }

	staleCache := `{"schema_version":` + strconv.Itoa(cache.SchemaVersion) +
		`,"go_module":"testmod","tool_version":"` + cacheToolVersion() +
		`","go_toolchain":"go-test-toolchain","coverage_key":"definitelywrongkey","coverage_profile":"mode: set\n","entries":[]}`
	if err := os.WriteFile(cachePath, []byte(staleCache), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := captureOutput(t, func() error { return run(context.Background(), args) }); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !called {
		t.Fatal("runCoverageFunc not called despite stale coverage_key — cache key check is missing or inverted")
	}

	// And: the rewritten cache should contain a non-stale coverage_key.
	cacheBytes, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	if strings.Contains(string(cacheBytes), "definitelywrongkey") {
		t.Errorf("stale coverage_key was not overwritten after miss: %s", cacheBytes)
	}
}

// TestRun_CacheOff_NeverWritesCoverageFields confirms the disabled
// path: with --cache=off, runCoverageFunc is called every time and no
// cache file is created.
func TestRun_CacheOff_NeverWritesCoverageFields(t *testing.T) {
	dir := setupTinyProject(t)
	orig, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(orig)

	cachePath := filepath.Join(dir, ".gomutants-cache.json")
	args := []string{
		"--only", "ARITHMETIC_BASE",
		"-w", "1",
		"-o", filepath.Join(dir, "r.json"),
		"--cache", "off",
		"testmod",
	}

	if _, err := captureOutput(t, func() error { return run(context.Background(), args) }); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Errorf("cache file unexpectedly created with --cache=off: stat err=%v", err)
	}
}

// TestRunCheckpointInterval exercises mid-run cache checkpointing without
// a real kill. cacheSaveFunc is the only caller-visible cache write, so
// swapping it for a counter observes every checkpoint. With
// --checkpoint-interval=1ns every onResult callback checkpoints (the
// throttle window is always elapsed), so the cache is saved once per
// tested mutant plus once more for the final forced flush. With
// --checkpoint-interval=0 periodic checkpointing is off and only the
// final flush writes the cache — exactly the pre-checkpointing behavior.
func TestRunCheckpointInterval(t *testing.T) {
	countSaves := func(t *testing.T, intervalArg string) int {
		t.Helper()
		dir := setupTinyProject(t)
		t.Chdir(dir)

		var saves int
		origSave := cacheSaveFunc
		defer func() { cacheSaveFunc = origSave }()
		cacheSaveFunc = func(c *cache.Cache, path string) error {
			saves++
			return origSave(c, path)
		}

		args := []string{
			"--only", "ARITHMETIC_BASE",
			"-w", "1",
			"-o", filepath.Join(dir, "r.json"),
			"--cache", filepath.Join(dir, ".gomutants-cache.json"),
			"--checkpoint-interval", intervalArg,
			"testmod",
		}
		if _, err := captureOutput(t, func() error { return run(context.Background(), args) }); err != nil {
			t.Fatalf("run: %v", err)
		}
		return saves
	}

	// 1ns: every onResult checkpoints, plus one final forced flush.
	if got := countSaves(t, "1ns"); got < 2 {
		t.Errorf("--checkpoint-interval=1ns: %d cache saves, want >= 2 (periodic + final flush)", got)
	}

	// 0: periodic checkpointing disabled; only the final flush writes.
	if got := countSaves(t, "0"); got != 1 {
		t.Errorf("--checkpoint-interval=0: %d cache saves, want exactly 1 (final flush only)", got)
	}
}

// setupTinyProject creates a minimal Go project with one TestAdd that
// kills the ARITHMETIC_BASE mutation on `+`. Used by tests that want a
// cheap full-pipeline run.
func setupTinyProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":      "module testmod\n\ngo 1.26\n",
		"add.go":      "package testmod\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n",
		"add_test.go": "package testmod\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"wrong\")\n\t}\n}\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// writeLoggingModule creates a module whose only interesting arithmetic
// appears twice: once inside a log.Printf call, once outside it. Every
// --exclude-calls test below turns on the same distinction — what the
// filter drops versus what it must leave alone.
func writeLoggingModule(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"go.mod": "module testmod\n\ngo 1.26\n",
		"ratio.go": "package testmod\n\nimport \"log\"\n\n" +
			"func Ratio(done, total int) int {\n" +
			"\tlog.Printf(\"progress %d%%\", done*100/total)\n" +
			"\treturn done * 100 / total\n}\n",
		"ratio_test.go": "package testmod\n\nimport \"testing\"\n\n" +
			"func TestRatio(t *testing.T) {\n\tif Ratio(1, 2) != 50 {\n\t\tt.Fatal(\"wrong\")\n\t}\n}\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestRunExcludeCallsDefaultsSuppressLogArithmetic pins the out-of-the-box
// behaviour the feature exists for: with no configuration at all, mutants
// inside a stdlib logging call are gone, and the identical arithmetic one
// line below is untouched.
func TestRunExcludeCallsDefaultsSuppressLogArithmetic(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess-spawning test in short mode (self-mutation guard)")
	}
	dir := writeLoggingModule(t)
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	out, err := captureOutput(t, func() error {
		return run(context.Background(), []string{"--dry-run", "-w", "1", "testmod"})
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(out, "ratio.go:6:") {
		t.Errorf("mutants inside the log.Printf call should be suppressed by default; got:\n%s", out)
	}
	if !strings.Contains(out, "ratio.go:7:") {
		t.Errorf("arithmetic outside the log call must survive; got:\n%s", out)
	}
}

// TestRunExcludeCallsDefaultsOff pins the escape hatch: with the built-in
// set switched off and no user list, nothing is suppressed.
func TestRunExcludeCallsDefaultsOff(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess-spawning test in short mode (self-mutation guard)")
	}
	dir := writeLoggingModule(t)
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	out, err := captureOutput(t, func() error {
		return run(context.Background(), []string{"--dry-run", "--exclude-calls-defaults=false", "-w", "1", "testmod"})
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "ratio.go:6:") {
		t.Errorf("--exclude-calls-defaults=false must leave log-call mutants in place; got:\n%s", out)
	}
}

// TestRunExcludeCallsFlagExtendsDefaults pins that a CLI list adds to the
// built-ins rather than replacing them: the user pattern reaches a call
// the defaults don't cover, and log.Printf stays covered.
func TestRunExcludeCallsFlagExtendsDefaults(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess-spawning test in short mode (self-mutation guard)")
	}
	dir := t.TempDir()
	files := map[string]string{
		"go.mod": "module testmod\n\ngo 1.26\n",
		"ratio.go": "package testmod\n\nimport \"log\"\n\n" +
			"func metric(name string, v int) {}\n\n" +
			"func Ratio(done, total int) int {\n" +
			"\tlog.Printf(\"progress %d%%\", done*100/total)\n" +
			"\tmetric(\"ratio\", done*100/total)\n" +
			"\treturn done * 100 / total\n}\n",
		"ratio_test.go": "package testmod\n\nimport \"testing\"\n\n" +
			"func TestRatio(t *testing.T) {\n\tif Ratio(1, 2) != 50 {\n\t\tt.Fatal(\"wrong\")\n\t}\n}\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	out, err := captureOutput(t, func() error {
		return run(context.Background(), []string{"--dry-run", "--exclude-calls", "metric", "-w", "1", "testmod"})
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(out, "ratio.go:8:") {
		t.Errorf("the built-in log.Print* entry must still apply alongside a user list; got:\n%s", out)
	}
	if strings.Contains(out, "ratio.go:9:") {
		t.Errorf("the user's metric pattern should suppress its call; got:\n%s", out)
	}
	if !strings.Contains(out, "ratio.go:10:") {
		t.Errorf("arithmetic outside any excluded call must survive; got:\n%s", out)
	}
}

// TestRunRejectsMatchAllExcludeCalls pins the fail-fast: a pattern that
// would suppress everything is rejected before any go test runs, and the
// message names the flag.
func TestRunRejectsMatchAllExcludeCalls(t *testing.T) {
	dir := writeLoggingModule(t)
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	err := run(context.Background(), []string{"--exclude-calls", "*", "testmod"})
	requireExitCode(t, err, exitCodeUsageError)
	if !strings.Contains(err.Error(), "--exclude-calls") {
		t.Errorf("error should name the flag, got: %v", err)
	}
}

// TestRunExcludeCallsReportsBreakdown pins the reporting contract end to
// end: call-suppressed mutants land in the shared mutants_suppressed
// bucket, are broken out under mutants_suppressed_by_calls, and are
// labelled by source on the summary line.
func TestRunExcludeCallsReportsBreakdown(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess-spawning test in short mode (self-mutation guard)")
	}
	dir := writeLoggingModule(t)
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	outPath := filepath.Join(dir, "report.json")
	out, err := captureOutput(t, func() error {
		return run(context.Background(), []string{
			"--only", "ARITHMETIC_BASE", "-w", "1", "-o", outPath, "testmod",
		})
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "Suppressed:   2  (exclude-calls)") {
		t.Errorf("summary should attribute the suppressions to exclude-calls; got:\n%s", out)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading report: %v", err)
	}
	var r struct {
		MutantsTotal             int `json:"mutants_total"`
		MutantsSuppressed        int `json:"mutants_suppressed"`
		MutantsSuppressedByCalls int `json:"mutants_suppressed_by_calls"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatalf("parsing report: %v", err)
	}
	if r.MutantsSuppressed != 2 {
		t.Errorf("mutants_suppressed=%d, want 2", r.MutantsSuppressed)
	}
	if r.MutantsSuppressedByCalls != 2 {
		t.Errorf("mutants_suppressed_by_calls=%d, want 2", r.MutantsSuppressedByCalls)
	}
	if r.MutantsTotal != 2 {
		t.Errorf("mutants_total=%d, want 2 (suppressed mutants leave every other count)", r.MutantsTotal)
	}
}

// TestRunRejectsInvalidExcludeCallsDefaults pins the BoolFunc parse path:
// a non-boolean value must fail with the flag named, not be silently
// treated as "off".
func TestRunRejectsInvalidExcludeCallsDefaults(t *testing.T) {
	_, err := captureStderr(t, func() error {
		return run(context.Background(), []string{"--exclude-calls-defaults=maybe"})
	})
	requireExitCode(t, err, exitCodeUsageError)
	if !strings.Contains(err.Error(), "exclude-calls-defaults") {
		t.Errorf("error should name the flag, got: %v", err)
	}
}

func TestRunUnknownMutantIDIsUsageError(t *testing.T) {
	dir := setupTinyProject(t)
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	err := run(context.Background(), []string{
		"--only", "ARITHMETIC_BASE",
		"--run-mutant-id", "no/such/file.go:Nope:ARITHMETIC_BASE#1",
		"-w", "1",
		"-o", filepath.Join(dir, "report.json"),
		"testmod",
	})
	// Exit 2, not 1: naming a mutant that doesn't exist is a bad
	// invocation, in the same class as an unknown flag.
	requireExitCode(t, err, exitCodeUsageError)
	if !strings.Contains(err.Error(), "no mutant matches") {
		t.Errorf("error should explain the id matched nothing, got: %v", err)
	}
}

func TestRunAmbiguousMutantIDIsUsageError(t *testing.T) {
	dir := setupTinyProject(t)
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	// setupTinyProject's Add has one `+`, so widen the mutator set to get
	// several mutants under one file prefix.
	err := run(context.Background(), []string{
		"--run-mutant-id", "add.go:",
		"-w", "1",
		"-o", filepath.Join(dir, "report.json"),
		"testmod",
	})
	requireExitCode(t, err, exitCodeUsageError)
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("error should report the prefix as ambiguous, got: %v", err)
	}
}

// TestRunMutantIDResolvesBeforeCoverage pins the *placement* of the id
// resolution: it happens on the discovered set right after packages are
// resolved, so a typo costs nothing. Both slow phases are stubbed to fail;
// if resolution moved back below them, one of those markers would surface
// instead of the usage error.
func TestRunMutantIDResolvesBeforeCoverage(t *testing.T) {
	dir := setupTinyProject(t)
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	origCov := runCoverageFunc
	defer func() { runCoverageFunc = origCov }()
	runCoverageFunc = func(_ context.Context, _ string, _ []string, _, _, _ string, _ []string) (string, error) {
		return "", errors.New("coverage ran: marker_cov")
	}
	origM := measureBaselineFunc
	defer func() { measureBaselineFunc = origM }()
	measureBaselineFunc = func(_ context.Context, _ string, _ []string, _ string, _ []string) (time.Duration, error) {
		return 0, errors.New("baseline ran: marker_base")
	}

	err := run(context.Background(), []string{
		"--only", "ARITHMETIC_BASE",
		"--run-mutant-id", "no/such/file.go:Nope:ARITHMETIC_BASE#1",
		"-w", "1",
		"-o", filepath.Join(dir, "report.json"),
		"testmod",
	})
	requireExitCode(t, err, exitCodeUsageError)
	if !strings.Contains(err.Error(), "no mutant matches") {
		t.Errorf("an unknown id must be diagnosed before the coverage and baseline runs, got: %v", err)
	}
}

// TestRunMutantIDRejectsDryRun: --dry-run returns before anything is
// compiled or tested, so the pair would exit 0 for a mutant that was never
// measured — which a script reads as a kill.
func TestRunMutantIDRejectsDryRun(t *testing.T) {
	err := run(context.Background(), []string{
		"--run-mutant-id", "add.go:Add:ARITHMETIC_BASE#1",
		"--dry-run",
		"testmod",
	})
	requireExitCode(t, err, exitCodeUsageError)
	want := "--run-mutant-id cannot be used with --dry-run: a dry run tests nothing, so there is no verdict to report"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

// TestRunMutantIDRejectsDryRunFromYAML pins the guard's placement after
// ApplyFlags: dry-run is a config-file key with no CLI way to turn it back
// off, so a committed `dry-run: true` is exactly the case the caller cannot
// see. Hoisting the check above the merge would pass this project through.
func TestRunMutantIDRejectsDryRunFromYAML(t *testing.T) {
	dir := setupTinyProject(t)
	cfgPath := filepath.Join(dir, ".gomutants.yml")
	if err := os.WriteFile(cfgPath, []byte("dry-run: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := run(context.Background(), []string{
		"--config", cfgPath,
		"--run-mutant-id", "add.go:Add:ARITHMETIC_BASE#1",
		"testmod",
	})
	requireExitCode(t, err, exitCodeUsageError)
	want := "--run-mutant-id cannot be used with --dry-run: a dry run tests nothing, so there is no verdict to report"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestRunMutantIDNarrowsToOneMutant(t *testing.T) {
	dir := setupTinyProject(t)
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	// First a full run, to learn a real id from the report rather than
	// hard-coding one the anchoring scheme might later change.
	full := filepath.Join(dir, "full.json")
	if err := run(context.Background(), []string{
		"--only", "ARITHMETIC_BASE", "-w", "1", "--cache=off", "-o", full, "testmod",
	}); err != nil {
		t.Fatalf("full run: %v", err)
	}
	r := loadReport(t, full)
	if len(r.Files) == 0 || len(r.Files[0].Mutations) == 0 {
		t.Fatalf("full run produced no mutations: %+v", r.Files)
	}
	id := r.Files[0].Mutations[0].ID

	one := filepath.Join(dir, "one.json")
	if err := run(context.Background(), []string{
		"--only", "ARITHMETIC_BASE", "-w", "1", "--cache=off",
		"--run-mutant-id", id, "-o", one, "testmod",
	}); err != nil {
		t.Fatalf("single-mutant run: %v", err)
	}

	got := loadReport(t, one)
	total := 0
	for _, f := range got.Files {
		total += len(f.Mutations)
	}
	if total != 1 {
		t.Fatalf("single-mutant run reported %d mutations, want 1", total)
	}
	if got.Files[0].Mutations[0].ID != id {
		t.Errorf("reported id=%q, want %q", got.Files[0].Mutations[0].ID, id)
	}
	if got.MutantsTotal != 1 {
		t.Errorf("MutantsTotal=%d, want 1", got.MutantsTotal)
	}
}

func TestRunMutantIDBypassesCache(t *testing.T) {
	dir := setupTinyProject(t)
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	cachePath := filepath.Join(dir, "cache.json")
	full := filepath.Join(dir, "full.json")
	if err := run(context.Background(), []string{
		"--only", "ARITHMETIC_BASE", "-w", "1", "--cache", cachePath, "-o", full, "testmod",
	}); err != nil {
		t.Fatalf("warming run: %v", err)
	}
	id := loadReport(t, full).Files[0].Mutations[0].ID

	// Same source, warm cache: an ordinary re-run would replay the stored
	// verdict. --run-mutant-id must measure it again instead, or the
	// write-a-test/did-it-die loop would report stale results.
	one := filepath.Join(dir, "one.json")
	if err := run(context.Background(), []string{
		"--only", "ARITHMETIC_BASE", "-w", "1", "--cache", cachePath,
		"--run-mutant-id", id, "-o", one, "testmod",
	}); err != nil {
		t.Fatalf("single-mutant run: %v", err)
	}

	got := loadReport(t, one)
	if got.MutantsCached != 0 {
		t.Errorf("MutantsCached=%d, want 0 — --run-mutant-id must skip the cache lookup", got.MutantsCached)
	}
	if got.MutantsTotal != 1 {
		t.Fatalf("MutantsTotal=%d, want 1", got.MutantsTotal)
	}
	if got.MutantsKilled+got.MutantsLived != 1 {
		t.Errorf("the mutant should have a fresh verdict, got killed=%d lived=%d",
			got.MutantsKilled, got.MutantsLived)
	}
}

// writeModuleWithFiles creates a module rooted at a temp dir from the
// given file map, with go.mod written for the caller.
func writeModuleWithFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	files["go.mod"] = "module testmod\n\ngo 1.26\n"
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRunMutantIDSuppressedIsUsageError(t *testing.T) {
	dir := writeModuleWithFiles(t, map[string]string{
		"add.go": "package testmod\n\nfunc Add(a, b int) int {\n" +
			"\treturn a + b // gomutants:disable ARITHMETIC_BASE reason=\"checked elsewhere\"\n}\n",
		"add_test.go": "package testmod\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"wrong\")\n\t}\n}\n",
	})
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	// The id resolves — the directive filter runs after FilterByStableID —
	// so without the guard this run would test nothing and exit 0.
	err := run(context.Background(), []string{
		"--only", "ARITHMETIC_BASE",
		"--run-mutant-id", "add.go:Add:ARITHMETIC_BASE#1",
		"-w", "1", "--cache=off",
		"-o", filepath.Join(dir, "report.json"),
		"testmod",
	})
	requireExitCode(t, err, exitCodeUsageError)
	want := `the mutant matching --run-mutant-id "add.go:Add:ARITHMETIC_BASE#1" is suppressed at add.go:4 (checked elsewhere)`
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestRunMutantIDWithoutVerdictIsError(t *testing.T) {
	dir := writeModuleWithFiles(t, map[string]string{
		// Sub has no test, so its mutant comes back NOT COVERED: a status
		// that leaves the efficacy denominator empty and would otherwise
		// exit 0 through the skipped gate.
		"add.go": "package testmod\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n\n" +
			"func Sub(a, b int) int {\n\treturn a - b\n}\n",
		"add_test.go": "package testmod\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"wrong\")\n\t}\n}\n",
	})
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	err := run(context.Background(), []string{
		"--only", "ARITHMETIC_BASE",
		"--run-mutant-id", "add.go:Sub:ARITHMETIC_BASE#1",
		"--threshold-efficacy", "100",
		"-w", "1", "--cache=off",
		"-o", filepath.Join(dir, "report.json"),
		"testmod",
	})
	// Exit 1, not 2: the invocation was fine, the measurement was not.
	requireExitCode(t, err, exitCodeRuntimeError)
	want := `--run-mutant-id "add.go:Sub:ARITHMETIC_BASE#1" produced no verdict: the mutant is NOT COVERED`
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestRunMutantDroppedError(t *testing.T) {
	suppression := func(reason string) []discover.Suppression {
		return []discover.Suppression{{
			Mutant: mutator.Mutant{RelFile: "pkg/a.go", Line: 12},
			Reason: reason,
		}}
	}
	tests := []struct {
		name         string
		changedSince string
		suppressed   []discover.Suppression
		want         string
	}{
		{
			name:       "suppressed with reason",
			suppressed: suppression("commutative"),
			want:       `the mutant matching --run-mutant-id "a#1" is suppressed at pkg/a.go:12 (commutative)`,
		},
		{
			name:       "suppressed without reason",
			suppressed: suppression(""),
			want:       `the mutant matching --run-mutant-id "a#1" is suppressed at pkg/a.go:12 (no reason)`,
		},
		{
			name:         "outside the diff",
			changedSince: "origin/main",
			want:         `the mutant matching --run-mutant-id "a#1" is not on any line changed since "origin/main"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := runMutantDroppedError("a#1", tt.changedSince, tt.suppressed)
			if got.Error() != tt.want {
				t.Errorf("error = %q, want %q", got.Error(), tt.want)
			}
		})
	}
}

// TestEmbedFilesByDir asserts the shape cache.Hasher.SetEmbedFiles expects:
// keyed by package directory, valued by go list's dir-relative paths, and
// with embed-less packages left out entirely (an absent directory and one
// mapped to an empty list hash identically, so the sparse map is the
// smaller thing to carry).
func TestEmbedFilesByDir(t *testing.T) {
	got := embedFilesByDir([]discover.Package{
		{Dir: "/m/a", ImportPath: "m/a", EmbedFiles: []string{"data/schema.json", "tmpl/x.tmpl"}},
		{Dir: "/m/b", ImportPath: "m/b"},
		{Dir: "/m/c", ImportPath: "m/c", EmbedFiles: []string{}},
	})

	if len(got) != 1 {
		t.Fatalf("embedFilesByDir = %v, want only the embedding package", got)
	}
	if !slices.Equal(got["/m/a"], []string{"data/schema.json", "tmpl/x.tmpl"}) {
		t.Errorf("got[\"/m/a\"] = %v, want both embed inputs", got["/m/a"])
	}
}
