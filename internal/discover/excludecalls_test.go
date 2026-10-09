package discover

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gomutants/gomutants/internal/mutator"
)

// discoverFixture writes src as the only file of a temp package and runs
// Discover over it, returning the full result (mutants plus the parse
// cache FilterByCalls consumes) and the FileSet they share.
func discoverFixture(t *testing.T, src string) (*Result, *token.FileSet) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "src.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	pkgs := []Package{{Dir: dir, ImportPath: "example.com/test", GoFiles: []string{"src.go"}}}
	fset := token.NewFileSet()
	return Discover(fset, pkgs, mutator.NewRegistry().Mutators(), dir, "example.com/test"), fset
}

// mustExcluder builds a CallExcluder, failing the test on a rejected spec.
func mustExcluder(t *testing.T, specs ...string) *CallExcluder {
	t.Helper()
	ce, err := NewCallExcluder(specs)
	if err != nil {
		t.Fatalf("NewCallExcluder(%q): %v", specs, err)
	}
	if ce == nil {
		t.Fatalf("NewCallExcluder(%q): want non-nil excluder", specs)
	}
	return ce
}

// linesOf counts mutants per source line.
func linesOf(mutants []mutator.Mutant) map[int]int {
	out := make(map[int]int)
	for _, m := range mutants {
		out[m.Line]++
	}
	return out
}

// suppressedLines counts suppressions per source line.
func suppressedLines(suppressed []Suppression) map[int]int {
	out := make(map[int]int)
	for _, s := range suppressed {
		out[s.Mutant.Line]++
	}
	return out
}

func TestNewCallExcluderEmptyReturnsNil(t *testing.T) {
	// The CLI path trims blanks before this point, but a YAML list can
	// carry empty or whitespace-only entries; they must not produce a
	// live pattern (`^$`, which would match the empty selector).
	for _, specs := range [][]string{nil, {}, {"", "  ", "\t"}} {
		ce, err := NewCallExcluder(specs)
		if err != nil {
			t.Fatalf("specs %q: unexpected error %v", specs, err)
		}
		if ce != nil {
			t.Errorf("specs %q: want nil CallExcluder, got %+v", specs, ce)
		}
	}
}

func TestNewCallExcluderRejectsMatchAllPattern(t *testing.T) {
	// A bare "*" would suppress every call in the module — almost every
	// mutant — with no other symptom. Reject rather than obey.
	for _, spec := range []string{"*", "**", " * "} {
		ce, err := NewCallExcluder([]string{"log.Print*", spec})
		if err == nil {
			t.Errorf("spec %q: want error, got nil", spec)
		}
		if ce != nil {
			t.Errorf("spec %q: want nil CallExcluder on error, got %+v", spec, ce)
		}
	}
}

func TestNewCallExcluderSkipsBlanksKeepsLater(t *testing.T) {
	// A blank entry must be skipped (continue), not stop the loop (break):
	// the valid pattern after it has to survive.
	ce := mustExcluder(t, "", "log.Print*")
	if len(ce.patterns) != 1 {
		t.Fatalf("want 1 pattern after a leading blank, got %d", len(ce.patterns))
	}
	if !ce.Match("log.Printf") {
		t.Error("pattern after blank must still match")
	}
}

func TestNewCallExcluderTrimsSurroundingSpace(t *testing.T) {
	// A selector never contains whitespace, so space around a YAML entry
	// is always accidental — trimmed, not treated as part of the pattern.
	ce := mustExcluder(t, "  log.Printf\t")
	if !ce.Match("log.Printf") {
		t.Error("want padded spec to match after trimming")
	}
}

