package coverage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// runWithDeadline runs fn in a goroutine and fails the test if it doesn't
// return within d. Mutations on close(channel) / counter-increment / range
// feeders deadlock the production code; wrapping the call lets us catch
// those as t.Fatal (mutant KILLED) instead of hanging until gomutants's
// per-mutant timeout fires (mutant TIMED OUT).
func runWithDeadline(t *testing.T, d time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("deadlocked: function exceeded %s", d)
	}
}

// setupTestProject creates a minimal Go project for integration tests.
func setupTestProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	files := map[string]string{
		"go.mod": "module testmod\n\ngo 1.26\n",
		"add.go": `package testmod

func Add(a, b int) int {
	return a + b
}

func Unused() int {
	return 42
}
`,
		"add_test.go": `package testmod

import "testing"

func TestAdd(t *testing.T) {
	if Add(1, 2) != 3 {
		t.Fatal("wrong")
	}
}

func TestAddNegative(t *testing.T) {
	if Add(-1, -2) != -3 {
		t.Fatal("wrong")
	}
}
`,
	}

	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestBuildTestMap(t *testing.T) {
	dir := setupTestProject(t)
	tmpDir := t.TempDir()

	var (
		tm  *TestMap
		err error
	)
	runWithDeadline(t, 30*time.Second, func() {
		tm, err = BuildTestMap(context.Background(), dir, []string{"testmod"}, BuildOptions{TmpDir: tmpDir, Workers: 2})
	})
	if err != nil {
		t.Fatalf("BuildTestMap: %v", err)
	}

	if tm == nil {
		t.Fatal("TestMap is nil")
	}

	// The Add function (line 4: return a + b) should be covered by TestAdd and TestAddNegative.
	tests := tm.TestsFor("testmod/add.go", 4)
	if len(tests) == 0 {
		t.Error("expected tests covering add.go:4, got none")
	}

	// Add's coverage block spans lines 3–5 (opening brace to closing brace).
	// Asserting coverage at line 5 (the block's EndLine) kills
	// CONDITIONALS_BOUNDARY on the indexer's `line <= b.EndLine` loop —
	// mutating to `<` would exclude EndLine, leaving this key unmapped.
	tests = tm.TestsFor("testmod/add.go", 5)
	if len(tests) == 0 {
		t.Error("expected tests covering add.go:5 (block EndLine), got none — CONDITIONALS_BOUNDARY on `line <= b.EndLine` would drop this")
	}

	// Unused() at lines 7-9 is not called by any test. Its block exists in
	// every test's coverage profile with Count=0. Kills BRANCH_IF on
	// `if b.Count == 0 { continue }`: under mutation the zero-count block
	// would still be indexed, falsely mapping Unused's lines to tests that
	// never exercised it.
	tests = tm.TestsFor("testmod/add.go", 8)
	if len(tests) != 0 {
		t.Errorf("Unused() line 8 should have no covering tests (Count=0 block), got %v", tests)
	}

	// TestsFor should return nil for uncovered lines.
	tests = tm.TestsFor("testmod/add.go", 999)
	if tests != nil {
		t.Errorf("expected nil for uncovered line, got %v", tests)
	}
}

// writeModule writes `files` (path → content) under a fresh temp dir and
// returns it.
func writeModule(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
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

// compileFixture compiles the test binary of the module rooted at dir,
// whose package is `importPath`.
func compileFixture(t *testing.T, dir, importPath string) *compiledPkg {
	t.Helper()
	cp, err := compileTestBinary(context.Background(), dir, BuildOptions{TmpDir: t.TempDir()}, resolvedPkg{importPath: importPath, dir: dir})
	if err != nil {
		t.Fatalf("compileTestBinary: %v", err)
	}
	return cp
}

// cwdListModule's TestMain refuses to run (stderr + exit 3) unless
// testdata/marker resolves against the working directory.
var cwdListModule = map[string]string{
	"go.mod": "module cwdlist\n\ngo 1.26\n",
	"lib.go": "package cwdlist\n\nfunc F() int { return 1 }\n",
	"lib_test.go": `package cwdlist

import (
	"fmt"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if _, err := os.Stat("testdata/marker"); err != nil {
		fmt.Fprintln(os.Stderr, "listing refused: no marker in cwd")
		os.Exit(3)
	}
	os.Exit(m.Run())
}

func TestF(t *testing.T) { F() }
`,
}

// TestListBinTestsRunsFromPkgDir kills STATEMENT_REMOVE on `cmd.Dir =
// cp.dir` in listBinTests: the TestMain only lets listing proceed when its
// relative fixture resolves against the package directory.
func TestListBinTestsRunsFromPkgDir(t *testing.T) {
	files := maps.Clone(cwdListModule)
	files["testdata/marker"] = "x\n"
	dir := writeModule(t, files)
	cp := compileFixture(t, dir, "cwdlist")

	names, err := listBinTests(context.Background(), cp, 0)
	if err != nil {
		t.Fatalf("listBinTests: %v", err)
	}
	if !slices.Equal(names, []string{"TestF"}) {
		t.Errorf("listBinTests = %v, want [TestF]", names)
	}
}

// TestListBinTestsErrorIncludesStderr kills STATEMENT_REMOVE on
// `cmd.Stderr = &stderr` in listBinTests: the reason must quote the
// binary's own message (its first stderr line), not the GOCOVERDIR warning
// a -cover binary appends after it.
func TestListBinTestsErrorIncludesStderr(t *testing.T) {
	dir := writeModule(t, cwdListModule)
	cp := compileFixture(t, dir, "cwdlist")

	_, err := listBinTests(context.Background(), cp, 0)
	if err == nil {
		t.Fatal("expected error from a binary that exits 3 on -test.list")
	}
	if !strings.HasSuffix(err.Error(), ": listing refused: no marker in cwd") {
		t.Errorf("error should end with the binary's first stderr line, got: %q", err)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
		t.Errorf("error should wrap the binary's exit status 3, got: %v", err)
	}
}

// TestListBinTestsRejectsGluedOutput: a TestMain that prints without a
// trailing newline glues its output onto the first test name, losing that
// test, so the listing must not be trusted.
func TestListBinTestsRejectsGluedOutput(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"go.mod": "module gluemod\n\ngo 1.26\n",
		"lib_test.go": `package gluemod

import (
	"fmt"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	fmt.Print("partial")
	os.Exit(m.Run())
}

func TestOne(t *testing.T) {}

func TestTwo(t *testing.T) {}
`,
	})
	cp := compileFixture(t, dir, "gluemod")

	names, err := listBinTests(context.Background(), cp, 0)
	if err == nil || !strings.Contains(err.Error(), `"partialTestOne"`) {
		t.Errorf("listBinTests = (%v, %v), want an error quoting the glued line", names, err)
	}
}

// TestBuildTestMapUnmapsUnlistablePackage: a binary that refuses to list
// leaves its package unmapped, with the binary's message as the reason,
// instead of failing the whole map.
func TestBuildTestMapUnmapsUnlistablePackage(t *testing.T) {
	dir := writeModule(t, cwdListModule)

	tm, err := BuildTestMap(context.Background(), dir, []string{"./..."}, BuildOptions{TmpDir: t.TempDir(), Workers: 1})
	if err != nil {
		t.Fatalf("BuildTestMap: %v", err)
	}
	got := tm.Unmapped()
	if len(got) != 1 || got[0].ImportPath != "cwdlist" || !strings.Contains(got[0].Reason, "listing refused") {
		t.Errorf("Unmapped() = %+v, want cwdlist with the listing's reason", got)
	}
}

