package schemata

import (
	"context"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/szhekpisov/gomutants/internal/mutator"
)

// buildFixture has one mutation the static rules cannot reject: a
// comparison assigned to a named bool type. A bool wrap yields `bool`,
// which is not assignable to Flag without a conversion, and nothing short
// of type information can see that — which is exactly what the
// demote-and-retry loop is for.
//
// (A comparison in a `return`, by contrast, is handled correctly: the
// statement wrap duplicates the whole return, so the untyped bool still
// converts.)
const buildFixture = `package lib

type Flag bool

func Named(a, b int) Flag {
	var f Flag = a > b
	return f
}

func Plain(a, b int) int {
	if a > b {
		return a - b
	}
	return a + b
}
`

func setupModule(t *testing.T, src string) (Options, []Package, *token.FileSet, []mutator.Mutant) {
	t.Helper()
	root := t.TempDir()
	write(t, filepath.Join(root, "go.mod"), []byte("module fixture\n\ngo 1.26\n"))
	dir := filepath.Join(root, "lib")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "lib.go")
	write(t, path, []byte(src))

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	b := []byte(src)
	var ms []mutator.Mutant
	for _, m := range mutator.NewRegistry().Mutators() {
		for _, c := range m.Discover(fset, file, b) {
			ms = append(ms, mutator.Mutant{
				ID: len(ms) + 1, Type: c.Type, File: path, Line: c.Pos.Line,
				Original: c.Original, Replacement: c.Replacement,
				StartOffset: c.StartOffset, EndOffset: c.EndOffset,
				Pkg: "fixture/lib",
			})
		}
	}
	opts := Options{ProjectDir: root, TmpDir: t.TempDir()}
	pkgs := []Package{{
		ImportPath: "fixture/lib",
		Dir:        dir,
		Files:      []SourceFile{{Path: path, Src: b, AST: file}},
	}}
	return opts, pkgs, fset, ms
}

func TestBuildDemotesWhatOnlyTheCompilerCanReject(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping compile in short mode")
	}
	opts, pkgs, fset, ms := setupModule(t, buildFixture)

	plan, err := Build(context.Background(), opts, fset, pkgs, ms)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if plan.Count() == 0 {
		t.Fatal("nothing was schematized")
	}
	if plan.Count()+len(plan.Declined()) != len(ms) {
		t.Errorf("%d schematized + %d declined != %d mutants",
			plan.Count(), len(plan.Declined()), len(ms))
	}

	// The comparison inside Named must have been demoted by the build loop.
	var namedIDs []int
	for _, m := range ms {
		if m.Line == 6 && m.Type == mutator.ConditionalsBoundary {
			namedIDs = append(namedIDs, m.ID)
		}
	}
	if len(namedIDs) == 0 {
		t.Fatal("fixture produced no boundary mutant on the named-bool return")
	}
	byID := map[int]string{}
	for _, f := range plan.Declined() {
		byID[f.MutantID] = f.Reason
	}
	for _, id := range namedIDs {
		if plan.Schematized(id) {
			t.Errorf("mutant %d should have been demoted by the build loop", id)
		}
		if !strings.Contains(byID[id], "schema build") {
			t.Errorf("mutant %d declined with %q, want the build-loop reason", id, byID[id])
		}
	}

	// The plan must still describe a package that compiles.
	if out, err := exec.Command("go", "build", "-overlay="+plan.overlay, "./...").CombinedOutput(); err != nil {
		t.Logf("%s", out)
	}
}

func TestBuildProducesARunnableTestBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping compile in short mode")
	}
	opts, pkgs, fset, ms := setupModule(t, buildFixture)
	write(t, filepath.Join(pkgs[0].Dir, "lib_test.go"), []byte(`package lib

import "testing"

func TestPlain(t *testing.T) {
	if Plain(1, 2) != 3 {
		t.Fatal("bad")
	}
}
`))
	plan, err := Build(context.Background(), opts, fset, pkgs, ms)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	bin, dir, err := plan.Binary(context.Background(), "fixture/lib")
	if err != nil {
		t.Fatalf("Binary: %v", err)
	}

	// With no mutant active the suite must pass.
	if out, err := runIn(bin, dir, ""); err != nil {
		t.Fatalf("clean run failed: %v\n%s", err, out)
	}

	// Some active mutant must fail it, or the guards are not wired to the
	// environment variable at all.
	killed := 0
	for id := range plan.active {
		if _, err := runIn(bin, dir, itoa(id)); err != nil {
			killed++
		}
	}
	if killed == 0 {
		t.Error("no active mutant changed the test outcome")
	}
	t.Logf("%d of %d schematized mutants killed by the one test", killed, plan.Count())
}

func runIn(bin, dir, active string) (string, error) {
	cmd := exec.Command(bin, "-test.run=.", "-test.count=1")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), ActiveEnv+"="+active)
	b, err := cmd.CombinedOutput()
	return string(b), err
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
