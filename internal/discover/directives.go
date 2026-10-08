package discover

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/szhekpisov/gomutants/internal/mutator"
)

const directivePrefix = "gomutants:"

var directivePrefixBytes = []byte(directivePrefix)

type directiveKind int

// directiveSameLine MUST stay at iota=0. The self-suppression on the
// `case "disable":` arm of parseDirective relies on the var's zero-value
// matching this constant — reorder these and the suppression silently
// masks a real bug.
const (
	directiveSameLine directiveKind = iota
	directiveNextLine
	directiveFunc
	directiveRegexp
)

// directive is a parsed `// gomutants:disable*` source annotation.
// Mutators nil means "all mutators". FuncStart/FuncEnd are populated
// only for directiveFunc; Regexp only for directiveRegexp.
type directive struct {
	Kind      directiveKind
	Mutators  map[mutator.MutationType]struct{}
	Reason    string
	FuncStart int
	FuncEnd   int
	Regexp    *regexp.Regexp
}

type Suppression struct {
	Mutant mutator.Mutant
	Reason string
}

func warnf(w io.Writer, relPath string, line int, format string, args ...any) {
	fmt.Fprintf(w, "gomutants: %s:%d: "+format+"\n", append([]any{relPath, line}, args...)...)
}

// FilterByDirectives drops mutants matched by any `// gomutants:disable*`
// directive in their source file. Returns the surviving mutants and the
// suppressions for reporting. Malformed directives and unknown mutator
// names are written to os.Stderr and the offending directive (or name)
// is dropped.
//
// Not idempotent: warnings are emitted on each call. Call once per run,
// after all other discovery filters have run.
//
// This entry point reads and parses each file itself. Prefer
// FilterByDirectivesWithCache when you already have the parse cache from
// Discover — it skips the duplicate read+parse.
func FilterByDirectives(fset *token.FileSet, mutants []mutator.Mutant) ([]mutator.Mutant, []Suppression, error) {
	return filterByDirectives(fset, mutants, nil, os.Stderr)
}

// FilterByDirectivesWithCache is the cache-aware variant: it reuses the
// (src, *ast.File) entries from Discover.Files instead of reading and
// parsing each file a second time. Files entries missing from the cache
// fall back to the read+parse path so this also tolerates partial caches.
func FilterByDirectivesWithCache(fset *token.FileSet, mutants []mutator.Mutant, files map[string]*ParsedFile) ([]mutator.Mutant, []Suppression, error) {
	return filterByDirectives(fset, mutants, files, os.Stderr)
}

// FilterByDirectivesTo is FilterByDirectivesWithCache with its warnings
// written to warn instead of os.Stderr, for a pre-check whose files a
// later FilterByDirectivesWithCache call indexes again: it holds them back
// and prints them only when that later call will not run, so a malformed
// directive is reported once either way.
func FilterByDirectivesTo(fset *token.FileSet, mutants []mutator.Mutant, files map[string]*ParsedFile, warn io.Writer) ([]mutator.Mutant, []Suppression, error) {
	return filterByDirectives(fset, mutants, files, warn)
}

func filterByDirectives(fset *token.FileSet, mutants []mutator.Mutant, files map[string]*ParsedFile, warn io.Writer) ([]mutator.Mutant, []Suppression, error) {
	if len(mutants) == 0 {
		return mutants, nil, nil
	}

	indexes := make(map[string]*fileIndex)
	for _, m := range mutants {
		if _, seen := indexes[m.File]; seen {
			continue
		}
		idx, err := indexFor(fset, m.File, m.RelFile, files, warn)
		if err != nil {
			return nil, nil, err
		}
		indexes[m.File] = idx
	}

	kept := make([]mutator.Mutant, 0, len(mutants))
	var suppressed []Suppression
	for _, m := range mutants {
		idx := indexes[m.File]
		if d, ok := matchMutant(idx, m); ok {
			suppressed = append(suppressed, Suppression{Mutant: m, Reason: d.Reason})
			continue
		}
		kept = append(kept, m)
	}
	return kept, suppressed, nil
}

// fileIndex organises directives so each mutant lookup is O(1) for
// line-scoped directives and O(F+R) for func-scope and regexp.
type fileIndex struct {
	sameLine map[int][]directive
	nextLine map[int][]directive
	funcs    []directive
	regexps  []directive
	lines    []string
}

// indexFor returns the per-file directive index, preferring the parse
// cache. Cache misses fall back to reading and parsing the file from disk.
func indexFor(fset *token.FileSet, path, relPath string, files map[string]*ParsedFile, warn io.Writer) (*fileIndex, error) {
	if pf, ok := files[path]; ok {
		return buildFileIndex(fset, pf.Src, pf.File, relPath, warn), nil
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	// gomutants:disable-next-line BRANCH_IF reason="fast-path optimisation; identical observable when removed (the slow path also yields an empty index for files without the prefix)"
	if !bytes.Contains(src, directivePrefixBytes) {
		return &fileIndex{}, nil
	}
	// ParseComments is required so funcDecl.Doc is populated and we can
	// distinguish a comment that sits on a function declaration from one
	// that floats free in the file.
	file, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return &fileIndex{}, nil
	}
	return buildFileIndex(fset, src, file, relPath, warn), nil
}