// hangModule has a test that sleeps for an hour, and a TestMain that does
// the same before m.Run when HANG_MAIN=1. A sleep, not a bare select{}:
// the runtime would report a blocked-forever select as a deadlock and exit.
var hangModule = map[string]string{
	"go.mod": "module hangmod\n\ngo 1.26\n",
	"lib.go": "package hangmod\n\nfunc F() int { return 1 }\n",
	"lib_test.go": `package hangmod

import (
	"os"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("HANG_MAIN") == "1" {
		time.Sleep(time.Hour)
	}
	os.Exit(m.Run())
}

func TestQuick(t *testing.T) { F() }

func TestHang(t *testing.T) { time.Sleep(time.Hour) }
`,
}

// TestRunCompiledTestTimeout is the regression gate for a test that hangs
// when run alone: a bare test binary has no default timeout, so without
// the deadline the coverage phase would block forever.
func TestRunCompiledTestTimeout(t *testing.T) {
	dir := writeModule(t, hangModule)
	cp := compileFixture(t, dir, "hangmod")

	var (
		blocks  []Block
		dur     time.Duration
		skipped bool
		err     error
	)
	runWithDeadline(t, 30*time.Second, func() {
		blocks, dur, skipped, err = runCompiledTest(context.Background(), cp, "TestHang", filepath.Join(t.TempDir(), "hang.cov"), 200*time.Millisecond)
	})
	if blocks != nil || skipped {
		t.Errorf("a killed run returned %d blocks, skipped %v; want none", len(blocks), skipped)
	}
	if err == nil || err.Error() != "TestHang timed out after 200ms when run alone" {
		t.Errorf("err = %v, want the timeout reason", err)
	}
	if dur < 200*time.Millisecond {
		t.Errorf("duration %v is shorter than the timeout that cut the run off", dur)
	}
}

// TestListBinTestsTimeout covers the hang that -test.timeout would miss: a
// TestMain stuck before m.Run. The deadline must still end the listing.
func TestListBinTestsTimeout(t *testing.T) {
	dir := writeModule(t, hangModule)
	cp := compileFixture(t, dir, "hangmod")
	t.Setenv("HANG_MAIN", "1")

	var err error
	runWithDeadline(t, 30*time.Second, func() {
		_, err = listBinTests(context.Background(), cp, 200*time.Millisecond)
	})
	if err == nil || err.Error() != "listing its tests timed out after 200ms" {
		t.Errorf("err = %v, want the listing's timeout", err)
	}
}

// TestResolvePackagesErrorMessage kills STATEMENT_REMOVE on
// `cmd.Stderr = &stderr` in resolvePackages.
func TestResolvePackagesErrorMessage(t *testing.T) {
	_, err := resolvePackages(context.Background(), t.TempDir(), []string{"definitely/nonexistent/pkg/zzz"}, "")
	if err == nil {
		t.Fatal("expected error for nonexistent package")
	}
	msg := err.Error()
	if !strings.Contains(msg, "definitely/nonexistent/pkg/zzz") && !strings.Contains(msg, "cannot find") && !strings.Contains(msg, "no required module") {
		t.Errorf("error should include stderr content, got: %q", msg)
	}
}

// TestBuildTestMapCoverPkgNoMatch kills three mutations on the
// `if coverPkg != "" { args = append(args, "-coverpkg="+coverPkg) }` block:
//
//   - CONDITIONALS_NEGATION (!=  →  ==): a non-empty coverPkg would no
//     longer trigger the append; the test binary would be built with
//     default coverage (of the tested package) and the map populates.
//   - BRANCH_IF (body elided): the append is skipped; same as negation.
//   - STATEMENT_REMOVE on the append: same as BRANCH_IF.
//
// Strategy: pass a coverpkg pattern that matches no real package. Under
// original behavior, the test binary is built with `-coverpkg=<nomatch>`
// and records coverage for nothing — so no test lines end up in the map.
// Under any of the three mutations, the flag isn't passed, the default
// coverage of "testmod" kicks in, and add.go:4 gets mapped to TestAdd.
func TestBuildTestMapCoverPkgNoMatch(t *testing.T) {
	dir := setupTestProject(t)
	tmpDir := t.TempDir()

	tm, err := BuildTestMap(context.Background(), dir, []string{"testmod"}, BuildOptions{CoverPkg: "completely/nonexistent/zzz", TmpDir: tmpDir, Workers: 2})
	if err != nil {
		t.Fatalf("BuildTestMap: %v", err)
	}
	tests := tm.TestsFor("testmod/add.go", 4)
	if len(tests) != 0 {
		t.Errorf("with coverpkg=nomatch, no lines should be mapped; got %v — flag must have been dropped by mutation", tests)
	}
}

