package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cachepkg "github.com/gomutants/gomutants/internal/cache"
	"github.com/gomutants/gomutants/internal/mutator"
	"github.com/gomutants/gomutants/internal/report"
)

// TestIncrementalCacheColdThenWarm runs the simple testdata twice with
// --cache and asserts the second run reuses every prior outcome.
func TestIncrementalCacheColdThenWarm(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	dir := setupTestdataCopy(t, "testdata/simple")

	cachePath := filepath.Join(dir, ".gomutants-cache.json")
	reportPath := filepath.Join(dir, "report.json")

	cold := runInDir(t, dir, []string{
		"-w", "4",
		"-cache", cachePath,
		"-o", reportPath,
		"./...",
	})

	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("cache file not created: %v", err)
	}

	// Sanity: cache contains the expected number of entries
	// (every cacheable terminal status — KILLED, LIVED, NOT_VIABLE,
	// TIMED_OUT — but not NOT_COVERED).
	cacheable := cold.MutantsKilled + cold.MutantsLived + cold.MutantsNotViable +
		(cold.MutantsTotal - cold.MutantsKilled - cold.MutantsLived - cold.MutantsNotCovered - cold.MutantsNotViable)
	loaded := loadCacheFile(t, cachePath)
	if len(loaded.Entries) != cacheable {
		t.Errorf("cache entries=%d, want %d (cacheable terminal statuses)", len(loaded.Entries), cacheable)
	}

	// Warm run: same args, same testdata. Every cacheable mutant should be reused.
	warm := runInDir(t, dir, []string{
		"-w", "4",
		"-cache", cachePath,
		"-o", reportPath,
		"./...",
	})

	if warm.MutantsTotal != cold.MutantsTotal {
		t.Errorf("warm total=%d, want %d", warm.MutantsTotal, cold.MutantsTotal)
	}
	if warm.MutantsKilled != cold.MutantsKilled {
		t.Errorf("warm killed=%d, want %d", warm.MutantsKilled, cold.MutantsKilled)
	}
	if warm.MutantsLived != cold.MutantsLived {
		t.Errorf("warm lived=%d, want %d", warm.MutantsLived, cold.MutantsLived)
	}
	if warm.MutantsCached != cacheable {
		t.Errorf("warm cached=%d, want %d (every cacheable mutant)", warm.MutantsCached, cacheable)
	}
}

// TestIncrementalCacheInvalidatesPerturbedProdFile rewrites a production
// file between runs and asserts the cache invalidates only that file's
// mutants while reusing the rest.
func TestIncrementalCacheInvalidatesPerturbedProdFile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	dir := setupTestdataCopy(t, "testdata/simple")
	cachePath := filepath.Join(dir, ".gomutants-cache.json")
	reportPath := filepath.Join(dir, "report.json")

	// Cold run.
	runInDir(t, dir, []string{"-w", "4", "-cache", cachePath, "-o", reportPath, "./..."})

	priorEntries := len(loadCacheFile(t, cachePath).Entries)

	// Append a no-op comment to math.go — every mutant in math.go must
	// invalidate (prod_hash mismatch). There are no other prod files,
	// so this should drop cached count to 0 on the warm run.
	mathPath := filepath.Join(dir, "math.go")
	body, err := os.ReadFile(mathPath)
	if err != nil {
		t.Fatalf("read math.go: %v", err)
	}
	body = append(body, []byte("\n// touched\n")...)
	if err := os.WriteFile(mathPath, body, 0o644); err != nil {
		t.Fatalf("write math.go: %v", err)
	}

	warm := runInDir(t, dir, []string{"-w", "4", "-cache", cachePath, "-o", reportPath, "./..."})

	if warm.MutantsCached != 0 {
		t.Errorf("warm cached=%d, want 0 (every mutant in math.go must invalidate after edit)", warm.MutantsCached)
	}

	// After the warm run, the cache should still hold roughly the same
	// number of entries (recomputed) — the new prod_hash overwrites the
	// stale ones.
	updatedEntries := len(loadCacheFile(t, cachePath).Entries)
	if updatedEntries == 0 {
		t.Error("cache empty after warm run — Update should have repopulated it")
	}
	// Sanity: not strictly equal because new line counts shift mutant
	// positions, but it should be in the same ballpark.
	if updatedEntries < priorEntries/2 {
		t.Errorf("cache shrank dramatically: prior=%d updated=%d", priorEntries, updatedEntries)
	}
}

