package discover

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/szhekpisov/gomutants/internal/mutator"
)

// parseSrc parses src as a file named name and returns the AST alongside
// the FileSet, so span offsets resolve the same way Discover resolves them.
func parseSrc(t *testing.T, name, src string) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return fset, f
}

func TestFuncSpansSkipsNonFuncDecls(t *testing.T) {
	fset, f := parseSrc(t, "p.go", `package p

import "fmt"

var V = 1

type T struct{}

func One() {}

const C = 2

func Two() {}
`)

	spans := funcSpans(fset, f)
	if len(spans) != 2 {
		t.Fatalf("len(spans)=%d, want 2 (import/var/type/const decls must be skipped)", len(spans))
	}
	if spans[0].name != "One" || spans[1].name != "Two" {
		t.Errorf("names = %q, %q; want One, Two", spans[0].name, spans[1].name)
	}
	// Source order, non-overlapping, and each span must actually cover text.
	if spans[0].start >= spans[0].end {
		t.Errorf("spans[0] covers no text: %+v", spans[0])
	}
	if spans[0].end > spans[1].start {
		t.Errorf("spans overlap: %+v", spans)
	}
	if spans[1].start >= spans[1].end {
		t.Errorf("spans[1] covers no text: %+v", spans[1])
	}
}

func TestFuncSpansRecordsReceivers(t *testing.T) {
	fset, f := parseSrc(t, "p.go", `package p

type A struct{}
type B struct{}
type Stack[T any] struct{}
type Pair[K, V any] struct{}

func Plain() {}
func (a A) M() {}
func (b *B) M() {}
func (s Stack[T]) Push() {}
func (p *Pair[K, V]) Get() {}
`)

	want := []string{"Plain", "(A).M", "(*B).M", "(Stack).Push", "(*Pair).Get"}
	spans := funcSpans(fset, f)
	if len(spans) != len(want) {
		t.Fatalf("len(spans)=%d, want %d", len(spans), len(want))
	}
	for i, w := range want {
		if spans[i].name != w {
			t.Errorf("spans[%d].name=%q, want %q", i, spans[i].name, w)
		}
	}
}

func TestFuncNameWithoutReceiver(t *testing.T) {
	// A nil Recv and a present-but-empty Recv must both fall back to the
	// bare function name rather than rendering an empty "()." prefix.
	fn := &ast.FuncDecl{Name: ast.NewIdent("Run")}
	if got := funcName(fn); got != "Run" {
		t.Errorf("nil Recv: funcName=%q, want %q", got, "Run")
	}

	fn.Recv = &ast.FieldList{}
	if got := funcName(fn); got != "Run" {
		t.Errorf("empty Recv.List: funcName=%q, want %q", got, "Run")
	}
}

func TestReceiverTypeUnrecognized(t *testing.T) {
	// Any shape the switch doesn't know renders as empty, so the method
	// still gets an anchor ("().M") instead of merging with plain funcs.
	if got := receiverType(&ast.BasicLit{Value: "1"}); got != "" {
		t.Errorf("receiverType=%q, want empty", got)
	}

	fn := &ast.FuncDecl{
		Name: ast.NewIdent("M"),
		Recv: &ast.FieldList{List: []*ast.Field{{Type: &ast.BasicLit{Value: "1"}}}},
	}
	if got := funcName(fn); got != "().M" {
		t.Errorf("funcName=%q, want %q", got, "().M")
	}
}

func TestReceiverTypeParenthesized(t *testing.T) {
	// `func (x (T)) M()` and `func (x (*T)) M()` are legal Go; the parser
	// hands back an *ast.ParenExpr. Without unwrapping it the receiver falls
	// to the unrecognized branch, so every parenthesized receiver in a file
	// collapses onto the same "().M" anchor and shares one ordinal counter.
	fset, f := parseSrc(t, "p.go", `package p

type T struct{}

func (x (T)) M() int { return 1 + 2 }

func (y (*T)) N() int { return 3 + 4 }
`)
	spans := funcSpans(fset, f)
	want := []string{"(T).M", "(*T).N"}
	if len(spans) != len(want) {
		t.Fatalf("len(spans)=%d, want %d", len(spans), len(want))
	}
	for i, w := range want {
		if spans[i].name != w {
			t.Errorf("spans[%d].name=%q, want %q", i, spans[i].name, w)
		}
	}
}

