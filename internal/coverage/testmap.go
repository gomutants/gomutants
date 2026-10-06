package coverage

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	testName string
	duration time.Duration
	blocks   []Block
}

// compiledPkg holds a pre-compiled test binary for a package.
type compiledPkg struct {
	binPath    string // Path to compiled test binary.
	importPath string // Package import path.
	dir        string // Package directory (for running the binary).
}

// BuildTestMap compiles each package's test binary once, lists the tests
// each binary contains, then runs every test function alone against its
// binary with coverage. Uses parallel workers.
//
// testTimeout bounds every run of a compiled test binary (listing and each
// per-test run); zero means no bound. A test that hangs when run alone
// would otherwise block the coverage phase forever: unlike `go test`, a
// bare test binary has no default timeout.
func BuildTestMap(ctx context.Context, projectDir string, packages []string, coverPkg, tags string, tmpDir string, workers int, testTimeout time.Duration) (*TestMap, error) {
	// 1. Resolve package patterns to individual packages and compile test binaries.
	resolvedPkgs, err := resolvePackagesFunc(ctx, projectDir, packages, tags)
	if err != nil {
		return nil, fmt.Errorf("resolving packages: %w", err)
	}

	pkgBins, compileFailures := buildPkgBins(ctx, projectDir, tmpDir, coverPkg, tags, resolvedPkgs)
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
			len(compileFailures), compileFailures[0])
	}

	// 2. List each binary's tests. Keying them by the binary's import path
	// means every listed test has a binary to run against by construction.
	tests, err := listTestsFunc(ctx, pkgBins, testTimeout)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("listing tests: %w", err)
	}

	// 3. Run tests in parallel using compiled binaries.
	work := make(chan testEntry, len(tests))
	results := make(chan testCoverage, workers)

	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			processWork(ctx, work, pkgBins, tmpDir, workerID, testTimeout, results)
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
	tm := &TestMap{
		index:        make(map[string]map[testKey]bool),
		durations:    make(map[testKey]time.Duration),
		pkgDurations: make(map[string]time.Duration),
	}
	for tc := range results {
		tm.ingestResult(tc)
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
		blocks, dur := runCompiledTestFunc(ctx, cp, test.name, profilePath, testTimeout)
		// Forward the timing even when the test produced no blocks: the
		// mutant covering this test still executes it, so its duration
		// matters for the per-mutant timeout. Without this, a fast unit
		// test that touches no shared coverage line gets a 0 contribution
		// and the package sum understates real wall time.
		if len(blocks) == 0 && dur <= 0 {
			continue
		}
		results <- testCoverage{
			pkg:      test.pkg,
			testName: test.name,
			duration: dur,
			blocks:   blocks,
		}
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

// buildPkgBins compiles each package's test binary and indexes the results
// by import path. Failures are non-fatal — the package is skipped and the
// rest keep going — but every one other than a package without test files
// is returned, so the caller can tell "nothing to test" from "nothing
// compiled". Extracted from BuildTestMap so the skip behavior can be
// tested without driving the whole pipeline.
func buildPkgBins(ctx context.Context, projectDir, tmpDir, coverPkg, tags string, pkgs []resolvedPkg) (map[string]*compiledPkg, []error) {
	pkgBins := make(map[string]*compiledPkg)
	var failures []error
	for _, pkg := range pkgs {
		cp, err := compileTestBinaryFunc(ctx, projectDir, tmpDir, coverPkg, tags, pkg)
		if err != nil {
			if !errors.Is(err, errNoTestBinary) {
				failures = append(failures, err)
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
func compileTestBinary(ctx context.Context, projectDir, tmpDir, coverPkg, tags string, pkg resolvedPkg) (*compiledPkg, error) {
	binPath := filepath.Join(tmpDir, "testbin-"+sanitize(pkg.importPath)+".test")
	args := []string{"test", "-c", "-o", binPath, "-cover"}
	if coverPkg != "" {
		args = append(args, "-coverpkg="+coverPkg)
	}
	if tags != "" {
		args = append(args, tagsBuildFlag+tags)
	}
	args = append(args, pkg.importPath)

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
func runCompiledTest(ctx context.Context, cp *compiledPkg, testName, profilePath string, timeout time.Duration) ([]Block, time.Duration) {
	args := []string{
		fmt.Sprintf("-test.run=^%s$", regexp.QuoteMeta(testName)),
		"-test.coverprofile=" + profilePath,
	}

	ctx, cancel := withTestTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, cp.binPath, args...)
	cmd.Dir = cp.dir

	start := time.Now()
	runErr := cmd.Run()
	dur := time.Since(start)

	if runErr != nil {
		return nil, dur
	}

	profile, err := parseFileFunc(profilePath)
	if err != nil {
		return nil, dur
	}
	return profile.blocks, dur
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
	tm := &TestMap{
		index:        make(map[string]map[testKey]bool),
		durations:    make(map[testKey]time.Duration),
		pkgDurations: make(map[string]time.Duration),
	}
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
// path the binary was built for. Listing the binary that will run the
// tests, rather than parsing `go test -list` output for the package
// patterns, means the keys match pkgBins by construction (#105) and no
// package is compiled twice.
func listTests(ctx context.Context, pkgBins map[string]*compiledPkg, timeout time.Duration) ([]testEntry, error) {
	var tests []testEntry
	for pkg, cp := range pkgBins {
		names, err := listBinTests(ctx, cp, timeout)
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			tests = append(tests, testEntry{name: name, pkg: pkg})
		}
	}
	return tests, nil
}

// listBinTests runs `<binary> -test.list=.` from the package directory, as
// the per-test runs do, so a TestMain that depends on the working directory
// behaves the same in both.
func listBinTests(ctx context.Context, cp *compiledPkg, timeout time.Duration) ([]string, error) {
	ctx, cancel := withTestTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, cp.binPath, "-test.list=.")
	cmd.Dir = cp.dir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s -test.list: %w\n%s", cp.importPath, err, stderr.String())
	}
	return parseTestList(stdout.String()), nil
}

// runnableTestName matches the names in -test.list output that -test.run
// can select: tests, fuzz targets (their seed corpus runs as a test) and
// examples. Anything else is skipped — benchmarks, which -test.run never
// matches, and output the binary's init or TestMain printed to stdout.
var runnableTestName = regexp.MustCompile(`^(Test|Fuzz|Example)[\p{L}\p{N}_]*$`)

// parseTestList extracts the runnable test names from a test binary's
// -test.list output, one per line.
func parseTestList(out string) []string {
	var names []string
	for line := range strings.Lines(out) {
		if name := strings.TrimSpace(line); runnableTestName.MatchString(name) {
			names = append(names, name)
		}
	}
	return names
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
