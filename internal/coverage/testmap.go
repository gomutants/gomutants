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

	"github.com/gomutants/gomutants/internal/proctree"
)

// tagsBuildFlag is the `go` build-tags flag prefix; the configured tags
// value is appended to it when non-empty.
const tagsBuildFlag = "-tags="

// Function variables for testing.
var (
	resolvePackagesFunc   = resolvePackages
	listTestsFunc         = listTests
	listBinTestsFunc      = listBinTests
	testBinaryArgsFunc    = TestBinaryArgs
	testDepsFunc          = testDeps
	parseFileFunc         = ParseFile
	compileTestBinaryFunc = compileTestBinary
	runCompiledTestFunc   = runCompiledTest
	groupPassesFunc       = groupPasses
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

// SuiteDir returns the directory of pkg, one of the map's suites, and
// whether the map holds it. Suites are sorted by import path, so the
// lookup copies nothing.
func (tm *TestMap) SuiteDir(pkg string) (string, bool) {
	if tm == nil {
		return "", false
	}
	i, ok := slices.BinarySearchFunc(tm.suites, pkg, func(p Package, pkg string) int {
		return strings.Compare(p.ImportPath, pkg)
	})
	if !ok {
		return "", false
	}
	return tm.suites[i].Dir, true
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
	// Lines holds the positions (see LineKey) of the mutants to be
	// tested: only the groups of tests covering them are checked to pass
	// together (see checkGroups). Nil checks every covered line's.
	Lines map[string]bool
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

	// 5. Check that each group of tests a mutant will run passes together
	// without any mutant, and stop routing to those that don't.
	failed := checkGroups(ctx, tm.testGroups(opts.Lines), pkgBins, opts.TestTimeout, opts.Workers)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(failed) > 0 {
		tm.dropGroups(failed)
		tm.warnings = append(tm.warnings, fmt.Sprintf(
			"test groups that fail when run together without any mutant: %d, so mutants on their lines run whole suites instead; first: %s",
			len(failed), failed[0]))
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
			key := LineKey(b.File, line)
			if tm.index[key] == nil {
				tm.index[key] = make(map[testKey]bool)
			}
			tm.index[key][testKey{pkg: pkg, name: testName}] = true
		}
	}
}

// LineKey returns the coverage map's key for line in file, a coverage
// profile path such as a mutant's CoverageFile.
func LineKey(file string, line int) string {
	return file + ":" + fmt.Sprint(line)
}

// testGroup is the tests of one package that cover a line, sorted: what a
// mutant on the line runs in that package.
type testGroup struct {
	pkg   string
	tests []string
}

func (g testGroup) key() string {
	return g.pkg + "\x00" + strings.Join(g.tests, "\x00")
}

func (g testGroup) String() string {
	return g.pkg + ": " + strings.Join(g.tests, ", ")
}

func compareGroups(a, b testGroup) int {
	if c := strings.Compare(a.pkg, b.pkg); c != 0 {
		return c
	}
	return slices.Compare(a.tests, b.tests)
}

// lineGroups splits the tests covering a line by package.
func lineGroups(set map[testKey]bool) []testGroup {
	byPkg := map[string][]string{}
	for k := range set {
		byPkg[k.pkg] = append(byPkg[k.pkg], k.name)
	}
	var groups []testGroup
	for pkg, tests := range byPkg {
		slices.Sort(tests)
		groups = append(groups, testGroup{pkg: pkg, tests: tests})
	}
	return groups
}

// testGroups returns the distinct groups of two or more tests covering a
// line in lines (every line when lines is nil), sorted. A group of one
// test already passed alone when the map was built (see processWork).
func (tm *TestMap) testGroups(lines map[string]bool) []testGroup {
	keys := maps.Keys(tm.index)
	if lines != nil {
		keys = maps.Keys(lines)
	}
	seen := map[string]bool{}
	var groups []testGroup
	for key := range keys {
		for _, g := range lineGroups(tm.index[key]) {
			if len(g.tests) > 1 && !seen[g.key()] {
				seen[g.key()] = true
				groups = append(groups, g)
			}
		}
	}
	slices.SortFunc(groups, compareGroups)
	return groups
}