// TestBuildTestMapPackageArgPassed kills STATEMENT_REMOVE on
// `args = append(args, pkg.importPath)`. Root has go.mod only (no Go
// files), tests live in a subpackage. Under the original the compile
// command is `go test -c ... rootmod/sub` — builds sub's tests. Under
// mutation the command runs in projectDir (rootDir) without a package
// arg, fails with "no Go files", skips the package, and the map is
// empty.
func TestBuildTestMapPackageArgPassed(t *testing.T) {
	rootDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootDir, "go.mod"), []byte("module rootmod\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	subDir := filepath.Join(rootDir, "sub")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	libSrc := "package sub\n\nfunc F() int { return 1 + 2 }\n"
	testSrc := "package sub\n\nimport \"testing\"\n\nfunc TestF(t *testing.T) { if F() != 3 { t.Fatal(\"wrong\") } }\n"
	if err := os.WriteFile(filepath.Join(subDir, "lib.go"), []byte(libSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "lib_test.go"), []byte(testSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	tm, err := BuildTestMap(context.Background(), rootDir, []string{"rootmod/sub"}, BuildOptions{TmpDir: t.TempDir(), Workers: 1})
	if err != nil {
		t.Fatalf("BuildTestMap: %v", err)
	}
	tests := tm.TestsFor("rootmod/sub/lib.go", 3)
	if len(tests) == 0 {
		t.Error("expected TestF mapped to lib.go:3 — without the package arg `go test -c` defaults to cwd (rootDir) which has no Go files")
	}
}

func TestBuildTestMapWithCoverpkg(t *testing.T) {
	dir := setupTestProject(t)
	tmpDir := t.TempDir()

	tm, err := BuildTestMap(context.Background(), dir, []string{"testmod"}, BuildOptions{CoverPkg: "testmod", TmpDir: tmpDir, Workers: 2})
	if err != nil {
		t.Fatalf("BuildTestMap with coverpkg: %v", err)
	}
	if tm == nil {
		t.Fatal("TestMap is nil")
	}
}

// TestBuildTestMapContextCancelled cancels the ctx during the first
// per-test run, so feedWork's close-on-ctx.Done path and the workers' ctx
// check run mid-pipeline. Mutating that close to a no-op can leave the
// worker blocked on an empty channel; the deadline catches it. The partial
// map must not be returned: it would route mutants away from tests that
// never ran.
func TestBuildTestMapContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var tests []testEntry
	for i := range 1000 {
		tests = append(tests, testEntry{name: fmt.Sprintf("Test%d", i), pkg: "testmod"})
	}
	stubBuildTestMapDeps(t, tests, []resolvedPkg{{importPath: "testmod", dir: t.TempDir()}})
	var ran int32
	runCompiledTestFunc = func(context.Context, *compiledPkg, string, string, time.Duration) ([]Block, time.Duration, bool, error) {
		atomic.AddInt32(&ran, 1)
		cancel()
		return nil, time.Millisecond, false, nil
	}

	var (
		tm  *TestMap
		err error
	)
	runWithDeadline(t, 30*time.Second, func() {
		tm, err = BuildTestMap(ctx, t.TempDir(), []string{"testmod"}, BuildOptions{TmpDir: t.TempDir(), Workers: 1})
	})
	if err != context.Canceled || tm != nil {
		t.Errorf("BuildTestMap = (%v, %v), want (nil, context.Canceled)", tm, err)
	}
	if got := atomic.LoadInt32(&ran); got != 1 {
		t.Errorf("runCompiledTestFunc called %d times, want 1 — the worker must stop at the cancellation", got)
	}
}
func TestListTests(t *testing.T) {
	dir := setupTestProject(t)
	cp := compileFixture(t, dir, "testmod")

	tests, unmapped := listTests(context.Background(), map[string]*compiledPkg{"testmod": cp}, 0, 1)
	if len(unmapped) > 0 {
		t.Fatalf("listTests: %v", unmapped)
	}

	if len(tests) != 2 {
		t.Fatalf("expected 2 tests, got %d", len(tests))
	}

	names := make(map[string]bool)
	for _, te := range tests {
		names[te.name] = true
		if te.pkg != "testmod" {
			t.Errorf("test %q has pkg=%q, want testmod", te.name, te.pkg)
		}
	}
	if !names["TestAdd"] {
		t.Error("missing TestAdd")
	}
	if !names["TestAddNegative"] {
		t.Error("missing TestAddNegative")
	}
}

// TestBuildTestMapWithPackagePatterns is the #105 regression gate: per-test
// routing must work for package patterns, not only import paths. Before the
// fix, tests listed via `./...` or `.` were tagged with the pattern, never
// matched the import-path-keyed binaries, and the map came back empty.
func TestBuildTestMapWithPackagePatterns(t *testing.T) {
	for _, pattern := range []string{"./...", ".", "testmod"} {
		t.Run(pattern, func(t *testing.T) {
			dir := setupTestProject(t)
			var (
				tm  *TestMap
				err error
			)
			runWithDeadline(t, 30*time.Second, func() {
				tm, err = BuildTestMap(context.Background(), dir, []string{pattern}, BuildOptions{TmpDir: t.TempDir(), Workers: 2})
			})
			if err != nil {
				t.Fatalf("BuildTestMap(%q): %v", pattern, err)
			}
			if tests := tm.TestsFor("testmod/add.go", 4); len(tests) == 0 {
				t.Errorf("BuildTestMap(%q): no tests mapped to add.go:4 — per-test routing is off for this pattern", pattern)
			}
		})
	}
}

// writeTwoPkgModule writes module "twopkg" with packages twopkg/a and
// twopkg/sub, both declaring a test named TestShared plus one of their own.
func writeTwoPkgModule(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":          "module twopkg\n\ngo 1.26\n",
		"a/a.go":          "package a\n\nfunc A() int { return 1 }\n",
		"a/a_test.go":     "package a\n\nimport \"testing\"\n\nfunc TestShared(t *testing.T) {}\n\nfunc TestOnlyA(t *testing.T) {}\n",
		"sub/sub.go":      "package sub\n\nfunc S() int { return 2 }\n",
		"sub/sub_test.go": "package sub\n\nimport \"testing\"\n\nfunc TestShared(t *testing.T) {}\n\nfunc TestOnlySub(t *testing.T) {}\n",
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

// TestListTestsKeysByImportPath drives resolve → compile → list through a
// real multi-package module and checks every test carries its own
// package's import path and its place in that package's listing — the
// same-named TestShared once per package — and that overlapping patterns
// don't list a test twice.
func TestListTestsKeysByImportPath(t *testing.T) {
	dir := writeTwoPkgModule(t)
	want := []testEntry{
		{name: "TestOnlyA", pkg: "twopkg/a", order: 1},
		{name: "TestOnlySub", pkg: "twopkg/sub", order: 1},
		{name: "TestShared", pkg: "twopkg/a", order: 0},
		{name: "TestShared", pkg: "twopkg/sub", order: 0},
	}
	for _, patterns := range [][]string{{"./..."}, {"./...", "./sub"}} {
		pkgs, err := resolvePackages(context.Background(), dir, patterns, "")
		if err != nil {
			t.Fatalf("resolvePackages(%v): %v", patterns, err)
		}
		bins, failures := buildPkgBins(context.Background(), dir, BuildOptions{TmpDir: t.TempDir()}, pkgs)
		if len(failures) > 0 {
			t.Fatalf("buildPkgBins(%v): %v", patterns, failures)
		}
		got, unmapped := listTests(context.Background(), bins, 0, 2)
		if len(unmapped) > 0 {
			t.Fatalf("listTests(%v): %v", patterns, unmapped)
		}
		slices.SortFunc(got, func(x, y testEntry) int {
			return strings.Compare(x.name+"\x00"+x.pkg, y.name+"\x00"+y.pkg)
		})
		if !slices.Equal(got, want) {
			t.Errorf("listTests(%v) =\n  %+v\nwant\n  %+v", patterns, got, want)
		}
	}
}

// TestListTestsFailure: a binary that can't be listed comes back unmapped
// with the reason, rather than failing the listing or silently dropping
// its package's tests.
func TestListTestsFailure(t *testing.T) {
	bins := map[string]*compiledPkg{"gone": {binPath: "/nonexistent/absolutely/not/a/binary", importPath: "gone", dir: t.TempDir()}}
	tests, unmapped := listTests(context.Background(), bins, 0, 1)
	if len(tests) != 0 || !strings.HasPrefix(unmapped["gone"], "listing its tests failed") {
		t.Fatalf("listTests = %+v, unmapped %v; want no tests and gone unmapped", tests, unmapped)
	}
}

func TestResolvePackagesCoverage(t *testing.T) {
	dir := setupTestProject(t)

	pkgs, err := resolvePackages(context.Background(), dir, []string{"testmod"}, "")
	if err != nil {
		t.Fatalf("resolvePackages: %v", err)
	}

	if len(pkgs) != 1 {
		t.Fatalf("expected 1 package, got %d", len(pkgs))
	}
	if pkgs[0].importPath != "testmod" {
		t.Errorf("importPath=%q, want testmod", pkgs[0].importPath)
	}
	if pkgs[0].dir == "" {
		t.Error("dir should not be empty")
	}
}

// TestResolvePackagesProbeFile: a rebuild probe changes a package's first
// production file, or its first test file when it has none.
func TestResolvePackagesProbeFile(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"go.mod":            "module probemod\n\ngo 1.26\n",
		"p/b.go":            "package p\n",
		"p/a.go":            "package p\n",
		"p/a_test.go":       "package p\n",
		"e2e/x_test.go":     "package e2e_test\n",
		"inner/i.go":        "package inner\n",
		"inner/i_test.go":   "package inner\n",
		"onlyx/x_test.go":   "package onlyx\n",
		"onlyext/z_test.go": "package onlyext_test\n",
	})
	pkgs, err := resolvePackages(context.Background(), dir, []string{"./..."}, "")
	if err != nil {
		t.Fatalf("resolvePackages: %v", err)
	}
	got := map[string]string{}
	for _, p := range pkgs {
		rel, _ := filepath.Rel(dir, p.probeFile)
		got[p.importPath] = filepath.ToSlash(rel)
	}
	want := map[string]string{"probemod/p": "p/a.go", "probemod/e2e": "e2e/x_test.go", "probemod/inner": "inner/i.go", "probemod/onlyext": "onlyext/z_test.go", "probemod/onlyx": "onlyx/x_test.go"}
	if !maps.Equal(got, want) {
		t.Errorf("probe files = %v, want %v", got, want)
	}
}

