package discover

import (
	"path/filepath"

	"github.com/gomutants/gomutants/internal/coverage"
	"github.com/gomutants/gomutants/internal/mutator"
)

// FilterByCoverage marks mutants whose positions are not covered as StatusNotCovered.
// It builds a mapping from coverage-profile paths (module-relative) to absolute paths
// to bridge the two path formats.
//
// A position is considered covered if (a) it sits inside a block with
// Count > 0, or (b) the file has at least one Count > 0 block AND the
// position sits outside every block in the profile. Case (b) lets us
// test mutants on positions Go's coverage tool doesn't instrument
// (package-level const/var initializers, select-case receive
// expressions, gaps between adjacent blocks on the same line) as long
// as the file is otherwise tested. Positions inside a Count == 0 block
// remain uncovered — the coverage tool saw them and they did not run.
func FilterByCoverage(mutants []mutator.Mutant, profile *coverage.Profile, pkgs []Package, goModule string) {
	// Build mapping: absolute path → coverage profile path.
	// Coverage uses "module/pkg/file.go", we need to map to "/abs/path/file.go".
	absToProfile := make(map[string]string)
	for _, pkg := range pkgs {
		for _, f := range pkg.GoFiles {
			absPath := filepath.Join(pkg.Dir, f)
			profilePath := pkg.ImportPath + "/" + f
			absToProfile[absPath] = profilePath
		}
	}

	for i := range mutants {
		if mutants[i].Status != mutator.StatusPending {
			continue
		}
		profilePath, ok := absToProfile[mutants[i].File]
		if !ok {
			mutants[i].Status = mutator.StatusNotCovered
			continue
		}
		if profile.IsCovered(profilePath, mutants[i].Line, mutants[i].Col) {
			continue
		}
		// Treat uninstrumented positions in tested files as covered.
		if profile.HasCoveredBlock(profilePath) && !profile.IsInAnyBlock(profilePath, mutants[i].Line, mutants[i].Col) {
			continue
		}
		mutants[i].Status = mutator.StatusNotCovered
	}
}
