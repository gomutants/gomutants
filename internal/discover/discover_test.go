package discover

import (
	"context"
	"fmt"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/szhekpisov/gomutants/internal/coverage"
	"github.com/szhekpisov/gomutants/internal/mutator"
)

// runWithDeadline runs fn in a goroutine and fails the test if it doesn't
// return within d. Used to catch mutations that turn bounded loops into
// infinite loops (e.g. dropping `prefix = prefix[:slash]` in
// longestCommonPrefix, or skipping the early-return on a JSON decode error
// so the decoder spins on the same bad token forever).
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

func TestDiscover(t *testing.T) {
	// Create a temp Go source file.
	dir := t.TempDir()
	src := `package testpkg

func Add(a, b int) int {
	return a + b
}
`
	if err := os.WriteFile(filepath.Join(dir, "add.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	pkgs := []Package{
		{
			Dir:        dir,
			ImportPath: "example.com/test/testpkg",
			GoFiles:    []string{"add.go"},
		},
	}

	reg := mutator.NewRegistry()
	fset := token.NewFileSet()
	mutants := Discover(fset, pkgs, reg.EnabledMutators([]string{"ARITHMETIC_BASE"}, nil), dir, "example.com/test").Mutants

	// "a + b" should produce at least 1 ARITHMETIC_BASE mutant.
	if len(mutants) == 0 {
		t.Fatal("expected at least 1 mutant")
	}

	m := mutants[0]
	if m.ID != 1 {
		t.Errorf("ID=%d, want 1", m.ID)
	}
	if m.Type != mutator.ArithmeticBase {
		t.Errorf("Type=%v, want ARITHMETIC_BASE", m.Type)
	}
	if m.Status != mutator.StatusPending {
		t.Errorf("Status=%v, want PENDING", m.Status)
	}
	if m.File != filepath.Join(dir, "add.go") {
		t.Errorf("File=%q", m.File)
	}
	if m.Pkg != "example.com/test/testpkg" {
		t.Errorf("Pkg=%q", m.Pkg)
	}
	if m.Original != "+" || m.Replacement != "-" {
		t.Errorf("mutation: %q→%q", m.Original, m.Replacement)
	}
}

func TestDiscoverUnparseableFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.go"), []byte("not valid go"), 0o644); err != nil {
		t.Fatal(err)
	}

	pkgs := []Package{
		{Dir: dir, ImportPath: "example.com/bad", GoFiles: []string{"bad.go"}},
	}

	reg := mutator.NewRegistry()
	fset := token.NewFileSet()
	// Should not panic — unparseable files are skipped.
	mutants := Discover(fset, pkgs, reg.Mutators(), dir, "example.com").Mutants
	if len(mutants) != 0 {
		t.Errorf("expected 0 mutants from unparseable file, got %d", len(mutants))
	}
}