func TestCallExcluderMatch(t *testing.T) {
	tests := []struct {
		name     string
		patterns []string
		selector string
		want     bool
	}{
		{"prefix glob hits bare name", []string{"log.Print*"}, "log.Print", true},
		{"prefix glob hits suffixed name", []string{"log.Print*"}, "log.Printf", true},
		{"prefix glob hits Println", []string{"log.Print*"}, "log.Println", true},
		{"prefix glob misses sibling func", []string{"log.Print*"}, "log.Fatalf", false},
		// Anchoring is the whole point: a pattern for the log package must
		// not match a project package whose name ends in "log".
		{"anchored at start", []string{"log.Print*"}, "mylog.Printf", false},
		{"anchored at end", []string{"log.Print"}, "log.Printf", false},
		{"package glob", []string{"slog.*"}, "slog.InfoContext", true},
		{"package glob misses bare package", []string{"slog.*"}, "slog", false},
		{"receiver glob", []string{"*.Debug"}, "logger.Debug", true},
		{"receiver glob spans dotted receiver", []string{"*.Debug"}, "s.log.Debug", true},
		{"receiver glob hits unknown receiver", []string{"*.Debug"}, "_.Debug", true},
		{"receiver glob is anchored too", []string{"*.Debug"}, "logger.Debugf", false},
		{"dot is literal, not any-char", []string{"log.Print"}, "logXPrint", false},
		{"bare identifier", []string{"println"}, "println", true},
		{"second pattern matches", []string{"log.*", "slog.*"}, "slog.Info", true},
		{"no pattern matches", []string{"log.*", "slog.*"}, "fmt.Sprintf", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ce := mustExcluder(t, tt.patterns...)
			if got := ce.Match(tt.selector); got != tt.want {
				t.Errorf("patterns %q Match(%q) = %v, want %v", tt.patterns, tt.selector, got, tt.want)
			}
		})
	}
}

func TestCallExcluderNilMatchesNothing(t *testing.T) {
	var ce *CallExcluder
	if ce.Match("log.Printf") {
		t.Error("nil CallExcluder must never match")
	}
}

func TestRenderCallSelector(t *testing.T) {
	tests := []struct {
		expr string
		want string
	}{
		{"printf(x)", "printf"},
		{"log.Printf(x)", "log.Printf"},
		{"s.log.Errorf(x)", "s.log.Errorf"},
		// Call results and index expressions have no syntactic identity,
		// so only the method name survives.
		{"getLogger().Info(x)", "_.Info"},
		{"loggers[i].Info(x)", "_.Info"},
		{"Config{}.Info(x)", "_.Info"},
		{"(log.Printf)(x)", "log.Printf"},
		{"(s).log.Info(x)", "s.log.Info"},
		{"f[int](x)", "f"},
		{"f[int, string](x)", "f"},
		{"obj.Method[int](x)", "obj.Method"},
		// No nameable selector at all: never matches any pattern.
		{"func() {}()", ""},
	}
	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			e, err := parser.ParseExpr(tt.expr)
			if err != nil {
				t.Fatalf("parsing %q: %v", tt.expr, err)
			}
			call, ok := e.(*ast.CallExpr)
			if !ok {
				t.Fatalf("%q parsed as %T, want *ast.CallExpr", tt.expr, e)
			}
			if got := renderCallSelector(call.Fun); got != tt.want {
				t.Errorf("renderCallSelector(%q) = %q, want %q", tt.expr, got, tt.want)
			}
		})
	}
}

// The issue's motivating case: arithmetic inside a progress-logging call.
const progressLogSrc = `package p

import "log"

func report(done, total int) int {
	log.Printf("imported %d/%d rows (%.1f%%)", done, total, float64(done)/float64(total)*100)
	rate := done * 100 / total
	return rate
}
`

func TestFilterByCallsSuppressesInsideLoggingCall(t *testing.T) {
	res, fset := discoverFixture(t, progressLogSrc)
	kept, suppressed := FilterByCalls(fset, res.Mutants, res.Files, mustExcluder(t, "log.Print*"))

	const logLine, rateLine = 6, 7
	if suppressedLines(suppressed)[logLine] == 0 {
		t.Fatalf("want mutants suppressed on the log line, got none (of %d suppressed)", len(suppressed))
	}
	if got := linesOf(kept)[logLine]; got != 0 {
		t.Errorf("want no surviving mutants on the log line, got %d", got)
	}
	// The neighbouring arithmetic must be untouched — the filter is scoped
	// to the call, not to logging-adjacent code.
	if linesOf(kept)[rateLine] == 0 {
		t.Errorf("want surviving mutants on the non-log line, got none")
	}
	if len(kept)+len(suppressed) != len(res.Mutants) {
		t.Errorf("kept %d + suppressed %d != %d discovered", len(kept), len(suppressed), len(res.Mutants))
	}
}

