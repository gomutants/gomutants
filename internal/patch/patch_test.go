package patch_test

import (
	"testing"

	"github.com/gomutants/gomutants/internal/patch"
)

func TestApplySameLength(t *testing.T) {
	original := []byte("a + b")
	got, err := patch.Apply(original, 2, 3, "-")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "a - b" {
		t.Errorf("got %q, want %q", string(got), "a - b")
	}
}

func TestApplyShorter(t *testing.T) {
	original := []byte("a <= b")
	got, err := patch.Apply(original, 2, 4, "<")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "a < b" {
		t.Errorf("got %q, want %q", string(got), "a < b")
	}
}

func TestApplyLonger(t *testing.T) {
	original := []byte("a < b")
	got, err := patch.Apply(original, 2, 3, "<=")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "a <= b" {
		t.Errorf("got %q, want %q", string(got), "a <= b")
	}
}

func TestApplyBlockReplacement(t *testing.T) {
	original := []byte(`if x > 0 {
		return x
	}`)
	// Replace the block body "{\n\t\treturn x\n\t}" with "{ _ = 0 }"
	got, err := patch.Apply(original, 9, len(original), "{ _ = 0 }")
	if err != nil {
		t.Fatal(err)
	}
	want := "if x > 0 { _ = 0 }"
	if string(got) != want {
		t.Errorf("got %q, want %q", string(got), want)
	}
}

func TestApplyInvalidRange(t *testing.T) {
	original := []byte("hello")

	tests := []struct {
		name       string
		start, end int
	}{
		{"negative start", -1, 3},
		{"end beyond length", 0, 10},
		{"start > end", 3, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := patch.Apply(original, tt.start, tt.end, "x")
			if err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

// Kills CONDITIONALS_BOUNDARY on `start < 0` (start==0 is valid).
func TestApplyStartZero(t *testing.T) {
	original := []byte("hello")
	got, err := patch.Apply(original, 0, 0, "X")
	if err != nil {
		t.Fatalf("Apply with start=0, end=0 should succeed: %v", err)
	}
	if string(got) != "Xhello" {
		t.Errorf("got %q, want %q", string(got), "Xhello")
	}
}

// Kills CONDITIONALS_BOUNDARY on `start > end` (start==end is valid, empty insertion).
func TestApplyStartEqualsEnd(t *testing.T) {
	original := []byte("hello")
	got, err := patch.Apply(original, 2, 2, "XY")
	if err != nil {
		t.Fatalf("Apply with start==end should succeed: %v", err)
	}
	if string(got) != "heXYllo" {
		t.Errorf("got %q, want %q", string(got), "heXYllo")
	}
}

// Kills CONDITIONALS_BOUNDARY on `end > len(original)` (end==len is valid, append to end).
func TestApplyEndAtLength(t *testing.T) {
	original := []byte("hello")
	got, err := patch.Apply(original, 5, 5, "!")
	if err != nil {
		t.Fatalf("Apply with end==len should succeed: %v", err)
	}
	if string(got) != "hello!" {
		t.Errorf("got %q, want %q", string(got), "hello!")
	}
}

// Kills ARITHMETIC_BASE and INVERT_NEGATIVES on the capacity expression at
// patch.go:12. Capacity is a pre-sizing optimization — wrong capacity still
// produces correct output, so behavioral assertions on the returned bytes
// don't catch these mutants. cap(result) is observable: the three appends
// total to exactly the pre-sized capacity, so no grow occurs and cap matches
// the formula exactly. Mutating - to + or + to - changes cap observably.
func TestApplyCapacityIsExact(t *testing.T) {
	original := []byte("abcdef")
	got, err := patch.Apply(original, 1, 4, "X")
	if err != nil {
		t.Fatal(err)
	}
	// len(original) - (end-start) + len(replacement) = 6 - 3 + 1 = 4.
	if want := 4; cap(got) != want {
		t.Errorf("cap(got)=%d, want %d", cap(got), want)
	}
}

func TestApplyDoesNotMutateOriginal(t *testing.T) {
	original := []byte("a + b")
	snapshot := string(original)
	_, err := patch.Apply(original, 2, 3, "-")
	if err != nil {
		t.Fatal(err)
	}
	if string(original) != snapshot {
		t.Error("Apply mutated the original slice")
	}
}
