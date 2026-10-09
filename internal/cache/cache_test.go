package cache

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/szhekpisov/gomutants/internal/mutator"
)

const (
	testModule  = "github.com/example/proj"
	testVersion = "0.1.0"
)

func TestHashFile_StableAndContentSensitive(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.go")
	if err := os.WriteFile(p, []byte("package x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	h1, err := HashFile(p)
	if err != nil {
		t.Fatalf("hash1: %v", err)
	}
	h2, err := HashFile(p)
	if err != nil {
		t.Fatalf("hash2: %v", err)
	}
	if h1 != h2 {
		t.Fatalf("hash not stable: %s vs %s", h1, h2)
	}

	if err := os.WriteFile(p, []byte("package x\n\n"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	h3, err := HashFile(p)
	if err != nil {
		t.Fatalf("hash3: %v", err)
	}
	if h3 == h1 {
		t.Fatalf("hash unchanged after content edit: %s", h3)
	}
}

func TestHashFile_Missing(t *testing.T) {
	if _, err := HashFile(filepath.Join(t.TempDir(), "nope.go")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestHasher_UsesSrcCacheBeforeDisk(t *testing.T) {
	// File on disk has different content than the srcCache entry — the
	// hasher must hash the in-memory bytes, never falling back to disk.
	dir := t.TempDir()
	p := filepath.Join(dir, "x.go")
	mustWrite(t, p, "DISK CONTENT\n")

	memBody := []byte("MEMORY CONTENT\n")
	h := NewHasher(map[string][]byte{p: memBody})
	got, err := h.File(p)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	want, err := HashFile(p) // hashes the disk file
	if err != nil {
		t.Fatalf("disk hash: %v", err)
	}
	if got == want {
		t.Fatalf("hasher used disk content, expected srcCache hit")
	}
}

// TestHasher_SetSrcCacheAttachesAfterConstruction verifies the post-hoc
// srcCache wire-up used by main.go (Hasher is created before
// PreReadFiles for the coverage-key calc, then srcCache is attached
// once discovery completes). A subsequent File() call on a brand-new
// path must hit the in-memory bytes rather than the disk file.
func TestHasher_SetSrcCacheAttachesAfterConstruction(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.go")
	mustWrite(t, p, "DISK CONTENT\n")

	h := NewHasher(nil) // empty srcCache up front
	memBody := []byte("MEMORY CONTENT\n")
	h.SetSrcCache(map[string][]byte{p: memBody})

	got, err := h.File(p)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	disk, err := HashFile(p)
	if err != nil {
		t.Fatalf("disk hash: %v", err)
	}
	if got == disk {
		t.Fatalf("SetSrcCache attachment didn't take effect — hasher hashed the disk file")
	}
}

// TestHasher_SetSrcCacheDropsFileMemo: a hash memoized from disk before
// the source map was attached (the coverage-key calc) must not outlive it.
// The run measures the bytes in srcCache, so an edit saved in between has
// to change the hash its verdict is stored under.
func TestHasher_SetSrcCacheDropsFileMemo(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.go")
	mustWrite(t, p, "DISK CONTENT\n")

	h := NewHasher(nil)
	disk, err := h.File(p)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	h.SetSrcCache(map[string][]byte{p: []byte("MEMORY CONTENT\n")})

	got, err := h.File(p)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if got == disk {
		t.Fatalf("File returned the hash memoized from disk before SetSrcCache")
	}
}

// TestHasher_SetSrcCacheDropsDirMemo: the same for the per-directory
// pkg_hash memo, which would otherwise pin every sibling file's old hash.
func TestHasher_SetSrcCacheDropsDirMemo(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.go")
	mustWrite(t, p, "package x\n")

	h := NewHasher(nil)
	before, err := h.HashPkgFiles(dir)
	if err != nil {
		t.Fatalf("HashPkgFiles: %v", err)
	}
	h.SetSrcCache(map[string][]byte{p: []byte("package x // edited\n")})

	after, err := h.HashPkgFiles(dir)
	if err != nil {
		t.Fatalf("HashPkgFiles: %v", err)
	}
	if after == before {
		t.Fatalf("HashPkgFiles returned the directory hash memoized before SetSrcCache")
	}
}

// TestHasher_SetSrcCacheKeepsOtherMemos: a hash memoized for a file the
// source map does not hold — a test file, hashed by the coverage-key calc
// before any mutant ran — must survive. Re-reading it at write time would
// key a verdict measured on the old test under an edit saved mid-run.
func TestHasher_SetSrcCacheKeepsOtherMemos(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "x.go")
	test := filepath.Join(dir, "x_test.go")
	mustWrite(t, src, "package x\n")
	mustWrite(t, test, "package x // before\n")

	h := NewHasher(nil)
	before, err := h.File(test)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	mustWrite(t, test, "package x // edited mid-run\n")
	h.SetSrcCache(map[string][]byte{src: []byte("package x\n")})

	got, err := h.File(test)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if got != before {
		t.Fatalf("File re-read a file SetSrcCache does not hold; its pre-run hash was dropped")
	}
}

// TestHasher_SetSrcCacheKeepsOtherDirMemos: only the directories srcCache
// has a file in lose their pkg_hash memo.
func TestHasher_SetSrcCacheKeepsOtherDirMemos(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	for _, d := range []string{a, b} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, filepath.Join(a, "a.go"), "package a\n")
	mustWrite(t, filepath.Join(b, "b.go"), "package b\n")

	h := NewHasher(nil)
	before, err := h.HashPkgFiles(b)
	if err != nil {
		t.Fatalf("HashPkgFiles: %v", err)
	}
	mustWrite(t, filepath.Join(b, "b.go"), "package b // edited\n")
	h.SetSrcCache(map[string][]byte{filepath.Join(a, "a.go"): []byte("package a\n")})

	got, err := h.HashPkgFiles(b)
	if err != nil {
		t.Fatalf("HashPkgFiles: %v", err)
	}
	if got != before {
		t.Fatalf("HashPkgFiles dropped the memo of a directory SetSrcCache holds no file in")
	}
}

// freshPkgHash is HashPkgFiles(dir) on a new Hasher holding srcCache and
// embeds: what a package hashes to with nothing memoized.
func freshPkgHash(t *testing.T, dir string, srcCache map[string][]byte, embeds map[string][]string) string {
	t.Helper()
	h := NewHasher(srcCache)
	h.SetEmbedFiles(embeds)
	v, err := h.HashPkgFiles(dir)
	if err != nil {
		t.Fatalf("HashPkgFiles(%s): %v", dir, err)
	}
	return v
}

// TestHasher_SetSrcCacheRehashesExcludedSibling: a production file
// srcCache leaves out (--exclude-files, a build tag) still compiles into
// every mutant from disk. Its hash memoized before coverage must not
// outlive SetSrcCache, or an edit saved during the baseline would be
// measured but keyed on the version before it.
func TestHasher_SetSrcCacheRehashesExcludedSibling(t *testing.T) {
	dir := t.TempDir()
	src, gen := filepath.Join(dir, "x.go"), filepath.Join(dir, "gen.go")
	mustWrite(t, src, "package x\n")
	mustWrite(t, gen, "package x // before\n")

	h := NewHasher(nil)
	if _, err := h.File(gen); err != nil {
		t.Fatalf("hash: %v", err)
	}
	mustWrite(t, gen, "package x // edited during the baseline\n")
	srcCache := map[string][]byte{src: []byte("package x\n")}
	h.SetSrcCache(srcCache)

	got, err := h.HashPkgFiles(dir)
	if err != nil {
		t.Fatalf("HashPkgFiles: %v", err)
	}
	if want := freshPkgHash(t, dir, srcCache, nil); got != want {
		t.Fatalf("HashPkgFiles kept the pre-coverage hash of a sibling srcCache does not hold")
	}
}

// TestHasher_SetSrcCacheRehashesEmbedFile: the same for an embedded file,
// which sits in a subdirectory of the package.
func TestHasher_SetSrcCacheRehashesEmbedFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	src, data := filepath.Join(dir, "x.go"), filepath.Join(dir, "data", "schema.json")
	mustWrite(t, src, "package x\n")
	mustWrite(t, data, "{}\n")
	embeds := map[string][]string{dir: {"data/schema.json"}}

	h := NewHasher(nil)
	h.SetEmbedFiles(embeds)
	if _, err := h.File(data); err != nil {
		t.Fatalf("hash: %v", err)
	}
	mustWrite(t, data, "{\"edited\": true}\n")
	srcCache := map[string][]byte{src: []byte("package x\n")}
	h.SetSrcCache(srcCache)

	got, err := h.HashPkgFiles(dir)
	if err != nil {
		t.Fatalf("HashPkgFiles: %v", err)
	}
	if want := freshPkgHash(t, dir, srcCache, embeds); got != want {
		t.Fatalf("HashPkgFiles kept the pre-coverage hash of an embedded file")
	}
}

// TestHasher_SetSrcCacheSnapshotsPkgHash: the package hash is taken when
// srcCache is attached, not on first use. A --run-mutant-id run first asks
// for it in Update, after the mutant ran; a sibling edited during the run
// must not be recorded against a verdict measured on the old one.
func TestHasher_SetSrcCacheSnapshotsPkgHash(t *testing.T) {
	dir := t.TempDir()
	src, gen := filepath.Join(dir, "x.go"), filepath.Join(dir, "gen.go")
	mustWrite(t, src, "package x\n")
	mustWrite(t, gen, "package x // as measured\n")
	srcCache := map[string][]byte{src: []byte("package x\n")}
	want := freshPkgHash(t, dir, srcCache, nil)

	h := NewHasher(nil)
	h.SetSrcCache(srcCache)
	mustWrite(t, gen, "package x // edited during the mutant run\n")

	got, err := h.HashPkgFiles(dir)
	if err != nil {
		t.Fatalf("HashPkgFiles: %v", err)
	}
	if got != want {
		t.Fatalf("HashPkgFiles hashed the package on first use, not when SetSrcCache froze its sources")
	}
}

func TestHashTestFiles_OrderInvariantAndContentSensitive(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a_test.go")
	b := filepath.Join(dir, "b_test.go")
	mustWrite(t, a, "package x\n")
	mustWrite(t, b, "package x\n")

	h := NewHasher(nil)
	h1, err := h.HashTestFiles([]string{a, b})
	if err != nil {
		t.Fatalf("hash1: %v", err)
	}
	// Reverse order — must produce the same hash.
	h2, err := NewHasher(nil).HashTestFiles([]string{b, a})
	if err != nil {
		t.Fatalf("hash2: %v", err)
	}
	if h1 != h2 {
		t.Fatalf("hash depends on input order: %s vs %s", h1, h2)
	}

	// Edit a — hash must change.
	mustWrite(t, a, "package x\n\n")
	h3, err := NewHasher(nil).HashTestFiles([]string{a, b})
	if err != nil {
		t.Fatalf("hash3: %v", err)
	}
	if h3 == h1 {
		t.Fatal("hash unchanged after content edit")
	}
}

func TestHashTestFiles_EmptyAndDuplicates(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a_test.go")
	mustWrite(t, a, "package x\n")

	h := NewHasher(nil)
	hEmpty, err := h.HashTestFiles(nil)
	if err != nil {
		t.Fatalf("empty: %v", err)
	}
	hSingle, err := h.HashTestFiles([]string{a})
	if err != nil {
		t.Fatalf("single: %v", err)
	}
	hDup, err := h.HashTestFiles([]string{a, a})
	if err != nil {
		t.Fatalf("dup: %v", err)
	}
	if hEmpty == hSingle {
		t.Fatal("empty and single inputs hash to the same value")
	}
	if hSingle != hDup {
		t.Fatalf("duplicates not deduped: %s vs %s", hSingle, hDup)
	}
}

func TestHashTestFiles_MissingFilePropagatesError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.go")
	if _, err := NewHasher(nil).HashTestFiles([]string{missing}); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestCacheString(t *testing.T) {
	var nilC *Cache
	if got := nilC.String(); got != "<nil>" {
		t.Errorf("nil cache String = %q, want \"<nil>\"", got)
	}
	c := &Cache{GoModule: "m", ToolVersion: "v", Entries: []Entry{{}, {}, {}}}
	got := c.String()
	if !strings.Contains(got, "module=m") || !strings.Contains(got, "tool=v") || !strings.Contains(got, "entries=3") {
		t.Errorf("String() = %q, missing module/tool/entries fields", got)
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	c := &Cache{
		SchemaVersion: SchemaVersion,
		GoModule:      testModule,
		ToolVersion:   testVersion,
		Entries: []Entry{
			{
				RelFile: "pkg/x.go", Line: 10, Col: 5,
				Type: "ARITHMETIC_BASE", StartOffset: 100,
				Original: "+", Replacement: "-",
				ProdHash: "abc", TestsHash: "def",
				Status: "KILLED", DurationMs: 42,
			},
		},
	}
	if err := Save(c, path); err != nil {
		t.Fatalf("save: %v", err)
	}
	got := Load(path, testModule, testVersion, "", "", "")
	if len(got.Entries) != 1 || got.Entries[0] != c.Entries[0] {
		t.Fatalf("round-trip mismatch: %+v", got.Entries)
	}
}

// TestSaveLoad_RoundTripCoverageFields locks down the v3 CoverageKey /
// CoverageProfile fields: a Save then Load must preserve them byte-for-byte,
// otherwise a future warm-cache hit would parse a different profile than
// the one captured (silent stale-skip bug).
func TestSaveLoad_RoundTripCoverageFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	profileBody := "mode: set\npkg/x.go:1.2,3.4 1 1\n"
	c := &Cache{
		SchemaVersion:   SchemaVersion,
		GoModule:        testModule,
		ToolVersion:     testVersion,
		CoverageKey:     "deadbeefcafe",
		CoverageProfile: profileBody,
	}
	if err := Save(c, path); err != nil {
		t.Fatalf("save: %v", err)
	}
	got := Load(path, testModule, testVersion, "", "", "")
	if got.CoverageKey != "deadbeefcafe" {
		t.Errorf("CoverageKey not preserved: got %q", got.CoverageKey)
	}
	if got.CoverageProfile != profileBody {
		t.Errorf("CoverageProfile not preserved: got %q", got.CoverageProfile)
	}
}

func TestLoad_EmptyPath(t *testing.T) {
	c := Load("", testModule, testVersion, "", "", "")
	if c == nil || len(c.Entries) != 0 {
		t.Fatalf("expected empty cache, got %+v", c)
	}
	if c.GoModule != testModule || c.ToolVersion != testVersion {
		t.Fatalf("metadata not stamped: %+v", c)
	}
}

func TestLoad_Missing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nonexistent.json")
	c := Load(p, testModule, testVersion, "", "", "")
	if len(c.Entries) != 0 {
		t.Fatalf("expected empty cache for missing file, got %d entries", len(c.Entries))
	}
}

func TestLoad_GarbageJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cache.json")
	mustWrite(t, p, "{not json")
	c := Load(p, testModule, testVersion, "", "", "")
	if len(c.Entries) != 0 {
		t.Fatal("expected empty cache for garbage")
	}
}

func TestLoad_SchemaVersionMismatch(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cache.json")
	mustWrite(t, p, `{"schema_version":99,"go_module":"`+testModule+`","tool_version":"`+testVersion+`","entries":[{"rel_file":"x.go","status":"KILLED"}]}`)
	c := Load(p, testModule, testVersion, "", "", "")
	if len(c.Entries) != 0 {
		t.Fatal("expected empty cache for schema mismatch")
	}
}

// TestLoad_SchemaVersionPinned hardcodes the current on-disk schema number
// so that bumping or nudging the SchemaVersion constant without intent is
// caught: a cache written at literal version 9 must load under the current
// constant. (Pins SchemaVersion == 9; kills off-by-one mutations of it.)
func TestLoad_SchemaVersionPinned(t *testing.T) {
	if SchemaVersion != 9 {
		t.Fatalf("SchemaVersion = %d; update this pinned test and the on-disk fixture deliberately", SchemaVersion)
	}
	p := filepath.Join(t.TempDir(), "cache.json")
	mustWrite(t, p, fmt.Sprintf(`{"schema_version":9,"go_module":"%s","tool_version":"%s","entries":[{"rel_file":"x.go","status":"KILLED"}]}`, testModule, testVersion))
	c := Load(p, testModule, testVersion, "", "", "")
	if len(c.Entries) != 1 {
		t.Fatalf("a literal-version-9 cache must load under SchemaVersion=9, got %d entries", len(c.Entries))
	}
}

// TestLoad_V8CacheRejected pins the v9 bump's reason: a v8 KILLED entry
// may record a mutant routed to covering tests that fail together without
// it, so the metadata gate must discard it wholesale.
func TestLoad_V8CacheRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cache.json")
	mustWrite(t, p, fmt.Sprintf(`{"schema_version":8,"go_module":"%s","tool_version":"%s","entries":[{"rel_file":"x.go","status":"KILLED"}]}`, testModule, testVersion))
	c := Load(p, testModule, testVersion, "", "", "")
	if len(c.Entries) != 0 {
		t.Fatalf("a v8 cache must be discarded under SchemaVersion=9, got %d entries", len(c.Entries))
	}
}