func TestAnchorFor(t *testing.T) {
	spans := []funcSpan{
		{start: 10, end: 20, name: "f"},
		{start: 30, end: 40, name: "g"},
	}

	tests := []struct {
		name   string
		offset int
		want   string
	}{
		{"before first span", 5, ""},
		{"exactly at start of first span", 10, "f"},
		{"inside first span", 15, "f"},
		{"exactly at end of first span (exclusive)", 20, ""},
		{"between spans", 25, ""},
		{"exactly at start of second span", 30, "g"},
		{"inside second span", 35, "g"},
		{"past last span", 45, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := anchorFor(spans, tt.offset); got != tt.want {
				t.Errorf("anchorFor(offset=%d)=%q, want %q", tt.offset, got, tt.want)
			}
		})
	}
}

func TestAnchorForNoSpans(t *testing.T) {
	// A file of only package-level declarations has no spans at all; every
	// offset must resolve to the empty anchor rather than indexing spans[-1].
	if got := anchorFor(nil, 0); got != "" {
		t.Errorf("anchorFor(nil, 0)=%q, want empty", got)
	}
	if got := anchorFor([]funcSpan{}, 100); got != "" {
		t.Errorf("anchorFor(empty, 100)=%q, want empty", got)
	}
}

func TestStableIDFormat(t *testing.T) {
	k := ordinalKey{file: "internal/cli/cli.go", anchor: "parseArgs", typ: mutator.ConditionalsBoundary}
	want := "internal/cli/cli.go:parseArgs:CONDITIONALS_BOUNDARY#2"
	if got := stableID(k, 2); got != want {
		t.Errorf("stableID=%q, want %q", got, want)
	}

	// An empty anchor (package-level declaration) leaves an empty segment
	// rather than collapsing the separators.
	k.anchor = ""
	want = "internal/cli/cli.go::CONDITIONALS_BOUNDARY#1"
	if got := stableID(k, 1); got != want {
		t.Errorf("stableID=%q, want %q", got, want)
	}
}

// discoverArithmetic runs Discover over a single-package tempdir with only
// the ARITHMETIC_BASE mutator enabled, which keeps expected IDs predictable.
func discoverArithmetic(t *testing.T, files map[string]string) []mutator.Mutant {
	t.Helper()
	dir := t.TempDir()
	names := make([]string, 0, len(files))
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	pkgs := []Package{{Dir: dir, ImportPath: "example.com/test/p", GoFiles: names}}
	reg := mutator.NewRegistry()
	fset := token.NewFileSet()
	return Discover(fset, pkgs, reg.EnabledMutators([]string{"ARITHMETIC_BASE"}, nil), dir, "example.com/test").Mutants
}

func TestDiscoverStableIDAnchorsAndOrdinals(t *testing.T) {
	src := `package p

var V = 1 + 2

type A struct{}
type B struct{}

func (a A) Same() int { return 3 + 4 }

func (b *B) Same() int { return 5 + 6 }

func Plain() int {
	x := 7 + 8
	y := 9 + 10
	return x + y
}

func WithClosure() int {
	f := func() int { return 11 + 12 }
	return f()
}
`
	mutants := discoverArithmetic(t, map[string]string{"p.go": src})

	// One ARITHMETIC_BASE mutant per `+`, in (line, col) order. The anchors
	// pin three behaviors: package-level declarations get the empty anchor,
	// same-named methods stay apart via their receivers, and a closure's
	// mutant anchors to the enclosing declaration rather than the literal.
	want := []string{
		"::ARITHMETIC_BASE#1",            // var V = 1 + 2
		":(A).Same:ARITHMETIC_BASE#1",    // return 3 + 4
		":(*B).Same:ARITHMETIC_BASE#1",   // return 5 + 6
		":Plain:ARITHMETIC_BASE#1",       // x := 7 + 8
		":Plain:ARITHMETIC_BASE#2",       // y := 9 + 10
		":Plain:ARITHMETIC_BASE#3",       // return x + y
		":WithClosure:ARITHMETIC_BASE#1", // closure body: 11 + 12
	}
	if len(mutants) != len(want) {
		for _, m := range mutants {
			t.Logf("line %d col %d: %s", m.Line, m.Col, m.StableID)
		}
		t.Fatalf("len(mutants)=%d, want %d", len(mutants), len(want))
	}
	for i, suffix := range want {
		w := mutants[i].RelFile + suffix
		if mutants[i].StableID != w {
			t.Errorf("mutants[%d].StableID=%q, want %q", i, mutants[i].StableID, w)
		}
	}
}

