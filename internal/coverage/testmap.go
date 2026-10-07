package coverage

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// tagsBuildFlag is the `go` build-tags flag prefix; the configured tags
// value is appended to it when non-empty.
const tagsBuildFlag = "-tags="

// Function variables for testing.
var (
	resolvePackagesFunc   = resolvePackages
	listTestsFunc         = listTests
	listBinTestsFunc      = listBinTests
	testBinaryArgsFunc    = testBinaryArgs
	parseFileFunc         = ParseFile
	compileTestBinaryFunc = compileTestBinary
	runCompiledTestFunc   = runCompiledTest
	statFileFunc          = os.Stat
)

// TestMap maps (file, line) positions to the test functions that cover them.
//
// Concurrency: TestMap is built sequentially in BuildTestMap (the
// receive loop is single-goroutine even though the producers are
// parallel) and is treated as immutable thereafter. The runner's
// per-mutant Worker.Test reads it concurrently across N workers
// without locking — that's safe only because the build phase
// completes before any worker reads. Don't expose new methods that
// mutate TestMap state after BuildTestMap returns, or the lock-free
// read assumption breaks.
type TestMap struct {
	// index maps "file:line" to the set of covering tests, keyed by
	// (pkg, name). Package context is retained so cross-package integration
	// routing can dispatch a covering test to its own `go test` invocation;
	// TestsFor projects the names out for the package-agnostic cache path.
	index map[string]map[testKey]bool

	// durations is the per-test execution time captured during the
	// per-test coverage build, keyed by (pkg, test). Per-mutant adaptive
	// timeout reads from this directly. Same-named tests in different
	// packages get distinct entries because go test scopes -run within
	// a single package.
	durations map[testKey]time.Duration

	// pkgDurations is the running sum of per-test durations per package,
	// used as the per-package fallback when a mutant has no per-test
	// covering set (e.g. mutated line outside any covered block). Kept
	// alongside durations so package totals don't require a fresh
	// O(n) scan on every Worker.computeTimeout call.
	pkgDurations map[string]time.Duration

	// unmapped holds the packages whose tests the map could not cover
	// test by test, keyed by import path. Routing a mutant to a subset of
	// such a package's tests could skip the one that kills it, so the
	// runner runs these packages in full instead (see FullRunPkgs).
	unmapped map[string]UnmappedPkg

	// crossPkg reports whether the test binaries were built with
	// -coverpkg, so a test can record coverage outside its own package
	// and an unmapped package's tests may cover a mutant anywhere.
	crossPkg bool
}

// UnmappedPkg is a package whose tests the coverage map could not cover
// test by test, with the reason why.
type UnmappedPkg struct {
	ImportPath string
	Dir        string
	Reason     string
}

func newTestMap(crossPkg bool) *TestMap {
	return &TestMap{
		index:        make(map[string]map[testKey]bool),
		durations:    make(map[testKey]time.Duration),
		pkgDurations: make(map[string]time.Duration),
		unmapped:     make(map[string]UnmappedPkg),
		crossPkg:     crossPkg,
	}
}

// markUnmapped records that pkg's tests can't be routed individually. The
// first reason is kept: it is the one a user would fix first.
func (tm *TestMap) markUnmapped(importPath, dir, reason string) {
	if _, ok := tm.unmapped[importPath]; !ok {
		tm.unmapped[importPath] = UnmappedPkg{ImportPath: importPath, Dir: dir, Reason: reason}
	}
}

// Unmapped returns every package the map could not cover test by test,
// sorted by import path.
func (tm *TestMap) Unmapped() []UnmappedPkg {
	if tm == nil {
		return nil
	}
	pkgs := slices.Collect(maps.Values(tm.unmapped))
	slices.SortFunc(pkgs, func(a, b UnmappedPkg) int { return strings.Compare(a.ImportPath, b.ImportPath) })
	return pkgs
}

