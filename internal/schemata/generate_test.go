package schemata

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/szhekpisov/gomutants/internal/mutator"
)

// discover parses src and turns every registered mutator's candidates into
// mutants, mirroring what internal/discover does at run time but without
// needing a module on disk.
func discover(t *testing.T, src string) (*token.FileSet, *ast.File, []byte, []mutator.Mutant) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "in.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}
	b := []byte(src)
	var ms []mutator.Mutant
	for _, m := range mutator.NewRegistry().Mutators() {
		for _, c := range m.Discover(fset, file, b) {
			ms = append(ms, mutator.Mutant{
				ID:          len(ms) + 1,
				Type:        c.Type,
				Line:        c.Pos.Line,
				Original:    c.Original,
				Replacement: c.Replacement,
				StartOffset: c.StartOffset,
				EndOffset:   c.EndOffset,
			})
		}
	}
	return fset, file, b, ms
}

func generate(t *testing.T, src string) (FileSchema, []fallback, []mutator.Mutant) {
	t.Helper()
	fset, file, b, ms := discover(t, src)
	fs, declined, err := Generate(fset, file, b, ms)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return fs, declined, ms
}

// reasons maps mutant id to the reason it was declined.
func reasons(declined []fallback) map[int]string {
	out := map[int]string{}
	for _, f := range declined {
		out[f.mutant.ID] = f.reason
	}
	return out
}

func TestGenerateProducesParseableSource(t *testing.T) {
	// Exercises every strategy: bool conditions, arithmetic in a return,
	// an emptied if body, an emptied case body, and a range loop.
	const src = `package p

import "fmt"

func Classify(a, b int, xs []int) string {
	if a > b && a != 0 {
		return "gt"
	}
	switch {
	case a < b:
		fmt.Println(a + b)
	default:
		a++
	}
	total := 0
	for _, v := range xs {
		total += v
	}
	for i := 0; i < len(xs); i++ {
		total -= i
	}
	return fmt.Sprintf("%d", total)
}
`
	fs, declined, ms := generate(t, src)
	if len(ms) == 0 {
		t.Fatal("fixture produced no mutants")
	}
	if len(fs.Guards) == 0 {
		t.Fatal("no mutant was schematized")
	}
	// Generate already parses its output, but assert it here too: this is
	// the property the whole build depends on.
	if err := parseFragment(string(fs.Source)); err != nil {
		t.Fatalf("generated source does not parse: %v\n%s", err, fs.Source)
	}
	if got := len(fs.Guards) + len(declined); got != len(ms) {
		t.Errorf("every mutant must be schematized or declined: %d guards + %d declined != %d mutants",
			len(fs.Guards), len(declined), len(ms))
	}
	// Every guard must name a distinct mutant.
	seen := map[int]bool{}
	for _, g := range fs.Guards {
		if seen[g.MutantID] {
			t.Errorf("mutant %d has more than one guard", g.MutantID)
		}
		seen[g.MutantID] = true
		if g.Start < 0 || g.End > len(fs.Source) || g.Start >= g.End {
			t.Errorf("mutant %d: bad guard range [%d:%d) in %d bytes", g.MutantID, g.Start, g.End, len(fs.Source))
		}
	}
}

func TestGenerateGuardsAreFindableInOutput(t *testing.T) {
	const src = `package p

func Add(a, b int) int { return a + b }
`
	fs, _, _ := generate(t, src)
	for _, g := range fs.Guards {
		text := string(fs.Source[g.Start:g.End])
		if !strings.Contains(text, guardFunc) {
			t.Errorf("mutant %d: guard range does not contain the guard call: %q", g.MutantID, text)
		}
	}
}

func TestGenerateDeclinesConstContexts(t *testing.T) {
	const src = `package p

const Size = 4 + 1

var Buf [8]byte

func Use() int { return Size + len(Buf) }
`
	_, declined, ms := generate(t, src)
	declinedIDs := reasons(declined)
	for _, m := range ms {
		// The only schematizable mutants here are inside Use's return.
		if m.Line <= 5 {
			if _, ok := declinedIDs[m.ID]; !ok {
				t.Errorf("mutant %d (%s, line %d) in a constant context was schematized", m.ID, m.Type, m.Line)
			}
		}
	}
}

func TestGenerateDeclinesRecoverInACondition(t *testing.T) {
	// Wrapping a recover() call in a closure silently stops it recovering,
	// so the condition must stay unwrapped.
	const src = `package p

func Safe() (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	return true
}
`
	fs, _, _ := generate(t, src)
	if strings.Contains(string(fs.Source), "func() bool { if "+guardFunc) &&
		strings.Contains(string(fs.Source), "recover()") {
		// Only fails if a bool wrap actually swallowed the recover call.
		for _, line := range strings.Split(string(fs.Source), "\n") {
			if strings.Contains(line, "recover()") && strings.Contains(line, "func() bool") {
				t.Errorf("recover() was wrapped in a closure: %s", strings.TrimSpace(line))
			}
		}
	}
}

