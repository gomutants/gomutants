package patch

import (
	"go/parser"
	"go/token"
	"testing"
)

// TestRepairNoIdentifierAtOffset pins that repair declines an offset no
// identifier starts at rather than indexing an empty path. go/types reports
// every unused import or local at one, so only this can reach it.
func TestRepairNoIdentifierAtOffset(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "p.go", "package p\n\nfunc F() {}\n", 0)
	if err != nil {
		t.Fatal(err)
	}
	if e := repair(fset, file, len("package p\n\nfunc F(")); e != (edit{}) {
		t.Errorf("repair at `)` = %+v, want the zero edit", e)
	}
}

func TestGuessName(t *testing.T) {
	for path, want := range map[string]string{
		"strconv":                     "strconv",
		"example.com/yaml/v3":         "yaml",
		"example.com/v2":              "example.com",
		"gopkg.in/yaml.v3":            "yaml",
		"example.com/a.b":             "a.b",
		"github.com/mattn/go-sqlite3": "sqlite3",
		"example.com/yaml-go":         "yaml",
	} {
		if got := guessName(path); got != want {
			t.Errorf("guessName(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestIsMajorVersion(t *testing.T) {
	for s, want := range map[string]bool{
		"v2": true, "v10": true, "v": false, "": false, "2": false, "x2": false, "vx": false,
	} {
		if got := isMajorVersion(s); got != want {
			t.Errorf("isMajorVersion(%q) = %v, want %v", s, got, want)
		}
	}
}