func TestDiscoverStableIDsAreUniqueAndFileScoped(t *testing.T) {
	// Byte-identical files: every anchor, type and ordinal collides, so the
	// rel-file segment is the only thing keeping the IDs apart.
	src := `package p

func F() int {
	return 1 + 2
}
`
	mutants := discoverArithmetic(t, map[string]string{"a.go": src, "b.go": src})
	if len(mutants) != 2 {
		t.Fatalf("len(mutants)=%d, want 2", len(mutants))
	}

	seen := make(map[string]bool, len(mutants))
	for _, m := range mutants {
		if m.StableID == "" {
			t.Fatalf("mutant at %s:%d has empty StableID", m.RelFile, m.Line)
		}
		if seen[m.StableID] {
			t.Errorf("duplicate StableID %q", m.StableID)
		}
		seen[m.StableID] = true
		if !strings.HasSuffix(m.StableID, ":F:ARITHMETIC_BASE#1") {
			t.Errorf("StableID=%q, want the ordinal to reset per file", m.StableID)
		}
	}
}

func TestDiscoverStableIDsSurviveLineShifts(t *testing.T) {
	// The whole point of anchoring: inserting lines above a function must
	// not change the IDs of the mutants inside it.
	body := `
func F() int {
	return 1 + 2
}

func G() int {
	return 3 + 4
}
`
	before := discoverArithmetic(t, map[string]string{"p.go": "package p\n" + body})
	after := discoverArithmetic(t, map[string]string{
		"p.go": "package p\n\nvar Pad = \"inserted\"\n\n// a comment too\n" + body,
	})

	if len(before) != len(after) || len(before) != 2 {
		t.Fatalf("mutant counts: before=%d after=%d, want 2 each", len(before), len(after))
	}
	for i := range before {
		if before[i].Line == after[i].Line {
			t.Fatalf("mutants[%d] did not shift lines; the fixture no longer tests anything", i)
		}
		if before[i].StableID != after[i].StableID {
			t.Errorf("mutants[%d].StableID changed across a line shift: %q → %q",
				i, before[i].StableID, after[i].StableID)
		}
	}
}