// TestLoad_V7CacheRejected pins the v8 bump's reason: a v7 LIVED entry
// may record a survivor its covering tests missed, never re-checked
// against the full suites, so the metadata gate must discard it wholesale.
func TestLoad_V7CacheRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cache.json")
	mustWrite(t, p, fmt.Sprintf(`{"schema_version":7,"go_module":"%s","tool_version":"%s","entries":[{"rel_file":"x.go","status":"LIVED"}]}`, testModule, testVersion))
	c := Load(p, testModule, testVersion, "", "", "")
	if len(c.Entries) != 0 {
		t.Fatalf("a v7 cache must be discarded under SchemaVersion=9, got %d entries", len(c.Entries))
	}
}

// TestLoad_V6CacheRejected pins the v7 bump's reason. A v6 cache carries no
// pkg_hash on any entry, so replaying it would reuse verdicts keyed on the
// mutated file alone — exactly the sibling-file staleness v7 exists to
// close. The metadata gate must discard it wholesale.
func TestLoad_V6CacheRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cache.json")
	mustWrite(t, p, fmt.Sprintf(`{"schema_version":6,"go_module":"%s","tool_version":"%s","entries":[{"rel_file":"x.go","status":"KILLED"}]}`, testModule, testVersion))
	c := Load(p, testModule, testVersion, "", "", "")
	if len(c.Entries) != 0 {
		t.Fatalf("a v6 cache must be discarded under SchemaVersion=9, got %d entries", len(c.Entries))
	}
}