// TestIncrementalCacheInvalidatesPerturbedTestFile touches a test file
// and asserts that mutants whose status depended on test content
// (KILLED, LIVED) are invalidated, while NOT_VIABLE / TIMED_OUT
// (which depend only on prod) remain cached.
func TestIncrementalCacheInvalidatesPerturbedTestFile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	dir := setupTestdataCopy(t, "testdata/simple")
	cachePath := filepath.Join(dir, ".gomutants-cache.json")
	reportPath := filepath.Join(dir, "report.json")

	cold := runInDir(t, dir, []string{"-w", "4", "-cache", cachePath, "-o", reportPath, "./..."})

	// Touch math_test.go — appending whitespace changes the file hash
	// without changing test semantics.
	testPath := filepath.Join(dir, "math_test.go")
	body, err := os.ReadFile(testPath)
	if err != nil {
		t.Fatalf("read math_test.go: %v", err)
	}
	body = append(body, []byte("\n")...)
	if err := os.WriteFile(testPath, body, 0o644); err != nil {
		t.Fatalf("write math_test.go: %v", err)
	}

	warm := runInDir(t, dir, []string{"-w", "4", "-cache", cachePath, "-o", reportPath, "./..."})

	// KILLED + LIVED entries must invalidate (their tests_hash changed).
	// NOT_VIABLE entries must stay cached (test content is irrelevant).
	// TIMED_OUT entries (if any) must also stay cached.
	killedAndLived := cold.MutantsKilled + cold.MutantsLived
	if warm.MutantsCached >= killedAndLived+cold.MutantsNotViable {
		// MutantsCached should be < total cacheable — at least the
		// killed+lived ones must drop.
		t.Errorf("warm cached=%d, expected < %d (killed+lived must invalidate)",
			warm.MutantsCached, killedAndLived+cold.MutantsNotViable)
	}
	if warm.MutantsCached < cold.MutantsNotViable {
		t.Errorf("warm cached=%d, want >= %d (NOT_VIABLE must stay cached)",
			warm.MutantsCached, cold.MutantsNotViable)
	}
}

// TestIncrementalCacheInvalidatesOnSiblingFileEdit is the cache-soundness
// probe. A mutant's verdict depends on every source file its package
// compiles against, not just the file it lives in — but the per-mutant key
// hashes only the mutated file plus the covering tests. This test edits a
// *sibling* production file in the same package, leaving the mutated file
// and every test file byte-identical, and asserts the verdict is recomputed
// rather than replayed.
//
// testdata/crossfile is built so the verdict genuinely flips:
//
//	a.go:      func Adjust(raw int) int { return raw + Offset }
//	b.go:      const Offset = 5
//	a_test.go: Adjust(0) must equal Offset
//
// With Offset=5 the ARITHMETIC_BASE mutant (+ → -) yields -5 ≠ 5 and is
// KILLED. Set Offset=0 and the unmutated code still passes (0+0 == 0), but
// the mutant now yields 0 == 0 and LIVES. A cache that replays the stale
// KILLED reports a killed mutant that was never executed and is in fact
// alive — silent false confidence, the dangerous direction to be wrong in.
//
// Equivalence detection stays off (the default): with Offset=0 the compiler
// folds `raw + 0` and `raw - 0` to identical code, so a TCE pass would
// classify this mutant EQUIVALENT rather than LIVED.
func TestIncrementalCacheInvalidatesOnSiblingFileEdit(t *testing.T) {
	staleVerdictProbe{
		fixture:    "testdata/crossfile",
		dependency: "b.go's sibling constant",
		// Edit ONLY b.go. a.go and a_test.go keep their exact bytes, so
		// both prod_hash and tests_hash still match the cached entry.
		edit: func(t *testing.T, dir string) {
			bPath := filepath.Join(dir, "b.go")
			body, err := os.ReadFile(bPath)
			if err != nil {
				t.Fatalf("read b.go: %v", err)
			}
			edited := strings.Replace(string(body), "const Offset = 5", "const Offset = 0", 1)
			if edited == string(body) {
				t.Fatal("b.go did not contain `const Offset = 5` — fixture drifted")
			}
			if err := os.WriteFile(bPath, []byte(edited), 0o644); err != nil {
				t.Fatalf("write b.go: %v", err)
			}
		},
	}.run(t)
}

