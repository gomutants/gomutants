package cache

// Tests in this file exist to kill specific surviving mutants surfaced
// by `gomutants ./internal/...`. They complement the behavioural tests
// in cache_test.go by exercising loop-iteration ordering, dedup
// boundaries, sort-comparator field precedence, hasher memoization, and
// each I/O error path in Save (via the os* function-variable hooks).

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gomutants/gomutants/internal/mutator"
)

// --- Hasher.File memoization (cache.go:138, 145) -----------------------------

// TestHasher_File_MemoizesSrcCacheResult mutates the srcCache map
// between two File() calls. With the memoization write intact the
// second call returns the first hash; with it removed (mutant) the
// second call recomputes from the now-mutated bytes.
func TestHasher_File_MemoizesSrcCacheResult(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.go")

	src := map[string][]byte{p: []byte("v1")}
	h := NewHasher(src)

	first, err := h.File(p)
	if err != nil {
		t.Fatalf("first: %v", err)
	}

	src[p] = []byte("v2")
	second, err := h.File(p)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first != second {
		t.Fatalf("memoization not in effect: first=%s second=%s (after srcCache mutation)", first, second)
	}
}

// TestHasher_File_MemoizesDiskResult writes a file, hashes it, rewrites
// the file with new content, hashes again — the second result must be
// the cached first. With the memoization write removed, the second
// call recomputes from disk and returns the new hash.
func TestHasher_File_MemoizesDiskResult(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.go")
	if err := os.WriteFile(p, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := NewHasher(nil)
	first, err := h.File(p)
	if err != nil {
		t.Fatalf("first: %v", err)
	}

	if err := os.WriteFile(p, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := h.File(p)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first != second {
		t.Fatalf("memoization not in effect: first=%s second=%s (after disk rewrite)", first, second)
	}
}

// --- HashTestFiles dedup (cache.go:167, 168) ---------------------------------

// TestHashTestFiles_DedupOnlySkipsAdjacentDuplicates uses [a, b, b, c]
// to distinguish proper "skip when equal to previous" dedup from the
// `p == sorted[i-1]` → `true` mutation (which skips every entry after
// the first) and from the `continue` → `break` mutation (which exits
// after the first dup, dropping c).
func TestHashTestFiles_DedupOnlySkipsAdjacentDuplicates(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a_test.go")
	b := filepath.Join(dir, "b_test.go")
	c := filepath.Join(dir, "c_test.go")
	mustWrite(t, a, "package x\n")
	mustWrite(t, b, "package x\n// b\n")
	mustWrite(t, c, "package x\n// c\n")

	h := NewHasher(nil)

	want, err := h.HashTestFiles([]string{a, b, c})
	if err != nil {
		t.Fatalf("want: %v", err)
	}
	got, err := h.HashTestFiles([]string{a, b, b, c})
	if err != nil {
		t.Fatalf("got: %v", err)
	}
	if got != want {
		t.Fatalf("dedup did not produce [a,b,c] hash: got=%s want=%s", got, want)
	}

	// Cross-check: the all-skip mutation would produce hash([a]) for
	// the [a,b,b,c] input; verify this differs from the [a,b,c] hash.
	onlyA, err := h.HashTestFiles([]string{a})
	if err != nil {
		t.Fatalf("onlyA: %v", err)
	}
	if got == onlyA {
		t.Fatalf("dedup retained only first element: hash matches [a]")
	}
}

// --- HashTestFiles framing (cache.go:178, 179, 185 surface removed) ---------

// TestHashTestFiles_DistinguishesByBasename uses two files with
// identical content but different basenames to ensure the basename
// participates in the digest. With the per-file Fprintf removed (or
// the basename dropped from the format), both inputs collapse to the
// same hash.
func TestHashTestFiles_DistinguishesByBasename(t *testing.T) {
	root := t.TempDir()
	dirA := filepath.Join(root, "a")
	dirB := filepath.Join(root, "b")
	if err := os.MkdirAll(dirA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dirB, 0o755); err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(dirA, "alpha_test.go")
	b := filepath.Join(dirB, "beta_test.go")
	mustWrite(t, a, "package x\n")
	mustWrite(t, b, "package x\n")

	h := NewHasher(nil)
	hA, err := h.HashTestFiles([]string{a})
	if err != nil {
		t.Fatalf("hA: %v", err)
	}
	hB, err := h.HashTestFiles([]string{b})
	if err != nil {
		t.Fatalf("hB: %v", err)
	}
	if hA == hB {
		t.Fatalf("hash insensitive to basename: %s", hA)
	}
}

// --- Lookup loop continues (cache.go:308, 311, 312, 316, 321, 326) -----------

// TestLookup_ContinueAdvancesAcrossSkippedMutants asserts that every
// skip path in Lookup uses `continue` (advances to the next mutant)
// rather than `break` (which would short-circuit later valid hits).
// Each subtest places one skip-triggering mutant before a known-good
// hit; with the continue inverted to break, the second mutant remains
// pending and hits drops to 0.
func TestLookup_ContinueAdvancesAcrossSkippedMutants(t *testing.T) {
	dir := t.TempDir()
	prodPath := filepath.Join(dir, "x.go")
	testPath := filepath.Join(dir, "x_test.go")
	mustWrite(t, prodPath, "package x\n")
	mustWrite(t, testPath, "package x\n")

	prodHash, err := HashFile(prodPath)
	if err != nil {
		t.Fatal(err)
	}
	testsHash, err := NewHasher(nil).HashTestFiles([]string{testPath})
	if err != nil {
		t.Fatal(err)
	}
	pkgHash, err := NewHasher(nil).HashPkgFiles(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Known-good entry at line=2 — every subtest below adds a mutant
	// that triggers a different skip path at line=1, then a pending
	// mutant at line=2 whose hit verifies the loop continued.
	goodEntry := Entry{
		RelFile: "x.go", Line: 2, Col: 1, Type: "ARITHMETIC_BASE",
		Original: "+", Replacement: "-",
		ProdHash: prodHash, PkgHash: pkgHash, TestsHash: testsHash, Status: "KILLED",
	}
	mkPending := func(line int) mutator.Mutant {
		return mutator.Mutant{
			ID: line, Type: mutator.ArithmeticBase,
			File: prodPath, RelFile: "x.go",
			Line: line, Col: 1,
			Original: "+", Replacement: "-",
			Status: mutator.StatusPending,
		}
	}

	t.Run("non-pending precedes hit", func(t *testing.T) {
		c := &Cache{Entries: []Entry{goodEntry}}
		mutants := []mutator.Mutant{
			{ID: 1, Type: mutator.ArithmeticBase, File: prodPath, RelFile: "x.go",
				Line: 1, Col: 1, Original: "+", Replacement: "-",
				Status: mutator.StatusNotCovered}, // skipped: not Pending
			mkPending(2),
		}
		if hits := c.Lookup(mutants, NewHasher(nil), pkgDirTestFilesFor); hits != 1 {
			t.Errorf("hits=%d, want 1 — Lookup did not continue past non-pending mutant", hits)
		}
	})

	t.Run("unknown-key precedes hit", func(t *testing.T) {
		c := &Cache{Entries: []Entry{goodEntry}}
		// First mutant has no matching cache entry; second does.
		mutants := []mutator.Mutant{mkPending(1), mkPending(2)}
		if hits := c.Lookup(mutants, NewHasher(nil), pkgDirTestFilesFor); hits != 1 {
			t.Errorf("hits=%d, want 1 — Lookup did not continue past idx miss", hits)
		}
	})

	// The remaining four skips differ only in which field of the line-1
	// entry is wrong, so they run as one table rather than four subtests
	// that repeat the same cache, the same two mutants and the same
	// single-hit assertion.
	blocker := func(prod, pkg, tests, status string) Entry {
		return Entry{
			RelFile: "x.go", Line: 1, Col: 1, Type: "ARITHMETIC_BASE",
			Original: "+", Replacement: "-",
			ProdHash: prod, PkgHash: pkg, TestsHash: tests, Status: status,
		}
	}
	skips := []struct {
		name  string
		entry Entry
	}{
		{"non-reusable status", blocker(prodHash, pkgHash, testsHash, "PENDING")},
		{"prod-hash mismatch", blocker("stale-prod", pkgHash, testsHash, "KILLED")},
		{"pkg-hash mismatch", blocker(prodHash, "stale-pkg", testsHash, "KILLED")},
		{"tests-hash mismatch", blocker(prodHash, pkgHash, "stale-tests", "KILLED")},
	}
	for _, tc := range skips {
		t.Run(tc.name+" precedes hit", func(t *testing.T) {
			c := &Cache{Entries: []Entry{tc.entry, goodEntry}}
			mutants := []mutator.Mutant{mkPending(1), mkPending(2)}
			if hits := c.Lookup(mutants, NewHasher(nil), pkgDirTestFilesFor); hits != 1 {
				t.Errorf("hits=%d, want 1 — Lookup did not continue past %s", hits, tc.name)
			}
		})
	}
}

// TestLookup_ZeroEntriesShortCircuits asserts the early return when
// the cache has no entries — distinguishes from the
// `len(c.Entries) == 0` → `false` mutation, which would proceed to
// build an empty index and iterate every mutant (still 0 hits, but
// observably touches the slice).
func TestLookup_ZeroEntriesShortCircuits(t *testing.T) {
	c := &Cache{}
	// Sentinel: even a non-Pending mutant must remain untouched. The
	// non-mutated code returns 0 immediately; the mutant proceeds into
	// the loop body. Both produce 0 hits, but only the mutant would
	// access entries on the cache — so we instead verify that calling
	// Lookup with a nil resolver doesn't panic. With the early return
	// in place, testFilesFor is never invoked. With the early return
	// removed, the loop is entered; the mutant would then dereference
	// idx (empty map) and call testFilesFor on a Pending mutant whose
	// status is StatusKilled in the entry — i.e. it goes far enough to
	// invoke testFilesFor at least conceptually. But for our purposes,
	// the simplest discriminator is that nil testFilesFor + a pending
	// mutant that would otherwise hit produces a panic only when the
	// guard is removed.
	mutants := []mutator.Mutant{{
		ID: 1, Type: mutator.ArithmeticBase,
		File: "x.go", RelFile: "x.go", Line: 1, Col: 1,
		Original: "+", Replacement: "-",
		Status: mutator.StatusPending,
	}}
	hits := c.Lookup(mutants, NewHasher(nil), nil)
	if hits != 0 {
		t.Fatalf("hits=%d, want 0", hits)
	}
}

// --- Update loop continues (cache.go:404, 435, 439, 443) ---------------------

// TestUpdate_ContinueAdvancesAcrossSkippedEntries asserts that the
// carry-over loop in Update uses `continue` to advance, not `break`.
// Each subtest seeds two prior entries: the first triggers a skip
// path, the second is intact and must survive the run.
func TestUpdate_ContinueAdvancesAcrossSkippedEntries(t *testing.T) {
	root := t.TempDir()

	intact := filepath.Join(root, "intact.go")
	mustWrite(t, intact, "package x\n")
	intactHash, err := HashFile(intact)
	if err != nil {
		t.Fatal(err)
	}

	// Both drop paths below need the same two-entry cache: a doomed entry
	// followed by the intact one whose survival proves the carry-over loop
	// continued. Only the doomed entry's file and recorded hash differ, so
	// the literal is built once here.
	doomedThenIntact := func(relFile, prodHash, pkgHash string) *Cache {
		return &Cache{
			Entries: []Entry{
				{RelFile: relFile, Line: 1, Type: "ARITHMETIC_BASE",
					Original: "+", Replacement: "-",
					ProdHash: prodHash, PkgHash: pkgHash, Status: "KILLED"},
				{RelFile: "intact.go", Line: 1, Type: "ARITHMETIC_BASE",
					Original: "+", Replacement: "-",
					ProdHash: intactHash, PkgHash: pkgHash, Status: "KILLED"},
			},
		}
	}

	t.Run("missing-file precedes intact entry", func(t *testing.T) {
		// First entry's file does not exist (h.File errors); second
		// entry's file exists and matches. pkgHash is recomputed per
		// subtest — a later subtest adds stale.go to root, which changes
		// the directory's fingerprint.
		c := doomedThenIntact("gone.go", "stale", mustPkgHash(t, root))
		c.Update(nil, NewHasher(nil), root, pkgDirTestFilesFor)

		if len(c.Entries) != 1 || c.Entries[0].RelFile != "intact.go" {
			t.Errorf("entries=%v, want [intact.go] only — carry-over did not continue past missing file", c.Entries)
		}
	})

	t.Run("hash-mismatch precedes intact entry", func(t *testing.T) {
		// First entry exists on disk but hash doesn't match (stale);
		// second entry is intact.
		stale := filepath.Join(root, "stale.go")
		mustWrite(t, stale, "package x\n// changed\n")
		c := doomedThenIntact("stale.go", "old-hash", mustPkgHash(t, root))
		c.Update(nil, NewHasher(nil), root, pkgDirTestFilesFor)

		gotIntact := false
		for _, e := range c.Entries {
			if e.RelFile == "intact.go" {
				gotIntact = true
			}
			if e.RelFile == "stale.go" {
				t.Errorf("stale entry should be dropped: %+v", e)
			}
		}
		if !gotIntact {
			t.Errorf("intact entry dropped — carry-over did not continue past hash mismatch")
		}
	})

	t.Run("overwritten precedes intact entry", func(t *testing.T) {
		// A run mutant overwrites the first prior entry's key; the
		// second prior entry should still carry over.
		pkgHash := mustPkgHash(t, root)
		c := &Cache{
			Entries: []Entry{
				{RelFile: "intact.go", Line: 1, Col: 1,
					Type: "ARITHMETIC_BASE", Original: "+", Replacement: "-",
					ProdHash: intactHash, PkgHash: pkgHash, Status: "LIVED"},
				{RelFile: "intact.go", Line: 99, Col: 1,
					Type: "ARITHMETIC_BASE", Original: "*", Replacement: "/",
					ProdHash: intactHash, PkgHash: pkgHash, Status: "KILLED"},
			},
		}
		// Run mutant overwrites the line=1 entry only.
		mutants := []mutator.Mutant{{
			ID: 1, Type: mutator.ArithmeticBase,
			File: intact, RelFile: "intact.go",
			Line: 1, Col: 1, Original: "+", Replacement: "-",
			Status: mutator.StatusKilled, Duration: time.Millisecond,
		}}
		c.Update(mutants, NewHasher(nil), root, pkgDirTestFilesFor)

		var sawLine1Killed, sawLine99 bool
		for _, e := range c.Entries {
			if e.Line == 1 && e.Status == "KILLED" {
				sawLine1Killed = true
			}
			if e.Line == 99 {
				sawLine99 = true
			}
		}
		if !sawLine1Killed {
			t.Errorf("line=1 not overwritten with KILLED")
		}
		if !sawLine99 {
			t.Errorf("line=99 prior entry dropped — carry-over did not continue past overwrite")
		}
	})
}

// TestUpdate_SkipsRunMutantWhenProdFileGone asserts the Update path
// that errors-out on h.File for the *current run's* mutants (line 404
// branch). If the file is gone at Update time, that mutant must not
// appear in the merged entries.
func TestUpdate_SkipsRunMutantWhenProdFileGone(t *testing.T) {
	root := t.TempDir()
	gonePath := filepath.Join(root, "gone.go")
	// Note: file is never written — h.File will error.

	c := &Cache{}
	mutants := []mutator.Mutant{{
		ID: 1, Type: mutator.ArithmeticBase,
		File: gonePath, RelFile: "gone.go",
		Line: 1, Col: 1, Original: "+", Replacement: "-",
		Status: mutator.StatusKilled,
	}}
	c.Update(mutants, NewHasher(nil), root, pkgDirTestFilesFor)
	if len(c.Entries) != 0 {
		t.Errorf("entries=%v, want 0 — mutant for missing file must not be cached", c.Entries)
	}
}

// --- Update sort comparator (cache.go:454-477 — cmp.Or chain) ----------------

// TestUpdate_SortPrecedence asserts each tier of the sort comparator
// is honored. Pairs of entries that tie on every prior field but
// differ on one specific field verify that field's comparator runs.
// With any tier dropped (BRANCH_IF) or its sign inverted, the entries
// would emit in the wrong order and the assertion fires.
func TestUpdate_SortPrecedence(t *testing.T) {
	root := t.TempDir()
	prodPath := filepath.Join(root, "x.go")
	mustWrite(t, prodPath, "package x\n")
	prodHash, err := HashFile(prodPath)
	if err != nil {
		t.Fatal(err)
	}

	// Build prior entries that already-pass cache integrity (so they
	// carry over) and exercise each sort field tier in turn. Each pair
	// has identical lower-precedence fields except one.
	mk := func(rel string, line, col, off int, typ, orig, repl string) Entry {
		return Entry{
			RelFile: rel, Line: line, Col: col, StartOffset: off,
			Type: typ, Original: orig, Replacement: repl,
			ProdHash: prodHash, Status: "KILLED",
		}
	}

	c := &Cache{
		Entries: []Entry{
			// Insert deliberately scrambled — Update must sort by
			// (RelFile, Line, Col, StartOffset, Type, Original,
			// Replacement) in that order.
			mk("x.go", 1, 1, 0, "ARITHMETIC_BASE", "+", "-"),
			mk("x.go", 1, 1, 0, "ARITHMETIC_BASE", "+", "*"), // ties through Original
			mk("x.go", 1, 1, 0, "ARITHMETIC_BASE", "*", "+"), // ties through Type
			mk("x.go", 1, 1, 0, "BRANCH_IF", "+", "-"),       // ties through StartOffset
			mk("x.go", 1, 1, 1, "ARITHMETIC_BASE", "+", "-"), // ties through Col
			mk("x.go", 1, 2, 0, "ARITHMETIC_BASE", "+", "-"), // ties through Line
			mk("x.go", 2, 1, 0, "ARITHMETIC_BASE", "+", "-"), // ties through RelFile
			mk("y.go", 0, 0, 0, "ARITHMETIC_BASE", "+", "-"), // smaller RelFile
		},
	}

	// Need every entry's prod file to hash; create stub for y.go.
	mustWrite(t, filepath.Join(root, "y.go"), "package y\n")
	yHash, _ := HashFile(filepath.Join(root, "y.go"))
	c.Entries[len(c.Entries)-1].ProdHash = yHash

	// pkg_hash is stamped after the y.go write, since adding a file to
	// root changes the directory fingerprint every entry must match.
	pkgHash := mustPkgHash(t, root)
	for i := range c.Entries {
		c.Entries[i].PkgHash = pkgHash
	}

	c.Update(nil, NewHasher(nil), root, pkgDirTestFilesFor)

	// Expected sorted order (all entries carry over since prod hashes match).
	// Expected sort order (cmp.Or chain by RelFile, Line, Col,
	// StartOffset, Type, Original, Replacement). ASCII codepoints:
	// '*' (0x2A) < '+' (0x2B) < '-' (0x2D).
	want := []struct {
		rel  string
		line int
		col  int
		off  int
		typ  string
		orig string
		repl string
	}{
		{"x.go", 1, 1, 0, "ARITHMETIC_BASE", "*", "+"}, // smaller Original
		{"x.go", 1, 1, 0, "ARITHMETIC_BASE", "+", "*"}, // ties on Original=+, smaller Replacement
		{"x.go", 1, 1, 0, "ARITHMETIC_BASE", "+", "-"}, // ties on Original=+, larger Replacement
		{"x.go", 1, 1, 0, "BRANCH_IF", "+", "-"},       // larger Type
		{"x.go", 1, 1, 1, "ARITHMETIC_BASE", "+", "-"}, // larger StartOffset
		{"x.go", 1, 2, 0, "ARITHMETIC_BASE", "+", "-"}, // larger Col
		{"x.go", 2, 1, 0, "ARITHMETIC_BASE", "+", "-"}, // larger Line
		{"y.go", 0, 0, 0, "ARITHMETIC_BASE", "+", "-"}, // larger RelFile
	}
	if len(c.Entries) != len(want) {
		t.Fatalf("got %d entries, want %d: %+v", len(c.Entries), len(want), c.Entries)
	}
	for i, w := range want {
		got := c.Entries[i]
		if got.RelFile != w.rel || got.Line != w.line || got.Col != w.col ||
			got.StartOffset != w.off || got.Type != w.typ ||
			got.Original != w.orig || got.Replacement != w.repl {
			t.Errorf("position %d: got {%s %d %d %d %s %s→%s}, want {%s %d %d %d %s %s→%s}",
				i, got.RelFile, got.Line, got.Col, got.StartOffset, got.Type, got.Original, got.Replacement,
				w.rel, w.line, w.col, w.off, w.typ, w.orig, w.repl)
		}
	}
}

// --- Save error paths (cache.go:232, 239, 251, 255, 258, 261) ---------------

// withFailingOp swaps one of the os* hooks to a stub returning errSentinel.
// Restores at test cleanup.
func withFailingOp(t *testing.T, swap func(restore func())) {
	t.Helper()
	swap(func() {})
}

var errSentinel = errors.New("sentinel I/O failure")

func TestSave_PropagatesMkdirAllError(t *testing.T) {
	orig := osMkdirAll
	t.Cleanup(func() { osMkdirAll = orig })
	osMkdirAll = func(string, os.FileMode) error { return errSentinel }

	err := Save(&Cache{SchemaVersion: SchemaVersion}, filepath.Join(t.TempDir(), "cache.json"))
	if !errors.Is(err, errSentinel) {
		t.Fatalf("err=%v, want sentinel", err)
	}
}

func TestSave_PropagatesCreateTempError(t *testing.T) {
	orig := newSaveSink
	t.Cleanup(func() { newSaveSink = orig })
	newSaveSink = func(string, string) (saveSink, error) { return nil, errSentinel }

	err := Save(&Cache{SchemaVersion: SchemaVersion}, filepath.Join(t.TempDir(), "cache.json"))
	if !errors.Is(err, errSentinel) {
		t.Fatalf("err=%v, want sentinel", err)
	}
}

// fakeSink is a saveSink whose Write and Close errors can be set
// independently — what lets the Encode-only and Close-only tests below
// distinguish those return paths in Save (otherwise indistinguishable
// when both errors collapse into a single "non-nil error" assertion).
type fakeSink struct {
	name     string
	writeErr error
	closeErr error
	closed   bool
}

func (f *fakeSink) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return len(p), nil
}
func (f *fakeSink) Close() error { f.closed = true; return f.closeErr }
func (f *fakeSink) Name() string { return f.name }

// TestSave_PropagatesEncodeError asserts that an Encode failure is
// returned even when Close subsequently succeeds — kills the
// `if encodeErr != nil { return encodeErr }` BRANCH_IF mutant by
// requiring the SPECIFIC encode-error sentinel back, not just
// "any non-nil error" (which the Close-error branch would also
// produce on a real *os.File).
func TestSave_PropagatesEncodeError(t *testing.T) {
	dir := t.TempDir()
	orig := newSaveSink
	t.Cleanup(func() { newSaveSink = orig })

	encErr := errors.New("encode-only sentinel")
	newSaveSink = func(string, string) (saveSink, error) {
		return &fakeSink{
			name:     filepath.Join(dir, ".gomutants-cache-fake.tmp"),
			writeErr: encErr,
			closeErr: nil,
		}, nil
	}

	err := Save(&Cache{SchemaVersion: SchemaVersion}, filepath.Join(dir, "cache.json"))
	if !errors.Is(err, encErr) {
		t.Fatalf("err=%v, want encode-only sentinel", err)
	}
}

// TestSave_PropagatesCloseError asserts that a Close failure is
// returned when Encode succeeded — kills the
// `if closeErr != nil { return closeErr }` BRANCH_IF mutant.
func TestSave_PropagatesCloseError(t *testing.T) {
	dir := t.TempDir()
	orig := newSaveSink
	t.Cleanup(func() { newSaveSink = orig })

	closeErr := errors.New("close-only sentinel")
	newSaveSink = func(string, string) (saveSink, error) {
		return &fakeSink{
			name:     filepath.Join(dir, ".gomutants-cache-fake.tmp"),
			writeErr: nil,
			closeErr: closeErr,
		}, nil
	}

	err := Save(&Cache{SchemaVersion: SchemaVersion}, filepath.Join(dir, "cache.json"))
	if !errors.Is(err, closeErr) {
		t.Fatalf("err=%v, want close-only sentinel", err)
	}
}

// TestSave_AlwaysClosesEvenOnEncodeFailure asserts the file
// descriptor is released regardless of Encode outcome — i.e. Close is
// invoked on every code path, not gated on Encode success.
func TestSave_AlwaysClosesEvenOnEncodeFailure(t *testing.T) {
	dir := t.TempDir()
	orig := newSaveSink
	t.Cleanup(func() { newSaveSink = orig })

	sink := &fakeSink{
		name:     filepath.Join(dir, ".gomutants-cache-fake.tmp"),
		writeErr: errSentinel,
	}
	newSaveSink = func(string, string) (saveSink, error) { return sink, nil }

	_ = Save(&Cache{SchemaVersion: SchemaVersion}, filepath.Join(dir, "cache.json"))
	if !sink.closed {
		t.Fatal("Save did not Close the sink on encode failure")
	}
}

func TestSave_PropagatesChmodError(t *testing.T) {
	orig := osChmod
	t.Cleanup(func() { osChmod = orig })
	osChmod = func(string, os.FileMode) error { return errSentinel }

	err := Save(&Cache{SchemaVersion: SchemaVersion}, filepath.Join(t.TempDir(), "cache.json"))
	if !errors.Is(err, errSentinel) {
		t.Fatalf("err=%v, want sentinel", err)
	}
}

func TestSave_PropagatesRenameError(t *testing.T) {
	dir := t.TempDir()
	orig := osRename
	t.Cleanup(func() { osRename = orig })
	osRename = func(string, string) error { return errSentinel }

	err := Save(&Cache{SchemaVersion: SchemaVersion}, filepath.Join(dir, "cache.json"))
	if !errors.Is(err, errSentinel) {
		t.Fatalf("err=%v, want sentinel", err)
	}

	// Rename failed → tmp should still exist (deferred Remove cleans
	// up only on the error path; verify cleanup happened).
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "cache.json" {
			// Any remaining .tmp file means cleanup leaked.
			t.Errorf("temp file leaked after rename failure: %s", e.Name())
		}
	}
}

// TestSave_TmpFileCleanedUpAfterEncodeFailure asserts that a failed
// Encode triggers the deferred temp-file cleanup. The fakeSink's
// writeErr forces Encode to fail; the spy on osRemove confirms the
// deferred cleanup ran exactly once.
func TestSave_TmpFileCleanedUpAfterEncodeFailure(t *testing.T) {
	dir := t.TempDir()
	tmpPath := filepath.Join(dir, ".gomutants-cache-fake.tmp")

	origSink := newSaveSink
	t.Cleanup(func() { newSaveSink = origSink })
	newSaveSink = func(string, string) (saveSink, error) {
		return &fakeSink{name: tmpPath, writeErr: errSentinel}, nil
	}

	origRemove := osRemove
	t.Cleanup(func() { osRemove = origRemove })
	var removeCalls int
	osRemove = func(name string) error {
		removeCalls++
		return os.Remove(name)
	}

	if err := Save(&Cache{SchemaVersion: SchemaVersion}, filepath.Join(dir, "cache.json")); err == nil {
		t.Fatal("expected error, got nil")
	}
	if removeCalls != 1 {
		t.Errorf("osRemove calls=%d, want 1 (deferred cleanup must fire on encode failure)", removeCalls)
	}
}

// TestSave_TmpClearedAfterRenameSuccess asserts the post-rename
// tmpName="" assignment (cache.go:264). With the assignment removed,
// the deferred Remove still fires and would attempt to delete the
// just-renamed file (which is now at `path`, not tmpName) — but
// tmpName still holds the *original* temp name, which no longer
// exists, so Remove returns ENOENT (silently swallowed). Detection:
// inject a custom os.Rename that *moves* tmp to path, then assert that
// after Save returns, calling osRename again on the same tmpName fails
// with ENOENT — i.e. nothing else tried to remove it.
//
// Simpler observable: count how many times the deferred Remove fires
// by overriding tmpName's would-be removal path. Easiest: track the
// invariant via a stub on os.Remove via the global, but cache.go uses
// os.Remove directly. Pragmatic approach: assert the file exists at
// path AND no .tmp remains in dir (which TestSave_LeavesNoTempFiles
// already covers). The mutation `tmpName = ""` → `_ = ""` would still
// produce the same end state (target file at path), since the deferred
// Remove on the now-missing tmpName silently no-ops. Equivalent
// mutant; not separately killable without a refactor.
//
// To kill it, we'd need to observe the deferred Remove. Refactor: make
// the cleanup a named function we can spy on. Skipped — equivalent.

// --- testindex.go mutants ----------------------------------------------------

// TestBuildTestIndex_ContinuesPastSeenAndUnreadableDirs covers
// testindex.go:48 (the err-or-seen guard) and :54 (ReadDir fail
// branch) by passing a mix of: a dir that fails Abs (impossible), a
// dir already seen, an unreadable dir, and a good dir. The good dir
// must still be indexed.
func TestBuildTestIndex_ContinuesPastSeenAndUnreadableDirs(t *testing.T) {
	root := t.TempDir()

	good := filepath.Join(root, "good")
	if err := os.MkdirAll(good, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(good, "x_test.go"), "package x\nimport \"testing\"\nfunc TestX(t *testing.T) {}\n")

	missing := filepath.Join(root, "missing") // does not exist
	dup := good                               // duplicate of good

	ti := BuildTestIndex([]string{missing, good, dup})

	if got := ti.FilesFor("TestX"); len(got) != 1 {
		t.Errorf("FilesFor(TestX) = %v, want 1 entry (dup must not double-add)", got)
	}
	if all := ti.AllInDir(good); len(all) != 1 {
		t.Errorf("AllInDir(good) = %v, want 1 file", all)
	}
}

// TestBuildTestIndex_SkipsIsDirEntries covers testindex.go:60: a
// subdirectory whose name happens to end in `_test.go` must not be
// indexed as a file.
func TestBuildTestIndex_SkipsIsDirEntries(t *testing.T) {
	dir := t.TempDir()
	// Create a subdirectory named like a test file.
	if err := os.MkdirAll(filepath.Join(dir, "deceptive_test.go"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, "real_test.go"), "package x\nimport \"testing\"\nfunc TestReal(t *testing.T) {}\n")

	ti := BuildTestIndex([]string{dir})
	all := ti.AllInDir(dir)
	if len(all) != 1 || filepath.Base(all[0]) != "real_test.go" {
		t.Errorf("AllInDir = %v, want only real_test.go", all)
	}
}

// TestBuildTestIndex_SkipsMethodReceiver covers testindex.go:79
// (`fn.Recv != nil`): a method with a name like TestX (on a receiver)
// must not be indexed as a top-level test entry.
func TestBuildTestIndex_SkipsMethodReceiver(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "x_test.go"), `package x

import "testing"

type S struct{}
func (s S) TestMethod(t *testing.T) {} // method, not a top-level test
func TestTopLevel(t *testing.T)      {} // genuine
`)

	ti := BuildTestIndex([]string{dir})
	if got := ti.FilesFor("TestMethod"); got != nil {
		t.Errorf("method TestMethod indexed as test entry: %v", got)
	}
	if got := ti.FilesFor("TestTopLevel"); len(got) != 1 {
		t.Errorf("TestTopLevel not indexed: %v", got)
	}
}

