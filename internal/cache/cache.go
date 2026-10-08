// Package cache implements PIT-style incremental analysis: per-mutant
// outcomes are persisted keyed by content hashes of the mutated file, the
// rest of its package's production sources (its //go:embed inputs
// included), and the files that decide what the covering tests assert. On
// the next run, mutants whose hashes still match are skipped.
//
// The cache is a single JSON file (default .gomutants-cache.json), opt-in
// via the --cache flag. NOT_COVERED is intentionally not cached — coverage
// is recomputed every run by discover.FilterByCoverage, which keeps "no
// longer covered" reclassifications correct.
//
// tests_hash is computed from the union of files identified by the
// TestFilesForFn callback (typically backed by the per-test coverage map
// + TestIndex). This handles cross-package -coverpkg correctly: tests in
// package B that exercise code in package A invalidate A's mutants when
// edited — and so do B's own helpers and production sources, which is
// where an end-to-end test keeps most of what it asserts.
package cache

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/szhekpisov/gomutants/internal/mutator"
)

// SchemaVersion is the on-disk format version. Bump when Entry shape or
// any hashing algorithm changes so old cache files are silently
// discarded rather than producing wrong skips.
//
//	v1: package-dir tests_hash (replaced — undercounted cross-package coverage).
//	v2: per-mutant tests_hash via TestFilesForFn; TIMED_OUT now gated on tests_hash.
//	v3: top-level CoverageKey/CoverageProfile so warm no-op runs can skip
//	    `go test -coverprofile`. HashCoverageInputs framing is part of the
//	    invariant — any algorithm change there demands another bump.
//	v4: GoToolchain joins the metadata gate, and EQUIVALENT is a reusable
//	    status. Equivalence verdicts depend on the compiler, so a toolchain
//	    change must discard the cache.
//	v5: TestFlags joins the metadata gate. --test-flags changes what the
//	    inner `go test` actually executes, so a verdict recorded under one
//	    value must not be replayed under another.
//	v6: TestFlags cache identity preserves argv order. v5 sorted distinct
//	    flag names, but arbitrary test-binary flags can interact even when
//	    their names differ, so old keys can describe a different run.
//	v7: pkg_hash joins the per-mutant key. Through v6 the key covered only
//	    the mutated file and the covering tests, so editing a *sibling*
//	    file in the same package replayed a stale verdict — a KILLED mutant
//	    whose kill depended on a constant next door stayed KILLED after that
//	    constant changed, without executing anything. v6 entries carry no
//	    pkg_hash and must not be replayed. The coverage key gained an
//	    //go:embed dimension in the same version: a v7 profile recorded
//	    before it simply mismatches and is recomputed, which is why this
//	    framing change rides on v7 rather than a bump of its own.
//	v8: LIVED means the mutant survived every test suite that can kill it,
//	    not just the tests the coverage map routed it to, which could miss
//	    a test that covers the line only after another test has run. v7
//	    LIVED entries can be such false survivors. With -coverpkg the
//	    tests dimension now spans every package whose tests link the
//	    mutant's, as all of them decide a LIVED verdict. Entries record
//	    whether the mutant was re-checked, so a cache hit counts in the
//	    report's re-check totals as the run that tested it did.
//	v9: KILLED means the mutant failed tests that pass together without it.
//	    Through v8 a mutant could be routed to covering tests that each
//	    pass alone but fail together on unmutated code — one leaves state
//	    behind that breaks another — so v8 KILLED entries can be such
//	    false kills.
const SchemaVersion = 9

// I/O syscalls used by Save are exposed as package-level function
// variables so tests can inject failures into each error path
// independently. Production code calls them through these vars; tests
// swap them out for stubs that return controlled errors. Mirrors the
// `var preReadFilesFunc = ...` pattern in main.go.
var (
	osMkdirAll = os.MkdirAll
	osChmod    = os.Chmod
	osRename   = os.Rename
	osRemove   = os.Remove

	// newSaveSink wraps the os.CreateTemp call so Save's flow runs
	// against an interface (saveSink) rather than *os.File directly.
	// This is what lets a test simulate "Encode fails / Close
	// succeeds" or "Encode succeeds / Close fails" — a contrast that
	// can't be produced from a real *os.File without filesystem-
	// specific tricks.
	newSaveSink = func(dir, pattern string) (saveSink, error) {
		return os.CreateTemp(dir, pattern)
	}
)

// saveSink is the minimal surface Save needs from a temp-file handle:
// write the encoded JSON, close the descriptor, and report the on-disk
// path so Chmod/Rename/Remove can target it. *os.File satisfies this
// directly; tests substitute a fake to inject controlled errors.
type saveSink interface {
	io.Writer
	io.Closer
	Name() string
}

