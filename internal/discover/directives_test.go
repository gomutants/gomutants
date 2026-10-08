package discover

import (
	"bytes"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/szhekpisov/gomutants/internal/mutator"
)

// writeFixture writes src to a temp file under a fresh package dir and
// runs Discover, returning the resulting mutants and the absolute path
// to the source file. The package always contains exactly one .go file.
func writeFixture(t *testing.T, src string) (mutants []mutator.Mutant, path string) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "src.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	pkgs := []Package{{Dir: dir, ImportPath: "example.com/test", GoFiles: []string{"src.go"}}}
	reg := mutator.NewRegistry()
	fset := token.NewFileSet()
	return Discover(fset, pkgs, reg.Mutators(), dir, "example.com/test").Mutants, path
}

// suppressedTypes returns the set of mutator types in suppressions.
func suppressedTypes(suppressed []Suppression) map[mutator.MutationType]int {
	out := make(map[mutator.MutationType]int)
	for _, s := range suppressed {
		out[s.Mutant.Type]++
	}
	return out
}

// keptTypes returns the set of mutator types in surviving mutants.
func keptTypes(kept []mutator.Mutant) map[mutator.MutationType]int {
	out := make(map[mutator.MutationType]int)
	for _, m := range kept {
		out[m.Type]++
	}
	return out
}

func TestSplitMutatorsAndReason(t *testing.T) {
	cases := []struct {
		in           string
		wantMutators string
		wantReason   string
		wantOK       bool
	}{
		{"", "", "", true},
		{"ARITHMETIC_BASE", "ARITHMETIC_BASE", "", true},
		{"A,B", "A,B", "", true},
		{`reason="hi"`, "", "hi", true},
		{`A reason="hi"`, "A", "hi", true},
		{`A,B reason="multi word"`, "A,B", "multi word", true},
		{`reason="esc \"q\""`, "", `esc "q"`, true},
		{`reason="unterminated`, "", "", false},
		{`reason=bare`, "", "", false},
		{`reason="ok" trailing`, "", "", false},
		{"reason=", "", "", false},
		{`A reason=`, "", "", false},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			gotM, gotR, gotOK := splitMutatorsAndReason(c.in)
			if gotOK != c.wantOK || gotM != c.wantMutators || gotR != c.wantReason {
				t.Errorf("splitMutatorsAndReason(%q) = (%q,%q,%v), want (%q,%q,%v)",
					c.in, gotM, gotR, gotOK, c.wantMutators, c.wantReason, c.wantOK)
			}
		})
	}
}

func TestParseDirectiveKinds(t *testing.T) {
	cases := []struct {
		text     string
		wantKind directiveKind
		wantOK   bool
	}{
		{"gomutants:disable", directiveSameLine, true},
		{"gomutants:disable-next-line", directiveNextLine, true},
		{"gomutants:disable-func", directiveFunc, true},
		{"gomutants:disable-regexp ^foo", directiveRegexp, true},
		{"gomutants:disable ARITHMETIC_BASE", directiveSameLine, true},
		{"gomutants:disable *", directiveSameLine, true},
		{"gomutants:disable-future-thing", 0, false}, // unknown kind: silently ignored
	}
	for _, c := range cases {
		t.Run(c.text, func(t *testing.T) {
			var buf bytes.Buffer
			d, ok := parseDirective(c.text, 1, "x.go", &buf)
			if ok != c.wantOK {
				t.Fatalf("ok=%v, want %v (warn=%q)", ok, c.wantOK, buf.String())
			}
			if ok && d.Kind != c.wantKind {
				t.Errorf("kind=%v, want %v", d.Kind, c.wantKind)
			}
		})
	}
}

func TestParseDirectiveUnknownMutatorWarns(t *testing.T) {
	var buf bytes.Buffer
	d, ok := parseDirective("gomutants:disable ARITHMETIC_BASE,NOT_A_MUTATOR", 5, "x.go", &buf)
	if !ok {
		t.Fatalf("expected directive to parse")
	}
	if !strings.Contains(buf.String(), `unknown mutator "NOT_A_MUTATOR"`) {
		t.Errorf("expected warning about unknown mutator, got %q", buf.String())
	}
	if _, ok := d.Mutators[mutator.ArithmeticBase]; !ok {
		t.Errorf("ARITHMETIC_BASE should still be in the directive's mutator set: %v", d.Mutators)
	}
	if _, ok := d.Mutators["NOT_A_MUTATOR"]; ok {
		t.Errorf("unknown mutator should not be in the directive's mutator set")
	}
}

func TestParseDirectiveAllUnknownDropsDirective(t *testing.T) {
	// When every named mutator is unknown, the directive is dropped (no
	// suppression) and a summary warning is emitted. Safer default than
	// matching all: a typo like `TYPP_O` must not silently disable every
	// mutator on the line — that would mask the exact failure mode
	// mutation testing is meant to surface.
	var buf bytes.Buffer
	_, ok := parseDirective("gomutants:disable BOGUS,ALSO_BOGUS", 5, "x.go", &buf)
	if ok {
		t.Fatalf("expected all-unknown directive to be dropped")
	}
	if !strings.Contains(buf.String(), "all named mutators unknown") {
		t.Errorf("expected summary warning; got %q", buf.String())
	}
	// Per-name warnings still fire so the user can see which names were wrong.
	if !strings.Contains(buf.String(), `unknown mutator "BOGUS"`) {
		t.Errorf("expected per-name warning for BOGUS; got %q", buf.String())
	}
}

func TestParseDirectiveMalformedReason(t *testing.T) {
	var buf bytes.Buffer
	_, ok := parseDirective(`gomutants:disable reason="unterminated`, 5, "x.go", &buf)
	if ok {
		t.Fatalf("expected malformed reason to drop directive")
	}
	if !strings.Contains(buf.String(), "malformed reason=") {
		t.Errorf("expected malformed-reason warning, got %q", buf.String())
	}
}

func TestParseDirectiveRegexpMissingPattern(t *testing.T) {
	var buf bytes.Buffer
	_, ok := parseDirective("gomutants:disable-regexp", 5, "x.go", &buf)
	if ok {
		t.Fatalf("expected missing pattern to drop directive")
	}
	if !strings.Contains(buf.String(), "missing pattern") {
		t.Errorf("expected missing-pattern warning, got %q", buf.String())
	}
}

func TestParseDirectiveRegexpInvalidPattern(t *testing.T) {
	var buf bytes.Buffer
	_, ok := parseDirective(`gomutants:disable-regexp [unclosed`, 5, "x.go", &buf)
	if ok {
		t.Fatalf("expected invalid pattern to drop directive")
	}
	if !strings.Contains(buf.String(), "invalid pattern") {
		t.Errorf("expected invalid-pattern warning, got %q", buf.String())
	}
}

