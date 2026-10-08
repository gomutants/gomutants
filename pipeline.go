package main

import (
	"context"
	"fmt"
	"go/token"
	"os"
	"runtime"
	"slices"
	"time"

	"github.com/szhekpisov/gomutants/internal/cache"
	"github.com/szhekpisov/gomutants/internal/config"
	"github.com/szhekpisov/gomutants/internal/coverage"
	"github.com/szhekpisov/gomutants/internal/discover"
	"github.com/szhekpisov/gomutants/internal/mutator"
	"github.com/szhekpisov/gomutants/internal/report"
	"github.com/szhekpisov/gomutants/internal/runner"
	"github.com/szhekpisov/gomutants/internal/tce"
)

// mutationRun is the state one gomutants invocation carries from phase to
// phase. run() calls the phase methods in a fixed order; each method reads
// what earlier phases set and may set or narrow fields for later ones.
type mutationRun struct {
	cfg     config.Config
	opts    runOptions
	filters runFilters

	projectDir string
	goModule   string
	// term prints the header and phase lines; progress, created once the
	// pending count is known, prints per-mutant results and the summary.
	term     *report.Terminal
	progress *report.Terminal

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

	pkgs []discover.Package
	// mutants is set by discoverMutants, narrowed by its filters, and
	// given its verdicts in place by pool.Run.
	mutants []mutator.Mutant
	// runTarget is the one mutant --run-mutant-id names. It points into
	// mutants, so it carries the verdict pool.Run records; nil on every
	// other run.
	runTarget *mutator.Mutant
	// earlyTarget is the mutant checkRunMutantID resolved the id to,
	// before coverage and the baseline. discoverMutants resolves the id
	// again and refuses to measure anything else.
	earlyTarget mutator.Mutant
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

	tmpDir     string
	coverStart time.Time
	// profile is cleared once discoverMutants has filtered by coverage,
	// profileBytes once runMutants has stamped it into the cache.
	// coverFromCache marks a profile the cache already holds under
	// coverageKey, which runMutants then has no reason to stamp.
	profile        *coverage.Profile
	profileBytes   []byte
	coverFromCache bool
	coverageKey    string
	testTimeout    time.Duration
	srcCache       map[string][]byte
	testMap        *coverage.TestMap

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
	// of the pipeline treats as "no cache at all".
	if mr.cfg.Cache != "" {
		mr.goToolchain = goVersionFunc(ctx)
		mr.loadedCache = cacheLoadFunc(mr.cfg.Cache, mr.goModule, cacheToolVersion(), mr.cfg.Tags, mr.cfg.CanonicalTestFlags(), mr.goToolchain)
		// Hasher is created before discovery's PreReadFiles so the
		// coverage-key calc can use it. SetSrcCache is called once
		// the in-memory source map exists (in preReadSources), so
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

// resolvePackages resolves the package patterns and applies
// --exclude-files.
func (mr *mutationRun) resolvePackages(ctx context.Context) error {
	var err error
	packages := mr.opts.packages

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
	return nil
}

// checkRunMutantID, under --run-mutant-id, fails fast when the id names
// no mutant, more than one, or one that --changed-since, a directive or
// --exclude-calls drops. None of that needs coverage, so a typo, a stale
// id or a suppressed target costs nothing to diagnose here; left to
// discoverMutants, it would charge a full `go test -cover` plus a baseline
// run first — on the one flag whose purpose is to avoid paying for the
// whole package. Only the files the id could name are parsed.
//
// The parse is thrown away: discoverMutants parses again after the
// baseline, so the bytes the overlays patch are no older than the sibling
// files and the TCE reference build, which both read from disk.
func (mr *mutationRun) checkRunMutantID(ctx context.Context) error {
	id := mr.cfg.RunMutantID
	if id == "" {
		return nil
	}
	scope := discover.ScopeToStableID(mr.pkgs, id, mr.projectDir)
	if !slices.ContainsFunc(scope, func(p discover.Package) bool { return len(p.GoFiles) > 0 }) {
		return usageError(fmt.Errorf(
			"no mutant matches --run-mutant-id %q: no source file in the resolved packages has a path it could name; check the package argument and that the id came from a report for this revision",
			id))
	}
	fset := token.NewFileSet()
	found := discover.Discover(fset, scope, mr.enabledMutators, mr.projectDir, mr.goModule)
	target, err := discover.FilterByStableID(found.Mutants, id)
	if err != nil {
		return usageError(err)
	}
	// Silent: discoverMutants filters this file again and warns there.
	kept, suppressed, _, err := mr.filterBySource(ctx, fset, target, found.Files, false)
	if err != nil {
		return err
	}
	if len(kept) == 0 {
		return usageError(runMutantDroppedError(id, mr.cfg.ChangedSince, suppressed))
	}
	mr.earlyTarget = target[0]
	return nil
}

// checkSameRunTarget fails when the id, resolved again after the
// baseline, names a different mutant than checkRunMutantID found. An edit
// in between can do that without making the id unknown or ambiguous: a
// prefix that now matches another mutant, or a new expression above the
// target that shifts every ordinal after it. Measuring the newcomer would
// answer a question nobody asked. A target that merely moved fails too —
// its code is no longer the code the id was looked up for.
func (mr *mutationRun) checkSameRunTarget(m mutator.Mutant) error {
	e := mr.earlyTarget
	if m.StableID == e.StableID && m.Line == e.Line && m.Col == e.Col && m.Original == e.Original {
		return nil
	}
	return fmt.Errorf("--run-mutant-id %q named %s at %s:%d:%d before the baseline but %s at %s:%d:%d after it; the source changed during the run, run again",
		mr.cfg.RunMutantID, e.StableID, e.RelFile, e.Line, e.Col, m.StableID, m.RelFile, m.Line, m.Col)
}

// resolveCoverageScope fixes the packages coverage and the baseline run
// over, widening them to the reverse-dependency closure under
// --integration.
func (mr *mutationRun) resolveCoverageScope(ctx context.Context) {
	// Integration mode widens coverage collection, the baseline run, and the
	// per-test map to the reverse-dependency closure R of the target packages
	// (T) — so a mutant in T can be killed by a covering test in any package
	// that imports it — with -coverpkg pinned to T so those importing tests
	// record coverage on the mutated code. Mutant *discovery* stays on T.
	// Non-integration runs leave the patterns and -coverpkg untouched.
	mr.coveragePatterns = mr.opts.packages
	mr.coverPkgEff = mr.cfg.CoverPkg
	mr.rDirs = dirsOfPackages(mr.pkgs)
	if mr.cfg.Integration {
		mr.coveragePatterns, mr.rDirs, mr.coverPkgEff = integrationScope(ctx, mr.projectDir, mr.goModule, mr.pkgs, mr.cfg.Tags, stderr)
	}
}

// makeTempDir creates the directory the coverage, test-map and mutant
// runs write into. run() removes it.
func (mr *mutationRun) makeTempDir() error {
	var err error
	mr.tmpDir, err = mkdirTempFunc("", "gomutants-*")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	return nil
}

// collectCoverage produces the coverage profile, from the cache when its
// key matches and from a fresh `go test -coverprofile` run otherwise.
func (mr *mutationRun) collectCoverage(ctx context.Context) error {
	// With --cache enabled, the profile is memoized under a content-hash
	// key that fingerprints every input that can change
	// `go test -coverprofile` output (sources, go.mod/sum, toolchain, env,
	// -coverpkg). A key match parses the cached profile in-process and
	// skips the multi-second `go test` invocation.
	mr.term.Phase("Collecting coverage...")
	mr.coverStart = time.Now()

	if mr.loadedCache != nil {
		mr.coverFromCache = mr.cachedCoverageProfile(ctx)
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
	if mr.coverFromCache {
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
			return true
		}
	}
	return false
}

// measureBaseline times one unmutated test run and derives the global
// per-mutant timeout ceiling from it.
func (mr *mutationRun) measureBaseline(ctx context.Context) error {
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

// discoverMutants parses the packages and applies the --run-mutant-id,
// --changed-since, directive, --exclude-calls and coverage filters to the
// mutants found. It returns the parse cache, every parsed file keyed by
// absolute path, for preReadSources; no later phase needs the ASTs, so the
// caller drops it once the source bytes are taken.
func (mr *mutationRun) discoverMutants(ctx context.Context) (map[string]*discover.ParsedFile, error) {
	mr.term.Phase("Discovering mutants...")
	fset := token.NewFileSet()
	found := discover.Discover(fset, mr.pkgs, mr.enabledMutators, mr.projectDir, mr.goModule)
	mr.mutants = found.Mutants
	if mr.cfg.RunMutantID != "" {
		// checkRunMutantID resolved the id against the source as it was
		// before coverage and the baseline; an edit since can change the
		// answer. Runs before every other filter so that "no mutant
		// matches this id" is diagnosed against the full discovered set
		// rather than against whatever --changed-since happened to leave
		// behind. Both drop mutants, so the order doesn't change the
		// intersection.
		var err error
		mr.mutants, err = discover.FilterByStableID(mr.mutants, mr.cfg.RunMutantID)
		if err != nil {
			return nil, usageError(err)
		}
		if err := mr.checkSameRunTarget(mr.mutants[0]); err != nil {
			return nil, usageError(err)
		}
	}
	var err error
	mr.mutants, mr.suppressed, mr.callSuppressed, err = mr.filterBySource(ctx, fset, mr.mutants, found.Files, true)
	if err != nil {
		return nil, err
	}
	// Coverage only sets a status, so it can run after the filters that
	// drop: it then skips the mutants they removed.
	discover.FilterByCoverage(mr.mutants, mr.profile, mr.pkgs, mr.goModule)
	mr.profile = nil
	if mr.cfg.Verbose {
		for _, s := range mr.suppressed {
			fmt.Fprintf(stderr, "suppressed %s at %s:%d (%s)\n",
				s.Mutant.Type, s.Mutant.RelFile, s.Mutant.Line, suppressionReason(s))
		}
	}
	// FilterByStableID resolved the id, but a later filter can still drop
	// what it found: checkRunMutantID ran the same filters, but an edit
	// since — a directive added during the baseline, say — can change
	// their answer. Without this the run would test nothing, print
	// "0 found" and exit 0 — indistinguishable, to a script reading the
	// exit code, from the mutant having been killed.
	// Otherwise mutants holds exactly the one FilterByStableID returned:
	// the filters only drop.
	if mr.cfg.RunMutantID != "" {
		if len(mr.mutants) == 0 {
			return nil, usageError(runMutantDroppedError(mr.cfg.RunMutantID, mr.cfg.ChangedSince, mr.suppressed))
		}
		mr.runTarget = &mr.mutants[0]
	}

	mr.pendingCount = 0
	notCoveredCount := 0
	for _, m := range mr.mutants {
		switch m.Status {
		case mutator.StatusPending:
			mr.pendingCount++
		case mutator.StatusNotCovered:
			notCoveredCount++
		}
	}
	mr.term.PhaseDone(fmt.Sprintf("%d found (%d not covered, %d to test)", len(mr.mutants), notCoveredCount, mr.pendingCount))
	return found.Files, nil
}

// filterBySource applies the filters that need only the source and git:
// --changed-since, then directives, then --exclude-calls. suppressed holds
// every mutant the last two dropped, callSuppressed the --exclude-calls
// share of it. warn=false silences directive warnings, for a pre-check
// whose files the run filters again.
func (mr *mutationRun) filterBySource(ctx context.Context, fset *token.FileSet, mutants []mutator.Mutant, files map[string]*discover.ParsedFile, warn bool) (kept []mutator.Mutant, suppressed, callSuppressed []discover.Suppression, err error) {
	kept = mutants
	if mr.cfg.ChangedSince != "" {
		gitRoot, gerr := discover.GitRoot(ctx, mr.projectDir)
		if gerr != nil {
			return nil, nil, nil, fmt.Errorf("--changed-since requires a git repository: %w", gerr)
		}
		ranges, gerr := discover.RunGitDiff(ctx, mr.projectDir, mr.cfg.ChangedSince)
		if gerr != nil {
			return nil, nil, nil, gerr
		}
		kept = discover.FilterByDiff(kept, ranges, gitRoot)
	}
	filterDirectives := discover.FilterByDirectivesWithCache
	if !warn {
		filterDirectives = discover.FilterByDirectivesSilently
	}
	kept, suppressed, err = filterDirectives(fset, kept, files)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("applying directives: %w", err)
	}
	// Directives run first so that where both could apply, the reason
	// surfaced under --verbose is the one a human wrote at the site.
	kept, callSuppressed = discover.FilterByCalls(fset, kept, files, mr.filters.callExcluder)
	return kept, append(suppressed, callSuppressed...), callSuppressed, nil
}

// printDryRun lists the discovered mutants for --dry-run.
func (mr *mutationRun) printDryRun() {
	for _, m := range mr.mutants {
		fmt.Fprintf(stdout, "[%s] %s:%d:%d  %s → %s  (%s)\n",
			m.Status.String(), m.RelFile, m.Line, m.Col,
			m.Original, m.Replacement, m.Type)
	}
}

// preReadSources collects every production source file into memory once,
// for the overlays and for cache hashing. parsed is discoverMutants' parse
// cache: the bytes discovery parsed are reused, so the overlays patch
// exactly what the mutant offsets were computed on.
func (mr *mutationRun) preReadSources(parsed map[string]*discover.ParsedFile) error {
	var err error
	mr.srcCache, err = preReadFilesFunc(mr.pkgs, parsed)
	if err != nil {
		return fmt.Errorf("pre-reading source files: %w", err)
	}
	if mr.hasher != nil {
		// Hasher was created early (before PreReadFiles) for the
		// coverage-key calc; attach the in-memory source map now so
		// per-mutant Lookup's prodHash calls skip disk reads. It also
		// drops what the coverage-key calc memoized: those hashes were
		// read before coverage and the baseline, and the cache entries
		// must be keyed on the bytes the mutants are measured on.
		mr.hasher.SetSrcCache(mr.srcCache)
	}
	return nil
}

// buildTestMap records which tests cover which lines, so each mutant runs
// only its covering tests. Failure is fatal only under -coverpkg.
func (mr *mutationRun) buildTestMap(ctx context.Context) error {
	var err error
	mr.term.Phase("Building per-test coverage map...")
	// testTimeout also bounds each test's solo coverage run: a test that
	// can't finish in the suite's ceiling would time out every mutant it
	// covers anyway, and a bare test binary has no timeout of its own.
	mr.testMap, err = buildTestMapFunc(ctx, mr.projectDir, mr.coveragePatterns, coverage.BuildOptions{
		CoverPkg:    mr.coverPkgEff,
		Tags:        mr.cfg.Tags,
		TmpDir:      mr.tmpDir,
		Workers:     mr.cfg.Workers,
		TestTimeout: mr.testTimeout,
		TestFlags:   coverageTestFlags(mr.cfg.TestFlagFields(), runner.ShortFlagFromEnv(), mr.cfg.TestCPU),
	})
	if err != nil {
		// An interrupt stops the run here, as in every other phase; it
		// isn't a map failure to work around.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// With -coverpkg the map fails only when it can't tell which
		// packages have tests (see coverage.BuildTestMap). Those are
		// the suites a mutant's verdict rests on beyond its own package's:
		// without them a mutant only an importer's tests kill reads LIVED.
		if mr.coverPkgEff != "" {
			return fmt.Errorf("per-test coverage map: %w", err)
		}
		// Non-fatal: fall back to running all tests per mutant.
		fmt.Fprintf(stderr, "warning: per-test coverage map failed: %v\n", err)
		mr.testMap = nil
		mr.term.PhaseDone("skipped (will run all tests per mutant)")
	} else {
		mr.term.PhaseDone("done")
		for _, w := range mr.testMap.Warnings() {
			fmt.Fprintf(stderr, "warning: per-test coverage map: %s\n", w)
		}
	}
	return nil
}

// applyCache replays prior verdicts for mutants whose package and covering
// tests are unchanged since the cached run.
func (mr *mutationRun) applyCache() {
	// Apply incremental-analysis cache (opt-in via --cache). Hits
	// flip the mutant from Pending to its prior terminal status, which
	// makes the runner's Pending-only filter naturally skip them.
	// loadedCache + hasher were created at module-read time so the
	// coverage phase could already consult them — here we just build
	// the test-files resolver and run the lookup.
	if mr.loadedCache == nil {
		return
	}
	// TestIndex is built from the reverse-dependency closure's directories
	// (rDirs; equal to the target dirs when integration mode is off) so
	// cross-package coverage — tests in an importing package exercising a
	// mutated target via -coverpkg — resolves to the right test files.
	// The map's suites join them: each can decide a survivor's verdict
	// (see testFilesResolver), and one need not be in rDirs — a package
	// --exclude-files emptied is gone from pkgs, but its tests still
	// run under --coverpkg, and a file the index lacks is in no key.
	testIndex := cache.BuildTestIndex(slices.Concat(mr.rDirs, suiteDirs(mr.testMap)))

	crossPkg := mr.cfg.Integration || mr.cfg.CoverPkg != ""
	mr.testFilesFor = testFilesResolver(testIndex, mr.testMap, crossPkg)

	// --run-mutant-id skips the lookup, not the resolver above: the
	// point of naming one mutant is to measure it again after editing
	// a test, and a cache hit would replay the previous verdict
	// instead of running anything. testFilesFor is still needed by
	// checkpoint's loadedCache.Update, so the fresh verdict lands in
	// the cache file as usual.
	if mr.cfg.RunMutantID != "" {
		return
	}
	hits := mr.loadedCache.Lookup(mr.mutants, mr.hasher, mr.testFilesFor)
	if hits == 0 {
		return
	}
	mr.pendingCount -= hits
	// When equivalence detection is off this run, a cached EQUIVALENT
	// must not surface — report the survivor honestly as LIVED. The
	// reuse already validated prod+tests hashes (EQUIVALENT needs a
	// tests hash), so a LIVED reading is sound.
	if !mr.cfg.DetectEquivalentEnabled() {
		demoteEquivalents(mr.mutants)
	}
	mr.term.Info("Cache: %d mutant outcomes reused from %s", hits, mr.cfg.Cache)
}

// demoteEquivalents reports every EQUIVALENT mutant as LIVED.
func demoteEquivalents(mutants []mutator.Mutant) {
	for i := range mutants {
		if mutants[i].Status == mutator.StatusEquivalent {
			mutants[i].Status = mutator.StatusLived
		}
	}
}

// runMutants tests every pending mutant, runs the opt-in equivalence pass
// over the survivors, and flushes the cache, checkpointing as it goes.
func (mr *mutationRun) runMutants(ctx context.Context) {
	mr.progress = report.NewTerminal(stdout, mr.pendingCount, mr.cfg.Verbose, mr.cfg.Quiet)
	// Idle "(compiling)" heartbeat so the TTY doesn't sit silent during
	// the first per-package go-test compile (no OnResult until the first
	// mutant completes). First OnResult auto-stops it; the defer covers
	// the all-cached / zero-pending paths where OnResult never fires.
	mr.progress.StartHeartbeat()
	defer mr.progress.StopHeartbeat()

	// pool.Run mutates the slice in place.
	// TimeoutPolicy resolves per-mutant deadlines from the per-test
	// durations recorded on the testMap, falling back to the global
	// baseline×coefficient ceiling. testTimeout stays the absolute cap.
	policy := timeoutPolicyFor(&mr.cfg, mr.testTimeout)

	// Stamp the coverage memo once, before the run loop: the profile and
	// its key are fixed for the whole run, so there's no reason to
	// re-serialize the (potentially large) profile on every checkpoint.
	// An empty coverageKey means hashing failed earlier and we silently
	// fell back to a fresh run; don't poison the cache with a missing key.
	// A profile that came from the cache is already stored under its key.
	if mr.loadedCache != nil && mr.coverageKey != "" && !mr.coverFromCache {
		mr.loadedCache.CoverageKey = mr.coverageKey
		mr.loadedCache.CoverageProfile = string(mr.profileBytes)
	}
	mr.profileBytes = nil

	pool := runner.NewPool(mr.cfg.Workers, runner.ExecOpts{TestCPU: mr.cfg.TestCPU, Tags: mr.cfg.Tags, TestFlags: mr.cfg.TestFlagFields()}, policy, mr.tmpDir, mr.srcCache, mr.projectDir, mr.testMap)
	// Seed lastCheckpoint so the first periodic checkpoint fires one full
	// interval into the run, not on the very first mutant.
	mr.lastCheckpoint = time.Now()
	pool.Run(ctx, mr.mutants, func(m mutator.Mutant) {
		mr.progress.OnResult(m)
		mr.checkpoint(false)
	})

	// Trivial Compiler Equivalence pass (opt-in). Recompile each
	// surviving (LIVED) mutant with package-scoped `-gcflags=-S` and
	// reclassify it as EQUIVALENT when the assembly matches the original —
	// a compiler-proven non-gap. Verdicts are checkpointed as they land (the
	// throttled checkpoint callback, serialized with the workers' writes by
	// the detector), so a hard kill mid-pass keeps the equivalence work done
	// so far. Cached EQUIVALENT survivors stay EQUIVALENT and are skipped
	// (their Status is no longer LIVED).
	if mr.cfg.DetectEquivalentEnabled() {
		mr.term.Phase("Detecting equivalent mutants...")
		det := tce.NewDetector(mr.projectDir, mr.cfg.Tags, mr.srcCache)
		equiv := det.Run(ctx, mr.mutants, mr.cfg.Workers, mr.tmpDir, func(mutator.Mutant) {
			mr.checkpoint(false)
		})
		mr.term.PhaseDone(fmt.Sprintf("%d equivalent", equiv))
	}

	// Final flush. force=true bypasses the throttle and the disable
	// switch, so even --checkpoint-interval=0 still writes the cache once.
	mr.checkpoint(true)
}

// checkpoint flushes completed mutant outcomes to the cache file,
// throttled to cfg.CheckpointInterval. cache.Update only emits
// terminal-status mutants, so flushing a partially-complete slice
// mid-run is safe — pending mutants are simply omitted. No locking
// needed: pool.Run invokes onResult from a single goroutine, so the
// mutants-slice reads here are serialized with the collector's writes.
// Write failures are non-fatal — a stale cache only costs speed.
func (mr *mutationRun) checkpoint(force bool) {
	if mr.loadedCache == nil {
		return
	}
	if !force && mr.cfg.CheckpointInterval <= 0 {
		return
	}
	if !force && time.Since(mr.lastCheckpoint) < mr.cfg.CheckpointInterval {
		return
	}
	mr.loadedCache.Update(mr.mutants, mr.hasher, mr.projectDir, mr.testFilesFor)
	if err := cacheSaveFunc(mr.loadedCache, mr.cfg.Cache); err != nil {
		fmt.Fprintf(stderr, "warning: writing cache to %s: %v\n", mr.cfg.Cache, err)
		return // leave lastCheckpoint stale so the next onResult retries
	}
	mr.lastCheckpoint = time.Now()
}

// writeReports prints the summary and writes every requested report.
func (mr *mutationRun) writeReports() (*report.Report, error) {
	totalElapsed := time.Since(mr.coverStart)
	r := report.Generate(mr.mutants, mr.goModule, totalElapsed, len(mr.suppressed))
	// Breakdown only; the aggregate stays in MutantsSuppressed so the two
	// suppression sources share one bucket everywhere else.
	r.MutantsSuppressedByCalls = len(mr.callSuppressed)
	mr.progress.Summary(r)

	if err := report.WriteJSON(r, mr.cfg.Output); err != nil {
		return nil, fmt.Errorf("writing report: %w", err)
	}
	mr.term.Info("Report: %s", mr.cfg.Output)

	if mr.opts.strykerOutput != "" {
		if err := report.WriteStryker(mr.opts.strykerOutput, mr.mutants, mr.projectDir, effectiveVersion()); err != nil {
			return nil, fmt.Errorf("writing Stryker report: %w", err)
		}
		mr.term.Info("Stryker report: %s", mr.opts.strykerOutput)
	}

	if mr.opts.htmlOutput != "" {
		if err := report.WriteHTML(mr.opts.htmlOutput, mr.mutants, mr.projectDir, effectiveVersion()); err != nil {
			return nil, fmt.Errorf("writing HTML report: %w", err)
		}
		mr.term.Info("HTML report: %s", mr.opts.htmlOutput)
	}

	if mr.opts.annotations == "github" {
		if err := report.WriteGitHubAnnotations(stdout, r); err != nil {
			return nil, fmt.Errorf("writing annotations: %w", err)
		}
	}
	return r, nil
}

// checkThresholds turns the finished report into run()'s exit status.
func (mr *mutationRun) checkThresholds(r *report.Report) error {
	// Threshold gates. Exit codes 10/11 match gremlins's surface so scripts
	// that distinguish the two failure modes keep working. Mutant coverage
	// uses the gremlins formula (KILLED+LIVED)/(KILLED+LIVED+NOT_COVERED);
	// r.MutationsCoverage in the JSON uses a different denominator and is
	// kept as-is for backward-compat with existing report consumers.
	//
	// We deviate from gremlins on two points: a gate is *skipped* (with a
	// stderr note) when its denominator is zero — empty discovery is almost
	// always a config issue, not a test-quality issue, and reporting "0.00%
	// below 80.00%" hides that. Error messages always include both
	// percentages so a single read shows the full state.
	// EQUIVALENT mutants are neither KILLED nor LIVED, so they fall out of
	// both gates' denominators here — a compiler-proven non-gap shouldn't
	// move efficacy or mutant coverage in either direction. INFRA ERROR
	// mutants fall out the same way, but unlike EQUIVALENT they represent a
	// *missing* measurement, so the run warns before evaluating the gates.
	warnInfraErrors(stderr, r)
	// --run-mutant-id exists to answer one question — "did the test I just
	// wrote kill this mutant?" — from an exit code. Every status other than
	// KILLED and LIVED leaves that unanswered, and the gates below would
	// still exit 0: those statuses drop out of the efficacy denominator,
	// and a zero denominator skips the gate entirely. Report the non-answer
	// rather than let a script read it as a kill.
	if mr.runTarget != nil {
		if s := mr.runTarget.Status; s != mutator.StatusKilled && s != mutator.StatusLived {
			return fmt.Errorf("--run-mutant-id %q produced no verdict: the mutant is %s", mr.cfg.RunMutantID, s)
		}
	}
	tested := r.MutantsKilled + r.MutantsLived
	mcoverDenom := tested + r.MutantsNotCovered
	mcover := 0.0
	if mcoverDenom > 0 {
		mcover = float64(tested) / float64(mcoverDenom) * 100
	}

	if mr.opts.thresholdEfficacy > 0 {
		if tested == 0 {
			fmt.Fprintln(stderr, "gomutants: no testable mutants discovered; --threshold-efficacy not evaluated")
		} else if r.TestEfficacy < mr.opts.thresholdEfficacy {
			return &exitError{
				code: exitCodeEfficacy,
				err:  fmt.Errorf("test efficacy %.2f%% below --threshold-efficacy=%.2f%% (mutant coverage: %.2f%%)", r.TestEfficacy, mr.opts.thresholdEfficacy, mcover),
			}
		}
	}
	if mr.opts.thresholdMcover > 0 {
		if mcoverDenom == 0 {
			fmt.Fprintln(stderr, "gomutants: no covered or testable mutants discovered; --threshold-mcover not evaluated")
		} else if mcover < mr.opts.thresholdMcover {
			return &exitError{
				code: exitCodeMutantCoverage,
				err:  fmt.Errorf("mutant coverage %.2f%% below --threshold-mcover=%.2f%% (test efficacy: %.2f%%)", mcover, mr.opts.thresholdMcover, r.TestEfficacy),
			}
		}
	}
	return nil
}
