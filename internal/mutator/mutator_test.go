package mutator_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strings"
	"testing"

	"github.com/gomutants/gomutants/internal/mutator"
)

func parse(t *testing.T, src string) (*token.FileSet, *ast.File, []byte) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "test.go", src, 0)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	return fset, f, []byte(src)
}

// --- Token-level mutators ---

func TestArithmeticBase(t *testing.T) {
	src := `package p
func f() int { return 1 + 2 - 3 * 4 / 5 % 6 }
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.ArithmeticBase)
	candidates := m.Discover(fset, file, srcBytes)

	// AST walk order depends on tree shape. Just check total count and all swaps are valid.
	if len(candidates) != 5 {
		t.Fatalf("expected 5 candidates, got %d", len(candidates))
	}

	validSwaps := map[string]string{
		"+": "-", "-": "+", "*": "/", "/": "*", "%": "*",
	}
	for i, c := range candidates {
		want, ok := validSwaps[c.Original]
		if !ok {
			t.Errorf("candidate %d: unexpected original %q", i, c.Original)
		} else if c.Replacement != want {
			t.Errorf("candidate %d: %q→%q, want %q→%q", i, c.Original, c.Replacement, c.Original, want)
		}
	}

	for i, c := range candidates {
		if c.Type != mutator.ArithmeticBase {
			t.Errorf("candidate %d: type=%v, want %v", i, c.Type, mutator.ArithmeticBase)
		}
		if c.StartOffset >= c.EndOffset {
			t.Errorf("candidate %d: invalid offset range [%d:%d)", i, c.StartOffset, c.EndOffset)
		}
	}
}

func TestConditionalsBoundary(t *testing.T) {
	src := `package p
func f(a, b int) {
	_ = a < b
	_ = a <= b
	_ = a > b
	_ = a >= b
	_ = a == b
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.ConditionalsBoundary)
	candidates := m.Discover(fset, file, srcBytes)

	// <→<=, <=→<, >→>=, >=→> (== is not boundary)
	if len(candidates) != 4 {
		t.Fatalf("expected 4 candidates, got %d", len(candidates))
	}

	expected := []struct {
		original    string
		replacement string
	}{
		{"<", "<="},
		{"<=", "<"},
		{">", ">="},
		{">=", ">"},
	}
	for i, c := range candidates {
		if c.Original != expected[i].original || c.Replacement != expected[i].replacement {
			t.Errorf("candidate %d: got %q→%q, want %q→%q",
				i, c.Original, c.Replacement, expected[i].original, expected[i].replacement)
		}
	}
}

func TestConditionalsNegation(t *testing.T) {
	src := `package p
func f(a, b int) {
	_ = a == b
	_ = a != b
	_ = a < b
	_ = a >= b
	_ = a > b
	_ = a <= b
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.ConditionalsNegation)
	candidates := m.Discover(fset, file, srcBytes)

	if len(candidates) != 6 {
		t.Fatalf("expected 6 candidates, got %d", len(candidates))
	}

	expected := []struct {
		original    string
		replacement string
	}{
		{"==", "!="},
		{"!=", "=="},
		{"<", ">="},
		{">=", "<"},
		{">", "<="},
		{"<=", ">"},
	}
	for i, c := range candidates {
		if c.Original != expected[i].original || c.Replacement != expected[i].replacement {
			t.Errorf("candidate %d: got %q→%q, want %q→%q",
				i, c.Original, c.Replacement, expected[i].original, expected[i].replacement)
		}
	}
}

func TestIncrementDecrement(t *testing.T) {
	src := `package p
func f() {
	x := 0
	x++
	x--
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.IncrementDecrement)
	candidates := m.Discover(fset, file, srcBytes)

	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(candidates))
	}

	if candidates[0].Original != "++" || candidates[0].Replacement != "--" {
		t.Errorf("candidate 0: got %q→%q, want \"++\"→\"--\"", candidates[0].Original, candidates[0].Replacement)
	}
	if candidates[1].Original != "--" || candidates[1].Replacement != "++" {
		t.Errorf("candidate 1: got %q→%q, want \"--\"→\"++\"", candidates[1].Original, candidates[1].Replacement)
	}
}

func TestInvertNegatives(t *testing.T) {
	src := `package p
func f() int { return -42 + -1 }
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.InvertNegatives)
	candidates := m.Discover(fset, file, srcBytes)

	// Two unary negatives: -42 and -1.
	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(candidates))
	}

	for i, c := range candidates {
		if c.Original != "-" || c.Replacement != "+" {
			t.Errorf("candidate %d: got %q→%q, want \"-\"→\"+\"", i, c.Original, c.Replacement)
		}
		if c.EndOffset-c.StartOffset != 1 {
			t.Errorf("candidate %d: byte length=%d, want 1", i, c.EndOffset-c.StartOffset)
		}
	}
}

func TestInvertNegativesBinary(t *testing.T) {
	src := `package p
func f(a, b int) int { return a - b }
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.InvertNegatives)
	candidates := m.Discover(fset, file, srcBytes)

	// Binary subtraction also produces an INVERT_NEGATIVES candidate (matches gremlins).
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}

	if candidates[0].Original != "-" || candidates[0].Replacement != "+" {
		t.Errorf("got %q→%q, want \"-\"→\"+\"", candidates[0].Original, candidates[0].Replacement)
	}
}

// --- Compound-assignment / bitwise / logical / loop-ctrl mutators ---

func TestInvertAssignments(t *testing.T) {
	src := `package p
func f(a, b int) {
	a += b
	a -= b
	a *= b
	a /= b
	a %= b
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.InvertAssignments)
	candidates := m.Discover(fset, file, srcBytes)

	if len(candidates) != 5 {
		t.Fatalf("expected 5 candidates, got %d", len(candidates))
	}

	expected := map[string]string{
		"+=": "-=", "-=": "+=", "*=": "/=", "/=": "*=", "%=": "*=",
	}
	for i, c := range candidates {
		want, ok := expected[c.Original]
		if !ok {
			t.Errorf("candidate %d: unexpected original %q", i, c.Original)
		} else if c.Replacement != want {
			t.Errorf("candidate %d: %q→%q, want %q→%q", i, c.Original, c.Replacement, c.Original, want)
		}
		if c.Type != mutator.InvertAssignments {
			t.Errorf("candidate %d: type=%v, want %v", i, c.Type, mutator.InvertAssignments)
		}
	}
}

func TestInvertAssignmentsSkipsPlainAssign(t *testing.T) {
	src := `package p
func f() {
	x := 0
	x = 1
	_ = x
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.InvertAssignments)
	if got := m.Discover(fset, file, srcBytes); len(got) != 0 {
		t.Errorf("expected 0 candidates for plain/short assigns, got %d", len(got))
	}
}

func TestInvertBitwise(t *testing.T) {
	src := `package p
func f(a, b uint) uint {
	_ = a & b
	_ = a | b
	_ = a ^ b
	_ = a &^ b
	_ = a << 1
	_ = a >> 1
	return 0
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.InvertBitwise)
	candidates := m.Discover(fset, file, srcBytes)

	if len(candidates) != 6 {
		t.Fatalf("expected 6 candidates, got %d", len(candidates))
	}

	expected := map[string]string{
		"&": "|", "|": "&", "^": "&", "&^": "&", "<<": ">>", ">>": "<<",
	}
	for i, c := range candidates {
		want, ok := expected[c.Original]
		if !ok {
			t.Errorf("candidate %d: unexpected original %q", i, c.Original)
		} else if c.Replacement != want {
			t.Errorf("candidate %d: %q→%q, want %q→%q", i, c.Original, c.Replacement, c.Original, want)
		}
	}
}

func TestInvertBitwiseSkipsArithmetic(t *testing.T) {
	src := `package p
func f(a, b int) int { return a + b }
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.InvertBitwise)
	if got := m.Discover(fset, file, srcBytes); len(got) != 0 {
		t.Errorf("expected 0 candidates for arithmetic source, got %d", len(got))
	}
}

func TestInvertBitwiseSkipsGenericConstraints(t *testing.T) {
	// The `|` in each generic type constraint is a union, parsed as the same
	// *ast.BinaryExpr{Op: token.OR} as a bitwise OR. None of them must yield a
	// candidate; only the real bitwise OR in g's body may. Covers all three
	// constraint sites: interface element, type-decl type params, func type
	// params — with and without the `~` approximation. Each interface and
	// type-param list holds multiple elements (and the union is not always
	// first) so a loop-break/short-circuit mutation on the collection walk
	// would leave a later union unmarked and surface as an extra candidate.
	src := `package p
type IntOrString interface { comparable; ~int | ~string }
type PlainUnion interface { int | string; ~float64 | ~float32 }
type Box[K int | string, V uint | uint64] struct{ k K; v V }
func h[T ~int | ~uint, U int | string](x T) T { return x }
func g(a, b uint) uint { return a | b }
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.InvertBitwise)
	candidates := m.Discover(fset, file, srcBytes)

	if len(candidates) != 1 {
		t.Fatalf("expected exactly 1 candidate (the bitwise OR in g), got %d: %+v", len(candidates), candidates)
	}
	if c := candidates[0]; c.Original != "|" || c.Replacement != "&" {
		t.Errorf("candidate = %q→%q, want |→&", c.Original, c.Replacement)
	}
	// The surviving candidate must be g's runtime OR, not any constraint union.
	wantLine := 1 + strings.Count(src[:strings.Index(src, "return a | b")], "\n")
	if candidates[0].Pos.Line != wantLine {
		t.Errorf("candidate on line %d, want the bitwise OR in g on line %d", candidates[0].Pos.Line, wantLine)
	}
}

func TestInvertBitwiseAssignments(t *testing.T) {
	src := `package p
func f(a, b uint) {
	a &= b
	a |= b
	a ^= b
	a &^= b
	a <<= 1
	a >>= 1
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.InvertBitwiseAssignments)
	candidates := m.Discover(fset, file, srcBytes)

	if len(candidates) != 6 {
		t.Fatalf("expected 6 candidates, got %d", len(candidates))
	}

	expected := map[string]string{
		"&=": "|=", "|=": "&=", "^=": "&=", "&^=": "&=", "<<=": ">>=", ">>=": "<<=",
	}
	for i, c := range candidates {
		want, ok := expected[c.Original]
		if !ok {
			t.Errorf("candidate %d: unexpected original %q", i, c.Original)
		} else if c.Replacement != want {
			t.Errorf("candidate %d: %q→%q, want %q→%q", i, c.Original, c.Replacement, c.Original, want)
		}
	}
}

func TestInvertLogical(t *testing.T) {
	src := `package p
func f(a, b bool) bool {
	return a && b || a
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.InvertLogical)
	candidates := m.Discover(fset, file, srcBytes)

	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(candidates))
	}

	expected := map[string]string{"&&": "||", "||": "&&"}
	for i, c := range candidates {
		want, ok := expected[c.Original]
		if !ok {
			t.Errorf("candidate %d: unexpected original %q", i, c.Original)
		} else if c.Replacement != want {
			t.Errorf("candidate %d: %q→%q, want %q→%q", i, c.Original, c.Replacement, c.Original, want)
		}
	}
}

func TestInvertLoopCtrl(t *testing.T) {
	src := `package p
func f() {
	for i := 0; i < 10; i++ {
		if i == 5 {
			break
		}
		if i == 3 {
			continue
		}
	}
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.InvertLoopCtrl)
	candidates := m.Discover(fset, file, srcBytes)

	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(candidates))
	}

	if candidates[0].Original != "break" || candidates[0].Replacement != "continue" {
		t.Errorf("candidate 0: got %q→%q, want \"break\"→\"continue\"", candidates[0].Original, candidates[0].Replacement)
	}
	if candidates[1].Original != "continue" || candidates[1].Replacement != "break" {
		t.Errorf("candidate 1: got %q→%q, want \"continue\"→\"break\"", candidates[1].Original, candidates[1].Replacement)
	}
}

