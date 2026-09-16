package schemata

import (
	"cmp"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"

	"github.com/szhekpisov/gomutants/internal/mutator"
	"github.com/szhekpisov/gomutants/internal/patch"
)

// Guard records where one mutant's generated code landed, so a compiler
// diagnostic can be attributed back to the mutant that caused it.
type Guard struct {
	MutantID int
	// Start and End bound the guard's own generated text.
	Start, End int
	// DeclStart and DeclEnd bound the enclosing top-level declaration.
	// Errors a guard causes are not always reported inside it — a guarded
	// block that was a function's terminating statement produces a
	// "missing return" at the closing brace — so attribution falls back to
	// the declaration when containment finds nothing.
	DeclStart, DeclEnd int
}

// FileSchema is one source file with every schematizable mutant emitted
// into it.
type FileSchema struct {
	Source []byte
	Guards []Guard
}

// Generate rewrites one file so that each of its schematizable mutants can
// be activated at run time, and reports the mutants it declined.
//
// All injected text is newline-free: the guards wrap spans of the original
// bytes rather than reformatting them, so the output stays close enough to
// the input to read in a debugger.
func Generate(fset *token.FileSet, file *ast.File, src []byte, mutants []mutator.Mutant) (FileSchema, []fallback, error) {
	sites, spots, inserts := selectSites(fset, file)
	slices.SortFunc(sites, func(a, b site) int {
		return cmp.Or(cmp.Compare(a.start, b.start), cmp.Compare(a.end, b.end))
	})

	routed, declined := route(fset, file, src, sites, spots, inserts, mutants)

	edits, rel := emit(src, routed)
	out, err := patch.ApplyAll(src, edits)
	if err != nil {
		return FileSchema{}, nil, fmt.Errorf("schemata: composing %s: %w", file.Name.Name, err)
	}

	guards := resolveGuards(fset, file, edits, rel)
	if err := parseFragment(string(out)); err != nil {
		// A generated file that does not parse is a bug in this package,
		// not a property of the user's code: report it so the caller can
		// fall the whole file back rather than handing the compiler
		// something meaningless.
		return FileSchema{}, nil, fmt.Errorf("schemata: generated source does not parse: %w", err)
	}
	return FileSchema{Source: out, Guards: guards}, declined, nil
}

// route assigns each mutant to the site that contains it, gives
// deletion-shaped mutants outside any site a guard of their own, and
// declines the rest.
func route(fset *token.FileSet, file *ast.File, src []byte, sites []site,
	spots map[int]guardSpot, inserts map[int]bool, mutants []mutator.Mutant,
) ([]site, []fallback) {
	spans := enclosingFuncs(fset, file)

	// Sites are non-nested by construction, so a mutant is inside at most
	// one and a binary search settles it.
	find := func(m mutator.Mutant) int {
		i, _ := slices.BinarySearchFunc(sites, m.StartOffset, func(s site, off int) int {
			return cmp.Compare(s.start, off)
		})
		// BinarySearchFunc lands on the first site at or after the mutant;
		// the containing site can only be that one or its predecessor.
		for _, j := range []int{i, i - 1} {
			if j >= 0 && j < len(sites) &&
				sites[j].start <= m.StartOffset && m.EndOffset <= sites[j].end {
				return j
			}
		}
		return -1
	}

	ordered := slices.Clone(mutants)
	slices.SortFunc(ordered, func(a, b mutator.Mutant) int { return cmp.Compare(a.ID, b.ID) })

	var extra []site
	var declined []fallback
	for _, m := range ordered {
		if reason := orphanReason(fset, file, src, m); reason != "" {
			declined = append(declined, fallback{mutant: m, reason: reason})
			continue
		}
		if j := find(m); j >= 0 {
			if reason := breaksTermination(spans, sites[j], m); reason != "" {
				declined = append(declined, fallback{mutant: m, reason: reason})
				continue
			}
			sites[j].mutants = append(sites[j].mutants, m)
			continue
		}
		s, reason := standaloneSite(m, spots, inserts, spans)
		if reason != "" {
			declined = append(declined, fallback{mutant: m, reason: reason})
			continue
		}
		extra = append(extra, s)
	}

	// Drop sites nothing routed into; they would emit a wrapper with no
	// alternatives, which is pure cost.
	kept := make([]site, 0, len(sites)+len(extra))
	for _, s := range sites {
		if len(s.mutants) > 0 {
			kept = append(kept, s)
		}
	}
	return append(kept, extra...), declined
}