// FullRunPkgs returns the unmapped packages whose whole test suite must
// run for a mutant in pkg, sorted by import path. Without -coverpkg a test
// covers only its own package, so that is pkg itself when it is unmapped;
// with it, any unmapped package's tests may cover the mutant.
func (tm *TestMap) FullRunPkgs(pkg string) []UnmappedPkg {
	if tm == nil {
		return nil
	}
	if tm.crossPkg {
		return tm.Unmapped()
	}
	if u, ok := tm.unmapped[pkg]; ok {
		return []UnmappedPkg{u}
	}
	return nil
}

// testKey identifies a single (pkg, test) timing entry. Using a struct
// instead of "pkg::name" string concatenation avoids the parsing cost
// on every lookup and removes a class of mutation surface (string-key
// off-by-one) that would force the timeout selector into a defensive
// trim path.
type testKey struct {
	pkg, name string
}

type testCoverage struct {
	pkg      string
	dir      string
	testName string
	duration time.Duration
	blocks   []Block
	// failure, when set, says why the test's solo run left no usable
	// coverage; its package is then unmapped.
	failure string
}

// compiledPkg holds a pre-compiled test binary for a package.
type compiledPkg struct {
	binPath    string   // Path to compiled test binary.
	importPath string   // Package import path.
	dir        string   // Package directory (for running the binary).
	testArgs   []string // Arguments every run of the binary gets (see setTestArgs).
}

// BuildOptions configures BuildTestMap.
type BuildOptions struct {
	// CoverPkg is the -coverpkg value the test binaries are built with;
	// empty means each package records coverage of itself only.
	CoverPkg string
	// Tags is the -tags value for resolving and compiling packages.
	Tags string
	// TmpDir holds the compiled test binaries and per-worker profiles.
	TmpDir string
	// Workers is the number of tests run in parallel.
	Workers int
	// TestTimeout bounds every run of a compiled test binary (listing and
	// each per-test run); zero means no bound. A test that hangs when run
	// alone would otherwise block the coverage phase forever: unlike
	// `go test`, a bare test binary has no default timeout.
	TestTimeout time.Duration
	// TestFlags are the flags the mutant runs pass to `go test` after the
	// package (the user's --test-flags, plus -short when the runner adds
	// it), so the coverage runs build and run tests the same way.
	TestFlags []string
}

// BuildTestMap compiles each package's test binary once, lists the tests
// each binary contains, then runs every test function alone against its
// binary with coverage. Uses parallel workers.
func BuildTestMap(ctx context.Context, projectDir string, packages []string, opts BuildOptions) (*TestMap, error) {
	// 1. Resolve package patterns to individual packages and compile test binaries.
	resolvedPkgs, err := resolvePackagesFunc(ctx, projectDir, packages, opts.Tags)
	if err != nil {
		return nil, fmt.Errorf("resolving packages: %w", err)
	}

	pkgBins, compileFailures := buildPkgBins(ctx, projectDir, opts, resolvedPkgs)
	// A cancelled ctx fails every remaining compile; report the
	// cancellation, not the compile failures it caused.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Every package with tests failed to compile: the map would come back
	// empty and per-test routing would switch off without a word. Surface
	// it so the caller warns and falls back explicitly (a nil map routes
	// exactly like an empty one).
	if len(pkgBins) == 0 && len(compileFailures) > 0 {
		return nil, fmt.Errorf("no test binary compiled (%d packages failed); first failure: %w",
			len(compileFailures), compileFailures[0].err)
	}
	tm := newTestMap(opts.CoverPkg != "")
	for _, f := range compileFailures {
		tm.markUnmapped(f.pkg.importPath, f.pkg.dir, "its test binary failed to compile")
	}
	if err := setTestArgs(ctx, projectDir, opts, pkgBins); err != nil {
		return nil, err
	}

	// 2. List each binary's tests. Keying them by the binary's import path
	// means every listed test has a binary to run against by construction.
	// A cancellation here surfaces after the workers, which run nothing
	// once the ctx is done.
	tests, listFailures := listTestsFunc(ctx, pkgBins, opts.TestTimeout, opts.Workers)
	for pkg, reason := range listFailures {
		tm.markUnmapped(pkg, pkgBins[pkg].dir, reason)
	}

	// 3. Run tests in parallel using compiled binaries.
	work := make(chan testEntry, len(tests))
	results := make(chan testCoverage, opts.Workers)

	var wg sync.WaitGroup
	for i := range opts.Workers {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			processWork(ctx, work, pkgBins, opts.TmpDir, workerID, opts.TestTimeout, results)
		}(i)
	}

	// Feed work.
	go func() {
		feedWork(ctx, tests, work)
	}()

	// Close results when all workers are done.
	go func() {
		wg.Wait()
		close(results)
	}()

	// 4. Collect and index results.
	for tc := range results {
		tm.ingestResult(tc)
	}
	// Tests skipped after a cancellation leave the map partial, and a
	// partial map routes mutants away from tests that were never run.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return tm, nil
}