func TestInvertLoopCtrlSkipsLabelled(t *testing.T) {
	src := `package p
func f() {
Outer:
	for {
		for {
			break Outer
		}
	}
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.InvertLoopCtrl)
	if got := m.Discover(fset, file, srcBytes); len(got) != 0 {
		t.Errorf("expected 0 candidates for labelled break, got %d", len(got))
	}
}

func TestInvertLoopCtrlSkipsGotoFallthrough(t *testing.T) {
	src := `package p
func f(x int) int {
	switch x {
	case 1:
		fallthrough
	case 2:
		goto end
	}
end:
	return 0
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.InvertLoopCtrl)
	if got := m.Discover(fset, file, srcBytes); len(got) != 0 {
		t.Errorf("expected 0 candidates for goto/fallthrough, got %d", len(got))
	}
}

func TestRemoveSelfAssignments(t *testing.T) {
	src := `package p
func f(a, b uint) {
	a += b
	a -= b
	a *= b
	a /= b
	a %= b
	a &= b
	a |= b
	a ^= b
	a &^= b
	a <<= 1
	a >>= 1
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.RemoveSelfAssignments)
	candidates := m.Discover(fset, file, srcBytes)

	if len(candidates) != 11 {
		t.Fatalf("expected 11 candidates (one per compound op), got %d", len(candidates))
	}

	for i, c := range candidates {
		if c.Replacement != "=" {
			t.Errorf("candidate %d: replacement=%q, want %q", i, c.Replacement, "=")
		}
	}
}

func TestRemoveSelfAssignmentsSkipsPlainAssign(t *testing.T) {
	src := `package p
func f() {
	x := 0
	x = 1
	_ = x
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.RemoveSelfAssignments)
	if got := m.Discover(fset, file, srcBytes); len(got) != 0 {
		t.Errorf("expected 0 candidates for plain/short assigns, got %d", len(got))
	}
}

func TestRemoveLogicalNot(t *testing.T) {
	src := `package p
func f(ok bool, s string) bool {
	if !ok {
		return false
	}
	_ = !isEmpty(s)
	return !ok
}

func isEmpty(s string) bool { return s == "" }
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.RemoveLogicalNot)
	candidates := m.Discover(fset, file, srcBytes)

	if len(candidates) != 3 {
		t.Fatalf("expected 3 candidates (!ok, !isEmpty(s), !ok), got %d", len(candidates))
	}
	for i, c := range candidates {
		if c.Original != "!" {
			t.Errorf("candidate %d: original=%q, want %q", i, c.Original, "!")
		}
		if c.Replacement != "" {
			t.Errorf("candidate %d: replacement=%q, want empty", i, c.Replacement)
		}
		if got := string(srcBytes[c.StartOffset:c.EndOffset]); got != "!" {
			t.Errorf("candidate %d: source at [%d:%d) is %q, want %q", i, c.StartOffset, c.EndOffset, got, "!")
		}
	}
}

// TestRemoveLogicalNotSkipsNegatedComparison covers the dedup against
// CONDITIONALS_NEGATION: `!(a == b)` and `!(a != b)` produce the same
// behaviour under either mutator, so only one of them may emit.
func TestRemoveLogicalNotSkipsNegatedComparison(t *testing.T) {
	src := `package p
func f(a, b int) {
	_ = !(a == b)
	_ = !(a != b)
	_ = !(a < b)
	_ = !(a <= b)
	_ = !(a > b)
	_ = !(a >= b)
	_ = !((a == b))
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.RemoveLogicalNot)
	if got := m.Discover(fset, file, srcBytes); len(got) != 0 {
		t.Errorf("expected 0 candidates for negated comparisons, got %d", len(got))
	}
}

// TestRemoveLogicalNotMutatesNegatedLogical guards the other side of that
// dedup: `&&` / `||` are not in the negation-swap set, so negating a logical
// expression is not a duplicate and must still emit.
func TestRemoveLogicalNotMutatesNegatedLogical(t *testing.T) {
	src := `package p
func f(a, b bool) {
	_ = !(a && b)
	_ = !(a || b)
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.RemoveLogicalNot)
	if got := m.Discover(fset, file, srcBytes); len(got) != 2 {
		t.Errorf("expected 2 candidates for negated logical exprs, got %d", len(got))
	}
}

// TestRemoveLogicalNotSkipsUnaryMinus keeps the mutator off arithmetic
// negation, which is INVERT_NEGATIVES' territory.
func TestRemoveLogicalNotSkipsUnaryMinus(t *testing.T) {
	src := `package p
func f(a int) int {
	b := -a
	c := ^a
	return b + c
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.RemoveLogicalNot)
	if got := m.Discover(fset, file, srcBytes); len(got) != 0 {
		t.Errorf("expected 0 candidates for unary minus/xor, got %d", len(got))
	}
}

// TestRemoveLogicalNotDescendsPastSkippedNodes pins the visitor's traversal.
// Each early return in the callback must keep walking the subtree: a
// negation nests freely inside a unary expression of another operator,
// inside a negated comparison that is itself skipped, and inside the operand
// of a negation that was just emitted.
func TestRemoveLogicalNotDescendsPastSkippedNodes(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{"inside another negation's operand", `_ = !count(!ok)`, 2},
		{"inside a non-NOT unary expression", `_ = -count(!ok)`, 1},
		{"inside a skipped negated comparison", `_ = !(count(!ok) == 0)`, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := "package p\n\nfunc count(b bool) int { return 0 }\n\nfunc f(ok bool) {\n\t" + tt.body + "\n}\n"
			fset, file, srcBytes := parse(t, src)
			m := findMutator(t, mutator.RemoveLogicalNot)
			if got := m.Discover(fset, file, srcBytes); len(got) != tt.want {
				t.Errorf("expected %d candidates, got %d", tt.want, len(got))
			}
		})
	}
}

// TestRemoveLogicalNotPatchCompiles asserts the byte range is exactly the
// `!`, so splicing it out leaves parseable source even with a space between
// the operator and its operand.
func TestRemoveLogicalNotPatchCompiles(t *testing.T) {
	src := `package p
func f(ok bool) bool {
	if ! ok {
		return false
	}
	return true
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.RemoveLogicalNot)
	candidates := m.Discover(fset, file, srcBytes)
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	c := candidates[0]
	mutated := string(srcBytes[:c.StartOffset]) + c.Replacement + string(srcBytes[c.EndOffset:])
	if !strings.Contains(mutated, "if  ok {") {
		t.Errorf("mutated source lost the operand: %q", mutated)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "mutated.go", mutated, 0); err != nil {
		t.Errorf("mutated source does not parse: %v\n%s", err, mutated)
	}
}

func TestErrorfWrap(t *testing.T) {
	src := `package p

import "fmt"

func f(err error) error {
	return fmt.Errorf("load config: %w", err)
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.ErrorfWrap)
	candidates := m.Discover(fset, file, srcBytes)

	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	c := candidates[0]
	if c.Original != "%w" || c.Replacement != "%v" {
		t.Errorf("got %q→%q, want %q→%q", c.Original, c.Replacement, "%w", "%v")
	}
	if got := string(srcBytes[c.StartOffset:c.EndOffset]); got != "%w" {
		t.Errorf("source at [%d:%d) is %q, want %q", c.StartOffset, c.EndOffset, got, "%w")
	}
	if c.Pos.Line != 6 {
		t.Errorf("line=%d, want 6", c.Pos.Line)
	}
}

func TestErrorfWrapMultipleVerbs(t *testing.T) {
	src := `package p

import "fmt"

func f(a, b error) error {
	return fmt.Errorf("both: %w and %w", a, b)
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.ErrorfWrap)
	candidates := m.Discover(fset, file, srcBytes)

	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(candidates))
	}
	if candidates[0].StartOffset >= candidates[1].StartOffset {
		t.Errorf("candidates not in source order: %d, %d", candidates[0].StartOffset, candidates[1].StartOffset)
	}
	for i, c := range candidates {
		if got := string(srcBytes[c.StartOffset:c.EndOffset]); got != "%w" {
			t.Errorf("candidate %d: source at [%d:%d) is %q, want %q", i, c.StartOffset, c.EndOffset, got, "%w")
		}
	}
}

// TestErrorfWrapSkipsEscapedPercent covers the `%%` scan: `%%w` formats as a
// literal "%w" and wraps nothing, so rewriting it would be a message-only
// change no test should have to catch.
func TestErrorfWrapSkipsEscapedPercent(t *testing.T) {
	src := `package p

import "fmt"

func f() error {
	return fmt.Errorf("literal %%w only")
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.ErrorfWrap)
	if got := m.Discover(fset, file, srcBytes); len(got) != 0 {
		t.Errorf("expected 0 candidates for %%%%w, got %d", len(got))
	}
}

// TestErrorfWrapEscapedThenReal makes sure consuming `%%` does not swallow a
// following genuine verb: `%%%w` is a literal percent then a wrap verb.
func TestErrorfWrapEscapedThenReal(t *testing.T) {
	src := `package p

import "fmt"

func f(err error) error {
	return fmt.Errorf("100%%%w", err)
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.ErrorfWrap)
	candidates := m.Discover(fset, file, srcBytes)
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	if got := string(srcBytes[candidates[0].StartOffset:candidates[0].EndOffset]); got != "%w" {
		t.Errorf("source at candidate offsets is %q, want %q", got, "%w")
	}
}

func TestErrorfWrapSkipsNonWrappingCalls(t *testing.T) {
	src := `package p

import "fmt"

func f(err error) error {
	fmt.Printf("progress %w", err)
	_ = fmt.Sprintf("value %w", err)
	return fmt.Errorf("no verbs here")
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.ErrorfWrap)
	if got := m.Discover(fset, file, srcBytes); len(got) != 0 {
		t.Errorf("expected 0 candidates, got %d", len(got))
	}
}

// TestErrorfWrapMatchesAlternativeShapes covers the syntactic match: a bare
// `Errorf` ident, an aliased/third-party package selector, and the
// context-first argument order all reach the same format string.
func TestErrorfWrapMatchesAlternativeShapes(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"bare ident", `return Errorf("x: %w", err)`},
		{"aliased package", `return xerrors.Errorf("x: %w", err)`},
		{"context first", `return log.Errorf(ctx, "x: %w", err)`},
		{"method on receiver", `return e.wrapper.Errorf("x: %w", err)`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := "package p\nfunc f() error {\n\t" + tt.body + "\n}\n"
			fset, file, srcBytes := parse(t, src)
			m := findMutator(t, mutator.ErrorfWrap)
			if got := m.Discover(fset, file, srcBytes); len(got) != 1 {
				t.Errorf("expected 1 candidate, got %d", len(got))
			}
		})
	}
}

// TestErrorfWrapSkipsNonLiteralFormat covers the case where the format
// string is not statically known — there is no source range to patch.
func TestErrorfWrapSkipsNonLiteralFormat(t *testing.T) {
	src := `package p

import "fmt"

const format = "x: %w"

func f(err error) error {
	return fmt.Errorf(format, err)
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.ErrorfWrap)
	if got := m.Discover(fset, file, srcBytes); len(got) != 0 {
		t.Errorf("expected 0 candidates for non-literal format, got %d", len(got))
	}
}

// TestErrorfWrapUsesFirstStringArgOnly guards against mutating a `%w` that
// appears in a format *operand* rather than the format string, where it is
// literal text and changes the message without affecting wrapping.
func TestErrorfWrapUsesFirstStringArgOnly(t *testing.T) {
	src := `package p

import "fmt"

func f(err error) error {
	return fmt.Errorf("%s: %w", "literal %w text", err)
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.ErrorfWrap)
	candidates := m.Discover(fset, file, srcBytes)
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	// The one candidate must be inside the format string, which ends before
	// the second argument begins.
	secondArg := strings.Index(src, `"literal`)
	if candidates[0].StartOffset > secondArg {
		t.Errorf("candidate at offset %d is in the operand, not the format string", candidates[0].StartOffset)
	}
}