func TestLoad_ModuleMismatch(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cache.json")
	mustWrite(t, p, fmt.Sprintf(`{"schema_version":%d,"go_module":"other/mod","tool_version":"%s","entries":[{"rel_file":"x.go","status":"KILLED"}]}`, SchemaVersion, testVersion))
	c := Load(p, testModule, testVersion, "", "", "")
	if len(c.Entries) != 0 {
		t.Fatal("expected empty cache for module mismatch")
	}
}

func TestLoad_ToolVersionMismatch(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cache.json")
	mustWrite(t, p, fmt.Sprintf(`{"schema_version":%d,"go_module":"%s","tool_version":"0.0.9","entries":[{"rel_file":"x.go","status":"KILLED"}]}`, SchemaVersion, testModule))
	c := Load(p, testModule, testVersion, "", "", "")
	if len(c.Entries) != 0 {
		t.Fatal("expected empty cache for tool-version mismatch")
	}
}

// loadGateDims enumerates the free-form string dimensions of Load's
// metadata gate. All three behave identically — a mismatch discards the
// whole cache, a match reuses it, and an absent field reads as "" — so
// they are exercised by one table rather than three near-identical tests.
// Each entry carries the *reason* its dimension gates, since that is the
// part a reader cannot recover from the mechanics.
var loadGateDims = []struct {
	name    string // dimension name, for failure messages
	jsonKey string // the cache-file field it round-trips through
	why     string // why a change here must discard cached verdicts
	stored  string // value baked into the fixture
	other   string // a different value, to force a mismatch
	// load calls Load with value in this dimension's argument position,
	// leaving the others empty. get reads the same dimension back off the
	// returned Cache.
	load func(path, value string) *Cache
	get  func(c *Cache) string
}{
	{
		name: "build tags", jsonKey: "build_tags",
		why:    "a tag change pulls in different compiled-in code, flipping outcomes without touching source or test hashes",
		stored: "integration", other: "e2e",
		load: func(p, v string) *Cache { return Load(p, testModule, testVersion, v, "", "") },
		get:  func(c *Cache) string { return c.BuildTags },
	},
	{
		name: "test flags", jsonKey: "test_flags",
		why:    "the documented workflow alternates a cheap --test-flags gate run with a full scoring run, and a LIVED earned at 20 rapid checks is not the verdict for a 100-check run",
		stored: "-rapid.checks=20", other: "-short",
		load: func(p, v string) *Cache { return Load(p, testModule, testVersion, "", v, "") },
		get:  func(c *Cache) string { return c.TestFlags },
	},
	{
		name: "go toolchain", jsonKey: "go_toolchain",
		why:    "EQUIVALENT verdicts are decided by the compiler's generated code, which changes between toolchains",
		stored: "go1.26.1", other: "go1.27.0",
		load: func(p, v string) *Cache { return Load(p, testModule, testVersion, "", "", v) },
		get:  func(c *Cache) string { return c.GoToolchain },
	},
}

// writeGateFixture writes a one-entry cache file whose metadata matches the
// test module/version, optionally carrying jsonKey:value. Passing an empty
// jsonKey omits the field entirely, which is how a cache written before the
// dimension existed looks on disk.
func writeGateFixture(t *testing.T, jsonKey, value string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cache.json")
	field := ""
	if jsonKey != "" {
		field = fmt.Sprintf(`"%s":"%s",`, jsonKey, value)
	}
	mustWrite(t, p, fmt.Sprintf(
		`{"schema_version":%d,"go_module":"%s","tool_version":"%s",%s"entries":[{"rel_file":"x.go","status":"KILLED"}]}`,
		SchemaVersion, testModule, testVersion, field))
	return p
}

// TestLoad_MetadataGateDimensions pins every string dimension of the gate
// in both directions. Kills CONDITIONALS_NEGATION and the STATEMENT_REMOVE /
// logical mutants on each `c.X != x` clause in Load: dropping a clause makes
// the mismatch rows reuse a stale cache, and negating one makes the matching
// row throw a good cache away.
func TestLoad_MetadataGateDimensions(t *testing.T) {
	for _, dim := range loadGateDims {
		t.Run(dim.name, func(t *testing.T) {
			p := writeGateFixture(t, dim.jsonKey, dim.stored)

			// A different value → discard. This is the direction that
			// matters: reusing here yields a wrong result, for the
			// reason the dimension records in `why`.
			if c := dim.load(p, dim.other); len(c.Entries) != 0 {
				t.Errorf("%s=%q must discard a cache built with %q, got %d entries (%s)",
					dim.name, dim.other, dim.stored, len(c.Entries), dim.why)
			}
			// Requesting the default against a non-default cache is
			// still a mismatch — "" must not be treated as a wildcard.
			if c := dim.load(p, ""); len(c.Entries) != 0 {
				t.Errorf("unset %s must discard a cache built with %q, got %d entries",
					dim.name, dim.stored, len(c.Entries))
			}
			// The same value → reuse. Without this the gate could pass
			// the rows above by rejecting unconditionally.
			if c := dim.load(p, dim.stored); len(c.Entries) != 1 {
				t.Errorf("matching %s=%q must reuse the cache, got %d entries",
					dim.name, dim.stored, len(c.Entries))
			}
		})
	}
}

// TestLoad_MetadataGateBackCompat pins that a cache file written before a
// dimension existed stays reusable for a run that doesn't use it. The
// absent field unmarshals to "" and must compare equal to an unset
// argument, so adding a dimension costs nothing to the common case of
// never setting it.
func TestLoad_MetadataGateBackCompat(t *testing.T) {
	for _, dim := range loadGateDims {
		t.Run(dim.name, func(t *testing.T) {
			p := writeGateFixture(t, "", "") // field omitted entirely
			if c := dim.load(p, ""); len(c.Entries) != 1 {
				t.Errorf("a cache with no %s field must stay reusable for an unset run, got %d entries",
					dim.jsonKey, len(c.Entries))
			}
		})
	}
}

// TestLoad_MetadataGateStampsRequested pins that the empty cache handed
// back on a gate miss carries the *requested* value, not the rejected
// file's. Otherwise the run would recompute every verdict and then save it
// stamped with the old value, so the next matching run would miss again
// and the cache would never converge.
func TestLoad_MetadataGateStampsRequested(t *testing.T) {
	for _, dim := range loadGateDims {
		t.Run(dim.name, func(t *testing.T) {
			p := writeGateFixture(t, dim.jsonKey, dim.stored)
			c := dim.load(p, dim.other)
			if got := dim.get(c); got != dim.other {
				t.Errorf("discarded cache stamped %s=%q, want the requested %q", dim.name, got, dim.other)
			}
		})
	}
}

