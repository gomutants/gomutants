package cache

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// TestIndex resolves test function names to the absolute paths of the
// _test.go files where they are declared, and lists every _test.go file in
// each known package directory. It is built once per run from a parse of
// every test file in scope.
//
// Cross-package coverage (tests in package B exercising code in package A
// via -coverpkg) means the per-test coverage map can return test names
// whose defining files live outside the mutant's own package directory.
// FilesFor surfaces those, so tests_hash includes every covering test
// file regardless of which package declared it. CoveringFiles combines
// both lookups into the set callers actually hash.
type TestIndex struct {
	byName map[string][]string // testName → abs paths of declaring _test.go files
	byDir  map[string][]string // pkgDir → abs paths of every _test.go in dir
	srcDir map[string][]string // pkgDir → abs paths of every non-test .go in dir
}

// BuildTestIndex parses each _test.go file in pkgDirs and indexes
// top-level test/benchmark/example/fuzz function declarations.
//
// Behavior on errors:
//   - A directory that fails to read is skipped (no entries indexed).
//   - A file that fails to parse is recorded in byDir (so fallback hashing
//     still includes it) but contributes no byName entries — its tests are
//     unknown, so any mutant resolving through it falls back to the
//     directory-wide list.
//
// Names that exist in multiple packages map to all declaring files; the
// per-test coverage map collapses package context, so we treat a same-
// named test as potentially covering through any of those files.
func BuildTestIndex(pkgDirs []string) *TestIndex {
	ti := &TestIndex{
		byName: make(map[string][]string),
		byDir:  make(map[string][]string),
		srcDir: make(map[string][]string),
	}
	seen := make(map[string]bool)
	for _, dir := range pkgDirs {
		abs, err := filepath.Abs(dir)
		if err != nil || seen[abs] {
			continue
		}
		seen[abs] = true

		// ReadDir failure (dir missing, permission denied, …) leaves
		// entries==nil; ranging over nil is a zero-iteration loop, the
		// same behavior we'd get from an explicit "continue on error"
		// guard — so no separate branch is needed.
		entries, _ := os.ReadDir(abs)

		var dirFiles, srcFiles []string
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			absPath := filepath.Join(abs, name)
			if !strings.HasSuffix(name, "_test.go") {
				// Non-test sources are recorded too: when this directory
				// is a *foreign* package declaring a covering test, its
				// production files decide that test's outcome and so gate
				// the mutant — see CoveringFiles. The .go filter keeps the
				// same set HashPkgFiles would compute for the directory.
				if strings.HasSuffix(name, ".go") {
					srcFiles = append(srcFiles, absPath)
				}
				continue
			}
			dirFiles = append(dirFiles, absPath)

			fset := token.NewFileSet()
			// SkipObjectResolution: we only need top-level FuncDecl names,
			// no need for the (slow) identifier-resolution pass. parser
			// returns a nil *ast.File only when the source can't be read,
			// in which case perr is also non-nil — so the `perr != nil`
			// guard is sufficient and the redundant `|| f == nil` is
			// elided to keep the mutation surface minimal.
			f, perr := parser.ParseFile(fset, absPath, nil, parser.SkipObjectResolution)
			if perr != nil {
				continue
			}
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv != nil {
					continue
				}
				if isTestEntryName(fn.Name.Name) {
					ti.byName[fn.Name.Name] = append(ti.byName[fn.Name.Name], absPath)
				}
			}
		}
		// Always record the dir, even when it contained no test files,
		// so AllInDir distinguishes "scanned but empty" from "never
		// scanned" via map presence. The guard form `if len(dirFiles)
		// > 0 { … }` was a near-equivalent mutant target (>= 0 still
		// stored an empty slice with the same observable AllInDir
		// result) so it's been folded into an unconditional write.
		ti.byDir[abs] = dirFiles
		ti.srcDir[abs] = srcFiles
	}
	return ti
}

// isTestEntryName reports whether name is a top-level test/benchmark/
// example/fuzz function declaration as recognized by `go test`. We don't
// validate the receiver/parameters — the goal is "did the user touch a
// file that contributes a test entry," not strict conformance.
func isTestEntryName(name string) bool {
	switch {
	case strings.HasPrefix(name, "Test"):
		return true
	case strings.HasPrefix(name, "Benchmark"):
		return true
	case strings.HasPrefix(name, "Example"):
		return true
	case strings.HasPrefix(name, "Fuzz"):
		return true
	}
	return false
}

// FilesFor returns the test files declaring testName, or nil if the name
// is unknown. Multiple files are returned when the same name was declared
// in more than one indexed package.
func (ti *TestIndex) FilesFor(testName string) []string {
	if ti == nil {
		return nil
	}
	return ti.byName[testName]
}

// AllInDir returns every _test.go file in pkgDir (absolute paths). It is
// the local half of every covering set — see CoveringFiles — and the whole
// of it when the per-test coverage map is unavailable.
func (ti *TestIndex) AllInDir(pkgDir string) []string {
	if ti == nil {
		return nil
	}
	return ti.byDir[pkgDir]
}