// TestErrorfWrapDescendsPastSkippedNodes pins the visitor's traversal: every
// early return in the callback must keep walking the subtree, because a
// wrapping Errorf is routinely nested inside a node the callback rejects.
func TestErrorfWrapDescendsPastSkippedNodes(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		// Rejected because the outer node is not an Errorf call at all.
		{"inside a non-Errorf call", `return wrap(fmt.Errorf("inner: %w", err))`},
		// Rejected because the outer Errorf's format is not a literal.
		{"inside a non-literal-format Errorf", `return fmt.Errorf(format, fmt.Errorf("inner: %w", err))`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := "package p\n\nimport \"fmt\"\n\nvar format string\n\nfunc wrap(e error) error { return e }\n\nfunc f(err error) error {\n\t" + tt.body + "\n}\n"
			fset, file, srcBytes := parse(t, src)
			m := findMutator(t, mutator.ErrorfWrap)
			if got := m.Discover(fset, file, srcBytes); len(got) != 1 {
				t.Errorf("expected the nested Errorf to be found, got %d candidates", len(got))
			}
		})
	}
}

// TestErrorfWrapNestedErrorfCalls covers descent past a *matched* call: the
// callback must keep walking after emitting, or a wrapped Errorf inside the
// arguments of another Errorf goes unseen.
func TestErrorfWrapNestedErrorfCalls(t *testing.T) {
	src := `package p

import "fmt"

func f(err error) error {
	return fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", err))
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.ErrorfWrap)
	if got := m.Discover(fset, file, srcBytes); len(got) != 2 {
		t.Errorf("expected 2 candidates (outer and inner), got %d", len(got))
	}
}

// TestErrorfWrapNonErrorfIdent covers the bare-ident arm of isErrorfCall
// rejecting a name that is not Errorf.
func TestErrorfWrapNonErrorfIdent(t *testing.T) {
	src := `package p

func annotate(format string, err error) error { return err }

func f(err error) error {
	return annotate("x: %w", err)
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.ErrorfWrap)
	if got := m.Discover(fset, file, srcBytes); len(got) != 0 {
		t.Errorf("expected 0 candidates for a non-Errorf ident call, got %d", len(got))
	}
}

// TestErrorfWrapUncallableFunShape covers the default arm of isErrorfCall:
// a call whose Fun is neither an ident nor a selector has no name to match.
func TestErrorfWrapUncallableFunShape(t *testing.T) {
	src := `package p

var handlers []func(string, error) error

func f(err error) error {
	return handlers[0]("x: %w", err)
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.ErrorfWrap)
	if got := m.Discover(fset, file, srcBytes); len(got) != 0 {
		t.Errorf("expected 0 candidates for an indexed call target, got %d", len(got))
	}
}

// TestErrorfWrapRawStringPosition checks the token.Pos arithmetic: inside a
// multi-line raw string the reported line must be the verb's line, not the
// literal's opening line.
func TestErrorfWrapRawStringPosition(t *testing.T) {
	src := "package p\n\nimport \"fmt\"\n\nfunc f(err error) error {\n\treturn fmt.Errorf(`multi\nline: %w`, err)\n}\n"
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.ErrorfWrap)
	candidates := m.Discover(fset, file, srcBytes)
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	c := candidates[0]
	if c.Pos.Line != 7 {
		t.Errorf("line=%d, want 7 (the verb's line, not the literal's start)", c.Pos.Line)
	}
	if got := string(srcBytes[c.StartOffset:c.EndOffset]); got != "%w" {
		t.Errorf("source at [%d:%d) is %q, want %q", c.StartOffset, c.EndOffset, got, "%w")
	}
}

// --- Block-level mutators ---

func TestBranchIf(t *testing.T) {
	src := `package p
func f(x int) int {
	if x > 0 {
		return x
	}
	return 0
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.BranchIf)
	candidates := m.Discover(fset, file, srcBytes)

	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}

	c := candidates[0]
	if c.Replacement != "{ _ = 0 }" {
		t.Errorf("replacement=%q, want %q", c.Replacement, "{ _ = 0 }")
	}
	if c.Type != mutator.BranchIf {
		t.Errorf("type=%v, want %v", c.Type, mutator.BranchIf)
	}
}

func TestBranchIfElseIf(t *testing.T) {
	src := `package p
func f(x int) int {
	if x > 0 {
		return 1
	} else if x < 0 {
		return -1
	}
	return 0
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.BranchIf)
	candidates := m.Discover(fset, file, srcBytes)

	// Both the "if" body and the "else if" body should be candidates.
	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(candidates))
	}
}

func TestBranchElse(t *testing.T) {
	src := `package p
func f(x int) int {
	if x > 0 {
		return x
	} else {
		return 0
	}
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.BranchElse)
	candidates := m.Discover(fset, file, srcBytes)

	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}

	if candidates[0].Replacement != "{ _ = 0 }" {
		t.Errorf("replacement=%q, want %q", candidates[0].Replacement, "{ _ = 0 }")
	}
}

func TestBranchElseSkipsElseIf(t *testing.T) {
	src := `package p
func f(x int) int {
	if x > 0 {
		return 1
	} else if x < 0 {
		return -1
	}
	return 0
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.BranchElse)
	candidates := m.Discover(fset, file, srcBytes)

	// else-if is not a plain else block, so no candidates.
	if len(candidates) != 0 {
		t.Fatalf("expected 0 candidates for else-if chain, got %d", len(candidates))
	}
}

func TestBranchCase(t *testing.T) {
	src := `package p
func f(x int) int {
	switch x {
	case 1:
		return 10
	case 2:
		return 20
	default:
		return 0
	}
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.BranchCase)
	candidates := m.Discover(fset, file, srcBytes)

	if len(candidates) != 3 {
		t.Fatalf("expected 3 candidates, got %d", len(candidates))
	}

	for i, c := range candidates {
		if c.Replacement != "_ = 0" {
			t.Errorf("candidate %d: replacement=%q, want %q", i, c.Replacement, "_ = 0")
		}
	}
}

func TestExpressionRemove(t *testing.T) {
	src := `package p
func f(a, b bool) bool {
	return a && b
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.ExpressionRemove)
	candidates := m.Discover(fset, file, srcBytes)

	// && produces 2 candidates: replace a with true, replace b with true.
	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(candidates))
	}

	if candidates[0].Replacement != "true" {
		t.Errorf("candidate 0: replacement=%q, want \"true\"", candidates[0].Replacement)
	}
	if candidates[1].Replacement != "true" {
		t.Errorf("candidate 1: replacement=%q, want \"true\"", candidates[1].Replacement)
	}
}

func TestExpressionRemoveOr(t *testing.T) {
	src := `package p
func f(a, b bool) bool {
	return a || b
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.ExpressionRemove)
	candidates := m.Discover(fset, file, srcBytes)

	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(candidates))
	}

	for i, c := range candidates {
		if c.Replacement != "false" {
			t.Errorf("candidate %d: replacement=%q, want \"false\"", i, c.Replacement)
		}
	}
}

func TestStatementRemoveAssign(t *testing.T) {
	src := `package p
func f() int {
	x := 0
	x = 42
	return x
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.StatementRemove)
	candidates := m.Discover(fset, file, srcBytes)

	// Only "x = 42" is a plain assign. ":=" is skipped.
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}

	if candidates[0].Replacement != "_ = 42" {
		t.Errorf("replacement=%q, want \"_ = 42\"", candidates[0].Replacement)
	}
}

func TestStatementRemoveExprStmt(t *testing.T) {
	src := `package p
func f() {
	println("hello")
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.StatementRemove)
	candidates := m.Discover(fset, file, srcBytes)

	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}

	if candidates[0].Replacement != "_ = 0" {
		t.Errorf("replacement=%q, want \"_ = 0\"", candidates[0].Replacement)
	}
}

func TestStatementRemoveIncDec(t *testing.T) {
	src := `package p
func f() int {
	x := 0
	x++
	return x
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.StatementRemove)
	candidates := m.Discover(fset, file, srcBytes)

	// x++ is also an IncDecStmt.
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}

	if candidates[0].Replacement != "_ = x" {
		t.Errorf("replacement=%q, want \"_ = x\"", candidates[0].Replacement)
	}
}

// TestStatementRemoveSkipsBlankLhs covers the early-return added so that
// "_ = expr" doesn't produce a candidate whose replacement is identical to
// the original (a phantom LIVED mutant). Without the guard, both expressions
// inside this function would surface as STATEMENT_REMOVE candidates.
func TestStatementRemoveSkipsBlankLhs(t *testing.T) {
	src := `package p
func f(x int) int {
	_ = x
	_ = 1 + 2
	y := x
	return y
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.StatementRemove)
	candidates := m.Discover(fset, file, srcBytes)
	for _, c := range candidates {
		if c.Original == c.Replacement {
			t.Errorf("found phantom mutation (original==replacement): %q at offset %d", c.Original, c.StartOffset)
		}
	}
}

// TestStatementRemoveMultiLhsNotSkipped kills BRANCH_IF on the
// `if len(lhs) != 1 { return false }` guard inside isBlankLhs. Without
// the early return, a multi-LHS assignment whose first slot happens to
// be `_` (e.g. `_, b = c, d`) would also be classified as "blank LHS"
// and the candidate would be skipped — even though the assignment as a
// whole has real side effects on `b` and is a legitimate STATEMENT_REMOVE
// target.
func TestStatementRemoveMultiLhsNotSkipped(t *testing.T) {
	src := `package p
func f() int {
	var b int
	_, b = 1, 2
	return b
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.StatementRemove)
	candidates := m.Discover(fset, file, srcBytes)
	found := false
	for _, c := range candidates {
		if strings.HasPrefix(c.Original, "_, b") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a candidate for `_, b = 1, 2`; BRANCH_IF on isBlankLhs's len-check elides the early-return and lhs[0]=`_` makes the multi-LHS look blank")
	}
}

func TestStatementRemoveSkipsShortDecl(t *testing.T) {
	src := `package p
func f() {
	x := 42
	_ = x
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.StatementRemove)
	candidates := m.Discover(fset, file, srcBytes)

	// ":=" is skipped (short decl), "_ = x" is also skipped (blank LHS
	// would yield a phantom mutation identical to the original).
	if len(candidates) != 0 {
		t.Fatalf("expected 0 candidates, got %d", len(candidates))
	}
}

// --- Numeric-literal increment / decrement ---

func TestIntegerIncrement(t *testing.T) {
	// 0xFF is reformatted as decimal — keeps the replacement unambiguous.
	assertReplacements(t, mutator.IntegerIncrement,
		"package p\nfunc f() int { return 1 + 7 + 0xFF }\n",
		[]replacementCase{{"1", "2"}, {"7", "8"}, {"0xFF", "256"}})
}

func TestIntegerIncrementSkipsFloatsAndImaginaries(t *testing.T) {
	requireCandidates(t, mutator.IntegerIncrement,
		"package p\nfunc f() complex128 { _ = 3.14; return 1i }\n", 0)
}

func TestIntegerIncrementHandlesUnderscores(t *testing.T) {
	assertReplacements(t, mutator.IntegerIncrement,
		"package p\nfunc f() int { return 1_000 }\n",
		[]replacementCase{{"1_000", "1001"}})
}

// TestIntegerIncrementSkipsMaxInt64 kills the mutation that widens the
// signed-overflow guard `delta > 0` to a tautology (e.g. `delta > -1`): with
// the guard disabled the helper would silently return a wrapped MinInt64
// instead of dropping the candidate.
func TestIntegerIncrementSkipsMaxInt64(t *testing.T) {
	requireCandidates(t, mutator.IntegerIncrement,
		"package p\nfunc f() int64 { return 9223372036854775807 }\n", 0)
}

// TestIntegerIncrementLargeLiteral kills the mutation that narrows
// strconv.ParseInt's bitSize from 64 to 63: a literal between 2^62 and
// 2^63-1 fits an int64 but not a signed 63-bit value, so bitSize=63 would
// reject it and drop the candidate.
func TestIntegerIncrementLargeLiteral(t *testing.T) {
	assertReplacements(t, mutator.IntegerIncrement,
		"package p\nfunc f() int64 { return 5000000000000000000 }\n",
		[]replacementCase{{"5000000000000000000", "5000000000000000001"}})
}