// standaloneSite gives a deletion-shaped mutant that no wrap site covers a
// guard of its own — an in-place wrapper that costs no duplicated source.
func standaloneSite(m mutator.Mutant, spots map[int]guardSpot, inserts map[int]bool, spans []funcSpan) (site, string) {
	switch {
	case blockGuardTypes[m.Type]:
		spot, ok := spots[m.StartOffset]
		if !ok {
			return site{}, "no guardable block at this offset"
		}
		if !spot.eligible {
			return site{}, spot.reason
		}
		// Guarding leaves an `if` with no else, which never terminates. If
		// that is what kept the enclosing function returning, the package
		// would not compile — and the overlay path reports NOT VIABLE for
		// the same mutation, so declining preserves the verdict.
		if breaksReturn(spans, spot.innerStart, spot.innerEnd, spot.node) {
			return site{}, "guarding the block would leave the function without a terminating statement"
		}
		return site{kind: siteBlockGuard, start: spot.innerStart, end: spot.innerEnd, node: spot.node, mutants: []mutator.Mutant{m}}, ""
	case m.Type == mutator.RangeBreak:
		if !inserts[m.StartOffset] {
			return site{}, "no range body at this offset"
		}
		return site{kind: siteInsert, start: m.StartOffset, end: m.StartOffset, mutants: []mutator.Mutant{m}}, ""
	default:
		return site{}, "no schematizable site encloses this mutant"
	}
}

// relGuard is a guard's position inside the text one edit emits, before the
// edit is placed in the output.
type relGuard struct {
	mutantID   int
	editIndex  int
	start, end int
}

// emit turns routed sites into byte edits, recording where each mutant's
// guard sits inside the emitted text.
func emit(src []byte, sites []site) ([]patch.Edit, []relGuard) {
	var edits []patch.Edit
	var rel []relGuard

	for _, s := range sites {
		switch s.kind {
		case siteBoolWrap, siteStmtWrap:
			var b strings.Builder
			if s.kind == siteBoolWrap {
				b.WriteString("func() bool { ")
			}
			for _, m := range s.mutants {
				clause := clauseFor(s.kind, src, s, m)
				rel = append(rel, relGuard{
					mutantID:  m.ID,
					editIndex: len(edits),
					start:     b.Len(),
					end:       b.Len() + len(clause),
				})
				b.WriteString(clause)
			}
			b.WriteString(originalTail(s.kind, string(src[s.start:s.end])))
			edits = append(edits, patch.Edit{Start: s.start, End: s.end, Replacement: b.String()})

		case siteBlockGuard:
			m := s.mutants[0]
			open := fmt.Sprintf("if !%s(%d) { ", guardFunc, m.ID)
			rel = append(rel, relGuard{mutantID: m.ID, editIndex: len(edits), start: 0, end: len(open)})
			edits = append(edits,
				patch.Edit{Start: s.start, End: s.start, Replacement: open},
				patch.Edit{Start: s.end, End: s.end, Replacement: " }"})

		case siteInsert:
			m := s.mutants[0]
			text := fmt.Sprintf(" if %s(%d) { break };", guardFunc, m.ID)
			rel = append(rel, relGuard{mutantID: m.ID, editIndex: len(edits), start: 0, end: len(text)})
			edits = append(edits, patch.Edit{Start: s.start, End: s.start, Replacement: text})
		}
	}
	return edits, rel
}

// clauseFor renders one mutant's alternative. The alternative is the
// original span with that mutant's byte replacement applied, so whatever
// the mutator produced is preserved exactly.
func clauseFor(kind siteKind, src []byte, s site, m mutator.Mutant) string {
	alt, err := patch.Apply(src[s.start:s.end], m.StartOffset-s.start, m.EndOffset-s.start, m.Replacement)
	if err != nil {
		// route only assigns mutants whose span lies inside the site, so
		// this is unreachable; degrade to the unmutated text rather than
		// emitting something that will not compile.
		alt = src[s.start:s.end]
	}
	if kind == siteBoolWrap {
		return fmt.Sprintf("if %s(%d) { return %s }; ", guardFunc, m.ID, alt)
	}
	return fmt.Sprintf("if %s(%d) { %s } else ", guardFunc, m.ID, alt)
}

