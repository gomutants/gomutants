package coverage

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"os/exec"
	"path"
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
	testDepsFunc          = testDeps
	parseFileFunc         = ParseFile
	compileTestBinaryFunc = compileTestBinary
	runCompiledTestFunc   = runCompiledTest
	measureRebuildFunc    = measureRebuild
	statFileFunc          = os.Stat
	writeFileFunc         = os.WriteFile
	checkSoloSkipsFunc    = checkSoloSkips
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

	// unmappedOrder is, per unmapped package, the listing order of the
	// test its reason comes from, or math.MinInt for a reason that
	// concerns the whole package (see markUnmappedAt).
	unmappedOrder map[string]int

	// unmappedSorted is unmapped's values sorted by import path, kept in
	// step by markUnmapped so per-mutant reads don't sort.
	unmappedSorted []UnmappedPkg

	// rebuilds is, per package, how long a mutant's `go test` takes to
	// rebuild its test binary (see measureRebuild).
	rebuilds map[string]time.Duration

	// crossPkg reports whether the test binaries were built with
	// -coverpkg, so a test can record coverage outside its own package
	// and an unmapped package's tests may cover a mutant in any package
	// their binary links.
	crossPkg bool

	// testDeps maps an unmapped package to the packages its test binary
	// links (see testDeps); it is read only with crossPkg. An unmapped
	// package without an entry is taken to reach every package.
	testDeps map[string]map[string]bool
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
		index:         make(map[string]map[testKey]bool),
		durations:     make(map[testKey]time.Duration),
		pkgDurations:  make(map[string]time.Duration),
		unmapped:      make(map[string]UnmappedPkg),
		unmappedOrder: make(map[string]int),
		rebuilds:      make(map[string]time.Duration),
		crossPkg:      crossPkg,
	}
}

// markUnmapped records that pkg's tests can't be routed individually, for
// a reason about the whole package, which outranks any test's.
func (tm *TestMap) markUnmapped(importPath, dir, reason string) {
	tm.markUnmappedAt(importPath, dir, reason, math.MinInt)
}

// markUnmappedAt records that pkg's tests can't be routed individually,
// for a reason about the test at order in its package's listing. Of
// several reasons, the one from the test listed first is kept: it is the
// likeliest cause of the rest, and it doesn't depend on which test's run
// happened to finish first.
func (tm *TestMap) markUnmappedAt(importPath, dir, reason string, order int) {
	if prev, ok := tm.unmappedOrder[importPath]; ok && prev <= order {
		return
	}
	tm.unmapped[importPath] = UnmappedPkg{ImportPath: importPath, Dir: dir, Reason: reason}
	tm.unmappedOrder[importPath] = order
	tm.unmappedSorted = slices.SortedFunc(maps.Values(tm.unmapped), func(a, b UnmappedPkg) int {
		return strings.Compare(a.ImportPath, b.ImportPath)
	})
}

// Unmapped returns every package the map could not cover test by test,
// sorted by import path.
func (tm *TestMap) Unmapped() []UnmappedPkg {
	if tm == nil {
		return nil
	}
	return slices.Clone(tm.unmappedSorted)
}

// FullRunPkgs returns the unmapped packages whose whole test suite must
// run for a mutant in pkg, sorted by import path. Without -coverpkg a test
// covers only its own package, so that is pkg itself when it is unmapped;
// with it, any unmapped package whose test binary links pkg.
func (tm *TestMap) FullRunPkgs(pkg string) []UnmappedPkg {
	if tm == nil {
		return nil
	}
	if tm.crossPkg {
		var pkgs []UnmappedPkg
		for _, u := range tm.unmappedSorted {
			if tm.reaches(u.ImportPath, pkg) {
				pkgs = append(pkgs, u)
			}
		}
		return pkgs
	}
	if u, ok := tm.unmapped[pkg]; ok {
		return []UnmappedPkg{u}
	}
	return nil
}