func TestParseDirectiveReasonRoundTrips(t *testing.T) {
	d, ok := parseDirective(`gomutants:disable reason="commutative \"add\""`, 5, "x.go", &bytes.Buffer{})
	if !ok {
		t.Fatalf("expected directive to parse")
	}
	if want := `commutative "add"`; d.Reason != want {
		t.Errorf("reason=%q, want %q", d.Reason, want)
	}
}

func TestNextNonCommentLine(t *testing.T) {
	lines := []string{
		"package p",          // 1
		"",                   // 2
		"// comment",         // 3 (the directive sits here)
		"// another comment", // 4
		"",                   // 5
		"x := a + b",         // 6 — first non-comment, non-blank line
	}
	if got := nextNonCommentLine(lines, 3); got != 6 {
		t.Errorf("nextNonCommentLine(_, 3) = %d, want 6", got)
	}
	// Off the end returns 0 (sentinel for "no following code").
	if got := nextNonCommentLine(lines, 6); got != 0 {
		t.Errorf("nextNonCommentLine off-end = %d, want 0", got)
	}
}

func TestFilterNextLineAtEOFWarns(t *testing.T) {
	// Directive sits on the last meaningful line with no code after it —
	// should emit a warning and not suppress anything.
	src := `package p

func F(a, b int) int { return a + b }

// gomutants:disable-next-line
`
	mutants, _ := writeFixture(t, src)
	fset := token.NewFileSet()
	var warn bytes.Buffer
	kept, suppressed, err := filterByDirectives(fset, mutants, nil, &warn)
	if err != nil {
		t.Fatal(err)
	}
	if len(suppressed) != 0 {
		t.Errorf("expected no suppressions; got %d", len(suppressed))
	}
	if len(kept) != len(mutants) {
		t.Errorf("expected all mutants to survive; got kept=%d in=%d", len(kept), len(mutants))
	}
	if !strings.Contains(warn.String(), "no following code") {
		t.Errorf("expected EOF warning; got %q", warn.String())
	}
}

