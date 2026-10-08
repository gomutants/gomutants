package main

import (
	"context"
	"fmt"
	"go/token"
	"os"
	"runtime"
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

// setup locates the module, loads the incremental cache, selects the
// enabled mutators and prints the run header.
func (mr *mutationRun) setup(ctx context.Context) error {
	var err error
	packages := mr.opts.packages

	// Determine project directory (current working directory).
	mr.projectDir, err = getwdFunc()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}

	// Read go module name from go.mod.
	mr.goModule, err = readModuleName(mr.projectDir)
	if err != nil {
		return err
	}

	// Load the cache early so the coverage phase can short-circuit on a
	// matching profile key. The per-mutant Lookup runs later off the same
	// *Cache; load failures fall through to a nil cache, which the rest
	// of the pipeline treats as "no cache at all". A single Hasher is
	// reused across the coverage-key calc and Lookup so per-file sha256s
	// are memoized only once.
	if mr.cfg.Cache != "" {
		mr.goToolchain = goVersionFunc(ctx)
		mr.loadedCache = cacheLoadFunc(mr.cfg.Cache, mr.goModule, cacheToolVersion(), mr.cfg.Tags, mr.cfg.CanonicalTestFlags(), mr.goToolchain)
		// Hasher is created before discovery's PreReadFiles so the
		// coverage-key calc can use it. SetSrcCache is called once
		// the in-memory source map exists (after step 6), so
		// per-mutant Lookup's prodHash calls reuse already-loaded bytes.
		mr.hasher = cache.NewHasher(nil)
	}

	// Get enabled mutators. Validate names first so a typo in --only /
	// --disable (or in the config file) surfaces as a stderr warning
	// instead of a silent filter miss. EnabledMutators already ignores
	// unknown names; warning is purely additive.
	reg := mutator.NewRegistry()
	unknownOnly := reg.UnknownNames(mr.cfg.Only)
	unknownDisable := reg.UnknownNames(mr.cfg.Disable)
	for _, n := range unknownOnly {
		fmt.Fprintf(stderr, "gomutants: unknown mutator %q in --only (ignored)\n", n)
	}
	for _, n := range unknownDisable {
		fmt.Fprintf(stderr, "gomutants: unknown mutator %q in --disable (ignored)\n", n)
	}
	if len(unknownOnly)+len(unknownDisable) > 0 {
		fmt.Fprintln(stderr, `gomutants: run "gomutants --list-mutators" to see valid mutator names`)
	}
	mr.enabledMutators = reg.EnabledMutators(mr.cfg.Only, mr.cfg.Disable)

	mr.term = report.NewTerminal(stdout, 0, mr.cfg.Verbose, mr.cfg.Quiet)
	mr.term.Header(effectiveVersion(), fmt.Sprintf("%v", packages), mr.cfg.Workers, len(mr.enabledMutators))
	return nil
}

// resolvePackages resolves the package patterns, applies --exclude-files,
// resolves --run-mutant-id early, and fixes the coverage scope.
func (mr *mutationRun) resolvePackages(ctx context.Context) error {
	var err error
	packages := mr.opts.packages

	// 1. Resolve packages.
	mr.term.Phase("Resolving packages...")
	mr.pkgs, err = discover.ResolvePackages(ctx, mr.projectDir, packages, mr.cfg.Tags)
	if err != nil {
		return err
	}
	var excludedFiles int
	mr.pkgs, excludedFiles = discover.ApplyExcludes(mr.pkgs, mr.filters.excluder, mr.projectDir)
	if mr.hasher != nil {
		// Feed the cache's pkg_hash the //go:embed inputs go list resolved
		// for these packages. Mutants only ever live in the packages
		// resolved here, so every directory HashPkgFiles is asked about as
		// a *mutant's* package is covered. Set here, before any Lookup or
		// Update, so every pkg_hash this run computes carries the
		// dimension.
		mr.hasher.SetEmbedFiles(embedFilesByDir(mr.pkgs))
	}
	resolveMsg := fmt.Sprintf("done (%d packages)", len(mr.pkgs))
	if excludedFiles > 0 {
		resolveMsg = fmt.Sprintf("done (%d packages, %d files excluded)", len(mr.pkgs), excludedFiles)
	}
	mr.term.PhaseDone(resolveMsg)

	// Resolve --run-mutant-id here, not at step 5. Discovery is pure AST
	// work over the packages just resolved, so an unknown or ambiguous id
	// costs nothing to diagnose; leaving it at step 5 would charge a full
	// `go test -cover` plus a baseline run before reporting a typo or a
	// stale id — on the one flag whose purpose is to avoid paying for the
	// whole package. The result is carried to step 5 rather than re-parsed.
	mr.fset = token.NewFileSet()
	if mr.cfg.RunMutantID != "" {
		mr.discovered = discover.Discover(mr.fset, mr.pkgs, mr.enabledMutators, mr.projectDir, mr.goModule)
		// Runs before every other filter so that "no mutant matches this
		// id" is diagnosed against the full discovered set rather than
		// against whatever --changed-since happened to leave behind. Both
		// drop mutants, so the order doesn't change the intersection.
		mr.mutants, err = discover.FilterByStableID(mr.discovered.Mutants, mr.cfg.RunMutantID)
		if err != nil {
			return usageError(err)
		}
	}

	// Integration mode widens coverage collection, the baseline run, and the
	// per-test map to the reverse-dependency closure R of the target packages
	// (T) — so a mutant in T can be killed by a covering test in any package
	// that imports it — with -coverpkg pinned to T so those importing tests
	// record coverage on the mutated code. Mutant *discovery* stays on T.
	// Non-integration runs leave the patterns and -coverpkg untouched.
	mr.coveragePatterns = packages
	mr.coverPkgEff = mr.cfg.CoverPkg
	mr.rDirs = dirsOfPackages(mr.pkgs)
	if mr.cfg.Integration {
		mr.coveragePatterns, mr.rDirs, mr.coverPkgEff = integrationScope(ctx, mr.projectDir, mr.goModule, mr.pkgs, mr.cfg.Tags, stderr)
	}
	return nil
}

