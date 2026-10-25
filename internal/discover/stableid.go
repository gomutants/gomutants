package discover

import (
	"fmt"
	"go/ast"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/gomutants/gomutants/internal/mutator"
)

// funcSpan is one top-level function's byte range in a source file,
// paired with the name used to anchor stable mutant IDs.
type funcSpan struct {
	start int // Byte offset of the `func` keyword.
	end   int // Byte offset just past the closing brace (exclusive).
	name  string
}

// funcSpans returns the top-level function declarations of f in source
// order. Function literals are not listed: a closure's mutants anchor to
// the enclosing declaration, which is what makes the anchor survive edits
// that add or remove closures.
//
// Declarations that render to the same name — two `func init()`, or two
// methods whose receiver shape receiverType cannot render — are given
// distinct anchors by occurrence, so they do not share an ordinal counter.
func funcSpans(fset *token.FileSet, f *ast.File) []funcSpan {
	var spans []funcSpan
	occurrences := make(map[string]int)
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		name := funcName(fn)
		occurrences[name]++
		spans = append(spans, funcSpan{
			start: fset.Position(fn.Pos()).Offset,
			end:   fset.Position(fn.End()).Offset,
			name:  anchorName(name, occurrences[name]),
		})
	}
	return spans
}

// anchorRepeatSep joins a repeated declaration name to its occurrence
// number. It is deliberately a character Go identifiers cannot contain, so
// the disambiguated anchor of one declaration can never collide with the
// plain name of another.
const anchorRepeatSep = "~"

// anchorName renders the anchor for the occurrence'th declaration sharing
// name in a file. The first occurrence keeps the bare name: only files that
// actually repeat a name pay the suffix, which keeps IDs unchanged for the
// overwhelmingly common case of every declaration being uniquely named.
//
// Occurrences are counted in source order, so inserting a new `func init()`
// above the existing ones shifts their suffixes and gives the newcomer the
// bare anchor — the old IDs then resolve to a different declaration. Any
// disambiguator derived from source order has that property; the
// alternatives (hashing the body, say) instead churn an unedited
// declaration's own IDs, which is the worse trade. Documented in README.md
// under the list of edits that change an ID.
func anchorName(name string, occurrence int) string {
	if occurrence == 1 {
		return name
	}
	return name + anchorRepeatSep + strconv.Itoa(occurrence)
}

// funcName renders a declaration's anchor name: "Run" for a plain
// function, "(*Runner).Run" or "(Runner).Run" for a method. Including the
// receiver keeps same-named methods on different types in one file apart.
func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	return "(" + receiverType(fn.Recv.List[0].Type) + ")." + fn.Name.Name
}

// receiverType renders a receiver's type as it appears in source, minus
// any type parameters and any parentheses: `*Runner` → "*Runner",
// `Stack[T]` → "Stack", `(T)` → "T". An unrecognized shape yields "", so
// the anchor degrades to "().M": the receiver is lost, but the method name
// still separates the declaration from its neighbours.
func receiverType(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return "*" + receiverType(t.X)
	case *ast.ParenExpr: // Legal but rare: func (x (T)) M().
		return receiverType(t.X)
	case *ast.IndexExpr: // Generic receiver: Stack[T].
		return receiverType(t.X)
	case *ast.IndexListExpr: // Generic receiver: Pair[K, V].
		return receiverType(t.X)
	case *ast.Ident:
		return t.Name
	default:
		return ""
	}
}

// anchorFor returns the name of the function containing offset, or "" when
// the offset falls outside every span — package-level var and const
// initializers are the usual case. spans must be in source order, which is
// what funcSpans returns.
func anchorFor(spans []funcSpan, offset int) string {
	// Index of the first span starting after offset; the candidate span is
	// therefore the one before it.
	i := sort.Search(len(spans), func(i int) bool {
		return spans[i].start > offset
	})
	if i == 0 {
		return ""
	}
	s := spans[i-1]
	if offset < s.end {
		return s.name
	}
	return ""
}

// ordinalKey groups mutants that a stable ID cannot otherwise tell apart.
// It holds exactly the fields stableID renders, which is what guarantees
// the rendered strings are unique within a run.
type ordinalKey struct {
	file   string
	anchor string
	typ    mutator.MutationType
}

// stableID renders a mutant's cross-run identity.
func stableID(k ordinalKey, ordinal int) string {
	return fmt.Sprintf("%s:%s:%s#%d", k.file, k.anchor, k.typ, ordinal)
}