// TestMeasureRebuild times a real rebuild of a package whose test binary
// is already cached. The package's own sources are untouched: the changed
// copy, with the probe's comment appended, goes next to the test binary
// through an overlay, and the probe's own binary is removed. Both files
// are written readable, as go reads them.
func TestMeasureRebuild(t *testing.T) {
	dir := setupTestProject(t)
	binDir := t.TempDir()
	pkgs, err := resolvePackages(context.Background(), dir, []string{"testmod"}, "")
	if err != nil || len(pkgs) != 1 {
		t.Fatalf("resolvePackages = (%v, %v)", pkgs, err)
	}
	probe := pkgs[0].probeFile
	before, err := os.ReadFile(probe)
	if err != nil {
		t.Fatal(err)
	}
	orig := writeFileFunc
	t.Cleanup(func() { writeFileFunc = orig })
	var perms []os.FileMode
	writeFileFunc = func(name string, data []byte, perm os.FileMode) error {
		perms = append(perms, perm)
		return orig(name, data, perm)
	}
	cp := &compiledPkg{importPath: "testmod", dir: dir, binPath: filepath.Join(binDir, "testmod.test"), probeFile: probe}

	d, err := measureRebuild(context.Background(), dir, BuildOptions{TestFlags: []string{"-short", "-trimpath"}}, cp)
	if err != nil || d <= 0 {
		t.Fatalf("measureRebuild = (%v, %v), want a positive duration", d, err)
	}
	if after, _ := os.ReadFile(probe); !bytes.Equal(after, before) {
		t.Error("measureRebuild changed the package's source file")
	}
	changed, err := os.ReadFile(filepath.Join(binDir, filepath.Base(probe)))
	if err != nil || !bytes.HasPrefix(changed, before) || !bytes.Contains(changed[len(before):], []byte("// gomutants rebuild probe ")) {
		t.Errorf("changed copy = (%q, %v), want the source with the probe's comment appended", changed, err)
	}
	if _, err := os.Stat(filepath.Join(binDir, "probe.test")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("probe binary left behind: %v", err)
	}
	if want := []os.FileMode{0o644, 0o644}; !slices.Equal(perms, want) {
		t.Errorf("write modes = %v, want %v", perms, want)
	}
}

// TestMeasureRebuildErrors: every step that can fail fails the
// measurement with no duration — reading the file to change, writing the
// changed file and overlay, and the build itself.
func TestMeasureRebuildErrors(t *testing.T) {
	dir := setupTestProject(t)
	pkgs, err := resolvePackages(context.Background(), dir, []string{"testmod"}, "")
	if err != nil || len(pkgs) != 1 {
		t.Fatalf("resolvePackages = (%v, %v)", pkgs, err)
	}
	probe := pkgs[0].probeFile
	bin := func(d string) string { return filepath.Join(d, "testmod.test") }
	missingDir := filepath.Join(t.TempDir(), "missing")

	cases := []struct {
		name string
		cp   *compiledPkg
	}{
		{"no file to change", &compiledPkg{importPath: "testmod", binPath: bin(t.TempDir())}},
		{"unreadable file", &compiledPkg{importPath: "testmod", binPath: bin(t.TempDir()), probeFile: filepath.Join(missingDir, "x.go")}},
		{"unwritable directory", &compiledPkg{importPath: "testmod", binPath: bin(missingDir), probeFile: probe}},
		{"build fails", &compiledPkg{importPath: "testmod/does/not/exist", binPath: bin(t.TempDir()), probeFile: probe}},
	}
	for _, c := range cases {
		if d, err := measureRebuild(context.Background(), dir, BuildOptions{}, c.cp); err == nil || d != 0 {
			t.Errorf("%s: measureRebuild = (%v, %v), want (0, an error)", c.name, d, err)
		}
	}

	// A failed build's error wraps go's exit and carries what it printed.
	_, err = measureRebuild(context.Background(), dir, BuildOptions{}, cases[3].cp)
	var exitErr *exec.ExitError
	if _, stderr, _ := strings.Cut(fmt.Sprint(err), "\n"); !errors.As(err, &exitErr) || !strings.Contains(stderr, "testmod/does/not/exist") {
		t.Errorf("build fails: err = %v, want go's exit error and its output", err)
	}

	orig := writeFileFunc
	t.Cleanup(func() { writeFileFunc = orig })
	boom := errors.New("boom")
	writeFileFunc = func(string, []byte, os.FileMode) error { return boom }
	good := &compiledPkg{importPath: "testmod", binPath: bin(t.TempDir()), probeFile: probe}
	if d, err := measureRebuild(context.Background(), dir, BuildOptions{}, good); !errors.Is(err, boom) || d != 0 {
		t.Errorf("write fails: measureRebuild = (%v, %v), want (0, the write's error)", d, err)
	}
}

// TestMeasureRebuildForwardsTags: a package whose only file is
// tag-gated builds only when the tags reach the probe, given either as
// --tags or among the test flags.
func TestMeasureRebuildForwardsTags(t *testing.T) {
	dir := setupTaggedProject(t)
	cp := &compiledPkg{importPath: "testmod/only", binPath: filepath.Join(t.TempDir(), "only.test"), probeFile: filepath.Join(dir, "only", "only.go")}
	if _, err := measureRebuild(context.Background(), dir, BuildOptions{}, cp); err == nil {
		t.Error("no tags: want the build to fail")
	}
	for _, opts := range []BuildOptions{{Tags: "mytag"}, {TestFlags: []string{"-tags=mytag"}}} {
		if _, err := measureRebuild(context.Background(), dir, opts, cp); err != nil {
			t.Errorf("%+v: measureRebuild: %v", opts, err)
		}
	}
}

// TestCompileTestBinaryNoScratchDir: a test binary's directory that can't
// be made fails the compile with the error.
func TestCompileTestBinaryNoScratchDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	_, err := compileTestBinary(context.Background(), t.TempDir(), BuildOptions{TmpDir: missing}, resolvedPkg{importPath: "testmod"})
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want the missing directory's error", err)
	}
}

// TestTestDepsError: a go list that can't run is an error, tags and all.
func TestTestDepsError(t *testing.T) {
	if _, err := testDeps(context.Background(), filepath.Join(t.TempDir(), "missing"), "mytag", []string{"m/p"}); err == nil {
		t.Error("testDeps in a missing directory: want an error")
	}
}

// TestCheckPkgSkipsTimeout: a check run cut off by its timeout says so.
func TestCheckPkgSkipsTimeout(t *testing.T) {
	dir := writeModule(t, hangModule)
	cp := compileFixture(t, dir, "hangmod")
	t.Setenv("HANG_MAIN", "1")
	skipped := []testEntry{{name: "TestQuick", pkg: "hangmod", order: 0}}
	f, ok := checkPkgSkips(context.Background(), cp, slices.Clone(skipped), skipped, 200*time.Millisecond)
	if !ok || f.reason != "TestQuick skips when run alone, and running it after the tests listed before it failed: timed out after 200ms" {
		t.Errorf("checkPkgSkips = (%+v, %v), want the timeout as the reason", f, ok)
	}
}

// soloSkipModule's TestCheck skips unless TestSetup ran first, and
// TestAlways always skips. Run alone, both skip; in package order only
// TestAlways does.
var soloSkipModule = map[string]string{
	"go.mod": "module skipmod\n\ngo 1.26\n",
	"lib.go": "package skipmod\n\nvar ready bool\n\nfunc Setup() { ready = true }\n\nfunc Check() bool { return ready }\n",
	"lib_test.go": `package skipmod

import "testing"

func TestSetup(t *testing.T) { Setup() }

func TestCheck(t *testing.T) {
	if !ready {
		t.Skip("not set up")
	}
	if !Check() {
		t.Fatal("not ready")
	}
}

func TestAlways(t *testing.T) { t.Skip("never runs") }
`,
}