// TestLoad_V2CacheRejectedAfterV3Bump locks down the v2→v3 silent-discard
// path. A v2 file with otherwise-matching metadata must surface as an
// empty cache once the running tool's SchemaVersion is 3+.
func TestLoad_V2CacheRejectedAfterV3Bump(t *testing.T) {
	if SchemaVersion < 3 {
		t.Skip("only relevant once SchemaVersion has been bumped past 2")
	}
	p := filepath.Join(t.TempDir(), "cache.json")
	mustWrite(t, p, fmt.Sprintf(`{"schema_version":2,"go_module":"%s","tool_version":"%s","entries":[{"rel_file":"x.go","status":"KILLED"}]}`, testModule, testVersion))
	c := Load(p, testModule, testVersion, "", "", "")
	if len(c.Entries) != 0 {
		t.Fatalf("expected empty cache (v2 rejected by v%d Load); got %d entries", SchemaVersion, len(c.Entries))
	}
}

func TestSave_CreatesParentDir(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "deeply", "nested", "cache.json")
	c := &Cache{SchemaVersion: SchemaVersion, GoModule: testModule, ToolVersion: testVersion}
	if err := Save(c, path); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not created: %v", err)
	}
}

func TestSave_EmptyPathIsNoOp(t *testing.T) {
	if err := Save(&Cache{}, ""); err != nil {
		t.Fatalf("save empty: %v", err)
	}
}

// TestSave_LeavesNoTempFiles asserts the atomic-rename path cleans up
// after itself on the success path: only the target file should remain
// in the directory after Save.
func TestSave_LeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")
	c := &Cache{SchemaVersion: SchemaVersion, GoModule: testModule, ToolVersion: testVersion}
	if err := Save(c, path); err != nil {
		t.Fatalf("save: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "cache.json" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected only cache.json, got %v", names)
	}
}

// TestSave_RewriteIsAtomic asserts that overwriting an existing cache
// file replaces it cleanly (no partial-write window): the file is
// always parseable as a valid Cache before AND after a Save call.
func TestSave_RewriteIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")

	first := &Cache{
		SchemaVersion: SchemaVersion, GoModule: testModule, ToolVersion: testVersion,
		Entries: []Entry{{RelFile: "a.go", Line: 1, Type: "ARITHMETIC_BASE", Status: mutator.StatusKilled.String()}},
	}
	if err := Save(first, path); err != nil {
		t.Fatalf("save 1: %v", err)
	}

	second := &Cache{
		SchemaVersion: SchemaVersion, GoModule: testModule, ToolVersion: testVersion,
		Entries: []Entry{{RelFile: "b.go", Line: 2, Type: "ARITHMETIC_BASE", Status: mutator.StatusLived.String()}},
	}
	if err := Save(second, path); err != nil {
		t.Fatalf("save 2: %v", err)
	}

	got := Load(path, testModule, testVersion, "", "", "")
	if len(got.Entries) != 1 || got.Entries[0].RelFile != "b.go" {
		t.Fatalf("rewrite did not replace cleanly: %+v", got.Entries)
	}
}

// lookupCase represents one row of the Lookup hit/miss matrix.
type lookupCase struct {
	name           string
	priorStatus    mutator.MutantStatus
	prodHashMatch  bool
	pkgHashMatch   bool
	testsHashMatch bool
	wantHit        bool
}

func TestLookup_HitMissMatrix(t *testing.T) {
	cases := []lookupCase{
		{"killed all match", mutator.StatusKilled, true, true, true, true},
		{"killed prod mismatch", mutator.StatusKilled, false, true, true, false},
		{"killed pkg mismatch", mutator.StatusKilled, true, false, true, false},
		{"killed tests mismatch", mutator.StatusKilled, true, true, false, false},
		{"lived all match", mutator.StatusLived, true, true, true, true},
		{"lived pkg mismatch", mutator.StatusLived, true, false, true, false},
		{"lived tests mismatch", mutator.StatusLived, true, true, false, false},
		// TIMED_OUT now requires tests_hash match — adding a faster
		// killer test could legitimately turn a prior timeout into KILLED.
		{"timed_out all match", mutator.StatusTimedOut, true, true, true, true},
		{"timed_out tests mismatch", mutator.StatusTimedOut, true, true, false, false},
		{"timed_out prod mismatch", mutator.StatusTimedOut, false, true, true, false},
		{"timed_out pkg mismatch", mutator.StatusTimedOut, true, false, true, false},
		// NOT_VIABLE is independent of *test* content (it never ran a
		// test), so tests_hash does not gate it — but it is not
		// independent of the rest of the package: a sibling file
		// supplying a missing symbol makes a previously uncompilable
		// mutant compile, so pkg_hash still gates it.
		{"not_viable prod+pkg match", mutator.StatusNotViable, true, true, false, true},
		{"not_viable prod mismatch", mutator.StatusNotViable, false, true, false, false},
		{"not_viable pkg mismatch", mutator.StatusNotViable, true, false, false, false},
		{"infra_error never reused", mutator.StatusInfraError, true, true, true, false},
		{"not_covered never reused", mutator.StatusNotCovered, true, true, true, false},
		{"pending never reused", mutator.StatusPending, true, true, true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runLookupCase(t, tc)
		})
	}
}

func runLookupCase(t *testing.T, tc lookupCase) {
	t.Helper()
	dir := t.TempDir()
	prodPath := filepath.Join(dir, "x.go")
	testPath := filepath.Join(dir, "x_test.go")
	mustWrite(t, prodPath, "package x\n")
	mustWrite(t, testPath, "package x\n")

	prodHash, err := HashFile(prodPath)
	if err != nil {
		t.Fatalf("prodHash: %v", err)
	}
	testsHash, err := NewHasher(nil).HashTestFiles([]string{testPath})
	if err != nil {
		t.Fatalf("testsHash: %v", err)
	}
	pkgHash := mustPkgHash(t, dir)
	storedProd := prodHash
	storedPkg := pkgHash
	storedTests := testsHash
	if !tc.prodHashMatch {
		storedProd = "stale-prod"
	}
	if !tc.pkgHashMatch {
		storedPkg = "stale-pkg"
	}
	if !tc.testsHashMatch {
		storedTests = "stale-tests"
	}

	c := &Cache{
		SchemaVersion: SchemaVersion,
		GoModule:      testModule,
		ToolVersion:   testVersion,
		Entries: []Entry{{
			RelFile: "x.go", Line: 1, Col: 1,
			Type: "ARITHMETIC_BASE", StartOffset: 0,
			Original: "+", Replacement: "-",
			ProdHash: storedProd, PkgHash: storedPkg, TestsHash: storedTests,
			Status: tc.priorStatus.String(), DurationMs: 7,
		}},
	}
	mutants := []mutator.Mutant{{
		ID: 1, Type: mutator.ArithmeticBase,
		File: prodPath, RelFile: "x.go",
		Line: 1, Col: 1, StartOffset: 0,
		Original: "+", Replacement: "-",
		Status: mutator.StatusPending,
	}}

	hits := c.Lookup(mutants, NewHasher(nil), pkgDirTestFilesFor)
	if tc.wantHit {
		if hits != 1 {
			t.Fatalf("expected hit, got %d hits", hits)
		}
		if !mutants[0].FromCache {
			t.Fatal("FromCache not set")
		}
		if mutants[0].Status != tc.priorStatus {
			t.Fatalf("Status=%s, want %s", mutants[0].Status, tc.priorStatus)
		}
		if mutants[0].Duration != 7*time.Millisecond {
			t.Fatalf("Duration=%v, want 7ms", mutants[0].Duration)
		}
	} else {
		if hits != 0 {
			t.Fatalf("expected miss, got %d hits", hits)
		}
		if mutants[0].FromCache {
			t.Fatal("FromCache set on miss")
		}
		if mutants[0].Status != mutator.StatusPending {
			t.Fatalf("Status=%s, want PENDING", mutants[0].Status)
		}
	}
}

