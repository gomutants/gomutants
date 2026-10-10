package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/gomutants/gomutants/internal/cache"
	"github.com/gomutants/gomutants/internal/config"
	"github.com/gomutants/gomutants/internal/coverage"
	"github.com/gomutants/gomutants/internal/discover"
	"github.com/gomutants/gomutants/internal/mutator"
	"github.com/gomutants/gomutants/internal/report"
	"github.com/gomutants/gomutants/internal/runner"
)

// Sentinel defaults; the effective* helpers upgrade these from build
// info when the corresponding ldflag wasn't injected.
const (
	devVersion   = "dev"
	devCommit    = "none"
	devBuildDate = "unknown"
)

// Process exit codes form part of the CLI contract consumed by CI wrappers.
const (
	exitCodeRuntimeError   = 1
	exitCodeUsageError     = 2
	exitCodeEfficacy       = 10
	exitCodeMutantCoverage = 11
)

// Overridden at release time via -ldflags '-X main.<field>=...'.
var (
	version   = devVersion
	commit    = devCommit
	buildDate = devBuildDate
)

// effectiveVersion returns the user-visible version. Ldflags-injected
// builds win; otherwise we try Main.Version from build info (set by
// `go install module@vX.Y.Z`); otherwise devVersion.
func effectiveVersion() string {
	if version != devVersion {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			// Module versions carry a leading "v" (v0.4.0) while
			// ldflags-injected versions don't (goreleaser's {{.Version}}
			// strips it); trim so formatVersion's "v%s" doesn't print "vv".
			return strings.TrimPrefix(v, "v")
		}
	}
	return version
}

// effectiveCommit returns the source commit. Ldflags win; otherwise
// vcs.revision from build info (populated for `go build` from a git
// tree, but not for `go install module@vX.Y.Z`); otherwise devCommit.
func effectiveCommit() string {
	if commit != devCommit {
		return commit
	}
	if v := vcsSetting("vcs.revision"); v != "" {
		return v
	}
	return commit
}

// effectiveBuildDate returns the build date. Ldflags win; otherwise
// vcs.time from build info; otherwise devBuildDate.
func effectiveBuildDate() string {
	if buildDate != devBuildDate {
		return buildDate
	}
	if v := vcsSetting("vcs.time"); v != "" {
		return v
	}
	return buildDate
}

func vcsSetting(key string) string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range info.Settings {
		if s.Key == key {
			return s.Value
		}
	}
	return ""
}

// formatVersion renders the rich `--version` line: name + version,
// then commit and build date when known.
func formatVersion() string {
	return fmt.Sprintf("gomutants v%s (commit: %s, built: %s)\n",
		effectiveVersion(), effectiveCommit(), effectiveBuildDate())
}

// writeMutatorCatalog renders one greppable line per registered mutator. The
// registry supplies alphabetical entries; tabwriter only aligns the three
// fields for terminal readers.
func writeMutatorCatalog(w io.Writer, entries []mutator.CatalogEntry) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	for _, entry := range entries {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\n", entry.Type, entry.Description, entry.Example); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// cacheToolVersion is the identifier stamped into the cache's
// `tool_version` field and gated on Load to invalidate stale entries.
//
// We extend the user-visible version with vcs metadata from
// runtime/debug so that local dev builds from different commits don't
// share a key. Without this, two contributors editing mutator code
// against the same constant version string would produce silent stale
// skips for each other (or for themselves across rebuilds).
//
// Format: "<version>" for clean release builds; "<version>+<short>"
// for committed dev builds; "<version>+<short>.dirty" when the working
// tree has uncommitted changes; "<version>+nobuildinfo" when build
// info is unavailable (e.g. `go run`). Computed once via sync.Once
// would be overkill — this runs exactly twice per process at most
// (startup + cache integration).
func cacheToolVersion() string {
	v := effectiveVersion()
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return v + "+nobuildinfo"
	}
	var rev string
	var modified bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if rev == "" {
		return v + "+nobuildinfo"
	}
	short := rev
	if len(short) > 12 {
		short = short[:12]
	}
	if modified {
		return v + "+" + short + ".dirty"
	}
	return v + "+" + short
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "gomutants: %v\n", err)
		os.Exit(exitCodeForError(err))
	}
}