// staleVerdictProbe drives one cache-invalidation regression: copy fixture,
// run gomutants cold, apply an edit that must change the probe mutant's
// verdict, run warm, and assert the cache did not replay the stale KILLED.
//
// Both tests that use it hinge on the same probe — the sole ARITHMETIC_BASE
// mutant in the fixture's a.go, KILLED while the dependency holds 5 and
// LIVED once it holds 0 — and differ only in which dependency the edit
// touches, so the run itself is written once here. dependency names that
// dependency for the failure messages.
type staleVerdictProbe struct {
	fixture    string
	dependency string
	edit       func(t *testing.T, dir string)
}

func (p staleVerdictProbe) run(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	dir := setupTestdataCopy(t, p.fixture)
	cachePath := filepath.Join(dir, ".gomutants-cache.json")
	reportPath := filepath.Join(dir, "report.json")
	args := []string{"-w", "4", "-cache", cachePath, "-o", reportPath, "./..."}

	cold := runInDir(t, dir, args)

	// Locate the probe mutant by StableID so the warm run can find the same
	// one. a.go is byte-identical across both runs, so the ID is stable.
	probe := findArithmeticMutation(t, cold, "a.go")
	if probe.Status != mutator.StatusKilled.String() {
		t.Fatalf("cold run: %s status=%s, want KILLED — fixture is not exercising %s",
			probe.ID, probe.Status, p.dependency)
	}

	p.edit(t, dir)

	warm := runInDir(t, dir, args)

	got := findMutationByID(t, warm, probe.ID)
	if got.Status == mutator.StatusKilled.String() {
		t.Errorf("warm run: %s replayed as KILLED after %s changed (cached=%d); "+
			"the mutant now survives and must be recomputed to LIVED",
			probe.ID, p.dependency, warm.MutantsCached)
	}
	if got.Status != mutator.StatusLived.String() {
		t.Errorf("warm run: %s status=%s, want LIVED", probe.ID, got.Status)
	}
}

// findArithmeticMutation returns the sole ARITHMETIC_BASE mutation reported
// for relFile, failing the test if there is not exactly one. The uniqueness
// check is deliberate: the caller uses this mutant as a fixed probe across
// two runs, so an ambiguous match means the fixture grew a second
// arithmetic operator and the test is no longer measuring what it claims.
func findArithmeticMutation(t *testing.T, r *report.Report, relFile string) report.MutationReport {
	t.Helper()
	var found []report.MutationReport
	for _, f := range r.Files {
		if filepath.Base(f.FileName) != relFile {
			continue
		}
		for _, m := range f.Mutations {
			if m.Type == string(mutator.ArithmeticBase) {
				found = append(found, m)
			}
		}
	}
	if len(found) != 1 {
		t.Fatalf("found %d ARITHMETIC_BASE mutations in %s, want exactly 1: %+v", len(found), relFile, found)
	}
	return found[0]
}

// findMutationByID looks up a mutation by its StableID across every file in
// the report.
func findMutationByID(t *testing.T, r *report.Report, id string) report.MutationReport {
	t.Helper()
	for _, f := range r.Files {
		for _, m := range f.Mutations {
			if m.ID == id {
				return m
			}
		}
	}
	t.Fatalf("mutation %s not found in report", id)
	return report.MutationReport{}
}

