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

	// rebuilds is, per package, how long a mutant's `go test` takes to
	// rebuild its test binary (see measureRebuild).
	rebuilds map[string]time.Duration

	// suites holds every package in the map's scope that has tests, sorted
	// by import path, whether or not its coverage binary compiled. A
	// mutant's verdict rests on those whose tests can run its code (see
	// SuitePkgs).
	suites []Package

	// crossPkg reports whether the test binaries were built with
	// -coverpkg, so a test can record coverage outside its own package
	// and a package's tests may kill a mutant in any package their binary
	// links.
	crossPkg bool

	// testDeps maps a package in suites to the packages its test binary
	// links (see testDeps); it is read only with crossPkg. A package
	// without an entry is taken to link every package.
	testDeps map[string]map[string]bool

	// warnings describes what the map lost while building without
	// failing: work it routes less precisely, or not at all (see Warnings).
	warnings []string
}

// Package is a package in the coverage map's scope that has tests.
type Package struct {
	ImportPath string
	Dir        string
}

func newTestMap(crossPkg bool) *TestMap {
	return &TestMap{
		index:     make(map[string]map[testKey]bool),
		durations: make(map[testKey]time.Duration),
		rebuilds:  make(map[string]time.Duration),
		crossPkg:  crossPkg,
	}
}

// SuitePkgs returns the packages whose whole test suites decide whether a
// mutant in pkg lives, sorted by import path: those in the map's scope
// whose tests can run code in pkg. Without -coverpkg a test covers only
// its own package, so that is pkg itself when it has tests; with it, every
// package whose test binary links pkg.
//
// The map's covering tests are only the likeliest killers, run first
// because they are cheap. A test can cover a line in package order but
// not alone — it fails, skips or hangs without an earlier test's setup,
// or its name was lost from the listing — so a mutant they don't kill
// lives only once these suites pass with it in place.
func (tm *TestMap) SuitePkgs(pkg string) []Package {
	if tm == nil {
		return nil
	}
	var pkgs []Package
	for _, p := range tm.suites {
		if p.ImportPath == pkg || tm.crossPkg && tm.links(p.ImportPath, pkg) {
			pkgs = append(pkgs, p)
		}
	}
	return pkgs
}

// Suites returns every package in the map's scope that has tests, sorted
// by import path: each one SuitePkgs can return.
func (tm *TestMap) Suites() []Package {
	if tm == nil {
		return nil
	}
	return slices.Clone(tm.suites)
}

// Warnings returns what went wrong building the map that didn't fail it,
// in the order it happened: each costs the run speed, never a kill, but
// the user should know why it is slower than the map promises.
func (tm *TestMap) Warnings() []string {
	return tm.warnings
}