// Cache is the on-disk artifact. Entries are keyed by mutant identity
// (rel_file, line, col, type, start_offset, original, replacement).
//
// CoverageKey/CoverageProfile (v3+) memoize the output of
// `go test -count=1 -coverprofile` so warm no-op runs can skip the
// coverage subprocess. The key fingerprints every input that could
// change the profile (see HashCoverageInputs); on a match the stored
// profile is parsed via coverage.ParseBytes instead. The omitempty
// tags are defensive forward-compat: a future code path that skips
// writing the coverage fields (e.g. when the user disables coverage
// caching) won't pollute the JSON with empty strings.
type Cache struct {
	SchemaVersion int    `json:"schema_version"`
	GoModule      string `json:"go_module"`
	ToolVersion   string `json:"tool_version"`
	// BuildTags is the `--tags` value the cache was built with. It joins
	// the metadata-gate identity (alongside SchemaVersion/GoModule/
	// ToolVersion): changing build tags can flip a mutant's outcome by
	// pulling in different compiled-in code even when the mutated source
	// and its test files are byte-identical, so a tag change must discard
	// the whole cache. omitempty + the default "" keeps pre-existing
	// (tag-less) caches reusable for tag-less runs.
	BuildTags string `json:"build_tags,omitempty"`
	// TestFlags is the `--test-flags` value the cache was built with, in
	// canonical form — whitespace collapsed but argv order preserved (see
	// config.CanonicalTestFlags), so behaviorally distinct orderings cannot
	// share a generation. It joins the metadata-gate identity for
	// the same reason BuildTags does, but the pressure here is sharper:
	// the documented workflow is alternating between a cheap gate run
	// (`--test-flags '-rapid.checks=20'`) and a full scoring run, and
	// per-mutant verdicts are not comparable across the two. A LIVED
	// recorded at 20 checks must not be replayed as the verdict for a
	// 100-check run, nor a KILLED under a full suite for a `-short` one.
	// omitempty + the default "" keeps flag-less caches reusable for
	// flag-less runs.
	TestFlags string `json:"test_flags,omitempty"`
	// GoToolchain is the project's `go version` string the cache was built
	// with. It joins the metadata-gate identity because the EQUIVALENT
	// classification is decided by the compiler's generated code, which can
	// change between toolchains; a toolchain change must therefore discard
	// the cache rather than reuse stale equivalence (or any other) verdicts.
	// omitempty + the default "" keeps pre-v4 caches inert (they fail the
	// gate on the SchemaVersion bump anyway).
	GoToolchain     string  `json:"go_toolchain,omitempty"`
	CoverageKey     string  `json:"coverage_key,omitempty"`
	CoverageProfile string  `json:"coverage_profile,omitempty"`
	Entries         []Entry `json:"entries"`
}

// Entry is one cached mutant outcome.
type Entry struct {
	RelFile     string `json:"rel_file"`
	Line        int    `json:"line"`
	Col         int    `json:"col"`
	Type        string `json:"type"`
	StartOffset int    `json:"start_offset"`
	Original    string `json:"original"`
	Replacement string `json:"replacement"`
	ProdHash    string `json:"prod_hash"`
	// PkgHash (v7+) fingerprints the non-test .go files in the mutant's
	// own package directory — see Hasher.HashPkgFiles. ProdHash alone
	// describes the mutated file; a mutant's verdict is decided by the
	// whole package that file compiles into, so this is the dimension
	// that catches a sibling file changing underneath a byte-identical
	// mutated file. It gates every reusable status, NOT_VIABLE included.
	PkgHash    string `json:"pkg_hash,omitempty"`
	TestsHash  string `json:"tests_hash"`
	Status     string `json:"status"`
	DurationMs int64  `json:"duration_ms"`
	// Rechecked (v8+) is the mutant's Rechecked, restored on a hit so the
	// report's re-check counts don't depend on how many mutants hit.
	Rechecked bool `json:"rechecked,omitempty"`
}

// key returns the identity tuple used for cache lookups.
func (e Entry) key() entryKey {
	return entryKey{
		RelFile:     e.RelFile,
		Line:        e.Line,
		Col:         e.Col,
		Type:        e.Type,
		StartOffset: e.StartOffset,
		Original:    e.Original,
		Replacement: e.Replacement,
	}
}

type entryKey struct {
	RelFile     string
	Line        int
	Col         int
	Type        string
	StartOffset int
	Original    string
	Replacement string
}

func mutantKey(m mutator.Mutant) entryKey {
	return entryKey{
		RelFile:     m.RelFile,
		Line:        m.Line,
		Col:         m.Col,
		Type:        string(m.Type),
		StartOffset: m.StartOffset,
		Original:    m.Original,
		Replacement: m.Replacement,
	}
}