// reaches reports whether unmapped package u's tests can run code in pkg:
// u is pkg, its test binary links pkg, or what it links is unknown.
func (tm *TestMap) reaches(u, pkg string) bool {
	deps, known := tm.testDeps[u]
	return u == pkg || !known || deps[pkg]
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
	// order is the test's place in its package's listing.
	order    int
	duration time.Duration
	blocks   []Block
	// failure, when set, says why the test's solo run left no usable
	// coverage; its package is then unmapped.
	failure string
	// skipped reports that the test skipped when run alone.
	skipped bool
}

// compiledPkg holds a pre-compiled test binary for a package.
type compiledPkg struct {
	binPath    string   // Path to compiled test binary.
	importPath string   // Package import path.
	dir        string   // Package directory (for running the binary).
	testArgs   []string // Arguments every run of the binary gets (see setTestArgs).
	probeFile  string   // The file measureRebuild changes (see resolvedPkg).
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
	tm.rebuilds = measureRebuilds(ctx, projectDir, opts, pkgBins)

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

	failed := &firstFailures{at: make(map[string]int)}
	var wg sync.WaitGroup
	for i := range opts.Workers {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			profilePath := filepath.Join(opts.TmpDir, fmt.Sprintf("testmap-%d.cov", workerID))
			processWork(ctx, work, pkgBins, profilePath, opts.TestTimeout, failed, results)
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
	soloSkips := make(map[string][]testEntry)
	for tc := range results {
		tm.ingestResult(tc)
		if tc.skipped {
			soloSkips[tc.pkg] = append(soloSkips[tc.pkg], testEntry{name: tc.testName, pkg: tc.pkg, order: tc.order})
		}
	}

	// 5. Check the tests that skipped alone (see checkSoloSkips).
	tm.unmapSoloSkips(ctx, pkgBins, tests, soloSkips, opts)
	// Tests or checks skipped after a cancellation leave the map partial,
	// and a partial map routes mutants away from tests that were never
	// run.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// An unmapped package runs in full only for the mutants its tests can
	// reach. Without knowing what they reach it runs for every mutant, so
	// failing to read it costs speed, never a missed kill.
	if tm.crossPkg && len(tm.unmapped) > 0 {
		tm.testDeps, _ = testDepsFunc(ctx, projectDir, opts.Tags, slices.Sorted(maps.Keys(tm.unmapped)))
	}
	return tm, nil
}

// unmapSoloSkips unmaps the packages checkSoloSkips finds a test in that
// skips alone but not in package order. skips holds the tests that
// skipped alone; a package already unmapped runs in full whatever they
// say, so it isn't checked.
func (tm *TestMap) unmapSoloSkips(ctx context.Context, pkgBins map[string]*compiledPkg, tests []testEntry, skips map[string][]testEntry, opts BuildOptions) {
	for pkg := range skips {
		if _, ok := tm.unmapped[pkg]; ok {
			delete(skips, pkg)
		}
	}
	for pkg, f := range checkSoloSkipsFunc(ctx, pkgBins, tests, skips, opts.TestTimeout, opts.Workers) {
		tm.markUnmappedAt(pkg, pkgBins[pkg].dir, f.reason, f.order)
	}
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
		tm.markUnmappedAt(tc.pkg, tc.dir, tc.failure, tc.order)
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
//
// A test listed after one of its package's tests already failed alone is
// not run: the failure unmaps the package, so its coverage would go
// unused, and a test that hangs alone would hold a worker for the whole
// timeout. Tests listed before the failure still run, as one of them may
// fail too and the earliest failure is the reason reported.
//
// profilePath is the worker's own coverage profile, rewritten by each run.
func processWork(ctx context.Context, work <-chan testEntry, pkgBins map[string]*compiledPkg, profilePath string, testTimeout time.Duration, failed *firstFailures, results chan<- testCoverage) {
	for test := range work {
		if ctx.Err() != nil {
			return
		}
		cp := pkgBins[test.pkg]
		if cp == nil || failed.before(test.pkg, test.order) {
			continue
		}
		blocks, dur, skipped, err := runCompiledTestFunc(ctx, cp, test.name, profilePath, testTimeout)
		// Forward the timing even when the test produced no blocks: the
		// mutant covering this test still executes it, so its duration
		// matters for the per-mutant timeout. Without this, a fast unit
		// test that touches no shared coverage line gets a 0 contribution
		// and the package sum understates real wall time. A failure is
		// always forwarded, as it unmaps the package.
		if len(blocks) == 0 && dur <= 0 && err == nil && !skipped {
			continue
		}
		tc := testCoverage{
			pkg:      test.pkg,
			dir:      cp.dir,
			testName: test.name,
			order:    test.order,
			duration: dur,
			blocks:   blocks,
			skipped:  skipped,
		}
		if err != nil {
			tc.failure = err.Error()
			failed.record(test.pkg, test.order)
		}
		results <- tc
	}
}

// firstFailures records, per package, the listing order of the earliest
// test that failed when run alone. The coverage workers share it; a nil
// one records nothing.
type firstFailures struct {
	mu sync.Mutex
	at map[string]int
}

// record notes that the test at order in pkg failed alone.
func (f *firstFailures) record(pkg string, order int) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if prev, ok := f.at[pkg]; ok {
		order = min(prev, order)
	}
	f.at[pkg] = order
}

// before reports whether a test listed before order in pkg failed alone.
func (f *firstFailures) before(pkg string, order int) bool {
	if f == nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	prev, ok := f.at[pkg]
	return ok && prev < order
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
// The build flags among opts.TestFlags follow the package, as in the
// mutant runs; see buildFlags.
//
// Each binary gets a directory of its own. A file name flattened from the
// import path can collide (m/api_v1 and m/api/v1 both flatten to
// m_api_v1), and one package's compile would then overwrite another's
// binary.
func compileTestBinary(ctx context.Context, projectDir string, opts BuildOptions, pkg resolvedPkg) (*compiledPkg, error) {
	binDir, err := os.MkdirTemp(opts.TmpDir, "testbin-")
	if err != nil {
		return nil, fmt.Errorf("go test -c %s: %w", pkg.importPath, err)
	}
	binPath := filepath.Join(binDir, path.Base(pkg.importPath)+".test")
	args := []string{"test", "-c", "-o", binPath, "-cover"}
	if opts.CoverPkg != "" {
		args = append(args, "-coverpkg="+opts.CoverPkg)
	}
	if opts.Tags != "" {
		args = append(args, tagsBuildFlag+opts.Tags)
	}
	args = append(args, pkg.importPath)
	args = append(args, buildFlags(opts.TestFlags)...)

	cmd := goCmd(ctx, projectDir, args...)
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
		probeFile:  pkg.probeFile,
	}, nil
}

// measureRebuilds measures, for every compiled package, how long a mutant's
// `go test` spends rebuilding its test binary (see measureRebuild),
// `opts.Workers` at a time, so the builds contend as the mutant runs' do.
// A package whose measurement fails gets no entry.
func measureRebuilds(ctx context.Context, projectDir string, opts BuildOptions, pkgBins map[string]*compiledPkg) map[string]time.Duration {
	rebuilds := make(map[string]time.Duration, len(pkgBins))
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		sem = make(chan struct{}, opts.Workers)
	)
	for pkg, cp := range pkgBins {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			d, err := measureRebuildFunc(ctx, projectDir, opts, cp)
			if err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			rebuilds[pkg] = d
		})
	}
	wg.Wait()
	return rebuilds
}