// ingestResult folds one per-test outcome into both the coverage index
// and the duration maps. Extracted so STATEMENT_REMOVE on the duration
// recording is observable without needing a full BuildTestMap pipeline
// to assert on (the receive loop is otherwise invisible to a unit
// test that doesn't drive real `go test` invocations).
func (tm *TestMap) ingestResult(tc testCoverage) {
	tm.addBlocks(tc.pkg, tc.testName, tc.blocks)
	tm.recordDuration(tc.pkg, tc.testName, tc.duration)
	// A test that fails alone (typically because it relies on another
	// test running first) is missing from the index, so a mutant routed to
	// its package's other tests would skip it. Only the whole package
	// reproduces the order it passes in.
	if tc.failure != "" {
		tm.markUnmapped(tc.pkg, tc.dir, tc.failure)
	}
}

// recordDuration stores a single (pkg, test) timing and updates the
// rolling per-package sum. Extracted so a duplicate observation (e.g.
// future retry logic re-running a test) accumulates rather than
// overwrites — matching the per-package sum's accumulation behavior.
func (tm *TestMap) recordDuration(pkg, name string, d time.Duration) {
	if d <= 0 {
		return
	}
	k := testKey{pkg: pkg, name: name}
	tm.durations[k] += d
	tm.pkgDurations[pkg] += d
}

// addBlocks indexes a test's coverage blocks into the map.
//
// Extracted so the inner line-stepping loop can be unit-tested directly
// with tiny inputs and a tight deadline — calling BuildTestMap to exercise
// it would let mutations like `line++ → line--` allocate gigabytes of
// map entries before any test-side timer can fire.
func (tm *TestMap) addBlocks(pkg, testName string, blocks []Block) {
	for _, b := range blocks {
		if b.Count == 0 {
			continue
		}
		for line := b.StartLine; line <= b.EndLine; line++ {
			key := b.File + ":" + fmt.Sprint(line)
			if tm.index[key] == nil {
				tm.index[key] = make(map[testKey]bool)
			}
			tm.index[key][testKey{pkg: pkg, name: testName}] = true
		}
	}
}

// processWork processes test entries from the work channel.
func processWork(ctx context.Context, work <-chan testEntry, pkgBins map[string]*compiledPkg, tmpDir string, workerID int, testTimeout time.Duration, results chan<- testCoverage) {
	for test := range work {
		if ctx.Err() != nil {
			return
		}
		cp := pkgBins[test.pkg]
		if cp == nil {
			continue
		}
		profilePath := filepath.Join(tmpDir, fmt.Sprintf("testmap-%d.cov", workerID))
		blocks, dur, err := runCompiledTestFunc(ctx, cp, test.name, profilePath, testTimeout)
		// Forward the timing even when the test produced no blocks: the
		// mutant covering this test still executes it, so its duration
		// matters for the per-mutant timeout. Without this, a fast unit
		// test that touches no shared coverage line gets a 0 contribution
		// and the package sum understates real wall time. A failure is
		// always forwarded, as it unmaps the package.
		if len(blocks) == 0 && dur <= 0 && err == nil {
			continue
		}
		tc := testCoverage{
			pkg:      test.pkg,
			dir:      cp.dir,
			testName: test.name,
			duration: dur,
			blocks:   blocks,
		}
		if err != nil {
			tc.failure = err.Error()
		}
		results <- tc
	}
}