func TestFilterByCallsSuppressesStatementRemoveOfLogCall(t *testing.T) {
	// STATEMENT_REMOVE of a bare `log.Printf(...)` statement spans exactly
	// the call expression. Suppressing it is the reason the excluded span
	// is the whole call rather than just the argument list: deleting a log
	// line is every bit as unkillable as mutating what it prints.
	res, fset := discoverFixture(t, progressLogSrc)
	_, suppressed := FilterByCalls(fset, res.Mutants, res.Files, mustExcluder(t, "log.Print*"))

	found := false
	for _, s := range suppressed {
		if s.Mutant.Type == mutator.StatementRemove {
			found = true
		}
	}
	if !found {
		t.Errorf("want STATEMENT_REMOVE of the log statement suppressed, got %v", suppressedTypes(suppressed))
	}
}

func TestFilterByCallsIsOffsetScopedNotLineScoped(t *testing.T) {
	// Two statements on one physical line: only the one inside the call is
	// suppressed. A line-based filter would drop both.
	src := `package p

import "log"

func f(a, b int) int {
	log.Println(a / b); c := a % b
	return c
}
`
	res, fset := discoverFixture(t, src)
	kept, suppressed := FilterByCalls(fset, res.Mutants, res.Files, mustExcluder(t, "log.Print*"))

	if got := suppressedTypes(suppressed)[mutator.ArithmeticBase]; got != 1 {
		t.Errorf("want the `/` inside the call suppressed (1 ARITHMETIC_BASE), got %d", got)
	}
	if got := keptTypes(kept)[mutator.ArithmeticBase]; got != 1 {
		t.Errorf("want the `%%` outside the call kept (1 ARITHMETIC_BASE), got %d", got)
	}
}

func TestFilterByCallsKeepsMutantsBeforeTheCall(t *testing.T) {
	// Containment needs both bounds: code preceding a matched call ends
	// before the call does, so an end-only test would swallow everything
	// above the first logging line in the file.
	src := `package p

import "log"

func f(a, b int) int {
	c := a / b
	log.Println(c)
	return c
}
`
	res, fset := discoverFixture(t, src)
	kept, suppressed := FilterByCalls(fset, res.Mutants, res.Files, mustExcluder(t, "log.Print*"))

	if linesOf(kept)[6] == 0 {
		t.Error("want the arithmetic above the log call kept, got none")
	}
	if got := suppressedLines(suppressed)[6]; got != 0 {
		t.Errorf("want nothing suppressed above the log call, got %d", got)
	}
	if suppressedLines(suppressed)[7] == 0 {
		t.Error("want the log statement itself suppressed")
	}
}

func TestFilterByCallsCoversNestedCalls(t *testing.T) {
	// A mutant nested inside another call in the argument list is still
	// inside the logging call's span, and is attributed to the outermost
	// matching call rather than the inner one.
	src := `package p

import "log"

func scale(v int) int { return v }

func f(a, b int) {
	log.Printf("%d", scale(a/b))
}
`
	res, fset := discoverFixture(t, src)
	_, suppressed := FilterByCalls(fset, res.Mutants, res.Files, mustExcluder(t, "log.Print*", "scale"))

	var arith []Suppression
	for _, s := range suppressed {
		if s.Mutant.Type == mutator.ArithmeticBase {
			arith = append(arith, s)
		}
	}
	if len(arith) != 1 {
		t.Fatalf("want the nested `/` suppressed once, got %d", len(arith))
	}
	if want := "exclude-calls: log.Printf"; arith[0].Reason != want {
		t.Errorf("Reason = %q, want %q (outermost matching call)", arith[0].Reason, want)
	}
}

func TestFilterByCallsMatchesEachOfSeveralCalls(t *testing.T) {
	// Several matching calls in one file: the span scan has to find the
	// right one, not just the first.
	src := `package p

import "log"

func f(a, b int) {
	log.Println(a + b)
	log.Println(a - b)
	log.Println(a * b)
}
`
	res, fset := discoverFixture(t, src)
	kept, suppressed := FilterByCalls(fset, res.Mutants, res.Files, mustExcluder(t, "log.Print*"))

	if len(kept) != 0 {
		t.Errorf("want every mutant suppressed, got %d kept: %v", len(kept), keptTypes(kept))
	}
	for line := 6; line <= 8; line++ {
		if suppressedLines(suppressed)[line] == 0 {
			t.Errorf("want suppressions on line %d, got none", line)
		}
	}
}

