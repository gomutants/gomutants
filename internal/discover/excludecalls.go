package discover

import (
	"fmt"
	"go/ast"
	"go/token"
	"regexp"
	"strings"

	"github.com/gomutants/gomutants/internal/mutator"
)

// unknownReceiver stands in for a call receiver that isn't a plain
// identifier chain — `getLogger().Info(...)` renders as `_.Info`. It is
// deliberately dot-free so a receiver glob (`*.Info`) still matches while
// a package glob (`log.*`) does not: we know the method name, but not
// what it was called on.
const unknownReceiver = "_"

// CallExcluder matches rendered call selectors (`log.Printf`,
// `logger.Debug`, `s.log.Errorf`) against glob patterns, where `*` stands
// for any run of characters. A nil *CallExcluder matches nothing, so
// callers can treat "no patterns configured" and "all patterns missed"
// the same way without a nil check at every call site.
type CallExcluder struct {
	patterns []*regexp.Regexp
}

// NewCallExcluder compiles each non-blank spec into an anchored regexp.
// Blank specs are skipped (splitAndTrim already drops them on the CLI
// path, but YAML lists can carry blanks). When no usable patterns remain
// it returns (nil, nil) so the caller gets a no-op excluder rather than
// an empty one.
//
// A spec of only asterisks is rejected: it matches every call in the
// module, which would suppress nearly every mutant. That is never what
// someone means, and as a silent config typo it would zero out a suite's
// mutation score without any other symptom.
func NewCallExcluder(specs []string) (*CallExcluder, error) {
	patterns := make([]*regexp.Regexp, 0, len(specs))
	for _, s := range specs {
		// Unlike file-path regexps, a call selector never contains
		// whitespace, so surrounding space is always accidental and safe
		// to trim rather than skip.
		spec := strings.TrimSpace(s)
		if spec == "" {
			continue
		}
		if strings.Trim(spec, "*") == "" {
			return nil, fmt.Errorf("pattern %q matches every call; list the calls to exclude instead", s)
		}
		// MustCompile is safe: globToRegexp emits QuoteMeta'd literals,
		// `.*`, and the two anchors, so the result always compiles. A
		// returned-and-checked error here would be an untestable branch.
		patterns = append(patterns, regexp.MustCompile(globToRegexp(spec)))
	}
	if len(patterns) == 0 {
		return nil, nil
	}
	return &CallExcluder{patterns: patterns}, nil
}

// Match reports whether selector matches any configured pattern. Patterns
// are anchored: `log.Print` matches only `log.Print`, not `log.Println`
// — use `log.Print*` for that. A nil receiver never matches.
func (c *CallExcluder) Match(selector string) bool {
	if c == nil {
		return false
	}
	for _, re := range c.patterns {
		if re.MatchString(selector) {
			return true
		}
	}
	return false
}

// globToRegexp translates a selector glob into an anchored regexp source.
// Every character except `*` is literal — dots especially, which is the
// reason for a glob syntax here rather than the raw regexps --exclude-files
// takes: `log.*` should read as "the log package", not "log" followed by
// anything.
func globToRegexp(glob string) string {
	var b strings.Builder
	b.WriteString("^")
	for i, part := range strings.Split(glob, "*") {
		if i > 0 {
			b.WriteString(".*")
		}
		b.WriteString(regexp.QuoteMeta(part))
	}
	b.WriteString("$")
	return b.String()
}

// FilterByCalls drops mutants that fall inside a call whose selector
// matches the excluder — the Go analogue of PITest's avoidCallsTo, aimed
// at operators inside logging and telemetry arguments that no test can
// reasonably assert on. Returns the surviving mutants and the
// suppressions for reporting.
//
// The suppressed span is the whole call expression, not just its argument
// list, so STATEMENT_REMOVE of a bare `log.Printf(...)` statement is
// dropped alongside the arithmetic inside it — both are unkillable for
// the same reason. That is also why the built-in default set excludes
// log.Fatal/log.Panic: deleting one of those is a real behavioural change
// a test can catch.
//
// files is the parse cache from Discover; every mutant's file is present
// in it, since a file that failed to parse produced no mutants. A missing
// entry is therefore treated as "no excluded calls" rather than re-read
// from disk.
func FilterByCalls(fset *token.FileSet, mutants []mutator.Mutant, files map[string]*ParsedFile, ce *CallExcluder) ([]mutator.Mutant, []Suppression) {
	// gomutants:disable-next-line BRANCH_IF reason="fast-path optimisation; identical observable when removed (a nil excluder matches nothing, so the slow path also suppresses nothing)"
	if ce == nil {
		return mutants, nil
	}

	indexes := make(map[string]*callIndex)
	kept := make([]mutator.Mutant, 0, len(mutants))
	var suppressed []Suppression
	for _, m := range mutants {
		idx, seen := indexes[m.File]
		if !seen {
			idx = buildCallIndex(fset, files[m.File], ce)
			// gomutants:disable-next-line STATEMENT_REMOVE reason="memoisation; without it the index is rebuilt per mutant, which is observably identical and only slower"
			indexes[m.File] = idx
		}
		if selector, hit := idx.match(m); hit {
			suppressed = append(suppressed, Suppression{Mutant: m, Reason: "exclude-calls: " + selector})
			continue
		}
		kept = append(kept, m)
	}
	return kept, suppressed
}

