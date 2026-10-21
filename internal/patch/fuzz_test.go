package patch_test

import (
	"bytes"
	"testing"

	"github.com/gomutants/gomutants/internal/patch"
)

// FuzzApplyMutant splices arbitrary replacements into arbitrary bytes.
// ApplyMutant must reject exactly the ranges Apply rejects, never panic on
// source its repairs can't make sense of, and never move a line: it may
// add spaces and same-line repairs, never newlines, so the positions in
// compiler output and coverage still match the original file.
func FuzzApplyMutant(f *testing.F) {
	f.Add([]byte("package p\nimport \"strconv\"\nfunc f(n int) string { return \"n\" + strconv.Itoa(n) }\n"), 57, 78, `""`)
	f.Add([]byte("package p\nfunc f(ok bool) bool { return!ok }\n"), 39, 40, "")
	f.Add([]byte("package p\nfunc f(a int) bool { return a<0 }\n"), 40, 41, "-1")
	f.Add([]byte("package p\nfunc f() { x := 1; _ = x }\n"), 21, 27, "_ = 0")
	f.Add([]byte("x"), 1, 0, "y")

	f.Fuzz(func(t *testing.T, original []byte, start, end int, replacement string) {
		got, err := patch.ApplyMutant("fuzz.go", original, start, end, replacement)
		_, wantErr := patch.Apply(original, start, end, replacement)
		if (err != nil) != (wantErr != nil) {
			t.Fatalf("ApplyMutant error = %v, Apply error = %v", err, wantErr)
		}
		if err != nil {
			return
		}
		if want := bytes.Count(original[:start], []byte("\n")) + bytes.Count([]byte(replacement), []byte("\n")) +
			bytes.Count(original[end:], []byte("\n")); bytes.Count(got, []byte("\n")) != want {
			t.Fatalf("ApplyMutant moved lines: got %d newlines, want %d:\n%s", bytes.Count(got, []byte("\n")), want, got)
		}
	})
}