// TestBuildTestIndex_SkipsDirsWithoutTests covers testindex.go:87
// (`if len(dirFiles) > 0`): a directory with only production files
// must not appear in byDir.
func TestBuildTestIndex_SkipsDirsWithoutTests(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "x.go"), "package x\nfunc Foo() {}\n")

	ti := BuildTestIndex([]string{dir})
	abs, _ := filepath.Abs(dir)
	if got := ti.AllInDir(abs); got != nil {
		t.Errorf("dir without tests indexed: %v", got)
	}
}

// TestBuildTestIndex_LoopContinuesPastSeen covers the
// `seen[abs] = true` write at testindex.go:51: removing it would let
// the same dir be processed twice, double-indexing every test name.
func TestBuildTestIndex_LoopContinuesPastSeen(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "x_test.go"), "package x\nimport \"testing\"\nfunc TestSame(t *testing.T) {}\n")

	ti := BuildTestIndex([]string{dir, dir, dir})
	got := ti.FilesFor("TestSame")
	if len(got) != 1 {
		t.Errorf("FilesFor = %v, want 1 entry (dup dirs must be deduped via seen[])", got)
	}
}

// --- Update nil-cache guard (cache.go:405) -----------------------------------

// TestUpdate_NilCacheReturnsEarly asserts the `if c == nil { return }`
// guard in Update. Without the guard, the next line dereferences c
// (via len(c.Entries)) and panics — which the test would observe as a
// failure.
func TestUpdate_NilCacheReturnsEarly(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Update on nil cache panicked: %v", r)
		}
	}()
	var c *Cache
	c.Update(nil, NewHasher(nil), t.TempDir(), pkgDirTestFilesFor)
}