// measureRebuild times what a mutant's `go test` of cp's package spends
// before its tests start: the package recompiled from a changed file, and
// its test binary relinked. The coverage compile doesn't show this, as a
// second run serves it from the build cache. The change is a comment,
// unique to this call, appended to cp.probeFile through an overlay; the
// flags are the mutant runs' build flags. Building also warms the cache
// the mutant runs then use.
//
// The changed file and overlay go next to cp's test binary, in its own
// directory; the probe's binary is removed once built.
func measureRebuild(ctx context.Context, projectDir string, opts BuildOptions, cp *compiledPkg) (time.Duration, error) {
	src, err := os.ReadFile(cp.probeFile)
	if err != nil {
		return 0, err
	}
	dir := filepath.Dir(cp.binPath)
	changed := filepath.Join(dir, filepath.Base(cp.probeFile))
	src = fmt.Appendf(src, "\n// gomutants rebuild probe %d\n", time.Now().UnixNano())
	ov, _ := json.Marshal(map[string]map[string]string{"Replace": {cp.probeFile: changed}})
	ovPath := filepath.Join(dir, "overlay.json")
	if err := errors.Join(writeFileFunc(changed, src, 0o644), writeFileFunc(ovPath, ov, 0o644)); err != nil {
		return 0, err
	}

	probeBin := filepath.Join(dir, "probe.test")
	defer func() { _ = os.Remove(probeBin) }()
	args := []string{"test", "-c", "-vet=off", "-o", probeBin, "-overlay=" + ovPath}
	if opts.Tags != "" {
		args = append(args, tagsBuildFlag+opts.Tags)
	}
	args = append(args, cp.importPath)
	args = append(args, buildFlags(opts.TestFlags)...)
	cmd := goCmd(ctx, projectDir, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	start := time.Now()
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("go test -c %s: %w\n%s", cp.importPath, err, stderr.String())
	}
	return time.Since(start), nil
}