// exitError carries a specific exit code through run()'s error return so
// main can map it to os.Exit. Code 2 distinguishes usage/configuration
// failures from runtime/build failures, while codes 10 and 11 match
// gremlins's threshold surface.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }

func (e *exitError) Unwrap() error { return e.err }

func exitCodeForError(err error) int {
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	return exitCodeRuntimeError
}

func usageError(err error) error {
	return &exitError{code: exitCodeUsageError, err: err}
}

func usageErrorf(format string, args ...any) error {
	return usageError(fmt.Errorf(format, args...))
}

// stdout is the writer for user-facing output. Tests swap this to capture output.
var stdout io.Writer = os.Stdout

// stderr is the writer for warnings. Tests swap this to capture output.
var stderr io.Writer = os.Stderr

// buildTestMapFunc is the per-test coverage map builder. Swappable so
// tests can drive the warning/skip path without engineering a real
// coverage-map failure.
var buildTestMapFunc = coverage.BuildTestMap

// runCoverageFunc / measureBaselineFunc / parseProfileFunc / preReadFilesFunc
// indirect through swappable variables so tests can selectively fail
// individual pipeline steps and lock down each err-return wrap.
var (
	runCoverageFunc     = runner.RunCoverage
	measureBaselineFunc = runner.MeasureBaseline
	parseBytesFunc      = coverage.ParseBytes
	preReadFilesFunc    = discover.PreReadFiles
	mkdirTempFunc       = os.MkdirTemp
	getwdFunc           = os.Getwd
	cacheLoadFunc       = cache.Load
	cacheSaveFunc       = cache.Save
	resolveCoverPkgFunc = discover.ResolvePackages
	goVersionFunc       = runGoVersion
	goRootFunc          = coverage.GOROOT
)

// phaseDurationDisplay rounds a duration to 100ms precision for display
// in phase-done lines. Extracted so the rounding constant is testable
// without parsing fmt output: ARITHMETIC mutations on `100*time.Millisecond`
// (e.g. `*` → `/`, which collapses to 0 and disables rounding) surface as
// observable changes in the returned Duration.
func phaseDurationDisplay(d time.Duration) time.Duration {
	return d.Round(100 * time.Millisecond)
}

// runInfoFlag handles the flags that print something instead of running:
// --version and --list-mutators. done reports whether one was set, and
// the run ends there.
func runInfoFlag(opts *cliOptions) (done bool, err error) {
	if opts.showVersion {
		fmt.Fprint(stdout, formatVersion())
		return true, nil
	}
	if opts.listMutators {
		return true, writeMutatorCatalog(stdout, mutator.NewRegistry().Catalog())
	}
	return false, nil
}

func run(ctx context.Context, args []string) error {
	opts, err := parseFlags(args)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}

	if done, err := runInfoFlag(opts); done {
		return err
	}

	cfg, filters, err := loadConfig(opts)
	if err != nil {
		return err
	}
	mr := &mutationRun{cfg: cfg, opts: opts.runOptions, filters: filters}

	if err := mr.setup(ctx); err != nil {
		return err
	}

	if err := mr.resolvePackages(ctx); err != nil {
		return err
	}

	if err := mr.checkRunMutantID(ctx); err != nil {
		return err
	}

	mr.resolveCoverageScope(ctx)

	if err := mr.makeTempDir(); err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(mr.tmpDir) }()

	if err := mr.collectCoverage(ctx); err != nil {
		return err
	}

	if err := mr.measureBaseline(ctx); err != nil {
		return err
	}

	parsed, err := mr.discoverMutants(ctx)
	if err != nil {
		return err
	}

	if mr.cfg.DryRun {
		mr.printDryRun()
		return nil
	}

	// parsed is not used past this call, so the ASTs it holds are
	// collectable for the rest of the run.
	if err := mr.preReadSources(parsed); err != nil {
		return err
	}

	if err := mr.buildTestMap(ctx); err != nil {
		return err
	}

	mr.applyCache()

	mr.runMutants(ctx)

	r, err := mr.writeReports()
	if err != nil {
		return err
	}

	return mr.checkThresholds(r)
}

