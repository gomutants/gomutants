package report

import (
	"net/url"
	"strings"
	"testing"
)

// FuzzEscapeProperty checks that an escaped property value can't break
// out of a workflow command — no newline ends the command early, no `:` or
// `,` splits the property list — and that it decodes back to the input.
func FuzzEscapeProperty(f *testing.F) {
	f.Add("internal/a.go")
	f.Add(`C:\src\a,b.go`)
	f.Add("a%2Cb\r\nc")

	f.Fuzz(func(t *testing.T, s string) {
		got := escapeProperty(s)
		if strings.ContainsAny(got, "\r\n:,") {
			t.Fatalf("escapeProperty(%q) = %q, still holds a separator", s, got)
		}
		if back, err := url.PathUnescape(got); err != nil || back != s {
			t.Fatalf("escapeProperty(%q) = %q, decodes to %q (%v)", s, got, back, err)
		}
	})
}

// FuzzEscapeMessage is FuzzEscapeProperty for the message segment, where
// only a newline would end the command early.
func FuzzEscapeMessage(f *testing.F) {
	f.Add("Mutant LIVED — ARITHMETIC_BASE (+ → -)")
	f.Add("100%\r\n")

	f.Fuzz(func(t *testing.T, s string) {
		got := escapeMessage(s)
		if strings.ContainsAny(got, "\r\n") {
			t.Fatalf("escapeMessage(%q) = %q, still holds a newline", s, got)
		}
		if back, err := url.PathUnescape(got); err != nil || back != s {
			t.Fatalf("escapeMessage(%q) = %q, decodes to %q (%v)", s, got, back, err)
		}
	})
}