// TestFilterByDirectivesSilently: same verdicts as the warning variant,
// with nothing written to stderr for a directive that would warn.
func TestFilterByDirectivesSilently(t *testing.T) {
	src := `package p

func F(a, b int) int { return a + b } // gomutants:disable reason=unterminated"

func G(a, b int) int { return a - b } // gomutants:disable ARITHMETIC_BASE
`
	mutants, _ := writeFixture(t, src)

	var (
		kept       []mutator.Mutant
		suppressed []Suppression
		err        error
	)
	out := captureStderr(t, func() {
		kept, suppressed, err = FilterByDirectivesSilently(token.NewFileSet(), mutants, nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Errorf("FilterByDirectivesSilently wrote to stderr: %q", out)
	}

	var warn bytes.Buffer
	wantKept, wantSuppressed, err := filterByDirectives(token.NewFileSet(), mutants, nil, &warn)
	if err != nil {
		t.Fatal(err)
	}
	if warn.Len() == 0 {
		t.Fatal("fixture should carry a directive that warns")
	}
	if len(kept) != len(wantKept) || len(suppressed) != len(wantSuppressed) || len(suppressed) == 0 {
		t.Errorf("kept=%d suppressed=%d, want kept=%d suppressed=%d (>0)",
			len(kept), len(suppressed), len(wantKept), len(wantSuppressed))
	}
}

// captureStderr redirects os.Stderr through a pipe for the duration of fn
// and returns whatever was written.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	done := make(chan struct{})
	var captured strings.Builder
	go func() {
		defer close(done)
		_, _ = io.Copy(&captured, r)
	}()
	fn()
	w.Close()
	<-done
	return captured.String()
}

func TestParseDirectiveRegexpUnknownMutatorHintsWhitespace(t *testing.T) {
	// `disable-regexp foo bar` — `bar` becomes the mutator name and is
	// unknown; under the all-unknown-drops-directive rule the directive
	// is dropped, but the whitespace hint warning must still fire so the
	// user understands *why* their pattern wasn't accepted.
	var buf bytes.Buffer
	_, ok := parseDirective("gomutants:disable-regexp foo bar", 5, "x.go", &buf)
	if ok {
		t.Fatalf("expected directive to be dropped (all named mutators unknown); warn=%q", buf.String())
	}
	if !strings.Contains(buf.String(), `unknown mutator "bar"`) {
		t.Errorf("expected unknown-mutator warning; got %q", buf.String())
	}
	if !strings.Contains(buf.String(), `cannot contain whitespace`) {
		t.Errorf("expected whitespace hint in regex-context warning; got %q", buf.String())
	}
}

func TestFilterSameLine(t *testing.T) {
	src := `package p

func F(a, b int) int {
	return a + b // gomutants:disable ARITHMETIC_BASE reason="commutative"
}
`
	mutants, _ := writeFixture(t, src)
	hadArithmetic := false
	for _, m := range mutants {
		if m.Type == mutator.ArithmeticBase {
			hadArithmetic = true
		}
	}
	if !hadArithmetic {
		t.Fatal("fixture didn't produce any ARITHMETIC_BASE mutant; check src")
	}

	fset := token.NewFileSet()
	kept, suppressed, err := FilterByDirectives(fset, mutants)
	if err != nil {
		t.Fatal(err)
	}
	if got := suppressedTypes(suppressed)[mutator.ArithmeticBase]; got == 0 {
		t.Errorf("expected ARITHMETIC_BASE to be suppressed; got %v", suppressedTypes(suppressed))
	}
	if got := keptTypes(kept)[mutator.ArithmeticBase]; got != 0 {
		t.Errorf("ARITHMETIC_BASE should be removed from kept; got %d", got)
	}
	if len(suppressed) > 0 && suppressed[0].Reason != "commutative" {
		t.Errorf("reason=%q, want commutative", suppressed[0].Reason)
	}
}

func TestFilterNextLine(t *testing.T) {
	src := `package p

func F(a, b int) int {
	// gomutants:disable-next-line
	return a + b
}
`
	mutants, _ := writeFixture(t, src)
	fset := token.NewFileSet()
	kept, suppressed, err := FilterByDirectives(fset, mutants)
	if err != nil {
		t.Fatal(err)
	}
	if len(suppressed) == 0 {
		t.Fatal("expected at least one suppressed mutant on the line after the directive")
	}
	for _, m := range kept {
		if m.Line == 5 {
			t.Errorf("mutant on line 5 (%s) was not suppressed", m.Type)
		}
	}
}

func TestFilterNextLineSkipsCommentLines(t *testing.T) {
	// disable-next-line on line 3; line 4 is also a comment; line 5 is the actual code.
	src := `package p

// gomutants:disable-next-line
// the comment we want to skip
func F(a, b int) int { return a + b }
`
	mutants, _ := writeFixture(t, src)
	fset := token.NewFileSet()
	kept, suppressed, err := FilterByDirectives(fset, mutants)
	if err != nil {
		t.Fatal(err)
	}
	if len(suppressed) == 0 {
		t.Fatal("expected directive to skip past comment-only lines and suppress code on line 5")
	}
	for _, m := range kept {
		if m.Line == 5 {
			t.Errorf("mutant on line 5 (%s) was not suppressed", m.Type)
		}
	}
}

func TestFilterFunc(t *testing.T) {
	src := `package p

// gomutants:disable-func reason="generated"
func F(a, b int) int {
	if a > b {
		return a + b
	}
	return a - b
}

func G(a, b int) int {
	return a * b
}
`
	mutants, _ := writeFixture(t, src)
	fset := token.NewFileSet()
	kept, suppressed, err := FilterByDirectives(fset, mutants)
	if err != nil {
		t.Fatal(err)
	}

	// All mutants in F (lines 4..9) should be suppressed; G's stay.
	for _, s := range suppressed {
		if s.Mutant.Line < 4 || s.Mutant.Line > 9 {
			t.Errorf("suppressed mutant outside F's body: line=%d type=%s", s.Mutant.Line, s.Mutant.Type)
		}
	}
	gKept := false
	for _, m := range kept {
		if m.Line == 12 && m.Type == mutator.ArithmeticBase {
			gKept = true
		}
		if m.Line >= 4 && m.Line <= 9 {
			t.Errorf("F-body mutant survived disable-func: line=%d type=%s", m.Line, m.Type)
		}
	}
	if !gKept {
		t.Errorf("G's ARITHMETIC_BASE on line 12 should have survived")
	}
}

func TestFilterFuncOnNonFuncWarns(t *testing.T) {
	src := `package p

// gomutants:disable-func
var x = 1 + 2
`
	mutants, _ := writeFixture(t, src)
	fset := token.NewFileSet()
	var warn bytes.Buffer
	kept, suppressed, err := filterByDirectives(fset, mutants, nil, &warn)
	if err != nil {
		t.Fatal(err)
	}
	if len(suppressed) != 0 {
		t.Errorf("disable-func not on a FuncDecl should not suppress anything; got %d", len(suppressed))
	}
	if !strings.Contains(warn.String(), "not on a function declaration") {
		t.Errorf("expected misplaced-disable-func warning; got %q", warn.String())
	}
	if len(kept) != len(mutants) {
		t.Errorf("nothing should have been filtered; got kept=%d in=%d", len(kept), len(mutants))
	}
}

func TestFilterRegexp(t *testing.T) {
	src := `package p

// gomutants:disable-regexp ^\s*log\.

import "log"

func F(a, b int) int {
	log.Printf("a+b = %d", a+b)
	return a + b
}
`
	mutants, _ := writeFixture(t, src)
	fset := token.NewFileSet()
	kept, suppressed, err := FilterByDirectives(fset, mutants)
	if err != nil {
		t.Fatal(err)
	}
	// The log.Printf line should be suppressed; the bare `return a + b` should not.
	logSuppressed := false
	for _, s := range suppressed {
		if strings.Contains(s.Mutant.Original, "+") && s.Mutant.Line == 8 {
			logSuppressed = true
		}
	}
	if !logSuppressed {
		t.Errorf("expected mutants on log.Printf line to be suppressed; got %v", suppressed)
	}
	returnSurvived := false
	for _, m := range kept {
		if m.Line == 9 && m.Type == mutator.ArithmeticBase {
			returnSurvived = true
		}
	}
	if !returnSurvived {
		t.Errorf("ARITHMETIC_BASE on the return line should not be suppressed; kept=%v", kept)
	}
}

func TestFilterMutatorTypeScoping(t *testing.T) {
	src := `package p

func F(a, b int, c bool) int {
	if c { return a + b } // gomutants:disable ARITHMETIC_BASE
	return a - b
}
`
	mutants, _ := writeFixture(t, src)
	fset := token.NewFileSet()
	kept, suppressed, err := FilterByDirectives(fset, mutants)
	if err != nil {
		t.Fatal(err)
	}
	// On line 4: ARITHMETIC_BASE should be suppressed; BRANCH_IF (and any
	// other non-arithmetic mutators on the same line) should survive.
	for _, s := range suppressed {
		if s.Mutant.Line == 4 && s.Mutant.Type != mutator.ArithmeticBase {
			t.Errorf("non-ARITHMETIC mutant suppressed on line 4: %s", s.Mutant.Type)
		}
	}
	branchIfKept := false
	for _, m := range kept {
		if m.Line == 4 && m.Type == mutator.BranchIf {
			branchIfKept = true
		}
	}
	if !branchIfKept {
		t.Errorf("BRANCH_IF on line 4 should have survived (directive only named ARITHMETIC_BASE)")
	}
}

func TestFilterWildcardSuppressesAll(t *testing.T) {
	src := `package p

func F(a, b int, c bool) int {
	if c { return a + b } // gomutants:disable *
	return a - b
}
`
	mutants, _ := writeFixture(t, src)
	fset := token.NewFileSet()
	kept, _, err := FilterByDirectives(fset, mutants)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range kept {
		if m.Line == 4 {
			t.Errorf("wildcard should suppress every mutant on line 4; got survivor %s", m.Type)
		}
	}
}

func TestFilterMultiFileIsolation(t *testing.T) {
	dir := t.TempDir()
	srcA := `package p

func A(a, b int) int { return a + b } // gomutants:disable ARITHMETIC_BASE
`
	srcB := `package p

func B(a, b int) int { return a + b }
`
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(srcA), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.go"), []byte(srcB), 0o644); err != nil {
		t.Fatal(err)
	}
	pkgs := []Package{{Dir: dir, ImportPath: "example.com/test", GoFiles: []string{"a.go", "b.go"}}}
	reg := mutator.NewRegistry()
	fset := token.NewFileSet()
	mutants := Discover(fset, pkgs, reg.Mutators(), dir, "example.com/test").Mutants

	kept, suppressed, err := FilterByDirectives(fset, mutants)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range suppressed {
		if filepath.Base(s.Mutant.File) != "a.go" {
			t.Errorf("directive in a.go leaked into %s", s.Mutant.File)
		}
	}
	bArithmeticKept := false
	for _, m := range kept {
		if filepath.Base(m.File) == "b.go" && m.Type == mutator.ArithmeticBase {
			bArithmeticKept = true
		}
	}
	if !bArithmeticKept {
		t.Errorf("b.go's ARITHMETIC_BASE should have survived")
	}
}

