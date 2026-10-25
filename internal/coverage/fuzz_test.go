package coverage

import (
	"fmt"
	"testing"
)

// FuzzParseBytes checks that no profile, however malformed, panics the
// parser, and that every block it keeps reads back the same once written
// out in the profile's own format.
func FuzzParseBytes(f *testing.F) {
	f.Add([]byte("mode: set\nexample.com/m/a.go:3.14,5.2 2 1\n"))
	f.Add([]byte("mode: atomic\nC:\\src\\a.go:1.1,1.20 1 0\nexample.com/m/b.go:10.5,12.3 3 7\n"))
	f.Add([]byte("mode: count\nbad line\na.go:1.1 1 1\na.go:1,2.3 1 1\na.go:x.1,2.3 1 1\n"))

	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := ParseBytes(data)
		if err != nil {
			return
		}
		for _, b := range p.blocks {
			line := fmt.Sprintf("%s:%d.%d,%d.%d %d %d",
				b.File, b.StartLine, b.StartCol, b.EndLine, b.EndCol, b.NumStmt, b.Count)
			got, err := parseLine(line)
			if err != nil {
				t.Fatalf("parseLine(%q): %v", line, err)
			}
			if got != b {
				t.Fatalf("parseLine(%q) = %+v, want %+v", line, got, b)
			}
		}
	})
}
