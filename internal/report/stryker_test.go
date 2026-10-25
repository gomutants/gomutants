package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gomutants/gomutants/internal/mutator"
)

func TestStrykerStatusMapping(t *testing.T) {
	cases := []struct {
		in   mutator.MutantStatus
		want string
	}{
		{mutator.StatusKilled, "Killed"},
		{mutator.StatusLived, "Survived"},
		{mutator.StatusNotCovered, "NoCoverage"},
		{mutator.StatusNotViable, "CompileError"},
		{mutator.StatusTimedOut, "Timeout"},
		{mutator.StatusInfraError, "RuntimeError"},
		{mutator.StatusEquivalent, "Ignored"},
		{mutator.StatusPending, "Pending"},
	}
	for _, tc := range cases {
		got := strykerStatus(tc.in)
		if got != tc.want {
			t.Errorf("strykerStatus(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFileIndexLineCol(t *testing.T) {
	idx := newFileIndex([]byte("ab\ncde\nf"))
	cases := []struct {
		off               int
		wantLine, wantCol int
	}{
		{0, 1, 1}, // 'a'
		{1, 1, 2}, // 'b'
		{2, 1, 3}, // '\n'
		{3, 2, 1}, // 'c'
		{5, 2, 3}, // 'e'
		{6, 2, 4}, // '\n' on line 2
		{7, 3, 1}, // 'f'
		{8, 3, 2}, // EOF
		{-1, 1, 1},
		{99, 3, 2},
	}
	for _, tc := range cases {
		gotLine, gotCol := idx.lineCol(tc.off)
		if gotLine != tc.wantLine || gotCol != tc.wantCol {
			t.Errorf("lineCol(%d) = (%d,%d), want (%d,%d)", tc.off, gotLine, gotCol, tc.wantLine, tc.wantCol)
		}
	}
}

// TestWriteStryker_RoundTrip writes a report and re-parses it as the schema
// types to confirm the on-disk JSON is well-formed and carries the data
// downstream consumers (HTML viewer, dashboard) need.
func TestWriteStryker_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.go")
	src := "package x\n\nfunc add(a, b int) int { return a + b }\n"
	if err := os.WriteFile(srcPath, []byte(src), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	plusOff := 0
	for i := 0; i < len(src); i++ {
		if src[i] == '+' {
			plusOff = i
			break
		}
	}
	plusLine, plusCol := newFileIndex([]byte(src)).lineCol(plusOff)

	mutants := []mutator.Mutant{
		{
			ID:          1,
			Type:        mutator.ArithmeticBase,
			File:        srcPath,
			RelFile:     "src.go",
			Line:        plusLine,
			Col:         plusCol,
			Original:    "+",
			Replacement: "-",
			StartOffset: plusOff,
			EndOffset:   plusOff + 1,
			Status:      mutator.StatusLived,
		},
		{
			ID:          2,
			Type:        mutator.ArithmeticBase,
			File:        srcPath,
			RelFile:     "src.go",
			Line:        plusLine,
			Col:         plusCol,
			Original:    "+",
			Replacement: "*",
			StartOffset: plusOff,
			EndOffset:   plusOff + 1,
			Status:      mutator.StatusKilled,
		},
	}

	out := filepath.Join(dir, "stryker.json")
	if err := WriteStryker(out, mutants, "/proj", "0.1.0"); err != nil {
		t.Fatalf("WriteStryker: %v", err)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	var got strykerReport
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.SchemaVersion != "2" {
		t.Errorf("SchemaVersion=%q, want 2", got.SchemaVersion)
	}
	if got.Framework == nil || got.Framework.Name != "gomutants" || got.Framework.Version != "0.1.0" {
		t.Errorf("Framework=%+v", got.Framework)
	}
	if got.Thresholds.High != 80 || got.Thresholds.Low != 60 {
		t.Errorf("Thresholds=%+v", got.Thresholds)
	}
	if got.ProjectRoot != "/proj" {
		t.Errorf("ProjectRoot=%q", got.ProjectRoot)
	}

	file, ok := got.Files["src.go"]
	if !ok {
		t.Fatalf("missing src.go in files: %+v", got.Files)
	}
	if file.Language != "go" {
		t.Errorf("Language=%q, want go", file.Language)
	}
	if file.Source != src {
		t.Errorf("Source not preserved verbatim")
	}
	if len(file.Mutants) != 2 {
		t.Fatalf("Mutants=%d, want 2", len(file.Mutants))
	}
	if file.Mutants[0].Status != "Survived" {
		t.Errorf("Mutants[0].Status=%q, want Survived", file.Mutants[0].Status)
	}
	if file.Mutants[1].Status != "Killed" {
		t.Errorf("Mutants[1].Status=%q, want Killed", file.Mutants[1].Status)
	}
	if file.Mutants[0].Replacement != "-" || file.Mutants[1].Replacement != "*" {
		t.Errorf("Replacement preservation broken: %+v", file.Mutants)
	}

	// Location end should be derived from byte offsets on the source.
	loc := file.Mutants[0].Location
	if loc.Start.Line != 3 {
		t.Errorf("Start.Line=%d, want 3", loc.Start.Line)
	}
	if loc.End.Line != 3 || loc.End.Column-loc.Start.Column != 1 {
		t.Errorf("End=%+v should be one column past Start=%+v on the same line", loc.End, loc.Start)
	}
}

func TestWriteStryker_MultiLineMutant(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "blk.go")
	src := "if x {\n  doStuff()\n  more()\n}\n"
	if err := os.WriteFile(srcPath, []byte(src), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// The body spans byte offsets covering "{...\n}" (multi-line).
	startOff := 5          // '{'
	endOff := len(src) - 1 // '}' is at len-2; EndOffset is exclusive.

	mutants := []mutator.Mutant{
		{
			ID: 1, Type: mutator.BranchIf, File: srcPath, RelFile: "blk.go",
			Line: 1, Col: 6,
			Original: "{\n  doStuff()\n  more()\n}", Replacement: "{ _ = 0 }",
			StartOffset: startOff, EndOffset: endOff,
			Status: mutator.StatusLived,
		},
	}

	out := filepath.Join(dir, "s.json")
	if err := WriteStryker(out, mutants, "/p", "0.1.0"); err != nil {
		t.Fatalf("WriteStryker: %v", err)
	}
	data, _ := os.ReadFile(out)
	var got strykerReport
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	loc := got.Files["blk.go"].Mutants[0].Location
	if loc.End.Line <= loc.Start.Line {
		t.Errorf("multi-line mutant end (%+v) should be on a later line than start (%+v)", loc.End, loc.Start)
	}
}

func TestWriteStryker_GroupsByFile(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.go", "b.go"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("package x\n"), 0o644); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	mutants := []mutator.Mutant{
		{ID: 1, Type: mutator.ArithmeticBase, File: filepath.Join(dir, "a.go"), RelFile: "a.go", Line: 1, Col: 1, Status: mutator.StatusKilled},
		{ID: 2, Type: mutator.ArithmeticBase, File: filepath.Join(dir, "b.go"), RelFile: "b.go", Line: 1, Col: 1, Status: mutator.StatusLived},
		{ID: 3, Type: mutator.ArithmeticBase, File: filepath.Join(dir, "a.go"), RelFile: "a.go", Line: 1, Col: 2, Status: mutator.StatusKilled},
	}
	out := filepath.Join(dir, "s.json")
	if err := WriteStryker(out, mutants, "/p", "0.1.0"); err != nil {
		t.Fatalf("WriteStryker: %v", err)
	}
	data, _ := os.ReadFile(out)
	var got strykerReport
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(got.Files["a.go"].Mutants) != 2 {
		t.Errorf("a.go mutants=%d, want 2", len(got.Files["a.go"].Mutants))
	}
	if len(got.Files["b.go"].Mutants) != 1 {
		t.Errorf("b.go mutants=%d, want 1", len(got.Files["b.go"].Mutants))
	}
}

func TestWriteStryker_PropagatesReadError(t *testing.T) {
	dir := t.TempDir()
	mutants := []mutator.Mutant{
		{ID: 1, Type: mutator.ArithmeticBase,
			File: filepath.Join(dir, "does-not-exist.go"), RelFile: "does-not-exist.go",
			Line: 1, Col: 1, Status: mutator.StatusKilled},
	}
	err := WriteStryker(filepath.Join(dir, "out.json"), mutants, "/p", "0.1.0")
	if err == nil {
		t.Fatal("expected error when mutant references unreadable file")
	}
	if !strings.Contains(err.Error(), "does-not-exist.go") {
		t.Errorf("error %q should mention the missing file", err)
	}
}

// TestWriteStryker_ReadsEachFileOnce pins the per-file cache: dropping the
// `indexCache[m.File] = idx` assignment would still produce correct output but
// re-read every file once per mutant — observable only by counting reads.
func TestWriteStryker_ReadsEachFileOnce(t *testing.T) {
	dir := t.TempDir()
	srcA := filepath.Join(dir, "a.go")
	srcB := filepath.Join(dir, "b.go")
	for _, p := range []string{srcA, srcB} {
		if err := os.WriteFile(p, []byte("package x\n"), 0o644); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}

	calls := map[string]int{}
	orig := readFile
	t.Cleanup(func() { readFile = orig })
	readFile = func(p string) ([]byte, error) {
		calls[p]++
		return orig(p)
	}

	mutants := []mutator.Mutant{
		{ID: 1, Type: mutator.ArithmeticBase, File: srcA, RelFile: "a.go", Line: 1, Col: 1, Status: mutator.StatusKilled},
		{ID: 2, Type: mutator.ArithmeticBase, File: srcA, RelFile: "a.go", Line: 1, Col: 2, Status: mutator.StatusKilled},
		{ID: 3, Type: mutator.ArithmeticBase, File: srcB, RelFile: "b.go", Line: 1, Col: 1, Status: mutator.StatusKilled},
		{ID: 4, Type: mutator.ArithmeticBase, File: srcA, RelFile: "a.go", Line: 1, Col: 3, Status: mutator.StatusKilled},
	}
	out := filepath.Join(dir, "s.json")
	if err := WriteStryker(out, mutants, "/p", "0.1.0"); err != nil {
		t.Fatalf("WriteStryker: %v", err)
	}
	if calls[srcA] != 1 || calls[srcB] != 1 {
		t.Errorf("expected one read per unique file, got %v", calls)
	}
}

// TestWriteStryker_EndOffsetFallbacks pins the three branches of the
// end-position guard at stryker.go:86. Each scenario distinguishes the live
// fallback from any of the mutations the tree produces (dropping the guard,
// flipping || to &&, weakening < to <=, etc).
func TestWriteStryker_EndOffsetFallbacks(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "x.go")
	src := "package x\n\n\n\nfunc f() { var ab = 1; _ = ab }\n"
	if err := os.WriteFile(srcPath, []byte(src), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	makeMutant := func(line, col, endOff int, orig string) mutator.Mutant {
		return mutator.Mutant{
			ID: 1, Type: mutator.ArithmeticBase, File: srcPath, RelFile: "x.go",
			Line: line, Col: col, Original: orig,
			EndOffset: endOff, Status: mutator.StatusLived,
		}
	}

	t.Run("EndOffset==0 falls back to startCol+len(Original)", func(t *testing.T) {
		// Without the fallback, lineCol(0) returns (1,1), so end would land on
		// line 1 column 1 instead of the start line.
		m := makeMutant(5, 10, 0, "ab")
		got := writeAndReadFirst(t, []mutator.Mutant{m}, "x.go")
		if got.Location.End.Line != 5 || got.Location.End.Column != 12 {
			t.Errorf("end=(%d,%d), want (5,12)", got.Location.End.Line, got.Location.End.Column)
		}
	})

	t.Run("EndOffset==0 fires the fallback even when startLine is 1", func(t *testing.T) {
		// Pins the `m.EndOffset == 0` arm of the OR specifically: with
		// startLine=1, `endLine < startLine` is false, so only the EndOffset
		// check distinguishes the live behavior from a mutation that drops
		// it.
		m := makeMutant(1, 4, 0, "ab")
		got := writeAndReadFirst(t, []mutator.Mutant{m}, "x.go")
		if got.Location.End.Line != 1 || got.Location.End.Column != 6 {
			t.Errorf("end=(%d,%d), want (1,6)", got.Location.End.Line, got.Location.End.Column)
		}
	})

	t.Run("stale EndOffset before start triggers fallback", func(t *testing.T) {
		// EndOffset=2 resolves to line 1; start is line 5. Without the
		// `endLine < startLine` arm of the guard, end would land on line 1.
		m := makeMutant(5, 10, 2, "ab")
		got := writeAndReadFirst(t, []mutator.Mutant{m}, "x.go")
		if got.Location.End.Line != 5 || got.Location.End.Column != 12 {
			t.Errorf("end=(%d,%d), want (5,12)", got.Location.End.Line, got.Location.End.Column)
		}
	})

	t.Run("valid EndOffset uses the lineCol-derived position", func(t *testing.T) {
		// "ab" appears in the source. Compute its real offsets and assert the
		// end is the lineCol value, not startCol+len. With < weakened to <=,
		// the fallback would fire and clobber the real value.
		startOff := strings.Index(src, "ab")
		if startOff < 0 {
			t.Fatal("setup: 'ab' not found in src")
		}
		startLine, startCol := newFileIndex([]byte(src)).lineCol(startOff)
		// Use a 1-character Original so startCol+len differs from the real
		// lineCol-derived end (which spans 2 chars).
		m := makeMutant(startLine, startCol, startOff+2, "a")
		got := writeAndReadFirst(t, []mutator.Mutant{m}, "x.go")
		if got.Location.End.Line != startLine {
			t.Errorf("end.Line=%d, want %d", got.Location.End.Line, startLine)
		}
		if got.Location.End.Column == startCol+1 {
			t.Errorf("end.Column=%d matches startCol+len(Original); fallback fired when it shouldn't", got.Location.End.Column)
		}
		if got.Location.End.Column != startCol+2 {
			t.Errorf("end.Column=%d, want %d", got.Location.End.Column, startCol+2)
		}
	})
}

// TestWriteStryker_SortOrdering pins the per-file (line, col, id) sort. With
// a non-sorted dispatch order, output must reach a single canonical order so
// any mutation that drops the sort, swaps the comparator, or flips operands
// is observable.
func TestWriteStryker_SortOrdering(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "s.go")
	if err := os.WriteFile(srcPath, []byte("package x\n"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	mk := func(stableID string, line, col int) mutator.Mutant {
		return mutator.Mutant{
			StableID: stableID, Type: mutator.ArithmeticBase, File: srcPath, RelFile: "s.go",
			Line: line, Col: col, Status: mutator.StatusKilled,
		}
	}
	// Dispatch order is intentionally jumbled. The id tiebreak is a
	// lexicographic compare of stable IDs.
	mutants := []mutator.Mutant{
		mk("s.go:g:ARITHMETIC_BASE#3", 5, 1),
		mk("s.go:f:ARITHMETIC_BASE#1", 2, 3),
		mk("s.go:f:ARITHMETIC_BASE#2", 2, 1),
		mk("s.go:f:ARITHMETIC_BASE#5", 2, 1),
		mk("s.go:g:ARITHMETIC_BASE#4", 5, 1),
	}

	out := filepath.Join(dir, "s.json")
	if err := WriteStryker(out, mutants, "/p", "0.1.0"); err != nil {
		t.Fatalf("WriteStryker: %v", err)
	}
	data, _ := os.ReadFile(out)
	var got strykerReport
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	gotIDs := []string{}
	for _, m := range got.Files["s.go"].Mutants {
		gotIDs = append(gotIDs, m.ID)
	}
	// (line 2, col 1, f#2), (2, 1, f#5), (2, 3, f#1), (5, 1, g#3), (5, 1, g#4).
	want := []string{
		"s.go:f:ARITHMETIC_BASE#2",
		"s.go:f:ARITHMETIC_BASE#5",
		"s.go:f:ARITHMETIC_BASE#1",
		"s.go:g:ARITHMETIC_BASE#3",
		"s.go:g:ARITHMETIC_BASE#4",
	}
	if !slices.Equal(gotIDs, want) {
		t.Errorf("ID order = %v, want %v", gotIDs, want)
	}
}

func writeAndReadFirst(t *testing.T, mutants []mutator.Mutant, relFile string) strykerMutantResult {
	t.Helper()
	out := filepath.Join(t.TempDir(), "out.json")
	if err := WriteStryker(out, mutants, "/p", "0.1.0"); err != nil {
		t.Fatalf("WriteStryker: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var got strykerReport
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(got.Files[relFile].Mutants) == 0 {
		t.Fatalf("no mutants in %s", relFile)
	}
	return got.Files[relFile].Mutants[0]
}

func TestWriteStryker_Empty(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "s.json")
	if err := WriteStryker(out, nil, "/p", "0.1.0"); err != nil {
		t.Fatalf("WriteStryker: %v", err)
	}
	data, _ := os.ReadFile(out)
	var got strykerReport
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.SchemaVersion != "2" {
		t.Errorf("SchemaVersion=%q", got.SchemaVersion)
	}
	if len(got.Files) != 0 {
		t.Errorf("Files should be empty, got %d", len(got.Files))
	}
}

func TestWriteStryker_UsesStableID(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "s.go")
	if err := os.WriteFile(srcPath, []byte("package x\n"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// ID and StableID disagree on purpose: the Stryker id must be the
	// cross-run handle, not the run-local sequential index, so the HTML
	// viewer and the dashboard can track a mutant between runs.
	mutants := []mutator.Mutant{
		{
			ID:       7,
			StableID: "s.go:Add:ARITHMETIC_BASE#1",
			Type:     mutator.ArithmeticBase,
			File:     srcPath, RelFile: "s.go",
			Line: 1, Col: 1, Status: mutator.StatusLived,
		},
	}

	got := writeAndReadFirst(t, mutants, "s.go")
	if got.ID != "s.go:Add:ARITHMETIC_BASE#1" {
		t.Errorf("ID=%q, want the stable ID", got.ID)
	}
	if got.ID == "7" {
		t.Errorf("ID fell back to the sequential Mutant.ID")
	}
}