// dropGroups removes the failed groups from the map: from each line whose
// tests in a group's package are exactly that group, those tests. A line
// that runs some of them alongside others keeps them, as its own group
// was checked on its own. A line left without tests runs its mutant's
// whole package.
func (tm *TestMap) dropGroups(failed []testGroup) {
	drop := make(map[string]bool, len(failed))
	for _, g := range failed {
		drop[g.key()] = true
	}
	for key, set := range tm.index {
		for _, g := range lineGroups(set) {
			if !drop[g.key()] {
				continue
			}
			for _, name := range g.tests {
				delete(set, testKey{pkg: g.pkg, name: name})
			}
		}
		if len(set) == 0 {
			delete(tm.index, key)
		}
	}
}

// checkGroups runs each group once against its package's compiled binary,
// across `workers` goroutines, and returns those that failed or timed out,
// sorted. Each of their tests passed alone, so typically one leaves state
// behind that breaks another, and a test the full suite runs in between
// resets it. Routing a mutant to such a group would fail with or without
// the mutant, a false kill. Every group's package has a binary: the map
// indexes only tests that ran against one. Once ctx is cancelled the
// remaining runs fail to start, and the caller reports the cancellation.
func checkGroups(ctx context.Context, groups []testGroup, pkgBins map[string]*compiledPkg, timeout time.Duration, workers int) []testGroup {
	var (
		failed []testGroup
		mu     sync.Mutex
		wg     sync.WaitGroup
		sem    = make(chan struct{}, workers)
	)
	for _, g := range groups {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			if groupPassesFunc(ctx, pkgBins[g.pkg], g.tests, timeout) {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			failed = append(failed, g)
		})
	}
	wg.Wait()
	slices.SortFunc(failed, compareGroups)
	return failed
}

// groupPasses runs tests together, once, against cp's binary from its
// package directory, and reports whether they all passed within timeout.
func groupPasses(ctx context.Context, cp *compiledPkg, tests []string, timeout time.Duration) bool {
	ctx, cancel := withTestTimeout(ctx, timeout)
	defer cancel()
	// Ours go first: a positional argument among testArgs (after -args)
	// ends the binary's flag parsing.
	args := append([]string{"-test.run=" + RunPattern(tests)}, cp.testArgs...)
	return testBinaryCmd(ctx, cp, args).Run() == nil
}

// processWork processes test entries from the work channel.
//
// A test that fails, times out or leaves no readable profile when run
// alone is left out of the map: typically it relies on an earlier test's
// setup, and routing a mutant to it alone would fail with or without the
// mutant, a false kill. Its lines route to the other tests covering them,
// or to the whole package, and a survivor is re-checked against the
// whole package either way (see SuitePkgs). Tests that pass alone but
// fail together are caught afterwards (see checkGroups).
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
// mutant runs; see BuildFlags.
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
	args = append(args, BuildFlags(opts.TestFlags)...)

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
	}, nil
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

// BuildFlags returns the build flags among testFlags, read as `go test`
// reads them: up to -args or `--`, with a value given as the next field.
// `go test -c` can't take the rest: it rejects any flag it doesn't know,
// such as a property framework's -rapid.checks=100, where `go test` would
// hand it to the test binary. Test flags reach the binary through
// testArgs instead.
func BuildFlags(testFlags []string) []string {
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

// testBinaryCmd returns the command for one run of cp's binary from its
// package directory. When ctx ends, the binary is killed along with every
// process it started, and the wait for output held open by one that
// escaped is capped (see proctree.Bound), so a run that hangs can't
// outlast its timeout for long.
func testBinaryCmd(ctx context.Context, cp *compiledPkg, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, cp.binPath, args...)
	cmd.Dir = cp.dir
	proctree.Bound(cmd)
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
	key := LineKey(file, line)
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
	key := LineKey(file, line)
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

// TestBinaryArgs returns the arguments `go test` passes a test binary for
// the given flags. It reads them from `go test -n`, which prints the
// binary's command line without building or running anything, so build
// flags, test flags (rewritten to -test.X) and -args are split exactly as
// `go test` splits them for the mutant runs.
func TestBinaryArgs(ctx context.Context, projectDir, tags, pkg string, flags []string) ([]string, error) {
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
	args = append(args, BuildFlags(opts.TestFlags)...)
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
}

func resolvePackages(ctx context.Context, projectDir string, patterns []string, tags string) ([]resolvedPkg, error) {
	args := []string{"list", "-f", "{{.ImportPath}}\t{{.Dir}}"}
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
		importPath, dir, _ := strings.Cut(scanner.Text(), "\t")
		pkgs = append(pkgs, resolvedPkg{importPath: importPath, dir: dir})
	}
	return pkgs, nil
}
