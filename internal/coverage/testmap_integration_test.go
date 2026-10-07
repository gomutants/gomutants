package coverage

import (
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
		blocks []Block
		dur    time.Duration
		err    error
	)
	runWithDeadline(t, 30*time.Second, func() {
		blocks, dur, err = runCompiledTest(context.Background(), cp, "TestHang", filepath.Join(t.TempDir(), "hang.cov"), 200*time.Millisecond)
	})
	if blocks != nil {
		t.Errorf("a killed run returned %d blocks, want none", len(blocks))
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
	if err == nil {
		t.Error("listing a binary whose TestMain hangs returned no error")
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
	runCompiledTestFunc = func(context.Context, *compiledPkg, string, string, time.Duration) ([]Block, time.Duration, error) {
		atomic.AddInt32(&ran, 1)
		cancel()
		return nil, time.Millisecond, nil
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
// package's import path — the same-named TestShared once per package —
// and that overlapping patterns don't list a test twice.
func TestListTestsKeysByImportPath(t *testing.T) {
	dir := writeTwoPkgModule(t)
	want := []testEntry{
		{name: "TestOnlyA", pkg: "twopkg/a"},
		{name: "TestOnlySub", pkg: "twopkg/sub"},
		{name: "TestShared", pkg: "twopkg/a"},
		{name: "TestShared", pkg: "twopkg/sub"},
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
	blocks, _, _ := runCompiledTest(ctx, cp, "TestAnything", profilePath, 0)
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

	processWork(ctx, work, pkgBins, tmpDir, 0, 0, results)
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
	blocks, dur, err := runCompiledTest(ctx, cp, "TestAdd", profilePath, 0)
	if blocks != nil {
		t.Errorf("expected nil blocks for failed test binary, got %d", len(blocks))
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
	blocks, dur, err := runCompiledTest(ctx, cp, "TestAdd", profilePath, 0)
	if blocks != nil {
		t.Errorf("expected nil blocks when ParseFile fails, got %d", len(blocks))
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
	blocks, _, _ := runCompiledTest(ctx, cp, "TestAdd", profileDir, 0)
	// cmd.Run fails because -test.coverprofile can't write to a directory.
	if blocks != nil {
		t.Logf("blocks=%d (expected nil or empty)", len(blocks))
	}
}

func TestRunCompiledTest(t *testing.T) {
	ctx, cp, tmpDir := compileTestmodBinary(t)

	profilePath := filepath.Join(tmpDir, "test.cov")
	blocks, dur, err := runCompiledTest(ctx, cp, "TestAdd", profilePath, 0)
	if len(blocks) == 0 || err != nil {
		t.Errorf("expected coverage blocks and no error from TestAdd, got %d blocks, err %v", len(blocks), err)
	}
	if dur <= 0 {
		t.Errorf("expected positive duration from runCompiledTest, got %v", dur)
	}

	// Running a non-existent test should return nil/empty blocks.
	blocks, _, _ = runCompiledTest(ctx, cp, "TestNonExistent", profilePath, 0)
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
// tag-gated TestTagged gets compiled in.
func TestCompileTestBinaryForwardsBuildFlags(t *testing.T) {
	dir := setupTaggedProject(t)
	cp, err := compileTestBinary(context.Background(), dir, BuildOptions{TmpDir: t.TempDir(), TestFlags: []string{"-tags=mytag"}}, resolvedPkg{importPath: "testmod", dir: dir})
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

	blocks, _, err := runCompiledTest(ctx, cp, "TestAdd", filepath.Join(tmpDir, "pos.cov"), 0)
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
