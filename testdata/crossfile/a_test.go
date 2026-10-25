package crossfile

import "testing"

// TestAdjustZero is deliberately written in terms of Offset rather than a
// literal, so it stays green for any value of the constant. That is what
// makes this fixture a cache-soundness probe: editing only b.go flips the
// ARITHMETIC_BASE mutant in a.go from KILLED to LIVED while a.go and this
// file stay byte-identical.
func TestAdjustZero(t *testing.T) {
	if got := Adjust(0); got != Offset {
		t.Errorf("Adjust(0) = %d, want %d", got, Offset)
	}
}
