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
			// The use lands inside the replacement, which reads m and
			// changes nothing else.
			name: "repair inside the replaced span",
			src:  "package p\n\nfunc F(n int) int {\n\tif m := n * 2; n > 0 { return m }\n\treturn 0\n}\n",
			span: `{ return m }`, repl: `{ _ = 0 }`,
			want: "package p\n\nfunc F(n int) int {\n\tif m := n * 2; n > 0 { _ = m; _ = 0 }\n\treturn 0\n}\n",
		},
		{
			name: "switch init has no same-line repair",
			src:  "package p\n\nfunc F(n int) int {\n\tswitch m := n * 2; n {\n\tcase 1: return m + 1\n\t}\n\treturn 0\n}\n",
			span: `m + 1`, repl: `0`,
			want: "package p\n\nfunc F(n int) int {\n\tswitch m := n * 2; n {\n\tcase 1: return 0\n\t}\n\treturn 0\n}\n",
		},
		{
			name: "grouped var declaration",
			src:  "package p\n\nfunc F(n int) int {\n\tvar (\n\t\tm = n * 2\n\t)\n\treturn m + 1\n}\n",
			span: `m + 1`, repl: `0`,
			want: "package p\n\nfunc F(n int) int {\n\tvar (\n\t\tm = n * 2; _ = m\n\t)\n\treturn 0\n}\n",
		},
		{
			// go/types reports the import after the local, so the
			// edits arrive out of order.
			name: "import and local in one mutant",
			src:  "package p\n\nimport \"strconv\"\n\nfunc L(n int) string { m := n; return strconv.Itoa(m) }\n",
			span: `strconv.Itoa(m)`, repl: `""`,
			want: "package p\n\nimport _ \"strconv\"\n\nfunc L(n int) string { m := n; _ = m; return \"\" }\n",
		},
		{
			name: "go- prefixed path",
			src:  "package p\n\nimport \"example.com/go-sqlite3\"\n\nfunc L() any { return sqlite3.Open }\n",
			span: `sqlite3.Open`, repl: `nil`,
			want: "package p\n\nimport _ \"example.com/go-sqlite3\"\n\nfunc L() any { return nil }\n",
		},
		{
			name: "undefined names from sibling files are not uses lost",
			src:  "package p\n\nfunc F(n int) int { m := helper(n); return m + other }\n",
			span: `m + other`, repl: `0`,
			want: "package p\n\nfunc F(n int) int { m := helper(n); _ = m; return 0 }\n",
		},
		{
			name: "second of two imports",
			src:  "package p\n\nimport (\n\t\"fmt\"\n\t\"strconv\"\n)\n\nfunc L(n int) string { return fmt.Sprint(n) + strconv.Itoa(n) }\n",
			span: `+ strconv.Itoa(n)`, repl: `+ ""`,
			want: "package p\n\nimport (\n\t\"fmt\"\n\t_ \"strconv\"\n)\n\nfunc L(n int) string { return fmt.Sprint(n) + \"\" }\n",
		},
		{
			// cgo reads `import "C"` by its exact spelling.
			name: "cgo import",
			src:  "package p\n\nimport \"C\"\n\nfunc L(n int) any { return C.int(n) }\n",
			span: `C.int(n)`, repl: `nil`,
			want: "package p\n\nimport \"C\"\n\nfunc L(n int) any { return nil }\n",
		},
		{
			name: "for init",
			src:  "package p\n\nfunc F(n int) (s int) {\n\tfor j, i := n, 0; i < n; i++ { s += j * 2 }\n\treturn s\n}\n",
			span: `j * 2`, repl: `0`,
			want: "package p\n\nfunc F(n int) (s int) {\n\tfor j, i := n, 0; i < n; i++ { _ = j; s += 0 }\n\treturn s\n}\n",
		},
		{
			name: "declaration in a select case body",
			src:  "package p\n\nfunc F(ch chan int) int {\n\tselect {\n\tcase <-ch: v := 1; return v + 1\n\t}\n}\n",
			span: `v + 1`, repl: `0`,
			want: "package p\n\nfunc F(ch chan int) int {\n\tselect {\n\tcase <-ch: v := 1; _ = v; return 0\n\t}\n}\n",
		},
		{
			name: "label has no same-line repair",
			src:  "package p\n\nfunc F(n int) {\nL:\n\tfor {\n\t\tif n > 0 { break L }\n\t}\n}\n",
			span: `{ break L }`, repl: `{ _ = 0 }`,
			want: "package p\n\nfunc F(n int) {\nL:\n\tfor {\n\t\tif n > 0 { _ = 0 }\n\t}\n}\n",
		},
		{
			// w is unused before the mutation too, so it isn't the
			// mutant's to repair.
			name: "local unused in the original too",
			src:  "package p\n\nimport \"strconv\"\n\nfunc L(n int) string {\n\tw := 1\n\ts := strconv.Itoa(n)\n\treturn s\n}\n",
			span: `strconv.Itoa(n)`, repl: `"x"`,
			want: "package p\n\nimport _ \"strconv\"\n\nfunc L(n int) string {\n\tw := 1\n\ts := \"x\"\n\treturn s\n}\n",
		},
		{
			// `other` is undefined only for want of the package's other
			// files, and past the span its offset differs between the
			// versions: an error that isn't an unused one must not read
			// as a lost use there.
			name: "undefined name past the replacement",
			src:  "package p\n\nfunc F(n int) (y int) {\n\tm := n * 2\n\tif n > 0 { return m + 1 }\n\ty = other\n\treturn\n}\n",
			span: `m + 1`, repl: `0`,
			want: "package p\n\nfunc F(n int) (y int) {\n\tm := n * 2; _ = m\n\tif n > 0 { return 0 }\n\ty = other\n\treturn\n}\n",
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