// HashFile returns the hex-encoded sha256 of the file at absPath.
//
// Implemented over os.ReadFile rather than open+io.Copy: the file is
// already small (cache entries are keyed off prod-source files we
// already pre-read into memory in the discovery phase, and test files
// in the rare cold-cache path), and a single error path means no
// hidden mutants where one if-err return is shadowed by another.
func HashFile(absPath string) (string, error) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// Hasher memoizes per-file hashes within a single run. Not safe for
// concurrent use — the pipeline calls Lookup/Update sequentially.
type Hasher struct {
	files    map[string]string   // absPath → hex sha256
	dirs     map[string]string   // package dir → hex sha256 over its non-test .go files
	srcCache map[string][]byte   // optional in-memory source files (from discover.PreReadFiles)
	embeds   map[string][]string // package dir → dir-relative //go:embed file paths
}

// NewHasher returns an empty per-run hasher. If srcCache is non-nil, it
// is used as a fast path for File() so production sources already read
// into memory by the discovery phase aren't re-read from disk.
func NewHasher(srcCache map[string][]byte) *Hasher {
	return &Hasher{
		files:    make(map[string]string),
		dirs:     make(map[string]string),
		srcCache: srcCache,
	}
}

// SetSrcCache attaches an in-memory source map (typically populated by
// discover.PreReadFiles) after the hasher was constructed. Used by
// callers that need a Hasher before pre-read completes (e.g. the
// coverage-key calc runs before discovery) and want subsequent File()
// calls to skip the disk read. Already-memoized hashes in h.files are
// preserved.
func (h *Hasher) SetSrcCache(srcCache map[string][]byte) {
	h.srcCache = srcCache
}

// SetEmbedFiles attaches the //go:embed inputs of each package, keyed by
// absolute package directory and valued by the dir-relative slash paths
// `go list` resolved the patterns to. HashPkgFiles folds them into
// pkg_hash, so editing an embedded file invalidates the mutants of the
// package that embeds it.
//
// Called after package resolution, which happens later than the Hasher's
// construction (the coverage-key calc needs a Hasher first) — hence a
// setter rather than a constructor argument. Directories absent from the
// map contribute no embed inputs, which is the correct answer for the
// packages that have none.
//
// There is no memo to invalidate here: HashPkgFiles is reached only from
// Lookup and Update, both of which run well after package resolution, so
// no directory hash predates this call.
func (h *Hasher) SetEmbedFiles(embeds map[string][]string) {
	h.embeds = embeds
}

// File returns the hash of absPath, computing it on first call.
func (h *Hasher) File(absPath string) (string, error) {
	if v, ok := h.files[absPath]; ok {
		return v, nil
	}
	if data, ok := h.srcCache[absPath]; ok {
		sum := sha256.Sum256(data)
		v := hex.EncodeToString(sum[:])
		h.files[absPath] = v
		return v, nil
	}
	v, err := HashFile(absPath)
	if err != nil {
		return "", err
	}
	h.files[absPath] = v
	return v, nil
}

// HashTestFiles returns a stable hex-encoded sha256 over the union of
// the test files in absPaths. Inputs are sorted and de-duplicated, so
// any iteration order produces the same hash. The per-file Fprintf
// uses length-prefixed framing so concatenation can't alias one file
// boundary into another.
//
// An empty (or nil) input returns the hash of the empty string — the
// same value the loop produces naturally without an early return, so
// no special-case branch is needed. A read error on any file is
// propagated — callers should treat this as a cache miss for that
// mutant.
func (h *Hasher) HashTestFiles(absPaths []string) (string, error) {
	sorted := append([]string(nil), absPaths...)
	slices.Sort(sorted)
	uniq := sorted[:0]
	for i, p := range sorted {
		if i > 0 && p == sorted[i-1] {
			continue
		}
		uniq = append(uniq, p)
	}

	hh := sha256.New()
	for _, p := range uniq {
		fileHex, err := h.File(p)
		if err != nil {
			return "", err
		}
		// Length-prefixed framing collapses basename and content hash
		// into a single Write so neither field can be silently dropped
		// by a STATEMENT_REMOVE mutation, and the explicit byte length
		// prevents any boundary aliasing between adjacent files (e.g.
		// "a"+hash colliding with "ahash"+content).
		base := filepath.Base(p)
		fmt.Fprintf(hh, "%d:%s|%s|", len(base), base, fileHex)
	}
	return hex.EncodeToString(hh.Sum(nil)), nil
}