// feedWork sends test entries to the work channel, respecting context cancellation.
func feedWork(ctx context.Context, tests []testEntry, work chan<- testEntry) {
	for _, t := range tests {
		select {
		case work <- t:
		case <-ctx.Done():
			close(work)
			return
		}
	}
	close(work)
}

// compileFailure is a package whose test binary failed to compile.
type compileFailure struct {
	pkg resolvedPkg
	err error
}

// buildPkgBins compiles each package's test binary and indexes the results
// by import path. Failures are non-fatal — the package is skipped and the
// rest keep going — but every one other than a package without test files
// is returned, so the caller can tell "nothing to test" from "nothing
// compiled" and stop routing the failed packages' mutants per test.
// Extracted from BuildTestMap so the skip behavior can be tested without
// driving the whole pipeline.
func buildPkgBins(ctx context.Context, projectDir string, opts BuildOptions, pkgs []resolvedPkg) (map[string]*compiledPkg, []compileFailure) {
	pkgBins := make(map[string]*compiledPkg)
	var failures []compileFailure
	for _, pkg := range pkgs {
		cp, err := compileTestBinaryFunc(ctx, projectDir, opts, pkg)
		if err != nil {
			if !errors.Is(err, errNoTestBinary) {
				failures = append(failures, compileFailure{pkg: pkg, err: err})
			}
			continue
		}
		pkgBins[pkg.importPath] = cp
	}
	return pkgBins, failures
}

// errNoTestBinary marks a package `go test -c` built without error but
// without writing a binary: it has no test files, so there is nothing to
// map rather than something that failed.
var errNoTestBinary = errors.New("no test files")

// compileTestBinary compiles `pkg`'s test binary into tmpDir and returns
// the compiledPkg metadata. Errors from `go test -c` (with its stderr) and
// from a missing output file (wrapping errNoTestBinary) are folded into
// the returned error so callers can `continue` on a single check.
//
// opts.TestFlags follow the package, as in the mutant runs: `go test -c`
// applies the build flags among them and ignores the rest.
func compileTestBinary(ctx context.Context, projectDir string, opts BuildOptions, pkg resolvedPkg) (*compiledPkg, error) {
	binPath := filepath.Join(opts.TmpDir, "testbin-"+sanitize(pkg.importPath)+".test")
	args := []string{"test", "-c", "-o", binPath, "-cover"}
	if opts.CoverPkg != "" {
		args = append(args, "-coverpkg="+opts.CoverPkg)
	}
	if opts.Tags != "" {
		args = append(args, tagsBuildFlag+opts.Tags)
	}
	args = append(args, pkg.importPath)
	args = append(args, opts.TestFlags...)

	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = projectDir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("go test -c %s: %w\n%s", pkg.importPath, err, stderr.String())
	}
	if _, err := statFileFunc(binPath); err != nil {
		return nil, fmt.Errorf("test binary missing for %s: %w: %w", pkg.importPath, errNoTestBinary, err)
	}
	return &compiledPkg{
		binPath:    binPath,
		importPath: pkg.importPath,
		dir:        pkg.dir,
	}, nil
}