// TestBuildTestMapUnmapsOrderDependentSkips: a test that skips alone but
// not after the tests listed before it unmaps its package; a test that
// skips either way leaves it mapped.
func TestBuildTestMapUnmapsOrderDependentSkips(t *testing.T) {
	dir := writeModule(t, soloSkipModule)
	tm, err := BuildTestMap(context.Background(), dir, []string{"./..."}, BuildOptions{TmpDir: t.TempDir(), Workers: 2})
	if err != nil {
		t.Fatalf("BuildTestMap: %v", err)
	}
	got := tm.Unmapped()
	if len(got) != 1 || got[0].Reason != "TestCheck skips when run alone but not after the tests listed before it" {
		t.Errorf("Unmapped() = %+v, want skipmod with TestCheck's skip", got)
	}

	dir = writeModule(t, map[string]string{
		"go.mod":      "module skipmod\n\ngo 1.26\n",
		"lib.go":      "package skipmod\n\nfunc F() int { return 1 }\n",
		"lib_test.go": "package skipmod\n\nimport \"testing\"\n\nfunc TestF(t *testing.T) { F() }\n\nfunc TestAlways(t *testing.T) { t.Skip(\"never runs\") }\n",
	})
	tm, err = BuildTestMap(context.Background(), dir, []string{"./..."}, BuildOptions{TmpDir: t.TempDir(), Workers: 2})
	if err != nil {
		t.Fatalf("BuildTestMap: %v", err)
	}
	if got := tm.Unmapped(); len(got) != 0 {
		t.Errorf("a test that always skips: Unmapped() = %+v, want none", got)
	}
	if _, ok := tm.RebuildDuration("skipmod"); !ok {
		t.Error("RebuildDuration(skipmod) unmeasured, want the probe's measurement")
	}
}

func TestResolvePackagesFailure(t *testing.T) {
	_, err := resolvePackages(context.Background(), t.TempDir(), []string{"nonexistent/pkg"}, "")
	if err == nil {
		t.Fatal("expected error for nonexistent package")
	}
}

// setupTaggedProject builds a module whose root package has a normal
// Add/TestAdd pair plus a `//go:build mytag`-gated Tagged()/TestTagged
// pair, and a `testmod/only` subpackage whose single file is entirely
// behind the same constraint. Nothing tag-gated is visible to the go tool
// unless `-tags=mytag` is forwarded, which makes tag forwarding observable
// from every go-invoking helper.
func setupTaggedProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "only"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"go.mod":         "module testmod\n\ngo 1.26\n",
		"add.go":         "package testmod\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n",
		"add_test.go":    "package testmod\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"wrong\")\n\t}\n}\n",
		"tagged.go":      "//go:build mytag\n\npackage testmod\n\nfunc Tagged() int {\n\treturn 7\n}\n",
		"tagged_test.go": "//go:build mytag\n\npackage testmod\n\nimport \"testing\"\n\nfunc TestTagged(t *testing.T) {\n\tif Tagged() != 7 {\n\t\tt.Fatal(\"wrong\")\n\t}\n}\n",
		"only/only.go":   "//go:build mytag\n\npackage only\n\nfunc Only() int { return 1 }\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestResolvePackagesForwardsTags kills the same guard in resolvePackages:
// the testmod/only package — whose only file is build-constrained — can be
// resolved only when the tag reaches `go list`.
func TestResolvePackagesForwardsTags(t *testing.T) {
	dir := setupTaggedProject(t)

	if _, err := resolvePackages(context.Background(), dir, []string{"testmod/only"}, ""); err == nil {
		t.Error("no tags: testmod/only has no buildable files, resolvePackages should error")
	}
	pkgs, err := resolvePackages(context.Background(), dir, []string{"testmod/only"}, "mytag")
	if err != nil {
		t.Fatalf("tags=mytag: resolvePackages should succeed, got: %v", err)
	}
	if len(pkgs) != 1 || pkgs[0].importPath != "testmod/only" {
		t.Errorf("tags=mytag: want [testmod/only], got %+v", pkgs)
	}
}

// TestBuildTestMapForwardsTags is the end-to-end guard for the whole
// tags-aware pipeline (resolvePackages + compileTestBinary):
// coverage for the tag-gated Tagged() line is mapped only when the tag is
// forwarded, so it kills the `-tags=` STATEMENT_REMOVE in compileTestBinary
// too — without it the test binary excludes TestTagged and the line is
// never covered.
func TestBuildTestMapForwardsTags(t *testing.T) {
	dir := setupTaggedProject(t)

	var (
		without, with *TestMap
		err           error
	)
	runWithDeadline(t, 60*time.Second, func() {
		without, err = BuildTestMap(context.Background(), dir, []string{"testmod"}, BuildOptions{TmpDir: t.TempDir(), Workers: 2})
	})
	if err != nil {
		t.Fatalf("BuildTestMap (no tags): %v", err)
	}
	// tagged.go isn't compiled in without the tag, so its return line maps
	// to nothing.
	if got := without.TestsFor("testmod/tagged.go", 6); len(got) != 0 {
		t.Errorf("no tags: tagged.go:6 should have no covering tests, got %v", got)
	}

	runWithDeadline(t, 60*time.Second, func() {
		with, err = BuildTestMap(context.Background(), dir, []string{"testmod"}, BuildOptions{Tags: "mytag", TmpDir: t.TempDir(), Workers: 2})
	})
	if err != nil {
		t.Fatalf("BuildTestMap (tags): %v", err)
	}
	if got := with.TestsFor("testmod/tagged.go", 6); len(got) == 0 {
		t.Error("tags=mytag: TestTagged must cover tagged.go:6, got none — compileTestBinary dropped -tags=")
	}
}

// TestBuildTestMapAllCompilesFail pins the diagnostic when no package's
// test binary compiles: an error carrying the compiler's own output, which
// also kills STATEMENT_REMOVE on `cmd.Stderr = &stderr` in
// compileTestBinary (only stderr names the bad file).
func TestBuildTestMapAllCompilesFail(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"go.mod":      "module testmod\n\ngo 1.26\n",
		"bad.go":      "package testmod\n\nfunc Bad() { SYNTAX ERROR }\n",
		"bad_test.go": "package testmod\nimport \"testing\"\nfunc TestBad(t *testing.T) {}\n",
	})

	_, err := BuildTestMap(context.Background(), dir, []string{"testmod"}, BuildOptions{TmpDir: t.TempDir(), Workers: 1})
	if err == nil {
		t.Fatal("expected error for package with syntax error")
	}
	if !strings.Contains(err.Error(), "no test binary compiled") || !strings.Contains(err.Error(), "bad.go:3") {
		t.Errorf("error should report the failed compile with the compiler's output, got: %q", err)
	}
}
func TestBuildTestMapResolveError(t *testing.T) {
	dir := setupTestProject(t)

	// Stub resolvePackagesFunc to fail.
	origResolve := resolvePackagesFunc
	resolvePackagesFunc = func(ctx context.Context, projectDir string, patterns []string, _ string) ([]resolvedPkg, error) {
		return nil, fmt.Errorf("injected resolve error")
	}
	defer func() { resolvePackagesFunc = origResolve }()

	_, err := BuildTestMap(context.Background(), dir, []string{"testmod"}, BuildOptions{TmpDir: t.TempDir(), Workers: 1})
	if err == nil {
		t.Fatal("expected error from resolvePackages")
	}
}