// goFilesIn returns every .go file directly inside the given package dirs
// (non-recursive), deduped and sorted so the result is independent of input
// ordering or repeats. Extracted from HashCoverageInputs to keep that
// method's cognitive complexity within the linter's threshold.
//
// skipTests drops _test.go files. HashCoverageInputs wants them (a test file
// decides which lines the profile marks covered); HashPkgFiles does not,
// because folding them in here would invalidate a package's NOT_VIABLE
// entries — whose reuse is explicitly test-independent — on every test edit.
// Test content is instead carried by tests_hash, and carried at package
// granularity: TestIndex.CoveringFiles always includes every _test.go in the
// mutant's own directory, not just the files declaring its covering tests,
// so a package-level test helper that declares no test entry point (and that
// therefore no coverage-derived name can resolve to) still gates reuse.
func goFilesIn(pkgDirs []string, skipTests bool) ([]string, error) {
	seen := make(map[string]bool, len(pkgDirs))
	var files []string
	for _, dir := range pkgDirs {
		if seen[dir] {
			continue
		}
		seen[dir] = true
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, fmt.Errorf("listing %s: %w", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
				continue
			}
			if skipTests && strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	slices.Sort(files)
	return files, nil
}

// embedFrames returns one length-prefixed hash frame per //go:embed input
// of the given package directories, in an order that depends only on the
// set of directories and not on how the caller ordered or repeated them.
// A directory the caller never described via SetEmbedFiles contributes no
// frames — the right answer for the packages with no embed directives, and
// the pre-embed behavior for any directory outside the resolved set.
//
// Frames are the shared piece of pkg_hash and the coverage key: embedded
// data decides both a mutant's verdict (a test asserting on a parsed
// schema.json) and which lines the coverage profile marks covered (a test
// iterating an embedded table calls into code that an empty table never
// reaches), so both dimensions have to carry it.
//
// Inputs are framed by their dir-relative slash path rather than their
// basename: they can sit in subdirectories, where two files may share a
// basename (data/a/x.json, data/b/x.json) that a basename-only framing
// would alias together. Directories are sorted rather than emitted in
// call order so the same package set hashes identically however it was
// enumerated, and a directory named twice contributes its frames once;
// within a directory the rel paths are sorted and deduped too.
//
// A read error is propagated — callers treat it as a cache miss, never as
// a match.
func (h *Hasher) embedFrames(pkgDirs []string) ([]string, error) {
	dirs := slices.Clone(pkgDirs)
	slices.Sort(dirs)

	var frames []string
	for i, dir := range dirs {
		// Sorted, so a repeat sits next to its twin. Skipped here rather
		// than compacted away: slices.Compact zeroes the tail it drops, and
		// an empty directory name carries no embed inputs, so keeping or
		// dropping the `dirs = slices.Compact(dirs)` assignment hashed the
		// same — an unobservable statement. The rel-path Compact below is
		// observable for the opposite reason: an empty rel path resolves to
		// the directory itself, which fails to read.
		if i > 0 && dir == dirs[i-1] {
			continue
		}
		rels := slices.Clone(h.embeds[dir])
		slices.Sort(rels)
		rels = slices.Compact(rels)
		for _, rel := range rels {
			fileHex, err := h.File(filepath.Join(dir, filepath.FromSlash(rel)))
			if err != nil {
				return nil, err
			}
			frames = append(frames, fmt.Sprintf("%d:%s|%s|", len(rel), rel, fileHex))
		}
	}
	return frames, nil
}

// HashPkgFiles returns a stable hex-encoded sha256 over every non-test .go
// file directly inside dir, followed by every file the package's production
// //go:embed directives pull in. It is the per-mutant key's dependency
// dimension: a mutant's verdict is decided by the whole package it compiles
// into, not just the file the mutation lives in, so a sibling file changing
// must invalidate that mutant even when its own file is byte-identical.
//
// Embedded files count as package inputs for the same reason. A mutant
// killed by a test that asserts on a parsed schema.json survives once that
// schema changes, with every .go file byte-identical — the same stale-KILLED
// shape a sibling .go edit produces. They are hashed only when the caller
// supplied them via SetEmbedFiles; a package with no embed directives (the
// overwhelming majority) writes nothing extra and hashes exactly as before.
//
// Framing matches HashTestFiles — sorted, deduped, length-prefixed — so no
// STATEMENT_REMOVE or boundary-alias mutation against the writes can go
// unobserved. The embed half is framed by embedFrames, which the coverage
// key shares.
//
// Results are memoized per directory for the lifetime of the Hasher, so a
// package is listed and hashed once per run rather than once per mutant.
// The memo is keyed on dir alone; a run never edits its own sources
// mid-flight, so there is nothing to stale out.
//
// A read or listing error is propagated — callers treat it as a cache miss
// for that mutant, never as a match.
func (h *Hasher) HashPkgFiles(dir string) (string, error) {
	if v, ok := h.dirs[dir]; ok {
		return v, nil
	}
	files, err := goFilesIn([]string{dir}, true)
	if err != nil {
		return "", err
	}
	hh := sha256.New()
	for _, p := range files {
		fileHex, err := h.File(p)
		if err != nil {
			return "", err
		}
		base := filepath.Base(p)
		fmt.Fprintf(hh, "%d:%s|%s|", len(base), base, fileHex)
	}
	frames, err := h.embedFrames([]string{dir})
	if err != nil {
		return "", err
	}
	for _, f := range frames {
		fmt.Fprint(hh, f)
	}
	v := hex.EncodeToString(hh.Sum(nil))
	h.dirs[dir] = v
	return v, nil
}

// HashCoverageInputs returns a stable fingerprint of every input that
// could change the coverage profile produced by `go test -coverprofile`.
// A mismatch on the next run invalidates the cached profile.
//
// pkgDirs must contain every directory whose source could land in the
// profile, not just the user's target packages. With -coverpkg=<pattern>
// covering a broader set than the target, the caller is responsible for
// resolving the expanded list (e.g. via a second go-list call) before
// invoking this method.
//
// Each input dimension is hashed with the same length-prefixed framing
// used by HashTestFiles so STATEMENT_REMOVE / boundary-alias mutations
// against the new writes are observable from the tests. A read error on
// any required file is propagated — callers should treat it as a miss
// and re-run coverage.
//
// go.sum is optional: a missing file contributes the empty-content hash.
// This keeps the helper usable on modules with no recorded checksums
// (single-module repos with no external deps) without an extra branch
// at every call site.
// testFlags is the user's --test-flags string. It is a hash dimension
// because those flags reach the coverage run itself: `-short` can skip
// tests, which changes which lines the profile marks covered, so a profile
// recorded under one value must never be replayed under another.
func (h *Hasher) HashCoverageInputs(pkgDirs []string, projectDir, coverPkg, tags, testFlags, toolchain, envSnapshot string) (string, error) {
	// 1. Collect every .go file under pkgDirs (deduped + sorted) so the hash
	// is independent of pkgDir ordering. Extracted into goFilesIn to keep
	// this method's cognitive complexity in check. h.File is used below so
	// the per-run sha256 memo is shared with HashTestFiles and Lookup.
	files, err := goFilesIn(pkgDirs, false)
	if err != nil {
		return "", err
	}

	hh := sha256.New()
	for _, p := range files {
		fileHex, err := h.File(p)
		if err != nil {
			return "", err
		}
		// Mirror HashTestFiles framing: length-prefixed basename + hash.
		// The same anti-aliasing reasoning applies.
		base := filepath.Base(p)
		fmt.Fprintf(hh, "%d:%s|%s|", len(base), base, fileHex)
	}

	// 2. Fold in the //go:embed inputs of those same directories. A test
	// that iterates an embedded table decides which lines the profile
	// marks covered exactly as its .go source does — add a case that
	// reaches a previously untouched branch and every .go file is still
	// byte-identical, so without this the stale profile would be replayed
	// and the mutants on those lines would stay NOT_COVERED, untested.
	// Frames carry an "embed:" prefix so an embedded x.go can't alias the
	// production x.go framed above.
	//
	// Only directories the caller described via SetEmbedFiles contribute;
	// with a coverage scope wider than the resolved target packages (the
	// --integration closure, a broader -coverpkg) the extra directories'
	// embed inputs stay outside the key, as do //go:embed directives in
	// _test.go files, which go list reports separately and discover does
	// not collect.
	frames, err := h.embedFrames(pkgDirs)
	if err != nil {
		return "", fmt.Errorf("hashing embedded files: %w", err)
	}
	for _, f := range frames {
		fmt.Fprintf(hh, "embed:%s", f)
	}

	// 3. Hash go.mod (required) and go.sum (optional).
	modHex, err := h.File(filepath.Join(projectDir, "go.mod"))
	if err != nil {
		return "", fmt.Errorf("hashing go.mod: %w", err)
	}
	fmt.Fprintf(hh, "gomod:%d:%s|", len(modHex), modHex)

	sumHex := ""
	if v, err := h.File(filepath.Join(projectDir, "go.sum")); err == nil {
		sumHex = v
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("hashing go.sum: %w", err)
	}
	fmt.Fprintf(hh, "gosum:%d:%s|", len(sumHex), sumHex)

	// 4. Mix in the configuration / toolchain dimensions with the same
	// length-prefixed framing so adjacent values can't alias.
	fmt.Fprintf(hh, "coverpkg:%d:%s|", len(coverPkg), coverPkg)
	fmt.Fprintf(hh, "tags:%d:%s|", len(tags), tags)
	fmt.Fprintf(hh, "testflags:%d:%s|", len(testFlags), testFlags)
	fmt.Fprintf(hh, "toolchain:%d:%s|", len(toolchain), toolchain)
	fmt.Fprintf(hh, "env:%d:%s|", len(envSnapshot), envSnapshot)

	return hex.EncodeToString(hh.Sum(nil)), nil
}

// Load reads the cache from path. Returns an empty (but valid) Cache
// stamped with the caller's identity if the file is missing, fails to
// parse, or has a mismatched schema/module/tool version. Callers should
// treat the returned Cache as authoritative regardless of error — Load
// never returns nil.
//
// Mutator definitions can change between tool versions, so stale
// entries can silently produce wrong skips. Pessimistic invalidation
// on any metadata mismatch is the safe default; the metadata gate is
// the *only* observable rejection path because the read- and
// parse-failure branches both produce a zero-value Cache that fails
// the gate identically (SchemaVersion=0 ≠ caller's SchemaVersion).
func Load(path, goModule, toolVersion, buildTags, testFlags, goToolchain string) *Cache {
	empty := &Cache{
		SchemaVersion: SchemaVersion,
		GoModule:      goModule,
		ToolVersion:   toolVersion,
		BuildTags:     buildTags,
		TestFlags:     testFlags,
		GoToolchain:   goToolchain,
	}
	var c Cache
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &c) // c stays zero-value on parse error → fails metadata gate.
	}
	if c.SchemaVersion != SchemaVersion || c.GoModule != goModule || c.ToolVersion != toolVersion ||
		c.BuildTags != buildTags || c.TestFlags != testFlags || c.GoToolchain != goToolchain {
		return empty
	}
	return &c
}