func TestLookup_NonPendingNotTouched(t *testing.T) {
	dir := t.TempDir()
	prodPath := filepath.Join(dir, "x.go")
	mustWrite(t, prodPath, "package x\n")
	mustWrite(t, filepath.Join(dir, "x_test.go"), "package x\n")
	prod, _ := HashFile(prodPath)
	tests, _ := NewHasher(nil).HashTestFiles([]string{filepath.Join(dir, "x_test.go")})

	c := &Cache{
		SchemaVersion: SchemaVersion,
		GoModule:      testModule,
		ToolVersion:   testVersion,
		Entries: []Entry{{
			RelFile: "x.go", Line: 1, Col: 1, Type: "ARITHMETIC_BASE",
			Original: "+", Replacement: "-",
			ProdHash: prod, TestsHash: tests, Status: "KILLED",
		}},
	}
	// Mutant is already NotCovered (e.g. coverage filter promoted it).
	// Must remain NotCovered — cache reuse must not override.
	mutants := []mutator.Mutant{{
		ID: 1, Type: mutator.ArithmeticBase,
		File: prodPath, RelFile: "x.go",
		Line: 1, Col: 1,
		Original: "+", Replacement: "-",
		Status: mutator.StatusNotCovered,
	}}
	hits := c.Lookup(mutants, NewHasher(nil), pkgDirTestFilesFor)
	if hits != 0 {
		t.Fatalf("expected 0 hits on non-pending mutant, got %d", hits)
	}
	if mutants[0].Status != mutator.StatusNotCovered {
		t.Fatalf("Status changed: %s", mutants[0].Status)
	}
}

func TestLookup_UnknownIdentityKey(t *testing.T) {
	dir := t.TempDir()
	prodPath := filepath.Join(dir, "x.go")
	mustWrite(t, prodPath, "package x\n")
	prod, _ := HashFile(prodPath)

	c := &Cache{
		Entries: []Entry{{
			RelFile: "other.go", Line: 1, Col: 1, Type: "ARITHMETIC_BASE",
			Original: "+", Replacement: "-",
			ProdHash: prod, Status: "KILLED",
		}},
	}
	mutants := []mutator.Mutant{{
		ID: 1, File: prodPath, RelFile: "x.go", Line: 1, Col: 1,
		Type: mutator.ArithmeticBase, Original: "+", Replacement: "-",
		Status: mutator.StatusPending,
	}}
	if hits := c.Lookup(mutants, NewHasher(nil), pkgDirTestFilesFor); hits != 0 {
		t.Fatalf("expected miss, got %d", hits)
	}
}

func TestLookup_NilCache(t *testing.T) {
	mutants := []mutator.Mutant{{Status: mutator.StatusPending}}
	var c *Cache
	if hits := c.Lookup(mutants, NewHasher(nil), pkgDirTestFilesFor); hits != 0 {
		t.Fatalf("nil cache should return 0 hits, got %d", hits)
	}
}

func TestLookup_CorruptedStatusFallsThrough(t *testing.T) {
	dir := t.TempDir()
	prodPath := filepath.Join(dir, "x.go")
	mustWrite(t, prodPath, "package x\n")
	prod, _ := HashFile(prodPath)
	c := &Cache{
		Entries: []Entry{{
			RelFile: "x.go", Line: 1, Col: 1, Type: "ARITHMETIC_BASE",
			Original: "+", Replacement: "-",
			ProdHash: prod, Status: "WAT",
		}},
	}
	mutants := []mutator.Mutant{{
		ID: 1, File: prodPath, RelFile: "x.go", Line: 1, Col: 1,
		Type: mutator.ArithmeticBase, Original: "+", Replacement: "-",
		Status: mutator.StatusPending,
	}}
	if hits := c.Lookup(mutants, NewHasher(nil), pkgDirTestFilesFor); hits != 0 {
		t.Fatalf("corrupted status must miss, got %d hits", hits)
	}
}

func TestUpdate_KeepsUnchangedCarriesNewDropsStale(t *testing.T) {
	root := t.TempDir()
	// Two prod files in SEPARATE packages. pkga/file_a.go stays the same;
	// pkgb/file_b.go is rewritten. The packages must be separate for this
	// test to isolate per-file carry-over: pkg_hash makes every entry in a
	// package share that package's fingerprint, so co-locating the two
	// would (correctly) drop file_a's entry too — the behavior
	// TestUpdate_DropsSiblingEntriesOnPackageChange covers.
	dirA := filepath.Join(root, "pkga")
	dirB := filepath.Join(root, "pkgb")
	if err := os.MkdirAll(dirA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dirB, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dirA, "file_a.go"), "package a\n")
	mustWrite(t, filepath.Join(dirB, "file_b.go"), "package b\n// v1\n")

	hA, _ := HashFile(filepath.Join(dirA, "file_a.go"))
	hB, _ := HashFile(filepath.Join(dirB, "file_b.go"))
	pkgA := mustPkgHash(t, dirA)
	pkgB := mustPkgHash(t, dirB)

	c := &Cache{
		SchemaVersion: SchemaVersion, GoModule: testModule, ToolVersion: testVersion,
		Entries: []Entry{
			{ // prior entry for file_a — package unchanged, should be carried over.
				RelFile: filepath.Join("pkga", "file_a.go"), Line: 1, Col: 1,
				Type: "ARITHMETIC_BASE", Original: "+", Replacement: "-",
				ProdHash: hA, PkgHash: pkgA, Status: "KILLED",
			},
			{ // prior entry for file_b — about to be rewritten, should drop.
				RelFile: filepath.Join("pkgb", "file_b.go"), Line: 1, Col: 1,
				Type: "ARITHMETIC_BASE", Original: "+", Replacement: "-",
				ProdHash: hB, PkgHash: pkgB, Status: "KILLED",
			},
		},
	}

	// Rewrite file_b so its prior hash no longer matches.
	mustWrite(t, filepath.Join(dirB, "file_b.go"), "package b\n// v2\n")

	// This run only tested a NEW mutant in file_b (different line/col).
	mutants := []mutator.Mutant{{
		ID: 1, Type: mutator.ArithmeticBase,
		File: filepath.Join(dirB, "file_b.go"), RelFile: filepath.Join("pkgb", "file_b.go"),
		Line: 5, Col: 3, Original: "*", Replacement: "/",
		Status: mutator.StatusLived, Duration: 11 * time.Millisecond,
	}}

	c.Update(mutants, NewHasher(nil), root, pkgDirTestFilesFor)

	if len(c.Entries) != 2 {
		t.Fatalf("expected 2 entries (file_a kept + file_b new), got %d: %+v", len(c.Entries), c.Entries)
	}

	relA := filepath.Join("pkga", "file_a.go")
	relB := filepath.Join("pkgb", "file_b.go")
	var sawA, sawNewB bool
	for _, e := range c.Entries {
		if e.RelFile == relA && e.Status == mutator.StatusKilled.String() {
			sawA = true
		}
		if e.RelFile == relB && e.Line == 5 && e.Status == mutator.StatusLived.String() {
			sawNewB = true
			if e.DurationMs != 11 {
				t.Errorf("DurationMs=%d, want 11", e.DurationMs)
			}
		}
		if e.RelFile == relB && e.Line == 1 {
			t.Errorf("stale file_b entry not dropped: %+v", e)
		}
	}
	if !sawA {
		t.Error("file_a entry was dropped — should be kept (package unchanged)")
	}
	if !sawNewB {
		t.Error("new file_b entry not added")
	}
}

// TestUpdate_DropsSiblingEntriesOnPackageChange is the carry-over half of
// the v7 cache-soundness fix. Update's carry-over loop re-checks pkg_hash,
// not just prod_hash: an entry for a byte-identical file whose *package*
// moved on could never be reused by Lookup, so keeping it only grows the
// cache file with weight no run can spend.
func TestUpdate_DropsSiblingEntriesOnPackageChange(t *testing.T) {
	root := t.TempDir()
	aPath := filepath.Join(root, "a.go")
	bPath := filepath.Join(root, "b.go")
	mustWrite(t, aPath, "package x\n")
	mustWrite(t, bPath, "package x\n// v1\n")

	hA, _ := HashFile(aPath)

	c := &Cache{
		SchemaVersion: SchemaVersion, GoModule: testModule, ToolVersion: testVersion,
		Entries: []Entry{{
			RelFile: "a.go", Line: 1, Col: 1,
			Type: "ARITHMETIC_BASE", Original: "+", Replacement: "-",
			ProdHash: hA, PkgHash: mustPkgHash(t, root), Status: "KILLED",
		}},
	}

	// Edit only the sibling. a.go itself is untouched, so prod_hash still
	// matches — pkg_hash is the only thing that can catch this.
	mustWrite(t, bPath, "package x\n// v2\n")

	c.Update(nil, NewHasher(nil), root, pkgDirTestFilesFor)

	if len(c.Entries) != 0 {
		t.Errorf("entry for a.go survived a sibling edit: %+v", c.Entries)
	}
}

