package patch

import (
	"go/scanner"
	"go/token"
)

// keepApart returns replacement with a space on either side where, spliced
// into original at [start:end), its edge would fuse with the byte next to
// it into one token. Mutators write replacements for gofmt'd source, where
// tokens stand apart; in unformatted source the mutant would otherwise
// parse as something else or not at all:
//
//	return!ok  → returnok   (REMOVE_LOGICAL_NOT; an identifier)
//	a<0        → a<-1       (INTEGER_DECREMENT; a receive)
//	a+-b       → a++b       (INVERT_NEGATIVES; an increment)
//
// A seam whose bytes already fused in original is inside one token — a
// string literal ERRORF_WRAP edits, say — and is left alone. A space moves
// no line, so positions in compiler output stay true.
func keepApart(original []byte, start, end int, replacement string) string {
	if start < 0 || end > len(original) || start > end {
		return replacement // Apply rejects the range.
	}
	// An empty replacement leaves one seam, original[start-1] against
	// original[end]; any other leaves two, one at each edge.
	before, after := original[:start], original[end:]
	if replacement == "" {
		if fusesAt(before, after) && !fusesAt(before, original[start:]) {
			return " "
		}
		return ""
	}
	out := replacement
	if fusesAt(before, []byte(replacement)) && !fusesAt(before, original[start:]) {
		out = " " + out
	}
	if fusesAt([]byte(replacement), after) && !fusesAt(original[:end], after) {
		out += " "
	}
	return out
}

// fusesAt reports whether the last byte of left and the first byte of
// right fuse. Nothing fuses across the edge of the file.
func fusesAt(left, right []byte) bool {
	return len(left) > 0 && len(right) > 0 && fuses(left[len(left)-1], right[0])
}

// fuses reports whether a followed by b scans as the start of one token:
// `ab` an identifier, `--` a decrement, `<-` a receive, `//` a comment.
func fuses(a, b byte) bool {
	if isSpace(a) || isSpace(b) {
		return false
	}
	src := []byte{a, b}
	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(src))
	var s scanner.Scanner
	// A nil error handler counts errors without stopping the scan, which
	// is all a lone quote or other partial token needs.
	s.Init(file, src, nil, scanner.ScanComments)
	s.Scan()
	next, _, _ := s.Scan()
	return file.Offset(next) > 1
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}