func TestFilterEmpty(t *testing.T) {
	fset := token.NewFileSet()
	kept, suppressed, err := FilterByDirectives(fset, nil)
	if err != nil {
		t.Fatal(err)
	}
	if kept != nil || suppressed != nil {
		t.Errorf("empty input should yield empty output; got kept=%v suppressed=%v", kept, suppressed)
	}
}

func TestFilterNoDirectivesKeepsAll(t *testing.T) {
	src := `package p

func F(a, b int) int { return a + b }
`
	mutants, _ := writeFixture(t, src)
	fset := token.NewFileSet()
	kept, suppressed, err := FilterByDirectives(fset, mutants)
	if err != nil {
		t.Fatal(err)
	}
	if len(suppressed) != 0 {
		t.Errorf("expected zero suppressed; got %d", len(suppressed))
	}
	if len(kept) != len(mutants) {
		t.Errorf("expected all mutants to be kept; got kept=%d in=%d", len(kept), len(mutants))
	}
}

// TestParseDirectiveLeadingWhitespaceAfterPrefix kills STATEMENT_REMOVE
// on splitFirstWord's leading TrimLeft: without the trim, the kind word
// is "" and the directive is silently dropped.
func TestParseDirectiveLeadingWhitespaceAfterPrefix(t *testing.T) {
	d, ok := parseDirective("gomutants:  disable ARITHMETIC_BASE", 1, "x.go", &bytes.Buffer{})
	if !ok {
		t.Fatalf("directive with extra whitespace after prefix should still parse")
	}
	if d.Kind != directiveSameLine {
		t.Errorf("kind=%v, want directiveSameLine", d.Kind)
	}
	if _, ok := d.Mutators[mutator.ArithmeticBase]; !ok {
		t.Errorf("ARITHMETIC_BASE should be in directive's mutator set: %v", d.Mutators)
	}
}

// TestParseDirectiveWildcardEmitsNoWarning kills two parseDirective
// mutants on the `mutatorsStr != "" && mutatorsStr != "*"` guard:
// any mutation that admits "*" into the per-name loop produces a
// spurious "unknown mutator" warning.
func TestParseDirectiveWildcardEmitsNoWarning(t *testing.T) {
	var buf bytes.Buffer
	d, ok := parseDirective("gomutants:disable *", 1, "x.go", &buf)
	if !ok {
		t.Fatalf("expected directive to parse; warn=%q", buf.String())
	}
	if d.Mutators != nil {
		t.Errorf("`disable *` should leave Mutators nil (= all); got %v", d.Mutators)
	}
	if buf.Len() != 0 {
		t.Errorf("`disable *` must emit no warnings; got %q", buf.String())
	}
}

// TestParseDirectiveBlankNameInList kills the BRANCH_IF on
// `if name == "" { continue }`: dropping that continue lets the empty
// name reach isKnownMutator and produces a spurious unknown-mutator
// warning.
func TestParseDirectiveBlankNameInList(t *testing.T) {
	var buf bytes.Buffer
	d, ok := parseDirective("gomutants:disable ARITHMETIC_BASE,,BRANCH_IF", 1, "x.go", &buf)
	if !ok {
		t.Fatalf("expected directive to parse; warn=%q", buf.String())
	}
	if _, ok := d.Mutators[mutator.ArithmeticBase]; !ok {
		t.Errorf("ARITHMETIC_BASE missing: %v", d.Mutators)
	}
	if _, ok := d.Mutators[mutator.BranchIf]; !ok {
		t.Errorf("BRANCH_IF missing: %v", d.Mutators)
	}
	if strings.Contains(buf.String(), `unknown mutator ""`) {
		t.Errorf("blank entry should not produce an unknown-mutator warning; got %q", buf.String())
	}
}

// TestParseDirectivePartialUnknownContinuesPastUnknown kills the
// INVERT_LOOP_CTRL `continue → break` after the unknown-mutator
// warning. With break, the known name after an unknown one would
// never be registered.
func TestParseDirectivePartialUnknownContinuesPastUnknown(t *testing.T) {
	d, ok := parseDirective("gomutants:disable BOGUS,ARITHMETIC_BASE", 1, "x.go", &bytes.Buffer{})
	if !ok {
		t.Fatalf("expected directive to parse")
	}
	if _, ok := d.Mutators[mutator.ArithmeticBase]; !ok {
		t.Errorf("ARITHMETIC_BASE following an unknown name should still be registered: %v", d.Mutators)
	}
}

// TestParseDirectiveTrimsNamesWithWhitespace kills STATEMENT_REMOVE on
// `name = strings.TrimSpace(name)`: untrimmed names are rejected by
// isKnownMutator.
func TestParseDirectiveTrimsNamesWithWhitespace(t *testing.T) {
	var buf bytes.Buffer
	d, ok := parseDirective("gomutants:disable  ARITHMETIC_BASE  ,  BRANCH_IF  ", 1, "x.go", &buf)
	if !ok {
		t.Fatalf("expected directive to parse; warn=%q", buf.String())
	}
	if _, ok := d.Mutators[mutator.ArithmeticBase]; !ok {
		t.Errorf("ARITHMETIC_BASE should be registered after trimming: %v", d.Mutators)
	}
	if _, ok := d.Mutators[mutator.BranchIf]; !ok {
		t.Errorf("BRANCH_IF should be registered after trimming: %v", d.Mutators)
	}
	if buf.Len() != 0 {
		t.Errorf("trimmed names must produce no warnings; got %q", buf.String())
	}
}

// TestParseDirectiveUnknownKindIsSilent locks the forward-compat
// behaviour: future kinds (or typos that look like kinds) are dropped
// without a warning so a newer-release directive does not break older
// gomutants runs.
func TestParseDirectiveUnknownKindIsSilent(t *testing.T) {
	var buf bytes.Buffer
	_, ok := parseDirective("gomutants:disable-future-thing", 1, "x.go", &buf)
	if ok {
		t.Errorf("unknown kind should not parse")
	}
	if buf.Len() != 0 {
		t.Errorf("unknown kind must be silent (forward-compat); got %q", buf.String())
	}
}