func TestUpdate_StatusFiltering(t *testing.T) {
	root := t.TempDir()
	prodPath := filepath.Join(root, "x.go")
	mustWrite(t, prodPath, "package x\n")
	mustWrite(t, filepath.Join(root, "x_test.go"), "package x\n")

	c := &Cache{SchemaVersion: SchemaVersion, GoModule: testModule, ToolVersion: testVersion}

	mutants := []mutator.Mutant{
		// NOT_COVERED — must NOT be cached (recomputed each run).
		mkMutant(prodPath, 1, mutator.StatusNotCovered),
		// PENDING — must NOT be cached (incomplete run).
		mkMutant(prodPath, 2, mutator.StatusPending),
		// KILLED — cached.
		mkMutant(prodPath, 3, mutator.StatusKilled),
		// LIVED — cached.
		mkMutant(prodPath, 4, mutator.StatusLived),
		// TIMED_OUT — cached.
		mkMutant(prodPath, 5, mutator.StatusTimedOut),
		// NOT_VIABLE — cached.
		mkMutant(prodPath, 6, mutator.StatusNotViable),
		// INFRA_ERROR — must NOT be cached (environmental failure is transient).
		mkMutant(prodPath, 7, mutator.StatusInfraError),
	}
	c.Update(mutants, NewHasher(nil), root, pkgDirTestFilesFor)

	if len(c.Entries) != 4 {
		t.Fatalf("expected 4 cacheable entries, got %d: %+v", len(c.Entries), c.Entries)
	}

	for _, e := range c.Entries {
		if e.Status == mutator.StatusPending.String() ||
			e.Status == mutator.StatusNotCovered.String() ||
			e.Status == mutator.StatusInfraError.String() {
			t.Errorf("non-cacheable status %q persisted: %+v", e.Status, e)
		}
	}
}

// TestUpdateLookup_RoundTripsRechecked: whether a mutant was re-checked
// survives the cache, so a hit counts in the report's re-check totals as
// the run that tested it did, rather than those totals shrinking with the
// hit rate.
func TestUpdateLookup_RoundTripsRechecked(t *testing.T) {
	root := t.TempDir()
	prodPath := filepath.Join(root, "x.go")
	mustWrite(t, prodPath, "package x\n")
	mustWrite(t, filepath.Join(root, "x_test.go"), "package x\n")

	rechecked := mkMutant(prodPath, 1, mutator.StatusLived)
	rechecked.Rechecked = true
	c := &Cache{SchemaVersion: SchemaVersion, GoModule: testModule, ToolVersion: testVersion}
	c.Update([]mutator.Mutant{rechecked, mkMutant(prodPath, 2, mutator.StatusKilled)}, NewHasher(nil), root, pkgDirTestFilesFor)

	mutants := []mutator.Mutant{mkMutant(prodPath, 1, mutator.StatusPending), mkMutant(prodPath, 2, mutator.StatusPending)}
	if hits := c.Lookup(mutants, NewHasher(nil), pkgDirTestFilesFor); hits != 2 {
		t.Fatalf("Lookup hits = %d, want 2", hits)
	}
	if !mutants[0].Rechecked || mutants[1].Rechecked {
		t.Errorf("Rechecked = %v, %v after the round trip, want true, false", mutants[0].Rechecked, mutants[1].Rechecked)
	}
}

// TestUpdate_TestsHashStampedWhenGated asserts that Update writes
// tests_hash for exactly the statuses whose reuse is gated on it
// (KILLED/LIVED/TIMED_OUT) and leaves it empty for NOT_VIABLE (which
// reuses on prod_hash alone — a compile failure is purely a function of
// the mutated source).
func TestUpdate_TestsHashStampedWhenGated(t *testing.T) {
	root := t.TempDir()
	prodPath := filepath.Join(root, "x.go")
	mustWrite(t, prodPath, "package x\n")
	mustWrite(t, filepath.Join(root, "x_test.go"), "package x\n")

	c := &Cache{SchemaVersion: SchemaVersion, GoModule: testModule, ToolVersion: testVersion}
	mutants := []mutator.Mutant{
		mkMutant(prodPath, 1, mutator.StatusKilled),
		mkMutant(prodPath, 2, mutator.StatusLived),
		mkMutant(prodPath, 3, mutator.StatusTimedOut),
		mkMutant(prodPath, 4, mutator.StatusNotViable),
	}
	c.Update(mutants, NewHasher(nil), root, pkgDirTestFilesFor)

	for _, e := range c.Entries {
		switch e.Status {
		case mutator.StatusKilled.String(),
			mutator.StatusLived.String(),
			mutator.StatusTimedOut.String():
			if e.TestsHash == "" {
				t.Errorf("%s entry missing tests_hash: %+v", e.Status, e)
			}
		case mutator.StatusNotViable.String():
			if e.TestsHash != "" {
				t.Errorf("NOT_VIABLE entry has tests_hash %q (should be empty): %+v", e.TestsHash, e)
			}
		}
	}
}

func TestUpdate_OverwritesPriorEntryAtSameKey(t *testing.T) {
	root := t.TempDir()
	prodPath := filepath.Join(root, "x.go")
	mustWrite(t, prodPath, "package x\n")
	mustWrite(t, filepath.Join(root, "x_test.go"), "package x\n")
	prod, _ := HashFile(prodPath)

	c := &Cache{
		SchemaVersion: SchemaVersion, GoModule: testModule, ToolVersion: testVersion,
		Entries: []Entry{{
			RelFile: "x.go", Line: 1, Col: 1, Type: "ARITHMETIC_BASE",
			Original: "+", Replacement: "-",
			ProdHash: prod, TestsHash: "old-tests", Status: "LIVED", DurationMs: 1,
		}},
	}
	mutants := []mutator.Mutant{{
		ID: 1, Type: mutator.ArithmeticBase,
		File: prodPath, RelFile: "x.go", Line: 1, Col: 1,
		Original: "+", Replacement: "-",
		Status: mutator.StatusKilled, Duration: 99 * time.Millisecond,
	}}
	c.Update(mutants, NewHasher(nil), root, pkgDirTestFilesFor)

	if len(c.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(c.Entries))
	}
	e := c.Entries[0]
	if e.Status != mutator.StatusKilled.String() || e.DurationMs != 99 {
		t.Errorf("entry not overwritten: %+v", e)
	}
	if e.TestsHash == "old-tests" {
		t.Errorf("tests_hash not refreshed: %+v", e)
	}
}

func TestUpdate_DeterministicOrder(t *testing.T) {
	root := t.TempDir()
	prodPath := filepath.Join(root, "x.go")
	mustWrite(t, prodPath, "package x\n")
	mustWrite(t, filepath.Join(root, "x_test.go"), "package x\n")

	c := &Cache{SchemaVersion: SchemaVersion, GoModule: testModule, ToolVersion: testVersion}
	mutants := []mutator.Mutant{
		mkMutantAt(prodPath, "x.go", 5, 1, mutator.StatusKilled),
		mkMutantAt(prodPath, "x.go", 1, 1, mutator.StatusKilled),
		mkMutantAt(prodPath, "x.go", 3, 1, mutator.StatusKilled),
	}
	c.Update(mutants, NewHasher(nil), root, pkgDirTestFilesFor)

	for i := 1; i < len(c.Entries); i++ {
		if c.Entries[i-1].Line >= c.Entries[i].Line {
			t.Errorf("entries not sorted by line: %+v", c.Entries)
		}
	}
}