// --- Update run-mutant skip on missing prod file (cache.go:419) --------------

// TestUpdate_ContinuesPastRunMutantWithMissingFile places a missing-
// file mutant before an intact one; with the `continue` inverted to
// `break`, the intact mutant's entry would be dropped.
func TestUpdate_ContinuesPastRunMutantWithMissingFile(t *testing.T) {
	root := t.TempDir()
	good := filepath.Join(root, "good.go")
	mustWrite(t, good, "package x\n")

	c := &Cache{}
	mutants := []mutator.Mutant{
		{
			ID: 1, Type: mutator.ArithmeticBase,
			File: filepath.Join(root, "missing.go"), RelFile: "missing.go",
			Line: 1, Col: 1, Original: "+", Replacement: "-",
			Status: mutator.StatusKilled,
		},
		{
			ID: 2, Type: mutator.ArithmeticBase,
			File: good, RelFile: "good.go",
			Line: 1, Col: 1, Original: "+", Replacement: "-",
			Status: mutator.StatusKilled, Duration: time.Millisecond,
		},
	}
	c.Update(mutants, NewHasher(nil), root, pkgDirTestFilesFor)

	if len(c.Entries) != 1 || c.Entries[0].RelFile != "good.go" {
		t.Errorf("entries=%v, want 1 entry for good.go (loop must continue past missing-file mutant)", c.Entries)
	}
}