// goBuildFlags are the `go build` flags that shape a test binary, each
// mapped to whether it takes a value. Flags that only change go's own
// output (-n, -x, -v, -work, -json), must come first (-C) or are set by
// compileTestBinary itself (coverage, -o) are left out.
var goBuildFlags = map[string]bool{
	"a": false, "asan": false, "buildvcs": false, "linkshared": false,
	"modcacherw": false, "msan": false, "race": false, "trimpath": false,
	"asmflags": true, "buildmode": true, "compiler": true, "gccgoflags": true,
	"gcflags": true, "installsuffix": true, "ldflags": true, "mod": true,
	"modfile": true, "p": true, "pgo": true, "pkgdir": true, "tags": true,
	"toolexec": true,
}

// buildFlags returns the build flags among testFlags, read as `go test`
// reads them: up to -args or `--`, with a value given as the next field.
// `go test -c` can't take the rest: it rejects any flag it doesn't know,
// such as a property framework's -rapid.checks=100, where `go test` would
// hand it to the test binary. Test flags reach the binary through
// testArgs instead.
func buildFlags(testFlags []string) []string {
	var out []string
	for i := 0; i < len(testFlags); i++ {
		f := testFlags[i]
		if f == "-args" || f == "--args" || f == "--" {
			break
		}
		if !strings.HasPrefix(f, "-") {
			continue
		}
		name, _, inline := strings.Cut(strings.TrimPrefix(f[1:], "-"), "=")
		takesValue, ok := goBuildFlags[name]
		if !ok {
			continue
		}
		out = append(out, f)
		if takesValue && !inline && i+1 < len(testFlags) {
			i++
			out = append(out, testFlags[i])
		}
	}
	return out
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
// skipped reports that the test skipped (see checkSoloSkips).
//
// The error says why the run left no usable coverage — the test failed,
// timed out, or its profile couldn't be read — and reads as the reason its
// package can't be mapped.
func runCompiledTest(ctx context.Context, cp *compiledPkg, testName, profilePath string, timeout time.Duration) (blocks []Block, dur time.Duration, skipped bool, err error) {
	// Ours go first: a positional argument among testArgs (after -args)
	// ends the binary's flag parsing.
	args := append([]string{
		fmt.Sprintf("-test.run=^%s$", regexp.QuoteMeta(testName)),
		"-test.coverprofile=" + profilePath,
		"-test.v=true",
	}, cp.testArgs...)

	runCtx, cancel := withTestTimeout(ctx, timeout)
	defer cancel()
	cmd := testBinaryCmd(runCtx, cp, args)
	var skips skipScanner
	cmd.Stdout = &skips

	start := time.Now()
	runErr := cmd.Run()
	dur = time.Since(start)

	// ErrWaitDelay: the test passed, but a process it left behind held
	// the output open.
	if runErr != nil && !errors.Is(runErr, exec.ErrWaitDelay) {
		if runCtx.Err() == context.DeadlineExceeded {
			return nil, dur, false, fmt.Errorf("%s timed out after %s when run alone", testName, timeout)
		}
		return nil, dur, false, fmt.Errorf("%s failed when run alone: %w", testName, runErr)
	}

	profile, err := parseFileFunc(profilePath)
	if err != nil {
		return nil, dur, false, fmt.Errorf("%s: reading its coverage profile: %w", testName, err)
	}
	return profile.blocks, dur, slices.Contains(skips.skipped, testName), nil
}

// skipLinePrefix starts the line a -test.v run prints for a top-level
// test that skipped: "--- SKIP: TestX (0.00s)". A subtest's is indented.
const skipLinePrefix = "--- SKIP: "

// maxScannedLine bounds how much of one output line skipScanner keeps;
// a skip line is far shorter.
const maxScannedLine = 4096

// skipScanner collects the top-level tests a -test.v run reports skipped,
// keeping only the start of the line being read.
type skipScanner struct {
	line    []byte
	skipped []string
}

func (s *skipScanner) Write(p []byte) (int, error) {
	n := len(p)
	for {
		chunk, rest, eol := bytes.Cut(p, []byte{'\n'})
		// s.line never outgrows maxScannedLine, so room is never negative.
		room := maxScannedLine - len(s.line)
		s.line = append(s.line, chunk[:min(len(chunk), room)]...)
		if !eol {
			return n, nil
		}
		if name, ok := bytes.CutPrefix(s.line, []byte(skipLinePrefix)); ok {
			name, _, _ = bytes.Cut(name, []byte(" "))
			s.skipped = append(s.skipped, string(name))
		}
		s.line = s.line[:0]
		p = rest
	}
}

// soloSkipFailure is why checkSoloSkips unmaps a package, and the listing
// order of the test it concerns.
type soloSkipFailure struct {
	reason string
	order  int
}

// checkSoloSkips finds the packages with a test that skips when run alone
// but not in its package's order — one that skips until an earlier test
// has set something up. Mapped from the coverage of its skip, it would
// lose the mutants only its assertions kill. For each package in skips
// (the tests that skipped alone), the tests listed up to the last of them
// run together, in listing order; a skipped-alone test that doesn't skip
// there, or a run that fails, unmaps the package. `workers` packages are
// checked at a time.
func checkSoloSkips(ctx context.Context, pkgBins map[string]*compiledPkg, tests []testEntry, skips map[string][]testEntry, timeout time.Duration, workers int) map[string]soloSkipFailure {
	listed := make(map[string][]testEntry)
	for _, t := range tests {
		if _, ok := skips[t.pkg]; ok {
			listed[t.pkg] = append(listed[t.pkg], t)
		}
	}
	failures := make(map[string]soloSkipFailure)
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		sem = make(chan struct{}, workers)
	)
	for pkg, skipped := range skips {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			f, ok := checkPkgSkips(ctx, pkgBins[pkg], listed[pkg], skipped, timeout)
			if !ok {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			failures[pkg] = f
		})
	}
	wg.Wait()
	return failures
}