func mustWrite(t *testing.T, p, body string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

// mustPkgHash returns the package-dependency hash of dir, failing the
// test on error. Callers must compute it *after* every write that shapes
// the directory: HashPkgFiles fingerprints the directory listing, so a
// file added later changes the result.
func mustPkgHash(t *testing.T, dir string) string {
	t.Helper()
	h, err := NewHasher(nil).HashPkgFiles(dir)
	if err != nil {
		t.Fatalf("HashPkgFiles(%s): %v", dir, err)
	}
	return h
}

// pkgDirTestFilesFor is the conservative resolver used by unit tests:
// every _test.go file in the mutant's package directory. Mirrors what
// the production resolver does when no per-test coverage map is
// available — exercises the same code path without requiring tests to
// stand up a TestIndex.
func pkgDirTestFilesFor(m mutator.Mutant) []string {
	dir := filepath.Dir(m.File)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if filepath.Ext(e.Name()) == ".go" && len(e.Name()) > len("_test.go") &&
			e.Name()[len(e.Name())-len("_test.go"):] == "_test.go" {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	return files
}

func mkMutant(prodPath string, line int, status mutator.MutantStatus) mutator.Mutant {
	return mkMutantAt(prodPath, filepath.Base(prodPath), line, 1, status)
}

func mkMutantAt(prodPath, relFile string, line, col int, status mutator.MutantStatus) mutator.Mutant {
	return mutator.Mutant{
		ID:          line, // unique per call
		Type:        mutator.ArithmeticBase,
		File:        prodPath,
		RelFile:     relFile,
		Line:        line,
		Col:         col,
		StartOffset: line * 10,
		Original:    "+",
		Replacement: "-",
		Status:      status,
		Duration:    1 * time.Millisecond,
	}
}

// setupCoverageProject lays out a minimal module suitable for
// HashCoverageInputs: one prod file, one test file, go.mod, go.sum.
// Returns the project dir and the package dir (here, the same).
func setupCoverageProject(t *testing.T) (projectDir string, pkgDir string) {
	t.Helper()
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module testmod\n\ngo 1.26\n")
	mustWrite(t, filepath.Join(dir, "go.sum"), "")
	mustWrite(t, filepath.Join(dir, "x.go"), "package testmod\nfunc Add(a, b int) int { return a + b }\n")
	mustWrite(t, filepath.Join(dir, "x_test.go"), "package testmod\nimport \"testing\"\nfunc TestAdd(t *testing.T) {}\n")
	return dir, dir
}

func TestHashCoverageInputs_StableAcrossCalls(t *testing.T) {
	dir, pkg := setupCoverageProject(t)
	// Two fresh hashers — the memo can't shortcut the result.
	h1, err := NewHasher(nil).HashCoverageInputs([]string{pkg}, dir, "", "", "", "toolchain-x", "env-x")
	if err != nil {
		t.Fatalf("hash1: %v", err)
	}
	h2, err := NewHasher(nil).HashCoverageInputs([]string{pkg}, dir, "", "", "", "toolchain-x", "env-x")
	if err != nil {
		t.Fatalf("hash2: %v", err)
	}
	if h1 != h2 {
		t.Fatalf("hash not stable across runs: %s vs %s", h1, h2)
	}
}

// TestHashCoverageInputs_DetectsEachInputChange is the safety net for the
// coverage-cache invalidation contract. Each row mutates one and only one
// input dimension and asserts the resulting hash differs from the baseline.
// Surviving STATEMENT_REMOVE on any Fprintf inside HashCoverageInputs would
// collapse one of these dimensions; the table forces every Fprintf to be
// observable.
func TestHashCoverageInputs_DetectsEachInputChange(t *testing.T) {
	// coverageInputs bundles the dimensions HashCoverageInputs fingerprints.
	// A struct rather than a positional parameter list: there are seven
	// same-typed strings, so a transposed pair would silently still compile
	// and the test would keep passing against the wrong dimension. Named
	// fields also keep the helper below within the parameter-count limit.
	type coverageInputs struct {
		pkgDir, projectDir, coverPkg, tags, testFlags, toolchain, env string
	}
	// hash runs HashCoverageInputs and fails the test on error. Extracted so
	// the baseline and each of the table cases below is a single call rather
	// than repeating the hash-and-check boilerplate (which the duplication
	// detector flags). Cases hash a single dir (pkgDir == projectDir); the
	// baseline hashes the subpackage dir against the project root.
	hash := func(t *testing.T, in coverageInputs) string {
		t.Helper()
		h, err := NewHasher(nil).HashCoverageInputs(
			[]string{in.pkgDir}, in.projectDir, in.coverPkg, in.tags, in.testFlags, in.toolchain, in.env)
		if err != nil {
			t.Fatalf("hash: %v", err)
		}
		return h
	}
	baseline := func(t *testing.T) (string, string) {
		dir, pkg := setupCoverageProject(t)
		return dir, hash(t, coverageInputs{
			pkgDir:     pkg,
			projectDir: dir,
			coverPkg:   "./...",
			toolchain:  "go1.26",
			env:        "GOEXPERIMENT=|",
		})
	}

	// Each row changes exactly one HashCoverageInputs input relative to the
	// baseline. write (optional) mutates the project on disk first; the
	// remaining fields override the baseline's coverPkg/tags/toolchain/env.
	// Empty string means "same as baseline" for that dimension. Data-driven
	// (rather than per-row closures) so the rows don't repeat boilerplate.
	const (
		baseCoverPkg  = "./..."
		baseToolchain = "go1.26"
		baseEnv       = "GOEXPERIMENT=|"
	)
	tests := []struct {
		name                                      string
		write                                     func(t *testing.T, dir string)
		coverPkg, tags, testFlags, toolchain, env string
	}{
		{name: "prod file content", write: func(t *testing.T, dir string) {
			mustWrite(t, filepath.Join(dir, "x.go"), "package testmod\nfunc Add(a, b int) int { return b + a }\n")
		}},
		{name: "test file content", write: func(t *testing.T, dir string) {
			mustWrite(t, filepath.Join(dir, "x_test.go"), "package testmod\nimport \"testing\"\nfunc TestAdd(t *testing.T) { _ = 1 }\n")
		}},
		{name: "go.mod bytes", write: func(t *testing.T, dir string) {
			mustWrite(t, filepath.Join(dir, "go.mod"), "module testmod\n\ngo 1.27\n")
		}},
		{name: "go.sum bytes", write: func(t *testing.T, dir string) {
			mustWrite(t, filepath.Join(dir, "go.sum"), "h1:fakefakefake=\n")
		}},
		{name: "coverPkg", coverPkg: "testmod/sub"},
		{name: "tags", tags: "integration"},
		// --test-flags reach the coverage run itself, so a profile taken
		// under -short must not be replayed for a full run (or vice versa).
		{name: "test flags", testFlags: "-short"},
		{name: "toolchain", toolchain: "go1.27"},
		{name: "env snapshot", env: "GOEXPERIMENT=loopvar|"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir, base := baseline(t)
			if tc.write != nil {
				tc.write(t, dir)
			}
			got := hash(t, coverageInputs{
				pkgDir:     dir,
				projectDir: dir,
				coverPkg:   cmp.Or(tc.coverPkg, baseCoverPkg),
				tags:       tc.tags,
				testFlags:  tc.testFlags,
				toolchain:  cmp.Or(tc.toolchain, baseToolchain),
				env:        cmp.Or(tc.env, baseEnv),
			})
			if got == base {
				t.Errorf("hash unchanged after mutating %s — STATEMENT_REMOVE on the corresponding Fprintf collapses this dimension", tc.name)
			}
		})
	}
}

// TestHashCoverageInputs_GoSumOptional ensures a module without go.sum
// (e.g. no external deps yet) still hashes successfully and stays
// distinguishable from one whose go.sum is empty-but-present. Both cases
// produce the empty-content sum hash, so they hash to the same value —
// the test asserts both succeed and match. The point is: no error.
func TestHashCoverageInputs_GoSumOptional(t *testing.T) {
	dir, pkg := setupCoverageProject(t)
	if err := os.Remove(filepath.Join(dir, "go.sum")); err != nil {
		t.Fatalf("rm go.sum: %v", err)
	}
	if _, err := NewHasher(nil).HashCoverageInputs([]string{pkg}, dir, "", "", "", "tc", "env"); err != nil {
		t.Errorf("hash with missing go.sum should succeed, got: %v", err)
	}
}

// TestHashCoverageInputs_MissingGoModFails locks the required-go.mod
// path: a project with no go.mod must surface an error (not a silent
// "empty content" hash that would let two unrelated projects collide).
func TestHashCoverageInputs_MissingGoModFails(t *testing.T) {
	dir, pkg := setupCoverageProject(t)
	if err := os.Remove(filepath.Join(dir, "go.mod")); err != nil {
		t.Fatalf("rm go.mod: %v", err)
	}
	_, err := NewHasher(nil).HashCoverageInputs([]string{pkg}, dir, "", "", "", "tc", "env")
	if err == nil {
		t.Fatal("expected error when go.mod is missing")
	}
	if !strings.Contains(err.Error(), "go.mod") {
		t.Errorf("error should mention go.mod, got: %v", err)
	}
}

// TestHashCoverageInputs_MissingPkgDirFails ensures a stale package dir
// is reported, not silently elided.
func TestHashCoverageInputs_MissingPkgDirFails(t *testing.T) {
	dir, _ := setupCoverageProject(t)
	bogus := filepath.Join(dir, "no-such-pkg")
	_, err := NewHasher(nil).HashCoverageInputs([]string{bogus}, dir, "", "", "", "tc", "env")
	if err == nil {
		t.Fatal("expected error for missing pkgDir")
	}
}

// TestHashCoverageInputs_UnreadableProdFile locks the error-return inside
// the file loop: a .go file that ReadDir surfaced but HashFile cannot
// read must propagate the error, not be silently skipped (which would
// produce a hash that collides with the same project minus that file).
func TestHashCoverageInputs_UnreadableProdFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file-mode permissions; chmod 000 does not block reads")
	}
	dir, pkg := setupCoverageProject(t)
	bad := filepath.Join(pkg, "x.go")
	if err := os.Chmod(bad, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	// Restore so t.TempDir() cleanup can remove the file.
	defer func() { _ = os.Chmod(bad, 0o644) }()

	_, err := NewHasher(nil).HashCoverageInputs([]string{pkg}, dir, "", "", "", "tc", "env")
	if err == nil {
		t.Fatal("expected error from unreadable .go file")
	}
}