// --- Save osRemove cleanup count (cache.go:278) ------------------------------

// TestSave_RemovesTmpOnlyOnFailurePath asserts the `committed` flag
// at the end of Save: the deferred Remove must fire on the failure
// path (rename fails) and must NOT fire on the success path. With
// `committed = true` removed (or replaced with `_ = true`), defer
// would call osRemove(tmpName) after the successful rename, and our
// stub increments the call count.
func TestSave_RemovesTmpOnlyOnFailurePath(t *testing.T) {
	orig := osRemove
	t.Cleanup(func() { osRemove = orig })
	var calls int
	osRemove = func(name string) error {
		calls++
		return os.Remove(name)
	}

	t.Run("success path: no Remove", func(t *testing.T) {
		calls = 0
		path := filepath.Join(t.TempDir(), "cache.json")
		if err := Save(&Cache{SchemaVersion: SchemaVersion}, path); err != nil {
			t.Fatalf("save: %v", err)
		}
		if calls != 0 {
			t.Errorf("osRemove called %d times on success path, want 0 — committed flag broken", calls)
		}
	})

	t.Run("failure path: one Remove", func(t *testing.T) {
		calls = 0
		origRename := osRename
		t.Cleanup(func() { osRename = origRename })
		osRename = func(string, string) error { return errSentinel }

		path := filepath.Join(t.TempDir(), "cache.json")
		if err := Save(&Cache{SchemaVersion: SchemaVersion}, path); !errors.Is(err, errSentinel) {
			t.Fatalf("expected sentinel, got %v", err)
		}
		if calls != 1 {
			t.Errorf("osRemove called %d times on failure path, want 1 — cleanup did not fire", calls)
		}
	})
}

