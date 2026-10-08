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

	"github.com/szhekpisov/gomutants/internal/cache"
	"github.com/szhekpisov/gomutants/internal/config"
	"github.com/szhekpisov/gomutants/internal/coverage"
	"github.com/szhekpisov/gomutants/internal/discover"
	"github.com/szhekpisov/gomutants/internal/mutator"
	"github.com/szhekpisov/gomutants/internal/report"
	"github.com/szhekpisov/gomutants/internal/runner"
	"github.com/szhekpisov/gomutants/internal/tce"
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
)

// phaseDurationDisplay rounds a duration to 100ms precision for display
// in phase-done lines. Extracted so the rounding constant is testable
// without parsing fmt output: ARITHMETIC mutations on `100*time.Millisecond`
// (e.g. `*` → `/`, which collapses to 0 and disables rounding) surface as
// observable changes in the returned Duration.
func phaseDurationDisplay(d time.Duration) time.Duration {
	return d.Round(100 * time.Millisecond)
}

func run(ctx context.Context, args []string) error {
	opts, err := parseFlags(args)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}

	if opts.showVersion {
		fmt.Fprint(stdout, formatVersion())
		return nil
	}
	if opts.listMutators {
		return writeMutatorCatalog(stdout, mutator.NewRegistry().Catalog())
	}

	cfg, filters, err := loadConfig(opts)
	if err != nil {
		return err
	}
	mr := &mutationRun{cfg: cfg, opts: opts, filters: filters}

	if err := mr.setup(ctx); err != nil {
		return err
	}

	if err := mr.resolvePackages(ctx); err != nil {
		return err
	}

	// 2. Create temp directory.
	mr.tmpDir, err = mkdirTempFunc("", "gomutants-*")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(mr.tmpDir) }()

	if err := mr.collectCoverage(ctx); err != nil {
		return err
	}

	if err := mr.measureBaseline(ctx); err != nil {
		return err
	}

	if err := mr.discoverMutants(ctx); err != nil {
		return err
	}

	if mr.cfg.DryRun {
		mr.printDryRun()
		return nil
	}

	if err := mr.preReadSources(); err != nil {
		return err
	}

	if err := mr.buildTestMap(ctx); err != nil {
		return err
	}

	mr.applyCache()

	// 8. Run mutation testing. pool.Run mutates the slice in place.
	// TimeoutPolicy resolves per-mutant deadlines from the per-test
	// durations recorded on the testMap, falling back to the global
	// baseline×coefficient ceiling. testTimeout stays the absolute cap.
	policy := timeoutPolicyFor(&mr.cfg, mr.testTimeout)
	term2 := report.NewTerminal(stdout, mr.pendingCount, mr.cfg.Verbose, mr.cfg.Quiet)
	// Idle "(compiling)" heartbeat so the TTY doesn't sit silent during
	// the first per-package go-test compile (no OnResult until the first
	// mutant completes). First OnResult auto-stops it; the defer covers
	// the all-cached / zero-pending paths where OnResult never fires.
	term2.StartHeartbeat()
	defer term2.StopHeartbeat()

	// Stamp the coverage memo once, before the run loop: the profile and
	// its key are fixed for the whole run, so there's no reason to
	// re-serialize the (potentially large) profile on every checkpoint.
	// An empty coverageKey means hashing failed earlier and we silently
	// fell back to a fresh run; don't poison the cache with a missing key.
	if mr.loadedCache != nil && mr.coverageKey != "" && len(mr.profileBytes) > 0 {
		mr.loadedCache.CoverageKey = mr.coverageKey
		mr.loadedCache.CoverageProfile = string(mr.profileBytes)
	}

	// 8a. checkpoint flushes completed mutant outcomes to the cache file,
	// throttled to cfg.CheckpointInterval. cache.Update only emits
	// terminal-status mutants, so flushing a partially-complete slice
	// mid-run is safe — pending mutants are simply omitted. No locking
	// needed: pool.Run invokes onResult from a single goroutine, so the
	// mutants-slice reads here are serialized with the collector's writes.
	// Write failures are non-fatal — a stale cache only costs speed.
	checkpoint := func(force bool) {
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

	pool := runner.NewPool(mr.cfg.Workers, runner.ExecOpts{TestCPU: mr.cfg.TestCPU, Tags: mr.cfg.Tags, TestFlags: mr.cfg.TestFlagFields()}, policy, mr.tmpDir, mr.srcCache, mr.projectDir, mr.testMap)
	// Seed lastCheckpoint so the first periodic checkpoint fires one full
	// interval into the run, not on the very first mutant.
	mr.lastCheckpoint = time.Now()
	pool.Run(ctx, mr.mutants, func(m mutator.Mutant) {
		term2.OnResult(m)
		checkpoint(false)
	})

	// 8b. Trivial Compiler Equivalence pass (opt-in). Recompile each
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
			checkpoint(false)
		})
		mr.term.PhaseDone(fmt.Sprintf("%d equivalent", equiv))
	}

	// Final flush. force=true bypasses the throttle and the disable
	// switch, so even --checkpoint-interval=0 still writes the cache once.
	checkpoint(true)

	// 9. Generate report.
	totalElapsed := time.Since(mr.coverStart)
	r := report.Generate(mr.mutants, mr.goModule, totalElapsed, len(mr.suppressed))
	// Breakdown only; the aggregate stays in MutantsSuppressed so the two
	// suppression sources share one bucket everywhere else.
	r.MutantsSuppressedByCalls = len(mr.callSuppressed)
	term2.Summary(r)

	if err := report.WriteJSON(r, mr.cfg.Output); err != nil {
		return fmt.Errorf("writing report: %w", err)
	}
	if !mr.cfg.Quiet {
		fmt.Fprintf(stdout, "Report: %s\n", mr.cfg.Output)
	}

	if opts.strykerOutput != "" {
		if err := report.WriteStryker(opts.strykerOutput, mr.mutants, mr.projectDir, effectiveVersion()); err != nil {
			return fmt.Errorf("writing Stryker report: %w", err)
		}
		if !mr.cfg.Quiet {
			fmt.Fprintf(stdout, "Stryker report: %s\n", opts.strykerOutput)
		}
	}

	if opts.htmlOutput != "" {
		if err := report.WriteHTML(opts.htmlOutput, mr.mutants, mr.projectDir, effectiveVersion()); err != nil {
			return fmt.Errorf("writing HTML report: %w", err)
		}
		fmt.Fprintf(stdout, "HTML report: %s\n", opts.htmlOutput)
	}

	if opts.annotations == "github" {
		if err := report.WriteGitHubAnnotations(stdout, r); err != nil {
			return fmt.Errorf("writing annotations: %w", err)
		}
	}

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
	// rather than let a script read it as a kill. mutants holds exactly the
	// one FilterByStableID returned; anything that empties it has already
	// returned above.
	if mr.cfg.RunMutantID != "" {
		if s := mr.mutants[0].Status; s != mutator.StatusKilled && s != mutator.StatusLived {
			return fmt.Errorf("--run-mutant-id %q produced no verdict: the mutant is %s", mr.cfg.RunMutantID, s)
		}
	}
	tested := r.MutantsKilled + r.MutantsLived
	mcoverDenom := tested + r.MutantsNotCovered
	mcover := 0.0
	if mcoverDenom > 0 {
		mcover = float64(tested) / float64(mcoverDenom) * 100
	}

	if opts.thresholdEfficacy > 0 {
		if tested == 0 {
			fmt.Fprintln(stderr, "gomutants: no testable mutants discovered; --threshold-efficacy not evaluated")
		} else if r.TestEfficacy < opts.thresholdEfficacy {
			return &exitError{
				code: exitCodeEfficacy,
				err:  fmt.Errorf("test efficacy %.2f%% below --threshold-efficacy=%.2f%% (mutant coverage: %.2f%%)", r.TestEfficacy, opts.thresholdEfficacy, mcover),
			}
		}
	}
	if opts.thresholdMcover > 0 {
		if mcoverDenom == 0 {
			fmt.Fprintln(stderr, "gomutants: no covered or testable mutants discovered; --threshold-mcover not evaluated")
		} else if mcover < opts.thresholdMcover {
			return &exitError{
				code: exitCodeMutantCoverage,
				err:  fmt.Errorf("mutant coverage %.2f%% below --threshold-mcover=%.2f%% (test efficacy: %.2f%%)", mcover, opts.thresholdMcover, r.TestEfficacy),
			}
		}
	}
	return nil
}