// TestHashCoverageInputs_IgnoresSubdirs locks down the `if e.IsDir()
// continue` branch. The subdir is given a `.go` suffix so the trailing
// suffix check would NOT skip it on its own — only the IsDir check
// keeps us from treating the directory itself as a Go file and trying
// to read its bytes. Without the IsDir guard, h.File on the directory
// errors and HashCoverageInputs propagates that error.
func TestHashCoverageInputs_IgnoresSubdirs(t *testing.T) {
	dir, pkg := setupCoverageProject(t)
	base, err := NewHasher(nil).HashCoverageInputs([]string{pkg}, dir, "", "", "", "tc", "env")
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	// `.go`-suffixed directory: defeats the suffix filter, exercises the
	// IsDir branch specifically.
	if err := os.Mkdir(filepath.Join(pkg, "vendor_tools.go"), 0o755); err != nil {
		t.Fatal(err)
	}
	after, err := NewHasher(nil).HashCoverageInputs([]string{pkg}, dir, "", "", "", "tc", "env")
	if err != nil {
		t.Fatalf("after: %v — IsDir branch removed, hasher tried to read a directory as a file", err)
	}
	if base != after {
		t.Errorf("subdir name leaked into hash: base=%s after=%s", base, after)
	}
}

// TestHashCoverageInputs_SortStableAcrossPkgDirOrder kills STATEMENT_REMOVE
// on slices.Sort(files): two pkgDir orderings must produce the same hash.
// Without the sort, files would be in walk-order ([pkg1 files..., pkg2
// files...]) and reversing pkgDirs would reverse that order, changing the
// hash.
func TestHashCoverageInputs_SortStableAcrossPkgDirOrder(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "go.mod"), "module testmod\n\ngo 1.26\n")
	mustWrite(t, filepath.Join(root, "go.sum"), "")
	pkgA := filepath.Join(root, "a")
	pkgB := filepath.Join(root, "b")
	if err := os.MkdirAll(pkgA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(pkgB, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(pkgA, "alpha.go"), "package a\n")
	mustWrite(t, filepath.Join(pkgB, "beta.go"), "package b\n")

	hAB, err := NewHasher(nil).HashCoverageInputs([]string{pkgA, pkgB}, root, "", "", "", "tc", "env")
	if err != nil {
		t.Fatalf("AB: %v", err)
	}
	hBA, err := NewHasher(nil).HashCoverageInputs([]string{pkgB, pkgA}, root, "", "", "", "tc", "env")
	if err != nil {
		t.Fatalf("BA: %v", err)
	}
	if hAB != hBA {
		t.Errorf("hash depends on pkgDir order — STATEMENT_REMOVE on slices.Sort(files) survived: AB=%s BA=%s", hAB, hBA)
	}
}

// TestHashCoverageInputs_DeduplicatesPkgDirs locks the pkgDir dedupe
// loop, including its `continue`-on-dup branch.
//
// We test three shapes:
//   - one pkgDir
//   - the same pkgDir twice (dup at position 1, no further entries)
//   - the same pkgDir twice followed by a NEW pkgDir (dup at position 1,
//     real entry at position 2). This last shape distinguishes `continue`
//     from `break`: with `continue` the new pkgDir is appended; with
//     `break` the loop exits at the dup and the new pkgDir is dropped.
func TestHashCoverageInputs_DeduplicatesPkgDirs(t *testing.T) {
	dir, pkg := setupCoverageProject(t)
	other := filepath.Join(dir, "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(other, "z.go"), "package other\n")

	once, err := NewHasher(nil).HashCoverageInputs([]string{pkg}, dir, "", "", "", "tc", "env")
	if err != nil {
		t.Fatalf("once: %v", err)
	}
	twice, err := NewHasher(nil).HashCoverageInputs([]string{pkg, pkg}, dir, "", "", "", "tc", "env")
	if err != nil {
		t.Fatalf("twice: %v", err)
	}
	if once != twice {
		t.Errorf("duplicate pkgDirs not deduped: once=%s twice=%s", once, twice)
	}

	// Reference: pkg + other, no duplicates.
	canonical, err := NewHasher(nil).HashCoverageInputs([]string{pkg, other}, dir, "", "", "", "tc", "env")
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	// Same set, but with a duplicate pkg entry mid-list. With `continue`
	// the loop keeps going and appends `other`. With `break` (mutant) the
	// loop exits at the dup, `other` is dropped, and the hash differs.
	dupThenNew, err := NewHasher(nil).HashCoverageInputs([]string{pkg, pkg, other}, dir, "", "", "", "tc", "env")
	if err != nil {
		t.Fatalf("dupThenNew: %v", err)
	}
	if canonical != dupThenNew {
		t.Errorf("dup followed by new pkgDir not handled — INVERT_LOOP_CTRL on the dedupe `continue` survived: canonical=%s dupThenNew=%s", canonical, dupThenNew)
	}
}

// TestHashCoverageInputs_GoSumReadErrorSurfaced locks the
// `else if !os.IsNotExist(err)` branch: a go.sum that exists but can't
// be read (here, replaced with a directory of the same name) must
// propagate as an error — not be silently swallowed like a missing file.
func TestHashCoverageInputs_GoSumReadErrorSurfaced(t *testing.T) {
	dir, pkg := setupCoverageProject(t)
	if err := os.Remove(filepath.Join(dir, "go.sum")); err != nil {
		t.Fatalf("rm go.sum: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "go.sum"), 0o755); err != nil {
		t.Fatalf("mkdir go.sum: %v", err)
	}
	_, err := NewHasher(nil).HashCoverageInputs([]string{pkg}, dir, "", "", "", "tc", "env")
	if err == nil {
		t.Fatal("expected error when go.sum is unreadable (is-a-directory)")
	}
	if !strings.Contains(err.Error(), "go.sum") {
		t.Errorf("error should mention go.sum, got: %v", err)
	}
}

// TestHashCoverageInputs_IgnoresNonGoFiles confirms only .go files are
// folded into the hash. A README change in the package dir must not
// invalidate the coverage profile (it can't affect what `go test
// -coverprofile` produces).
func TestHashCoverageInputs_IgnoresNonGoFiles(t *testing.T) {
	dir, pkg := setupCoverageProject(t)
	base, err := NewHasher(nil).HashCoverageInputs([]string{pkg}, dir, "", "", "", "tc", "env")
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	mustWrite(t, filepath.Join(pkg, "README.md"), "hello")
	mustWrite(t, filepath.Join(pkg, "data.json"), `{"x": 1}`)
	after, err := NewHasher(nil).HashCoverageInputs([]string{pkg}, dir, "", "", "", "tc", "env")
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if base != after {
		t.Errorf("non-.go files leaked into hash: base=%s after=%s", base, after)
	}
}

// TestParseStatus_Equivalent covers the EQUIVALENT round-trip through the
// on-disk status string, killing the BRANCH_CASE on parseStatus.
func TestParseStatus_Equivalent(t *testing.T) {
	if got := parseStatus(mutator.StatusEquivalent.String()); got != mutator.StatusEquivalent {
		t.Errorf("parseStatus(%q) = %v, want StatusEquivalent", mutator.StatusEquivalent.String(), got)
	}
}

func TestInfraErrorCacheBehavior(t *testing.T) {
	if canReuse(mutator.StatusInfraError) {
		t.Error("INFRA ERROR must not be reusable")
	}
	if needsTestsHash(mutator.StatusInfraError) {
		t.Error("INFRA ERROR must not require a tests hash because it is not cacheable")
	}
	if got := parseStatus(mutator.StatusInfraError.String()); got != mutator.StatusPending {
		t.Errorf("parseStatus(%q)=%v, want Pending", mutator.StatusInfraError, got)
	}
}
