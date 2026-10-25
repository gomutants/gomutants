package embedcache

import (
	// Blank import: //go:embed on a string variable requires the embed
	// package to be imported, but nothing here references it by name.
	_ "embed"
	"strconv"
	"strings"
)

// offsetRaw is the calibration constant, kept in a data file rather than a
// Go declaration. That is what makes this fixture a cache-soundness probe
// for the //go:embed dimension: editing offset.txt flips the
// ARITHMETIC_BASE mutant below from KILLED to LIVED while every .go file
// in the package stays byte-identical.
//
//go:embed offset.txt
var offsetRaw string

// Offset parses the embedded calibration constant.
func Offset() int {
	n, _ := strconv.Atoi(strings.TrimSpace(offsetRaw))
	return n
}

// Adjust converts a raw reading into a calibrated one.
func Adjust(raw int) int {
	return raw + Offset()
}