// originalTail closes a wrapper with the unmutated text, which is what runs
// when no mutant of this site is active.
func originalTail(kind siteKind, original string) string {
	if kind == siteBoolWrap {
		return "return " + original + " }()"
	}
	return "{ " + original + " }"
}

// resolveGuards converts guard positions recorded against each edit's own
// text into absolute offsets in the generated file.
func resolveGuards(fset *token.FileSet, file *ast.File, edits []patch.Edit, rel []relGuard) []Guard {
	// Edits were built in site order; ApplyAll sorts internally, so mirror
	// that here to compute each edit's output position.
	order := make([]int, len(edits))
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(a, b int) int {
		return cmp.Or(
			cmp.Compare(edits[a].Start, edits[b].Start),
			cmp.Compare(edits[a].End, edits[b].End),
			cmp.Compare(edits[a].Replacement, edits[b].Replacement),
		)
	})

	// outStart[i] is where edit i's replacement begins in the output, and
	// shiftAt lets any original offset be mapped forward.
	outStart := make([]int, len(edits))
	type shift struct{ origEnd, delta int }
	shifts := make([]shift, 0, len(edits))
	delta := 0
	for _, i := range order {
		e := edits[i]
		outStart[i] = e.Start + delta
		delta += len(e.Replacement) - (e.End - e.Start)
		shifts = append(shifts, shift{origEnd: e.End, delta: delta})
	}
	mapOffset := func(off int) int {
		d := 0
		for _, s := range shifts {
			if s.origEnd > off {
				break
			}
			d = s.delta
		}
		return off + d
	}

	decls := collectDeclSpans(fset, file)
	guards := make([]Guard, 0, len(rel))
	for _, r := range rel {
		base := outStart[r.editIndex]
		ds, de := decls.find(edits[r.editIndex].Start)
		guards = append(guards, Guard{
			MutantID:  r.mutantID,
			Start:     base + r.start,
			End:       base + r.end,
			DeclStart: mapOffset(ds),
			DeclEnd:   mapOffset(de),
		})
	}
	return guards
}

// declSpan is one top-level declaration's byte range in the original file.
type declSpan struct{ start, end int }

type declSpans []declSpan

func (d declSpans) find(off int) (int, int) {
	for _, s := range d {
		if s.start <= off && off < s.end {
			return s.start, s.end
		}
	}
	return 0, 0
}

func collectDeclSpans(fset *token.FileSet, file *ast.File) declSpans {
	out := make(declSpans, 0, len(file.Decls))
	for _, d := range file.Decls {
		out = append(out, declSpan{
			start: fset.Position(d.Pos()).Offset,
			end:   fset.Position(d.End()).Offset,
		})
	}
	return out
}

// breaksTermination reports why a mutant cannot join a wrap site because
// its alternative would stop the enclosing function from returning.
//
// Only a mutation that replaces a whole statement can change whether that
// statement terminates; anything narrower leaves the statement's shape
// intact. So the check is limited to those, and costs a parse of the
// replacement text only for the handful of deletion-shaped mutants.
func breaksTermination(spans []funcSpan, s site, m mutator.Mutant) string {
	if s.kind != siteStmtWrap || s.node == nil {
		return ""
	}
	if m.StartOffset != s.start || m.EndOffset != s.end {
		return ""
	}
	orig, ok := s.node.(ast.Stmt)
	if !ok || !(nonTerm{}).terminates(orig) {
		return "" // Nothing to lose.
	}
	if stmtTextTerminates(m.Replacement) {
		return ""
	}
	if breaksReturn(spans, s.start, s.end, orig) {
		return "replacing the statement would leave the function without a terminating statement"
	}
	return ""
}

// stmtTextTerminates reports whether a replacement statement is itself a
// terminating statement. Unparseable text reads as non-terminating, which
// only costs the mutant the fast path.
func stmtTextTerminates(text string) bool {
	f, err := parser.ParseFile(token.NewFileSet(), "alt.go",
		"package p\nfunc _() {\n"+text+"\n}\n", parser.SkipObjectResolution)
	if err != nil || len(f.Decls) == 0 {
		return false
	}
	fn, ok := f.Decls[0].(*ast.FuncDecl)
	if !ok || fn.Body == nil {
		return false
	}
	return (nonTerm{}).terminatesList(fn.Body.List)
}