func TestBuildTestMapCompileFailure(t *testing.T) {
	dir := setupTestProject(t)

	// Stub resolvePackagesFunc to return a package that won't compile.
	origResolve := resolvePackagesFunc
	resolvePackagesFunc = func(ctx context.Context, projectDir string, patterns []string, _ string) ([]resolvedPkg, error) {
		return []resolvedPkg{{importPath: "nonexistent/package", dir: projectDir}}, nil
	}
	defer func() { resolvePackagesFunc = origResolve }()

	// The only package's binary won't compile, so nothing can be mapped —
	// that must surface as an error the caller warns on, not an empty map
	// that silently disables per-test routing.
	tm, err := BuildTestMap(context.Background(), dir, []string{"testmod"}, BuildOptions{TmpDir: t.TempDir(), Workers: 1})
	if err == nil {
		t.Fatal("BuildTestMap should error when no test binary compiles")
	}
	if tm != nil {
		t.Errorf("expected nil map alongside the error, got %+v", tm)
	}
}

func TestBuildTestMapNoTestsPkg(t *testing.T) {
	// Package with no tests — go test -c produces no binary.
	dir := t.TempDir()
	files := map[string]string{
		"go.mod": "module testmod\n\ngo 1.26\n",
		"lib.go": "package testmod\n\nfunc Add(a, b int) int { return a + b }\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tmpDir := t.TempDir()

	tm, err := BuildTestMap(context.Background(), dir, []string{"testmod"}, BuildOptions{TmpDir: tmpDir, Workers: 1})
	if err != nil {
		t.Fatalf("BuildTestMap: %v", err)
	}
	// No tests found, map should be empty.
	if tm == nil {
		t.Fatal("TestMap should not be nil")
	}
}

// TestRunCompiledTestStaleProfileNotReturned kills BRANCH_IF on
// runCompiledTest's `if err := cmd.Run(); err != nil { return nil }`.
// We pre-seed the profile path with valid coverage data, then hand in a
// nonexistent binary so cmd.Run fails. Under the original, the failing
// Run short-circuits and returns nil. Under mutation, execution falls
// through to parseFileFunc, which happily reads the pre-seeded stale
// data and returns its blocks.
func TestRunCompiledTestStaleProfileNotReturned(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := context.Background()

	profilePath := filepath.Join(tmpDir, "stale.cov")
	staleProfile := "mode: set\ntestmod/fake.go:1.1,2.2 1 1\n"
	if err := os.WriteFile(profilePath, []byte(staleProfile), 0o644); err != nil {
		t.Fatal(err)
	}
	cp := &compiledPkg{
		binPath:    "/nonexistent/absolutely/not/a/binary",
		importPath: "testmod",
		dir:        tmpDir,
	}
	blocks, _, _, _ := runCompiledTest(ctx, cp, "TestAnything", profilePath, 0)
	if blocks != nil {
		t.Errorf("cmd.Run failed but got %d blocks from stale profile — BRANCH_IF on the err check lets it through", len(blocks))
	}
}

// TestProcessWorkContextCancelledSkipsWork kills BRANCH_IF on processWork's
// `if ctx.Err() != nil { return }`. Under the original, a cancelled
// context makes the worker return before reading the next entry, so the
// pre-filled work item is left unprocessed. Under mutation, the return
// is elided and the worker falls through to the test-run path, which
// would actually execute our real compiled binary and push a result.
func TestProcessWorkContextCancelledSkipsWork(t *testing.T) {
	dir := setupTestProject(t)
	tmpDir := t.TempDir()

	binPath := filepath.Join(tmpDir, "testbin.test")
	cmd := exec.CommandContext(context.Background(), "go", "test", "-c", "-o", binPath, "-cover", "testmod")
	cmd.Dir = dir
	if err := cmd.Run(); err != nil {
		t.Fatalf("go test -c: %v", err)
	}
	cp := &compiledPkg{binPath: binPath, importPath: "testmod", dir: dir}
	pkgBins := map[string]*compiledPkg{"testmod": cp}

	work := make(chan testEntry, 1)
	results := make(chan testCoverage, 1)
	work <- testEntry{name: "TestAdd", pkg: "testmod"}
	close(work)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel before processWork reads

	processWork(ctx, work, pkgBins, filepath.Join(tmpDir, "p.cov"), 0, nil, results)
	close(results)

	count := 0
	for range results {
		count++
	}
	if count != 0 {
		t.Errorf("processWork produced %d results on a cancelled ctx — BRANCH_IF on ctx.Err() check lets work through", count)
	}
}

// compileTestmodBinary sets up the standard testmod project, resolves it,
// and compiles its test binary, returning the context, the compiledPkg,
// and the tmpDir. Shared by the runCompiledTest tests below, which
// otherwise repeat this setup verbatim.
func compileTestmodBinary(t *testing.T) (context.Context, *compiledPkg, string) {
	t.Helper()
	dir := setupTestProject(t)
	tmpDir := t.TempDir()
	ctx := context.Background()

	pkgs, err := resolvePackages(ctx, dir, []string{"testmod"}, "")
	if err != nil {
		t.Fatalf("resolvePackages: %v", err)
	}

	binPath := filepath.Join(tmpDir, "testbin.test")
	cmd := exec.CommandContext(ctx, "go", "test", "-c", "-o", binPath, "-cover", "testmod")
	cmd.Dir = dir
	if err := cmd.Run(); err != nil {
		t.Fatalf("go test -c: %v", err)
	}

	return ctx, &compiledPkg{binPath: binPath, importPath: "testmod", dir: pkgs[0].dir}, tmpDir
}

func TestRunCompiledTestFailure(t *testing.T) {
	dir := setupTestProject(t)
	tmpDir := t.TempDir()
	ctx := context.Background()

	pkgs, err := resolvePackages(ctx, dir, []string{"testmod"}, "")
	if err != nil {
		t.Fatalf("resolvePackages: %v", err)
	}

	// Use a non-existent binary path — cmd.Run will fail.
	cp := &compiledPkg{
		binPath:    "/nonexistent/binary",
		importPath: "testmod",
		dir:        pkgs[0].dir,
	}

	profilePath := filepath.Join(tmpDir, "test.cov")
	blocks, dur, skipped, err := runCompiledTest(ctx, cp, "TestAdd", profilePath, 0)
	if blocks != nil || skipped {
		t.Errorf("expected nil blocks and no skip for failed test binary, got %d blocks, skipped %v", len(blocks), skipped)
	}
	if err == nil || !strings.HasPrefix(err.Error(), "TestAdd failed when run alone: ") || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want the failed-alone reason wrapping the start error", err)
	}
	// A failed run still reports its time: the mutant runs will spend it.
	if dur <= 0 {
		t.Errorf("duration = %v, want the failed run's wall time", dur)
	}
}

func TestRunCompiledTestParseError(t *testing.T) {
	ctx, cp, tmpDir := compileTestmodBinary(t)

	// Stub parseFileFunc to return an error.
	origParse := parseFileFunc
	parseErr := errors.New("injected parse error")
	parseFileFunc = func(path string) (*Profile, error) {
		return nil, parseErr
	}
	defer func() { parseFileFunc = origParse }()

	profilePath := filepath.Join(tmpDir, "test.cov")
	blocks, dur, skipped, err := runCompiledTest(ctx, cp, "TestAdd", profilePath, 0)
	if blocks != nil || skipped {
		t.Errorf("expected nil blocks and no skip when ParseFile fails, got %d blocks, skipped %v", len(blocks), skipped)
	}
	if err == nil || err.Error() != "TestAdd: reading its coverage profile: injected parse error" || !errors.Is(err, parseErr) {
		t.Errorf("err = %v, want the unreadable-profile reason wrapping the parse error", err)
	}
	if dur <= 0 {
		t.Errorf("duration = %v, want the run's wall time", dur)
	}
}