// buildFileIndex builds the per-file directive index from already-parsed
// source. Both FilterByDirectives (after a fresh read+parse) and
// FilterByDirectivesWithCache (via the Discover cache) funnel through here.
func buildFileIndex(fset *token.FileSet, src []byte, file *ast.File, relPath string, warn io.Writer) *fileIndex {
	// This guard is for the cache-path callers — indexFor's no-cache
	// path already gates the parse on the same check, so it never
	// reaches this point with a no-prefix file.
	// gomutants:disable-next-line BRANCH_IF reason="fast-path optimisation; identical observable when removed (the slow path also yields an empty index for files without the prefix)"
	if !bytes.Contains(src, directivePrefixBytes) {
		return &fileIndex{}
	}

	funcRangeByDocPos := make(map[token.Pos][2]int)
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Doc == nil || fd.Body == nil {
			continue
		}
		startLine := fset.Position(fd.Body.Lbrace).Line
		endLine := fset.Position(fd.Body.Rbrace).Line
		funcRangeByDocPos[fd.Doc.Pos()] = [2]int{startLine, endLine}
	}

	idx := &fileIndex{
		sameLine: make(map[int][]directive),
		nextLine: make(map[int][]directive),
	}

	type pendingNextLine struct {
		d          directive
		sourceLine int
	}
	var pending []pendingNextLine

	for _, cg := range file.Comments {
		funcRange, isFuncDoc := funcRangeByDocPos[cg.Pos()]
		for _, c := range cg.List {
			// Block comments (`/* ... */`) reach this point with their
			// delimiters intact, so the directive-prefix check below
			// rejects them — no separate `//` guard needed.
			text := strings.TrimSpace(strings.TrimPrefix(c.Text, "//"))
			if !strings.HasPrefix(text, directivePrefix) {
				continue
			}
			line := fset.Position(c.Pos()).Line
			d, parsedOK := parseDirective(text, line, relPath, warn)
			if !parsedOK {
				continue
			}
			switch d.Kind {
			case directiveSameLine:
				idx.sameLine[line] = append(idx.sameLine[line], d)
			case directiveNextLine:
				pending = append(pending, pendingNextLine{d, line})
			case directiveFunc:
				if !isFuncDoc {
					warnf(warn, relPath, line, "disable-func not on a function declaration (skipped)")
					// gomutants:disable-next-line INVERT_LOOP_CTRL reason="this continue lives inside a switch case; Go's `break` exits the switch (not the for), and the switch is the last statement in the for body — so `continue` and `break` are observably identical"
					continue
				}
				d.FuncStart = funcRange[0]
				d.FuncEnd = funcRange[1]
				idx.funcs = append(idx.funcs, d)
			case directiveRegexp:
				idx.regexps = append(idx.regexps, d)
			}
		}
	}

	// Only materialise the line array if we actually need to scan source
	// text — for files with only same-line and disable-func directives,
	// this skips the allocation entirely.
	// gomutants:disable-next-line CONDITIONALS_BOUNDARY reason="optimisation gate; widening to `>=` (always true) just allocates lines unnecessarily, no observable change"
	if len(pending) > 0 || len(idx.regexps) > 0 {
		idx.lines = strings.Split(string(src), "\n")
	}
	for _, p := range pending {
		target := nextNonCommentLine(idx.lines, p.sourceLine)
		if target == 0 {
			warnf(warn, relPath, p.sourceLine, "disable-next-line has no following code (skipped)")
			continue
		}
		idx.nextLine[target] = append(idx.nextLine[target], p.d)
	}
	return idx
}

func matchMutant(idx *fileIndex, m mutator.Mutant) (directive, bool) {
	for _, d := range idx.sameLine[m.Line] {
		if directiveMatchesType(d, m.Type) {
			return d, true
		}
	}
	for _, d := range idx.nextLine[m.Line] {
		if directiveMatchesType(d, m.Type) {
			return d, true
		}
	}
	for _, d := range idx.funcs {
		if m.Line >= d.FuncStart && m.Line <= d.FuncEnd && directiveMatchesType(d, m.Type) {
			return d, true
		}
	}
	for _, d := range idx.regexps {
		if d.Regexp.MatchString(idx.lines[m.Line-1]) && directiveMatchesType(d, m.Type) {
			return d, true
		}
	}
	return directive{}, false
}

// directiveMatchesType reports whether a directive's mutator filter
// applies to the given mutator type. Empty/nil Mutators means "all
// mutators" (the result of either omitting the list or supplying "*").
func directiveMatchesType(d directive, t mutator.MutationType) bool {
	if len(d.Mutators) == 0 {
		return true
	}
	_, ok := d.Mutators[t]
	return ok
}

