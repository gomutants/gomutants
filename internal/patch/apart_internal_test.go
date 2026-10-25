package patch

import (
	"strings"
	"testing"
)

func TestKeepApart(t *testing.T) {
	tests := []struct {
		name        string
		original    string
		start, end  int
		replacement string
		want        string
	}{
		{"deletion gluing keyword to operand", "return!ok", 6, 7, "", " "},
		{"deletion after a space", "a = !ok", 4, 5, "", ""},
		{"deletion at end of file", "a!", 1, 2, "", ""},
		{"deletion inside one token", "abc", 1, 2, "", ""},
		{"negative after <", "a<0", 2, 3, "-1", " -1"},
		{"negative after -", "a-0", 2, 3, "-1", " -1"},
		{"negative after +", "a+0", 2, 3, "-1", "-1"},
		{"plus after plus", "a+-b", 2, 3, "+", " +"},
		{"identifier before a number", "+1", 0, 1, "x", "x "},
		{"replacement at end of file", "a+", 1, 2, "b", " b"},
		{"insertion between unfused bytes", "a+", 1, 1, "b", " b"},
		{"replacement fusing on both sides", "a+b", 1, 2, "c", " c "},
		{"seams inside one token", "abc", 1, 2, "x", "x"},
		{"empty file", "", 0, 0, "x", "x"},
		{"negative start", "ab", -1, 1, "x", "x"},
		{"end past the file", "ab", 1, 3, "x", "x"},
		{"start past end", "ab", 2, 1, "x", "x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := keepApart([]byte(tt.original), tt.start, tt.end, tt.replacement); got != tt.want {
				t.Errorf("keepApart(%q, %d, %d, %q) = %q, want %q",
					tt.original, tt.start, tt.end, tt.replacement, got, tt.want)
			}
		})
	}
}

func TestFuses(t *testing.T) {
	for _, pair := range []string{"ab", "a1", "--", "++", "<-", "<=", "//", "/*", "1.", "&^", "\"x"} {
		if !fuses(pair[0], pair[1]) {
			t.Errorf("fuses(%q, %q) = false, want true", pair[0], pair[1])
		}
	}
	for _, pair := range []string{"+1", "-1", ")a", "a(", "a+", "x\"", ";a"} {
		if fuses(pair[0], pair[1]) {
			t.Errorf("fuses(%q, %q) = true, want false", pair[0], pair[1])
		}
	}
	// Whitespace on either side never fuses, though the scanner alone
	// would read `\na` as one token past the newline, and `a ` as a token
	// running up to the semicolon it inserts at the end.
	for _, ws := range []byte(" \t\n\r") {
		if fuses(ws, 'a') || fuses('a', ws) {
			t.Errorf("fuses with %q = true, want false", ws)
		}
	}
}

// TestApplyMutantKeepsTokensApart runs the mutants the fuzzer first found
// broken through ApplyMutant end to end. Found by
// FuzzMutatorsKeepSourceParseable.
func TestApplyMutantKeepsTokensApart(t *testing.T) {
	tests := []struct {
		name        string
		original    string
		target      string
		replacement string
		want        string
	}{
		{
			"REMOVE_LOGICAL_NOT", "package p\nfunc f(ok bool) bool { return!ok }\n", "!", "",
			"package p\nfunc f(ok bool) bool { return ok }\n",
		},
		{
			"INTEGER_DECREMENT", "package p\nfunc f(a int) bool { return a<0 }\n", "0", "-1",
			"package p\nfunc f(a int) bool { return a< -1 }\n",
		},
		{
			"unused import repair", "package p\nimport\"strconv\"\nfunc f(n int) string { return strconv.Itoa(n) }\n",
			"strconv.Itoa(n)", `""`,
			"package p\nimport _ \"strconv\"\nfunc f(n int) string { return \"\" }\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start := strings.LastIndex(tt.original, tt.target)
			got, err := ApplyMutant("p.go", []byte(tt.original), start, start+len(tt.target), tt.replacement)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}
