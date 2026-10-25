package embedcache

import "testing"

// TestAdjustZero is deliberately written in terms of Offset rather than a
// literal, so it stays green for any value in offset.txt. Editing only that
// data file must still invalidate the cached verdict for Adjust's mutant.
func TestAdjustZero(t *testing.T) {
	if got := Adjust(0); got != Offset() {
		t.Errorf("Adjust(0) = %d, want %d", got, Offset())
	}
}
