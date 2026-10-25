// Package unusedimport is the fixture for mutants that drop the last use
// of a name Go requires to be used. Label's return expression is the one
// place strconv is used, and Twice's the one place m is read, so a
// RETURN_ZERO that replaces either wholesale leaves the name unused.
package unusedimport

import "strconv"

// Label returns n prefixed with "n".
func Label(n int) string { return "n" + strconv.Itoa(n) }

// Twice returns n doubled.
func Twice(n int) int {
	m := n * 2
	return m
}