// runMutantDroppedError explains which filter dropped the mutant that
// --run-mutant-id had already resolved. A suppression carries its own
// reason, written either at the site or by --exclude-calls; when nothing
// was suppressed, --changed-since is what narrowed the run.
func runMutantDroppedError(id, changedSince string, suppressed []discover.Suppression) error {
	if len(suppressed) > 0 {
		return fmt.Errorf("the mutant matching --run-mutant-id %q is suppressed at %s:%d (%s)",
			id, suppressed[0].Mutant.RelFile, suppressed[0].Mutant.Line, suppressionReason(suppressed[0]))
	}
	return fmt.Errorf("the mutant matching --run-mutant-id %q is not on any line changed since %q",
		id, changedSince)
}

// suppressionReason is a suppression's reason as gomutants prints it: the
// one written at the site or by --exclude-calls, else "no reason".
func suppressionReason(s discover.Suppression) string {
	if s.Reason == "" {
		return "no reason"
	}
	return s.Reason
}

// warnInfraErrors notes on stderr that part of the run never produced a
// verdict. The terminal summary already prints the count, but that goes to
// stdout for a human watching the run: a CI job that keeps only the JSON
// report and the exit code would otherwise see a green threshold gate
// computed over fewer mutants than were discovered, with nothing saying so.
func warnInfraErrors(w io.Writer, r *report.Report) {
	if r.MutantsInfraError == 0 {
		return
	}
	fmt.Fprintf(w, "gomutants: %d of %d mutants ended in INFRA ERROR (host resource or I/O failure); "+
		"they are excluded from both threshold gates and were not cached — rerun once the host is healthy for a complete measurement\n",
		r.MutantsInfraError, r.MutantsTotal)
}

// runGoVersion shells out to `go version` once so the coverage cache key
// can fingerprint the toolchain that actually compiles the tests
// (independent of runtime.Version() for the gomutants binary itself).
// Swappable via goVersionFunc for tests that want deterministic output.
//
// Returns a sentinel on failure so the "go binary unavailable" mode is
// distinct from any conceivable successful empty output — collapsing
// both into "" would let the cache survive a toolchain swap if the
// other inputs happen to stay constant.
func runGoVersion(ctx context.Context) string {
	cmd := exec.CommandContext(ctx, "go", "version")
	out, err := cmd.Output()
	if err != nil {
		return "go-version-unavailable"
	}
	return strings.TrimSpace(string(out))
}

// timeoutPolicyFor builds the per-mutant deadline policy. global is the
// baseline×coefficient ceiling, which stays the absolute cap in every
// mode. --test-flags needs no special case: the per-test timings come
// from the coverage map's runs, which apply the same flags as the mutant
// runs (see coverageTestFlags).
//
// With adaptive sizing on, --timeout-min floors the ceiling too: a small
// module's baseline×coefficient can sit below it (1.887s against a 2s
// floor), and the mutants that fall back to the ceiling would get less
// time than the floor promises every mutant.
func timeoutPolicyFor(cfg *config.Config, global time.Duration) runner.TimeoutPolicy {
	adaptive := cfg.AdaptiveTimeoutEnabled()
	if adaptive {
		global = max(global, cfg.TimeoutMin)
	}
	return runner.TimeoutPolicy{
		Global:   global,
		Margin:   cfg.TimeoutMargin,
		Min:      cfg.TimeoutMin,
		Adaptive: adaptive,
	}
}

// managedTestFlags are inner `go test` flags gomutants sets for itself,
// mapped to the reason a user value can't be honored. Passing one through
// --test-flags would not fail loudly, it would quietly produce a wrong
// run: a replaced -overlay means no mutant is ever applied and every one
// of them "survives", which reads as a legitimate result. Rejecting at
// parse time is the only point where the user still finds out.
var managedTestFlags = map[string]string{
	"overlay":      "each mutant is applied through gomutants' own overlay",
	"run":          "the test filter comes from the per-test coverage map",
	"coverprofile": "gomutants owns the coverage profile path; see --coverpkg",
	"coverpkg":     "use gomutants' own --coverpkg flag",
	"c":            "gomutants builds each mutant's test binaries itself",
	"o":            "gomutants builds each mutant's test binaries itself",
	"exec":         "gomutants invokes the test binary itself",
	// gomutants computes each mutant's deadline from the adaptive-timeout
	// policy and cuts every test-binary run off at it, so a longer user
	// value would be silently overridden (the mutant still lands as
	// TIMED_OUT) rather than honored.
	"timeout": "the per-mutant deadline is computed by gomutants; see --timeout-coefficient, --timeout-margin, and --timeout-min",
}