// runCompiledTest runs a pre-compiled test binary for a single test with
// coverage and reports the wall-clock duration alongside the parsed blocks.
//
// The duration is reported even on parse failure or non-zero exit: the
// per-mutant adaptive timeout uses these timings, and a flaky test that
// happens to fail during the build phase still represents real work the
// runner will do. Timing only the success path would systematically
// understate timeouts for packages with environment-sensitive tests.
//
// Note on context cancellation: if `ctx` is cancelled mid-run (e.g. an
// upstream SIGINT during the coverage build), the returned duration is
// the partial run-time at cancellation, not the full test cost. That
// can under-record the per-test timing for that one entry; the
// adaptive selector's per-package fallback and the global ceiling
// absorb the impact, but be aware that one cancelled coverage build
// can leave behind tighter-than-real timings until the next clean run.
//
// A run cut off by `timeout` reports the timeout as its duration.
//
// The error says why the run left no usable coverage — the test failed,
// timed out, or its profile couldn't be read — and reads as the reason its
// package can't be mapped.
func runCompiledTest(ctx context.Context, cp *compiledPkg, testName, profilePath string, timeout time.Duration) ([]Block, time.Duration, error) {
	// Ours go first: a positional argument among testArgs (after -args)
	// ends the binary's flag parsing.
	args := append([]string{
		fmt.Sprintf("-test.run=^%s$", regexp.QuoteMeta(testName)),
		"-test.coverprofile=" + profilePath,
	}, cp.testArgs...)

	runCtx, cancel := withTestTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, cp.binPath, args...)
	cmd.Dir = cp.dir

	start := time.Now()
	runErr := cmd.Run()
	dur := time.Since(start)

	if runErr != nil {
		if runCtx.Err() == context.DeadlineExceeded {
			return nil, dur, fmt.Errorf("%s timed out after %s when run alone", testName, timeout)
		}
		return nil, dur, fmt.Errorf("%s failed when run alone: %w", testName, runErr)
	}

	profile, err := parseFileFunc(profilePath)
	if err != nil {
		return nil, dur, fmt.Errorf("%s: reading its coverage profile: %w", testName, err)
	}
	return profile.blocks, dur, nil
}

// withTestTimeout bounds one run of a compiled test binary. The deadline
// is enforced by killing the process rather than through -test.timeout,
// which only starts counting inside m.Run and so misses a TestMain that
// hangs in its setup.
func withTestTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

// TestRef identifies a single covering test by its package import path and
// test function name. Cross-package routing needs the package so a covering
// test can be dispatched to a `go test` invocation against its own package.
type TestRef struct {
	Pkg  string
	Name string
}

// TestRefsFor returns the (pkg, name) references of the tests covering the
// given position. Returns nil if no mapping exists (caller should run all
// tests). The runner uses this to preserve package context across packages.
func (tm *TestMap) TestRefsFor(file string, line int) []TestRef {
	if tm == nil {
		return nil
	}
	key := file + ":" + fmt.Sprint(line)
	testSet := tm.index[key]
	if len(testSet) == 0 {
		return nil
	}
	refs := make([]TestRef, 0, len(testSet))
	for k := range testSet {
		refs = append(refs, TestRef{Pkg: k.pkg, Name: k.name})
	}
	return refs
}

// TestsFor returns the test function names that cover the given position,
// deduplicated across packages. Returns nil if no mapping exists (caller
// should run all tests). Used by the package-agnostic cache resolver; the
// runner uses TestRefsFor to keep package context.
func (tm *TestMap) TestsFor(file string, line int) []string {
	if tm == nil {
		return nil
	}
	key := file + ":" + fmt.Sprint(line)
	testSet := tm.index[key]
	if len(testSet) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(testSet))
	tests := make([]string, 0, len(testSet))
	for k := range testSet {
		if !seen[k.name] {
			seen[k.name] = true
			tests = append(tests, k.name)
		}
	}
	return tests
}

