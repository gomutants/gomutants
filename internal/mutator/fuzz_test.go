package mutator_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"

	"github.com/gomutants/gomutants/internal/mutator"
	"github.com/gomutants/gomutants/internal/patch"
)

// FuzzMutatorsKeepSourceParseable feeds arbitrary Go source to every
// registered mutator and checks each candidate it emits: the byte range
// lies inside the source, and applying it the way the runner does leaves a
// file that still parses. A mutant that breaks the syntax can never
// compile, so it would read NOT VIABLE however good the tests are.
func FuzzMutatorsKeepSourceParseable(f *testing.F) {
	seeds, _ := filepath.Glob("../../testdata/*/*.go")
	for _, path := range seeds {
		src, err := os.ReadFile(path)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(src)
	}
	for _, src := range []string{
		"package p\nfunc f() int { return 1 + 2 - 3 * 4 / 5 % 6 }\n",
		"package p\nfunc f(a, b int) bool { return a < b && !(a >= b) || a == b }\n",
		"package p\nfunc f(x uint) { x &= 1; x |= 2; x <<= 3; x ^= 4; x &^= 5; x++; x-- }\n",
		"package p\nfunc f(xs []int) { for i := 0; i < len(xs); i++ { if xs[i] > 0 { continue } else { break } } }\n",
		"package p\nfunc f(xs []int) { for _, v := range xs { switch v { case 1: _ = -v; default: } } }\n",
		"package p\nimport \"fmt\"\nfunc f(err error) (int, error) { return 0, fmt.Errorf(\"x: %w\", err) }\n",
		"package p\nfunc f() (float64, bool, string) { return 1.5e2, true, \"s\" }\n",
		"package p\nconst c = 0x1F + 0o7 + 0b1 + 1_000 + 'a'\n",
		"package p\nfunc f[T ~int | ~uint](x T) T { return -x }\n",
	} {
		f.Add([]byte(src))
	}

	reg := mutator.NewRegistry()
	f.Fuzz(func(t *testing.T, src []byte) {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "fuzz.go", src, parser.ParseComments)
		if err != nil {
			t.Skip()
		}
		for _, m := range reg.Mutators() {
			for _, c := range m.Discover(fset, file, src) {
				if c.StartOffset < 0 || c.StartOffset > c.EndOffset || c.EndOffset > len(src) {
					t.Fatalf("%s: range [%d:%d) outside %d-byte source", c.Type, c.StartOffset, c.EndOffset, len(src))
				}
				mutated, err := patch.ApplyMutant("fuzz.go", src, c.StartOffset, c.EndOffset, c.Replacement)
				if err != nil {
					t.Fatalf("%s: %v", c.Type, err)
				}
				if _, err := parser.ParseFile(token.NewFileSet(), "fuzz.go", mutated, 0); err != nil {
					t.Fatalf("%s: %q -> %q leaves source that does not parse: %v\n%s",
						c.Type, src[c.StartOffset:c.EndOffset], c.Replacement, err, mutated)
				}
			}
		}
	})
}
