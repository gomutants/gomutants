package unusedimport

import "testing"

// TestWeak covers Label and Twice and asserts nothing about their values,
// so a mutant of either that compiles has to survive.
func TestWeak(t *testing.T) {
	_ = Label(3)
	_ = Twice(3)
}