// checkPkgSkips runs one package's check for checkSoloSkips. listed is
// the package's tests and skipped those that skipped alone, both in any
// order: the run's -test.run pattern selects by name, and the binary runs
// what it selects in listing order.
func checkPkgSkips(ctx context.Context, cp *compiledPkg, listed, skipped []testEntry, timeout time.Duration) (soloSkipFailure, bool) {
	slices.SortFunc(skipped, func(a, b testEntry) int { return a.order - b.order })
	last := skipped[len(skipped)-1].order
	var names []string
	for _, t := range listed {
		if t.order <= last {
			names = append(names, t.name)
		}
	}

	runCtx, cancel := withTestTimeout(ctx, timeout)
	defer cancel()
	cmd := testBinaryCmd(runCtx, cp, append([]string{"-test.run=" + RunPattern(names), "-test.v=true"}, cp.testArgs...))
	var skips skipScanner
	cmd.Stdout = &skips
	if err := cmd.Run(); err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		first := skipped[0]
		if runCtx.Err() == context.DeadlineExceeded {
			err = fmt.Errorf("timed out after %s", timeout)
		}
		return soloSkipFailure{order: first.order, reason: fmt.Sprintf("%s skips when run alone, and running it after the tests listed before it failed: %v", first.name, err)}, true
	}
	for _, t := range skipped {
		if !slices.Contains(skips.skipped, t.name) {
			return soloSkipFailure{order: t.order, reason: t.name + " skips when run alone but not after the tests listed before it"}, true
		}
	}
	return soloSkipFailure{}, false
}