// TestIntegerIncrementSkipsUnparseable asserts the contract that an
// integer literal exceeding int64 range produces no candidate. The
// BRANCH_IF mutation on the `if err != nil` body is *equivalent* for
// IntegerIncrement (ParseInt returns MaxInt64 + ErrRange, +1 wraps to
// MinInt64, sign-flip guard drops the candidate either way) — this
// test alone does not kill it. TestIntegerDecrementSkipsUnparseable
// below is what actually kills the BRANCH_IF.
func TestIntegerIncrementSkipsUnparseable(t *testing.T) {
	requireCandidates(t, mutator.IntegerIncrement,
		"package p\nfunc f() { const x = 99999999999999999999; _ = x }\n", 0)
}

// TestIntegerDecrementSkipsUnparseable kills the BRANCH_IF on the
// `if err != nil` body in mutateNumericLiteral. ParseInt returns
// (MaxInt64, ErrRange) for out-of-range positive literals; with the
// early-return mutated away, the decrement (delta=-1) does NOT trigger
// the `delta > 0 && result < v` sign-flip guard, so a bogus
// "9223372036854775806" candidate would be emitted for a literal that
// should drop. The +1 direction has its own equivalent sign-flip path,
// so only the decrement case exposes the mutation.
func TestIntegerDecrementSkipsUnparseable(t *testing.T) {
	requireCandidates(t, mutator.IntegerDecrement,
		"package p\nfunc f() { const x = 99999999999999999999; _ = x }\n", 0)
}

// TestFloatIncrementSkipsUnparseable kills the BRANCH_IF on the float
// err-return: a literal that exceeds float64 range must drop the
// candidate, not emit "+Inf+1" garbage.
func TestFloatIncrementSkipsUnparseable(t *testing.T) {
	requireCandidates(t, mutator.FloatIncrement,
		"package p\nfunc f() float64 { const x = 1e10000; return x }\n", 0)
}

func TestIntegerDecrement(t *testing.T) {
	assertReplacements(t, mutator.IntegerDecrement,
		"package p\nfunc f() int { return 1 + 7 + 0 }\n",
		[]replacementCase{{"1", "0"}, {"7", "6"}, {"0", "-1"}})
}

func TestFloatIncrement(t *testing.T) {
	// 0.0 → 1.0 must stay a float literal, not collapse to "1".
	assertReplacements(t, mutator.FloatIncrement,
		"package p\nfunc f() float64 { return 1.5 + 0.0 }\n",
		[]replacementCase{{"1.5", "2.5"}, {"0.0", "1.0"}})
}

func TestFloatIncrementSkipsIntsAndImaginaries(t *testing.T) {
	requireCandidates(t, mutator.FloatIncrement,
		"package p\nfunc f() complex128 { _ = 42; return 1i }\n", 0)
}

// TestFloatDecrement asserts exact replacement values (not just float-ness)
// to kill mutations on the delta arg in literal_step.go (e.g. `-1 → -2`
// would yield `-0.5`, `-1.0`, `98.0`). 1e2 (=100.0) decrements to 99 — the
// `'g'`-formatter would emit "99" which is an int literal in Go, so the
// helper must append ".0" to keep the result a float literal.
func TestFloatDecrement(t *testing.T) {
	cs := assertReplacements(t, mutator.FloatDecrement,
		"package p\nfunc f() float64 { return 1.5 + 0.0 + 1e2 }\n",
		[]replacementCase{{"1.5", "0.5"}, {"0.0", "-1.0"}, {"1e2", "99.0"}})
	// `99.0` contains `.`; if the helper ever dropped its append-".0"
	// guard, `99` would slip through and break the surrounding code.
	for i, c := range cs {
		if !strings.ContainsAny(c.Replacement, ".eEpP") {
			t.Errorf("candidate %d replacement=%q is not a float literal", i, c.Replacement)
		}
	}
}

// --- Loop condition ---

func TestLoopCondition(t *testing.T) {
	assertReplacements(t, mutator.LoopCondition,
		"package p\nfunc f() { for i := 0; i < 10; i++ { _ = i } }\n",
		[]replacementCase{{"i < 10", "false"}})
}

func TestLoopConditionSkipsInfiniteAndRange(t *testing.T) {
	requireCandidates(t, mutator.LoopCondition,
		"package p\nfunc f() { for { break }; for _, v := range []int{1, 2} { _ = v } }\n", 0)
}

func TestLoopConditionSkipsAlreadyFalse(t *testing.T) {
	requireCandidates(t, mutator.LoopCondition,
		"package p\nfunc f() { for false { _ = 1 } }\n", 0)
}

// --- Range break ---

// TestRangeBreak anchors StartOffset exactly one byte past the body's `{`.
// Off-by-one mutations either overwrite the brace or land inside the body —
// both produce NotViable patches that erode signal — so this assertion kills
// arithmetic mutations on the +1 insertOffset arithmetic.
func TestRangeBreak(t *testing.T) {
	src := "package p\nfunc f() { for _, v := range []int{1, 2, 3} { _ = v } }\n"
	cs := requireCandidates(t, mutator.RangeBreak, src, 1)
	c := cs[0]
	if c.Original != "" {
		t.Errorf("Original=%q, want empty", c.Original)
	}
	if c.StartOffset != c.EndOffset {
		t.Errorf("StartOffset=%d EndOffset=%d, want equal (zero-width insertion)", c.StartOffset, c.EndOffset)
	}
	wantOffset := strings.Index(src, "{ _ = v") + 1
	if c.StartOffset != wantOffset {
		t.Errorf("StartOffset=%d, want %d (one byte past the body Lbrace)", c.StartOffset, wantOffset)
	}
	if c.Replacement != " break;" {
		t.Errorf("Replacement=%q, want %q", c.Replacement, " break;")
	}
}

// TestRangeBreakPatchProducesParseableSource sanity-checks that applying the
// inserted `break` yields a file the Go parser still accepts. This kills any
// mutation that strips the leading space or trailing `;` from the inserted
// text, which would fuse the new token onto an adjacent identifier.
func TestRangeBreakPatchProducesParseableSource(t *testing.T) {
	src := "package p\nfunc f() { for _, v := range []int{1, 2, 3} { _ = v } }\n"
	srcBytes := []byte(src)
	cs := requireCandidates(t, mutator.RangeBreak, src, 1)
	c := cs[0]
	out := append([]byte(nil), srcBytes[:c.StartOffset]...)
	out = append(out, c.Replacement...)
	out = append(out, srcBytes[c.EndOffset:]...)
	if _, err := parser.ParseFile(token.NewFileSet(), "patched.go", string(out), 0); err != nil {
		t.Errorf("patched source failed to parse: %v\n%s", err, out)
	}
}

// TestRangeBreakHandlesEmptyBody kills the `len > 0` → `len > -1` mutation:
// the mutated guard would always enter the inner branch and panic on
// `rng.Body.List[0]` for an empty body. The test asserts a candidate is
// emitted and no panic occurs.
func TestRangeBreakHandlesEmptyBody(t *testing.T) {
	requireCandidates(t, mutator.RangeBreak,
		"package p\nfunc f(ch chan int) { for range ch { } }\n", 1)
}

func TestRangeBreakSkipsExistingBreak(t *testing.T) {
	requireCandidates(t, mutator.RangeBreak,
		"package p\nfunc f() { for _, v := range []int{1, 2, 3} { break; _ = v } }\n", 0)
}

// TestRangeBreakSkipsSingleBreakBody kills the `len > 0` → `len > 1`
// mutation: with a one-statement body of just `break`, the original guard
// (`> 0`) enters the branch and skips the candidate; the mutated guard
// (`> 1`) skips the branch and emits a phantom-identical insertion.
func TestRangeBreakSkipsSingleBreakBody(t *testing.T) {
	requireCandidates(t, mutator.RangeBreak,
		"package p\nfunc f(ch chan int) { for range ch { break } }\n", 0)
}

// TestRangeBreakEmitsForLeadingContinue kills the mutation that drops the
// `b.Tok == token.BREAK` check (collapsing it to `true`): with the check
// gone, any BranchStmt at the body's head — including `continue` — would
// be treated as an existing break and the candidate skipped.
func TestRangeBreakEmitsForLeadingContinue(t *testing.T) {
	requireCandidates(t, mutator.RangeBreak,
		"package p\nfunc f(ch chan int) { for range ch { continue } }\n", 1)
}

// TestRangeBreakEmitsForLeadingLabelledBreak kills the mutation that drops
// the `b.Label == nil` check: an unconditional `break Outer` exits the
// outer loop, leaving the inner range to still iterate, so the candidate
// must still be emitted. Collapsing the label check to `true` would treat
// the labelled break as the inner-loop short-circuit and skip the mutant.
// Two candidates are expected: one for the outer range (body starts with
// the inner ForStmt — not a BranchStmt) and one for the inner range (body
// starts with a *labelled* break, which must not be confused with the
// unlabelled-break short-circuit).
func TestRangeBreakEmitsForLeadingLabelledBreak(t *testing.T) {
	requireCandidates(t, mutator.RangeBreak,
		"package p\nfunc f(rows [][]int) { Outer: for _, row := range rows { for _, v := range row { break Outer; _ = v } } }\n", 2)
}