// TestFilterFuncBoundariesAndScope is the comprehensive disable-func
// test. It covers, in one fixture:
//   - mutants in another function ABOVE the disable-func target are
//     not suppressed (kills STATEMENT_REMOVE on `d.FuncStart = funcRange[0]`
//     and EXPRESSION_REMOVE `m.Line >= d.FuncStart → true`)
//   - mutants on the FuncStart line and the FuncEnd line are suppressed
//     (locks `>=` and `<=` boundaries against `>` / `<`)
//   - mutator-scoped disable-func suppresses only the named mutator
//     type (kills EXPRESSION_REMOVE `directiveMatchesType(...) → true`)
func TestFilterFuncBoundariesAndScope(t *testing.T) {
	src := `package p

func A(a, b int) int {
	return a + b
}

// gomutants:disable-func ARITHMETIC_BASE
func B(a, b int) int {
	if a > b {
		return a + b
	}
	return a - b
}
`
	mutants, _ := writeFixture(t, src)
	fset := token.NewFileSet()
	kept, suppressed, err := FilterByDirectives(fset, mutants)
	if err != nil {
		t.Fatal(err)
	}

	// A is above B; A's ARITHMETIC_BASE on its `return a + b` line
	// must survive — its line number is below B's FuncStart.
	aArithKept := false
	for _, m := range kept {
		if m.Type == mutator.ArithmeticBase && m.Line == 4 {
			aArithKept = true
		}
	}
	if !aArithKept {
		t.Errorf("A's ARITHMETIC_BASE on line 4 must survive (not in B's range)")
	}

	// B's CONDITIONALS_BOUNDARY on `a > b` must NOT be suppressed:
	// the directive scopes to ARITHMETIC_BASE.
	condKept := false
	for _, m := range kept {
		if m.Type == mutator.ConditionalsBoundary && m.Line == 9 {
			condKept = true
		}
	}
	if !condKept {
		t.Errorf("B's CONDITIONALS_BOUNDARY on line 9 must survive (mutator-scoped directive)")
	}

	// B's ARITHMETIC_BASE inside the body (lines 10 and 12) must be
	// suppressed. Line 8 is FuncStart (the `func B(...) int {` line),
	// line 13 is FuncEnd (the closing `}`).
	wantSuppressedLines := map[int]bool{10: true, 12: true}
	for line := range wantSuppressedLines {
		found := false
		for _, s := range suppressed {
			if s.Mutant.Type == mutator.ArithmeticBase && s.Mutant.Line == line {
				found = true
			}
		}
		if !found {
			t.Errorf("B's ARITHMETIC_BASE on line %d should have been suppressed", line)
		}
	}
}

// TestFilterFuncSingleLineBoundaries puts the function body on a single
// line so a mutant's Line equals both d.FuncStart and d.FuncEnd. This
// kills the boundary mutants on `>=` (→ `>`) and `<=` (→ `<`) — `>` /
// `<` would exclude the line that matches both endpoints.
func TestFilterFuncSingleLineBoundaries(t *testing.T) {
	src := `package p

// gomutants:disable-func
func F(a, b int) int { return a + b }
`
	mutants, _ := writeFixture(t, src)
	fset := token.NewFileSet()
	kept, suppressed, err := FilterByDirectives(fset, mutants)
	if err != nil {
		t.Fatal(err)
	}
	if len(suppressed) == 0 {
		t.Fatalf("expected mutants on the single-line function body to be suppressed")
	}
	for _, m := range kept {
		if m.Line == 4 {
			t.Errorf("single-line body mutant on line 4 (= FuncStart = FuncEnd) survived: %s", m.Type)
		}
	}
}

// TestFilterFuncSkipsNonFuncDeclSibling guards the decl-loop continue:
// removing it would panic on the non-FuncDecl `var x = 1` (fd is nil),
// and the INVERT_LOOP_CTRL mutation `continue → break` would stop
// iteration before reaching F's directive.
func TestFilterFuncSkipsNonFuncDeclSibling(t *testing.T) {
	src := `package p

var x = 1

// gomutants:disable-func
func F(a, b int) int { return a + b }
`
	mutants, _ := writeFixture(t, src)
	fset := token.NewFileSet()
	kept, suppressed, err := FilterByDirectives(fset, mutants)
	if err != nil {
		t.Fatal(err)
	}
	if len(suppressed) == 0 {
		t.Fatalf("expected F's body mutants to be suppressed despite the var decl above it")
	}
	for _, m := range kept {
		if m.Line == 6 {
			t.Errorf("F-body mutant survived the disable-func directive: %s", m.Type)
		}
	}
}

// TestFilterMalformedDirectiveDoesNotSuppress kills the BRANCH_IF on
// `if !parsedOK { continue }`: removing the continue would register
// a zero-valued (kind=directiveSameLine, Mutators=nil) directive on
// the malformed line, suppressing every mutant there.
func TestFilterMalformedDirectiveDoesNotSuppress(t *testing.T) {
	src := `package p

func F(a, b int) int {
	return a + b // gomutants:disable reason="unterminated
}
`
	mutants, _ := writeFixture(t, src)
	fset := token.NewFileSet()
	var warn bytes.Buffer
	kept, suppressed, err := filterByDirectives(fset, mutants, nil, &warn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warn.String(), "malformed reason=") {
		t.Errorf("expected malformed-reason warning; got %q", warn.String())
	}
	if len(suppressed) != 0 {
		t.Errorf("malformed directive must not suppress anything; got %d", len(suppressed))
	}
	if len(kept) != len(mutants) {
		t.Errorf("all mutants must be kept; got kept=%d in=%d", len(kept), len(mutants))
	}
}

// TestFilterNonDirectiveCommentNotMisinterpreted kills the BRANCH_IF on
// `if !strings.HasPrefix(text, directivePrefix) { continue }`: removing
// the continue would let a regular comment whose first word matches a
// directive kind (e.g., "disable") slip into parseDirective and silently
// register a same-line directive that suppresses everything on its line.
//
// The non-directive comment must parse to a *valid* directive once the
// prefix check is gone, otherwise the bug is unobservable: `disable for
// now` is rejected (unknown mutators), but `disable reason="..."` parses
// to a same-line directive with empty Mutators ("all"). It sits on the
// same line as the mutated expression so the fictitious directive's
// same-line scope would actually catch the mutant.
//
// The fixture also needs a real directive *somewhere* so the
// bytes.Contains fast-path doesn't short-circuit before the prefix-check
// runs.
func TestFilterNonDirectiveCommentNotMisinterpreted(t *testing.T) {
	src := `package p

// gomutants:disable-regexp __DOES_NOT_MATCH_ANY_LINE__

func F(a, b int) int {
	return a + b // disable reason="for now"
}
`
	mutants, _ := writeFixture(t, src)
	fset := token.NewFileSet()
	kept, suppressed, err := FilterByDirectives(fset, mutants)
	if err != nil {
		t.Fatal(err)
	}
	if len(suppressed) != 0 {
		t.Errorf("non-directive comment must not suppress anything; got %d: %+v", len(suppressed), suppressed)
	}
	if len(kept) != len(mutants) {
		t.Errorf("all mutants must be kept; got kept=%d in=%d", len(kept), len(mutants))
	}
}

