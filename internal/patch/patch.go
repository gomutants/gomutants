package patch

import (
	"cmp"
	"fmt"
	"slices"
)

// Apply returns a copy of original with bytes [start:end) replaced by replacement.
func Apply(original []byte, start, end int, replacement string) ([]byte, error) {
	if start < 0 || end > len(original) || start > end {
		return nil, fmt.Errorf("patch: invalid range [%d:%d) in %d-byte file", start, end, len(original))
	}
	// Pre-size so the three appends below don't grow the backing array.
	// For large files × many mutants this saves ~2× the final size in churn.
	out := make([]byte, 0, len(original)-(end-start)+len(replacement))
	out = append(out, original[:start]...)
	out = append(out, replacement...)
	out = append(out, original[end:]...)
	return out, nil
}

// Edit replaces the bytes [Start:End) of a file with Replacement. A
// zero-width edit (Start == End) is an insertion at that offset —
// RANGE_BREAK already models its mutation that way, so ApplyAll has to
// carry the same shape.
type Edit struct {
	Start       int
	End         int
	Replacement string
}

// ApplyAll returns a copy of original with every edit applied. Unlike
// Apply, which rewrites one span for one mutant, ApplyAll composes the
// whole of a package file's schema in a single pass: mutant schemata puts
// every guard into one compilation, so the edits have to land together or
// their offsets — all measured against the *original* bytes — would shift
// under each other.
//
// Edits are sorted here rather than demanded sorted from the caller, so a
// caller assembling edits per mutator cannot produce a wrong file by
// getting the order wrong. Overlapping edits are rejected: two guards
// covering the same bytes have no single correct composition, and the
// schemata classifier is expected to have demoted one of them to the
// per-mutant overlay path before reaching here.
func ApplyAll(original []byte, edits []Edit) ([]byte, error) {
	// No empty-input special case: with no edits the loop below does not
	// run and the final append copies the whole file, which is exactly the
	// wanted result.
	//
	// Sort a copy: the caller's slice is its own, and schemata reuses its
	// decision slice across the build loop's retry rounds.
	sorted := slices.Clone(edits)
	slices.SortFunc(sorted, func(a, b Edit) int {
		// Replacement is the last tiebreaker for the same reason
		// discover.candidateLess uses it: two zero-width inserts at one
		// offset are legal and must compose in a stable order across runs,
		// or the generated source (and every build-cache key over it)
		// churns for no reason.
		return cmp.Or(
			cmp.Compare(a.Start, b.Start),
			cmp.Compare(a.End, b.End),
			cmp.Compare(a.Replacement, b.Replacement),
		)
	})

	size := len(original)
	// gomutants:disable-next-line INTEGER_DECREMENT reason="the invalid-range check inside the loop returns before the prevEnd comparison for any Start < 0, so on the first iteration `Start < 0` and `Start < -1` are both false for every Start that reaches it; later iterations overwrite prevEnd"
	prevEnd := 0
	for i, e := range sorted {
		if e.Start < 0 || e.End > len(original) || e.Start > e.End {
			return nil, fmt.Errorf("patch: invalid range [%d:%d) in %d-byte file", e.Start, e.End, len(original))
		}
		if e.Start < prevEnd {
			return nil, fmt.Errorf("patch: edit %d [%d:%d) overlaps the previous edit ending at %d", i, e.Start, e.End, prevEnd)
		}
		prevEnd = e.End
		// gomutants:disable-next-line INVERT_ASSIGNMENTS,ARITHMETIC_BASE,INVERT_NEGATIVES reason="size is only the `make` capacity below; a wrong value changes how much the slice grows, never what it contains, so no assertion on the returned bytes can distinguish these"
		size += len(e.Replacement) - (e.End - e.Start)
	}

	// Pre-size exactly: a schematized file is large and is rebuilt on every
	// retry round, so the growth churn is worth avoiding.
	out := make([]byte, 0, size)
	cursor := 0
	for _, e := range sorted {
		out = append(out, original[cursor:e.Start]...)
		out = append(out, e.Replacement...)
		cursor = e.End
	}
	return append(out, original[cursor:]...), nil
}