// checkTestFlags rejects --test-flags entries that collide with a flag
// gomutants manages. Matching is on the flag name via config.FlagName, so
// `-overlay=x`, `-overlay x`, `--overlay`, and the `-test.`-prefixed
// spelling are all caught, while a non-flag field never matches.
//
// The `-test.` spelling has to be covered because `go test` forwards it
// straight to the test binary, where it wins over the flag gomutants set:
// `go test -run=A -test.run=B` runs B, and `-test.coverprofile` writes
// the profile somewhere other than where RunCoverage looks for it. Those
// are the same silent wrongness the un-prefixed names are rejected for.
//
// -args and the `--` terminator relax the scan, because gomutants has
// already placed the package and its own flags ahead of the user fields:
// past a boundary the go command claims nothing more, so an unprefixed
// name there belongs to the test binary and can override nothing. They
// relax it by different amounts. -args forwards the rest verbatim, so a
// managed `-test.*` spelling still lands in the test binary and beats the
// flag gomutants set — those stay rejected. `--` is forwarded too, but
// the test binary's own flag parser stops at it and treats everything
// after as positional, so nothing behind it can override anything.
//
// Neither relaxation applies to a boundary the go command may never have
// read as one; see boundaryConsumed.
func checkTestFlags(flags []string) error {
	afterArgs := false
	for i, f := range flags {
		if isFlagBoundary(f) && !boundaryConsumed(flags, i) {
			if f == "--" {
				return nil
			}
			afterArgs = true
			continue
		}
		if afterArgs {
			spelled, _, _ := strings.Cut(strings.TrimLeft(f, "-"), "=")
			if !strings.HasPrefix(spelled, "test.") {
				continue
			}
		}
		// FlagName yields "" for a field that isn't a flag at all (a
		// detached value, as in `-gcflags all=-N`), and "" is never a
		// managed name — so a value that happens to spell one can't trip
		// the check, with no separate skip branch to keep in sync.
		name, _ := config.FlagName(f)
		if reason, ok := managedTestFlags[name]; ok {
			// Echo the spelling the user typed, not the canonical name:
			// being told "-run is managed" after passing `-test.run` reads
			// like the wrong flag was rejected.
			spelled, _, _ := strings.Cut(f, "=")
			return fmt.Errorf("--test-flags: %s is managed by gomutants and cannot be overridden (%s)", spelled, reason)
		}
	}
	return nil
}

// isFlagBoundary reports whether a field is one of the tokens that ends
// the go command's own argument claiming: -args (either spelling) or the
// `--` terminator.
func isFlagBoundary(f string) bool {
	return f == "-args" || f == "--args" || f == "--"
}

// boundaryConsumed reports whether the boundary token at index i may have
// been read as the *value* of the field before it rather than as a
// boundary at all.
//
// Which parser does the consuming depends on where the token sits. Before
// any -args, it is the go command: `go test pkg -bench -args -overlay=x`
// binds "-args" to -bench, so -overlay is still parsed by the go command
// and silently replaces the mutation overlay — the precise failure the
// guard exists to prevent, waved through by a relaxation that assumed a
// boundary was there. After an -args the go command claims nothing more,
// but the test binary's own flag parser takes over and behaves the same
// way: flag.parseOne assigns the next argument to a value-taking flag
// without inspecting it, so in `-args -x -- -test.run=X` a non-boolean -x
// swallows the "--" and -test.run stays live against the binary. Either
// spelling of the boundary is swallowed by either parser.
//
// Deciding this exactly would take the set of go test and build flags
// that accept a value — plus, past -args, the flags of a test binary that
// has not been compiled yet. The first changes between Go releases and
// would rot here; the second is not knowable at this point at all. The
// test is syntactic instead: a preceding field that is a flag carrying no
// inline `=` value is one that might consume the next field, so the guard
// declines to relax behind it.
//
// The imprecision only ever errs strict. `-race -args -run=custom` is safe
// in fact — -race is boolean and consumes nothing — but reads as
// ambiguous here and is rejected; spelling the value inline
// (`-race=true -args -run=custom`) restores the boundary. Only the eight
// managed names are affected, so the documented `-args` uses, which pass
// names gomutants does not manage, are untouched either way.
// Over-rejecting costs a diagnostic the user can act on; under-rejecting
// costs a whole run reported wrong.
func boundaryConsumed(fields []string, i int) bool {
	if i == 0 {
		return false
	}
	prev := fields[i-1]
	return strings.HasPrefix(prev, "-") && !strings.Contains(prev, "=")
}