// TestFilterRegexpAtPackageLevel locks the placement freedom the README
// implies: a `disable-regexp` directive at file scope (before any func)
// applies to every matching line in the file.
func TestFilterRegexpAtPackageLevel(t *testing.T) {
	src := `package p

// gomutants:disable-regexp ^\s*return\s+a\s*\+\s*b\b

func F(a, b int) int {
	return a + b
}

func G(a, b int) int {
	return a + b
}
`
	mutants, _ := writeFixture(t, src)
	fset := token.NewFileSet()
	kept, suppressed, err := FilterByDirectives(fset, mutants)
	if err != nil {
		t.Fatal(err)
	}
	if len(suppressed) == 0 {
		t.Fatalf("expected package-level disable-regexp to suppress matching lines")
	}
	for _, m := range kept {
		if m.Type == mutator.ArithmeticBase && (m.Line == 6 || m.Line == 10) {
			t.Errorf("ARITHMETIC_BASE on line %d (matches package-level regexp) should be suppressed", m.Line)
		}
	}
}

// TestFilterByDirectivesPerMutantClassification asserts that two
// mutants in the same file are classified independently against the
// shared per-file index: a same-line directive on one line suppresses
// only that line's mutant, leaving an adjacent-line mutant untouched.
func TestFilterByDirectivesPerMutantClassification(t *testing.T) {
	src := `package p

func F(a, b int) int {
	x := a + b // gomutants:disable ARITHMETIC_BASE
	return x + a
}
`
	mutants, _ := writeFixture(t, src)
	fset := token.NewFileSet()
	kept, suppressed, err := FilterByDirectives(fset, mutants)
	if err != nil {
		t.Fatal(err)
	}
	// Line 4 ARITHMETIC_BASE suppressed, line 5 ARITHMETIC_BASE kept.
	supLine4 := false
	for _, s := range suppressed {
		if s.Mutant.Line == 4 && s.Mutant.Type == mutator.ArithmeticBase {
			supLine4 = true
		}
	}
	if !supLine4 {
		t.Errorf("expected line-4 ARITHMETIC_BASE to be suppressed; suppressed=%+v", suppressed)
	}
	keptLine5 := false
	for _, m := range kept {
		if m.Line == 5 && m.Type == mutator.ArithmeticBase {
			keptLine5 = true
		}
	}
	if !keptLine5 {
		t.Errorf("expected line-5 ARITHMETIC_BASE to survive; kept=%+v", kept)
	}
}

// TestFilterByDirectivesUnreadableFile drives the os.ReadFile error path:
// a synthetic mutant pointing to a non-existent file makes buildFileIndex
// return the read error.
func TestFilterByDirectivesUnreadableFile(t *testing.T) {
	m := mutator.Mutant{
		File:    "/nonexistent/does-not-exist.go",
		RelFile: "does-not-exist.go",
		Line:    1,
		Type:    mutator.ArithmeticBase,
	}
	fset := token.NewFileSet()
	_, _, err := filterByDirectives(fset, []mutator.Mutant{m}, nil, io.Discard)
	if err == nil {
		t.Errorf("expected an error for unreadable file path")
	}
}

// TestFilterMultiFileBothHaveDirectives kills INVERT_LOOP_CTRL on the
// index-build loop's `continue → break`: with break, only the first
// file in iteration order is indexed and the second file's directives
// are silently skipped.
func TestFilterMultiFileBothHaveDirectives(t *testing.T) {
	dir := t.TempDir()
	srcA := `package p

func A(a, b int) int { return a + b } // gomutants:disable ARITHMETIC_BASE
func A2(a, b int) int { return a + b } // gomutants:disable ARITHMETIC_BASE
`
	srcB := `package p

func B(a, b int) int { return a + b } // gomutants:disable ARITHMETIC_BASE
func B2(a, b int) int { return a + b } // gomutants:disable ARITHMETIC_BASE
`
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(srcA), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.go"), []byte(srcB), 0o644); err != nil {
		t.Fatal(err)
	}
	pkgs := []Package{{Dir: dir, ImportPath: "example.com/test", GoFiles: []string{"a.go", "b.go"}}}
	reg := mutator.NewRegistry()
	fset := token.NewFileSet()
	mutants := Discover(fset, pkgs, reg.Mutators(), dir, "example.com/test").Mutants

	kept, _, err := FilterByDirectives(fset, mutants)
	if err != nil {
		t.Fatal(err)
	}
	// Both files' ARITHMETIC_BASE mutants must have been suppressed.
	for _, m := range kept {
		if m.Type == mutator.ArithmeticBase {
			t.Errorf("ARITHMETIC_BASE in %s line %d should have been suppressed", filepath.Base(m.File), m.Line)
		}
	}
}

// TestFilterFuncForwardDeclSibling kills EXPRESSION_REMOVE on the
// `fd.Body == nil` guard. Without that guard, a doc-commented forward
// declaration (no body) flows past the continue and dereferences a nil
// fd.Body.Lbrace.
func TestFilterFuncForwardDeclSibling(t *testing.T) {
	src := `package p

// External assembly impl — body lives in a .s file.
func ext()

// gomutants:disable-func
func F(a, b int) int { return a + b }
`
	mutants, _ := writeFixture(t, src)
	fset := token.NewFileSet()
	kept, suppressed, err := FilterByDirectives(fset, mutants)
	if err != nil {
		t.Fatal(err)
	}
	if len(suppressed) == 0 {
		t.Fatalf("expected F's body mutants to be suppressed despite the forward-decl above")
	}
	for _, m := range kept {
		if m.Line == 7 {
			t.Errorf("F-body mutant on line 7 should have been suppressed: %s", m.Type)
		}
	}
}