// Save writes c to path, creating parent directories as needed. The
// write is atomic within the target's filesystem: serialization goes
// to a temp file in the same directory, then os.Rename swaps it into
// place. A crash before the rename leaves the prior cache file
// untouched; a crash after leaves the new one fully written. Either
// way the file on disk parses successfully on the next Load.
func Save(c *Cache, path string) error {
	if path == "" {
		return nil
	}
	dir := filepath.Dir(path)
	if err := osMkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Temp file must live in the same directory as the target so
	// os.Rename stays atomic — cross-filesystem renames degrade to
	// copy+unlink on some platforms.
	tmp, err := newSaveSink(dir, ".gomutants-cache-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// committed flips to true once the rename has placed the file at
	// `path`; the deferred cleanup removes the *original* tmp path
	// only on the failure path. (Once committed, tmpName no longer
	// exists on disk, so calling Remove on it would be wrong.)
	committed := false
	defer func() {
		if !committed {
			_ = osRemove(tmpName)
		}
	}()

	// Encode + Close happen unconditionally (we always want to release
	// the file descriptor) and the first non-nil error wins. Wiring it
	// this way means an Encode failure that was followed by a
	// successful Close still surfaces — without this, a mutant that
	// drops the encode-error return would silently produce a bogus
	// cache file.
	encodeErr := json.NewEncoder(tmp).Encode(c)
	closeErr := tmp.Close()
	if encodeErr != nil {
		return encodeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := osChmod(tmpName, 0o644); err != nil {
		return err
	}
	if err := osRename(tmpName, path); err != nil {
		return err
	}
	committed = true
	return nil
}

// TestFilesForFn resolves a mutant to the set of absolute test-file
// paths whose contents gate cache reuse for that mutant. The integrator
// wires this through TestIndex.CoveringFiles: every _test.go in the
// mutant's package directory, plus the files declaring whichever tests
// the per-test coverage map attributes to the mutant (those can live in
// another package under -coverpkg, and that package is then hashed whole).
// The package-wide halves are what keep an edit to a test helper — which
// declares no test entry point, so no coverage-derived name names it —
// from replaying a stale verdict.
type TestFilesForFn func(m mutator.Mutant) []string

// Lookup applies cache hits to the mutants slice in place. For each
// pending mutant whose identity key + content hashes match a cached
// entry, sets Status, Duration, Rechecked and FromCache so the runner's
// Pending-only filter naturally skips it. Returns the number of hits.
//
// Skip rules — pkg_hash gates every reusable status:
//
//	prior=KILLED      + prod_hash + pkg_hash + tests_hash match → reuse
//	prior=LIVED       + prod_hash + pkg_hash + tests_hash match → reuse
//	prior=TIMED_OUT   + prod_hash + pkg_hash + tests_hash match → reuse
//	prior=EQUIVALENT  + prod_hash + pkg_hash + tests_hash match → reuse
//	prior=NOT_VIABLE  + prod_hash + pkg_hash match (compile failure — tests irrelevant) → reuse
//	otherwise → leave Pending
//
// TIMED_OUT is gated on tests because adding a faster killer test could
// legitimately turn a prior timeout into KILLED on the next run; without
// the gate we'd silently skip a now-killable mutant. NOT_VIABLE is the one
// outcome independent of *test* content (it failed to compile), so it
// alone skips the tests_hash check — but it is not independent of the rest
// of the package: a sibling file supplying a missing symbol turns a prior
// NOT_VIABLE into a real, testable mutant, so pkg_hash still gates it.
//
// pkg_hash exists because prod_hash describes only the mutated file. A
// mutant compiles as part of its whole package, so its verdict can flip
// when a *sibling* file changes while the mutated file and the covering
// tests stay byte-identical — a KILLED mutant whose kill depends on a
// constant defined next door survives once that constant changes. Without
// this dimension the cache replays the stale KILLED without executing
// anything, which is the dangerous direction to be wrong in: silent false
// confidence.
//
// Scope, stated plainly. Between them the three dimensions cover: the
// mutated file, every other non-test .go file in its package, that
// package's //go:embed inputs, every _test.go file in its package, and —
// for a covering test the coverage map places in another package, which
// takes an instrumentation scope wider than the package under test
// (--integration, or an explicit --coverpkg) — that package's test *and*
// production sources.
//
// That last half is conditional on the wider scope for a reason: the
// coverage map's test names carry no package, so without one a foreign
// declaring directory is a name collision rather than a dependency, and
// expanding on it would fold unrelated packages' sources into tests_hash.
// See TestIndex.CoveringFiles.
//
// What is left: an edit in a package the mutant's package *imports*. That
// mutant still replays a stale verdict. Closing it needs the forward import
// closure, and the churn it would add is real — a change to a widely
// imported package would invalidate most of the repository's cache. The
// --integration direction is not in that bucket even though it looks
// similar: the reverse-dependency closure is already computed for that mode
// and the covering package is named for us by the coverage map, so it costs
// nothing to hash.
//
// Also outside the key: //go:embed inputs of *test* files (go list's
// TestEmbedFiles/XTestEmbedFiles). A golden file embedded by a test and
// edited to expect a looser value replays a stale KILLED, the same shape as
// the production-embed case that is closed above.
//
// Hash failures (unreadable file/dir) are silently treated as a miss
// for that mutant so a transient I/O error never produces a wrong skip.
//
// The "no entries" and "key not found" cases fall through naturally:
// indexing an empty (or nil — see below) idx with `idx[k]` returns the
// zero-value Entry whose Status="" parses to Pending and fails
// canReuse, so no extra short-circuit is needed.
func (c *Cache) Lookup(mutants []mutator.Mutant, h *Hasher, testFilesFor TestFilesForFn) int {
	if c == nil {
		return 0
	}
	idx := make(map[entryKey]Entry, len(c.Entries))
	for _, e := range c.Entries {
		idx[e.key()] = e
	}
	hits := 0
	for i := range mutants {
		m := &mutants[i]
		if m.Status != mutator.StatusPending {
			continue
		}
		entry := idx[mutantKey(*m)] // zero-value Entry on miss; fails canReuse below.
		status := parseStatus(entry.Status)
		if !canReuse(status) {
			continue
		}

		prodHash, err := h.File(m.File)
		if err != nil || prodHash != entry.ProdHash {
			continue
		}
		pkgHash, err := h.HashPkgFiles(filepath.Dir(m.File))
		if err != nil || pkgHash != entry.PkgHash {
			continue
		}
		if needsTestsHash(status) {
			testsHash, err := h.HashTestFiles(testFilesFor(*m))
			if err != nil || testsHash != entry.TestsHash {
				continue
			}
		}

		m.Status = status
		m.Duration = time.Duration(entry.DurationMs) * time.Millisecond
		m.Rechecked = entry.Rechecked
		m.FromCache = true
		hits++
	}
	return hits
}

// canReuse reports whether a status is one we cache + reuse on the next
// run. PENDING / NOT_COVERED are not reusable — the first means the
// prior run never finished; the second is recomputed every run from the
// fresh coverage profile.
func canReuse(s mutator.MutantStatus) bool {
	switch s {
	case mutator.StatusKilled,
		mutator.StatusLived,
		mutator.StatusTimedOut,
		mutator.StatusNotViable,
		mutator.StatusEquivalent:
		return true
	}
	return false
}

// needsTestsHash reports whether a cacheable status's reuse depends on
// the test files matching, in addition to the production source. Only
// NOT_VIABLE is independent of test content (it failed to compile, so no
// test ever ran), which exempts it from this check alone — prod_hash and
// pkg_hash still gate it, since a sibling file can turn an uncompilable
// mutant into a compilable one.
//
// EQUIVALENT is compiler-proven from the source alone, but we still stamp
// and gate its tests_hash: an equivalent mutant is also a LIVED one, and
// when equivalence detection is off this run the caller downgrades a
// cached EQUIVALENT back to LIVED — a reuse that must observe the same
// tests-changed invalidation as any LIVED outcome.
func needsTestsHash(s mutator.MutantStatus) bool {
	switch s {
	case mutator.StatusKilled, mutator.StatusLived, mutator.StatusTimedOut, mutator.StatusEquivalent:
		return true
	}
	return false
}

// parseStatus maps an on-disk status string back to a MutantStatus.
// Unknown values fall through to Pending so a corrupted cache entry
// can never silently produce a terminal status.
func parseStatus(s string) mutator.MutantStatus {
	switch s {
	case mutator.StatusKilled.String():
		return mutator.StatusKilled
	case mutator.StatusLived.String():
		return mutator.StatusLived
	case mutator.StatusTimedOut.String():
		return mutator.StatusTimedOut
	case mutator.StatusNotViable.String():
		return mutator.StatusNotViable
	case mutator.StatusEquivalent.String():
		return mutator.StatusEquivalent
	}
	return mutator.StatusPending
}

// Update merges this run's results into c and drops entries whose
// prod_hash or pkg_hash no longer matches the current source. Entries
// for files outside this run's mutant set (e.g. excluded by
// --changed-since) are preserved when their file still exists with
// matching content *and* their package still hashes the same — a
// carried-over entry is reused by the next run's Lookup, so it has to
// clear the same two gates Lookup applies.
//
// projectDir lets us resolve a stored RelFile back to an absolute path
// for re-hashing prior entries.
func (c *Cache) Update(mutants []mutator.Mutant, h *Hasher, projectDir string, testFilesFor TestFilesForFn) {
	if c == nil {
		return
	}

	// 1. Build new entries from this run for any mutant with a
	//    cacheable terminal status. Cache hits (FromCache=true) keep
	//    the same content, just re-emitted.
	newByKey := make(map[entryKey]Entry, len(mutants))
	for _, m := range mutants {
		if !canReuse(m.Status) {
			continue
		}
		prodHash, err := h.File(m.File)
		if err != nil {
			continue
		}
		// pkg_hash gates every reusable status, so an entry we cannot
		// stamp it on is unusable — skip rather than write a hit that
		// Lookup would have to reject.
		pkgHash, err := h.HashPkgFiles(filepath.Dir(m.File))
		if err != nil {
			continue
		}
		entry := Entry{
			RelFile:     m.RelFile,
			Line:        m.Line,
			Col:         m.Col,
			Type:        string(m.Type),
			StartOffset: m.StartOffset,
			Original:    m.Original,
			Replacement: m.Replacement,
			ProdHash:    prodHash,
			PkgHash:     pkgHash,
			Status:      m.Status.String(),
			DurationMs:  m.Duration.Milliseconds(),
			Rechecked:   m.Rechecked,
		}
		// tests_hash is only meaningful for statuses where it gates
		// reuse. Stamp it for KILLED/LIVED/TIMED_OUT so future
		// Lookups can compare; NOT_VIABLE leaves it empty.
		if needsTestsHash(m.Status) {
			testsHash, err := h.HashTestFiles(testFilesFor(m))
			if err == nil {
				entry.TestsHash = testsHash
			}
		}
		newByKey[entry.key()] = entry
	}

	// 2. Carry over prior entries whose source still hashes the same and
	//    that this run did not overwrite. Both dimensions are re-checked,
	//    not just prod: an entry whose package moved on is one Lookup
	//    would reject anyway, so keeping it only grows the file with
	//    weight no run can spend.
	//
	//    This step is garbage collection, not a safety gate — Lookup
	//    re-verifies both hashes on the next run and treats its own hash
	//    errors as a miss — so the three outcomes below are chosen for
	//    what they do to the *file*, not to correctness:
	//
	//      source gone      → drop; it can never match again, and keeping
	//                         it would grow the file without bound
	//      unreadable       → keep; we cannot tell whether it is stale,
	//                         and Update runs on every checkpoint, so
	//                         dropping would discard a package's warm
	//                         cache over one transient EMFILE/EIO
	//      readable         → keep only on a full match
	//
	//    Errors never reach the comparisons, which is what keeps a failed
	//    hash's "" from matching an entry that carries an empty hash —
	//    the laundering TestLookup_PkgHashErrorIsAMiss locks out on the
	//    Lookup side.
	for _, prior := range c.Entries {
		if _, overwritten := newByKey[prior.key()]; overwritten {
			continue
		}
		abs := filepath.Join(projectDir, prior.RelFile)
		curHash, fileErr := h.File(abs)
		curPkgHash, pkgErr := h.HashPkgFiles(filepath.Dir(abs))
		switch {
		case errors.Is(fileErr, fs.ErrNotExist) || errors.Is(pkgErr, fs.ErrNotExist):
			continue
		case fileErr != nil || pkgErr != nil:
			newByKey[prior.key()] = prior
		case curHash == prior.ProdHash && curPkgHash == prior.PkgHash:
			newByKey[prior.key()] = prior
		}
	}

	// 3. Emit entries in deterministic order so the on-disk file
	//    diffs cleanly between runs.
	merged := make([]Entry, 0, len(newByKey))
	for _, e := range newByKey {
		merged = append(merged, e)
	}
	// cmp.Or chain: each Compare returns 0 on tie (delegating to the
	// next field) or non-zero with the right sign. Original/Replacement
	// tie-break the (file,line,col,offset,type) identity-key collision
	// case so two such entries always emit in a deterministic order.
	slices.SortFunc(merged, func(a, b Entry) int {
		return cmp.Or(
			cmp.Compare(a.RelFile, b.RelFile),
			cmp.Compare(a.Line, b.Line),
			cmp.Compare(a.Col, b.Col),
			cmp.Compare(a.StartOffset, b.StartOffset),
			cmp.Compare(a.Type, b.Type),
			cmp.Compare(a.Original, b.Original),
			cmp.Compare(a.Replacement, b.Replacement),
		)
	})
	c.Entries = merged
}

// String returns a short human-readable summary of the cache state for
// debug output.
func (c *Cache) String() string {
	if c == nil {
		return "<nil>"
	}
	return fmt.Sprintf("Cache{module=%s tool=%s entries=%d}",
		c.GoModule, c.ToolVersion, len(c.Entries))
}