func TestRangeBreakDoesNotTouchForStmt(t *testing.T) {
	src := `package p
func f() {
	for i := 0; i < 3; i++ {
		_ = i
	}
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.RangeBreak)
	if got := m.Discover(fset, file, srcBytes); len(got) != 0 {
		t.Errorf("expected 0 candidates for non-range ForStmt, got %d (%+v)", len(got), got)
	}
}

// --- Registry ---

func TestRegistryIsKnown(t *testing.T) {
	reg := mutator.NewRegistry()

	for _, name := range []string{"ARITHMETIC_BASE", "BRANCH_IF", "STATEMENT_REMOVE"} {
		if !reg.IsKnown(name) {
			t.Errorf("IsKnown(%q)=false, want true", name)
		}
	}

	for _, name := range []string{"", "FOO", "ARTIHMETIC_BASE", "arithmetic_base"} {
		if reg.IsKnown(name) {
			t.Errorf("IsKnown(%q)=true, want false", name)
		}
	}
}

func TestRegistryUnknownNames(t *testing.T) {
	reg := mutator.NewRegistry()

	if got := reg.UnknownNames(nil); got != nil {
		t.Errorf("UnknownNames(nil)=%v, want nil", got)
	}

	if got := reg.UnknownNames([]string{"ARITHMETIC_BASE", "BRANCH_IF"}); got != nil {
		t.Errorf("UnknownNames(all-known)=%v, want nil", got)
	}

	got := reg.UnknownNames([]string{"ARITHMETIC_BASE", "ARTIHMETIC_BASE", "FOO"})
	want := []string{"ARTIHMETIC_BASE", "FOO"}
	if len(got) != len(want) {
		t.Fatalf("UnknownNames=%v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("UnknownNames[%d]=%q, want %q", i, got[i], w)
		}
	}
}

func TestRegistryEnabledMutators(t *testing.T) {
	reg := mutator.NewRegistry()

	all := reg.Mutators()
	if len(all) != 28 {
		t.Fatalf("expected 28 mutators, got %d", len(all))
	}

	only := reg.EnabledMutators([]string{"ARITHMETIC_BASE"}, nil)
	if len(only) != 1 {
		t.Fatalf("expected 1 with --only, got %d", len(only))
	}

	disabled := reg.EnabledMutators(nil, []string{"ARITHMETIC_BASE", "BRANCH_IF"})
	if len(disabled) != 26 {
		t.Fatalf("expected 26 after disabling 2, got %d", len(disabled))
	}
}

// --- Offset sanity ---

// TestOffsetsMatchSource asserts StartOffset:EndOffset corresponds to Original
// text for every mutator type. Kills mutations on the offset arithmetic in each
// mutator (e.g. `+ len(original)` → `- len(original)` produces wrong byte range,
// either out of bounds or mismatching the token text).
func TestOffsetsMatchSource(t *testing.T) {
	// Source covers every mutator's target construct so each mutator produces
	// at least one candidate and this test exercises its offset computation.
	src := `package p

import "fmt"

func f(a, b int) int {
	if a > 0 {
		a++
		a = a - 1
	} else {
		a--
	}
	switch a {
	case 1:
		return a + b
	case 2:
		return a * b / 2 % 3
	}
	if a == b && a < b {
		return -a
	}
	if a != b || a >= b {
		return a
	}
	// Compound assignments — InvertAssignments / RemoveSelfAssignments / InvertBitwiseAssignments.
	a += b
	a -= b
	a *= b
	a /= b
	a %= b
	a &= b
	a |= b
	a ^= b
	a &^= b
	a <<= 1
	a >>= 1
	// Bitwise binary — InvertBitwise.
	_ = a & b
	_ = a | b
	_ = a ^ b
	_ = a &^ b
	_ = a << 1
	_ = a >> 1
	// Loop control — InvertLoopCtrl. The non-nil condition also exercises LoopCondition.
	for i := 0; i < 10; i++ {
		if i == 5 {
			break
		}
		if i == 3 {
			continue
		}
	}
	// Range — RangeBreak.
	for _, v := range []int{1, 2, 3} {
		_ = v
	}
	// Float literal — Float{Increment,Decrement}.
	_ = 3.14
	// Negated non-comparison — RemoveLogicalNot. A negated comparison
	// would be skipped as a duplicate of ConditionalsNegation.
	ok := a > b
	if !ok {
		return b
	}
	return 0
}

var errRich error

// Wrapping verb — ErrorfWrap.
func h() error {
	return fmt.Errorf("rich: %w", errRich)
}

// Return slots — ReturnErrorNil / ReturnZero / ReturnTrue / ReturnFalse.
// The literal ` + "`true`" + ` is skipped by ReturnTrue and the literal ` + "`nil`" + ` by
// ReturnErrorNil (both phantom), so each mutator needs a slot it can act on:
// ` + "`errRich`" + ` for ReturnErrorNil, ` + "`true`" + ` for ReturnFalse, ` + "`a < 0`" + ` for both
// boolean mutators, and ` + "`f`" + `'s int returns above for ReturnZero.
func g(a int) (bool, error) {
	if a > 0 {
		return true, errRich
	}
	return a < 0, nil
}
`
	fset, file, srcBytes := parse(t, src)
	reg := mutator.NewRegistry()
	totalCandidates := 0
	for _, m := range reg.Mutators() {
		candidates := m.Discover(fset, file, srcBytes)
		totalCandidates += len(candidates)
		for _, c := range candidates {
			if c.StartOffset < 0 || c.EndOffset > len(srcBytes) || c.StartOffset > c.EndOffset {
				t.Errorf("%s: invalid offset [%d:%d) in %d-byte source",
					c.Type, c.StartOffset, c.EndOffset, len(srcBytes))
				continue
			}
			got := string(srcBytes[c.StartOffset:c.EndOffset])
			if got != c.Original {
				t.Errorf("%s at offset [%d:%d): source has %q, candidate says %q",
					c.Type, c.StartOffset, c.EndOffset, got, c.Original)
			}
		}
		// Each built-in mutator must produce at least one candidate on this rich source.
		if len(candidates) == 0 {
			t.Errorf("%s: expected at least one candidate on rich source, got 0", m.Type())
		}
	}
	if totalCandidates == 0 {
		t.Fatal("no mutators produced candidates")
	}
}

// TestBitwiseOpsProduceNoArithCandidates kills BRANCH_IF and BRANCH_CASE
// mutations on the `!ok`/default guards in arithmetic/boundary/negation
// mutators: if the guard is removed, bitwise ops produce bogus candidates.
func TestBitwiseOpsProduceNoArithCandidates(t *testing.T) {
	src := `package p

func f(a, b int) int {
	_ = a & b
	_ = a | b
	_ = a ^ b
	_ = a << 1
	_ = a >> 1
	return 0
}
`
	fset, file, srcBytes := parse(t, src)
	targets := []mutator.MutationType{
		mutator.ArithmeticBase,
		mutator.ConditionalsBoundary,
		mutator.ConditionalsNegation,
	}
	for _, tt := range targets {
		m := findMutator(t, tt)
		if got := m.Discover(fset, file, srcBytes); len(got) != 0 {
			t.Errorf("%s: expected 0 candidates for bitwise-only source, got %d (%+v)", tt, len(got), got)
		}
	}
}

// TestExpressionRemoveSkipsArithmetic kills BRANCH_CASE on the default
// clause of the LAND/LOR switch — without the default return, arithmetic
// ops would incorrectly produce EXPRESSION_REMOVE candidates with empty
// identity value.
func TestExpressionRemoveSkipsArithmetic(t *testing.T) {
	src := `package p
func f(a, b int) int { return a + b }
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.ExpressionRemove)
	if got := m.Discover(fset, file, srcBytes); len(got) != 0 {
		t.Errorf("ExpressionRemove: arithmetic ops should produce 0 candidates, got %d (%+v)", len(got), got)
	}
}

// TestInvertLogicalSkipsNonLogical kills BRANCH_IF on the `if !ok { return true }`
// guard at invert_logical.go:25. Without it, any binary expression (arithmetic,
// comparison, bitwise) would produce a candidate with a zero-value (ILLEGAL)
// replacement token.
func TestInvertLogicalSkipsNonLogical(t *testing.T) {
	src := `package p
func f(a, b int) int {
	_ = a + b
	_ = a == b
	_ = a & b
	return a - b
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.InvertLogical)
	if got := m.Discover(fset, file, srcBytes); len(got) != 0 {
		t.Errorf("InvertLogical: non-logical ops should produce 0 candidates, got %d (%+v)", len(got), got)
	}
}

// TestInvertBitwiseAssignmentsSkipsNonBitwise kills BRANCH_IF on the
// `if !ok { return true }` guard at invert_bitwise_assignments.go:29. Without
// it, plain `=` and arithmetic compound assigns would produce candidates with
// a zero-value replacement token.
func TestInvertBitwiseAssignmentsSkipsNonBitwise(t *testing.T) {
	src := `package p
func f(a, b int) {
	a = b
	a += b
	a -= b
	a *= b
	a /= b
	a %= b
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.InvertBitwiseAssignments)
	if got := m.Discover(fset, file, srcBytes); len(got) != 0 {
		t.Errorf("InvertBitwiseAssignments: non-bitwise assigns should produce 0 candidates, got %d (%+v)", len(got), got)
	}
}

// TestStatementRemoveEmptyRhs kills BRANCH_IF on the `if len(stmt.Rhs) == 0`
// guard at statement_remove.go:24. The guard protects against parser-recovered
// AssignStmts with empty Rhs; without it, `stmt.Rhs[0].Pos()` panics. Synthetic
// AST construction is used because Go's parser recovery normally yields
// Rhs=[BadExpr] rather than an empty slice.
func TestStatementRemoveEmptyRhs(t *testing.T) {
	file := &ast.File{
		Name: ast.NewIdent("p"),
		Decls: []ast.Decl{
			&ast.FuncDecl{
				Name: ast.NewIdent("f"),
				Type: &ast.FuncType{Params: &ast.FieldList{}},
				Body: &ast.BlockStmt{
					List: []ast.Stmt{
						&ast.AssignStmt{
							Lhs: []ast.Expr{ast.NewIdent("x")},
							Tok: token.ASSIGN,
						},
					},
				},
			},
		},
	}
	fset := token.NewFileSet()
	m := findMutator(t, mutator.StatementRemove)
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("StatementRemove panicked on empty-Rhs AssignStmt: %v", r)
		}
	}()
	if got := m.Discover(fset, file, nil); len(got) != 0 {
		t.Errorf("expected 0 candidates for empty Rhs, got %d", len(got))
	}
}

// TestInvertNegativesSkipsNonSub kills the BRANCH_IF on `node.Op != token.SUB`
// guards: without them, unary `+` and binary `+` would produce candidates.
func TestInvertNegativesSkipsNonSub(t *testing.T) {
	src := `package p
func f(a, b int) int { _ = +a; return a + b }
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.InvertNegatives)
	if got := m.Discover(fset, file, srcBytes); len(got) != 0 {
		t.Errorf("InvertNegatives: non-SUB ops should produce 0 candidates, got %d (%+v)", len(got), got)
	}
}

// TestEnabledMutatorsPreservesOrderAndSet kills CONDITIONALS_BOUNDARY on
// `len(disable) > 0` in EnabledMutators: mutating to `>=` would include the
// empty-disable case and return a copy (different slice than r.mutators).
func TestEnabledMutatorsEmptyDisableReturnsSameSlice(t *testing.T) {
	reg := mutator.NewRegistry()
	full := reg.Mutators()
	got := reg.EnabledMutators(nil, nil)
	// Must return the ORIGINAL mutators slice (same length, same ordering, same type ids).
	if len(got) != len(full) {
		t.Fatalf("len=%d, want %d", len(got), len(full))
	}
	for i := range got {
		if got[i].Type() != full[i].Type() {
			t.Errorf("[%d] type=%v, want %v", i, got[i].Type(), full[i].Type())
		}
	}
}

// --- Status String ---

func TestMutantStatusString(t *testing.T) {
	tests := []struct {
		status mutator.MutantStatus
		want   string
	}{
		{mutator.StatusPending, "PENDING"},
		{mutator.StatusKilled, "KILLED"},
		{mutator.StatusLived, "LIVED"},
		{mutator.StatusNotCovered, "NOT COVERED"},
		{mutator.StatusNotViable, "NOT VIABLE"},
		{mutator.StatusTimedOut, "TIMED OUT"},
		{mutator.StatusEquivalent, "EQUIVALENT"},
		{mutator.StatusInfraError, "INFRA ERROR"},
		{mutator.MutantStatus(99), "UNKNOWN"},
	}
	for _, tc := range tests {
		got := tc.status.String()
		if got != tc.want {
			t.Errorf("MutantStatus(%d).String() = %q, want %q", tc.status, got, tc.want)
		}
	}
}

// TestMutantStatusStringsAreUnique pins the invariant the persisted formats
// actually depend on: a status is written to the cache and to every report as
// its String(), and read back by matching that text (cache.parseStatus). Two
// statuses sharing a spelling would make a cache round-trip return the wrong
// verdict — silently, since both sides would still parse.
func TestMutantStatusStringsAreUnique(t *testing.T) {
	all := []mutator.MutantStatus{
		mutator.StatusPending,
		mutator.StatusKilled,
		mutator.StatusLived,
		mutator.StatusNotCovered,
		mutator.StatusNotViable,
		mutator.StatusTimedOut,
		mutator.StatusEquivalent,
		mutator.StatusInfraError,
	}
	seen := make(map[string]mutator.MutantStatus, len(all))
	for _, s := range all {
		if prev, dup := seen[s.String()]; dup {
			t.Errorf("MutantStatus(%d) and MutantStatus(%d) both stringify to %q", prev, s, s.String())
		}
		seen[s.String()] = s
		if s.String() == "UNKNOWN" {
			t.Errorf("MutantStatus(%d) has no String() case; it would persist as UNKNOWN", s)
		}
	}
}

// --- No-op mutators on empty functions ---

func TestMutatorsOnEmptyFunc(t *testing.T) {
	src := `package p
func f() {}
`
	fset, file, srcBytes := parse(t, src)
	reg := mutator.NewRegistry()
	for _, m := range reg.Mutators() {
		// Should not panic on minimal source.
		_ = m.Discover(fset, file, srcBytes)
	}
}

// TestMutatorsNonMatchingNodes exercises the early-return paths in each mutator
// by providing AST nodes that don't match the mutator's target pattern.
func TestMutatorsNonMatchingNodes(t *testing.T) {
	// This source has diverse AST nodes but specifically avoids matching
	// certain mutator patterns, exercising the "no match" branches.
	src := `package p

import "fmt"

func f(x int) string {
	// Bitwise ops — not in arithmetic swap table.
	a := x & 0xff
	b := x | 0x0f
	c := x ^ 0x01
	d := x << 2
	e := x >> 1

	// String concatenation — ADD token but not numeric.
	s := "hello" + "world"

	// Comparison with == — not in ConditionalsBoundary.
	if a == b {
		fmt.Println(c, d, e, s)
	}

	// For loop (not if/switch).
	for i := 0; i < 10; i++ {
		_ = i
	}

	// Select statement (not switch).
	ch := make(chan int, 1)
	ch <- 1
	select {
	case v := <-ch:
		_ = v
	}

	// Type switch (case clause with no body beyond type assert).
	var iface interface{} = 42
	switch iface.(type) {
	case int:
	}

	// Return statement (not AssignStmt/ExprStmt/IncDecStmt for StatementRemove).
	return fmt.Sprintf("%d %d", a, b)
}
`
	fset, file, srcBytes := parse(t, src)
	reg := mutator.NewRegistry()
	for _, m := range reg.Mutators() {
		candidates := m.Discover(fset, file, srcBytes)
		// We just want these to run without panic and exercise all branches.
		_ = candidates
	}
}

// --- Edge cases for branch mutators ---

func TestBranchIfEmptyBody(t *testing.T) {
	src := `package p
func f(x int) {
	if x > 0 {
	}
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.BranchIf)
	candidates := m.Discover(fset, file, srcBytes)
	// Empty if body should be skipped.
	if len(candidates) != 0 {
		t.Errorf("expected 0 candidates for empty if body, got %d", len(candidates))
	}
}