// links reports whether p's test binary links pkg, or what it links is
// unknown.
func (tm *TestMap) links(p, pkg string) bool {
	deps, known := tm.testDeps[p]
	return !known || deps[pkg]
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
	tm := newTestMap(opts.CoverPkg != "")
	tm.suites = suitePkgs(pkgBins, compileFailures)
	// Every package with tests failed to compile: the map would come back
	// empty and per-test routing would switch off without a word. Surface
	// it so the caller warns and falls back explicitly (a nil map routes
	// exactly like an empty one; see unrouted).
	if len(pkgBins) == 0 && len(compileFailures) > 0 {
		return tm.unrouted(fmt.Errorf("no test binary compiled (%d packages failed); first failure: %w",
			len(compileFailures), compileFailures[0].err))
	}
	if err := setTestArgs(ctx, projectDir, opts, pkgBins); err != nil {
		return tm.unrouted(err)
	}
	tm.rebuilds = measureRebuilds(ctx, projectDir, opts, pkgBins)

	// 2. List each binary's tests. Keying them by the binary's import path
	// means every listed test has a binary to run against by construction.
	tests, listFailures := listTestsFunc(ctx, pkgBins, opts.TestTimeout, opts.Workers)
	// A cancelled ctx fails every remaining listing; report the
	// cancellation, not the listing failures it caused.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Every listing failed: as with every compile failing, the map would
	// come back empty and routing would switch off without a word.
	if len(tests) == 0 && len(listFailures) > 0 {
		return tm.unrouted(fmt.Errorf("no tests listed (%d packages failed); first failure: %w",
			len(listFailures), listFailures[0]))
	}
	if len(listFailures) > 0 {
		tm.warnings = append(tm.warnings, fmt.Sprintf(
			"listing the tests of %d packages failed, so their mutants run their whole suites; first failure: %v",
			len(listFailures), listFailures[0]))
	}

	// 3. Run tests in parallel using compiled binaries.
	work := make(chan testEntry, len(tests))
	results := make(chan testCoverage, opts.Workers)

	stalled := &stalledPkgs{set: make(map[string]bool)}
	var wg sync.WaitGroup
	for i := range opts.Workers {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			profilePath := filepath.Join(opts.TmpDir, fmt.Sprintf("testmap-%d.cov", workerID))
			processWork(ctx, work, pkgBins, profilePath, opts.TestTimeout, stalled, results)
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
	// A cancelled build is reported as the cancellation, not as a map of
	// whatever ran before it.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// A package's suite decides a mutant's verdict only if its tests can
	// reach the mutant. Without knowing what they reach it decides every
	// verdict, so failing to read it costs speed, never a missed kill —
	// but every survivor then re-runs every suite, which the user is told.
	if tm.crossPkg {
		if tm.testDeps, err = testDepsFunc(ctx, projectDir, opts, importPaths(tm.suites)); err != nil {
			tm.warnings = append(tm.warnings, fmt.Sprintf(
				"reading what each package's tests link failed, so every survivor is re-checked against every package's suite: %v", err))
		}
	}
	return tm, nil
}

// unrouted ends a build that can route no mutant to its covering tests,
// with err. Without -coverpkg the map is dropped: each mutant then runs
// its own package's whole suite, which is all that can kill it. With it,
// the suites of the packages whose tests link the mutant's can kill it
// too, and only the map knows them: dropped, a mutant only an importer's
// tests kill would read LIVED. So the map is kept without routes or
// links, err becomes a warning, and every survivor is re-checked against
// every package's suite.
func (tm *TestMap) unrouted(err error) (*TestMap, error) {
	if !tm.crossPkg {
		return nil, err
	}
	tm.warnings = append(tm.warnings, fmt.Sprintf(
		"routing mutants to their covering tests failed, so each runs its own package's whole suite and every survivor is re-checked against every package's suite: %v", err))
	return tm, nil
}

// suitePkgs returns the packages with tests, sorted by import path: those
// whose coverage binary compiled, and those whose didn't. A failed -cover
// build may still build for a mutant run, and its tests may kill.
func suitePkgs(pkgBins map[string]*compiledPkg, failures []compileFailure) []Package {
	var pkgs []Package
	for _, cp := range pkgBins {
		pkgs = append(pkgs, Package{ImportPath: cp.importPath, Dir: cp.dir})
	}
	for _, f := range failures {
		pkgs = append(pkgs, Package{ImportPath: f.pkg.importPath, Dir: f.pkg.dir})
	}
	slices.SortFunc(pkgs, func(a, b Package) int { return strings.Compare(a.ImportPath, b.ImportPath) })
	return pkgs
}

// importPaths returns pkgs' import paths, in order.
func importPaths(pkgs []Package) []string {
	paths := make([]string, len(pkgs))
	for i, p := range pkgs {
		paths[i] = p.ImportPath
	}
	return paths
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

// recordDuration stores a single (pkg, test) timing. A duplicate
// observation (e.g. future retry logic re-running a test) accumulates
// rather than overwrites, so a timing never undercounts the runs it
// describes.
func (tm *TestMap) recordDuration(pkg, name string, d time.Duration) {
	if d <= 0 {
		return
	}
	tm.durations[testKey{pkg: pkg, name: name}] += d
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
// A test that fails, times out or leaves no readable profile when run
// alone is left out of the map: typically it relies on an earlier test's
// setup, and routing a mutant to it alone would fail with or without the
// mutant, a false kill. Its lines route to the other tests covering them,
// or to the whole package, and a survivor is re-checked against the
// whole package either way (see SuitePkgs).
//
// Once one of a package's tests times out alone, its remaining tests are
// not run: they likely hang the same way, each holding a worker for the
// whole timeout. Leaving them out of the map costs only speed.
//
// profilePath is the worker's own coverage profile, rewritten by each run.
func processWork(ctx context.Context, work <-chan testEntry, pkgBins map[string]*compiledPkg, profilePath string, testTimeout time.Duration, stalled *stalledPkgs, results chan<- testCoverage) {
	for test := range work {
		if ctx.Err() != nil {
			return
		}
		cp := pkgBins[test.pkg]
		if cp == nil || stalled.has(test.pkg) {
			continue
		}
		blocks, dur, err := runCompiledTestFunc(ctx, cp, test.name, profilePath, testTimeout)
		if err != nil {
			if errors.Is(err, errSoloTimeout) {
				stalled.add(test.pkg)
			}
			continue
		}
		// Forward the timing even when the test produced no blocks: the
		// mutant covering this test still executes it, so its duration
		// matters for the per-mutant timeout.
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

// stalledPkgs records the packages with a test that timed out when run
// alone. The coverage workers share it; a nil one records nothing.
type stalledPkgs struct {
	mu  sync.Mutex
	set map[string]bool
}

// add notes that a test in pkg timed out alone.
func (s *stalledPkgs) add(pkg string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.set[pkg] = true
}

// has reports whether a test in pkg timed out alone.
func (s *stalledPkgs) has(pkg string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.set[pkg]
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
// compiled", and keep the failed packages' suites in scope.
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
// directory; the probe's binary is removed once built. Its name extends
// that of cp's binary, as a fixed name can be the binary's own: a package
// named probe builds probe.test, which the probe would overwrite and then
// remove before the package's tests are listed.
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

	probeBin := cp.binPath + ".rebuild"
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
// A run cut off by `timeout` reports the timeout as its duration, and an
// error wrapping errSoloTimeout.
//
// The error says why the run left no usable coverage — the test failed,
// timed out, or its profile couldn't be read.
func runCompiledTest(ctx context.Context, cp *compiledPkg, testName, profilePath string, timeout time.Duration) (blocks []Block, dur time.Duration, err error) {
	// Ours go first: a positional argument among testArgs (after -args)
	// ends the binary's flag parsing.
	args := append([]string{
		fmt.Sprintf("-test.run=^%s$", regexp.QuoteMeta(testName)),
		"-test.coverprofile=" + profilePath,
	}, cp.testArgs...)
	// The worker's previous run left its profile here. A binary that exits
	// 0 without writing one (an os.Exit(0) before m.Run returns) must not
	// read as having covered what that run did.
	_ = os.Remove(profilePath)

	runCtx, cancel := withTestTimeout(ctx, timeout)
	defer cancel()
	cmd := testBinaryCmd(runCtx, cp, args)

	start := time.Now()
	runErr := cmd.Run()
	dur = time.Since(start)

	if runErr != nil {
		if runCtx.Err() == context.DeadlineExceeded {
			return nil, dur, fmt.Errorf("%s: %w after %s", testName, errSoloTimeout, timeout)
		}
		return nil, dur, fmt.Errorf("%s failed when run alone: %w", testName, runErr)
	}

	profile, err := parseFileFunc(profilePath)
	if err != nil {
		return nil, dur, fmt.Errorf("%s: reading its coverage profile: %w", testName, err)
	}
	return profile.blocks, dur, nil
}

// errSoloTimeout marks a test run alone that runCompiledTest cut off at
// its timeout.
var errSoloTimeout = errors.New("timed out when run alone")

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
// is trustworthy: if any test was missing (e.g. it failed alone during
// the coverage build and was left out, or the test was added after the
// build) the caller should fall back to a coarser deadline.
//
// On a nil TestMap, returns (0, false). Empty tests slice short-circuits
// to (0, false) so the fallback is reached without a misread of "all 0 of
// 0 tests had data → complete=true → use 0 timeout".
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
// timing yields (0, false) so the caller degrades to the global ceiling
// rather than trusting a partial or zero sum.
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
// the internal (pkg, name)→duration map.
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

// WithSuitesForTesting returns a copy of tm whose scope is `suites`, with
// crossPkg as whether their tests may cover mutants outside themselves and
// deps as what their test binaries link (see SuitePkgs). Exposed for the
// runner's routing tests, like NewTestMapForTesting. tm itself is left as
// it is: a built TestMap is read without locks.
func (tm *TestMap) WithSuitesForTesting(crossPkg bool, deps map[string]map[string]bool, suites ...Package) *TestMap {
	c := *tm
	c.crossPkg = crossPkg
	c.testDeps = deps
	c.suites = slices.SortedFunc(slices.Values(suites), func(a, b Package) int {
		return strings.Compare(a.ImportPath, b.ImportPath)
	})
	return &c
}

// WithWarningsForTesting returns a copy of tm whose Warnings are exactly
// `warnings`. Exposed for main's tests, like NewTestMapForTesting.
func (tm *TestMap) WithWarningsForTesting(warnings ...string) *TestMap {
	c := *tm
	c.warnings = warnings
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
}

// listTests lists the tests in each compiled binary, keyed by the import
// path the binary was built for, across `workers` goroutines. Listing the
// binary that will run the tests, rather than parsing `go test -list`
// output for the package patterns, means the keys match pkgBins by
// construction (#105) and no package is compiled twice.
//
// A binary whose listing fails doesn't fail the map: its package just
// lists no tests, so its mutants run its whole suite. Its failure is
// returned, naming the package, with the others sorted by package.
func listTests(ctx context.Context, pkgBins map[string]*compiledPkg, timeout time.Duration, workers int) ([]testEntry, []error) {
	var (
		tests    []testEntry
		failures []listFailure
		mu       sync.Mutex
		wg       sync.WaitGroup
		sem      = make(chan struct{}, workers)
	)
	for pkg, cp := range pkgBins {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			names, err := listBinTestsFunc(ctx, cp, timeout)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures = append(failures, listFailure{pkg: pkg, err: err})
			}
			for _, name := range names {
				tests = append(tests, testEntry{name: name, pkg: pkg})
			}
		})
	}
	wg.Wait()
	slices.SortFunc(failures, func(a, b listFailure) int { return strings.Compare(a.pkg, b.pkg) })
	errs := make([]error, len(failures))
	for i, f := range failures {
		errs[i] = fmt.Errorf("%s: %w", f.pkg, f.err)
	}
	return tests, errs
}

// listFailure is a package whose test binary's listing failed.
type listFailure struct {
	pkg string
	err error
}

// listBinTests runs `<binary> -test.list=.` from the package directory, as
// the per-test runs do, so a TestMain that depends on the working directory
// behaves the same in both.
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
	return parseTestList(stdout.String()), nil
}

// runnableTestName matches the names in -test.list output that -test.run
// can select: tests, fuzz targets (their seed corpus runs as a test) and
// examples. Anything else is skipped — benchmarks, which -test.run never
// matches, and output the binary's init or TestMain printed to stdout. A
// test whose name such output was printed onto, without a newline, is
// lost; its package's suite still decides every verdict (see SuitePkgs).
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

// setTestArgs gives every compiled binary the arguments `go test` would
// pass it for opts.TestFlags. Without them, a test the mutant runs skip
// under -short would still run (and could hang) here, and a test needing
// a custom flag would fail and be left out of the map.
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
// reports an error for is left out, as its deps may be incomplete.
//
// The build flags among opts.TestFlags are passed as in the mutant runs:
// -tags or -race can add test files, and with them imports.
func testDeps(ctx context.Context, projectDir string, opts BuildOptions, pkgs []string) (map[string]map[string]bool, error) {
	args := []string{"list", "-e", "-test", "-f", "{{if not (or .Error .DepsErrors)}}{{range .Deps}}{{$.ImportPath}}\t{{.}}\n{{end}}{{end}}"}
	if opts.Tags != "" {
		args = append(args, tagsBuildFlag+opts.Tags)
	}
	args = append(args, buildFlags(opts.TestFlags)...)
	args = append(args, pkgs...)
	cmd := goCmd(ctx, projectDir, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list -test: %w: %s", err, bytes.TrimSpace(stderr.Bytes()))
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
