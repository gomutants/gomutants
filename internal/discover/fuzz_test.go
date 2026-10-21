package discover

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// FuzzParseUnifiedDiff checks that ParseUnifiedDiff never panics and that
// every range it returns is a non-empty run of real, 1-indexed lines.
func FuzzParseUnifiedDiff(f *testing.F) {
	f.Add([]byte("diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1,2 +3,4 @@ func f() {\n"))
	f.Add([]byte("+++ b/a.go\t2024-01-01\n@@ -5 +5 @@\n@@ -7,0 +8,0 @@\n+++ /dev/null\n@@ -1 +1 @@\n"))
	f.Add([]byte("+++ a.go\n@@ -1 +12,3 @@\n@@ -1 +x,y @@\n@@ nothing\n"))
	f.Add([]byte("+++ a.go\n@@ -1 +9223372036854775807,2 @@\n"))

	f.Fuzz(func(t *testing.T, data []byte) {
		ranges, err := ParseUnifiedDiff(bytes.NewReader(data))
		if err != nil {
			return
		}
		for file, rs := range ranges {
			for _, r := range rs {
				if r.Start < 1 || r.End < r.Start {
					t.Fatalf("%s: range %+v is not a run of real lines", file, r)
				}
			}
		}
	})
}

// FuzzParseDirective checks that no comment body, however malformed,
// panics the directive parser, and that a directive it accepts names only
// registered mutators.
func FuzzParseDirective(f *testing.F) {
	f.Add(`gomutants:disable`)
	f.Add(`gomutants:disable-next-line ARITHMETIC_BASE,BRANCH_IF reason="x"`)
	f.Add(`gomutants:disable-func * reason="a \"quoted\" reason"`)
	f.Add(`gomutants:disable-regexp ^log\. STATEMENT_REMOVE`)
	f.Add(`gomutants:disable-regexp ( reason=`)
	f.Add(`gomutants:disable TYPO reason="unterminated`)

	f.Fuzz(func(t *testing.T, text string) {
		d, ok := parseDirective(text, 1, "fuzz.go", io.Discard)
		if !ok {
			return
		}
		for typ := range d.Mutators {
			if !isKnownMutator(string(typ)) {
				t.Fatalf("directive %q accepted unknown mutator %q", text, typ)
			}
		}
	})
}

// FuzzCallExcluder checks that no glob panics NewCallExcluder, which
// compiles with regexp.MustCompile, and that a glob without `*` matches
// exactly the selector it spells, its dots and other metacharacters taken
// literally.
func FuzzCallExcluder(f *testing.F) {
	f.Add("log.Printf", "log.Printf")
	f.Add("log.*", "log.Println")
	f.Add("(*T).Method", "(*T).Method")
	f.Add("a.b", "aXb")
	f.Add("[", "[")

	f.Fuzz(func(t *testing.T, glob, selector string) {
		ce, err := NewCallExcluder([]string{glob})
		if err != nil || ce == nil {
			return
		}
		spec := strings.TrimSpace(glob)
		if strings.Contains(spec, "*") {
			return
		}
		if got, want := ce.Match(selector), selector == spec; got != want {
			t.Fatalf("NewCallExcluder(%q).Match(%q) = %v, want %v", glob, selector, got, want)
		}
	})
}
