package report

import (
	"cmp"
	"fmt"
	"os"
	"slices"
	"sort"

	"github.com/gomutants/gomutants/internal/mutator"
)

// Stryker schema v2 — https://github.com/stryker-mutator/mutation-testing-elements
// The schema feeds the shared HTML viewer (mutation-testing-elements) and the
// Stryker Dashboard, which both expect this exact shape.

// readFile is a swappable seam so tests can observe how often each source
// file is read, pinning the per-file caching behavior of WriteStryker.
var readFile = os.ReadFile

type strykerReport struct {
	SchemaVersion string                       `json:"schemaVersion"`
	Thresholds    strykerThresholds            `json:"thresholds"`
	ProjectRoot   string                       `json:"projectRoot,omitempty"`
	Framework     *strykerFramework            `json:"framework,omitempty"`
	Files         map[string]strykerFileResult `json:"files"`
}

type strykerThresholds struct {
	High int `json:"high"`
	Low  int `json:"low"`
}

type strykerFramework struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type strykerFileResult struct {
	Language string                `json:"language"`
	Source   string                `json:"source"`
	Mutants  []strykerMutantResult `json:"mutants"`
}

type strykerMutantResult struct {
	ID          string          `json:"id"`
	MutatorName string          `json:"mutatorName"`
	Location    strykerLocation `json:"location"`
	Status      string          `json:"status"`
	Replacement string          `json:"replacement,omitempty"`
}

type strykerLocation struct {
	Start strykerPosition `json:"start"`
	End   strykerPosition `json:"end"`
}

type strykerPosition struct {
	Line   int `json:"line"`
	Column int `json:"column"`
}

// WriteStryker writes a Stryker v2 mutation-testing report to path. mutants
// must reference real source files on disk (Mutant.File absolute paths) so the
// emitter can read source and resolve byte offsets to line/column positions.
//
// frameworkVersion is recorded under .framework.version; pass the running
// gomutants version.
func WriteStryker(path string, mutants []mutator.Mutant, projectDir, frameworkVersion string) error {
	rep, err := buildStrykerReport(mutants, projectDir, frameworkVersion)
	if err != nil {
		return err
	}
	return writeJSONFile(path, rep)
}

// buildStrykerReport constructs the in-memory Stryker v2 report from the
// mutant list. Shared by WriteStryker (marshal + write JSON) and WriteHTML
// (marshal + inject into the HTML viewer template).
func buildStrykerReport(mutants []mutator.Mutant, projectDir, frameworkVersion string) (strykerReport, error) {
	files := make(map[string]strykerFileResult)
	// Cache a per-file index so each file is read once and offset->line/col
	// lookups are O(log lines) instead of O(file_size) per mutant.
	indexCache := make(map[string]*fileIndex)

	for _, m := range mutants {
		idx, ok := indexCache[m.File]
		if !ok {
			b, err := readFile(m.File)
			if err != nil {
				return strykerReport{}, fmt.Errorf("reading %s for Stryker report: %w", m.File, err)
			}
			idx = newFileIndex(b)
			indexCache[m.File] = idx
		}

		startLine, startCol := m.Line, m.Col
		endLine, endCol := idx.lineCol(m.EndOffset)
		// Some mutators don't populate EndOffset (zero-valued) and a stale
		// offset can resolve before start; in either case fall back to the
		// visible Original span on the start line.
		if m.EndOffset == 0 || endLine < startLine {
			endLine = startLine
			endCol = startCol + len(m.Original)
		}

		f, exists := files[m.RelFile]
		if !exists {
			f = strykerFileResult{
				Language: "go",
				Source:   string(idx.src),
			}
		}

		f.Mutants = append(f.Mutants, strykerMutantResult{
			ID:          m.StableID,
			MutatorName: string(m.Type),
			Location: strykerLocation{
				Start: strykerPosition{Line: startLine, Column: startCol},
				End:   strykerPosition{Line: endLine, Column: endCol},
			},
			Status:      strykerStatus(m.Status),
			Replacement: m.Replacement,
		})

		files[m.RelFile] = f
	}

	// Sort mutants within each file by (line, col, id) so the output is
	// deterministic regardless of dispatch order. The id tiebreak is a
	// lexicographic compare of stable IDs; it only decides ties between
	// mutants sharing a start position, where any total order will do.
	for k, f := range files {
		slices.SortStableFunc(f.Mutants, func(a, b strykerMutantResult) int {
			return cmp.Or(
				cmp.Compare(a.Location.Start.Line, b.Location.Start.Line),
				cmp.Compare(a.Location.Start.Column, b.Location.Start.Column),
				cmp.Compare(a.ID, b.ID),
			)
		})
		files[k] = f
	}

	return strykerReport{
		SchemaVersion: "2",
		Thresholds:    strykerThresholds{High: 80, Low: 60},
		ProjectRoot:   projectDir,
		Framework:     &strykerFramework{Name: "gomutants", Version: frameworkVersion},
		Files:         files,
	}, nil
}

// strykerStatus maps gomutants status values to the Stryker schema enum.
// Unknown statuses fall through to "Pending" so the output stays valid.
func strykerStatus(s mutator.MutantStatus) string {
	switch s {
	case mutator.StatusKilled:
		return "Killed"
	case mutator.StatusLived:
		return "Survived"
	case mutator.StatusNotCovered:
		return "NoCoverage"
	case mutator.StatusNotViable:
		return "CompileError"
	case mutator.StatusTimedOut:
		return "Timeout"
	case mutator.StatusInfraError:
		return "RuntimeError"
	case mutator.StatusEquivalent:
		// Stryker's schema has no "Equivalent"; "Ignored" is its bucket for
		// mutants deliberately excluded from scoring, which matches a
		// compiler-proven equivalent mutant.
		return "Ignored"
	default:
		return "Pending"
	}
}

// fileIndex pre-computes line-start byte offsets for one file so byte-offset
// → (line, column) lookups run in O(log lines) instead of O(file_size). Build
// once per file; reuse across all mutants in that file.
type fileIndex struct {
	src        []byte
	lineStarts []int // lineStarts[i] is the byte offset of line i+1's first byte
}

func newFileIndex(src []byte) *fileIndex {
	starts := []int{0}
	for i, b := range src {
		if b == '\n' {
			starts = append(starts, i+1)
		}
	}
	return &fileIndex{src: src, lineStarts: starts}
}

// lineCol returns the 1-indexed (line, column) of byte offset off. Column
// counts bytes, not runes — Stryker consumes bytes consistently across
// emitters.
func (fi *fileIndex) lineCol(off int) (line, col int) {
	off = min(max(off, 0), len(fi.src))
	i := max(sort.Search(len(fi.lineStarts), func(i int) bool { return fi.lineStarts[i] > off })-1, 0)
	return i + 1, off - fi.lineStarts[i] + 1
}