func TestBranchElseEmptyBody(t *testing.T) {
	src := `package p
func f(x int) {
	if x > 0 {
		_ = x
	} else {
	}
}
`
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, mutator.BranchElse)
	candidates := m.Discover(fset, file, srcBytes)
	// Empty else body should be skipped.
	if len(candidates) != 0 {
		t.Errorf("expected 0 candidates for empty else body, got %d", len(candidates))
	}
}

// --- Nested-construct traversal ---

// TestNestedConstructsAreTraversed pins that every mutator keeps descending
// after it has examined a node, rather than pruning that node's subtree.
//
// Each ast.Inspect visitor ends in `return true`, and most carry interior
// `return true` guards for nodes of the right kind but the wrong sub-kind (a
// *ast.BinaryExpr whose operator this mutator doesn't swap, an if with an
// empty body). Flipping any of them to `false` tells ast.Inspect to skip that
// node's children — which changes nothing at all unless a *mutable* construct
// is nested underneath. The flat fixtures elsewhere in this file leave that
// undetected, so the sources below deliberately nest each mutator's target
// inside a node the same visitor visits first:
//
//   - under a non-matching node of the same kind, for the sub-kind guards
//     (`(a + b) < (a * b)` — the `<` is a BinaryExpr the arithmetic mutator
//     declines, and the operands it would then never reach are the point);
//   - under a matching node, for the final `return true` (`(a & b) | (a ^ b)`).
//
// Where a construct cannot nest inside itself, the nesting goes through a
// function literal or a call argument — `m[func() int { y++; return 0 }()]++`
// is the only way one IncDecStmt lands under another.
//
// Counts are exact on purpose: a pruned subtree shows up as a *smaller*
// count, which a "at least one candidate" assertion would miss entirely.
//
// Not covered here, because pruning them provably cannot change the result:
// INVERT_LOOP_CTRL (a BranchStmt's only child is its label) and the
// numeric-literal mutators (a BasicLit has no children at all).
func TestNestedConstructsAreTraversed(t *testing.T) {
	cases := []struct {
		typ  mutator.MutationType
		want int
		src  string
	}{
		{mutator.ArithmeticBase, 2, `package p
var a, b int
func f() { _ = (a + b) < (a * b) }
`},
		{mutator.ConditionalsBoundary, 5, `package p
var a, b int
func g(bool) int { return 0 }
func f() {
	_ = (a <= b) == (a >= b)
	_ = g(a < b) < g(a > b)
}
`},
		{mutator.ConditionalsNegation, 5, `package p
var a, b int
func g(bool) int { return 0 }
func f() {
	_ = (a == b) && (a != b)
	_ = g(a == b) == g(a != b)
}
`},
		{mutator.InvertLogical, 2, `package p
var a, b, c, d bool
func f() { _ = (a && b) == (c || d) }
`},
		// The three-element constraint union nests one `|` inside another,
		// which is what pins the recursive constraint-position scan: recording
		// only the outermost union would leave the inner one mutable.
		{mutator.InvertBitwise, 5, `package p
type C interface{ ~int | ~string | ~bool }
var a, b int
func f() {
	_ = (a & b) + (a | b)
	_ = (a & b) | (a ^ b)
}
`},
		{mutator.InvertAssignments, 3, `package p
var x, y int
func f() {
	x = func() int { y += 1; return y }()
	x += func() int { y *= 2; return y }()
}
`},
		{mutator.InvertBitwiseAssignments, 3, `package p
var x, y int
func f() {
	x = func() int { y &= 1; return y }()
	x |= func() int { y ^= 2; return y }()
}
`},
		{mutator.RemoveSelfAssignments, 3, `package p
var x, y int
func f() {
	x = func() int { y += 1; return y }()
	x += func() int { y *= 2; return y }()
}
`},
		// The outer statements are a short declaration and a blank assignment
		// — both declined — so only the nested plain assignments are found.
		{mutator.StatementRemove, 2, `package p
var x, y int
func f() {
	x := func() int { y = 1; return y }()
	_ = func() int { y = 2; return y }()
	_ = x
}
`},
		// The outer if has an empty body and is declined; the candidate comes
		// entirely from the if nested in its else.
		{mutator.BranchIf, 1, `package p
var a, b bool
func c() {}
func f() { if a { } else { if b { c() } } }
`},
		{mutator.BranchElse, 4, `package p
var a, b bool
func x() {}
func f() {
	if a { x() } else if b { x() } else { x() }
	if a { if b { x() } else { x() } } else { }
	if a { x() } else { if b { x() } else { x() } }
}
`},
		{mutator.BranchCase, 3, `package p
var q bool
func r() {}
func f() {
	switch { case func() bool { switch { case q: r() }; return true }(): }
	switch { case q: switch { case q: r() } }
}
`},
		{mutator.ExpressionRemove, 6, `package p
var a, b, c bool
func f() {
	_ = (a && b) == c
	_ = (a && b) || c
}
`},
		{mutator.IncrementDecrement, 2, `package p
var y int
var m = map[int]int{}
func f() { m[func() int { y++; return 0 }()]++ }
`},
		// This mutator switches on two node kinds, so both need nesting: a
		// binary subtraction under a non-subtracting binary, and one under a
		// unary operator that isn't negation.
		{mutator.InvertNegatives, 3, `package p
var a, b int
func f() {
	_ = (a - b) + (a - b)
	_ = !((a - b) > 0)
}
`},
		{mutator.LoopCondition, 3, `package p
func f() {
	for i := 0; false; i++ { for j := 0; j < 2; j++ { _ = j } }
	for i := 0; i < 2; i++ { for j := 0; j < 3; j++ { _ = j } }
}
`},
		// The outer range body already begins with a bare break and is
		// declined; the sole candidate is the range nested behind it.
		{mutator.RangeBreak, 1, `package p
var xs, ys []int
func f() {
	for _, v := range xs { break; for _, w := range ys { _ = w } }
	_ = xs
}
`},
	}
	for _, c := range cases {
		t.Run(string(c.typ), func(t *testing.T) {
			requireCandidates(t, c.typ, c.src, c.want)
		})
	}
}

// --- Return values ---

// returnMutators is the set of mutators that partition the return-slot space.
var returnMutators = []mutator.MutationType{
	mutator.ReturnErrorNil, mutator.ReturnZero, mutator.ReturnTrue, mutator.ReturnFalse,
}

// assertNoReturnCandidates asserts that none of the four return-value
// mutators finds anything in src — used for the shapes they all skip.
func assertNoReturnCandidates(t *testing.T, src string) {
	t.Helper()
	for _, typ := range returnMutators {
		requireCandidates(t, typ, src, 0)
	}
}

func TestReturnErrorNil(t *testing.T) {
	// The `return nil` in c is phantom — patching it would reproduce the
	// source byte-for-byte — so only a and b yield candidates.
	assertReplacements(t, mutator.ReturnErrorNil, `package p
var e error
var n int
func a() error { return e }
func b() (int, error) { return n, e }
func c() error { return nil }
`, []replacementCase{{"e", "nil"}, {"e", "nil"}})
}

func TestReturnErrorNilSkipsShadowedError(t *testing.T) {
	// The file declares its own `error`, so the slot is not the predeclared
	// one and ReturnErrorNil must not claim it. ReturnZero picks it up
	// instead, spelling the zero value as *new(error) rather than nil —
	// which is what a struct type actually needs.
	//
	// The `var` deliberately precedes the `type`: the scan for shadowing
	// names must skip non-type declarations and keep going, not stop at the
	// first one it sees.
	src := `package p
var e error
type error struct{}
func f() error { return e }
`
	requireCandidates(t, mutator.ReturnErrorNil, src, 0)
	assertReplacements(t, mutator.ReturnZero, src, []replacementCase{{"e", "*new(error)"}})
}

func TestReturnTrue(t *testing.T) {
	// The literal `true` in g is phantom for this mutator; h's `false` is
	// exactly the case a single boolean mutator could not cover.
	assertReplacements(t, mutator.ReturnTrue, `package p
func f(x int) bool { return x > 0 }
func g() bool { return true }
func h() bool { return false }
`, []replacementCase{{"x > 0", "true"}, {"false", "true"}})
}

func TestReturnFalse(t *testing.T) {
	assertReplacements(t, mutator.ReturnFalse, `package p
func f(x int) bool { return x > 0 }
func g() bool { return true }
func h() bool { return false }
`, []replacementCase{{"x > 0", "false"}, {"true", "false"}})
}

func TestReturnZeroBasicTypes(t *testing.T) {
	assertReplacements(t, mutator.ReturnZero, `package p
var v int
func s() string { return v }
func i() int { return v }
func u() uint64 { return v }
func f() float64 { return v }
func c() complex128 { return v }
func r() rune { return v }
func b() byte { return v }
func p2() uintptr { return v }
func z() any { return v }
`, []replacementCase{
		{"v", `""`}, {"v", "0"}, {"v", "0"}, {"v", "0"}, {"v", "0"},
		{"v", "0"}, {"v", "0"}, {"v", "0"}, {"v", "nil"},
	})
}

func TestReturnZeroNilableTypes(t *testing.T) {
	assertReplacements(t, mutator.ReturnZero, `package p
var v int
func a() *int { return v }
func b() []int { return v }
func c() map[string]int { return v }
func d() chan int { return v }
func e() func() int { return v }
func f() interface{ M() } { return v }
`, []replacementCase{
		{"v", "nil"}, {"v", "nil"}, {"v", "nil"},
		{"v", "nil"}, {"v", "nil"}, {"v", "nil"},
	})
}

func TestReturnZeroFallsBackToNew(t *testing.T) {
	// Every type here has a zero value that is awkward or impossible to
	// spell as a literal. *new(T) covers them all, and needs no import the
	// signature hasn't already required — including the fixed-size array,
	// which is the one *ast.ArrayType that has no nil.
	assertReplacements(t, mutator.ReturnZero, `package p
var v int
func a() [3]int { return v }
func b() time.Duration { return v }
func c() MyType { return v }
func d() Result[int] { return v }
func e[T any]() T { return v }
func f() struct{ A int } { return v }
`, []replacementCase{
		{"v", "*new([3]int)"},
		{"v", "*new(time.Duration)"},
		{"v", "*new(MyType)"},
		{"v", "*new(Result[int])"},
		{"v", "*new(T)"},
		{"v", "*new(struct{ A int })"},
	})
}