// TestDiscoverPartiallyInvalidFileSkips kills BRANCH_IF on the `if err != nil`
// body of parseFile: when parser.ParseFile returns an error together with a
// partial AST (common for recoverable syntax errors), the original code
// returns early so Discover skips the file. Under BRANCH_IF the early-return
// is elided; parseFile yields (src, partialFile, nil) and Discover then
// walks the partial AST, producing mutants from whatever valid expressions
// the parser recovered — e.g., the "1 + 2" here would surface as an
// ARITHMETIC_BASE candidate.
func TestDiscoverPartiallyInvalidFileSkips(t *testing.T) {
	dir := t.TempDir()
	// First two lines parse cleanly; the third line is garbage and makes
	// parser.ParseFile return an error — but the partial AST still contains
	// F() with its `1 + 2` binary expression.
	src := "package bad\nfunc F() int { return 1 + 2 }\nthis is garbage\n"
	if err := os.WriteFile(filepath.Join(dir, "bad.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	pkgs := []Package{
		{Dir: dir, ImportPath: "example.com/bad", GoFiles: []string{"bad.go"}},
	}
	fset := token.NewFileSet()
	reg := mutator.NewRegistry()
	mutants := Discover(fset, pkgs, reg.EnabledMutators([]string{"ARITHMETIC_BASE"}, nil), dir, "example.com").Mutants
	if len(mutants) != 0 {
		t.Errorf("parseFile error must short-circuit discovery; got %d mutants from partial AST", len(mutants))
	}
}

func TestLongestCommonPrefix(t *testing.T) {
	tests := []struct {
		pkgs []Package
		want string
	}{
		{nil, ""},
		{
			[]Package{{ImportPath: "github.com/foo/bar/pkg/a"}},
			"github.com/foo/bar/pkg/a",
		},
		{
			[]Package{
				{ImportPath: "github.com/foo/bar/pkg/a"},
				{ImportPath: "github.com/foo/bar/pkg/b"},
			},
			"github.com/foo/bar/pkg",
		},
		{
			[]Package{
				{ImportPath: "github.com/foo/bar"},
				{ImportPath: "github.com/baz/qux"},
			},
			"github.com",
		},
		{
			[]Package{
				{ImportPath: "a/b/c"},
				{ImportPath: "x/y/z"},
			},
			"",
		},
	}
	for _, tc := range tests {
		var got string
		runWithDeadline(t, 5*time.Second, func() {
			got = longestCommonPrefix(tc.pkgs)
		})
		if got != tc.want {
			t.Errorf("longestCommonPrefix(%v) = %q, want %q", tc.pkgs, got, tc.want)
		}
	}
}

func TestFilterByCoverage(t *testing.T) {
	profile := &coverage.Profile{}
	// Use the exported IsCovered via the profile — it has no blocks, so nothing is covered.

	pkgs := []Package{
		{
			Dir:        "/abs/path/pkg",
			ImportPath: "example.com/mod/pkg",
			GoFiles:    []string{"file.go"},
		},
	}

	mutants := []mutator.Mutant{
		{ID: 1, File: "/abs/path/pkg/file.go", Line: 10, Col: 5, Status: mutator.StatusPending},
		{ID: 2, File: "/abs/path/pkg/file.go", Line: 20, Col: 3, Status: mutator.StatusPending},
		{ID: 3, File: "/unknown/file.go", Line: 5, Col: 1, Status: mutator.StatusPending},
		{ID: 4, File: "/abs/path/pkg/file.go", Line: 10, Col: 5, Status: mutator.StatusKilled}, // Not pending — skip.
	}

	FilterByCoverage(mutants, profile, pkgs, "example.com/mod")

	// mutants 1 and 2 should be NOT_COVERED (profile has no covered blocks).
	if mutants[0].Status != mutator.StatusNotCovered {
		t.Errorf("mutant 1: status=%v, want NOT_COVERED", mutants[0].Status)
	}
	if mutants[1].Status != mutator.StatusNotCovered {
		t.Errorf("mutant 2: status=%v, want NOT_COVERED", mutants[1].Status)
	}
	// mutant 3: unknown file → NOT_COVERED.
	if mutants[2].Status != mutator.StatusNotCovered {
		t.Errorf("mutant 3: status=%v, want NOT_COVERED", mutants[2].Status)
	}
	// mutant 4: already KILLED, should remain unchanged.
	if mutants[3].Status != mutator.StatusKilled {
		t.Errorf("mutant 4: status=%v, want KILLED (unchanged)", mutants[3].Status)
	}
}

func TestPreReadFiles(t *testing.T) {
	dir := t.TempDir()
	content := []byte("package p\n")
	if err := os.WriteFile(filepath.Join(dir, "a.go"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.go"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	pkgs := []Package{
		{Dir: dir, GoFiles: []string{"a.go", "b.go"}},
	}

	files, err := PreReadFiles(pkgs, nil)
	if err != nil {
		t.Fatalf("PreReadFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}
	if string(files[dir+"/a.go"]) != "package p\n" {
		t.Errorf("unexpected content for a.go: %q", files[dir+"/a.go"])
	}
}

func TestPreReadFilesMissing(t *testing.T) {
	pkgs := []Package{
		{Dir: "/nonexistent", GoFiles: []string{"missing.go"}},
	}

	_, err := PreReadFiles(pkgs, nil)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestResolvePackagesIntegration(t *testing.T) {
	dir := t.TempDir()
	// add.go imports a stdlib package (Imports), the in-package test imports
	// another (TestImports), and the external test a third (XTestImports), so
	// all three decoded import fields are populated and asserted below.
	files := map[string]string{
		"go.mod":        "module testmod\n\ngo 1.26\n",
		"add.go":        "package testmod\n\nimport \"errors\"\n\nvar ErrX = errors.New(\"x\")\n\nfunc Add(a, b int) int { return a + b }\n",
		"add_test.go":   "package testmod\n\nimport (\n\t\"strings\"\n\t\"testing\"\n)\n\nfunc TestAdd(t *testing.T) { _ = strings.ToUpper(\"x\") }\n",
		"add_x_test.go": "package testmod_test\n\nimport (\n\t\"bytes\"\n\t\"testing\"\n)\n\nfunc TestX(t *testing.T) { _ = bytes.NewBuffer(nil) }\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	pkgs, err := ResolvePackages(context.Background(), dir, []string{"testmod"}, "")
	if err != nil {
		t.Fatalf("ResolvePackages: %v", err)
	}
	if len(pkgs) != 1 {
		t.Fatalf("expected 1 package, got %d", len(pkgs))
	}
	if pkgs[0].ImportPath != "testmod" {
		t.Errorf("ImportPath=%q", pkgs[0].ImportPath)
	}
	if len(pkgs[0].GoFiles) == 0 {
		t.Error("GoFiles should not be empty")
	}
	// Decoded import fields must be populated (pins decodeGoListJSON's
	// Imports/TestImports/XTestImports assignments).
	if len(pkgs[0].Imports) == 0 {
		t.Error("Imports should not be empty (add.go imports errors)")
	}
	if len(pkgs[0].TestImports) == 0 {
		t.Error("TestImports should not be empty (add_test.go imports strings)")
	}
	if len(pkgs[0].XTestImports) == 0 {
		t.Error("XTestImports should not be empty (add_x_test.go imports bytes)")
	}
}

// TestResolvePackagesForwardsTags kills BRANCH_IF / CONDITIONALS_NEGATION /
// STATEMENT_REMOVE on the `if tags != "" { -tags= }` guard: the package's
// only file is behind `//go:build mytag`, so `go list` resolves it only
// when the tag is forwarded (otherwise it reports "build constraints
// exclude all Go files", which decodeGoListJSON surfaces as an error).
func TestResolvePackagesForwardsTags(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":    "module testmod\n\ngo 1.26\n",
		"tagged.go": "//go:build mytag\n\npackage testmod\n\nfunc Tagged() int { return 7 }\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := ResolvePackages(context.Background(), dir, []string{"testmod"}, ""); err == nil {
		t.Error("no tags: all files build-excluded, ResolvePackages should error")
	}
	pkgs, err := ResolvePackages(context.Background(), dir, []string{"testmod"}, "mytag")
	if err != nil {
		t.Fatalf("tags=mytag: ResolvePackages should succeed, got: %v", err)
	}
	if len(pkgs) != 1 || len(pkgs[0].GoFiles) == 0 {
		t.Errorf("tags=mytag: want testmod with GoFiles, got %+v", pkgs)
	}
}

func TestDecodeGoListJSONValid(t *testing.T) {
	input := `{"Dir":"/a","ImportPath":"mod/a","GoFiles":["a.go"],"TestGoFiles":["a_test.go"],"EmbedFiles":["data/schema.json"]}
{"Dir":"/b","ImportPath":"mod/b","GoFiles":["b.go"]}
`
	pkgs, err := decodeGoListJSON(strings.NewReader(input))
	if err != nil {
		t.Fatalf("decodeGoListJSON: %v", err)
	}
	if len(pkgs) != 2 {
		t.Fatalf("expected 2 packages, got %d", len(pkgs))
	}
	if pkgs[0].ImportPath != "mod/a" {
		t.Errorf("pkgs[0].ImportPath=%q", pkgs[0].ImportPath)
	}
	// EmbedFiles feeds the incremental cache's pkg_hash, so a dropped
	// assignment here would silently stop invalidating mutants on an
	// embedded-data edit.
	if len(pkgs[0].EmbedFiles) != 1 || pkgs[0].EmbedFiles[0] != "data/schema.json" {
		t.Errorf("pkgs[0].EmbedFiles=%v, want [data/schema.json]", pkgs[0].EmbedFiles)
	}
	if pkgs[1].EmbedFiles != nil {
		t.Errorf("pkgs[1].EmbedFiles=%v, want nil for a package that embeds nothing", pkgs[1].EmbedFiles)
	}
}

func TestDecodeGoListJSONDecodeError(t *testing.T) {
	input := `{invalid json`
	var err error
	// Wrap in deadline: dropping the early-return on Decode error makes the
	// decoder loop on the same bad token forever (dec.More() keeps reporting
	// data is available, Decode keeps failing, we ignore — infinite loop).
	// Tight deadline (300ms): the decoder allocates fast and the RSS-based
	// mutant killer trips at ~3-4s; we need the timer to win that race.
	// Healthy run takes <1ms, so 300ms has plenty of headroom.
	runWithDeadline(t, 300*time.Millisecond, func() {
		_, err = decodeGoListJSON(strings.NewReader(input))
	})
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestDecodeGoListJSONPkgError(t *testing.T) {
	input := `{"ImportPath":"bad/pkg","Error":{"Err":"some error message"}}`
	_, err := decodeGoListJSON(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error for package with Error field")
	}
}

func TestResolvePackagesError(t *testing.T) {
	_, err := ResolvePackages(context.Background(), t.TempDir(), []string{"definitely/nonexistent/pkg/zzz"}, "")
	if err == nil {
		t.Fatal("expected error")
	}
	// Error must wrap stderr text. Kills STATEMENT_REMOVE on
	// `cmd.Stderr = &stderr` in ResolvePackages.
	msg := err.Error()
	if !strings.Contains(msg, "definitely/nonexistent/pkg/zzz") && !strings.Contains(msg, "no required module") && !strings.Contains(msg, "cannot find") {
		t.Errorf("error should include stderr content, got: %q", msg)
	}
}

func TestDiscoverMultiPackage(t *testing.T) {
	dir := t.TempDir()
	// Two packages with the same import path prefix.
	pkgA := filepath.Join(dir, "a")
	pkgB := filepath.Join(dir, "b")
	os.MkdirAll(pkgA, 0o755)
	os.MkdirAll(pkgB, 0o755)

	os.WriteFile(filepath.Join(pkgA, "a.go"), []byte("package a\nfunc F() int { return 1 + 2 }\n"), 0o644)
	os.WriteFile(filepath.Join(pkgB, "b.go"), []byte("package b\nfunc G() int { return 3 - 4 }\n"), 0o644)

	pkgs := []Package{
		{Dir: pkgA, ImportPath: "example.com/mod/a", GoFiles: []string{"a.go"}},
		{Dir: pkgB, ImportPath: "example.com/mod/b", GoFiles: []string{"b.go"}},
	}

	reg := mutator.NewRegistry()
	fset := token.NewFileSet()
	mutants := Discover(fset, pkgs, reg.EnabledMutators([]string{"ARITHMETIC_BASE"}, nil), dir, "example.com/mod").Mutants

	if len(mutants) < 2 {
		t.Fatalf("expected at least 2 mutants across packages, got %d", len(mutants))
	}

	// Verify RelFile contains subpackage prefix.
	for _, m := range mutants {
		if m.RelFile == "" {
			t.Errorf("mutant ID=%d has empty RelFile", m.ID)
		}
	}
}

func TestDiscoverRelFileNoCommonPrefix(t *testing.T) {
	dir := t.TempDir()
	pkgA := filepath.Join(dir, "a")
	pkgB := filepath.Join(dir, "b")
	os.MkdirAll(pkgA, 0o755)
	os.MkdirAll(pkgB, 0o755)

	os.WriteFile(filepath.Join(pkgA, "a.go"), []byte("package a\nfunc F() int { return 1 + 2 }\n"), 0o644)
	os.WriteFile(filepath.Join(pkgB, "b.go"), []byte("package b\nfunc G() int { return 3 + 4 }\n"), 0o644)

	// Two packages with no common import path prefix.
	pkgs := []Package{
		{Dir: pkgA, ImportPath: "alpha/pkg", GoFiles: []string{"a.go"}},
		{Dir: pkgB, ImportPath: "beta/pkg", GoFiles: []string{"b.go"}},
	}

	fset := token.NewFileSet()
	reg := mutator.NewRegistry()
	mutants := Discover(fset, pkgs, reg.EnabledMutators([]string{"ARITHMETIC_BASE"}, nil), dir, "unrelated/module").Mutants

	if len(mutants) < 2 {
		t.Fatalf("expected at least 2 mutants, got %d", len(mutants))
	}
	// When no common prefix, RelFile must come from filepath.Rel(moduleRoot, absPath) —
	// not from concatenating the package import path with the file base. Kills
	// EXPRESSION_REMOVE on the left operand of
	// `commonPrefix != "" && strings.HasPrefix(pkg, commonPrefix)`: if that
	// operand is replaced with `true`, HasPrefix(pkg, "") is trivially true,
	// the then-branch fires, and RelFile ends up prefixed with the full
	// package import path ("alpha/pkg/a.go" / "beta/pkg/b.go").
	for _, m := range mutants {
		if m.RelFile == "" {
			t.Errorf("mutant ID=%d has empty RelFile", m.ID)
		}
		if strings.HasPrefix(m.RelFile, "alpha/pkg") || strings.HasPrefix(m.RelFile, "beta/pkg") {
			t.Errorf("mutant ID=%d RelFile=%q should not carry package import path when commonPrefix is empty",
				m.ID, m.RelFile)
		}
	}
}

func TestDiscoverSortSameLineDifferentCol(t *testing.T) {
	dir := t.TempDir()
	// Multiple operators on different lines and same line — exercises sort comparisons.
	src := `package p

func f() int {
	a := 1 + 2
	b := 3 - 4
	return a + b - 1
}
`
	os.WriteFile(filepath.Join(dir, "multi.go"), []byte(src), 0o644)

	pkgs := []Package{
		{Dir: dir, ImportPath: "example.com/sort", GoFiles: []string{"multi.go"}},
	}

	fset := token.NewFileSet()
	reg := mutator.NewRegistry()
	mutants := Discover(fset, pkgs, reg.Mutators(), dir, "example.com").Mutants

	// Should have multiple mutants on the same line, testing sort comparisons.
	if len(mutants) < 2 {
		t.Fatalf("expected at least 2 mutants on same line, got %d", len(mutants))
	}
	// Verify sort: line should be non-decreasing.
	for i := 1; i < len(mutants); i++ {
		if mutants[i].Line < mutants[i-1].Line {
			t.Errorf("mutants not sorted by line: %d < %d", mutants[i].Line, mutants[i-1].Line)
		}
		if mutants[i].Line == mutants[i-1].Line && mutants[i].Col < mutants[i-1].Col {
			t.Errorf("same-line mutants not sorted by col: %d < %d", mutants[i].Col, mutants[i-1].Col)
		}
	}
}

// TestDiscoverSortTypeOrder asserts that when two mutations share the same
// (file, line, col), they're ordered by ascending MutationType string.
// Kills CONDITIONALS_BOUNDARY and CONDITIONALS_NEGATION mutations on the
// final `a.Type < b.Type` comparator.
func TestDiscoverSortTypeOrder(t *testing.T) {
	dir := t.TempDir()
	// `a - b` produces both ARITHMETIC_BASE and INVERT_NEGATIVES at the same
	// byte position (the '-' token). Since "ARITHMETIC_BASE" < "INVERT_NEGATIVES"
	// lexically, AB must come first.
	src := `package p
func f(a, b int) int { return a - b }
`
	os.WriteFile(filepath.Join(dir, "sub.go"), []byte(src), 0o644)

	pkgs := []Package{
		{Dir: dir, ImportPath: "example.com/sub", GoFiles: []string{"sub.go"}},
	}

	fset := token.NewFileSet()
	reg := mutator.NewRegistry()
	mutants := Discover(fset, pkgs, reg.EnabledMutators([]string{"ARITHMETIC_BASE", "INVERT_NEGATIVES"}, nil), dir, "example.com").Mutants

	if len(mutants) != 2 {
		t.Fatalf("expected 2 mutants, got %d", len(mutants))
	}
	if mutants[0].Line != mutants[1].Line || mutants[0].Col != mutants[1].Col {
		t.Fatalf("mutants should share position: (%d,%d) vs (%d,%d)",
			mutants[0].Line, mutants[0].Col, mutants[1].Line, mutants[1].Col)
	}
	// Assert explicit order by MutationType string.
	if mutants[0].Type != mutator.ArithmeticBase {
		t.Errorf("mutants[0].Type = %v, want ARITHMETIC_BASE", mutants[0].Type)
	}
	if mutants[1].Type != mutator.InvertNegatives {
		t.Errorf("mutants[1].Type = %v, want INVERT_NEGATIVES", mutants[1].Type)
	}
}

// TestDiscoverSortSameLineNeighborCol asserts strict ordering by column
// for same-line mutations. Kills CONDITIONALS_BOUNDARY on the column
// comparator.
func TestDiscoverSortSameLineNeighborCol(t *testing.T) {
	dir := t.TempDir()
	// Two arithmetic ops on the same line at different columns.
	src := `package p
func f() int { return 1 + 2 * 3 }
`
	os.WriteFile(filepath.Join(dir, "line.go"), []byte(src), 0o644)
	pkgs := []Package{{Dir: dir, ImportPath: "example.com/line", GoFiles: []string{"line.go"}}}
	fset := token.NewFileSet()
	reg := mutator.NewRegistry()
	mutants := Discover(fset, pkgs, reg.EnabledMutators([]string{"ARITHMETIC_BASE"}, nil), dir, "example.com").Mutants

	// Expect 2 mutants: one for '+' and one for '*'.
	if len(mutants) != 2 {
		t.Fatalf("expected 2 mutants, got %d", len(mutants))
	}
	if mutants[0].Line != mutants[1].Line {
		t.Fatalf("expected same line, got %d and %d", mutants[0].Line, mutants[1].Line)
	}
	if mutants[0].Col >= mutants[1].Col {
		t.Errorf("expected col ascending: mutants[0].Col=%d, mutants[1].Col=%d",
			mutants[0].Col, mutants[1].Col)
	}
	if mutants[0].Original != "+" || mutants[1].Original != "*" {
		t.Errorf("expected + then *, got %q then %q", mutants[0].Original, mutants[1].Original)
	}
}

// TestDiscoverSortMultiFile asserts strict ordering by filename across
// multiple files. Kills CONDITIONALS_BOUNDARY/NEGATION on filename comparator.
func TestDiscoverSortMultiFile(t *testing.T) {
	dir := t.TempDir()
	// Create two files where name order matters.
	os.WriteFile(filepath.Join(dir, "b.go"), []byte("package p\nfunc F() int { return 1 + 2 }\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package p\nfunc G() int { return 3 + 4 }\n"), 0o644)

	pkgs := []Package{{Dir: dir, ImportPath: "example.com/multi", GoFiles: []string{"b.go", "a.go"}}}
	fset := token.NewFileSet()
	reg := mutator.NewRegistry()
	mutants := Discover(fset, pkgs, reg.EnabledMutators([]string{"ARITHMETIC_BASE"}, nil), dir, "example.com").Mutants

	if len(mutants) != 2 {
		t.Fatalf("expected 2 mutants, got %d", len(mutants))
	}
	// a.go must come before b.go after sort.
	if !strings.HasSuffix(mutants[0].File, "a.go") {
		t.Errorf("mutants[0].File=%q, expected ends with a.go", mutants[0].File)
	}
	if !strings.HasSuffix(mutants[1].File, "b.go") {
		t.Errorf("mutants[1].File=%q, expected ends with b.go", mutants[1].File)
	}
}

func TestDiscoverSamePositionDifferentType(t *testing.T) {
	dir := t.TempDir()
	// Binary subtraction produces both ARITHMETIC_BASE and INVERT_NEGATIVES at the same position.
	src := `package p

func f(a, b int) int { return a - b }
`
	os.WriteFile(filepath.Join(dir, "sub.go"), []byte(src), 0o644)

	pkgs := []Package{
		{Dir: dir, ImportPath: "example.com/sub", GoFiles: []string{"sub.go"}},
	}

	fset := token.NewFileSet()
	reg := mutator.NewRegistry()
	mutants := Discover(fset, pkgs, reg.EnabledMutators([]string{"ARITHMETIC_BASE", "INVERT_NEGATIVES"}, nil), dir, "example.com").Mutants

	// Should have 2 mutants at the same position but different types.
	if len(mutants) != 2 {
		t.Fatalf("expected 2 mutants, got %d", len(mutants))
	}
	if mutants[0].Line != mutants[1].Line || mutants[0].Col != mutants[1].Col {
		t.Errorf("expected same position, got (%d,%d) and (%d,%d)",
			mutants[0].Line, mutants[0].Col, mutants[1].Line, mutants[1].Col)
	}
	if mutants[0].Type == mutants[1].Type {
		t.Errorf("expected different types at same position, both are %v", mutants[0].Type)
	}
}

func TestParseFileReadError(t *testing.T) {
	dir := t.TempDir()
	// The file exists on disk so parser.ParseFile would succeed if it
	// were ever reached with src==nil — that's exactly the equivalent
	// BRANCH_IF mutation we need to kill, so the wrapped-error assertion
	// below is what makes it observable.
	goSrc := "package p\nfunc F() int { return 1 + 2 }\n"
	path := filepath.Join(dir, "ok.go")
	os.WriteFile(path, []byte(goSrc), 0o644)

	orig := readFileBytesFunc
	readFileBytesFunc = func(string) ([]byte, error) {
		return nil, fmt.Errorf("injected read error")
	}
	defer func() { readFileBytesFunc = orig }()

	fset := token.NewFileSet()
	src, file, err := parseFile(fset, path)
	if err == nil {
		t.Fatalf("parseFile should propagate the read error; got src=%v file=%v", src, file)
	}
	if !strings.HasPrefix(err.Error(), "read ") {
		t.Errorf("parseFile must wrap read errors with a 'read <path>:' prefix so the early-return cannot be silently dropped; got: %v", err)
	}
	if !strings.Contains(err.Error(), "injected read error") {
		t.Errorf("wrapped error should preserve the underlying cause via %%w; got: %v", err)
	}

	// Integration check: Discover swallows the parseFile error and skips
	// the file, so the run yields zero mutants.
	pkgs := []Package{
		{Dir: dir, ImportPath: "example.com/err", GoFiles: []string{"ok.go"}},
	}
	reg := mutator.NewRegistry()
	mutants := Discover(fset, pkgs, reg.Mutators(), dir, "example.com").Mutants
	if len(mutants) != 0 {
		t.Errorf("expected 0 mutants when readFileBytes fails, got %d", len(mutants))
	}
}

func TestDiscoverRelPathEmpty(t *testing.T) {
	// Exercise the relPath == "" fallback (line 140-142).
	// This happens when filepath.Rel returns "" — which occurs when moduleRoot
	// and absPath have no relationship. We can trigger this by using a path on a
	// different volume/root.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "x.go"), []byte("package x\nfunc F() int { return 1 + 2 }\n"), 0o644)

	fset := token.NewFileSet()
	reg := mutator.NewRegistry()
	// Use two packages with no common prefix to force commonPrefix = "".
	pkgsTwo := []Package{
		{Dir: dir, ImportPath: "alpha", GoFiles: []string{"x.go"}},
		{Dir: dir, ImportPath: "beta", GoFiles: []string{"x.go"}},
	}

	mutants := Discover(fset, pkgsTwo, reg.EnabledMutators([]string{"ARITHMETIC_BASE"}, nil), dir, dir).Mutants
	for _, m := range mutants {
		if m.RelFile == "" {
			t.Error("RelFile should not be empty")
		}
	}
}

// TestDiscoverSortOrder asserts the full sort order of mutants by
// (file, line, col, type). Kills mutations on the sort comparators.
func TestDiscoverSortOrder(t *testing.T) {
	dir := t.TempDir()
	pkgA := filepath.Join(dir, "a")
	pkgB := filepath.Join(dir, "b")
	os.MkdirAll(pkgA, 0o755)
	os.MkdirAll(pkgB, 0o755)

	// Package b has a file that sorts AFTER pkg a's file alphabetically.
	os.WriteFile(filepath.Join(pkgA, "a.go"), []byte("package a\n\nfunc F(x int) int {\n\treturn x + 1 - x\n}\n"), 0o644)
	os.WriteFile(filepath.Join(pkgB, "b.go"), []byte("package b\nfunc G(x int) int { return x * 2 + 1 }\n"), 0o644)

	pkgs := []Package{
		{Dir: pkgB, ImportPath: "example.com/mod/b", GoFiles: []string{"b.go"}},
		{Dir: pkgA, ImportPath: "example.com/mod/a", GoFiles: []string{"a.go"}},
	}

	fset := token.NewFileSet()
	reg := mutator.NewRegistry()
	mutants := Discover(fset, pkgs, reg.Mutators(), dir, "example.com/mod").Mutants

	// Verify file ordering: a.go mutants come before b.go mutants.
	var aIdx, bIdx []int
	for i, m := range mutants {
		if strings.HasSuffix(m.File, "a.go") {
			aIdx = append(aIdx, i)
		} else if strings.HasSuffix(m.File, "b.go") {
			bIdx = append(bIdx, i)
		}
	}
	if len(aIdx) == 0 || len(bIdx) == 0 {
		t.Fatalf("need mutants in both files, got a=%d b=%d", len(aIdx), len(bIdx))
	}
	if aIdx[len(aIdx)-1] >= bIdx[0] {
		t.Errorf("a.go mutants should all come before b.go: last a=%d, first b=%d", aIdx[len(aIdx)-1], bIdx[0])
	}

	// Within a.go, verify mutants are sorted ascending by (line, col, type).
	var prevLine, prevCol int
	var prevType mutator.MutationType
	for i, idx := range aIdx {
		m := mutants[idx]
		if i > 0 {
			if m.Line < prevLine {
				t.Errorf("a.go mutants not sorted by line: [%d] line=%d < prev=%d", idx, m.Line, prevLine)
			}
			if m.Line == prevLine && m.Col < prevCol {
				t.Errorf("a.go mutants not sorted by col: [%d] col=%d < prev=%d", idx, m.Col, prevCol)
			}
			if m.Line == prevLine && m.Col == prevCol && m.Type < prevType {
				t.Errorf("a.go mutants not sorted by type: [%d] %v < prev=%v", idx, m.Type, prevType)
			}
		}
		prevLine, prevCol, prevType = m.Line, m.Col, m.Type
	}
}

// TestDiscoverRelFileFormat kills mutations on the RelFile computation.
// Three cases:
//   - pkg == commonPrefix (sub == ""): RelFile = filename only.
//   - pkg startswith commonPrefix (sub != ""): RelFile = sub/filename.
//   - No common prefix: RelFile = filepath.Rel(moduleRoot, absPath).
func TestDiscoverRelFileSubEmpty(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "x.go"), []byte("package x\nfunc F() int { return 1 + 2 }\n"), 0o644)

	pkgs := []Package{
		{Dir: dir, ImportPath: "example.com/mod", GoFiles: []string{"x.go"}},
	}

	fset := token.NewFileSet()
	reg := mutator.NewRegistry()
	mutants := Discover(fset, pkgs, reg.EnabledMutators([]string{"ARITHMETIC_BASE"}, nil), dir, "example.com/mod").Mutants
	if len(mutants) == 0 {
		t.Fatal("expected mutants")
	}
	// Single package with import "example.com/mod" and common prefix same → sub is "" → RelFile is filename only.
	if mutants[0].RelFile != "x.go" {
		t.Errorf("RelFile=%q, want %q", mutants[0].RelFile, "x.go")
	}
}

func TestDiscoverRelFileSubNonEmpty(t *testing.T) {
	dir := t.TempDir()
	pkgA := filepath.Join(dir, "a")
	pkgAB := filepath.Join(dir, "a", "b")
	os.MkdirAll(pkgA, 0o755)
	os.MkdirAll(pkgAB, 0o755)
	os.WriteFile(filepath.Join(pkgA, "a.go"), []byte("package a\nfunc F() int { return 1 + 2 }\n"), 0o644)
	os.WriteFile(filepath.Join(pkgAB, "b.go"), []byte("package b\nfunc G() int { return 3 + 4 }\n"), 0o644)

	// Common prefix = "example.com/mod", packages are "example.com/mod/a" and "example.com/mod/a/b".
	pkgs := []Package{
		{Dir: pkgA, ImportPath: "example.com/mod/a", GoFiles: []string{"a.go"}},
		{Dir: pkgAB, ImportPath: "example.com/mod/a/b", GoFiles: []string{"b.go"}},
	}

	fset := token.NewFileSet()
	reg := mutator.NewRegistry()
	mutants := Discover(fset, pkgs, reg.EnabledMutators([]string{"ARITHMETIC_BASE"}, nil), dir, "example.com").Mutants

	// a.go has common prefix "example.com/mod/a", sub == "" → RelFile = "a.go"
	// b.go has common prefix "example.com/mod/a", sub = "/b" → "b" → RelFile = "b/b.go"
	found := map[string]bool{}
	for _, m := range mutants {
		found[m.RelFile] = true
	}
	if !found["a.go"] {
		t.Errorf("expected RelFile=a.go, got map %v", found)
	}
	if !found["b/b.go"] {
		t.Errorf("expected RelFile=b/b.go, got map %v", found)
	}
}

// TestPreReadFilesActuallyDeduplicates stubs readFileBytesFunc to count calls
// and asserts duplicate paths are read only once. Kills BRANCH_IF on the
// `if _, ok := files[absPath]; ok { continue }` guard.
func TestPreReadFilesActuallyDeduplicates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	os.WriteFile(path, []byte("package p\n"), 0o644)

	orig := readFileBytesFunc
	var callCount int
	readFileBytesFunc = func(p string) ([]byte, error) {
		callCount++
		return os.ReadFile(p)
	}
	defer func() { readFileBytesFunc = orig }()

	pkgs := []Package{
		{Dir: dir, GoFiles: []string{"a.go"}},
		{Dir: dir, GoFiles: []string{"a.go"}}, // duplicate
		{Dir: dir, GoFiles: []string{"a.go"}}, // triplicate
	}

	files, err := PreReadFiles(pkgs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Errorf("expected 1 unique file, got %d", len(files))
	}
	if callCount != 1 {
		t.Errorf("expected 1 read call (dedup), got %d", callCount)
	}

	_ = path
}

// TestPreReadFilesReusesParsedSource checks that a file in Discover's parse
// cache contributes the bytes it was parsed from — not what is on disk now —
// and that only the files Discover skipped are read.
func TestPreReadFilesReusesParsedSource(t *testing.T) {
	dir := t.TempDir()
	parsedPath := filepath.Join(dir, "a.go")
	skippedPath := filepath.Join(dir, "b.go")
	os.WriteFile(parsedPath, []byte("package p // edited after discovery\n"), 0o644)
	os.WriteFile(skippedPath, []byte("package p\n"), 0o644)

	orig := readFileBytesFunc
	var reads []string
	readFileBytesFunc = func(p string) ([]byte, error) {
		reads = append(reads, p)
		return os.ReadFile(p)
	}
	defer func() { readFileBytesFunc = orig }()

	pkgs := []Package{{Dir: dir, GoFiles: []string{"a.go", "b.go"}}}
	parsed := map[string]*ParsedFile{parsedPath: {Src: []byte("package p\n")}}

	files, err := PreReadFiles(pkgs, parsed)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(files[parsedPath]); got != "package p\n" {
		t.Errorf("parsed file: got %q, want the bytes discovery parsed", got)
	}
	if got := string(files[skippedPath]); got != "package p\n" {
		t.Errorf("skipped file: got %q, want its disk contents", got)
	}
	if len(reads) != 1 || reads[0] != skippedPath {
		t.Errorf("reads = %v, want only %s", reads, skippedPath)
	}
}

// TestFilterByCoveragePreservesNonPendingStatus kills BRANCH_IF on the
// `if mutants[i].Status != StatusPending { continue }` guard.
func TestFilterByCoveragePreservesNonPendingStatus(t *testing.T) {
	profile := &coverage.Profile{} // No covered blocks.

	pkgs := []Package{
		{Dir: "/abs/path/pkg", ImportPath: "example.com/mod/pkg", GoFiles: []string{"file.go"}},
	}

	// All statuses: Killed, Lived, NotCovered, NotViable, TimedOut, Pending.
	// File is in the mapping so direct lookup succeeds; profile is empty so IsCovered is false.
	mutants := []mutator.Mutant{
		{ID: 1, File: "/abs/path/pkg/file.go", Line: 1, Col: 1, Status: mutator.StatusKilled},
		{ID: 2, File: "/abs/path/pkg/file.go", Line: 1, Col: 1, Status: mutator.StatusLived},
		{ID: 3, File: "/abs/path/pkg/file.go", Line: 1, Col: 1, Status: mutator.StatusNotViable},
		{ID: 4, File: "/abs/path/pkg/file.go", Line: 1, Col: 1, Status: mutator.StatusTimedOut},
		{ID: 5, File: "/abs/path/pkg/file.go", Line: 1, Col: 1, Status: mutator.StatusPending},
	}

	FilterByCoverage(mutants, profile, pkgs, "example.com/mod")

	// Non-pending statuses unchanged.
	if mutants[0].Status != mutator.StatusKilled {
		t.Errorf("#1 Killed was overwritten to %v", mutants[0].Status)
	}
	if mutants[1].Status != mutator.StatusLived {
		t.Errorf("#2 Lived was overwritten to %v", mutants[1].Status)
	}
	if mutants[2].Status != mutator.StatusNotViable {
		t.Errorf("#3 NotViable was overwritten to %v", mutants[2].Status)
	}
	if mutants[3].Status != mutator.StatusTimedOut {
		t.Errorf("#4 TimedOut was overwritten to %v", mutants[3].Status)
	}
	// Pending → NotCovered (profile is empty).
	if mutants[4].Status != mutator.StatusNotCovered {
		t.Errorf("#5 Pending should become NotCovered, got %v", mutants[4].Status)
	}
}

// TestDiscoverSortLinePrimary kills BRANCH_IF on the sort comparator's
// line-inequality branch. Two mutants on *different lines* but the *same
// column* in the same file force the sort to rely on the line comparison:
// under BRANCH_IF that branch body is removed and the sort falls through
// to column (equal) then type (equal), so order becomes undefined.
func TestDiscoverSortLinePrimary(t *testing.T) {
	dir := t.TempDir()
	// Two ARITHMETIC_BASE mutants on different lines but same column index
	// (both operators sit at the same byte column within their line).
	src := "package p\n\nfunc F(x int) int {\n\treturn x + 1\n}\n\nfunc G(x int) int {\n\treturn x - 1\n}\n"
	os.WriteFile(filepath.Join(dir, "f.go"), []byte(src), 0o644)
	pkgs := []Package{
		{Dir: dir, ImportPath: "example.com/p", GoFiles: []string{"f.go"}},
	}
	fset := token.NewFileSet()
	reg := mutator.NewRegistry()
	mutants := Discover(fset, pkgs,
		reg.EnabledMutators([]string{"ARITHMETIC_BASE"}, nil),
		dir, "example.com").Mutants
	if len(mutants) < 2 {
		t.Fatalf("expected >=2 arithmetic mutants, got %d", len(mutants))
	}
	// Earlier-line mutant must come first.
	if mutants[0].Line >= mutants[1].Line {
		t.Errorf("sort broken: mutants[0].Line=%d not < mutants[1].Line=%d",
			mutants[0].Line, mutants[1].Line)
	}
}

func TestPreReadFilesDeduplicate(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Same file in two packages — should only read once.
	pkgs := []Package{
		{Dir: dir, GoFiles: []string{"a.go"}},
		{Dir: dir, GoFiles: []string{"a.go"}},
	}

	files, err := PreReadFiles(pkgs, nil)
	if err != nil {
		t.Fatalf("PreReadFiles: %v", err)
	}
	if len(files) != 1 {
		t.Errorf("expected 1 unique file, got %d", len(files))
	}
}

// TestCandidateLessFallThrough directly exercises candidateLess with two
// candidates that share every field except Line. The extracted comparator
// makes this reachable: under BRANCH_IF on the line-inequality return,
// the body is elided and the function falls through to the equal column
// and equal type checks, finally returning `false` on the type tiebreaker.
// The original returns `true` because a.Line < b.Line.
func TestCandidateLessFallThrough(t *testing.T) {
	a := mutator.MutantCandidate{
		Type: mutator.ArithmeticBase,
		Pos:  mutator.Position{Filename: "/abs/f.go", Line: 4, Column: 11},
	}
	b := mutator.MutantCandidate{
		Type: mutator.ArithmeticBase,
		Pos:  mutator.Position{Filename: "/abs/f.go", Line: 8, Column: 11},
	}
	if got := candidateLess(a, b); !got {
		t.Errorf("candidateLess(line=4, line=8) = %v, want true — BRANCH_IF on the line-return body lets execution fall through to equal column/type comparisons and returns false", got)
	}
}

// TestCandidateLessTieBreakers exercises every comparison in candidateLess
// past the filename. The post-refactor structure pairs `<` and `>` checks
// without an `!=` guard, so each `<`/`>` mutation flips behavior on the
// equality path that the older guarded form rendered untestable.
func TestCandidateLessTieBreakers(t *testing.T) {
	mk := func(file string, line, col int, typ mutator.MutationType) mutator.MutantCandidate {
		return mutator.MutantCandidate{
			Type: typ,
			Pos:  mutator.Position{Filename: file, Line: line, Column: col},
		}
	}
	withEnd := func(c mutator.MutantCandidate, end int, repl string) mutator.MutantCandidate {
		c.EndOffset = end
		c.Replacement = repl
		return c
	}
	tests := []struct {
		name string
		a, b mutator.MutantCandidate
		want bool
	}{
		// Equal filename, line decides.
		{"eqFile aLineLt", mk("/f", 1, 5, mutator.ArithmeticBase), mk("/f", 9, 5, mutator.ArithmeticBase), true},
		{"eqFile aLineGt", mk("/f", 9, 5, mutator.ArithmeticBase), mk("/f", 1, 5, mutator.ArithmeticBase), false},
		// Equal filename + line, column decides.
		{"eqLine aColLt", mk("/f", 4, 1, mutator.ArithmeticBase), mk("/f", 4, 9, mutator.ArithmeticBase), true},
		{"eqLine aColGt", mk("/f", 4, 9, mutator.ArithmeticBase), mk("/f", 4, 1, mutator.ArithmeticBase), false},
		// Equal everything except type.
		{"eqAll typeLt", mk("/f", 4, 5, mutator.ArithmeticBase), mk("/f", 4, 5, mutator.InvertNegatives), true},
		{"eqAll typeGt", mk("/f", 4, 5, mutator.InvertNegatives), mk("/f", 4, 5, mutator.ArithmeticBase), false},
		// Filename `>` is decisive: BRANCH_IF on its `{ return false }` body
		// would let later checks return true here.
		{"aFileGtBLineLt", mk("/zzz", 1, 1, mutator.ArithmeticBase), mk("/aaa", 9, 9, mutator.InvertNegatives), false},
		// Line `>` is decisive (equal file, a.line > b.line); BRANCH_IF on
		// its body would fall through to col-`<` and return true.
		{"eqFile aLineGtBColLt", mk("/f", 9, 1, mutator.ArithmeticBase), mk("/f", 1, 9, mutator.InvertNegatives), false},
		// Column `>` is decisive (equal file/line, a.col > b.col);
		// BRANCH_IF on its body would fall through to type-`<` and return true.
		{"eqLine aColGtTypeLt", mk("/f", 4, 9, mutator.ArithmeticBase), mk("/f", 4, 1, mutator.InvertNegatives), false},
		// Type `>` is decisive (equal position, a.type > b.type); BRANCH_IF
		// on its body would fall through to end-offset-`<` and return true.
		{"eqPos aTypeGtEndLt", withEnd(mk("/f", 4, 5, mutator.InvertNegatives), 10, ""),
			withEnd(mk("/f", 4, 5, mutator.ArithmeticBase), 20, ""), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := candidateLess(tt.a, tt.b); got != tt.want {
				t.Errorf("candidateLess = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCandidateLessEqualCandidates exercises the final `return a.Type < b.Type`
// with two candidates that are identical in every field. The original
// returns false (equal types aren't "less than"); CONDITIONALS_BOUNDARY
// on `<` → `<=` returns true. This boundary is unreachable through sort
// because sort never compares an element to itself — hence the unit test.
func TestCandidateLessEqualCandidates(t *testing.T) {
	a := mutator.MutantCandidate{
		Type: mutator.ArithmeticBase,
		Pos:  mutator.Position{Filename: "/abs/f.go", Line: 4, Column: 11},
	}
	if got := candidateLess(a, a); got {
		t.Errorf("candidateLess(x, x) = true, want false — CONDITIONALS_BOUNDARY on the final `a.Replacement < b.Replacement` flips equal to true")
	}
}

// TestCandidateLessSpanTieBreakers covers the two comparisons that make the
// order total. They are what stops two candidates at the same position and
// type from tying, which an unstable sort would then be free to order either
// way — swapping the ordinal suffixes of their stable IDs.
func TestCandidateLessSpanTieBreakers(t *testing.T) {
	mk := func(end int, repl string) mutator.MutantCandidate {
		return mutator.MutantCandidate{
			Type:        mutator.ExpressionRemove,
			Pos:         mutator.Position{Filename: "/f.go", Line: 4, Column: 9},
			EndOffset:   end,
			Replacement: repl,
		}
	}
	tests := []struct {
		name string
		a, b mutator.MutantCandidate
		want bool
	}{
		// Equal position and type: the narrower span sorts first.
		{"aEndLt", mk(48, "true"), mk(53, "true"), true},
		{"aEndGt", mk(53, "true"), mk(48, "true"), false},
		// End offset `>` is decisive; BRANCH_IF on its body would fall
		// through to the replacement compare and return true here.
		{"aEndGtReplLt", mk(53, "aaa"), mk(48, "zzz"), false},
		// Identical span: replacement is the last resort.
		{"eqSpanReplLt", mk(48, "false"), mk(48, "true"), true},
		{"eqSpanReplGt", mk(48, "true"), mk(48, "false"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := candidateLess(tt.a, tt.b); got != tt.want {
				t.Errorf("candidateLess = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestComputeRelFileNonMatchingPkg kills EXPRESSION_REMOVE on the right
// operand of `commonPrefix != "" && strings.HasPrefix(pkg, commonPrefix)`.
// With commonPrefix="x/y" and pkg="z/q" (unrelated), HasPrefix is false
// and the else branch wins: RelFile comes from filepath.Rel(moduleRoot,
// absPath). Replacing the right operand with `true` sends us into the
// then branch, which concatenates the full pkg path with the file base.
func TestComputeRelFileNonMatchingPkg(t *testing.T) {
	got := computeRelFile("x/y", "z/q", "/root/sub/file.go", "/root")
	if got == "z/q/file.go" {
		t.Errorf("computeRelFile returned %q — EXPRESSION_REMOVE on HasPrefix sent non-matching pkg through the then branch", got)
	}
	// Original answer uses filepath.Rel.
	if got != "sub/file.go" {
		t.Errorf("computeRelFile(x/y, z/q, /root/sub/file.go, /root) = %q, want sub/file.go", got)
	}
}

// TestDiscoverContinuesPastUnparseable kills INVERT_LOOP_CTRL on the
// `continue` after `gomutant: skipping unparseable` (discover.go:94).
// Mutated to `break`, the file loop exits on the first parse failure and
// any subsequent files in the same package are silently skipped.
func TestDiscoverContinuesPastUnparseable(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.go"), []byte("not valid go"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "good.go"), []byte("package p\nfunc F() int { return 1 + 2 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pkgs := []Package{
		{Dir: dir, ImportPath: "example.com/mix", GoFiles: []string{"bad.go", "good.go"}},
	}

	fset := token.NewFileSet()
	reg := mutator.NewRegistry()
	mutants := Discover(fset, pkgs, reg.EnabledMutators([]string{"ARITHMETIC_BASE"}, nil), dir, "example.com").Mutants
	if len(mutants) == 0 {
		t.Fatal("expected mutants from good.go even after bad.go was skipped — INVERT_LOOP_CTRL on the unparseable-continue mutates the loop into an early exit")
	}
}

// TestPreReadFilesContinuesPastDuplicate kills INVERT_LOOP_CTRL on the
// `continue` in PreReadFiles (files.go:20). Mutated to `break`, hitting
// an already-cached path stops the inner file loop, dropping later files
// in the same package.
func TestPreReadFilesContinuesPastDuplicate(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "shared.go"), []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "after.go"), []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Two packages share shared.go. The second package lists shared.go
	// first (already cached), then after.go (must still be read).
	pkgs := []Package{
		{Dir: dir, GoFiles: []string{"shared.go"}},
		{Dir: dir, GoFiles: []string{"shared.go", "after.go"}},
	}

	files, err := PreReadFiles(pkgs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files[filepath.Join(dir, "after.go")]; !ok {
		t.Errorf("after.go missing from result — INVERT_LOOP_CTRL turned `continue` into `break`, exiting the inner file loop on the duplicate")
	}
}

// TestFilterByCoverageContinuesPastUnmapped kills INVERT_LOOP_CTRL on
// filter.go's `continue` (line 32). Mutated to `break`, the first
// unmapped mutant terminates the loop and later mutants stay Pending.
func TestFilterByCoverageContinuesPastUnmapped(t *testing.T) {
	profile := &coverage.Profile{}
	pkgs := []Package{
		{Dir: "/abs/path/pkg", ImportPath: "example.com/mod/pkg", GoFiles: []string{"file.go"}},
	}
	mutants := []mutator.Mutant{
		{ID: 1, File: "/unknown/file.go", Line: 1, Col: 1, Status: mutator.StatusPending},
		{ID: 2, File: "/abs/path/pkg/file.go", Line: 1, Col: 1, Status: mutator.StatusPending},
	}

	FilterByCoverage(mutants, profile, pkgs, "example.com/mod")

	if mutants[0].Status != mutator.StatusNotCovered {
		t.Errorf("mutants[0] should be NotCovered, got %v", mutants[0].Status)
	}
	if mutants[1].Status == mutator.StatusPending {
		t.Errorf("mutants[1] still Pending — INVERT_LOOP_CTRL on the unmapped `continue` exits the loop and skips later mutants")
	}
}

// TestFilterByCoverageStatusOnMissingEntry kills BRANCH_IF on filter.go's
// `if !ok { Status = NotCovered; continue }`. The mutation elides the
// body — execution falls through to `profile.IsCovered(profilePath, ...)`
// where profilePath is "" (the zero value from a missing map entry).
// Normally IsCovered("") always returns false so the next statement
// *also* sets Status=NotCovered and the mutation stays invisible. We
// construct a profile whose File="" block *does* cover the point; under
// mutation IsCovered("") returns true, leaving Status=Pending.
func TestFilterByCoverageStatusOnMissingEntry(t *testing.T) {
	// Coverage line with empty file: ":L.C,L.C N count".
	dir := t.TempDir()
	profilePath := filepath.Join(dir, "c.out")
	if err := os.WriteFile(profilePath, []byte("mode: set\n:1.1,1000.1000 1 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	profile, err := coverage.ParseFile(profilePath)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}

	// Mutant whose File isn't in any pkg → map miss on absToProfile.
	mutants := []mutator.Mutant{
		{ID: 1, File: "/not/in/any/pkg/file.go", Line: 10, Col: 5, Status: mutator.StatusPending},
	}
	pkgs := []Package{
		{Dir: "/abs/pkg", ImportPath: "mod/pkg", GoFiles: []string{"other.go"}},
	}

	FilterByCoverage(mutants, profile, pkgs, "mod")

	if mutants[0].Status != mutator.StatusNotCovered {
		t.Errorf("Status=%v, want NotCovered — BRANCH_IF on `if !ok` elides the early assignment; IsCovered(\"\") on a profile with an empty-file block returns true, leaving Status=Pending",
			mutants[0].Status)
	}
}

// TestFilterByCoverageKeepsUninstrumentedPositionInTestedFile pins down
// the relaxation that lets us test mutants on positions Go's coverage
// tool doesn't instrument (top-level const/var initializers,
// select-case receive expressions, gaps between adjacent blocks). When
// the file has at least one Count > 0 block and the position falls
// outside every block, the mutant stays Pending.
func TestFilterByCoverageKeepsUninstrumentedPositionInTestedFile(t *testing.T) {
	profile := loadProfile(t, "mode: set\nmod/pkg/file.go:10.1,20.99 1 1\n")

	pkgs := []Package{
		{Dir: "/abs/pkg", ImportPath: "mod/pkg", GoFiles: []string{"file.go"}},
	}
	mutants := []mutator.Mutant{
		// Outside any block, file has Count>0 block → stay Pending.
		{ID: 1, File: "/abs/pkg/file.go", Line: 1, Col: 5, Status: mutator.StatusPending},
		// Inside the Count>0 block → stay Pending.
		{ID: 2, File: "/abs/pkg/file.go", Line: 12, Col: 5, Status: mutator.StatusPending},
	}

	FilterByCoverage(mutants, profile, pkgs, "mod")

	if mutants[0].Status != mutator.StatusPending {
		t.Errorf("uninstrumented position in tested file: Status=%v, want Pending", mutants[0].Status)
	}
	if mutants[1].Status != mutator.StatusPending {
		t.Errorf("covered position: Status=%v, want Pending", mutants[1].Status)
	}
}

// TestFilterByCoverageMarksUninstrumentedPositionInUntestedFile pins down
// the other half of the relaxation: when the file has *no* Count > 0
// block (no test reaches it), an uninstrumented position is still
// NotCovered. Without this, a file no test touches would silently get
// its mutants run.
func TestFilterByCoverageMarksUninstrumentedPositionInUntestedFile(t *testing.T) {
	profile := loadProfile(t, "mode: set\nmod/pkg/file.go:10.1,20.99 1 0\n")

	pkgs := []Package{
		{Dir: "/abs/pkg", ImportPath: "mod/pkg", GoFiles: []string{"file.go"}},
	}
	mutants := []mutator.Mutant{
		{ID: 1, File: "/abs/pkg/file.go", Line: 1, Col: 5, Status: mutator.StatusPending},
	}

	FilterByCoverage(mutants, profile, pkgs, "mod")

	if mutants[0].Status != mutator.StatusNotCovered {
		t.Errorf("uninstrumented position in untested file: Status=%v, want NotCovered", mutants[0].Status)
	}
}

// TestFilterByCoverageMarksUninstrumentedFileInTestedPackage pins down
// the symmetric case: when a different file in the package has covered
// blocks but the mutant's own file has none in the profile at all,
// HasCoveredBlock(mutant.file) is false and the mutant is NotCovered.
func TestFilterByCoverageMarksUninstrumentedFileInTestedPackage(t *testing.T) {
	profile := loadProfile(t, "mode: set\nmod/pkg/other.go:10.1,20.99 1 1\n")

	pkgs := []Package{
		{Dir: "/abs/pkg", ImportPath: "mod/pkg", GoFiles: []string{"file.go", "other.go"}},
	}
	mutants := []mutator.Mutant{
		{ID: 1, File: "/abs/pkg/file.go", Line: 1, Col: 5, Status: mutator.StatusPending},
	}

	FilterByCoverage(mutants, profile, pkgs, "mod")

	if mutants[0].Status != mutator.StatusNotCovered {
		t.Errorf("file with no profile entry: Status=%v, want NotCovered", mutants[0].Status)
	}
}

// TestFilterByCoverageContinuesAfterCovered kills INVERT_LOOP_CTRL on
// the `continue` after the IsCovered check in FilterByCoverage. Mutated
// to `break`, processing stops at the first covered mutant — every
// later mutant in the slice keeps its Pending status instead of being
// classified.
func TestFilterByCoverageContinuesAfterCovered(t *testing.T) {
	profile := loadProfile(t, "mode: set\nmod/pkg/file.go:10.1,20.99 1 1\n")

	pkgs := []Package{
		{Dir: "/abs/pkg", ImportPath: "mod/pkg", GoFiles: []string{"file.go"}},
	}
	mutants := []mutator.Mutant{
		// Covered — `continue` skips past this one.
		{ID: 1, File: "/abs/pkg/file.go", Line: 12, Col: 5, Status: mutator.StatusPending},
		// Outside any block in an untested file — must be NotCovered.
		// `break` instead of `continue` would leave it Pending.
		{ID: 2, File: "/missing/pkg/file.go", Line: 1, Col: 1, Status: mutator.StatusPending},
	}

	FilterByCoverage(mutants, profile, pkgs, "mod")

	if mutants[1].Status != mutator.StatusNotCovered {
		t.Errorf("mutant 2: status=%v, want NotCovered — INVERT_LOOP_CTRL on `continue` would `break` after the covered mutant and leave this Pending", mutants[1].Status)
	}
}

// TestFilterByCoverageContinuesAfterRelaxedMatch kills INVERT_LOOP_CTRL
// on the second `continue` (the relaxed branch). Mutated to `break`,
// processing stops at the first uninstrumented-but-tested mutant; later
// mutants stay Pending.
func TestFilterByCoverageContinuesAfterRelaxedMatch(t *testing.T) {
	profile := loadProfile(t, "mode: set\nmod/pkg/file.go:10.1,20.99 1 1\n")

	pkgs := []Package{
		{Dir: "/abs/pkg", ImportPath: "mod/pkg", GoFiles: []string{"file.go"}},
	}
	mutants := []mutator.Mutant{
		// Uninstrumented (line 1 is outside the 10–20 block) but in a
		// tested file — relaxed branch fires `continue`.
		{ID: 1, File: "/abs/pkg/file.go", Line: 1, Col: 5, Status: mutator.StatusPending},
		// In an untested file — must be NotCovered. `break` instead of
		// `continue` after mutant 1 would leave this Pending.
		{ID: 2, File: "/missing/pkg/file.go", Line: 1, Col: 1, Status: mutator.StatusPending},
	}

	FilterByCoverage(mutants, profile, pkgs, "mod")

	if mutants[1].Status != mutator.StatusNotCovered {
		t.Errorf("mutant 2: status=%v, want NotCovered — INVERT_LOOP_CTRL on relaxed-branch `continue` would `break` and leave this Pending", mutants[1].Status)
	}
}

// TestFilterByCoverageMarksPositionInCount0Block pins the distinction
// between uninstrumented positions (no block at all) and explicitly
// uncovered positions (Count==0 block). The relaxation must NOT treat
// the latter as covered — those are real test gaps. Kills
// EXPRESSION_REMOVE on the `!profile.IsInAnyBlock(...)` operand: if
// it's elided to `true`, a position inside a Count==0 block would slip
// through and stay Pending instead of being marked NotCovered.
func TestFilterByCoverageMarksPositionInCount0Block(t *testing.T) {
	// File with both a Count>0 block (lines 10–20) AND a Count==0 block
	// (lines 30–40). HasCoveredBlock is true; the Count==0 block is a
	// real coverage gap.
	profile := loadProfile(t, "mode: set\nmod/pkg/file.go:10.1,20.99 1 1\nmod/pkg/file.go:30.1,40.99 1 0\n")

	pkgs := []Package{
		{Dir: "/abs/pkg", ImportPath: "mod/pkg", GoFiles: []string{"file.go"}},
	}
	mutants := []mutator.Mutant{
		// Position inside the Count==0 block — must be NotCovered.
		{ID: 1, File: "/abs/pkg/file.go", Line: 35, Col: 5, Status: mutator.StatusPending},
	}

	FilterByCoverage(mutants, profile, pkgs, "mod")

	if mutants[0].Status != mutator.StatusNotCovered {
		t.Errorf("position in Count==0 block: Status=%v, want NotCovered (relaxation must not swallow real coverage gaps)", mutants[0].Status)
	}
}

func loadProfile(t *testing.T, content string) *coverage.Profile {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "c.out")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := coverage.ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	return p
}