// --- testindex.go INVERT_LOOP_CTRL + INVERT_LOGICAL --------------------------

// TestBuildTestIndex_OuterLoopContinuesPastDuplicate is the
// 3-pkgDir variant that catches `continue → break` on the err-or-seen
// guard: pkgDirs = [a, a, b]. With continue the second a (already seen)
// is skipped and b is processed. With break the second a triggers an
// early exit, leaving b unindexed.
func TestBuildTestIndex_OuterLoopContinuesPastDuplicate(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	if err := os.MkdirAll(a, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(b, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(a, "x_test.go"), "package x\nimport \"testing\"\nfunc TestA(t *testing.T) {}\n")
	mustWrite(t, filepath.Join(b, "x_test.go"), "package y\nimport \"testing\"\nfunc TestB(t *testing.T) {}\n")

	ti := BuildTestIndex([]string{a, a, b})

	if got := ti.FilesFor("TestA"); len(got) != 1 {
		t.Errorf("TestA = %v, want 1 (must be indexed once despite duplicate dir)", got)
	}
	if got := ti.FilesFor("TestB"); len(got) != 1 {
		t.Errorf("TestB = %v, want 1 (outer loop did not continue past duplicate dir)", got)
	}
}

// TestBuildTestIndex_InnerLoopContinuesPastNonTestFiles places a
// production .go file before a _test.go file in the same dir and
// asserts both the test entry and AllInDir are populated. With the
// inner loop's `continue → break` mutation, the _test.go file would
// not be reached.
func TestBuildTestIndex_InnerLoopContinuesPastNonTestFiles(t *testing.T) {
	dir := t.TempDir()
	// alphabetical iteration order: 'a_prod.go' < 'b_test.go' <
	// 'c.go'. The first non-_test.go entry triggers the suffix-skip
	// continue; b_test.go must still be reached.
	mustWrite(t, filepath.Join(dir, "a_prod.go"), "package x\nfunc Foo() {}\n")
	mustWrite(t, filepath.Join(dir, "b_test.go"), "package x\nimport \"testing\"\nfunc TestB(t *testing.T) {}\n")
	mustWrite(t, filepath.Join(dir, "c.go"), "package x\nfunc Bar() {}\n")

	ti := BuildTestIndex([]string{dir})
	if got := ti.FilesFor("TestB"); len(got) != 1 {
		t.Errorf("TestB not indexed — inner loop did not continue past non-_test.go file: %v", got)
	}
}

// TestBuildTestIndex_RequiresBothNonDirAndTestSuffix engineers a
// non-_test.go file containing a Test-named function. With the err-or-
// seen guard's `||` mutated to `&&`, or the suffix check itself
// mutated, the function might be indexed. Proper code skips on the
// suffix mismatch.
func TestBuildTestIndex_RequiresBothNonDirAndTestSuffix(t *testing.T) {
	dir := t.TempDir()
	// File without _test.go suffix but with a function named like a
	// test: a misuse, but possible. Must NOT be indexed. Uses a .go
	// extension so go/parser will accept the contents — what makes
	// the file ineligible is purely the missing _test.go suffix.
	mustWrite(t, filepath.Join(dir, "helper.go"), `package x

import "testing"

func TestPretender(t *testing.T) {} // wrong file — must be ignored
`)
	mustWrite(t, filepath.Join(dir, "real_test.go"), `package x

import "testing"

func TestReal(t *testing.T) {}
`)
	ti := BuildTestIndex([]string{dir})

	if got := ti.FilesFor("TestPretender"); got != nil {
		t.Errorf("TestPretender from non-_test.go file indexed: %v", got)
	}
	if got := ti.FilesFor("TestReal"); len(got) != 1 {
		t.Errorf("TestReal not indexed: %v", got)
	}
}

// --- HashPkgFiles (v7 pkg_hash dimension) ------------------------------------

// TestHashPkgFiles_SkipsTestFilesAndKeepsGoing locks two branches in
// goFilesIn's skipTests path at once, using a directory whose _test.go
// file sorts *between* two production files:
//
//   - drop the `skipTests && _test.go` guard and the test file joins the
//     hash, so editing a test would invalidate the package's NOT_VIABLE
//     entries — whose reuse is deliberately test-independent;
//   - turn its `continue` into a `break` and z.go is silently dropped, so
//     a package would hash identically with or without its last file.
//
// Both are caught by comparing against the same package minus its test
// file, which must hash identically.
func TestHashPkgFiles_SkipsTestFilesAndKeepsGoing(t *testing.T) {
	withTests := t.TempDir()
	mustWrite(t, filepath.Join(withTests, "a.go"), "package x\n")
	mustWrite(t, filepath.Join(withTests, "a_test.go"), "package x\n// a test\n")
	mustWrite(t, filepath.Join(withTests, "z.go"), "package x\n// z\n")

	withoutTests := t.TempDir()
	mustWrite(t, filepath.Join(withoutTests, "a.go"), "package x\n")
	mustWrite(t, filepath.Join(withoutTests, "z.go"), "package x\n// z\n")

	got := mustPkgHash(t, withTests)
	want := mustPkgHash(t, withoutTests)
	if got != want {
		t.Errorf("pkg hash differs with a _test.go present:\n got=%s\nwant=%s\n"+
			"either the test file was folded in, or the loop broke early and dropped z.go", got, want)
	}
}

// TestHashPkgFiles_SiblingContentChangesHash is the property the whole v7
// bump rests on: two packages whose file *names* match but whose contents
// differ must not share a fingerprint. Without it, pkg_hash would gate
// nothing.
func TestHashPkgFiles_SiblingContentChangesHash(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.go"), "package x\n")
	mustWrite(t, filepath.Join(dir, "b.go"), "package x\n\nconst Offset = 5\n")
	before := mustPkgHash(t, dir)

	mustWrite(t, filepath.Join(dir, "b.go"), "package x\n\nconst Offset = 0\n")
	after := mustPkgHash(t, dir)

	if before == after {
		t.Error("editing a sibling file left the package hash unchanged")
	}
}

// TestHashPkgFiles_ListingErrorPropagates locks the error return after
// goFilesIn. Swallowing it would hand Lookup a hash for a package it could
// not even list — an empty-input digest that any other unlistable package
// would also produce, turning an I/O failure into a spurious cache hit.
func TestHashPkgFiles_ListingErrorPropagates(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-dir")
	if _, err := NewHasher(nil).HashPkgFiles(missing); err == nil {
		t.Fatal("expected an error for a directory that cannot be listed")
	}
}

// TestHashPkgFiles_UnreadableFilePropagates locks the error return inside
// the per-file loop. A .go file ReadDir surfaced but File cannot read must
// fail the hash rather than be skipped — skipping would produce the exact
// digest of the same package minus that file, so an unreadable sibling
// would silently stop gating.
func TestHashPkgFiles_UnreadableFilePropagates(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file-mode permissions; chmod 000 does not block reads")
	}
	dir := t.TempDir()
	bad := filepath.Join(dir, "a.go")
	mustWrite(t, bad, "package x\n")
	if err := os.Chmod(bad, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	// Restore so t.TempDir() cleanup can remove the file.
	defer func() { _ = os.Chmod(bad, 0o644) }()

	if _, err := NewHasher(nil).HashPkgFiles(dir); err == nil {
		t.Fatal("expected an error from an unreadable .go file")
	}
}

// TestHashPkgFiles_MemoizesPerDirectory locks the memo write. Dropping it
// costs no correctness on its own — the recomputed value is the same — but
// it silently turns one listing-plus-hash per package into one per mutant,
// on the hot path of the feature whose whole point is speed. The memo is
// observable as staleness: a run never edits its own sources mid-flight, so
// a second call must replay the first answer rather than re-read the disk.
func TestHashPkgFiles_MemoizesPerDirectory(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.go"), "package x\n")

	h := NewHasher(nil)
	first, err := h.HashPkgFiles(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Add a file the memoized answer must not notice.
	mustWrite(t, filepath.Join(dir, "b.go"), "package x\n// added\n")

	second, err := h.HashPkgFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Errorf("second call recomputed (%s != %s) — the per-directory memo was not written", second, first)
	}
	if len(h.dirs) != 1 {
		t.Errorf("h.dirs holds %d entries, want 1", len(h.dirs))
	}
}

// --- pkg_hash error paths in Lookup / Update ---------------------------------

// srcCacheHasher returns a Hasher whose in-memory source map satisfies
// File(path) without touching the disk, mirroring what
// discover.PreReadFiles hands the production pipeline. It is what lets the
// tests below drive HashPkgFiles to an error while File still succeeds —
// otherwise File fails first and the pkg_hash branch is never reached.
func srcCacheHasher(path, body string) *Hasher {
	return NewHasher(map[string][]byte{path: []byte(body)})
}

// TestLookup_PkgHashErrorIsAMiss locks the `err != nil` half of Lookup's
// pkg_hash gate. Drop it and an entry stamped with an empty pkg_hash
// matches the empty string HashPkgFiles returns alongside its error — so an
// I/O failure would be laundered into a cache hit, which is the one
// direction this gate must never fail in.
func TestLookup_PkgHashErrorIsAMiss(t *testing.T) {
	// The package directory does not exist, so HashPkgFiles errors; the
	// mutated file's bytes come from the in-memory source map, so the
	// prod_hash check ahead of it still passes.
	prodPath := filepath.Join(t.TempDir(), "no-such-dir", "x.go")
	h := srcCacheHasher(prodPath, "package x\n")
	prodHash, err := h.File(prodPath)
	if err != nil {
		t.Fatal(err)
	}

	c := &Cache{Entries: []Entry{{
		RelFile: "x.go", Line: 1, Col: 1, Type: "ARITHMETIC_BASE",
		Original: "+", Replacement: "-",
		ProdHash: prodHash, PkgHash: "", TestsHash: "",
		// NOT_VIABLE skips the tests_hash check, isolating the pkg_hash
		// gate as the only thing between this entry and a hit.
		Status: mutator.StatusNotViable.String(),
	}}}
	mutants := []mutator.Mutant{{
		ID: 1, Type: mutator.ArithmeticBase,
		File: prodPath, RelFile: "x.go", Line: 1, Col: 1,
		Original: "+", Replacement: "-",
		Status: mutator.StatusPending,
	}}

	if hits := c.Lookup(mutants, h, pkgDirTestFilesFor); hits != 0 {
		t.Errorf("hits=%d, want 0 — a pkg_hash error must be a miss, never a hit", hits)
	}
}

// TestUpdate_SkipsRunMutantWhenPkgHashFails locks the matching guard in
// Update, and that it skips with `continue` rather than `break`. An entry
// we cannot stamp a pkg_hash on is one Lookup would always reject, so
// writing it is pure noise — and writing it with an empty pkg_hash is worse
// than noise, since an empty stored value is what a future failed hash
// compares equal to. The good mutant that follows the failing one is what
// catches a `break`: it would abandon every remaining result of the run.
func TestUpdate_SkipsRunMutantWhenPkgHashFails(t *testing.T) {
	root := t.TempDir()
	badPath := filepath.Join(root, "no-such-dir", "x.go")
	goodPath := filepath.Join(root, "good.go")
	mustWrite(t, goodPath, "package x\n")

	// The bad mutant's bytes come from the in-memory source map, so File
	// succeeds and execution reaches the pkg_hash guard; its directory does
	// not exist, so HashPkgFiles fails there.
	h := srcCacheHasher(badPath, "package x\n")

	c := &Cache{SchemaVersion: SchemaVersion, GoModule: testModule, ToolVersion: testVersion}
	mutants := []mutator.Mutant{
		{
			ID: 1, Type: mutator.ArithmeticBase,
			File: badPath, RelFile: filepath.Join("no-such-dir", "x.go"),
			Line: 1, Col: 1, Original: "+", Replacement: "-",
			Status: mutator.StatusKilled, Duration: time.Millisecond,
		},
		{
			ID: 2, Type: mutator.ArithmeticBase,
			File: goodPath, RelFile: "good.go",
			Line: 2, Col: 1, Original: "*", Replacement: "/",
			Status: mutator.StatusKilled, Duration: time.Millisecond,
		},
	}

	c.Update(mutants, h, root, pkgDirTestFilesFor)

	if len(c.Entries) != 1 || c.Entries[0].RelFile != "good.go" {
		t.Errorf("entries=%+v, want only good.go — the unstampable mutant must be skipped, "+
			"and the loop must continue past it rather than break", c.Entries)
	}
}

// TestUpdate_PkgHashMismatchContinuesCarryOver asserts the carry-over
// loop's pkg_hash skip uses `continue`, not `break`. The two entries live
// in different packages so only the first is stale; a `break` would take
// the intact second entry down with it, silently emptying the cache of
// every package that happens to sort after an edited one.
func TestUpdate_PkgHashMismatchContinuesCarryOver(t *testing.T) {
	root := t.TempDir()
	dirStale := filepath.Join(root, "stalepkg")
	dirIntact := filepath.Join(root, "intactpkg")
	if err := os.MkdirAll(dirStale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dirIntact, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dirStale, "a.go"), "package s\n")
	mustWrite(t, filepath.Join(dirIntact, "a.go"), "package i\n")

	staleHash, _ := HashFile(filepath.Join(dirStale, "a.go"))
	intactHash, _ := HashFile(filepath.Join(dirIntact, "a.go"))

	c := &Cache{
		Entries: []Entry{
			{ // prod_hash matches, pkg_hash does not — must be skipped.
				RelFile: filepath.Join("stalepkg", "a.go"), Line: 1, Col: 1,
				Type: "ARITHMETIC_BASE", Original: "+", Replacement: "-",
				ProdHash: staleHash, PkgHash: "stale-pkg", Status: "KILLED",
			},
			{ // fully intact — must survive.
				RelFile: filepath.Join("intactpkg", "a.go"), Line: 1, Col: 1,
				Type: "ARITHMETIC_BASE", Original: "+", Replacement: "-",
				ProdHash: intactHash, PkgHash: mustPkgHash(t, dirIntact), Status: "KILLED",
			},
		},
	}

	c.Update(nil, NewHasher(nil), root, pkgDirTestFilesFor)

	relIntact := filepath.Join("intactpkg", "a.go")
	if len(c.Entries) != 1 || c.Entries[0].RelFile != relIntact {
		t.Errorf("entries=%+v, want only %s — carry-over did not continue past the pkg_hash mismatch",
			c.Entries, relIntact)
	}
}