func TestGenerateDeclinesOrphaningStatementRemoval(t *testing.T) {
	// Removing the only use of x would today fail to compile
	// ("x declared and not used") and report NOT VIABLE. Schemata keeps the
	// original in the else branch, so it must decline instead of silently
	// turning a NOT VIABLE into a real verdict.
	const src = `package p

import "fmt"

func Show() {
	x := compute()
	fmt.Println(x)
}

func compute() int { return 1 }
`
	_, declined, ms := generate(t, src)
	got := reasons(declined)
	found := false
	for _, m := range ms {
		if m.Type != mutator.StatementRemove || !strings.Contains(m.Original, "Println") {
			continue
		}
		found = true
		if r := got[m.ID]; !strings.Contains(r, "orphan") {
			t.Errorf("mutant %d (%s %q) should have been declined as orphaning, got %q",
				m.ID, m.Type, m.Original, r)
		}
	}
	if !found {
		t.Fatal("fixture produced no STATEMENT_REMOVE on the Println call")
	}
}

func TestGenerateDeclinesFallthroughCaseBody(t *testing.T) {
	const src = `package p

func F(n int) int {
	out := 0
	switch n {
	case 1:
		out = 1
		fallthrough
	case 2:
		out += 2
	}
	return out
}
`
	_, declined, ms := generate(t, src)
	got := reasons(declined)
	for _, m := range ms {
		if m.Type != mutator.BranchCase || !strings.Contains(m.Original, "fallthrough") {
			continue
		}
		if r := got[m.ID]; !strings.Contains(r, "fallthrough") {
			t.Errorf("BRANCH_CASE on a fallthrough body should be declined, got %q", r)
		}
	}
}

// TestGenerateDeclinesOrphanedRangeVariables covers a real miss: `for i, b
// := range src` declares i and b, and emptying the only body that reads
// them leaves "declared and not used". The declaration is a range clause,
// not a `:=`, so a check that only looked at assignments let it through and
// turned a NOT VIABLE into a live verdict.
func TestGenerateDeclinesOrphanedRangeVariables(t *testing.T) {
	const src = `package p

func lineStarts(src []byte) []int {
	starts := []int{0}
	for i, b := range src {
		if b == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}
`
	_, declined, ms := generate(t, src)
	got := reasons(declined)
	found := false
	for _, m := range ms {
		if m.Type != mutator.BranchIf {
			continue
		}
		found = true
		if r := got[m.ID]; !strings.Contains(r, "orphan") {
			t.Errorf("BRANCH_IF emptying the only use of the range variables should be declined, got %q", r)
		}
	}
	if !found {
		t.Fatal("fixture produced no BRANCH_IF mutant")
	}
}

// TestGenerateDeclinesWhenOnlyReaderIsRemoved covers the second half of the
// same miss: being assigned to is not a use. isTTY is declared, then
// written in a branch, then read once — and a check that counted raw
// identifier occurrences saw three and concluded it was still used after
// the single read was mutated away.
func TestGenerateDeclinesWhenOnlyReaderIsRemoved(t *testing.T) {
	const src = `package p

type T struct{ flag bool }

func New(on bool) *T {
	isTTY := false
	if on {
		isTTY = true
	}
	return &T{flag: isTTY}
}
`
	_, declined, ms := generate(t, src)
	got := reasons(declined)
	found := false
	for _, m := range ms {
		// RETURN_ZERO replaces the composite literal, the only read of isTTY.
		if m.Type != mutator.ReturnZero {
			continue
		}
		found = true
		if r := got[m.ID]; !strings.Contains(r, "orphan") {
			t.Errorf("RETURN_ZERO removing the only read of isTTY should be declined, got %q", r)
		}
	}
	if !found {
		t.Fatal("fixture produced no RETURN_ZERO mutant")
	}
}

// TestCountUsesIgnoresWrites pins the rule directly: a plain assignment is
// not a use, a compound assignment is, and an indexed target reads its
// operands.
func TestCountUsesIgnoresWrites(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{"declaration alone", "x := 1\n_ = 0", 0},
		{"plain assignment is not a use", "x := 1\nx = 2\n_ = 0", 0},
		{"compound assignment reads", "x := 1\nx += 2\n_ = 0", 1},
		{"indexed target reads the operand", "x := []int{1}\nx[0] = 2\n_ = 0", 1},
		{"read counts", "x := 1\nprintln(x)", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "in.go", "package p\nfunc g() {\n"+tt.body+"\n}\n", parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			if got := countUses(f, fset, "x", -1, -1); got != tt.want {
				t.Errorf("countUses = %d, want %d", got, tt.want)
			}
		})
	}
}