// SumDurationsFor returns the total observed wall-time of `tests` within
// `pkg` (sum of per-test timings recorded during the coverage build),
// and reports whether every requested test had a recorded timing.
//
// Callers use the returned `complete` flag to decide whether the sum
// is trustworthy: if any test was missing (e.g. it failed during the
// coverage build and produced 0 duration, or the test was added after
// build) the per-package fallback should be used instead.
//
// On a nil TestMap, returns (0, false). Empty tests slice short-circuits
// to (0, false) so the per-package fallback is reached without a misread
// of "all 0 of 0 tests had data → complete=true → use 0 timeout".
func (tm *TestMap) SumDurationsFor(pkg string, tests []string) (time.Duration, bool) {
	if tm == nil || len(tests) == 0 {
		return 0, false
	}
	var total time.Duration
	for _, t := range tests {
		d, ok := tm.durations[testKey{pkg: pkg, name: t}]
		if !ok {
			return 0, false
		}
		total += d
	}
	return total, true
}

// SumDurationsForRefs is the cross-package analogue of SumDurationsFor: it
// sums the recorded wall-time of the given (pkg, name) references and reports
// whether every reference had a recorded timing. Empty input or any missing
// timing yields (0, false) so the caller degrades to a coarser estimate
// (per-package fallback, then the global ceiling) rather than trusting a
// partial or zero sum.
func (tm *TestMap) SumDurationsForRefs(refs []TestRef) (time.Duration, bool) {
	if tm == nil || len(refs) == 0 {
		return 0, false
	}
	var total time.Duration
	for _, r := range refs {
		d, ok := tm.durations[testKey{pkg: r.Pkg, name: r.Name}]
		if !ok {
			return 0, false
		}
		total += d
	}
	return total, true
}

// PackageDuration returns the sum of per-test durations recorded for
// `pkg` during the coverage build. Used as the per-package fallback
// when a mutant has no resolvable per-test covering set.
//
// Returns 0 for unknown packages or a nil TestMap; callers treat 0 as
// "no per-package signal" and degrade further to the global timeout.
func (tm *TestMap) PackageDuration(pkg string) time.Duration {
	if tm == nil {
		return 0
	}
	return tm.pkgDurations[pkg]
}

// NewTestMapForTesting constructs a TestMap directly from raw timing
// data and a "file:line" → tests cover index. Exposed only because the
// runner-package timeout selector needs to be exercised against
// hand-built fixtures without spinning up `go test` to populate a real
// map. Production code must use BuildTestMap.
//
// perTest: keys are [pkg, name] pairs; the helper aggregates them into
// the internal (pkg, name)→duration map and the rolling per-package
// totals so PackageDuration matches what BuildTestMap would produce.
//
// coverIndex: keys are "file:line"; values are the covering tests as
// (pkg, name) references, mirroring the package-aware index BuildTestMap
// produces.
func NewTestMapForTesting(perTest map[[2]string]time.Duration, coverIndex map[string][]TestRef) *TestMap {
	tm := newTestMap(false)
	for k, d := range perTest {
		tm.recordDuration(k[0], k[1], d)
	}
	for fileLine, refs := range coverIndex {
		set := make(map[testKey]bool, len(refs))
		for _, r := range refs {
			set[testKey{pkg: r.Pkg, name: r.Name}] = true
		}
		tm.index[fileLine] = set
	}
	return tm
}

// WithUnmappedForTesting returns tm with `unmapped` recorded as packages
// that could not be mapped test by test, and crossPkg as whether they may
// cover mutants outside themselves. Exposed for the runner's routing and
// timeout tests, like NewTestMapForTesting.
func (tm *TestMap) WithUnmappedForTesting(crossPkg bool, unmapped ...UnmappedPkg) *TestMap {
	tm.crossPkg = crossPkg
	for _, u := range unmapped {
		tm.markUnmapped(u.ImportPath, u.Dir, u.Reason)
	}
	return tm
}