// withTestTimeout bounds one run of a compiled test binary. The deadline
// is enforced by killing the process rather than through -test.timeout,
// which only starts counting inside m.Run and so misses a TestMain that
// hangs in its setup.
func withTestTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return ctx, func() {
			// No timeout, so nothing to release.
		}
	}
	return context.WithTimeout(ctx, timeout)
}

// pipeDrainDelay bounds how long a run of a test binary waits, once the
// binary has exited or been killed, for output pipes a process it started
// still holds open. A var so tests can shorten it.
var pipeDrainDelay = 5 * time.Second

// testBinaryCmd returns the command for one run of cp's binary from its
// package directory. When ctx ends, the binary is killed along with every
// process it started (see killTreeOnCancel), and pipeDrainDelay caps the
// wait for output held open by one that escaped, so a run that hangs can't
// outlast its timeout for long.
func testBinaryCmd(ctx context.Context, cp *compiledPkg, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, cp.binPath, args...)
	cmd.Dir = cp.dir
	killTreeOnCancel(cmd)
	cmd.WaitDelay = pipeDrainDelay
	return cmd
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

// RebuildDuration returns how long a mutant's `go test` of pkg took to
// rebuild its test binary when measured (see measureRebuild), and whether
// it was. A mutant's deadline covers that build as well as its tests.
func (tm *TestMap) RebuildDuration(pkg string) (time.Duration, bool) {
	if tm == nil {
		return 0, false
	}
	d, ok := tm.rebuilds[pkg]
	return d, ok
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
//
// Every package the fixture names rebuilds in no time, so its
// deadlines come from the test durations alone; see
// WithRebuildsForTesting.
func NewTestMapForTesting(perTest map[[2]string]time.Duration, coverIndex map[string][]TestRef) *TestMap {
	tm := newTestMap(false)
	for k, d := range perTest {
		tm.recordDuration(k[0], k[1], d)
		tm.rebuilds[k[0]] = 0
	}
	for fileLine, refs := range coverIndex {
		set := make(map[testKey]bool, len(refs))
		for _, r := range refs {
			set[testKey{pkg: r.Pkg, name: r.Name}] = true
			tm.rebuilds[r.Pkg] = 0
		}
		tm.index[fileLine] = set
	}
	return tm
}

// WithUnmappedForTesting returns a copy of tm with `unmapped` recorded as
// packages that could not be mapped test by test, and crossPkg as whether
// they may cover mutants outside themselves. Exposed for the runner's
// routing and timeout tests, like NewTestMapForTesting. tm itself is left
// as it is: a built TestMap is read without locks.
func (tm *TestMap) WithUnmappedForTesting(crossPkg bool, unmapped ...UnmappedPkg) *TestMap {
	c := *tm
	c.crossPkg = crossPkg
	c.unmapped = maps.Clone(tm.unmapped)
	c.unmappedOrder = maps.Clone(tm.unmappedOrder)
	for _, u := range unmapped {
		c.markUnmapped(u.ImportPath, u.Dir, u.Reason)
	}
	return &c
}

// WithRebuildsForTesting returns a copy of tm whose rebuild durations (see
// RebuildDuration) are exactly `rebuilds`: a package left out has none.
// Exposed for the runner's timeout tests, like NewTestMapForTesting.
func (tm *TestMap) WithRebuildsForTesting(rebuilds map[string]time.Duration) *TestMap {
	c := *tm
	c.rebuilds = maps.Clone(rebuilds)
	return &c
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
	// order is the test's place in its package's listing, which is the
	// order a run of the whole package runs them in.
	order int
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
			for i, name := range names {
				tests = append(tests, testEntry{name: name, pkg: pkg, order: i})
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
	cmd := testBinaryCmd(ctx, cp, append([]string{"-test.list=."}, cp.testArgs...))

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// ErrWaitDelay means the binary succeeded but a process it left behind
	// still held the output open: the list it printed is complete.
	if err := cmd.Run(); err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("listing its tests timed out after %s", timeout)
		}
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
	out, err := goCmd(ctx, projectDir, args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("go test -n %s: %w\n%s", pkg, err, out)
	}
	return parseTestBinaryArgs(string(out))
}

// goTestOwnArg reports whether a test-binary argument is one `go test` adds
// for itself rather than for the user's flags: BuildTestMap bounds runs
// itself, and the test2json framing serves go test's own output handling.
// So does any path in go test's $WORK directory — the log file, and the
// coverage directory -cover or -covermode adds — which exists only inside
// a `go test` run: a binary given it fails to write there.
func goTestOwnArg(arg string) bool {
	return strings.HasPrefix(arg, "-test.timeout=") ||
		strings.Contains(arg, "$WORK") ||
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
		// The binary itself is in $WORK, so goTestOwnArg drops it too.
		return slices.DeleteFunc(fields, goTestOwnArg), nil
	}
	return nil, errors.New("go test -n printed no test binary command")
}

