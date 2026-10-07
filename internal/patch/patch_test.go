package patch_test

import (
	"strings"
	"testing"

	"github.com/szhekpisov/gomutants/internal/patch"
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

func TestApplyAllEmptyReturnsCopy(t *testing.T) {
	original := []byte("a + b")
	got, err := patch.ApplyAll(original, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "a + b" {
		t.Errorf("got %q, want %q", string(got), "a + b")
	}
	// Must be a copy: schemata hands the result to a file write while the
	// original stays in the shared source cache.
	got[0] = 'z'
	if original[0] != 'a' {
		t.Error("ApplyAll returned an alias of the original")
	}
}

func TestApplyAllMultipleEdits(t *testing.T) {
	original := []byte("a + b - c")
	got, err := patch.ApplyAll(original, []patch.Edit{
		{Start: 2, End: 3, Replacement: "*"},
		{Start: 6, End: 7, Replacement: "/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "a * b / c" {
		t.Errorf("got %q, want %q", string(got), "a * b / c")
	}
}

func TestApplyAllSortsUnorderedInput(t *testing.T) {
	original := []byte("a + b - c")
	got, err := patch.ApplyAll(original, []patch.Edit{
		{Start: 6, End: 7, Replacement: "/"},
		{Start: 2, End: 3, Replacement: "*"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "a * b / c" {
		t.Errorf("got %q, want %q", string(got), "a * b / c")
	}
}

func TestApplyAllAdjacentEditsAreNotOverlaps(t *testing.T) {
	original := []byte("abcd")
	got, err := patch.ApplyAll(original, []patch.Edit{
		{Start: 0, End: 2, Replacement: "X"},
		{Start: 2, End: 4, Replacement: "Y"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "XY" {
		t.Errorf("got %q, want %q", string(got), "XY")
	}
}

func TestApplyAllZeroWidthInsert(t *testing.T) {
	// The shape RANGE_BREAK uses: insert without consuming any bytes.
	original := []byte("for range x {\n}")
	got, err := patch.ApplyAll(original, []patch.Edit{
		{Start: 13, End: 13, Replacement: " break;"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "for range x { break;\n}" {
		t.Errorf("got %q", string(got))
	}
}

func TestApplyAllZeroWidthInsertsAtSameOffsetComposeStably(t *testing.T) {
	original := []byte("ab")
	edits := []patch.Edit{
		{Start: 1, End: 1, Replacement: "Y"},
		{Start: 1, End: 1, Replacement: "X"},
	}
	got, err := patch.ApplyAll(original, edits)
	if err != nil {
		t.Fatal(err)
	}
	// Sorted by Replacement, so the order is input-independent.
	if string(got) != "aXYb" {
		t.Errorf("got %q, want %q", string(got), "aXYb")
	}
	reversed, err := patch.ApplyAll(original, []patch.Edit{edits[1], edits[0]})
	if err != nil {
		t.Fatal(err)
	}
	if string(reversed) != string(got) {
		t.Errorf("input order changed the result: %q vs %q", string(reversed), string(got))
	}
}

func TestApplyAllDoesNotReorderCallerSlice(t *testing.T) {
	edits := []patch.Edit{
		{Start: 6, End: 7, Replacement: "/"},
		{Start: 2, End: 3, Replacement: "*"},
	}
	if _, err := patch.ApplyAll([]byte("a + b - c"), edits); err != nil {
		t.Fatal(err)
	}
	if edits[0].Start != 6 {
		t.Errorf("caller slice was sorted in place: %+v", edits)
	}
}

func TestApplyAllRejectsOverlap(t *testing.T) {
	tests := []struct {
		name  string
		edits []patch.Edit
	}{
		{"nested", []patch.Edit{{Start: 0, End: 5, Replacement: "X"}, {Start: 1, End: 2, Replacement: "Y"}}},
		{"straddling", []patch.Edit{{Start: 0, End: 3, Replacement: "X"}, {Start: 2, End: 5, Replacement: "Y"}}},
		{"identical", []patch.Edit{{Start: 1, End: 3, Replacement: "X"}, {Start: 1, End: 3, Replacement: "Y"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := patch.ApplyAll([]byte("abcdefgh"), tt.edits); err == nil {
				t.Error("expected an overlap error, got nil")
			}
		})
	}
}

func TestApplyAllRejectsInvalidRange(t *testing.T) {
	// The message is asserted, not just the presence of an error: a
	// negative start also trips the overlap check that follows, so
	// "some error" cannot tell the two guards apart.
	tests := []struct {
		name string
		edit patch.Edit
	}{
		{"negative start", patch.Edit{Start: -1, End: 2, Replacement: "X"}},
		{"end past EOF", patch.Edit{Start: 0, End: 99, Replacement: "X"}},
		{"start after end", patch.Edit{Start: 3, End: 1, Replacement: "X"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := patch.ApplyAll([]byte("abcde"), []patch.Edit{tt.edit})
			if err == nil {
				t.Fatal("expected a range error, got nil")
			}
			if !strings.Contains(err.Error(), "invalid range") {
				t.Errorf("want an invalid-range error, got %q", err)
			}
		})
	}
}

func TestApplyAllOverlapErrorNamesTheOverlap(t *testing.T) {
	_, err := patch.ApplyAll([]byte("abcdefgh"), []patch.Edit{
		{Start: 0, End: 3, Replacement: "X"},
		{Start: 2, End: 5, Replacement: "Y"},
	})
	if err == nil {
		t.Fatal("expected an overlap error, got nil")
	}
	if !strings.Contains(err.Error(), "overlaps") {
		t.Errorf("want an overlap error, got %q", err)
	}
}
