package main

import (
	"go/token"
	"time"

	"github.com/szhekpisov/gomutants/internal/cache"
	"github.com/szhekpisov/gomutants/internal/config"
	"github.com/szhekpisov/gomutants/internal/coverage"
	"github.com/szhekpisov/gomutants/internal/discover"
	"github.com/szhekpisov/gomutants/internal/mutator"
	"github.com/szhekpisov/gomutants/internal/report"
)

// mutationRun is the state one gomutants invocation carries from phase to
// phase: each field is set by one phase of run() and read by later ones.
type mutationRun struct {
	cfg     config.Config
	opts    *cliOptions
	filters runFilters

	projectDir string
	goModule   string
	term       *report.Terminal

	enabledMutators []mutator.Mutator

	// Incremental-analysis cache; all nil/empty when --cache is off.
	loadedCache  *cache.Cache
	hasher       *cache.Hasher
	testFilesFor cache.TestFilesForFn
	// goToolchain fingerprints the project's `go` (its `go version`
	// string). It joins the cache metadata gate because EQUIVALENT
	// verdicts are decided by the compiler, and feeds the coverage-key
	// toolchain dimension. Computed once, only when caching is on.
	goToolchain string

	pkgs       []discover.Package
	fset       *token.FileSet
	discovered *discover.Result
	mutants    []mutator.Mutant
	// suppressed holds every mutant a directive or --exclude-calls
	// removed; callSuppressed is the --exclude-calls share of it.
	suppressed     []discover.Suppression
	callSuppressed []discover.Suppression
	pendingCount   int

	// Coverage scope: the patterns `go test` runs, the -coverpkg it
	// passes, and the directories of the packages those tests live in.
	// Widened to the reverse-dependency closure under --integration.
	coveragePatterns []string
	coverPkgEff      string
	rDirs            []string

	tmpDir       string
	coverStart   time.Time
	profile      *coverage.Profile
	profileBytes []byte
	coverageKey  string
	testTimeout  time.Duration
	srcCache     map[string][]byte
	testMap      *coverage.TestMap

	lastCheckpoint time.Time
}