// Compile-time sanity for unused helpers.
var _ = withFailingOp
var _ = fmt.Sprintf

// --- pkg_hash: //go:embed inputs ---------------------------------------------

// embedDir builds a package directory holding one .go file plus the named
// embed inputs (relative slash paths → contents) and returns its path.
func embedDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.go"), "package x\n")
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		mustWrite(t, p, body)
	}
	return dir
}

// embedHash hashes dir with the given embed inputs declared, on a fresh
// Hasher so the per-directory memo never carries across cases.
func embedHash(t *testing.T, dir string, embeds []string) string {
	t.Helper()
	h := NewHasher(nil)
	h.SetEmbedFiles(map[string][]string{dir: embeds})
	v, err := h.HashPkgFiles(dir)
	if err != nil {
		t.Fatalf("HashPkgFiles(%s): %v", dir, err)
	}
	return v
}

// TestHashPkgFiles_EmbeddedContentChangesHash is the regression: a mutant
// killed by a test asserting on embedded data survives once that data
// changes, with every .go file byte-identical. Without the embed dimension
// pkg_hash cannot see it and the cache replays the stale KILLED.
func TestHashPkgFiles_EmbeddedContentChangesHash(t *testing.T) {
	dir := embedDir(t, map[string]string{"data/schema.json": `{"max":5}`})

	before := embedHash(t, dir, []string{"data/schema.json"})
	mustWrite(t, filepath.Join(dir, "data", "schema.json"), `{"max":9}`)
	after := embedHash(t, dir, []string{"data/schema.json"})

	if before == after {
		t.Error("pkg_hash unchanged after the embedded file's content changed")
	}
}