// RunPattern returns a -run regex pattern that matches exactly the given tests.
func RunPattern(tests []string) string {
	if len(tests) == 0 {
		return ""
	}
	escaped := make([]string, len(tests))
	for i, t := range tests {
		escaped[i] = regexp.QuoteMeta(t)
	}
	return "^(" + strings.Join(escaped, "|") + ")$"
}

type testEntry struct {
	name string
	pkg  string
}

// listTests lists the tests in each compiled binary, keyed by the import
// path the binary was built for, across `workers` goroutines. Listing the
// binary that will run the tests, rather than parsing `go test -list`
// output for the package patterns, means the keys match pkgBins by
// construction (#105) and no package is compiled twice.
//
// A binary whose listing fails or can't be trusted to name every test
// doesn't fail the map: its package is returned in unmapped, keyed by
// import path, with the reason, and its mutants run it in full.
func listTests(ctx context.Context, pkgBins map[string]*compiledPkg, timeout time.Duration, workers int) (tests []testEntry, unmapped map[string]string) {
	unmapped = make(map[string]string)
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		sem = make(chan struct{}, workers)
	)
	for pkg, cp := range pkgBins {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			names, err := listBinTestsFunc(ctx, cp, timeout)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				unmapped[pkg] = err.Error()
				return
			}
			for _, name := range names {
				tests = append(tests, testEntry{name: name, pkg: pkg})
			}
		})
	}
	wg.Wait()
	return tests, unmapped
}

// listBinTests runs `<binary> -test.list=.` from the package directory, as
// the per-test runs do, so a TestMain that depends on the working directory
// behaves the same in both. The error, if any, reads as the reason the
// package can't be mapped.
func listBinTests(ctx context.Context, cp *compiledPkg, timeout time.Duration) ([]string, error) {
	ctx, cancel := withTestTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, cp.binPath, append([]string{"-test.list=."}, cp.testArgs...)...)
	cmd.Dir = cp.dir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// The first line is the binary's own message; a -cover binary
		// appends a GOCOVERDIR warning after it.
		first, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n")
		return nil, fmt.Errorf("listing its tests failed: %w: %s", err, first)
	}
	names, glued := parseTestList(stdout.String())
	if glued != "" {
		return nil, fmt.Errorf("output printed without a newline hides a test name in its test list: %q", glued)
	}
	return names, nil
}

// runnableTestName matches the names in -test.list output that -test.run
// can select: tests, fuzz targets (their seed corpus runs as a test) and
// examples. Anything else is skipped — benchmarks, which -test.run never
// matches, and output the binary's init or TestMain printed to stdout.
var runnableTestName = regexp.MustCompile(`^(Test|Fuzz|Example)[\p{L}\p{N}_]*$`)

// gluedTestName matches a line that ends in a runnable name without
// starting with it: output printed without a trailing newline, with the
// next listed name glued on, which loses that test.
var gluedTestName = regexp.MustCompile(`.(Test|Fuzz|Example)[\p{L}\p{N}_]*$`)

// benchmarkName matches a benchmark in -test.list output. Benchmarks are
// never run, so a name like BenchmarkTestX is not a glued test name.
var benchmarkName = regexp.MustCompile(`^Benchmark[\p{L}\p{N}_]*$`)

// parseTestList extracts the runnable test names from a test binary's
// -test.list output, one per line, and returns the first line that looks
// like output glued onto a test name. A log line that happens to end in a
// test name is reported too; that only costs speed, as its package then
// runs in full.
func parseTestList(out string) (names []string, glued string) {
	for line := range strings.Lines(out) {
		name := strings.TrimSpace(line)
		switch {
		case runnableTestName.MatchString(name):
			names = append(names, name)
		case glued == "" && !benchmarkName.MatchString(name) && gluedTestName.MatchString(name):
			glued = name
		}
	}
	return names, glued
}