func TestFilterByCallsMethodAndUnknownReceiver(t *testing.T) {
	// `*.Debug` reaches both a plain variable receiver and a call-result
	// receiver, which renders as the dot-free `_` placeholder.
	src := `package p

type logger struct{}

func (l logger) Debug(v ...any) {}

func get() logger { return logger{} }

func f(a, b int) {
	l := logger{}
	l.Debug(a / b)
	get().Debug(a - b)
	other(a * b)
}

func other(v int) {}
`
	res, fset := discoverFixture(t, src)
	kept, suppressed := FilterByCalls(fset, res.Mutants, res.Files, mustExcluder(t, "*.Debug"))

	if suppressedLines(suppressed)[11] == 0 {
		t.Error("want the variable-receiver Debug call suppressed")
	}
	if suppressedLines(suppressed)[12] == 0 {
		t.Error("want the call-result-receiver Debug call suppressed")
	}
	if linesOf(kept)[13] == 0 {
		t.Error("want the non-matching call's arithmetic kept")
	}
}

func TestFilterByCallsNilExcluderIsNoOp(t *testing.T) {
	res, fset := discoverFixture(t, progressLogSrc)
	kept, suppressed := FilterByCalls(fset, res.Mutants, res.Files, nil)
	if len(kept) != len(res.Mutants) {
		t.Errorf("kept %d mutants, want all %d", len(kept), len(res.Mutants))
	}
	if suppressed != nil {
		t.Errorf("want nil suppressions, got %v", suppressed)
	}
}

func TestFilterByCallsNoMutantsIsNoOp(t *testing.T) {
	kept, suppressed := FilterByCalls(token.NewFileSet(), nil, nil, mustExcluder(t, "log.*"))
	if len(kept) != 0 || suppressed != nil {
		t.Errorf("want (empty, nil), got (%v, %v)", kept, suppressed)
	}
}

func TestFilterByCallsMissingParseCacheEntry(t *testing.T) {
	// Can't happen via Discover (an unparseable file yields no mutants),
	// but the lookup must degrade to "no excluded calls" rather than panic.
	res, fset := discoverFixture(t, progressLogSrc)
	kept, suppressed := FilterByCalls(fset, res.Mutants, map[string]*ParsedFile{}, mustExcluder(t, "log.Print*"))
	if len(kept) != len(res.Mutants) {
		t.Errorf("kept %d mutants, want all %d", len(kept), len(res.Mutants))
	}
	if len(suppressed) != 0 {
		t.Errorf("want no suppressions without a parse cache, got %d", len(suppressed))
	}
}

func TestFilterByCallsUnmatchedFileKeepsEverything(t *testing.T) {
	res, fset := discoverFixture(t, progressLogSrc)
	kept, suppressed := FilterByCalls(fset, res.Mutants, res.Files, mustExcluder(t, "zap.*"))
	if len(kept) != len(res.Mutants) {
		t.Errorf("kept %d mutants, want all %d", len(kept), len(res.Mutants))
	}
	if len(suppressed) != 0 {
		t.Errorf("want no suppressions when no call matches, got %d", len(suppressed))
	}
}

func TestFilterByCallsReasonNamesSelector(t *testing.T) {
	res, fset := discoverFixture(t, progressLogSrc)
	_, suppressed := FilterByCalls(fset, res.Mutants, res.Files, mustExcluder(t, "log.Print*"))
	if len(suppressed) == 0 {
		t.Fatal("want suppressions")
	}
	for _, s := range suppressed {
		if !strings.HasPrefix(s.Reason, "exclude-calls: log.Printf") {
			t.Errorf("Reason = %q, want it to name the matched call", s.Reason)
		}
	}
}

func TestGlobToRegexp(t *testing.T) {
	tests := []struct{ glob, want string }{
		{"log.Printf", `^log\.Printf$`},
		{"log.*", `^log\..*$`},
		{"*.Debug", `^.*\.Debug$`},
		{"*.Log*", `^.*\.Log.*$`},
	}
	for _, tt := range tests {
		if got := globToRegexp(tt.glob); got != tt.want {
			t.Errorf("globToRegexp(%q) = %q, want %q", tt.glob, got, tt.want)
		}
	}
}
