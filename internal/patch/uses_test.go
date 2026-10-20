package patch_test

import (
	"strings"
	"testing"

	"github.com/gomutants/gomutants/internal/patch"
)

// TestApplyMutantKeepsUses pins each repair ApplyMutant makes, and each
// case it must leave alone. A case replaces the one occurrence of span in
// src with repl; want is the whole file it should produce.
func TestApplyMutantKeepsUses(t *testing.T) {
	tests := []struct {
		name, src, span, repl, want string
	}{
		{
			name: "import loses its last use",
			src:  "package p\n\nimport \"strconv\"\n\nfunc L(n int) string { return \"n\" + strconv.Itoa(n) }\n",
			span: `"n" + strconv.Itoa(n)`, repl: `""`,
			want: "package p\n\nimport _ \"strconv\"\n\nfunc L(n int) string { return \"\" }\n",
		},
		{
			name: "renamed import",
			src:  "package p\n\nimport sc \"strconv\"\n\nfunc L(n int) string { return sc.Itoa(n) }\n",
			span: `sc.Itoa(n)`, repl: `""`,
			want: "package p\n\nimport _ \"strconv\"\n\nfunc L(n int) string { return \"\" }\n",
		},
		{
			name: "import still used elsewhere",
			src:  "package p\n\nimport \"strconv\"\n\nfunc L(n int) string { return strconv.Itoa(n) }\nfunc M(n int) string { return strconv.Itoa(n) }\n",
			span: `strconv.Itoa(n) }` + "\nfunc M", repl: `"" }` + "\nfunc M",
			want: "package p\n\nimport \"strconv\"\n\nfunc L(n int) string { return \"\" }\nfunc M(n int) string { return strconv.Itoa(n) }\n",
		},
		{
			name: "major-version path",
			src:  "package p\n\nimport \"example.com/yaml/v3\"\n\nfunc L() any { return yaml.Marshal }\n",
			span: `yaml.Marshal`, repl: `nil`,
			want: "package p\n\nimport _ \"example.com/yaml/v3\"\n\nfunc L() any { return nil }\n",
		},
		{
			name: "gopkg.in path",
			src:  "package p\n\nimport \"gopkg.in/yaml.v3\"\n\nfunc L() any { return yaml.Marshal }\n",
			span: `yaml.Marshal`, repl: `nil`,
			want: "package p\n\nimport _ \"gopkg.in/yaml.v3\"\n\nfunc L() any { return nil }\n",
		},
		{
			// The package name isn't the path's last element, so the
			// single-file check reads the import unused before the
			// mutation too and can't tell it was the last use.
			name: "package name the path doesn't give",
			src:  "package p\n\nimport \"example.com/notname\"\n\nfunc L() any { return realname.X }\n",
			span: `realname.X`, repl: `nil`,
			want: "package p\n\nimport \"example.com/notname\"\n\nfunc L() any { return nil }\n",
		},
		{
			name: "short variable declaration",
			src:  "package p\n\nfunc F(n int) int { m := n * 2; return m + 1 }\n",
			span: `m + 1`, repl: `0`,
			want: "package p\n\nfunc F(n int) int { m := n * 2; _ = m; return 0 }\n",
		},
		{
			name: "var declaration",
			src:  "package p\n\nfunc F(n int) int {\n\tvar m = n * 2\n\treturn m + 1\n}\n",
			span: `m + 1`, repl: `0`,
			want: "package p\n\nfunc F(n int) int {\n\tvar m = n * 2; _ = m\n\treturn 0\n}\n",
		},
		{
			name: "two locals in one statement",
			src:  "package p\n\nfunc F(n int) int { a, b := n, n; return a + b }\n",
			span: `a + b`, repl: `0`,
			want: "package p\n\nfunc F(n int) int { a, b := n, n; _ = a; _ = b; return 0 }\n",
		},
		{
			name: "if init",
			src:  "package p\n\nfunc F(n int) int {\n\tif m := n * 2; n > 0 { return m * 3 }\n\treturn 0\n}\n",
			span: `m * 3`, repl: `0`,
			want: "package p\n\nfunc F(n int) int {\n\tif m := n * 2; n > 0 { _ = m; return 0 }\n\treturn 0\n}\n",
		},
		{
			name: "range value",
			src:  "package p\n\nfunc F(xs []int) (s int) {\n\tfor _, v := range xs { s += v * 2 }\n\treturn s\n}\n",
			span: `v * 2`, repl: `0`,
			want: "package p\n\nfunc F(xs []int) (s int) {\n\tfor _, v := range xs { _ = v; s += 0 }\n\treturn s\n}\n",
		},
		{
			name: "select receive",
			src:  "package p\n\nfunc F(ch chan int) int {\n\tselect {\n\tcase v := <-ch: return v + 1\n\t}\n}\n",
			span: `v + 1`, repl: `0`,
			want: "package p\n\nfunc F(ch chan int) int {\n\tselect {\n\tcase v := <-ch: _ = v; return 0\n\t}\n}\n",
		},
		{
			name: "type switch symbol has no same-line repair",
			src:  "package p\n\nfunc F(x any) int {\n\tswitch v := x.(type) {\n\tcase int: return v + 1\n\t}\n\treturn 0\n}\n",
			span: `v + 1`, repl: `0`,
			want: "package p\n\nfunc F(x any) int {\n\tswitch v := x.(type) {\n\tcase int: return 0\n\t}\n\treturn 0\n}\n",
		},
		{
			// The use would go inside the replacement, which is the
			// mutation's text and not ours to change.
			name: "repair inside the replaced span",
			src:  "package p\n\nfunc F(n int) int {\n\tif m := n * 2; n > 0 { return m }\n\treturn 0\n}\n",
			span: `{ return m }`, repl: `{ _ = 0 }`,
			want: "package p\n\nfunc F(n int) int {\n\tif m := n * 2; n > 0 { _ = 0 }\n\treturn 0\n}\n",
		},
		{
			name: "undefined names from sibling files are not uses lost",
			src:  "package p\n\nfunc F(n int) int { m := helper(n); return m + other }\n",
			span: `m + other`, repl: `0`,
			want: "package p\n\nfunc F(n int) int { m := helper(n); _ = m; return 0 }\n",
		},
		{
			name: "mutant that does not parse",
			src:  "package p\n\nimport \"strconv\"\n\nfunc L(n int) string { return strconv.Itoa(n) }\n",
			span: `strconv.Itoa(n)`, repl: `)`,
			want: "package p\n\nimport \"strconv\"\n\nfunc L(n int) string { return ) }\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if strings.Count(tt.src, tt.span) != 1 {
				t.Fatalf("span %q must occur once in src", tt.span)
			}
			start := strings.Index(tt.src, tt.span)
			got, err := patch.ApplyMutant("p.go", []byte(tt.src), start, start+len(tt.span), tt.repl)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf("got\n%s\nwant\n%s", got, tt.want)
			}
			if strings.Count(string(got), "\n") != strings.Count(tt.src, "\n") {
				t.Errorf("repair moved a line: %d lines, source has %d",
					strings.Count(string(got), "\n"), strings.Count(tt.src, "\n"))
			}
		})
	}
}

func TestApplyMutantInvalidRange(t *testing.T) {
	if _, err := patch.ApplyMutant("p.go", []byte("package p\n"), 3, 1, "x"); err == nil {
		t.Error("expected error, got nil")
	}
}