// TestIncrementalCacheResumesAfterMidRunKill simulates a hard kill during
// a long run: a mid-run checkpoint persists the outcomes completed so
// far, the process "dies" before the end-of-run final flush, and a fresh
// invocation resumes by reusing exactly those checkpointed outcomes. This
// is the durability guarantee periodic checkpointing exists to provide —
// without it, an interrupted run that never reached the final save would
// lose everything.
func TestIncrementalCacheResumesAfterMidRunKill(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	dir := setupTestdataCopy(t, "testdata/simple")
	cachePath := filepath.Join(dir, ".gomutants-cache.json")
	reportPath := filepath.Join(dir, "report.json")
	t.Chdir(dir)

	// -w 1 so mutants complete one at a time and the kill lands on a
	// small partial set; 1ns interval so every onResult checkpoints.
	args := []string{
		"-w", "1",
		"-cache", cachePath,
		"-o", reportPath,
		"-checkpoint-interval", "1ns",
		"./...",
	}

	// Interrupted run. cacheSaveFunc stands in for the kill switch: the
	// first checkpoint that actually carries an entry is written to disk,
	// then ctx is cancelled (the "kill"). Every later save — including the
	// end-of-run final flush — is suppressed, so the on-disk cache
	// reflects ONLY what the mid-run checkpoint persisted.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var killed bool
	origSave := cacheSaveFunc
	cacheSaveFunc = func(c *cachepkg.Cache, path string) error {
		if killed {
			return nil // post-kill saves never happen in a real hard kill
		}
		if len(c.Entries) == 0 {
			return origSave(c, path) // nothing persisted yet; keep going
		}
		err := origSave(c, path)
		killed = true
		cancel()
		return err
	}
	err := run(ctx, args)
	cacheSaveFunc = origSave
	if err != nil {
		t.Fatalf("interrupted run: %v", err)
	}

	partial := loadCacheFile(t, cachePath)
	if len(partial.Entries) == 0 {
		t.Fatal("interrupted run left no cache entries — mid-run checkpoint did not persist")
	}

	// Resume: a fresh, uninterrupted invocation must reuse exactly the
	// checkpointed outcomes — no more (the kill interrupted the run), no
	// fewer (every checkpointed outcome is still valid).
	warm := runInDir(t, dir, args)
	if warm.MutantsCached != len(partial.Entries) {
		t.Errorf("resumed run cached=%d, want %d (every checkpointed outcome)", warm.MutantsCached, len(partial.Entries))
	}
	cacheableTotal := warm.MutantsKilled + warm.MutantsLived + warm.MutantsNotViable + warm.MutantsTimedOut
	if warm.MutantsCached >= cacheableTotal {
		t.Errorf("resumed run cached=%d, want a strict subset of %d cacheable mutants — the kill should have interrupted before completion", warm.MutantsCached, cacheableTotal)
	}
}

// runInDir invokes run() with args, chdir'd into dir for the duration
// of the call. Returns the parsed JSON report.
func runInDir(t *testing.T, dir string, args []string) *report.Report {
	t.Helper()
	t.Chdir(dir)

	if err := run(context.Background(), args); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Find the -o argument to read the report.
	var outPath string
	for i, a := range args {
		if (a == "-o" || a == "-output") && i+1 < len(args) {
			outPath = args[i+1]
			break
		}
	}
	if outPath == "" {
		t.Fatalf("no -o flag in args: %v", args)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read report %s: %v", outPath, err)
	}
	var r report.Report
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatalf("parse report: %v", err)
	}
	return &r
}

