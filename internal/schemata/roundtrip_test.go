package schemata

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/szhekpisov/gomutants/internal/patch"
)

// fixture exercises every strategy: bool conditions, arithmetic and
// literals in returns and assignments, an if body, an else body, case
// bodies, a range loop, error wrapping, and a package-level initializer.
const fixture = `package main

import (
	"errors"
	"fmt"
	"strings"
)

var Threshold = 3 + 1

func classify(a, b int) string {
	if a > b && a != 0 {
		return "gt"
	} else {
		if a == b {
			return "eq"
		}
	}
	return "lt"
}

func accumulate(xs []int) int {
	total := 0
	for _, v := range xs {
		if v%2 == 0 {
			total += v * 2
			continue
		}
		total -= v
	}
	for i := 0; i < len(xs); i++ {
		total++
	}
	return total
}

func bucket(n int) string {
	switch {
	case n < 0:
		return "neg"
	case n == 0:
		return "zero"
	default:
		return "pos"
	}
}

func wrap(fail bool) error {
	if !fail {
		return nil
	}
	return fmt.Errorf("outer: %w", errors.New("inner"))
}

func main() {
	var out []string
	out = append(out, strconvItoa(Threshold))
	out = append(out, classify(1, 2), classify(2, 2), classify(3, 2))
	out = append(out, strconvItoa(accumulate([]int{1, 2, 3, 4})))
	out = append(out, bucket(-1), bucket(0), bucket(5))
	out = append(out, fmt.Sprint(wrap(true)), fmt.Sprint(wrap(false)))
	fmt.Println(strings.Join(out, "|"))
}

func strconvItoa(n int) string { return fmt.Sprint(n) }
`

// TestRoundTripMatchesPerMutantPatch is the correctness gate for the
// generator. For every mutant it builds two programs — the source with that
// one mutation byte-patched in, which is what the overlay path compiles,
// and the schematized source with that mutant activated — and requires them
// to behave identically.
//
// The parity rule runs both ways. A mutant the generator schematized must
// produce the same output as its patched twin, and a mutation that does not
// compile on its own must have been declined, because under schemata the
// original survives in the else branch and would otherwise turn a
// NOT VIABLE into a real verdict.
func TestRoundTripMatchesPerMutantPatch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping compile-and-run round trip in short mode")
	}
	fset, file, src, mutants := discover(t, fixture)
	fs, declined, err := Generate(fset, file, src, mutants)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	t.Logf("%d mutants: %d schematized, %d declined", len(mutants), len(fs.Guards), len(declined))

	schematized := map[int]bool{}
	for _, g := range fs.Guards {
		schematized[g.MutantID] = true
	}

	patchDir := newModule(t, "patched")
	schemaDir := newModule(t, "schema")
	write(t, filepath.Join(schemaDir, "main.go"), fs.Source)
	write(t, filepath.Join(schemaDir, HelperFile), helperSource("main"))

	schemaBin := filepath.Join(t.TempDir(), "schema.bin")
	if out, err := build(schemaDir, schemaBin); err != nil {
		t.Fatalf("schematized source does not compile: %v\n%s\n---\n%s", err, out, fs.Source)
	}

	// Baseline: no mutant active must reproduce the unmutated program.
	write(t, filepath.Join(patchDir, "main.go"), src)
	patchBin := filepath.Join(t.TempDir(), "patched.bin")
	if out, err := build(patchDir, patchBin); err != nil {
		t.Fatalf("fixture does not compile: %v\n%s", err, out)
	}
	wantBase, _ := run(patchBin, "")
	gotBase, _ := run(schemaBin, "")
	if gotBase != wantBase {
		t.Fatalf("inactive schema changed behaviour:\n got %q\nwant %q", gotBase, wantBase)
	}

	for _, m := range mutants {
		patched, err := patch.Apply(src, m.StartOffset, m.EndOffset, m.Replacement)
		if err != nil {
			t.Fatalf("mutant %d: %v", m.ID, err)
		}
		write(t, filepath.Join(patchDir, "main.go"), patched)
		_, buildErr := build(patchDir, patchBin)

		if buildErr != nil {
			// The overlay path would report NOT VIABLE. Schemata must not
			// have turned it into a runnable mutant.
			if schematized[m.ID] {
				t.Errorf("mutant %d (%s %q -> %q) does not compile standalone but was schematized",
					m.ID, m.Type, m.Original, m.Replacement)
			}
			continue
		}
		if !schematized[m.ID] {
			continue // Declined; it runs on the overlay path unchanged.
		}
		want, _ := run(patchBin, "")
		got, _ := run(schemaBin, strconv.Itoa(m.ID))
		if got != want {
			t.Errorf("mutant %d (%s %q -> %q):\n schema %q\npatched %q",
				m.ID, m.Type, m.Original, m.Replacement, got, want)
		}
	}
}

func newModule(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "go.mod"), []byte("module "+name+"\n\ngo 1.26\n"))
	return dir
}

func write(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func build(dir, out string) (string, error) {
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = dir
	b, err := cmd.CombinedOutput()
	return string(b), err
}

func run(bin, active string) (string, error) {
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), ActiveEnv+"="+active)
	b, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(b)), err
}