// runMutantDroppedError explains which filter dropped the mutant that
// --run-mutant-id had already resolved. A suppression carries its own
// reason, written either at the site or by --exclude-calls; when nothing
// was suppressed, --changed-since is what narrowed the run.
func runMutantDroppedError(id, changedSince string, suppressed []discover.Suppression) error {
	if len(suppressed) > 0 {
		reason := suppressed[0].Reason
		if reason == "" {
			reason = "no reason"
		}
		return fmt.Errorf("the mutant matching --run-mutant-id %q is suppressed at %s:%d (%s)",
			id, suppressed[0].Mutant.RelFile, suppressed[0].Mutant.Line, reason)
	}
	return fmt.Errorf("the mutant matching --run-mutant-id %q is not on any line changed since %q",
		id, changedSince)
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
func timeoutPolicyFor(cfg *config.Config, global time.Duration) runner.TimeoutPolicy {
	return runner.TimeoutPolicy{
		Global:   global,
		Margin:   cfg.TimeoutMargin,
		Min:      cfg.TimeoutMin,
		Adaptive: cfg.AdaptiveTimeoutEnabled(),
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
	"c":            "gomutants runs tests here, it does not build binaries",
	"o":            "gomutants runs tests here, it does not build binaries",
	"exec":         "gomutants invokes the test binary itself",
	// gomutants computes -timeout from the adaptive-timeout policy and
	// caps every run with a context deadline of the same length, so a
	// longer user value is silently overridden (the mutant still lands as
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