// TestFilterCommentGroupNonDirectiveThenDirective kills INVERT_LOOP_CTRL
// on the prefix-check `continue → break`. The two doc comments on F
// share one comment group; with break, the first non-directive line
// terminates iteration before the directive is reached.
func TestFilterCommentGroupNonDirectiveThenDirective(t *testing.T) {
	src := `package p

// Some explanatory text about F.
// gomutants:disable-func
func F(a, b int) int { return a + b }
`
	mutants, _ := writeFixture(t, src)
	fset := token.NewFileSet()
	kept, suppressed, err := FilterByDirectives(fset, mutants)
	if err != nil {
		t.Fatal(err)
	}
	if len(suppressed) == 0 {
		t.Fatalf("expected F's body mutants to be suppressed when the directive sits below a non-directive comment in the same doc group")
	}
	for _, m := range kept {
		if m.Line == 5 {
			t.Errorf("F-body mutant on line 5 should have been suppressed: %s", m.Type)
		}
	}
}

// TestFilterCommentGroupMalformedThenValid kills INVERT_LOOP_CTRL on
// the parsedOK `continue → break`. With break, the first malformed
// directive in a group terminates iteration and the valid directive
// after it never registers.
func TestFilterCommentGroupMalformedThenValid(t *testing.T) {
	src := `package p

// gomutants:disable reason="unterminated
// gomutants:disable-func
func F(a, b int) int { return a + b }
`
	mutants, _ := writeFixture(t, src)
	fset := token.NewFileSet()
	var warn bytes.Buffer
	kept, suppressed, err := filterByDirectives(fset, mutants, nil, &warn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warn.String(), "malformed reason=") {
		t.Errorf("expected malformed-reason warning; got %q", warn.String())
	}
	if len(suppressed) == 0 {
		t.Fatalf("expected F's body mutants to be suppressed by the second (valid) directive in the same group")
	}
	for _, m := range kept {
		if m.Line == 5 {
			t.Errorf("F-body mutant on line 5 should have been suppressed: %s", m.Type)
		}
	}
}

// TestFilterCommentGroupMisplacedFuncThenNextLine kills INVERT_LOOP_CTRL
// on the !isFuncDoc `continue → break`. With break, the misplaced
// disable-func at the head of the group terminates iteration and the
// disable-next-line below it never targets the var.
func TestFilterCommentGroupMisplacedFuncThenNextLine(t *testing.T) {
	src := `package p

// gomutants:disable-func
// gomutants:disable-next-line
var x = 1 + 2
`
	mutants, _ := writeFixture(t, src)
	fset := token.NewFileSet()
	var warn bytes.Buffer
	kept, suppressed, err := filterByDirectives(fset, mutants, nil, &warn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warn.String(), "not on a function declaration") {
		t.Errorf("expected misplaced-disable-func warning; got %q", warn.String())
	}
	if len(suppressed) == 0 {
		t.Fatalf("expected disable-next-line to suppress var mutants despite the misplaced disable-func above it")
	}
	for _, m := range kept {
		if m.Line == 5 {
			t.Errorf("var mutant on line 5 should have been suppressed: %s", m.Type)
		}
	}
}

// TestFilterTwoTrailingNextLineWarnsForBoth kills INVERT_LOOP_CTRL on
// the `if target == 0 ... continue → break` in the pending loop. With
// break, only the first of two trailing disable-next-line directives
// gets warned about.
func TestFilterTwoTrailingNextLineWarnsForBoth(t *testing.T) {
	src := `package p

func F() int { return 1 + 2 }

// gomutants:disable-next-line
// gomutants:disable-next-line
`
	mutants, _ := writeFixture(t, src)
	fset := token.NewFileSet()
	var warn bytes.Buffer
	if _, _, err := filterByDirectives(fset, mutants, nil, &warn); err != nil {
		t.Fatal(err)
	}
	w := warn.String()
	if !strings.Contains(w, "src.go:5:") {
		t.Errorf("expected warning for line-5 directive; got %q", w)
	}
	if !strings.Contains(w, "src.go:6:") {
		t.Errorf("expected warning for line-6 directive too (break would skip it); got %q", w)
	}
}

// TestFilterRegexpRespectsMutatorScope kills EXPRESSION_REMOVE on the
// `directiveMatchesType(d, m.Type) → true` in matchMutant's regexp arm.
// A regex directive scoped to ARITHMETIC_BASE must leave a non-arithmetic
// mutant on the matching line untouched.
func TestFilterRegexpRespectsMutatorScope(t *testing.T) {
	src := `package p

import "log"

// gomutants:disable-regexp ^\s*log\. ARITHMETIC_BASE

func F(a, b int) int {
	log.Printf("a > b: %v, a+b: %d", a > b, a+b)
	return a + b
}
`
	mutants, _ := writeFixture(t, src)
	fset := token.NewFileSet()
	kept, _, err := FilterByDirectives(fset, mutants)
	if err != nil {
		t.Fatal(err)
	}
	condBoundaryKept := false
	for _, m := range kept {
		if m.Type == mutator.ConditionalsBoundary && m.Line == 8 {
			condBoundaryKept = true
		}
	}
	if !condBoundaryKept {
		t.Errorf("CONDITIONALS_BOUNDARY on the log.Printf line should NOT be suppressed by an ARITHMETIC_BASE-scoped regexp")
	}
}