// setupTestdataCopy copies srcRel's files into a tempdir (one level deep)
// alongside a synthesized go.mod so that the dir is a self-contained
// module rooted at the temp directory. Returns the absolute tempdir.
func setupTestdataCopy(t *testing.T, srcRel string) string {
	t.Helper()
	dst := t.TempDir()

	srcAbs, err := filepath.Abs(srcRel)
	if err != nil {
		t.Fatalf("abs %s: %v", srcRel, err)
	}
	entries, err := os.ReadDir(srcAbs)
	if err != nil {
		t.Fatalf("read testdata %s: %v", srcAbs, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		body, err := os.ReadFile(filepath.Join(srcAbs, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), body, 0o644); err != nil {
			t.Fatalf("write %s: %v", e.Name(), err)
		}
	}
	// Synthesize a minimal go.mod so the tempdir is its own module.
	gomod := "module example.com/simple\n\ngo 1.21\n"
	if err := os.WriteFile(filepath.Join(dst, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	return dst
}

func loadCacheFile(t *testing.T, path string) *cachepkg.Cache {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	var c cachepkg.Cache
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("parse cache: %v", err)
	}
	return &c
}

// TestIncrementalCacheInvalidatesOnEmbeddedFileEdit is the //go:embed
// counterpart of TestIncrementalCacheInvalidatesOnSiblingFileEdit, and
// covers the wiring the unit tests cannot: that `go list`'s EmbedFiles
// actually reaches the Hasher for the directories mutants live in.
//
// testdata/embedcache holds the calibration constant in offset.txt rather
// than a Go declaration:
//
//	offset.txt: 5
//	a.go:       func Adjust(raw int) int { return raw + Offset() }
//	a_test.go:  Adjust(0) must equal Offset()
//
// With 5 the ARITHMETIC_BASE mutant (+ → -) yields -5 ≠ 5 and is KILLED.
// Put 0 in offset.txt and the unmutated code still passes (0+0 == 0) while
// the mutant now yields 0 == 0 and LIVES — with every .go file in the
// package byte-identical, so prod_hash, the .go half of pkg_hash and
// tests_hash all still match. Only the embed dimension can see this.
func TestIncrementalCacheInvalidatesOnEmbeddedFileEdit(t *testing.T) {
	staleVerdictProbe{
		fixture:    "testdata/embedcache",
		dependency: "offset.txt's embedded constant",
		// Edit ONLY the embedded data file: every .go file in the package
		// keeps its exact bytes, so prod_hash, the .go half of pkg_hash and
		// tests_hash all still match.
		edit: func(t *testing.T, dir string) {
			offsetPath := filepath.Join(dir, "offset.txt")
			if err := os.WriteFile(offsetPath, []byte("0\n"), 0o644); err != nil {
				t.Fatalf("write offset.txt: %v", err)
			}
		},
	}.run(t)
}

// TestIncrementalCacheInvalidatesOnExcludedSuiteEdit: with --coverpkg a
// mutant's verdict rests on every suite whose tests link it, including one
// in a package --exclude-files emptied. That package is gone from the
// mutated packages but its tests still run, and its files must still be in
// the key. They weren't: the cache's test index covered only the mutated
// packages, so strengthening e2e's test replayed lib's mutant as LIVED.
func TestIncrementalCacheInvalidatesOnExcludedSuiteEdit(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	dir := t.TempDir()
	for name, content := range map[string]string{
		"go.mod":     "module excl\n\ngo 1.26\n",
		"lib/lib.go": "package lib\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n",
		"lib/lib_test.go": "package lib\n\nimport \"testing\"\n\n" +
			"func TestSmoke(t *testing.T) {}\n",
		"e2e/helpers.go": "package e2e\n\nimport \"excl/lib\"\n\n" +
			"func Sum(a, b int) int { return lib.Add(a, b) }\n",
		"e2e/e2e_test.go": "package e2e\n\nimport \"testing\"\n\n" +
			"func TestSum(t *testing.T) { _ = Sum(2, 3) }\n",
	} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	args := []string{
		"-w", "2",
		"--only", "ARITHMETIC_BASE",
		"--coverpkg", "./...",
		"--exclude-files", "^e2e/",
		"-cache", filepath.Join(dir, ".gomutants-cache.json"),
		"-o", filepath.Join(dir, "report.json"),
		"./...",
	}

	cold := runInDir(t, dir, args)
	probe := findArithmeticMutation(t, cold, "lib.go")
	if probe.Status != mutator.StatusLived.String() {
		t.Fatalf("cold run: %s status=%s, want LIVED — e2e's test asserts nothing", probe.ID, probe.Status)
	}

	strong := "package e2e\n\nimport \"testing\"\n\n" +
		"func TestSum(t *testing.T) {\n\tif Sum(2, 3) != 5 {\n\t\tt.Fatal(\"wrong\")\n\t}\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "e2e", "e2e_test.go"), []byte(strong), 0o644); err != nil {
		t.Fatal(err)
	}

	warm := runInDir(t, dir, args)
	if got := findMutationByID(t, warm, probe.ID); got.Status != mutator.StatusKilled.String() {
		t.Errorf("warm run: %s status=%s (cached=%d), want KILLED: e2e's strengthened test must invalidate it",
			probe.ID, got.Status, warm.MutantsCached)
	}
}
