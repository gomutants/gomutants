package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gomutants/gomutants/internal/mutator"
)

// Report is the gremlins-compatible JSON report structure.
//
// MutantsCached and MutantsSuppressed are gomutants-specific (additive,
// ignored by gremlins consumers). MutantsCached counts mutants whose
// status was sourced from the incremental-analysis cache.
// MutantsSuppressed counts mutants dropped by `// gomutants:disable*`
// directives or by --exclude-calls — these don't appear in
// Files[].Mutations and are excluded from MutantsTotal and from the
// TestEfficacy and MutationsCoverage denominators.
// MutantsSuppressedByCalls breaks out the --exclude-calls share of that
// total.
// MutantsEquivalent counts surviving mutants proven equivalent to the
// original by Trivial Compiler Equivalence (--detect-equivalent). They
// stay in MutantsTotal but count as neither KILLED nor LIVED, so they
// drop out of the TestEfficacy denominator — an equivalent mutant is not
// a test gap.
// MutantsInfraError counts mutants for which environmental resource or I/O
// failure prevented a verdict. They stay in MutantsTotal and mutation
// coverage, but count as neither KILLED nor LIVED for TestEfficacy.
// MutantsRechecked counts mutants their covering tests didn't kill that
// were then run against every test suite that can kill them, and
// MutantsRecheckKilled those of them that re-check killed: survivors the
// per-test coverage map would have reported, a sign of tests that depend
// on running after others.
type Report struct {
	GoModule          string       `json:"go_module"`
	Files             []FileReport `json:"files"`
	TestEfficacy      float64      `json:"test_efficacy"`
	MutationsCoverage float64      `json:"mutations_coverage"`
	MutantsTotal      int          `json:"mutants_total"`
	MutantsKilled     int          `json:"mutants_killed"`
	MutantsLived      int          `json:"mutants_lived"`
	MutantsNotViable  int          `json:"mutants_not_viable"`
	MutantsNotCovered int          `json:"mutants_not_covered"`
	MutantsTimedOut   int          `json:"mutants_timed_out,omitempty"`
	MutantsInfraError int          `json:"mutants_infra_error,omitempty"`
	MutantsCached     int          `json:"mutants_cached,omitempty"`
	MutantsSuppressed int          `json:"mutants_suppressed,omitempty"`
	// MutantsSuppressedByCalls is the --exclude-calls share of
	// MutantsSuppressed, not an additional bucket. Set by the caller after
	// Generate, since only it knows the split.
	MutantsSuppressedByCalls int            `json:"mutants_suppressed_by_calls,omitempty"`
	MutantsEquivalent        int            `json:"mutants_equivalent,omitempty"`
	MutantsRechecked         int            `json:"mutants_rechecked,omitempty"`
	MutantsRecheckKilled     int            `json:"mutants_recheck_killed,omitempty"`
	ElapsedTime              float64        `json:"elapsed_time"`
	MutatorStatistics        map[string]int `json:"mutator_statistics"`
}

// FileReport groups mutations by file.
type FileReport struct {
	FileName  string           `json:"file_name"`
	Mutations []MutationReport `json:"mutations"`
}

// MutationReport is a single mutation entry.
//
// ID is gomutants-specific (additive, ignored by gremlins consumers): a
// cross-run handle of the form "rel_file:anchor:TYPE#n" — see
// mutator.Mutant.StableID. It is stable against line shifts and edits to
// other functions, so reports from successive runs can be diffed entry by
// entry. It changes when the mutant's own function is renamed, or when a
// same-type mutation point is added or removed earlier in that function.
type MutationReport struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Status      string `json:"status"`
	Line        int    `json:"line"`
	Column      int    `json:"column"`
	Original    string `json:"original,omitempty"`
	Replacement string `json:"replacement,omitempty"`
}

// count adds m to the report's totals: overall, by status, and the
// cache and re-check counts. Split out of Generate to keep it within the
// cognitive-complexity budget.
func (r *Report) count(m mutator.Mutant) {
	r.MutantsTotal++

	if m.FromCache {
		r.MutantsCached++
	}
	if m.Rechecked {
		r.MutantsRechecked++
		if m.Status == mutator.StatusKilled {
			r.MutantsRecheckKilled++
		}
	}

	switch m.Status {
	case mutator.StatusKilled:
		r.MutantsKilled++
	case mutator.StatusLived:
		r.MutantsLived++
	case mutator.StatusNotViable:
		r.MutantsNotViable++
	case mutator.StatusNotCovered:
		r.MutantsNotCovered++
	case mutator.StatusTimedOut:
		r.MutantsTimedOut++
	case mutator.StatusInfraError:
		r.MutantsInfraError++
	case mutator.StatusEquivalent:
		r.MutantsEquivalent++
	}
}

// Generate builds a Report from the list of mutants. suppressedCount
// is the number of mutants dropped before this point by
// `// gomutants:disable*` directives or by --exclude-calls — they are not
// in `mutants` and contribute to no count except `MutantsSuppressed`.
// Callers that know the split set MutantsSuppressedByCalls afterwards.
func Generate(mutants []mutator.Mutant, goModule string, elapsed time.Duration, suppressedCount int) *Report {
	r := &Report{
		GoModule:          goModule,
		ElapsedTime:       elapsed.Seconds(),
		MutatorStatistics: make(map[string]int),
		MutantsSuppressed: suppressedCount,
	}

	fileMap := make(map[string][]MutationReport)

	for _, m := range mutants {
		r.count(m)

		// Mutator statistics use lower_snake_case keys.
		statKey := strings.ToLower(string(m.Type))
		if m.Status != mutator.StatusNotCovered {
			r.MutatorStatistics[statKey]++
		}

		mr := MutationReport{
			ID:          m.StableID,
			Type:        string(m.Type),
			Status:      m.Status.String(),
			Line:        m.Line,
			Column:      m.Col,
			Original:    m.Original,
			Replacement: m.Replacement,
		}
		fileMap[m.RelFile] = append(fileMap[m.RelFile], mr)
	}

	// Build file reports in the order files appear.
	seen := make(map[string]bool)
	for _, m := range mutants {
		if seen[m.RelFile] {
			continue
		}
		seen[m.RelFile] = true
		r.Files = append(r.Files, FileReport{
			FileName:  m.RelFile,
			Mutations: fileMap[m.RelFile],
		})
	}

	// Compute efficacy and coverage.
	tested := r.MutantsKilled + r.MutantsLived
	if tested > 0 {
		r.TestEfficacy = float64(r.MutantsKilled) / float64(tested) * 100
	}
	if r.MutantsTotal > 0 {
		r.MutationsCoverage = float64(r.MutantsTotal-r.MutantsNotCovered) / float64(r.MutantsTotal) * 100
	}

	return r
}

// marshalJSON is swappable for testing.
var marshalJSON = json.Marshal

// WriteJSON writes the report to a file.
func WriteJSON(r *Report, path string) error {
	return writeJSONFile(path, r)
}

// writeJSONFile marshals v as JSON and writes it to path, creating parent
// directories as needed. Shared by WriteJSON and WriteStryker.
func writeJSONFile(path string, v any) error {
	data, err := marshalJSON(v)
	if err != nil {
		return err
	}
	return writeFile(path, data)
}

// writeFile writes data to path, creating parent directories as needed.
// Shared by writeJSONFile and WriteHTML.
func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