func TestRunCompiledTestBadProfile(t *testing.T) {
	ctx, cp, tmpDir := compileTestmodBinary(t)

	// Use a directory as the profile path — go test will fail to write to it.
	profileDir := filepath.Join(tmpDir, "profdir")
	os.MkdirAll(profileDir, 0o755)
	blocks, _, _, _ := runCompiledTest(ctx, cp, "TestAdd", profileDir, 0)
	// cmd.Run fails because -test.coverprofile can't write to a directory.
	if blocks != nil {
		t.Logf("blocks=%d (expected nil or empty)", len(blocks))
	}
}

func TestRunCompiledTest(t *testing.T) {
	ctx, cp, tmpDir := compileTestmodBinary(t)

	profilePath := filepath.Join(tmpDir, "test.cov")
	blocks, dur, _, err := runCompiledTest(ctx, cp, "TestAdd", profilePath, 0)
	if len(blocks) == 0 || err != nil {
		t.Errorf("expected coverage blocks and no error from TestAdd, got %d blocks, err %v", len(blocks), err)
	}
	if dur <= 0 {
		t.Errorf("expected positive duration from runCompiledTest, got %v", dur)
	}

	// Running a non-existent test should return nil/empty blocks.
	blocks, _, _, _ = runCompiledTest(ctx, cp, "TestNonExistent", profilePath, 0)
	_ = blocks
}

// orderModule's TestNeedsSetup passes only after TestSetup has run, as in
// a suite that shares state between tests.
var orderModule = map[string]string{
	"go.mod": "module ordermod\n\ngo 1.26\n",
	"lib.go": "package ordermod\n\nfunc F() int { return 1 }\n",
	"lib_test.go": `package ordermod

import "testing"

var ready bool

func TestSetup(t *testing.T) { ready = F() == 1 }

func TestNeedsSetup(t *testing.T) {
	if !ready {
		t.Fatal("TestSetup has not run")
	}
}
`,
}

// TestBuildTestMapUnmapsOrderDependentPackage is the end-to-end gate for a
// test that fails when run alone: it has no coverage, so routing mutants
// to the package's other tests could skip it. The package is unmapped with
// the reason instead, and the test that passes alone is still indexed.
func TestBuildTestMapUnmapsOrderDependentPackage(t *testing.T) {
	dir := writeModule(t, orderModule)

	tm, err := BuildTestMap(context.Background(), dir, []string{"./..."}, BuildOptions{TmpDir: t.TempDir(), Workers: 2})
	if err != nil {
		t.Fatalf("BuildTestMap: %v", err)
	}
	got := tm.Unmapped()
	if len(got) != 1 || got[0].ImportPath != "ordermod" || got[0].Dir != dir ||
		!strings.HasPrefix(got[0].Reason, "TestNeedsSetup failed when run alone") {
		t.Errorf("Unmapped() = %+v, want ordermod unmapped by TestNeedsSetup", got)
	}
	if tests := tm.TestsFor("ordermod/lib.go", 3); !slices.Equal(tests, []string{"TestSetup"}) {
		t.Errorf("TestsFor(lib.go:3) = %v, want [TestSetup]", tests)
	}
}

// flagModule's TestMain refuses to run without the custom -need flag, and
// TestShort fails unless -short is set: both pass only when the user's
// test flags reach every run of the binary.
var flagModule = map[string]string{
	"go.mod": "module flagmod\n\ngo 1.26\n",
	"lib.go": "package flagmod\n\nfunc F() int { return 1 }\n",
	"lib_test.go": `package flagmod

import (
	"flag"
	"fmt"
	"os"
	"testing"
)

var need = flag.Bool("need", false, "required")

func TestMain(m *testing.M) {
	flag.Parse()
	if !*need {
		fmt.Fprintln(os.Stderr, "missing -need")
		os.Exit(3)
	}
	os.Exit(m.Run())
}

func TestShort(t *testing.T) {
	F()
	if !testing.Short() {
		t.Fatal("not short")
	}
}
`,
}

// TestBuildTestMapForwardsTestFlags is the end-to-end gate for test flags:
// -short and a custom flag after -args reach the listing and the per-test
// run, so the package maps normally. Without them it can't be listed.
func TestBuildTestMapForwardsTestFlags(t *testing.T) {
	dir := writeModule(t, flagModule)

	tm, err := BuildTestMap(context.Background(), dir, []string{"./..."}, BuildOptions{TmpDir: t.TempDir(), Workers: 1, TestFlags: []string{"-short", "-args", "-need"}})
	if err != nil {
		t.Fatalf("BuildTestMap: %v", err)
	}
	if got := tm.Unmapped(); len(got) != 0 {
		t.Errorf("Unmapped() = %+v, want none with the flags forwarded", got)
	}
	if tests := tm.TestsFor("flagmod/lib.go", 3); !slices.Equal(tests, []string{"TestShort"}) {
		t.Errorf("TestsFor(lib.go:3) = %v, want [TestShort]", tests)
	}

	// A custom flag needs no -args: `go test` hands it to the binary, and
	// it must not reach `go test -c`, which rejects it. -covermode makes go
	// test pass a coverage directory in its $WORK, which the binary
	// can't write to.
	tm, err = BuildTestMap(context.Background(), dir, []string{"./..."}, BuildOptions{TmpDir: t.TempDir(), Workers: 1, TestFlags: []string{"-covermode=atomic", "-need", "-short"}})
	if err != nil {
		t.Fatalf("BuildTestMap with a custom flag: %v", err)
	}
	if got := tm.Unmapped(); len(got) != 0 {
		t.Errorf("custom flag: Unmapped() = %+v, want none", got)
	}
	if tests := tm.TestsFor("flagmod/lib.go", 3); !slices.Equal(tests, []string{"TestShort"}) {
		t.Errorf("custom flag: TestsFor(lib.go:3) = %v, want [TestShort]", tests)
	}

	tm, err = BuildTestMap(context.Background(), dir, []string{"./..."}, BuildOptions{TmpDir: t.TempDir(), Workers: 1})
	if err != nil {
		t.Fatalf("BuildTestMap without flags: %v", err)
	}
	if got := tm.Unmapped(); len(got) != 1 || !strings.Contains(got[0].Reason, "missing -need") {
		t.Errorf("without flags: Unmapped() = %+v, want flagmod unlistable", got)
	}
}

// TestCompileTestBinaryForwardsBuildFlags: a build flag among the test
// flags reaches `go test -c` — here -tags, which is the only way the
// tag-gated TestTagged gets compiled in — past a custom flag that would
// fail the compile.
func TestCompileTestBinaryForwardsBuildFlags(t *testing.T) {
	dir := setupTaggedProject(t)
	cp, err := compileTestBinary(context.Background(), dir, BuildOptions{TmpDir: t.TempDir(), TestFlags: []string{"-short", "-custom", "-tags", "mytag", "-args", "-race"}}, resolvedPkg{importPath: "testmod", dir: dir})
	if err != nil {
		t.Fatalf("compileTestBinary: %v", err)
	}
	names, err := listBinTests(context.Background(), cp, 0)
	if err != nil || !slices.Contains(names, "TestTagged") {
		t.Errorf("listBinTests = (%v, %v), want TestTagged compiled in", names, err)
	}
}