// TestReturnZeroSkipsAlreadyZeroValues pins that the *new(T) fallback does not
// emit when the source already returns that zero value spelled another way.
// `Block{}` and `*new(Block)` are the same value, as are `0` and
// `*new(time.Duration)` — the byte-level phantom guard cannot see this because
// the two spellings differ, so such mutants would survive forever uncatchable.
func TestReturnZeroSkipsAlreadyZeroValues(t *testing.T) {
	requireCandidates(t, mutator.ReturnZero, `package p
type Block struct{ A int }
func a() Block { return Block{} }
func b() time.Duration { return 0 }
func c() time.Duration { return 0.0 }
func d() MyString { return "" }
func e() MyString { return `+"``"+` }
func f() MyFlag { return false }
func g() [3]int { return [3]int{} }
func h() MyIface { return nil }
`, 0)

	// Zero has a spelling in every numeric base and notation Go offers, and
	// a rune literal has four ways to write NUL. All denote the same value
	// the *new(T) patch would install, so all must be declined — the two
	// parsers exist precisely because neither covers this list alone.
	requireCandidates(t, mutator.ReturnZero, `package p
func a() MyMask { return 0b0 }
func b() MyMask { return 0o0 }
func c() MyMask { return 00 }
func d() MyMask { return 0x0 }
func e() MyMask { return 0_0 }
func f() MyFloat { return 0e10 }
func g() MyFloat { return 0x0p0 }
func h() MyComplex { return 0i }
func i() MyComplex { return 0.0i }
func j() MyRune { return '\x00' }
func k() MyRune { return '\000' }
`, 0)

	// A declined slot must not stop the rest of the return from being
	// examined: slot 0 is already zero and is skipped, slot 1 still mutates.
	assertReplacements(t, mutator.ReturnZero, `package p
type Block struct{ A int }
func a() (Block, time.Duration) { return Block{}, 5 }
`, []replacementCase{{"5", "*new(time.Duration)"}})

	// A non-zero value of the same types is still mutated — the skip is
	// about the value, not the type. `0b1` pins that the binary prefix is
	// read by the parser that understands it rather than being waved
	// through by the one that can't; `1e999` overflows ParseFloat, and a
	// parse that failed must never be read as a zero verdict. The last case
	// is an expression that is no kind of literal at all, whose value syntax
	// cannot decide — the safe direction is to mutate it, since a spurious
	// mutant is visible in the report and a missing one is not.
	assertReplacements(t, mutator.ReturnZero, `package p
type Block struct{ A int }
var n int
func a() Block { return Block{A: 1} }
func b() time.Duration { return 5 }
func c() MyString { return "x" }
func d() MyRune { return 'a' }
func e() MyFloat { return 1e999 }
func f() MyMask { return 0b1 }
func g() MyCount { return n + 1 }
`, []replacementCase{
		{"Block{A: 1}", "*new(Block)"},
		{"5", "*new(time.Duration)"},
		{`"x"`, "*new(MyString)"},
		{"'a'", "*new(MyRune)"},
		{"1e999", "*new(MyFloat)"},
		{"0b1", "*new(MyMask)"},
		{"n + 1", "*new(MyCount)"},
	})
}

// TestReturnZeroSkipsAlreadyZeroLiteralSlots is the zeroLiterals-path twin of
// TestReturnZeroSkipsAlreadyZeroValues. The shortest spelling of a predeclared
// type's zero is not the only spelling: `0.0` in a float64 slot and a raw
// empty string in a string slot are the same value the patch would install,
// so Discover's byte comparison lets them through while nothing can kill them.
//
// The `any` slot is the deliberate exception. Its zero is nil, and an
// interface holding 0 is not a nil interface, so that one is a real mutation
// and has to survive the guard.
func TestReturnZeroSkipsAlreadyZeroLiteralSlots(t *testing.T) {
	requireCandidates(t, mutator.ReturnZero, `package p
func a() float64 { return 0.0 }
func b() float64 { return 0e10 }
func c() float32 { return 0x0p0 }
func d() string { return `+"``"+` }
func e() int { return (0) }
func f() uint { return 0b0 }
func g() byte { return '\x00' }
func h() complex128 { return 0i }
`, 0)

	assertReplacements(t, mutator.ReturnZero, `package p
func a() any { return 0 }
func b() any { return "" }
func c() float64 { return 1.5 }
`, []replacementCase{{"0", "nil"}, {`""`, "nil"}, {"1.5", "0"}})
}

// TestReturnZeroMutatesNilableEmptyLiterals pins the half of the empty-literal
// rule that is not about being empty.
//
// `S{}` for a named slice is a non-nil empty slice and `M{}` a non-nil empty
// map, while *new(S) and *new(M) are both nil — observable under ==, under
// reflect.DeepEqual, in JSON as [] versus null, and for a map by writing to it,
// where the nil one panics. Treating those as already-zero would suppress a
// mutant a test can genuinely kill.
//
// An unnamed slice or map never gets this far, because zeroValueExpr answers
// *ast.ArrayType and *ast.MapType with a plain nil first. Only a named one
// reaches the *new(T) fallback, which is why the name has to be resolved.
func TestReturnZeroMutatesNilableEmptyLiterals(t *testing.T) {
	assertReplacements(t, mutator.ReturnZero, `package p
type S []int
type M map[string]int
type Chain S
type Alias = []int
func a() S { return S{} }
func b() M { return M{} }
func c() Chain { return Chain{} }
func d() Alias { return Alias{} }
`, []replacementCase{
		{"S{}", "*new(S)"},
		{"M{}", "*new(M)"},
		{"Chain{}", "*new(Chain)"},
		{"Alias{}", "*new(Alias)"},
	})

	// The literal must also be spelled as the slot's own type. `Impl{}` in an
	// interface slot is a non-nil interface holding a zero Impl, which is not
	// the nil that *new(I) yields.
	assertReplacements(t, mutator.ReturnZero, `package p
type I interface{ M() }
type Impl struct{}
func f() I { return Impl{} }
`, []replacementCase{{"Impl{}", "*new(I)"}})
}

// TestReturnZeroSuppressesNonNilableEmptyLiterals is the other side of
// TestReturnZeroMutatesNilableEmptyLiterals: the empty literal of a struct or
// a fixed-size array really is that type's zero value, named or not, and must
// stay suppressed. This is the case #80 added the guard for, and the
// nilable-type carve-out must not cost it.
//
// The last two are the shapes the resolution cannot decide. A name declared in
// a sibling file or another package does not resolve, and a cyclic declaration
// resolves to nothing; both keep the suppressing default, which loses a mutant
// rather than emitting one that could never be killed. The cycle is also here
// to pin termination — it parses even though it cannot compile, and Discover
// is only ever promised a file that parsed.
func TestReturnZeroSuppressesNonNilableEmptyLiterals(t *testing.T) {
	requireCandidates(t, mutator.ReturnZero, `package p
type Arr [3]int
type Block struct{ A int }
type C D
type D C
func a() Arr { return Arr{} }
func b() Block { return Block{} }
func c() [3]int { return [3]int{} }
func d() other.Thing { return other.Thing{} }
func e() Elsewhere { return Elsewhere{} }
func f() C { return C{} }
`, 0)
}

// TestReturnValueStripsParens pins that the phantom guard compares against the
// expression inside any enclosing parentheses. `(true)` would otherwise read as
// different from `true` and RETURN_TRUE would emit a patch that rewrites
// `return (true)` into itself.
//
// The span replaced stays the whole slot expression, parentheses included, so
// the recorded Original is `(true)` and the patch is `return false`. See
// TestReturnValueReportsTheReturnLine for why the span is not narrowed.
func TestReturnValueStripsParens(t *testing.T) {
	src := `package p
var e error
func a() bool { return (true) }
func b(x int) bool { return (x > 0) }
func c() error { return (e) }
`
	assertReplacements(t, mutator.ReturnTrue, src, []replacementCase{{"(x > 0)", "true"}})
	assertReplacements(t, mutator.ReturnFalse, src,
		[]replacementCase{{"(true)", "false"}, {"(x > 0)", "false"}})
	assertReplacements(t, mutator.ReturnErrorNil, src, []replacementCase{{"(e)", "nil"}})

	// Nesting must unwrap all the way down, not one layer.
	requireCandidates(t, mutator.ReturnZero, `package p
func f() int { return ((0)) }
`, 0)
}

// TestReturnValueReportsTheReturnLine pins that a candidate is reported on the
// line its `return` starts on, even when the returned expression begins on a
// later one.
//
// A gomutants:disable-next-line directive resolves to the first code line
// below it — the `return` line — and FilterByDirectives matches on the
// mutant's Line exactly. Reporting a multi-line `return (\n\texpr)` against
// the inner expression's line would leave the directive matching nothing, and
// a mutation the author explicitly suppressed would quietly start running
// again. Nothing else in the pipeline would notice.
func TestReturnValueReportsTheReturnLine(t *testing.T) {
	cs := requireCandidates(t, mutator.ReturnZero, `package p
func f() int {
	return (
		1 + 2)
}
`, 1)
	if cs[0].Pos.Line != 3 {
		t.Errorf("reported line %d, want 3 (the `return` line, which is what a disable-next-line above it resolves to)", cs[0].Pos.Line)
	}
}

func TestReturnZeroSkipsShadowedBasicType(t *testing.T) {
	// `string` here is a struct, so `""` would not compile. The grouped
	// type declaration also exercises a GenDecl carrying several specs, and
	// the leading `var` pins that the scan skips non-type declarations
	// rather than stopping at the first one.
	assertReplacements(t, mutator.ReturnZero, `package p
var v int
type (
	string struct{ A int }
	other  struct{}
)
func f() string { return v }
`, []replacementCase{{"v", "*new(string)"}})
}

// TestReturnZeroLeavesBoolSlotsAlone pins the boundary between ReturnZero
// and the two boolean mutators. Widening ReturnZero to claim bool slots as
// well would emit *new(bool) on top of the true/false pair — three mutants
// where two suffice, two of them equivalent.
func TestReturnZeroLeavesBoolSlotsAlone(t *testing.T) {
	requireCandidates(t, mutator.ReturnZero, `package p
func f(x int) bool { return x > 0 }
func g() (bool, bool) { return true, false }
`, 0)
}

// TestReturnValueContinuesPastSkippedSlots pins that a slot the mutator
// passes over — because it belongs to another mutator, or because its
// replacement would be phantom — does not stop the rest of the return
// statement from being examined. Both returns here put a skipped slot ahead
// of a mutable one.
func TestReturnValueContinuesPastSkippedSlots(t *testing.T) {
	src := `package p
var n int
var e error
func f() (string, int) { return "", n }
func g() (int, error) { return n, e }
`
	// Slot 0 of f is phantom for ReturnZero ("" is already the zero value),
	// slot 1 is not.
	assertReplacements(t, mutator.ReturnZero, src, []replacementCase{{"n", "0"}, {"n", "0"}})
	// Slot 0 of g belongs to ReturnZero, slot 1 to ReturnErrorNil.
	assertReplacements(t, mutator.ReturnErrorNil, src, []replacementCase{{"e", "nil"}})
}

func TestReturnValueSkipsUnmutableShapes(t *testing.T) {
	// A bare `return` under named results carries no expression to replace;
	// `return f()` spreads one call over two slots, so no per-slot span
	// exists; a void function's bare return has nothing to act on; and a
	// body-less declaration has no returns at all.
	assertNoReturnCandidates(t, `package p
func a() (int, error) { return g() }
func b() (n int, err error) { return }
func c() { return }
func d(x int) int
func g() (int, error) { return 0, nil }
`)
}

func TestReturnValueUsesClosureSignature(t *testing.T) {
	// A return inside a func literal belongs to the literal's own result
	// list, not the enclosing declaration's. Attributing it to the outer
	// signature would make ReturnErrorNil claim the inner `n` — an int.
	// Closure candidates come after the enclosing function's because the
	// outer walk reaches the literal while descending.
	src := `package p
var e error
var n int
func outer() error {
	g := func() int { return n }
	_ = g
	return e
}
`
	assertReplacements(t, mutator.ReturnErrorNil, src, []replacementCase{{"e", "nil"}})
	assertReplacements(t, mutator.ReturnZero, src, []replacementCase{{"n", "0"}})
}

