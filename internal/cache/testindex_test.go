package cache

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

func TestBuildTestIndex_IndexesTestFunctions(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a_test.go"), `package x

import "testing"

func TestAlpha(t *testing.T)        {}
func TestBeta(t *testing.T)         {}
func BenchmarkGamma(b *testing.B)   {}
func ExampleDelta()                  {}
func FuzzEpsilon(f *testing.F)      {}
func helperNotATest()                {}
`)
	mustWrite(t, filepath.Join(dir, "x.go"), `package x
func Foo() {}
`)

	ti := BuildTestIndex([]string{dir})

	for _, name := range []string{"TestAlpha", "TestBeta", "BenchmarkGamma", "ExampleDelta", "FuzzEpsilon"} {
		got := ti.FilesFor(name)
		if len(got) != 1 {
			t.Errorf("FilesFor(%q) = %v, want 1 file", name, got)
		}
	}
	if got := ti.FilesFor("helperNotATest"); got != nil {
		t.Errorf("non-test function indexed: %v", got)
	}
	if got := ti.FilesFor("Foo"); got != nil {
		t.Errorf("production function indexed: %v", got)
	}

	all := ti.AllInDir(dir)
	if len(all) != 1 || filepath.Base(all[0]) != "a_test.go" {
		t.Errorf("AllInDir(dir) = %v, want [a_test.go]", all)
	}
}

// TestBuildTestIndex_CrossPackageNameCollision asserts that the same
// test name declared in two packages maps to both files. This is the
// case the per-mutant tests_hash needs to handle: an edit to either
// declaring file must invalidate the cache entry.
func TestBuildTestIndex_CrossPackageNameCollision(t *testing.T) {
	root := t.TempDir()
	pkgA := filepath.Join(root, "a")
	pkgB := filepath.Join(root, "b")
	if err := os.MkdirAll(pkgA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(pkgB, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(pkgA, "x_test.go"), "package a\nimport \"testing\"\nfunc TestSame(t *testing.T) {}\n")
	mustWrite(t, filepath.Join(pkgB, "x_test.go"), "package b\nimport \"testing\"\nfunc TestSame(t *testing.T) {}\n")

	ti := BuildTestIndex([]string{pkgA, pkgB})
	got := ti.FilesFor("TestSame")
	if len(got) != 2 {
		t.Fatalf("FilesFor(TestSame) = %v, want 2 entries", got)
	}
	sort.Strings(got)
	wantA := filepath.Join(pkgA, "x_test.go")
	wantB := filepath.Join(pkgB, "x_test.go")
	if got[0] != wantA || got[1] != wantB {
		t.Errorf("FilesFor(TestSame) = %v, want [%s %s]", got, wantA, wantB)
	}
}

// TestBuildTestIndex_MalformedFileRecordedInDirOnly asserts that a
// _test.go that fails to parse still appears in AllInDir — the
// fallback hashing path must include it — but contributes no byName
// entries. Mutants resolving through it then fall back to the
// directory-wide list, which correctly captures the malformed file.
//
// The malformed file is engineered to parse just far enough that
// `parser.ParseFile` returns a *non-nil* *ast.File with a complete
// FuncDecl for TestPartial before encountering the syntax error.
// Without the `if perr != nil { continue }` guard, that partial AST
// would be walked and TestPartial would be (incorrectly) indexed —
// so this test also catches the BRANCH_IF mutation on that guard.
func TestBuildTestIndex_MalformedFileRecordedInDirOnly(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good_test.go")
	bad := filepath.Join(dir, "bad_test.go")
	mustWrite(t, good, "package x\nimport \"testing\"\nfunc TestOK(t *testing.T) {}\n")
	mustWrite(t, bad, `package x

import "testing"

func TestPartial(t *testing.T) {}

func {{{
`)

	ti := BuildTestIndex([]string{dir})

	if got := ti.FilesFor("TestOK"); len(got) != 1 || got[0] != good {
		t.Errorf("FilesFor(TestOK) = %v, want [%s]", got, good)
	}
	if got := ti.FilesFor("TestPartial"); got != nil {
		t.Errorf("TestPartial from a malformed file was indexed (parse-error guard not honored): %v", got)
	}
	all := ti.AllInDir(dir)
	if len(all) != 2 {
		t.Errorf("AllInDir = %v, want both files (incl. malformed)", all)
	}
}

func TestBuildTestIndex_NilSafe(t *testing.T) {
	var ti *TestIndex
	if got := ti.FilesFor("anything"); got != nil {
		t.Errorf("nil index FilesFor returned %v", got)
	}
	if got := ti.AllInDir("/nope"); got != nil {
		t.Errorf("nil index AllInDir returned %v", got)
	}
}

// TestCoveringFiles_IncludesHelperOnlyFile is the regression this method
// exists for. assert_test.go declares no test entry point, so it is absent
// from byName and no coverage-derived test name can ever resolve to it —
// yet TestAdjust's verdict depends on the assertion it holds. Resolving
// only through the covering names would leave an edit to that helper
// invisible to every hash dimension: it is not a production file (so
// pkg_hash skips it) and it declares no test (so FilesFor misses it).
func TestCoveringFiles_IncludesHelperOnlyFile(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a_test.go"), `package x

import "testing"

func TestAdjust(t *testing.T) { mustAdjust(t, 1, 2) }
`)
	mustWrite(t, filepath.Join(dir, "assert_test.go"), `package x

import "testing"

func mustAdjust(t *testing.T, in, want int) {}
`)

	ti := BuildTestIndex([]string{dir})

	if got := ti.FilesFor("mustAdjust"); got != nil {
		t.Fatalf("precondition failed: helper indexed by name as %v", got)
	}

	got := ti.CoveringFiles(dir, []string{"TestAdjust"}, false)
	var names []string
	for _, f := range got {
		names = append(names, filepath.Base(f))
	}
	sort.Strings(names)
	if len(names) != 2 || names[0] != "a_test.go" || names[1] != "assert_test.go" {
		t.Errorf("CoveringFiles = %v, want both a_test.go and assert_test.go — a "+
			"helper-only test file gates reuse but resolves through no test name", names)
	}
}

// TestCoveringFiles_AddsCrossPackageDeclarers asserts the other half: a
// covering test declared outside the mutant's package (what -coverpkg /
// --integration produces) is added to the local set, not substituted for it.
//
// crossPkg is false here on purpose: the declaring file itself is added
// whatever the instrumentation scope. Only the rest of its package is
// gated — see TestCoveringFiles_CollisionKeepsForeignPackageOut.
func TestCoveringFiles_AddsCrossPackageDeclarers(t *testing.T) {
	target := t.TempDir()
	importer := t.TempDir()
	mustWrite(t, filepath.Join(target, "unit_test.go"), `package x

import "testing"

func TestUnit(t *testing.T) {}
`)
	mustWrite(t, filepath.Join(importer, "e2e_test.go"), `package y

import "testing"

func TestEndToEnd(t *testing.T) {}
`)

	ti := BuildTestIndex([]string{target, importer})

	got := ti.CoveringFiles(target, []string{"TestEndToEnd"}, false)
	if len(got) != 2 {
		t.Fatalf("CoveringFiles = %v, want the local test file plus the cross-package declarer", got)
	}
	var sawLocal, sawRemote bool
	for _, f := range got {
		switch filepath.Dir(f) {
		case target:
			sawLocal = true
		case importer:
			sawRemote = true
		}
	}
	if !sawLocal || !sawRemote {
		t.Errorf("CoveringFiles = %v, want one file from each package (local=%v remote=%v)",
			got, sawLocal, sawRemote)
	}
}

// TestCoveringFiles_DedupesLocalDeclarer covers the seen-set: a covering
// test declared in the mutant's own package is already in the local half,
// and must not be hashed twice. Duplicate paths would not break soundness
// but would make tests_hash depend on the coverage map's name ordering.
func TestCoveringFiles_DedupesLocalDeclarer(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a_test.go"), `package x

import "testing"

func TestAlpha(t *testing.T) {}
`)

	ti := BuildTestIndex([]string{dir})

	if got := ti.CoveringFiles(dir, []string{"TestAlpha", "TestAlpha"}, false); len(got) != 1 {
		t.Errorf("CoveringFiles = %v, want 1 file", got)
	}
}

// TestCoveringFiles_UnknownNamesKeepLocalSet asserts that a coverage map
// naming tests the index never saw (a deleted test, a name from a package
// outside the scanned closure) degrades to the local set rather than to
// nothing — an empty covering set would hash the same for every mutant in
// the package and hand back stale verdicts.
func TestCoveringFiles_UnknownNamesKeepLocalSet(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a_test.go"), `package x

import "testing"

func TestAlpha(t *testing.T) {}
`)

	ti := BuildTestIndex([]string{dir})

	if got := ti.CoveringFiles(dir, []string{"TestVanished"}, false); len(got) != 1 {
		t.Errorf("CoveringFiles = %v, want the package's own test file", got)
	}
}

// TestCoveringFiles_UnknownDirIsEmpty asserts a directory the index never
// scanned yields no files, and that CoveringFiles is nil-receiver safe.
func TestCoveringFiles_UnknownDirIsEmpty(t *testing.T) {
	ti := BuildTestIndex(nil)
	if got := ti.CoveringFiles("/nope", []string{"TestAlpha"}, true); got != nil {
		t.Errorf("CoveringFiles on unscanned dir = %v, want nil", got)
	}

	var nilIdx *TestIndex
	if got := nilIdx.CoveringFiles("/nope", []string{"TestAlpha"}, true); got != nil {
		t.Errorf("nil index CoveringFiles = %v, want nil", got)
	}
}

// TestCoveringFiles_IncludesForeignPackageSources is the --integration
// regression. Mutants live in target package T while the covering test
// lives in importer R, and pkg_hash only ever hashes T — so R's non-test
// sources, which is most of what an end-to-end test is made of, sat in no
// dimension of the key. Loosen R's fixture and T's mutant replays KILLED
// after it started surviving.
//
// The stray notes.txt pins the .go filter: R's non-source files are not
// package inputs and must stay out of the hash.
func TestCoveringFiles_IncludesForeignPackageSources(t *testing.T) {
	target := t.TempDir()
	importer := t.TempDir()
	mustWrite(t, filepath.Join(target, "unit_test.go"), `package x

import "testing"

func TestUnit(t *testing.T) {}
`)
	mustWrite(t, filepath.Join(importer, "e2e_test.go"), `package y

import "testing"

func TestEndToEnd(t *testing.T) {}
`)
	mustWrite(t, filepath.Join(importer, "fixtures.go"), `package y

const Want = 42
`)
	mustWrite(t, filepath.Join(importer, "notes.txt"), "not a package input\n")

	ti := BuildTestIndex([]string{target, importer})

	got := ti.CoveringFiles(target, []string{"TestEndToEnd"}, true)
	var names []string
	for _, f := range got {
		names = append(names, filepath.Base(f))
	}
	sort.Strings(names)

	want := []string{"e2e_test.go", "fixtures.go", "unit_test.go"}
	if len(names) != len(want) {
		t.Fatalf("CoveringFiles = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("CoveringFiles = %v, want %v", names, want)
		}
	}
}

// TestCoveringFiles_IncludesForeignHelperOnlyFile is the helper-only
// argument applied where it was missing: to the *foreign* package. Under
// --integration the mutant lives in T and TestEndToEnd lives in importer R,
// whose assertion helper declares no test entry point — so BuildTestIndex
// files it under no name, and adding only R's declaring file leaves it in
// no dimension of the key. Loosen the assertion inside it and T's mutant
// replays KILLED after it started surviving, with every file the key does
// hash byte-identical.
func TestCoveringFiles_IncludesForeignHelperOnlyFile(t *testing.T) {
	target := t.TempDir()
	importer := t.TempDir()
	mustWrite(t, filepath.Join(target, "unit_test.go"), `package x

import "testing"

func TestUnit(t *testing.T) {}
`)
	mustWrite(t, filepath.Join(importer, "e2e_test.go"), `package y

import "testing"

func TestEndToEnd(t *testing.T) { mustMatch(t, 1, 1) }
`)
	mustWrite(t, filepath.Join(importer, "assert_test.go"), `package y

import "testing"

func mustMatch(t *testing.T, got, want int) {}
`)

	ti := BuildTestIndex([]string{target, importer})

	if got := ti.FilesFor("mustMatch"); got != nil {
		t.Fatalf("precondition failed: foreign helper indexed by name as %v", got)
	}

	got := ti.CoveringFiles(target, []string{"TestEndToEnd"}, true)
	var names []string
	for _, f := range got {
		names = append(names, filepath.Base(f))
	}
	sort.Strings(names)

	want := []string{"assert_test.go", "e2e_test.go", "unit_test.go"}
	if !slices.Equal(names, want) {
		t.Errorf("CoveringFiles = %v, want %v — the importing package's "+
			"helper-only test file decides the mutant's verdict too", names, want)
	}
}

// TestCoveringFiles_CollisionKeepsForeignPackageOut pins the crossPkg gate.
// TestsFor projects package context out of the covering names, so a name
// two packages share resolves to both declaring files; without an
// instrumentation scope wider than the package under test, the foreign one
// cannot describe real coverage. Expanding it anyway would fold that
// package's whole source into this mutant's tests_hash, so an edit anywhere
// in it would invalidate verdicts it has nothing to do with — TestAdd alone
// is declared in five packages of this repository.
func TestCoveringFiles_CollisionKeepsForeignPackageOut(t *testing.T) {
	mutant := t.TempDir()
	other := t.TempDir()
	mustWrite(t, filepath.Join(mutant, "a_test.go"), `package x

import "testing"

func TestAdd(t *testing.T) {}
`)
	mustWrite(t, filepath.Join(other, "b_test.go"), `package y

import "testing"

func TestAdd(t *testing.T) {}
`)
	mustWrite(t, filepath.Join(other, "helper_test.go"), `package y

func helper() {}
`)
	mustWrite(t, filepath.Join(other, "prod.go"), "package y\n\nfunc Foo() {}\n")

	ti := BuildTestIndex([]string{mutant, other})

	got := ti.CoveringFiles(mutant, []string{"TestAdd"}, false)
	var names []string
	for _, f := range got {
		names = append(names, filepath.Base(f))
	}
	sort.Strings(names)

	// The colliding declarer stays in (one file, conservative direction);
	// the rest of its package must not follow it.
	want := []string{"a_test.go", "b_test.go"}
	if !slices.Equal(names, want) {
		t.Errorf("CoveringFiles = %v, want %v — a shared test name must not drag "+
			"an unrelated package's sources into tests_hash", names, want)
	}

	// Same inputs under integration: now the foreign package comes along,
	// because the name can describe real cross-package coverage.
	if got := ti.CoveringFiles(mutant, []string{"TestAdd"}, true); len(got) != 4 {
		t.Errorf("CoveringFiles(crossPkg=true) = %v, want all four files", got)
	}
}

// TestCoveringFiles_ExcludesOwnPackageSources pins the `dir != pkgDir`
// guard. The mutant's own production files are pkg_hash's job; re-adding
// them here would make an unrelated production edit invalidate the tests
// dimension as well, for no extra soundness.
func TestCoveringFiles_ExcludesOwnPackageSources(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a_test.go"), `package x

import "testing"

func TestAlpha(t *testing.T) {}
`)
	mustWrite(t, filepath.Join(dir, "prod.go"), "package x\n\nfunc Foo() {}\n")

	ti := BuildTestIndex([]string{dir})

	got := ti.CoveringFiles(dir, []string{"TestAlpha"}, true)
	if len(got) != 1 || filepath.Base(got[0]) != "a_test.go" {
		t.Errorf("CoveringFiles = %v, want only a_test.go — the mutant's own "+
			"production files belong to pkg_hash", got)
	}
}

// TestPackageFiles: a package whose whole suite runs contributes its test
// files and its production sources, and nothing that isn't Go.
func TestPackageFiles(t *testing.T) {
	importer := t.TempDir()
	mustWrite(t, filepath.Join(importer, "e2e_test.go"), "package y\n\nimport \"testing\"\n\nfunc TestEndToEnd(t *testing.T) {}\n")
	mustWrite(t, filepath.Join(importer, "fixtures.go"), "package y\n\nconst Want = 42\n")
	mustWrite(t, filepath.Join(importer, "notes.txt"), "not a package input\n")

	ti := BuildTestIndex([]string{importer})
	var names []string
	for _, f := range ti.PackageFiles(importer) {
		names = append(names, filepath.Base(f))
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "e2e_test.go,fixtures.go" {
		t.Errorf("PackageFiles = %v, want [e2e_test.go fixtures.go]", names)
	}
	if got := ti.PackageFiles(t.TempDir()); got != nil {
		t.Errorf("PackageFiles(unindexed dir) = %v, want nil", got)
	}
}