// collectCoverage produces the coverage profile, from the cache when its
// key matches and from a fresh `go test -coverprofile` run otherwise.
func (mr *mutationRun) collectCoverage(ctx context.Context) error {
	// 3. Collect coverage. With --cache enabled, the profile is memoized
	// under a content-hash key that fingerprints every input that can
	// change `go test -coverprofile` output (sources, go.mod/sum, toolchain,
	// env, -coverpkg). A key match parses the cached profile in-process and
	// skips the multi-second `go test` invocation.
	mr.term.Phase("Collecting coverage...")
	mr.coverStart = time.Now()

	var coverFromCache bool
	if mr.loadedCache != nil {
		coverFromCache = mr.cachedCoverageProfile(ctx)
	}

	if mr.profile == nil {
		profilePath, rerr := runCoverageFunc(ctx, mr.projectDir, mr.coveragePatterns, mr.coverPkgEff, mr.cfg.Tags, mr.tmpDir, mr.cfg.TestFlagFields())
		if rerr != nil {
			return rerr
		}
		// Read the profile bytes once and reuse them for parsing + cache
		// persistence. Avoids a second file read at cache-write time.
		bs, rerr := os.ReadFile(profilePath)
		if rerr != nil {
			return fmt.Errorf("reading coverage profile: %w", rerr)
		}
		p, perr := parseBytesFunc(bs)
		if perr != nil {
			return perr
		}
		mr.profile = p
		mr.profileBytes = bs
	}

	coverSuffix := ""
	if coverFromCache {
		coverSuffix = ", cached"
	}
	mr.term.PhaseDone(fmt.Sprintf("done (%s%s)", phaseDurationDisplay(time.Since(mr.coverStart)), coverSuffix))
	return nil
}

// cachedCoverageProfile computes the coverage cache key and, when it
// matches the loaded cache, takes the profile from there. It reports
// whether it did; the key is recorded either way so the end of the run can
// stamp it into the cache.
func (mr *mutationRun) cachedCoverageProfile(ctx context.Context) bool {
	// Hash failures (unreadable file/dir) fall through to a fresh
	// coverage run rather than aborting — same conservative policy
	// as the per-mutant Lookup path.
	// In integration mode the coverage profile is produced by running R's
	// tests with -coverpkg=T, so the hash must fingerprint R's sources
	// (rDirs, which already include T) and the effective -coverpkg. The
	// distinct coverPkgEff value also keeps integration and
	// non-integration runs in separate cache namespaces.
	var (
		hashDirs []string
		derr     error
	)
	if mr.cfg.Integration {
		hashDirs = mr.rDirs
	} else {
		hashDirs, derr = coveragePkgDirs(ctx, mr.projectDir, mr.pkgs, mr.coverPkgEff, mr.cfg.Tags)
	}
	if derr == nil {
		toolchain := fmt.Sprintf("gomutants/%s|go/%s", runtime.Version(), mr.goToolchain)
		if k, herr := mr.hasher.HashCoverageInputs(hashDirs, mr.projectDir, mr.coverPkgEff, mr.cfg.Tags, mr.cfg.CanonicalTestFlags(), toolchain, captureCoverageEnv()); herr == nil {
			mr.coverageKey = k
		}
	}
	if mr.coverageKey != "" && mr.coverageKey == mr.loadedCache.CoverageKey && mr.loadedCache.CoverageProfile != "" {
		if p, perr := parseBytesFunc([]byte(mr.loadedCache.CoverageProfile)); perr == nil {
			mr.profile = p
			mr.profileBytes = []byte(mr.loadedCache.CoverageProfile)
			return true
		}
	}
	return false
}

// measureBaseline times one unmutated test run and derives the global
// per-mutant timeout ceiling from it.
func (mr *mutationRun) measureBaseline(ctx context.Context) error {
	// 4. Measure baseline test duration.
	mr.term.Phase("Measuring baseline...")
	baseline, err := measureBaselineFunc(ctx, mr.projectDir, mr.coveragePatterns, mr.cfg.Tags, mr.cfg.TestFlagFields())
	if err != nil {
		return err
	}
	mr.testTimeout = baseline * time.Duration(mr.cfg.TimeoutCoefficient)
	// Distinguish the displayed value: with adaptive sizing, testTimeout
	// is the upper-bound ceiling, not the deadline every mutant gets.
	// Without this, users see "timeout: 4m" and assume each mutant has
	// 4 minutes; in reality fast packages get sub-second deadlines.
	timeoutLabel := "timeout"
	if mr.cfg.AdaptiveTimeoutEnabled() {
		timeoutLabel = "ceiling"
	}
	mr.term.PhaseDone(fmt.Sprintf("done (%s, %s: %s)", phaseDurationDisplay(baseline), timeoutLabel, mr.testTimeout.Round(time.Second)))
	return nil
}