// callSpan is the half-open byte range [start, end) of one matched call,
// tagged with the selector that matched so the suppression can name it.
type callSpan struct {
	start    int
	end      int
	selector string
}

// callIndex holds a file's matched call spans. They are disjoint
// (buildCallIndex stops descending at a match), so at most one can contain
// a given mutant and the scan order doesn't matter.
type callIndex struct {
	spans []callSpan
}

// match reports whether the mutant's byte range sits entirely inside a
// matched call, returning the selector that matched. Linear, like the
// func-scope and regexp scans in directives.go: a file has a handful of
// matching calls, and the containment test is two comparisons.
func (ci *callIndex) match(m mutator.Mutant) (string, bool) {
	for _, s := range ci.spans {
		// Both bounds are inclusive of the call's own extent, which is what
		// makes STATEMENT_REMOVE of a bare `log.Printf(...)` statement —
		// whose span is exactly the call — match.
		if m.StartOffset >= s.start && m.EndOffset <= s.end {
			return s.selector, true
		}
	}
	return "", false
}

// buildCallIndex collects the spans of every matching call in one file.
// A matched call's subtree is not descended into: everything nested in it
// is already covered by its span, which is what keeps the resulting spans
// disjoint and makes the outermost matching call the one named in the
// suppression reason.
func buildCallIndex(fset *token.FileSet, pf *ParsedFile, ce *CallExcluder) *callIndex {
	idx := &callIndex{}
	if pf == nil {
		return idx
	}
	ast.Inspect(pf.File, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector := renderCallSelector(call.Fun)
		// The empty-selector guard is defence in depth: NewCallExcluder
		// rejects the all-asterisk specs that are the only patterns able to
		// match "", so today it can't fire. It stays so that loosening that
		// rejection can't silently make every func-literal call an excluded
		// one.
		// gomutants:disable-next-line EXPRESSION_REMOVE reason="unreachable given NewCallExcluder's all-asterisk rejection; kept as defence in depth, so no test can distinguish it"
		if selector == "" || !ce.Match(selector) {
			return true
		}
		idx.spans = append(idx.spans, callSpan{
			start:    fset.Position(call.Pos()).Offset,
			end:      fset.Position(call.End()).Offset,
			selector: selector,
		})
		return false
	})
	return idx
}

// renderCallSelector renders a call's function expression as a dotted
// selector: `printf`, `log.Printf`, `s.log.Errorf`, `_.Info`. Matching is
// purely syntactic — no go/types — so an aliased import (`import stdlog
// "log"`) renders under the alias and needs its own pattern. Returns ""
// for calls with no nameable selector (an immediately-invoked func
// literal, say), which never match.
func renderCallSelector(fun ast.Expr) string {
	switch e := ast.Unparen(fun).(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return renderReceiver(e.X) + "." + e.Sel.Name
	case *ast.IndexExpr:
		// Generic instantiation: `f[int](x)`, `x.Method[int](y)`.
		return renderCallSelector(e.X)
	case *ast.IndexListExpr:
		// Multi-parameter instantiation: `f[int, string](x)`.
		return renderCallSelector(e.X)
	}
	return ""
}

// renderReceiver renders the expression a method is called on. Identifier
// chains render in full (`s.log`); anything else — an index, a call
// result, a type assertion — collapses to unknownReceiver, since its
// runtime identity isn't knowable from syntax alone.
func renderReceiver(x ast.Expr) string {
	switch e := ast.Unparen(x).(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return renderReceiver(e.X) + "." + e.Sel.Name
	}
	return unknownReceiver
}