// stableIDFile renders the file segment of a stable ID: absPath relative to
// the module root, always slash-separated so an ID minted on Windows matches
// one minted on Linux.
//
// This is deliberately not mutator.Mutant.RelFile. RelFile strips the
// longest common import-path prefix of the packages in the run, so it is a
// function of the package arguments rather than of the source: the same
// mutant would be "a.go:F:T#1" under `gomutants ./internal/a/` and
// "internal/a/a.go:F:T#1" under `gomutants ./...`, and adding a new
// top-level package would renumber every ID in a whole-repo report. Anchoring
// to the module root keeps one mutant's ID the same under every scope.
//
// A path outside moduleRoot still has a relative form — "../other/a.go" —
// and keeps it. Only a pair filepath.Rel cannot relate at all, such as an
// empty moduleRoot against an absolute path, falls back to absPath, which
// keeps the ID unique within the report; that uniqueness is what the
// ordinal counter depends on.
func stableIDFile(absPath, moduleRoot string) string {
	rel, err := filepath.Rel(moduleRoot, absPath)
	if err != nil {
		return filepath.ToSlash(absPath)
	}
	return filepath.ToSlash(rel)
}

// ScopeToStableID narrows pkgs to the source files that could hold a
// mutant FilterByStableID would match against id, so a caller resolving one
// id need not parse the whole run. A file qualifies when its ID segment F
// and id agree up to the shorter of the two: id is a prefix of "F:" (it
// stops inside the file segment), or "F:" is a prefix of id (it goes on to
// name something inside F). Every StableID in F starts with "F:", so no
// other file can match.
//
// Every package is kept, emptied when none of its files qualify, and only
// GoFiles is narrowed: Discover derives RelFile from the set of import
// paths, and stable-ID ordinals are counted per file, so a mutant found
// over the narrowed packages is identical to the one the full set yields.
func ScopeToStableID(pkgs []Package, id, moduleRoot string) []Package {
	scoped := make([]Package, len(pkgs))
	for i, pkg := range pkgs {
		scoped[i] = pkg
		scoped[i].GoFiles = nil
		for _, name := range pkg.GoFiles {
			seg := stableIDFile(filepath.Join(pkg.Dir, name), moduleRoot) + ":"
			if strings.HasPrefix(seg, id) || strings.HasPrefix(id, seg) {
				scoped[i].GoFiles = append(scoped[i].GoFiles, name)
			}
		}
	}
	return scoped
}

// maxAmbiguousListed caps how many candidate IDs an ambiguity error
// spells out. Enough to pick from, short enough to stay readable when a
// one-character prefix matches a whole package.
const maxAmbiguousListed = 5

// FilterByStableID narrows mutants to the single one identified by id,
// which is the `id` field of a JSON report entry.
//
// An exact StableID match wins outright; only when there is none is id
// treated as a prefix, and then it must match exactly one mutant. The
// precedence is load-bearing rather than a convenience: "#1" is a literal
// prefix of "#10", so prefix-only matching would report the most ordinary
// case — a function's first mutant of some type — as ambiguous.
//
// Both failure modes are errors because both mean the run cannot do what
// was asked, but they carry different messages: an unknown id is usually
// a scoping mistake, while an ambiguous one just needs more characters.
//
// mutants is expected to be what Discover found over ScopeToStableID(pkgs,
// id), and the no-match message counts it as that: the mutants in the
// files the id's path could name, not every mutant of the run.
func FilterByStableID(mutants []mutator.Mutant, id string) ([]mutator.Mutant, error) {
	for _, m := range mutants {
		if m.StableID == id {
			return []mutator.Mutant{m}, nil
		}
	}

	var matches []mutator.Mutant
	for _, m := range mutants {
		if strings.HasPrefix(m.StableID, id) {
			matches = append(matches, m)
		}
	}

	switch len(matches) {
	case 1:
		return matches, nil
	case 0:
		return nil, fmt.Errorf(
			"no mutant matches --run-mutant-id %q among the %d discovered in the files its path could name; check --only/--disable and that the id came from a report for this revision",
			id, len(mutants))
	default:
		return nil, fmt.Errorf(
			"--run-mutant-id %q is ambiguous, matching %d mutants; use a longer prefix or the full id:\n%s",
			id, len(matches), formatCandidates(matches))
	}
}

// formatCandidates renders an ambiguity error's candidate list, one
// indented id per line, truncated to maxAmbiguousListed.
func formatCandidates(matches []mutator.Mutant) string {
	var b strings.Builder
	for i, m := range matches {
		if i > 0 {
			b.WriteString("\n")
		}
		if i == maxAmbiguousListed {
			fmt.Fprintf(&b, "  ... and %d more", len(matches)-maxAmbiguousListed)
			break
		}
		fmt.Fprintf(&b, "  %s", m.StableID)
	}
	return b.String()
}