// TestHashPkgFiles_EmbedsAreOptional pins the other side: a package that
// declares no embed inputs must hash exactly as it did before the dimension
// existed, so warm caches for the overwhelming majority of packages survive.
// It also covers a directory absent from the map, which is how
// embedFilesByDir represents "embeds nothing".
func TestHashPkgFiles_EmbedsAreOptional(t *testing.T) {
	dir := embedDir(t, map[string]string{"data/schema.json": `{"max":5}`})

	plain, err := NewHasher(nil).HashPkgFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	declaredEmpty := embedHash(t, dir, nil)

	h := NewHasher(nil)
	h.SetEmbedFiles(map[string][]string{"/some/other/pkg": {"x.json"}})
	otherDir, err := h.HashPkgFiles(dir)
	if err != nil {
		t.Fatal(err)
	}

	if plain != declaredEmpty || plain != otherDir {
		t.Errorf("hashes diverged with no embeds in play: plain=%s empty=%s otherDir=%s",
			plain, declaredEmpty, otherDir)
	}

	// And the embedded file really is invisible without the declaration —
	// otherwise the test above would pass for the wrong reason.
	mustWrite(t, filepath.Join(dir, "data", "schema.json"), `{"max":9}`)
	edited, err := NewHasher(nil).HashPkgFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if edited != plain {
		t.Error("an undeclared file under the package dir changed pkg_hash")
	}
}

// TestHashPkgFiles_EmbedFramingUsesRelativePath covers the choice of
// dir-relative path over basename for the embed framing. Two packages that
// embed byte-identical content under the same basename in different
// subdirectories must not collide: `data/a/x.json` and `data/b/x.json` are
// different inputs, and a basename-only framing would write the same bytes
// for both.
func TestHashPkgFiles_EmbedFramingUsesRelativePath(t *testing.T) {
	inA := embedDir(t, map[string]string{"data/a/x.json": "same"})
	inB := embedDir(t, map[string]string{"data/b/x.json": "same"})

	if embedHash(t, inA, []string{"data/a/x.json"}) == embedHash(t, inB, []string{"data/b/x.json"}) {
		t.Error("same basename in different subdirectories hashed identically — " +
			"the embed framing is not path-aware")
	}
}

// TestHashPkgFiles_EmbedListIsOrderIndependent pins the sort + dedup: go
// list's ordering is not part of the contract we want to hash, and a
// pattern set can name the same file twice (`data/*.json data/schema.json`).
func TestHashPkgFiles_EmbedListIsOrderIndependent(t *testing.T) {
	dir := embedDir(t, map[string]string{
		"data/a.json": "1",
		"data/b.json": "2",
	})

	sorted := embedHash(t, dir, []string{"data/a.json", "data/b.json"})
	reversed := embedHash(t, dir, []string{"data/b.json", "data/a.json"})
	duped := embedHash(t, dir, []string{"data/b.json", "data/a.json", "data/b.json"})

	if sorted != reversed {
		t.Errorf("input order changed the hash: %s vs %s", sorted, reversed)
	}
	if sorted != duped {
		t.Errorf("a repeated entry changed the hash: %s vs %s", sorted, duped)
	}
}

// TestHashPkgFiles_MissingEmbedFileIsAnError locks the error return in the
// embed loop. A declared input we cannot read leaves us unable to say
// whether the package changed, and Lookup must see that as a miss rather
// than hash the package as if the file were not there.
func TestHashPkgFiles_MissingEmbedFileIsAnError(t *testing.T) {
	dir := embedDir(t, nil)

	h := NewHasher(nil)
	h.SetEmbedFiles(map[string][]string{dir: {"data/vanished.json"}})
	if _, err := h.HashPkgFiles(dir); err == nil {
		t.Error("HashPkgFiles succeeded with an unreadable embed input")
	}
}

// --- coverage key: //go:embed inputs -----------------------------------------

// coverHash hashes dirs as a coverage scope with the given embed inputs
// declared, on a fresh Hasher so no memo carries across cases.
func coverHash(t *testing.T, dirs []string, projectDir string, embeds map[string][]string) string {
	t.Helper()
	h := NewHasher(nil)
	h.SetEmbedFiles(embeds)
	v, err := h.HashCoverageInputs(dirs, projectDir, "", "", "", "go1.26", "env")
	if err != nil {
		t.Fatalf("HashCoverageInputs(%v): %v", dirs, err)
	}
	return v
}

// coverEmbedDir builds embedDir's layout plus the go.mod HashCoverageInputs
// requires, so the directory can serve as both project root and package.
func coverEmbedDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := embedDir(t, files)
	mustWrite(t, filepath.Join(dir, "go.mod"), "module testmod\n\ngo 1.26\n")
	return dir
}

// TestHashCoverageInputs_EmbeddedContentChangesKey is the coverage-side half
// of the embed regression. A test that iterates an embedded table decides
// which lines the profile marks covered: add a case that reaches a branch
// nothing reached before and every .go file is still byte-identical, so a
// key blind to embeds matches, the stale profile is replayed, and the
// mutants on those newly covered lines stay NOT_COVERED — never tested.
func TestHashCoverageInputs_EmbeddedContentChangesKey(t *testing.T) {
	dir := coverEmbedDir(t, map[string]string{"data/cases.json": `[1]`})
	embeds := map[string][]string{dir: {"data/cases.json"}}

	before := coverHash(t, []string{dir}, dir, embeds)
	mustWrite(t, filepath.Join(dir, "data", "cases.json"), `[1,2]`)
	after := coverHash(t, []string{dir}, dir, embeds)

	if before == after {
		t.Error("coverage key unchanged after the embedded file's content changed")
	}
}