func TestReturnValueGroupedResults(t *testing.T) {
	// `(a, b error)` is a single *ast.Field with two Names and must flatten
	// to two slots; misflattening would shift every slot after it.
	src := `package p
var e error
var n int
func f() (a, b error) { return e, e }
func g() (a, b int, c error) { return n, n, e }
`
	requireCandidates(t, mutator.ReturnErrorNil, src, 3)
	assertReplacements(t, mutator.ReturnZero, src, []replacementCase{{"n", "0"}, {"n", "0"}})
}

// TestReturnValuePatchesAreDistinct asserts the four mutators never produce
// the same patch twice. Their slot predicates partition by declared type, so
// no span is claimed by two of them — except ReturnTrue and ReturnFalse,
// which deliberately share a span and are separated by their replacement.
func TestReturnValuePatchesAreDistinct(t *testing.T) {
	src := `package p
var e error
var n int
func f(x int) (bool, string, error) {
	if x > 0 {
		return true, "a", e
	}
	return x < 0, "", nil
}
`
	fset, file, srcBytes := parse(t, src)
	type patch struct {
		start, end  int
		replacement string
	}
	seen := make(map[patch]mutator.MutationType)
	total := 0
	for _, typ := range returnMutators {
		for _, c := range findMutator(t, typ).Discover(fset, file, srcBytes) {
			total++
			p := patch{c.StartOffset, c.EndOffset, c.Replacement}
			if prev, dup := seen[p]; dup {
				t.Errorf("%s duplicates a patch already emitted by %s: [%d:%d)→%q",
					typ, prev, p.start, p.end, p.replacement)
			}
			seen[p] = typ
		}
	}
	if total == 0 {
		t.Fatal("expected candidates from the return mutators")
	}
}

// --- Return-value equivalence corpus ---

// The corpus asks one question of RETURN_ZERO and answers it two ways.
//
// Every decision the mutator makes about a zero value is a claim that two
// pieces of Go text do, or do not, denote the same value. Until now those
// claims were checked by asserting the behaviour someone had reasoned their
// way to, which is exactly the step that has gone wrong: an empty literal was
// read as a zero value when for a named slice it is not, and `0.0` was read as
// distinct from `0` when it is not.
//
// So the expected answer here is not written down. It is computed by handing
// the two expressions to the Go runtime and asking reflect.DeepEqual, and the
// test asserts the biconditional: RETURN_ZERO declines a slot **if and only
// if** the value it would substitute is the value already there.
//
// That closes the direction nothing else can see. An equivalent mutant that is
// emitted at least surfaces as a survivor someone eventually investigates; a
// real mutant that is wrongly suppressed produces no signal at all — the count
// is simply lower, and the efficacy number looks better for it.
//
// corpusDecls is the type environment each case is discovered in. It must stay
// in lockstep with the Go declarations immediately below: the string feeds the
// mutator, the declarations feed reflect.DeepEqual, and the test only means
// anything while the two agree. They are kept adjacent so a change to one that
// misses the other is visible in the same diff hunk.
const corpusDecls = `
type zBlock struct{ A int }
type zArr [3]int
type zSlice []int
type zMap map[string]int
type zMask uint
type zCode rune
type zMarker interface{ Mark() }
type zImpl struct{}
type zAny any
type zAlias = zBlock
func (zImpl) Mark() {}
`

type zBlock struct{ A int }
type zArr [3]int
type zSlice []int
type zMap map[string]int
type zMask uint
type zCode rune
type zMarker interface{ Mark() }
type zImpl struct{}
type zAny any
type zAlias = zBlock

func (zImpl) Mark() {}

// equivalenceCase is one (slot type, returned expression) pair together with
// the text RETURN_ZERO does or would substitute for it. Whether it should is
// not a field anyone fills in — see same.
type equivalenceCase struct {
	typ  string // the slot type, as written in the signature
	expr string // the expression returned into that slot
	repl string // what RETURN_ZERO substitutes, or would if it did not decline
	// same is ground truth, evaluated by the Go runtime rather than asserted:
	// both operands are converted to the slot type first, because DeepEqual
	// compares dynamic types and would otherwise call float64(0) and int(0)
	// different for the wrong reason.
	same bool
}

var equivalenceCorpus = []equivalenceCase{
	// Alternate spellings of a zero the mutator has a short literal for.
	{"float64", "0.0", "0", reflect.DeepEqual(float64(0.0), float64(0))},
	{"float64", "0e10", "0", reflect.DeepEqual(float64(0e10), float64(0))},
	{"string", "``", `""`, reflect.DeepEqual(``, "")},
	{"int", "(0)", "0", reflect.DeepEqual(int((0)), int(0))},
	{"uint", "0b0", "0", reflect.DeepEqual(uint(0b0), uint(0))},
	{"complex128", "0i", "0", reflect.DeepEqual(complex128(0i), complex128(0))},
	{"byte", `'\x00'`, "0", reflect.DeepEqual(byte('\x00'), byte(0))},

	// Alternate spellings of a zero reached through the *new(T) fallback.
	{"zBlock", "zBlock{}", "*new(zBlock)", reflect.DeepEqual(zBlock{}, *new(zBlock))},
	{"zArr", "zArr{}", "*new(zArr)", reflect.DeepEqual(zArr{}, *new(zArr))},

	// Empty literals that are not the slot's zero: the composite kinds whose
	// empty form is non-nil, and a literal of some other type entirely.
	{"zSlice", "zSlice{}", "*new(zSlice)", reflect.DeepEqual(zSlice{}, *new(zSlice))},
	{"zMap", "zMap{}", "*new(zMap)", reflect.DeepEqual(zMap{}, *new(zMap))},
	{"zMarker", "zImpl{}", "*new(zMarker)", reflect.DeepEqual(zMarker(zImpl{}), *new(zMarker))},

	// An interface holding zero is not a nil interface — true of the
	// predeclared `any`, and equally of a named interface reached through the
	// *new(T) fallback, whatever spelling the zero-valued literal uses.
	{"any", "0", "nil", reflect.DeepEqual(any(0), *new(any))},
	{"zAny", "0", "*new(zAny)", reflect.DeepEqual(zAny(0), *new(zAny))},
	{"zAny", `'\x00'`, "*new(zAny)", reflect.DeepEqual(zAny('\x00'), *new(zAny))},
	{"zAny", "0b0", "*new(zAny)", reflect.DeepEqual(zAny(0b0), *new(zAny))},
	{"zAny", "0i", "*new(zAny)", reflect.DeepEqual(zAny(0i), *new(zAny))},
	{"zAny", "false", "*new(zAny)", reflect.DeepEqual(zAny(false), *new(zAny))},
	{"zAny", "``", "*new(zAny)", reflect.DeepEqual(zAny(``), *new(zAny))},

	// A literal spelled as a different name for the same non-nilable type is
	// still that type's zero, so it must stay suppressed. An alias and an
	// unnamed struct sharing the slot's underlying type both qualify.
	{"zAlias", "zBlock{}", "*new(zAlias)", reflect.DeepEqual(zAlias(zBlock{}), *new(zAlias))},
	{"zBlock", "struct{ A int }{}", "*new(zBlock)", reflect.DeepEqual(zBlock(struct{ A int }{}), *new(zBlock))},

	// Plainly non-zero values, as controls: a guard that declines these is as
	// broken as one that fails to decline the cases above.
	{"int", "1", "0", reflect.DeepEqual(int(1), int(0))},
	{"float64", "1.5", "0", reflect.DeepEqual(float64(1.5), float64(0))},
	{"string", `"x"`, `""`, reflect.DeepEqual("x", "")},
	{"zMask", "0b1", "*new(zMask)", reflect.DeepEqual(zMask(0b1), *new(zMask))},
	{"zCode", "'a'", "*new(zCode)", reflect.DeepEqual(zCode('a'), *new(zCode))},
}

// assertEquivalenceCase discovers RETURN_ZERO against one corpus case and
// holds it to the verdict reflect.DeepEqual gave.
func assertEquivalenceCase(t *testing.T, c equivalenceCase) {
	t.Helper()
	src := "package p\n" + corpusDecls + "\nfunc f() " + c.typ + " { return " + c.expr + " }\n"
	fset, file, srcBytes := parse(t, src)
	got := findMutator(t, mutator.ReturnZero).Discover(fset, file, srcBytes)

	if c.same {
		if len(got) != 0 {
			t.Errorf("%s slot returning %s: got mutant %q→%q, but the two are the same value — nothing could ever kill it",
				c.typ, c.expr, got[0].Original, got[0].Replacement)
		}
		return
	}
	if len(got) != 1 {
		t.Fatalf("%s slot returning %s: got %d mutants, want 1 — %s differs from %s, so a test can kill the swap",
			c.typ, c.expr, len(got), c.expr, c.repl)
	}
	if got[0].Replacement != c.repl {
		t.Errorf("%s slot returning %s: substituted %q, want %q",
			c.typ, c.expr, got[0].Replacement, c.repl)
	}
}

func TestReturnZeroEquivalenceCorpus(t *testing.T) {
	sawBoth := map[bool]int{}
	for _, c := range equivalenceCorpus {
		sawBoth[c.same]++
		t.Run(c.typ+"/"+c.expr, func(t *testing.T) { assertEquivalenceCase(t, c) })
	}
	// A corpus that drifted to all-equivalent or all-distinct would still pass
	// every case above while testing only one side of the biconditional.
	if sawBoth[true] == 0 || sawBoth[false] == 0 {
		t.Errorf("corpus covers only one verdict: %d equivalent, %d distinct", sawBoth[true], sawBoth[false])
	}
}

// --- EnabledMutators with both only and disable ---

func TestRegistryEnabledMutatorsNoFilter(t *testing.T) {
	reg := mutator.NewRegistry()
	all := reg.EnabledMutators(nil, nil)
	if len(all) != 28 {
		t.Errorf("expected 28, got %d", len(all))
	}
}

// replacementCase pairs the expected source span (Original) with the
// replacement text the mutator should emit; consumed by assertReplacements.
type replacementCase struct{ Original, Replacement string }

// requireCandidates discovers candidates for the named mutator against src
// and fails the test fast if the count doesn't match wantCount. Returned
// candidates are the discovery's output, in walk order. Extracted so the
// numeric-literal / loop-shape mutator tests don't each repeat the
// parse-find-discover-count quartet (which SonarCloud's duplication gate
// flags on new code).
func requireCandidates(t *testing.T, typ mutator.MutationType, src string, wantCount int) []mutator.MutantCandidate {
	t.Helper()
	fset, file, srcBytes := parse(t, src)
	m := findMutator(t, typ)
	candidates := m.Discover(fset, file, srcBytes)
	if len(candidates) != wantCount {
		t.Fatalf("%s: got %d candidates, want %d (%+v)", typ, len(candidates), wantCount, candidates)
	}
	return candidates
}

// assertReplacements runs requireCandidates with len(want) as the count and
// asserts each candidate's (Original, Replacement) pair matches want[i].
func assertReplacements(t *testing.T, typ mutator.MutationType, src string, want []replacementCase) []mutator.MutantCandidate {
	t.Helper()
	cs := requireCandidates(t, typ, src, len(want))
	for i, c := range cs {
		if c.Original != want[i].Original || c.Replacement != want[i].Replacement {
			t.Errorf("%s candidate %d: got %q→%q, want %q→%q",
				typ, i, c.Original, c.Replacement, want[i].Original, want[i].Replacement)
		}
	}
	return cs
}

// findMutator returns the mutator of the given type from the registry.
func findMutator(t *testing.T, typ mutator.MutationType) mutator.Mutator {
	t.Helper()
	reg := mutator.NewRegistry()
	for _, m := range reg.Mutators() {
		if m.Type() == typ {
			return m
		}
	}
	t.Fatalf("mutator %v not found in registry", typ)
	return nil
}