// CoveringFiles returns the files outside prod_hash and pkg_hash whose
// content gates cache reuse for a mutant in pkgDir that the per-test
// coverage map attributes to testNames. Pass a nil/empty testNames when no
// coverage map is available.
//
// The result is always a superset of AllInDir(pkgDir): every test file in
// the mutant's own package is in the set, not just the files declaring the
// covering tests. A package-level test helper — a table builder, a custom
// assertion, a fake — lives in a _test.go file that need not declare any
// test entry point at all, so BuildTestIndex records no name for it and no
// coverage-derived name can ever resolve to it. Loosen an assertion inside
// such a helper and a KILLED mutant starts surviving while its own file,
// its package's non-test files, and the covering test's file are all
// byte-identical. Test files are deliberately outside pkg_hash (folding
// them in would invalidate test-independent NOT_VIABLE entries on every
// test edit), so tests_hash is the only dimension that can carry them, and
// it can only carry them at package granularity.
//
// testNames then adds the cross-package half: with -coverpkg (--integration)
// the covering tests can be declared outside pkgDir entirely, and those
// files gate the mutant too. Names the index doesn't know contribute
// nothing — they resolve to no file, and the local set already stands.
//
// crossPkg says whether a foreign declaring directory can be a genuine
// cross-package coverage relationship: the caller sets it when this run
// instruments beyond the package under test (--integration, or an explicit
// --coverpkg), which is what it takes for a test outside pkgDir to record
// coverage on the mutant at all. When it is set, a foreign directory
// contributes its whole package the way pkgDir does: every _test.go in it
// (the same helper-only argument as above, which applies verbatim to R's
// assertion helpers) and every non-test .go file. Under --integration the dependency arrow points
// the other way from the usual one: mutants live in target package T, the
// covering tests live in importer R, and pkg_hash only ever hashes T. R's
// helpers and fixture builders — most of what an end-to-end test is made
// of — would otherwise sit in no dimension of the key at all, so loosening
// R's fixture would replay T's mutant as KILLED after it started surviving.
// pkgDir itself is excluded from this: pkg_hash already covers the mutant's
// own package, and re-adding it here would make an unrelated production
// edit invalidate the tests dimension too.
//
// Without crossPkg the expansion is skipped and only the declaring file
// itself is added, because a foreign hit is then a name collision rather
// than a dependency. TestMap.TestsFor deliberately projects package context
// out of the covering names, so FilesFor("TestNew") resolves to every
// indexed package declaring a TestNew — and TestAdd, TestParse or TestString
// live in several packages of most repositories, this one included.
// Expanding on a collision would fold unrelated packages' production sources
// into tests_hash and make an edit anywhere in them invalidate this mutant:
// package-scoped invalidation degraded to repository-scoped by a name two
// packages happened to share. The declaring file itself is still added — it
// is one file, and it keeps the conservative direction on the off chance
// that the name really is the covering test.
//
// A nil receiver yields nil, but there is no `ti == nil` guard here: both
// lookups this delegates to are already nil-safe and return nil, so the
// loops below iterate zero times and files stays nil. An explicit guard
// would be an unobservable branch — the same reason BuildTestIndex elides
// its redundant `|| f == nil`.
func (ti *TestIndex) CoveringFiles(pkgDir string, testNames []string, crossPkg bool) []string {
	var files []string
	seen := make(map[string]bool)
	add := func(f string) {
		if !seen[f] {
			seen[f] = true
			files = append(files, f)
		}
	}
	for _, f := range ti.AllInDir(pkgDir) {
		add(f)
	}
	for _, n := range testNames {
		for _, f := range ti.FilesFor(n) {
			add(f)
			if !crossPkg {
				continue
			}
			// Dereferencing ti here needs no nil guard: reaching this
			// point means FilesFor returned a file, which a nil index
			// never does.
			if dir := filepath.Dir(f); dir != pkgDir {
				ti.addWholePackage(dir, add)
			}
		}
	}
	return files
}

// PackageFiles returns every indexed file of dir: its test files and its
// production sources. A package whose whole suite runs for a mutant (see
// coverage.TestMap.FullRunPkgs) decides the verdict through all of them.
func (ti *TestIndex) PackageFiles(dir string) []string {
	var files []string
	ti.addWholePackage(dir, func(f string) { files = append(files, f) })
	return files
}

// addWholePackage feeds every indexed file of dir — its test files and its
// production sources both — to add. Split out of CoveringFiles so that
// method stays within the cognitive-complexity budget: inlined, its two
// loops sit four levels deep inside the name resolution above.
func (ti *TestIndex) addWholePackage(dir string, add func(string)) {
	for _, f := range ti.AllInDir(dir) {
		add(f)
	}
	for _, f := range ti.srcDir[dir] {
		add(f)
	}
}