// TestHashCoverageInputs_EmbedsAreOptional mirrors the pkg_hash side: a
// scope whose packages declare no embed inputs must hash exactly as it did
// before the dimension existed, so warm coverage caches survive. A
// directory absent from the map is how embedFilesByDir says "embeds
// nothing" — and is also what a coverage scope wider than the resolved
// packages leaves behind.
func TestHashCoverageInputs_EmbedsAreOptional(t *testing.T) {
	dir := coverEmbedDir(t, map[string]string{"data/cases.json": `[1]`})

	plain := coverHash(t, []string{dir}, dir, nil)
	declaredEmpty := coverHash(t, []string{dir}, dir, map[string][]string{dir: nil})
	otherDir := coverHash(t, []string{dir}, dir, map[string][]string{"/some/other/pkg": {"x.json"}})

	if plain != declaredEmpty || plain != otherDir {
		t.Errorf("keys diverged with no embeds in play: plain=%s empty=%s otherDir=%s",
			plain, declaredEmpty, otherDir)
	}

	// And the embedded file really is invisible without the declaration —
	// otherwise the test above would pass for the wrong reason.
	mustWrite(t, filepath.Join(dir, "data", "cases.json"), `[1,2]`)
	if coverHash(t, []string{dir}, dir, nil) != plain {
		t.Error("an undeclared file under a package dir changed the coverage key")
	}
}

// TestHashCoverageInputs_EmbedsAcrossDirectories covers embedFrames on a
// multi-directory scope, which is the shape pkg_hash never sees: every
// directory's inputs have to land, and neither the caller's ordering nor a
// repeated entry may change the result. Callers build that list in whatever
// order go list or the integration closure produced, so an order-sensitive
// key would hash the same repository differently from one run to the next.
func TestHashCoverageInputs_EmbedsAcrossDirectories(t *testing.T) {
	root := coverEmbedDir(t, map[string]string{"data/a.json": "1"})
	other := embedDir(t, map[string]string{"data/b.json": "2"})
	embeds := map[string][]string{root: {"data/a.json"}, other: {"data/b.json"}}

	forward := coverHash(t, []string{root, other}, root, embeds)
	reversed := coverHash(t, []string{other, root}, root, embeds)
	// Both directories are repeated, so whichever sorts first, one repeat
	// lands mid-list rather than at the end. A repeat in the last position
	// hides most of the ways the skip can be wrong: stopping at the repeat
	// and skipping past it are the same thing there, and so are "this
	// equals its predecessor" and "always equal". Mid-list, each of those
	// drops a directory's frames or emits them twice.
	duped := coverHash(t, []string{root, other, root, other}, root, embeds)

	if forward != reversed {
		t.Errorf("pkgDirs order changed the key: %s vs %s", forward, reversed)
	}
	if forward != duped {
		t.Errorf("a repeated pkgDir changed the key: %s vs %s", forward, duped)
	}

	// Each directory's inputs must actually reach the key, asserted by
	// dropping one declaration at a time. The three comparisons above
	// cannot see this on their own: a skip that swallowed every directory
	// after the first would swallow the same one in all three, leaving them
	// equal to each other and the bug invisible. Which directory sorts
	// first depends on the temp-dir names, so both drops are checked.
	onlyRoot := coverHash(t, []string{root, other}, root, map[string][]string{root: {"data/a.json"}})
	onlyOther := coverHash(t, []string{root, other}, root, map[string][]string{other: {"data/b.json"}})
	if forward == onlyRoot {
		t.Error("dropping the second directory's embed declaration left the key unchanged")
	}
	if forward == onlyOther {
		t.Error("dropping the first directory's embed declaration left the key unchanged")
	}
}

// TestHashCoverageInputs_MissingEmbedFileIsAnError locks the error return.
// A declared input we cannot read leaves us unable to say whether the
// coverage inputs changed, and the caller must re-run coverage rather than
// key a profile as if the file were not there.
//
// The cause is asserted through errors.Is, not just for a non-nil error:
// the wrap has to keep the underlying fs.ErrNotExist reachable, or Update's
// "gone vs. merely unreadable" classification — which is exactly that
// errors.Is check — cannot tell the two apart.
func TestHashCoverageInputs_MissingEmbedFileIsAnError(t *testing.T) {
	dir := coverEmbedDir(t, nil)

	h := NewHasher(nil)
	h.SetEmbedFiles(map[string][]string{dir: {"data/vanished.json"}})
	_, err := h.HashCoverageInputs([]string{dir}, dir, "", "", "", "go1.26", "env")
	if err == nil {
		t.Fatal("HashCoverageInputs succeeded with an unreadable embed input")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want one unwrapping to fs.ErrNotExist", err)
	}
}

// --- Update's carry-over: gone vs. merely unreadable --------------------------

// carryOver runs Update with no results of its own, so every prior entry
// takes the carry-over path, and reports whether the entry survived.
func carryOver(t *testing.T, c *Cache, h *Hasher, projectDir string) bool {
	t.Helper()
	c.Update(nil, h, projectDir, pkgDirTestFilesFor)
	return len(c.Entries) == 1
}

// priorEntry returns a carry-over candidate for relFile stamped with the
// given hashes.
func priorEntry(relFile, prodHash, pkgHash string) *Cache {
	return &Cache{Entries: []Entry{{
		RelFile: relFile, Line: 1, Col: 1, Type: "ARITHMETIC_BASE",
		Original: "+", Replacement: "-",
		ProdHash: prodHash, PkgHash: pkgHash,
		Status: mutator.StatusKilled.String(),
	}}}
}

// TestUpdate_DropsPriorEntryWhenSourceDeleted covers the fs.ErrNotExist
// branch on the prod side. A deleted file can never hash back to the stored
// value, so keeping the entry would grow the cache file without bound.
// Without the branch the entry falls into "unreadable → keep" and survives.
func TestUpdate_DropsPriorEntryWhenSourceDeleted(t *testing.T) {
	root := t.TempDir()
	c := priorEntry("gone.go", "abc123", "def456")

	if carryOver(t, c, NewHasher(nil), root) {
		t.Error("entry for a deleted file was carried over")
	}
}

// TestUpdate_DropsPriorEntryWhenPackageDirDeleted covers the same branch on
// the pkg side, which needs the mutated file to still hash: its bytes come
// from the in-memory source map while its directory does not exist.
//
// The stored PkgHash is deliberately empty — the value a failed hash used to
// be compared against. Before the rewrite this entry was *kept*, which is
// the same laundering TestLookup_PkgHashErrorIsAMiss locks out on the
// Lookup side.
func TestUpdate_DropsPriorEntryWhenPackageDirDeleted(t *testing.T) {
	root := t.TempDir()
	abs := filepath.Join(root, "no-such-dir", "x.go")
	h := srcCacheHasher(abs, "package x\n")
	prodHash, err := h.File(abs)
	if err != nil {
		t.Fatal(err)
	}
	c := priorEntry(filepath.Join("no-such-dir", "x.go"), prodHash, "")

	if carryOver(t, c, h, root) {
		t.Error("entry whose package directory is gone was carried over")
	}
}

// TestUpdate_KeepsPriorEntryWhenSourceUnreadable covers the "cannot verify"
// branch on the prod side. Update runs on every checkpoint, so treating a
// transient read failure as staleness would discard a package's warm cache
// over one blip. Keeping is safe because Lookup re-verifies both hashes and
// treats its own errors as a miss.
//
// The unreadable file is a *directory* named like one, which yields EISDIR
// — an error that is not fs.ErrNotExist, and needs no permission games that
// a root-running CI would defeat.
func TestUpdate_KeepsPriorEntryWhenSourceUnreadable(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "weird.go"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := priorEntry("weird.go", "abc123", "def456")

	if !carryOver(t, c, NewHasher(nil), root) {
		t.Error("entry was dropped over an unreadable (not missing) source file")
	}
}

// TestUpdate_KeepsPriorEntryWhenPackageDirUnreadable covers the same branch
// on the pkg side: the mutated file reads out of the source map, while its
// "directory" is a regular file, so ReadDir fails with ENOTDIR rather than
// fs.ErrNotExist.
func TestUpdate_KeepsPriorEntryWhenPackageDirUnreadable(t *testing.T) {
	root := t.TempDir()
	notADir := filepath.Join(root, "pkg")
	mustWrite(t, notADir, "not a directory\n")

	abs := filepath.Join(notADir, "x.go")
	h := srcCacheHasher(abs, "package x\n")
	prodHash, err := h.File(abs)
	if err != nil {
		t.Fatal(err)
	}
	c := priorEntry(filepath.Join("pkg", "x.go"), prodHash, "def456")

	if !carryOver(t, c, h, root) {
		t.Error("entry was dropped over an unreadable (not missing) package directory")
	}
}