// captureCoverageEnv returns an allowlisted snapshot of go-related env
// vars that can change the coverage profile. Restricted to a small set
// so that irrelevant churn (GOPROXY rotation, GOCACHE path changes)
// doesn't invalidate the cache. New entries must actually affect what
// `go test -coverprofile` produces.
func captureCoverageEnv() string {
	keys := []string{"GOEXPERIMENT", "GOFLAGS", "GOOS", "GOARCH", "CGO_ENABLED"}
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s|", k, os.Getenv(k))
	}
	return b.String()
}

// dirsOfPackages returns the unique directories of the given packages,
// preserving first-seen order.
func dirsOfPackages(pkgs []discover.Package) []string {
	dirs := make([]string, 0, len(pkgs))
	seen := make(map[string]bool, len(pkgs))
	for _, p := range pkgs {
		if !seen[p.Dir] {
			seen[p.Dir] = true
			dirs = append(dirs, p.Dir)
		}
	}
	return dirs
}

// suiteDirs returns the directory of each package suite in tm's scope (see
// coverage.TestMap.Suites); a nil map has none.
func suiteDirs(tm *coverage.TestMap) []string {
	var dirs []string
	for _, p := range tm.Suites() {
		dirs = append(dirs, p.Dir)
	}
	return dirs
}

// embedFilesByDir maps each package's directory to the dir-relative paths
// its production //go:embed directives resolved to, for cache.Hasher's
// pkg_hash. Packages that embed nothing are left out of the map entirely —
// an absent directory and one mapped to an empty list hash identically, and
// the sparse map is the smaller thing to carry.
//
// Two packages never share a directory, so a plain assignment is enough;
// the map is keyed by directory rather than import path because that is
// what HashPkgFiles is called with.
func embedFilesByDir(pkgs []discover.Package) map[string][]string {
	embeds := make(map[string][]string)
	for _, p := range pkgs {
		if len(p.EmbedFiles) > 0 {
			embeds[p.Dir] = p.EmbedFiles
		}
	}
	return embeds
}

// testFilesResolver returns the cache's resolver from a mutant to the test
// files its verdict depends on: every _test.go in the mutant's own package,
// plus the files declaring the tests the coverage map attributes to this
// mutant (which -coverpkg can place in another package). The local half is
// unconditional rather than a fallback because a package-level test helper
// declares no test entry point, so no coverage-derived name resolves to
// it — see CoveringFiles. A nil coverage map just means there are no names
// to add.
//
// crossPkg tells CoveringFiles whether a covering test resolving to a
// foreign directory can be a real cross-package dependency or is just two
// packages declaring the same test name. It takes an instrumentation scope
// wider than the package under test — --integration, or an explicit
// --coverpkg — for a test outside the mutant's package to record coverage
// on it at all; without one, TestsFor's package-agnostic names are the
// only way a foreign directory can turn up, and expanding on them would
// fold unrelated packages' sources into tests_hash.
func testFilesResolver(testIndex *cache.TestIndex, testMap *coverage.TestMap, crossPkg bool) cache.TestFilesForFn {
	return func(m mutator.Mutant) []string {
		var names []string
		if testMap != nil {
			names = testMap.TestsFor(m.CoverageFile, m.Line)
		}
		dir := filepath.Dir(m.File)
		files := testIndex.CoveringFiles(dir, names, crossPkg)
		// A survivor is re-checked against every package suite that can
		// kill it, so every file of them decides the verdict. The own
		// package is skipped for the reason CoveringFiles skips it: its
		// sources are already the production dimension.
		for _, p := range testMap.SuitePkgs(m.Pkg) {
			if p.Dir != dir {
				files = append(files, testIndex.PackageFiles(p.Dir)...)
			}
		}
		return files
	}
}