func TestDiscoverStableIDsSeparateNestedBooleanOperands(t *testing.T) {
	// `a && b && c` parses as `(a && b) && c`, so EXPRESSION_REMOVE emits a
	// candidate for the outer left operand (`a && b`) and one for the inner
	// (`a`) — same file, line, column and type. Position alone cannot order
	// them, and sort.Slice is not stable, so without the span tiebreak the
	// #1/#2 suffixes could swap and each id would name a different mutation
	// from run to run.
	dir := t.TempDir()
	src := `package p

func F(a, b, c bool) bool {
	return a && b && c
}
`
	if err := os.WriteFile(filepath.Join(dir, "p.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	pkgs := []Package{{Dir: dir, ImportPath: "example.com/test/p", GoFiles: []string{"p.go"}}}
	reg := mutator.NewRegistry()
	mutants := Discover(token.NewFileSet(), pkgs,
		reg.EnabledMutators([]string{"EXPRESSION_REMOVE"}, nil), dir, "example.com/test").Mutants

	if len(mutants) != 4 {
		t.Fatalf("len(mutants)=%d, want 4", len(mutants))
	}

	// The narrower span takes #1: `a` before `a && b`, both starting at
	// column 9. Pinning Original alongside the id is what makes a swap fail
	// here rather than silently rename the two mutants.
	want := []struct{ id, original string }{
		{"p.go:F:EXPRESSION_REMOVE#1", "a"},
		{"p.go:F:EXPRESSION_REMOVE#2", "a && b"},
		{"p.go:F:EXPRESSION_REMOVE#3", "b"},
		{"p.go:F:EXPRESSION_REMOVE#4", "c"},
	}
	for i, w := range want {
		if mutants[i].StableID != w.id {
			t.Errorf("mutants[%d].StableID=%q, want %q", i, mutants[i].StableID, w.id)
		}
		if mutants[i].Original != w.original {
			t.Errorf("mutants[%d].Original=%q, want %q", i, mutants[i].Original, w.original)
		}
	}
}

// mutantsWithIDs builds a mutant slice carrying only the StableIDs, which
// is all FilterByStableID looks at.
func mutantsWithIDs(ids ...string) []mutator.Mutant {
	ms := make([]mutator.Mutant, len(ids))
	for i, id := range ids {
		ms[i] = mutator.Mutant{StableID: id}
	}
	return ms
}

func TestFilterByStableIDExactMatch(t *testing.T) {
	ms := mutantsWithIDs(
		"a.go:F:ARITHMETIC_BASE#1",
		"a.go:F:ARITHMETIC_BASE#2",
		"b.go:G:BRANCH_IF#1",
	)

	got, err := FilterByStableID(ms, "a.go:F:ARITHMETIC_BASE#2")
	if err != nil {
		t.Fatalf("FilterByStableID: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got)=%d, want 1", len(got))
	}
	if got[0].StableID != "a.go:F:ARITHMETIC_BASE#2" {
		t.Errorf("got %q, want the exactly-matching mutant", got[0].StableID)
	}
}

func TestFilterByStableIDUniquePrefix(t *testing.T) {
	ms := mutantsWithIDs(
		"a.go:F:ARITHMETIC_BASE#1",
		"b.go:G:BRANCH_IF#1",
	)

	got, err := FilterByStableID(ms, "b.go:G")
	if err != nil {
		t.Fatalf("FilterByStableID: %v", err)
	}
	if len(got) != 1 || got[0].StableID != "b.go:G:BRANCH_IF#1" {
		t.Fatalf("got %+v, want the single b.go mutant", got)
	}
}

func TestFilterByStableIDExactBeatsPrefix(t *testing.T) {
	// "#1" is a literal prefix of "#10" and "#11". Without exact-match
	// precedence the most ordinary request there is — a function's first
	// mutant of some type — would be rejected as ambiguous.
	ms := mutantsWithIDs(
		"a.go:F:ARITHMETIC_BASE#1",
		"a.go:F:ARITHMETIC_BASE#10",
		"a.go:F:ARITHMETIC_BASE#11",
	)

	got, err := FilterByStableID(ms, "a.go:F:ARITHMETIC_BASE#1")
	if err != nil {
		t.Fatalf("FilterByStableID: %v", err)
	}
	if len(got) != 1 || got[0].StableID != "a.go:F:ARITHMETIC_BASE#1" {
		t.Fatalf("got %+v, want exactly #1", got)
	}
}

func TestFilterByStableIDUnknown(t *testing.T) {
	ms := mutantsWithIDs("a.go:F:ARITHMETIC_BASE#1", "b.go:G:BRANCH_IF#1")

	got, err := FilterByStableID(ms, "nope")
	if err == nil {
		t.Fatalf("expected an error, got %+v", got)
	}
	if got != nil {
		t.Errorf("expected no mutants alongside the error, got %+v", got)
	}
	// The message has to say what was searched and how big the haystack
	// was, so a typo is distinguishable from a scoping mistake.
	for _, want := range []string{"nope", "2 discovered", "--only"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestAnchorName(t *testing.T) {
	// The first occurrence is bare; only a repeat pays the suffix.
	if got := anchorName("init", 1); got != "init" {
		t.Errorf("anchorName(init, 1)=%q, want %q", got, "init")
	}
	if got := anchorName("init", 2); got != "init~2" {
		t.Errorf("anchorName(init, 2)=%q, want %q", got, "init~2")
	}
	if got := anchorName("(*B).Same", 3); got != "(*B).Same~3" {
		t.Errorf("anchorName((*B).Same, 3)=%q, want %q", got, "(*B).Same~3")
	}
}

func TestFuncSpansDisambiguatesRepeatedNames(t *testing.T) {
	// Two `func init()` are legal Go and render to the same name. Left
	// sharing an anchor they would also share an ordinal counter, so an
	// edit inside the first would renumber the second's mutants.
	fset, f := parseSrc(t, "p.go", `package p

func init() {}

func F() {}

func init() {}
`)
	spans := funcSpans(fset, f)
	want := []string{"init", "F", "init~2"}
	if len(spans) != len(want) {
		t.Fatalf("len(spans)=%d, want %d", len(spans), len(want))
	}
	for i, w := range want {
		if spans[i].name != w {
			t.Errorf("spans[%d].name=%q, want %q", i, spans[i].name, w)
		}
	}
}

func TestDiscoverStableIDsSurviveEditsToASameNamedNeighbour(t *testing.T) {
	// Adding a mutation point to the *first* init() must leave the second
	// init()'s ID alone — the guarantee the README states for edits outside
	// a function.
	before := discoverArithmetic(t, map[string]string{"p.go": `package p

func init() { _ = 1 + 2 }

func init() { _ = 3 + 4 }
`})
	after := discoverArithmetic(t, map[string]string{"p.go": `package p

func init() { _ = 1 + 2; _ = 5 + 6 }

func init() { _ = 3 + 4 }
`})

	if len(before) != 2 {
		t.Fatalf("len(before)=%d, want 2", len(before))
	}
	if len(after) != 3 {
		t.Fatalf("len(after)=%d, want 3", len(after))
	}
	if before[0].StableID != "p.go:init:ARITHMETIC_BASE#1" {
		t.Errorf("before[0].StableID=%q, want %q", before[0].StableID, "p.go:init:ARITHMETIC_BASE#1")
	}
	if before[1].StableID != "p.go:init~2:ARITHMETIC_BASE#1" {
		t.Errorf("before[1].StableID=%q, want %q", before[1].StableID, "p.go:init~2:ARITHMETIC_BASE#1")
	}
	// The added mutant takes #2 inside the first init(); the second init()
	// keeps #1 because it counts against its own anchor.
	if after[1].StableID != "p.go:init:ARITHMETIC_BASE#2" {
		t.Errorf("after[1].StableID=%q, want %q", after[1].StableID, "p.go:init:ARITHMETIC_BASE#2")
	}
	if after[2].StableID != before[1].StableID {
		t.Errorf("second init()'s StableID shifted: %q → %q", before[1].StableID, after[2].StableID)
	}
}

func TestStableIDFile(t *testing.T) {
	// The ordinary case: a path under the module root renders relative.
	if got := stableIDFile(filepath.Join("/mod", "internal", "a", "a.go"), "/mod"); got != "internal/a/a.go" {
		t.Errorf("stableIDFile under root=%q, want %q", got, "internal/a/a.go")
	}
	// An empty module root has no relative form, so the absolute path stands
	// in. It still differs per file, which is all the ordinal counter needs.
	abs := filepath.Join("/elsewhere", "a.go")
	if got := stableIDFile(abs, ""); got != filepath.ToSlash(abs) {
		t.Errorf("stableIDFile with empty root=%q, want %q", got, filepath.ToSlash(abs))
	}
}

// discoverArithmeticMulti runs Discover over a module root holding one
// package per entry of pkgFiles (keyed by directory relative to the root),
// scoped to the named packages. It exists to vary the *package argument*
// while holding the source fixed.
func discoverArithmeticMulti(t *testing.T, pkgFiles map[string]map[string]string, scope []string) []mutator.Mutant {
	t.Helper()
	root := t.TempDir()
	byDir := make(map[string]Package, len(pkgFiles))
	for dir, files := range pkgFiles {
		abs := filepath.Join(root, dir)
		if err := os.MkdirAll(abs, 0o755); err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(files))
		for name, src := range files {
			if err := os.WriteFile(filepath.Join(abs, name), []byte(src), 0o644); err != nil {
				t.Fatal(err)
			}
			names = append(names, name)
		}
		byDir[dir] = Package{Dir: abs, ImportPath: "example.com/test/" + dir, GoFiles: names}
	}

	pkgs := make([]Package, 0, len(scope))
	for _, dir := range scope {
		pkgs = append(pkgs, byDir[dir])
	}
	reg := mutator.NewRegistry()
	fset := token.NewFileSet()
	return Discover(fset, pkgs, reg.EnabledMutators([]string{"ARITHMETIC_BASE"}, nil), root, "example.com/test").Mutants
}

func TestDiscoverStableIDsAreIndependentOfThePackageArgument(t *testing.T) {
	// Same source, two scopes: the narrow run mirrors `gomutants ./a/`, the
	// wide one `gomutants ./...`. RelFile is a gremlins-compat path derived
	// from the packages in the run, so it differs between them; the stable
	// ID must not, or an id copied out of a whole-repo report would not
	// resolve under a per-package run.
	files := map[string]map[string]string{
		"a":   {"a.go": "package a\n\nfunc F() int {\n\treturn 1 + 2\n}\n"},
		"cmd": {"main.go": "package cmd\n\nfunc G() int {\n\treturn 3 + 4\n}\n"},
	}

	narrow := discoverArithmeticMulti(t, files, []string{"a"})
	wide := discoverArithmeticMulti(t, files, []string{"a", "cmd"})

	if len(narrow) != 1 {
		t.Fatalf("len(narrow)=%d, want 1", len(narrow))
	}
	if len(wide) != 2 {
		t.Fatalf("len(wide)=%d, want 2", len(wide))
	}

	const wantID = "a/a.go:F:ARITHMETIC_BASE#1"
	if narrow[0].StableID != wantID {
		t.Errorf("narrow StableID=%q, want %q", narrow[0].StableID, wantID)
	}
	if wide[0].StableID != wantID {
		t.Errorf("wide StableID=%q, want %q", wide[0].StableID, wantID)
	}

	// The gremlins-compat RelFile is left alone, and still varies with the
	// scope — which is exactly why the ID needed its own path.
	if narrow[0].RelFile != "a.go" {
		t.Errorf("narrow RelFile=%q, want %q", narrow[0].RelFile, "a.go")
	}
	if wide[0].RelFile != "a/a.go" {
		t.Errorf("wide RelFile=%q, want %q", wide[0].RelFile, "a/a.go")
	}
}

func TestFilterByStableIDAmbiguous(t *testing.T) {
	ms := mutantsWithIDs(
		"a.go:F:ARITHMETIC_BASE#1",
		"a.go:F:ARITHMETIC_BASE#2",
		"b.go:G:BRANCH_IF#1",
	)

	got, err := FilterByStableID(ms, "a.go:F")
	if err == nil {
		t.Fatalf("expected an ambiguity error, got %+v", got)
	}
	if got != nil {
		t.Errorf("expected no mutants alongside the error, got %+v", got)
	}
	msg := err.Error()
	if !strings.Contains(msg, "ambiguous") || !strings.Contains(msg, "2 mutants") {
		t.Errorf("error %q should name the ambiguity and the count", msg)
	}
	// Both candidates must be listed so the user can paste one back.
	for _, want := range []string{"a.go:F:ARITHMETIC_BASE#1", "a.go:F:ARITHMETIC_BASE#2"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing candidate %q", msg, want)
		}
	}
	if strings.Contains(msg, "b.go") {
		t.Errorf("error %q listed a non-matching mutant", msg)
	}
}

func TestFilterByStableIDAmbiguousTruncatesCandidates(t *testing.T) {
	// Seven matches: five listed, the rest summarized.
	ids := make([]string, 0, 7)
	for i := 1; i <= 7; i++ {
		ids = append(ids, fmt.Sprintf("a.go:F:ARITHMETIC_BASE#%d", i))
	}
	_, err := FilterByStableID(mutantsWithIDs(ids...), "a.go:")
	if err == nil {
		t.Fatal("expected an ambiguity error")
	}
	msg := err.Error()
	// On its own line, not trailing the fifth candidate.
	if !strings.Contains(msg, "\n  ... and 2 more") {
		t.Errorf("error %q should summarize the remaining candidates on their own line", msg)
	}
	if strings.Contains(msg, "ARITHMETIC_BASE#6") {
		t.Errorf("error %q listed a candidate past the cap", msg)
	}
	if !strings.Contains(msg, "ARITHMETIC_BASE#5") {
		t.Errorf("error %q should list up to and including the 5th candidate", msg)
	}
}

func TestFilterByStableIDEmptyInput(t *testing.T) {
	got, err := FilterByStableID(nil, "a.go:F:ARITHMETIC_BASE#1")
	if err == nil {
		t.Fatalf("expected an error on an empty mutant list, got %+v", got)
	}
	if !strings.Contains(err.Error(), "0 discovered") {
		t.Errorf("error %q should report an empty haystack", err)
	}
}

func TestFormatCandidates(t *testing.T) {
	// Exact equality, not a Contains check: the layout is the contract.
	// One indented id per line, no leading newline before the first and
	// no trailing newline after the last.
	got := formatCandidates(mutantsWithIDs("a.go:F:T#1", "a.go:F:T#2", "a.go:F:T#3"))
	want := "  a.go:F:T#1\n  a.go:F:T#2\n  a.go:F:T#3"
	if got != want {
		t.Errorf("formatCandidates =\n%q\nwant\n%q", got, want)
	}
}

func TestFormatCandidatesTruncatesExactly(t *testing.T) {
	ids := make([]string, 0, 7)
	for i := 1; i <= 7; i++ {
		ids = append(ids, fmt.Sprintf("a.go:F:T#%d", i))
	}

	// The cap replaces the sixth entry with the summary and stops there —
	// nothing from the tail may leak out past it.
	got := formatCandidates(mutantsWithIDs(ids...))
	want := "  a.go:F:T#1\n  a.go:F:T#2\n  a.go:F:T#3\n  a.go:F:T#4\n  a.go:F:T#5\n  ... and 2 more"
	if got != want {
		t.Errorf("formatCandidates =\n%q\nwant\n%q", got, want)
	}
}

func TestFormatCandidatesSingle(t *testing.T) {
	// One candidate: no separator anywhere.
	got := formatCandidates(mutantsWithIDs("a.go:F:T#1"))
	if got != "  a.go:F:T#1" {
		t.Errorf("formatCandidates = %q, want a single indented line", got)
	}
}

func TestFormatCandidatesExactlyAtCap(t *testing.T) {
	// Exactly maxAmbiguousListed candidates: all listed, no summary line.
	ids := make([]string, 0, maxAmbiguousListed)
	for i := 1; i <= maxAmbiguousListed; i++ {
		ids = append(ids, fmt.Sprintf("a.go:F:T#%d", i))
	}
	got := formatCandidates(mutantsWithIDs(ids...))
	want := "  a.go:F:T#1\n  a.go:F:T#2\n  a.go:F:T#3\n  a.go:F:T#4\n  a.go:F:T#5"
	if got != want {
		t.Errorf("formatCandidates =\n%q\nwant\n%q", got, want)
	}
}

func TestScopeToStableID(t *testing.T) {
	root := filepath.FromSlash("/mod")
	pkgs := []Package{
		{Dir: root, ImportPath: "m", GoFiles: []string{"add.go", "sub.go"}, TestGoFiles: []string{"add_test.go"}},
		{Dir: filepath.Join(root, "internal", "x"), ImportPath: "m/internal/x", GoFiles: []string{"x.go"}},
	}
	tests := []struct {
		id   string
		want [][]string // GoFiles per package, in order
	}{
		{"add.go:Add:ARITHMETIC_BASE#1", [][]string{{"add.go"}, nil}},
		{"add.go:", [][]string{{"add.go"}, nil}},
		{"add.go", [][]string{{"add.go"}, nil}},
		{"ad", [][]string{{"add.go"}, nil}},
		{"", [][]string{{"add.go", "sub.go"}, {"x.go"}}},
		{"internal/", [][]string{nil, {"x.go"}}},
		{"internal/x/x.go:F:CONDITIONALS_NEGATION#2", [][]string{nil, {"x.go"}}},
		// "add.go:" is not a prefix of "add.gox", and vice versa.
		{"add.gox", [][]string{nil, nil}},
		{"nope.go:F", [][]string{nil, nil}},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			got := ScopeToStableID(pkgs, tt.id, root)
			if len(got) != len(pkgs) {
				t.Fatalf("got %d packages, want every one of the %d kept", len(got), len(pkgs))
			}
			for i := range got {
				if fmt.Sprint(got[i].GoFiles) != fmt.Sprint(tt.want[i]) {
					t.Errorf("pkg %d GoFiles = %v, want %v", i, got[i].GoFiles, tt.want[i])
				}
				if got[i].ImportPath != pkgs[i].ImportPath || got[i].Dir != pkgs[i].Dir || len(got[i].TestGoFiles) != len(pkgs[i].TestGoFiles) {
					t.Errorf("pkg %d: only GoFiles may change, got %+v", i, got[i])
				}
			}
		})
	}
	if len(pkgs[0].GoFiles) != 2 {
		t.Errorf("input packages were modified: %v", pkgs[0].GoFiles)
	}
}