// setTestArgs gives every compiled binary the arguments `go test` would
// pass it for opts.TestFlags. Without them, a test the mutant runs skip
// under -short would still run (and could hang) here, and a test needing
// a custom flag would fail and unmap its package.
func setTestArgs(ctx context.Context, projectDir string, opts BuildOptions, pkgBins map[string]*compiledPkg) error {
	if len(opts.TestFlags) == 0 || len(pkgBins) == 0 {
		return nil
	}
	// The arguments depend only on the flags; any package with tests will
	// do, and the smallest import path keeps the choice stable.
	pkg := slices.Min(slices.Collect(maps.Keys(pkgBins)))
	args, err := testBinaryArgsFunc(ctx, projectDir, opts.Tags, pkg, opts.TestFlags)
	if err != nil {
		return fmt.Errorf("reading the test-binary arguments for --test-flags: %w", err)
	}
	for _, cp := range pkgBins {
		cp.testArgs = args
	}
	return nil
}

// testBinaryArgs returns the arguments `go test` passes a test binary for
// the given flags. It reads them from `go test -n`, which prints the
// binary's command line without building or running anything, so build
// flags, test flags (rewritten to -test.X) and -args are split exactly as
// `go test` splits them for the mutant runs.
func testBinaryArgs(ctx context.Context, projectDir, tags, pkg string, flags []string) ([]string, error) {
	args := []string{"test", "-n"}
	if tags != "" {
		args = append(args, tagsBuildFlag+tags)
	}
	args = append(append(args, pkg), flags...)
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = projectDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("go test -n %s: %w\n%s", pkg, err, out)
	}
	return parseTestBinaryArgs(string(out))
}

// goTestOwnArg reports whether a test-binary argument is one `go test` adds
// for itself rather than for the user's flags: BuildTestMap bounds runs
// itself, and the log file and test2json framing serve go test's own
// output handling.
func goTestOwnArg(arg string) bool {
	return strings.HasPrefix(arg, "-test.timeout=") ||
		strings.HasPrefix(arg, "-test.testlogfile=") ||
		arg == "-test.v=test2json"
}

// parseTestBinaryArgs finds the test binary's command line in `go test -n`
// output — "$WORK/b001/<name>.test -test.paniconexit0 ..." — and returns
// its arguments, minus the ones goTestOwnArg names. Arguments are printed
// unquoted, and none contains whitespace (TestFlags are whitespace-split),
// so splitting on whitespace recovers them exactly.
func parseTestBinaryArgs(out string) ([]string, error) {
	for line := range strings.Lines(out) {
		// With -json among the flags, go test frames its output as events.
		var event struct{ Output string }
		if json.Unmarshal([]byte(line), &event) == nil {
			line = event.Output
		}
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.HasPrefix(fields[0], "$WORK") ||
			!strings.HasSuffix(strings.TrimSuffix(fields[0], ".exe"), ".test") {
			continue
		}
		return slices.DeleteFunc(fields[1:], goTestOwnArg), nil
	}
	return nil, errors.New("go test -n printed no test binary command")
}

type resolvedPkg struct {
	importPath string
	dir        string
}

func resolvePackages(ctx context.Context, projectDir string, patterns []string, tags string) ([]resolvedPkg, error) {
	args := []string{"list", "-f", "{{.ImportPath}}\t{{.Dir}}"}
	if tags != "" {
		args = append(args, tagsBuildFlag+tags)
	}
	args = append(args, patterns...)
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = projectDir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("go list: %w\n%s", err, stderr.String())
	}

	var pkgs []resolvedPkg
	scanner := bufio.NewScanner(&stdout)
	for scanner.Scan() {
		parts := strings.SplitN(scanner.Text(), "\t", 2)
		if len(parts) == 2 {
			pkgs = append(pkgs, resolvedPkg{importPath: parts[0], dir: parts[1]})
		}
	}
	return pkgs, nil
}

// sanitize makes a test name safe for use as a filename.
func sanitize(s string) string {
	return strings.NewReplacer("/", "_", " ", "_", "\\", "_", ".", "_").Replace(s)
}