// testDeps returns, for each of pkgs, the packages its test binary links,
// read from the deps of the "<pkg>.test" main package `go list -test`
// generates, which take in the test files' imports too. A package go list
// reports an error for is left out, as its deps may be incomplete. The
// caller discards a failure, so it isn't dressed up.
func testDeps(ctx context.Context, projectDir, tags string, pkgs []string) (map[string]map[string]bool, error) {
	args := []string{"list", "-e", "-test", "-f", "{{if not (or .Error .DepsErrors)}}{{range .Deps}}{{$.ImportPath}}\t{{.}}\n{{end}}{{end}}"}
	if tags != "" {
		args = append(args, tagsBuildFlag+tags)
	}
	args = append(args, pkgs...)
	out, err := goCmd(ctx, projectDir, args...).Output()
	if err != nil {
		return nil, err
	}

	testMains := make(map[string]string, len(pkgs))
	for _, p := range pkgs {
		testMains[p+".test"] = p
	}
	deps := make(map[string]map[string]bool)
	for line := range strings.Lines(string(out)) {
		testMain, dep, _ := strings.Cut(strings.TrimSpace(line), "\t")
		pkg, ok := testMains[testMain]
		if !ok {
			continue
		}
		if deps[pkg] == nil {
			deps[pkg] = make(map[string]bool)
		}
		// A package recompiled for the test reads "p [p.test]".
		dep, _, _ = strings.Cut(dep, " ")
		deps[pkg][dep] = true
	}
	return deps, nil
}

// goCmd returns a `go` command run from projectDir.
func goCmd(ctx context.Context, projectDir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = projectDir
	return cmd
}

type resolvedPkg struct {
	importPath string
	dir        string
	// probeFile is the file measureRebuild changes: the package's first
	// production file, as mutants change those, or its first test file
	// in a package without any.
	probeFile string
}

func resolvePackages(ctx context.Context, projectDir string, patterns []string, tags string) ([]resolvedPkg, error) {
	args := []string{"list", "-f", "{{.ImportPath}}\t{{.Dir}}\t" +
		"{{if .GoFiles}}{{index .GoFiles 0}}{{else if .TestGoFiles}}{{index .TestGoFiles 0}}{{else if .XTestGoFiles}}{{index .XTestGoFiles 0}}{{end}}"}
	if tags != "" {
		args = append(args, tagsBuildFlag+tags)
	}
	args = append(args, patterns...)
	cmd := goCmd(ctx, projectDir, args...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("go list: %w\n%s", err, stderr.String())
	}

	var pkgs []resolvedPkg
	scanner := bufio.NewScanner(&stdout)
	for scanner.Scan() {
		importPath, rest, _ := strings.Cut(scanner.Text(), "\t")
		dir, file, _ := strings.Cut(rest, "\t")
		// go list lists only packages with a Go file, so file is set.
		pkgs = append(pkgs, resolvedPkg{importPath: importPath, dir: dir, probeFile: filepath.Join(dir, file)})
	}
	return pkgs, nil
}