// TestRunCompiledTestArgsComeFirst: a positional argument among testArgs
// (anything after -args) ends the binary's flag parsing, so our own
// -test.run and -test.coverprofile must precede it or the profile is never
// written.
func TestRunCompiledTestArgsComeFirst(t *testing.T) {
	ctx, cp, tmpDir := compileTestmodBinary(t)
	cp.testArgs = []string{"positional"}

	blocks, _, _, err := runCompiledTest(ctx, cp, "TestAdd", filepath.Join(tmpDir, "pos.cov"), 0)
	if err != nil || len(blocks) == 0 {
		t.Errorf("runCompiledTest = (%d blocks, %v), want coverage with a positional test arg", len(blocks), err)
	}
}

// TestTestBinaryArgs reads real `go test -n` output: test flags come back
// rewritten for the binary, custom flags after -args verbatim, and go
// test's own bookkeeping arguments dropped.
func TestTestBinaryArgs(t *testing.T) {
	dir := setupTestProject(t)
	got, err := testBinaryArgs(context.Background(), dir, "", "testmod", []string{"-short", "-cpu", "2", "-race", "-args", "-foo"})
	if err != nil {
		t.Fatalf("testBinaryArgs: %v", err)
	}
	want := []string{"-test.paniconexit0", "-test.short=true", "-test.cpu=2", "-foo"}
	if !slices.Equal(got, want) {
		t.Errorf("testBinaryArgs = %q, want %q", got, want)
	}
}

// TestTestBinaryArgsForwardsTags: `go test -n` must see the tags, or a
// package whose files are all tag-gated doesn't exist for it. The error
// carries go's output.
func TestTestBinaryArgsForwardsTags(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"go.mod":    "module gated\n\ngo 1.26\n",
		"g.go":      "//go:build mytag\n\npackage gated\n",
		"g_test.go": "//go:build mytag\n\npackage gated\n\nimport \"testing\"\n\nfunc TestG(t *testing.T) {}\n",
	})
	_, err := testBinaryArgs(context.Background(), dir, "", "gated", []string{"-short"})
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || !strings.Contains(err.Error(), "build constraints exclude all Go files") {
		t.Errorf("no tags: err = %v, want go's exit error and its build-constraint message", err)
	}
	if got, err := testBinaryArgs(context.Background(), dir, "mytag", "gated", []string{"-short"}); err != nil || !slices.Contains(got, "-test.short=true") {
		t.Errorf("tags=mytag: (%q, %v), want -test.short=true", got, err)
	}
}

// TestTestDeps reads real `go list -test` output: an external test
// package's imports count, a package recompiled for the test is reported
// under its own import path, and an unrelated package is not linked.
func TestTestDeps(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"go.mod":          "module depmod\n\ngo 1.26\n",
		"p/p.go":          "package p\n\nfunc F() int { return 1 }\n",
		"p/p_test.go":     "package p\n\nimport \"testing\"\n\nfunc TestF(t *testing.T) { F() }\n",
		"q/q.go":          "package q\n\nfunc G() int { return 2 }\n",
		"e/e.go":          "package e\n",
		"e/e_ext_test.go": "package e_test\n\nimport (\n\t\"testing\"\n\n\t\"depmod/p\"\n)\n\nfunc TestE(t *testing.T) { p.F() }\n",
	})
	deps, err := testDeps(context.Background(), dir, "", []string{"depmod/e", "depmod/p"})
	if err != nil {
		t.Fatalf("testDeps: %v", err)
	}
	if !deps["depmod/e"]["depmod/p"] || !deps["depmod/e"]["depmod/e"] || deps["depmod/e"]["depmod/q"] {
		t.Errorf("deps[e] links p %v, e %v, q %v; want p and e only", deps["depmod/e"]["depmod/p"], deps["depmod/e"]["depmod/e"], deps["depmod/e"]["depmod/q"])
	}
	if !deps["depmod/p"]["depmod/p"] || deps["depmod/p"]["depmod/e"] {
		t.Errorf("deps[p] = %v, want p without e", deps["depmod/p"])
	}
	if len(deps) != 2 {
		t.Errorf("deps has %d packages, want e and p only", len(deps))
	}

	gated := writeModule(t, map[string]string{
		"go.mod":    "module gated\n\ngo 1.26\n",
		"g.go":      "//go:build mytag\n\npackage gated\n",
		"g_test.go": "//go:build mytag\n\npackage gated\n\nimport \"testing\"\n\nfunc TestG(t *testing.T) {}\n",
	})
	if deps, err := testDeps(context.Background(), gated, "mytag", []string{"gated"}); err != nil || !deps["gated"]["gated"] {
		t.Errorf("tags=mytag: testDeps = (%v, %v), want gated's links", deps, err)
	}
}

// TestBuildTestMapKeepsCollidingBinariesApart: m/api_v1 and m/api/v1
// flatten to the same file name, so a binary path derived from the import
// path alone let the second compile overwrite the first, and one
// package's tests ran under the other's import path and directory.
func TestBuildTestMapKeepsCollidingBinariesApart(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"go.mod":           "module m\n\ngo 1.26\n",
		"api_v1/a.go":      "package api_v1\n\nfunc A() int { return 1 }\n",
		"api_v1/a_test.go": "package api_v1\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) { A() }\n",
		"api/v1/b.go":      "package v1\n\nfunc B() int { return 2 }\n",
		"api/v1/b_test.go": "package v1\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) { B() }\n",
	})
	tm, err := BuildTestMap(context.Background(), dir, []string{"./..."}, BuildOptions{TmpDir: t.TempDir(), Workers: 2})
	if err != nil {
		t.Fatalf("BuildTestMap: %v", err)
	}
	if got := tm.Unmapped(); len(got) != 0 {
		t.Errorf("Unmapped() = %+v, want none", got)
	}
	for file, want := range map[string]TestRef{
		"m/api_v1/a.go": {Pkg: "m/api_v1", Name: "TestA"},
		"m/api/v1/b.go": {Pkg: "m/api/v1", Name: "TestB"},
	} {
		if got := tm.TestRefsFor(file, 3); !slices.Equal(got, []TestRef{want}) {
			t.Errorf("TestRefsFor(%s:3) = %+v, want [%+v]", file, got, want)
		}
	}
}

// orderSkipModule: TestB always skips, and TestC, listed after it, fails.
var orderSkipModule = map[string]string{
	"go.mod":      "module ordermod\n\ngo 1.26\n",
	"lib.go":      "package ordermod\n",
	"lib_test.go": "package ordermod\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n\nfunc TestB(t *testing.T) { t.Skip(\"always\") }\n\nfunc TestC(t *testing.T) { t.Fatal(\"fails\") }\n",
}

// TestCheckSoloSkipsRunsOnlyUpToTheSkip: the check runs the tests listed
// up to the last one that skipped alone — those before it included, even
// handed over out of order — and nothing after it, so a later test's
// failure doesn't read as the skip's.
func TestCheckSoloSkipsRunsOnlyUpToTheSkip(t *testing.T) {
	dir := writeModule(t, orderSkipModule)
	cp := compileFixture(t, dir, "ordermod")
	tests := []testEntry{
		{name: "TestC", pkg: "ordermod", order: 2},
		{name: "TestB", pkg: "ordermod", order: 1},
		{name: "TestA", pkg: "ordermod", order: 0},
		{name: "TestZ", pkg: "other", order: 0},
	}
	skips := map[string][]testEntry{"ordermod": {tests[1]}}
	if got := checkSoloSkips(context.Background(), map[string]*compiledPkg{"ordermod": cp}, tests, skips, 0, 1); len(got) != 0 {
		t.Errorf("checkSoloSkips = %+v, want no failure", got)
	}
}