// pendingLines returns the positions of the mutants still to be tested, in
// the coverage map's key format: the lines whose groups of covering tests
// the map checks pass together (see coverage.BuildOptions.Lines).
func pendingLines(mutants []mutator.Mutant) map[string]bool {
	lines := map[string]bool{}
	for _, m := range mutants {
		if m.Status == mutator.StatusPending {
			lines[coverage.LineKey(m.CoverageFile, m.Line)] = true
		}
	}
	return lines
}

// coverageTestFlags returns the flags the mutant runs pass to `go test`
// that shape how their tests run — -cpu for --test-cpu and -short when the
// runner adds it, then the user's --test-flags, in the mutant runs' order
// so a repeated flag resolves the same way — so the per-test coverage
// runs match them. Their timings size the mutant runs' deadlines: a test
// timed at every core and run at -cpu=1 would outlast its deadline.
func coverageTestFlags(userFlags []string, short bool, testCPU int) []string {
	var flags []string
	if testCPU > 0 {
		flags = append(flags, fmt.Sprintf("-cpu=%d", testCPU))
	}
	if short {
		flags = append(flags, "-short")
	}
	return append(flags, userFlags...)
}

// integrationScope computes the reverse-dependency closure of the target
// packages for --integration mode: the `go test` patterns to run (R), the
// package directories of R (for the cache test index and the coverage-key
// hash), and the -coverpkg value (the targets themselves, so importing tests
// record coverage on the mutated code). On any failure it falls back to the
// whole module (./...) with -coverpkg pinned to the targets, warning that
// this can be slow on large modules.
func integrationScope(ctx context.Context, projectDir, goModule string, pkgs []discover.Package, tags string, stderr io.Writer) (patterns, dirs []string, coverPkg string) {
	targets := make([]string, 0, len(pkgs))
	for _, p := range pkgs {
		targets = append(targets, p.ImportPath)
	}
	rPatterns, rDirs, cpkg, err := discover.IntegrationClosure(ctx, projectDir, targets, goModule, tags)
	if err == nil {
		return rPatterns, rDirs, cpkg
	}
	fmt.Fprintf(stderr, "gomutants: integration closure failed (%v); falling back to ./... — this can be slow on large modules\n", err)
	coverPkg = strings.Join(targets, ",")
	if all, rerr := discover.ResolvePackages(ctx, projectDir, []string{"./..."}, tags); rerr == nil {
		return []string{"./..."}, dirsOfPackages(all), coverPkg
	}
	return []string{"./..."}, dirsOfPackages(pkgs), coverPkg
}

// coveragePkgDirs returns the set of package directories whose source
// could land in the coverage profile. With cfg.CoverPkg unset (or
// matching the target patterns), this is exactly `pkgs`. With a
// broader -coverpkg pattern, we resolve it separately so the hash
// covers every package that go test will instrument.
func coveragePkgDirs(ctx context.Context, projectDir string, pkgs []discover.Package, coverPkg, tags string) ([]string, error) {
	dirs := dirsOfPackages(pkgs)
	if coverPkg == "" {
		return dirs, nil
	}
	seen := make(map[string]bool, len(dirs))
	for _, d := range dirs {
		seen[d] = true
	}
	expanded, err := resolveCoverPkgFunc(ctx, projectDir, []string{coverPkg}, tags)
	if err != nil {
		return nil, err
	}
	for _, p := range expanded {
		if !seen[p.Dir] {
			seen[p.Dir] = true
			dirs = append(dirs, p.Dir)
		}
	}
	return dirs, nil
}

// readModuleName extracts the module path from go.mod by scanning for
// the first `module <path>` line. Tolerates tabs and multiple spaces
// between the directive and the path, and ignores inline comments
// (`module foo // comment`) — fields[1] is always the path for a
// well-formed go.mod. Prefer golang.org/x/mod/modfile if parsing ever
// needs to handle deprecation markers, the rare `module ( ... )` block
// form, or quoted module paths.
func readModuleName(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return "", fmt.Errorf("reading go.mod: %w", err)
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "module" {
			return fields[1], nil
		}
	}
	// bufio.Scanner caps lines at 64 KiB by default. A pathological
	// go.mod with a longer-than-cap line before the module directive
	// would surface as bufio.ErrTooLong here — we propagate it rather
	// than silently returning "module name not found", which would
	// mislead the user about what's wrong.
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("scanning go.mod: %w", err)
	}
	return "", fmt.Errorf("module name not found in go.mod")
}