// TestFilterByDirectivesWithCacheSkipsReread verifies that when the parse
// cache from Discover is supplied, FilterByDirectivesWithCache does not
// touch the filesystem: the source file is removed before the call and
// the directive must still suppress its target.
func TestFilterByDirectivesWithCacheSkipsReread(t *testing.T) {
	src := `package p

func F(a, b int) int {
	// gomutants:disable-next-line ARITHMETIC_BASE
	return a + b
}
`
	dir := t.TempDir()
	path := filepath.Join(dir, "src.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	pkgs := []Package{{Dir: dir, ImportPath: "example.com/test", GoFiles: []string{"src.go"}}}
	reg := mutator.NewRegistry()
	fset := token.NewFileSet()
	res := Discover(fset, pkgs, reg.EnabledMutators([]string{"ARITHMETIC_BASE"}, nil), dir, "example.com/test")

	// Drop the file: any read attempt downstream is now a hard error.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	_, suppressed, err := FilterByDirectivesWithCache(fset, res.Mutants, res.Files)
	if err != nil {
		t.Fatalf("cache path read disk: %v", err)
	}
	if len(suppressed) == 0 {
		t.Fatalf("expected directive to suppress the arithmetic mutant via the cache")
	}
}

// TestFilterByDirectivesWithCachePartialCacheFallsBack verifies that
// files missing from the supplied cache fall back to the read+parse
// path: with one file's entry deleted from the cache map (but the file
// still on disk), its directive must still suppress.
func TestFilterByDirectivesWithCachePartialCacheFallsBack(t *testing.T) {
	srcA := `package p

func A(a, b int) int {
	// gomutants:disable-next-line ARITHMETIC_BASE
	return a + b
}
`
	srcB := `package p

func B(a, b int) int {
	// gomutants:disable-next-line ARITHMETIC_BASE
	return a + b
}
`
	dir := t.TempDir()
	pathA := filepath.Join(dir, "a.go")
	pathB := filepath.Join(dir, "b.go")
	if err := os.WriteFile(pathA, []byte(srcA), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pathB, []byte(srcB), 0o644); err != nil {
		t.Fatal(err)
	}
	pkgs := []Package{{Dir: dir, ImportPath: "example.com/test", GoFiles: []string{"a.go", "b.go"}}}
	reg := mutator.NewRegistry()
	fset := token.NewFileSet()
	res := Discover(fset, pkgs, reg.EnabledMutators([]string{"ARITHMETIC_BASE"}, nil), dir, "example.com/test")

	// Drop a.go from the cache; it stays on disk so the fallback can read it.
	delete(res.Files, pathA)

	_, suppressed, err := FilterByDirectivesWithCache(fset, res.Mutants, res.Files)
	if err != nil {
		t.Fatalf("FilterByDirectivesWithCache: %v", err)
	}
	suppressedFiles := make(map[string]int)
	for _, s := range suppressed {
		suppressedFiles[s.Mutant.File]++
	}
	if suppressedFiles[pathA] == 0 {
		t.Errorf("a.go directive must suppress via the read+parse fallback; suppressed=%v", suppressedFiles)
	}
	if suppressedFiles[pathB] == 0 {
		t.Errorf("b.go directive must suppress via the cache; suppressed=%v", suppressedFiles)
	}
}

// TestFilterByDirectivesMalformedGoFile drives the parser-error fallback
// in indexFor: a file with the `gomutants:` prefix passes the byte-level
// gate but fails parser.ParseFile, which must yield an empty index — no
// error, no suppression — rather than aborting the run.
func TestFilterByDirectivesMalformedGoFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.go")
	src := "package p\n// gomutants:disable ARITHMETIC_BASE\nfunc F() { @ }\n"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	m := mutator.Mutant{
		File:    path,
		RelFile: "broken.go",
		Line:    3,
		Type:    mutator.ArithmeticBase,
	}
	fset := token.NewFileSet()
	kept, suppressed, err := filterByDirectives(fset, []mutator.Mutant{m}, nil, io.Discard)
	if err != nil {
		t.Fatalf("filterByDirectives: %v", err)
	}
	if len(suppressed) != 0 {
		t.Errorf("malformed file must yield empty index, got %d suppressions", len(suppressed))
	}
	if len(kept) != 1 {
		t.Errorf("mutant should survive an empty index, got %d kept", len(kept))
	}
}

// TestFilterByDirectivesWithCacheNoPrefixFastPath drives the no-prefix
// fast-path in buildFileIndex: a cached ParsedFile whose source has no
// `gomutants:` prefix must short-circuit to an empty index, leaving the
// mutant un-suppressed without scanning comments.
func TestFilterByDirectivesWithCacheNoPrefixFastPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clean.go")
	src := []byte("package p\n\nfunc F(a, b int) int { return a + b }\n")
	if err := os.WriteFile(path, src, 0o644); err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}
	cache := map[string]*ParsedFile{path: {Src: src, File: file}}
	m := mutator.Mutant{
		File:    path,
		RelFile: "clean.go",
		Line:    3,
		Type:    mutator.ArithmeticBase,
	}
	kept, suppressed, err := FilterByDirectivesWithCache(fset, []mutator.Mutant{m}, cache)
	if err != nil {
		t.Fatalf("FilterByDirectivesWithCache: %v", err)
	}
	if len(suppressed) != 0 {
		t.Errorf("no-prefix source must yield no suppressions, got %d", len(suppressed))
	}
	if len(kept) != 1 {
		t.Errorf("mutant should survive, got %d kept", len(kept))
	}
}

// TestFilterByDirectivesIndexDedupedPerFile kills the BRANCH_IF on the
// `if _, seen := indexes[m.File]; seen { continue }` dedup: dropping the
// continue rebuilds the file index once per mutant instead of once per
// file, so any per-file parse warning is re-emitted N times. The fixture
// carries a malformed directive (one warning) plus several mutants in
// the same file; the warning must appear exactly once.
func TestFilterByDirectivesIndexDedupedPerFile(t *testing.T) {
	src := `package p

// gomutants:disable-regexp
func F(a, b int) int {
	x := a + b
	return x * b
}
`
	mutants, _ := writeFixture(t, src)
	if len(mutants) < 2 {
		t.Fatalf("fixture must yield >=2 mutants in one file to exercise the dedup, got %d", len(mutants))
	}
	fset := token.NewFileSet()
	var warn bytes.Buffer
	if _, _, err := filterByDirectives(fset, mutants, nil, &warn); err != nil {
		t.Fatalf("filterByDirectives: %v", err)
	}
	if got := strings.Count(warn.String(), "disable-regexp missing pattern"); got != 1 {
		t.Errorf("per-file warning must be emitted once, got %d:\n%s", got, warn.String())
	}
}

// TestFilterByDirectivesMalformedGoFileSameLineDirective kills the
// BRANCH_IF on the `if err != nil { return &fileIndex{}, nil }` parser-
// error fallback in indexFor. parser.ParseFile returns a *partial* AST on
// syntax error; without the early return, buildFileIndex scans that
// partial AST and a directive that survived parsing would suppress a
// mutant on its line. The fixture puts a valid same-line directive on
// line 2 and the syntax error on line 3, so line 2's directive is
// retained — it must NOT take effect, because the error path should
// short-circuit to an empty index.
func TestFilterByDirectivesMalformedGoFileSameLineDirective(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.go")
	src := "package p\n" +
		"func F(a, b int) int { return a + b } // gomutants:disable ARITHMETIC_BASE\n" +
		"func G() { @ }\n"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	m := mutator.Mutant{
		File:    path,
		RelFile: "broken.go",
		Line:    2,
		Type:    mutator.ArithmeticBase,
	}
	fset := token.NewFileSet()
	kept, suppressed, err := filterByDirectives(fset, []mutator.Mutant{m}, nil, io.Discard)
	if err != nil {
		t.Fatalf("filterByDirectives: %v", err)
	}
	if len(suppressed) != 0 {
		t.Errorf("malformed file must short-circuit to an empty index; got %d suppressions", len(suppressed))
	}
	if len(kept) != 1 {
		t.Errorf("mutant should survive an empty index, got %d kept", len(kept))
	}
}