// parseDirective parses a stripped directive body like
// "disable ARITHMETIC_BASE reason=\"foo\"". The leading "gomutants:" has
// already been verified by the caller; this function consumes from the
// kind onward.
func parseDirective(text string, line int, relPath string, warn io.Writer) (directive, bool) {
	rest := strings.TrimPrefix(text, directivePrefix)
	kindStr, rest := splitFirstWord(rest)

	var kind directiveKind
	switch kindStr {
	case "disable":
		kind = directiveSameLine // gomutants:disable BRANCH_CASE,STATEMENT_REMOVE reason="directiveSameLine == 0 == zero-value of directiveKind, so the explicit assignment is observably identical to relying on the var's zero value"
	case "disable-next-line":
		kind = directiveNextLine
	case "disable-func":
		kind = directiveFunc
	case "disable-regexp":
		kind = directiveRegexp
	default:
		// Unrecognised kind under the gomutants: namespace. Silent so a
		// future kind added in a newer release doesn't break older
		// runs.
		return directive{}, false
	}

	d := directive{Kind: kind}

	if kind == directiveRegexp {
		patStr, after := splitFirstWord(rest)
		if patStr == "" {
			warnf(warn, relPath, line, "disable-regexp missing pattern (skipped)")
			return directive{}, false
		}
		re, err := regexp.Compile(patStr)
		if err != nil {
			warnf(warn, relPath, line, "disable-regexp invalid pattern %q: %v (skipped)", patStr, err)
			return directive{}, false
		}
		d.Regexp = re
		rest = after
	}

	mutatorsStr, reasonStr, ok := splitMutatorsAndReason(rest)
	if !ok {
		warnf(warn, relPath, line, "malformed reason= (skipped)")
		return directive{}, false
	}
	d.Reason = reasonStr

	if mutatorsStr != "*" {
		d.Mutators = make(map[mutator.MutationType]struct{})
		named := 0
		for name := range strings.SplitSeq(mutatorsStr, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			named++
			if !isKnownMutator(name) {
				if d.Kind == directiveRegexp {
					warnf(warn, relPath, line, "unknown mutator %q in disable-regexp directive (note: patterns cannot contain whitespace; use \\s) (skipped)", name)
				} else {
					warnf(warn, relPath, line, "unknown mutator %q in directive (skipped)", name)
				}
				continue
			}
			d.Mutators[mutator.MutationType(name)] = struct{}{}
		}
		// If the user named at least one mutator and every name was unknown,
		// drop the directive — falling back to "all" would let a typo like
		// `TYPP_O` silently disable every mutator on the line, masking the
		// exact failure mode mutation testing is meant to surface.
		if named > 0 && len(d.Mutators) == 0 {
			warnf(warn, relPath, line, "all named mutators unknown (skipped)")
			return directive{}, false
		}
	}

	return d, true
}

func splitFirstWord(s string) (first, rest string) {
	s = strings.TrimLeft(s, " \t")
	i := strings.IndexAny(s, " \t")
	// gomutants:disable-next-line CONDITIONALS_BOUNDARY reason="`< → <=` would also accept i==0; after the leading TrimLeft above, position 0 is never whitespace, so i==0 is unreachable and the boundary widening is observably identical"
	if i < 0 {
		return s, ""
	}
	return s[:i], strings.TrimLeft(s[i:], " \t")
}

// splitMutatorsAndReason parses an optional mutator list followed by an
// optional `reason="..."`. Returns ok=false if `reason=` is present but
// malformed (e.g., unbalanced quotes, trailing junk).
func splitMutatorsAndReason(s string) (mutators, reason string, ok bool) {
	before, tail, found := strings.Cut(strings.TrimSpace(s), "reason=")
	mutators = strings.TrimSpace(before)
	if !found {
		return mutators, "", true
	}
	quoted, err := strconv.QuotedPrefix(tail)
	if err != nil || strings.TrimSpace(tail[len(quoted):]) != "" {
		return "", "", false
	}
	val, _ := strconv.Unquote(quoted)
	return mutators, val, true
}

// nextNonCommentLine returns the smallest line number > directiveLine
// whose source is neither blank nor a `//` comment, or 0 if none.
func nextNonCommentLine(lines []string, directiveLine int) int {
	for l := directiveLine + 1; l <= len(lines); l++ {
		trimmed := strings.TrimSpace(lines[l-1])
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}
		return l
	}
	return 0
}

// defaultRegistry backs isKnownMutator so directive validation runs
// against the same registered-type set as --only/--disable validation
// (Registry.IsKnown). Names are validated independent of the user's
// --only/--disable so directive typos surface even when the user has
// narrowed their set.
var defaultRegistry = mutator.NewRegistry()

func isKnownMutator(name string) bool {
	return defaultRegistry.IsKnown(name)
}
