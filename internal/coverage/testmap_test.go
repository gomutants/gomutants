package coverage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTestMapTestsFor(t *testing.T) {
	tm := &TestMap{
		index: map[string]map[testKey]bool{
			"file.go:10": {{pkg: "p", name: "TestA"}: true, {pkg: "p", name: "TestB"}: true},
			"file.go:20": {{pkg: "p", name: "TestC"}: true},
		},
	}

	tests := tm.TestsFor("file.go", 10)
	if len(tests) != 2 {
		t.Fatalf("TestsFor(file.go, 10) = %d tests, want 2", len(tests))
	}

	tests = tm.TestsFor("file.go", 20)
	if len(tests) != 1 || tests[0] != "TestC" {
		t.Errorf("TestsFor(file.go, 20) = %v, want [TestC]", tests)
	}

	// No mapping.
	tests = tm.TestsFor("file.go", 99)
	if tests != nil {
		t.Errorf("TestsFor(file.go, 99) = %v, want nil", tests)
	}

	// Nil TestMap.
	var nilTm *TestMap
	tests = nilTm.TestsFor("file.go", 10)
	if tests != nil {
		t.Errorf("nil TestMap.TestsFor = %v, want nil", tests)
	}
}

func TestTestMapTestRefsFor(t *testing.T) {
	tm := &TestMap{
		index: map[string]map[testKey]bool{
			"f.go:10": {{pkg: "m/a", name: "TestA"}: true, {pkg: "m/b", name: "TestB"}: true},
		},
	}

	refs := tm.TestRefsFor("f.go", 10)
	if len(refs) != 2 {
		t.Fatalf("TestRefsFor(f.go, 10) = %d refs, want 2: %v", len(refs), refs)
	}
	got := map[TestRef]bool{}
	for _, r := range refs {
		got[r] = true
	}
	if !got[TestRef{Pkg: "m/a", Name: "TestA"}] || !got[TestRef{Pkg: "m/b", Name: "TestB"}] {
		t.Errorf("TestRefsFor lost a (pkg,name) pair; got %v", refs)
	}

	// No mapping for the position.
	if refs := tm.TestRefsFor("f.go", 99); refs != nil {
		t.Errorf("TestRefsFor(f.go, 99) = %v, want nil", refs)
	}

	// Nil receiver tolerated.
	var nilTm *TestMap
	if refs := nilTm.TestRefsFor("f.go", 10); refs != nil {
		t.Errorf("nil TestMap.TestRefsFor = %v, want nil", refs)
	}
}

func TestTestMapSumDurationsForRefs(t *testing.T) {
	tm := &TestMap{
		durations: map[testKey]time.Duration{
			{pkg: "m/a", name: "TestA"}: 100 * time.Millisecond,
			{pkg: "m/b", name: "TestB"}: 200 * time.Millisecond,
		},
	}

	// Cross-package sum: both refs present → total and complete.
	got, complete := tm.SumDurationsForRefs([]TestRef{{Pkg: "m/a", Name: "TestA"}, {Pkg: "m/b", Name: "TestB"}})
	if got != 300*time.Millisecond || !complete {
		t.Errorf("SumDurationsForRefs([a/A, b/B]) = (%v, %v), want (300ms, true)", got, complete)
	}

	// A missing ref → (0, false) so the caller falls back, not a partial sum.
	got, complete = tm.SumDurationsForRefs([]TestRef{{Pkg: "m/a", Name: "TestA"}, {Pkg: "m/x", Name: "TestMissing"}})
	if got != 0 || complete {
		t.Errorf("SumDurationsForRefs(with missing) = (%v, %v), want (0, false)", got, complete)
	}

	// Same name, wrong package must not match (cross-package isolation).
	got, complete = tm.SumDurationsForRefs([]TestRef{{Pkg: "m/b", Name: "TestA"}})
	if got != 0 || complete {
		t.Errorf("SumDurationsForRefs([b/A]) = (%v, %v), want (0, false) — package must be part of the key", got, complete)
	}

	// Empty input → (0, false).
	got, complete = tm.SumDurationsForRefs(nil)
	if got != 0 || complete {
		t.Errorf("SumDurationsForRefs(nil) = (%v, %v), want (0, false)", got, complete)
	}

	// Nil receiver tolerated.
	var nilTm *TestMap
	got, complete = nilTm.SumDurationsForRefs([]TestRef{{Pkg: "m/a", Name: "TestA"}})
	if got != 0 || complete {
		t.Errorf("nil.SumDurationsForRefs = (%v, %v), want (0, false)", got, complete)
	}
}

// TestTestMapTestsForDedupsAcrossPackages pins the name dedup in TestsFor:
// the same test name covering a line from two packages must surface once,
// since the cache resolver keys by name and is package-agnostic.
func TestTestMapTestsForDedupsAcrossPackages(t *testing.T) {
	tm := &TestMap{
		index: map[string]map[testKey]bool{
			"f.go:1": {
				{pkg: "m/a", name: "TestShared"}: true,
				{pkg: "m/b", name: "TestShared"}: true,
				{pkg: "m/a", name: "TestOther"}:  true,
			},
		},
	}
	got := tm.TestsFor("f.go", 1)
	if len(got) != 2 {
		t.Fatalf("TestsFor = %v (%d names), want 2 deduped names", got, len(got))
	}
	seen := map[string]bool{}
	for _, n := range got {
		if seen[n] {
			t.Errorf("duplicate name %q — TestsFor must dedup across packages", n)
		}
		seen[n] = true
	}
	if !seen["TestShared"] || !seen["TestOther"] {
		t.Errorf("TestsFor = %v, want {TestShared, TestOther}", got)
	}
}

func TestTestMapSumDurationsFor(t *testing.T) {
	tm := &TestMap{
		durations: map[testKey]time.Duration{
			{pkg: "p", name: "TestA"}: 100 * time.Millisecond,
			{pkg: "p", name: "TestB"}: 200 * time.Millisecond,
			{pkg: "q", name: "TestA"}: 999 * time.Millisecond,
		},
	}

	// Two known tests in pkg "p" → sum and complete.
	got, complete := tm.SumDurationsFor("p", []string{"TestA", "TestB"})
	if got != 300*time.Millisecond || !complete {
		t.Errorf("SumDurationsFor(p, [A,B]) = (%v, %v), want (300ms, true)", got, complete)
	}

	// Cross-package isolation: pkg q's TestA must not leak into p.
	got, complete = tm.SumDurationsFor("p", []string{"TestA"})
	if got != 100*time.Millisecond || !complete {
		t.Errorf("SumDurationsFor(p, [A]) = (%v, %v), want (100ms, true)", got, complete)
	}

	// Missing test → not complete; sum reset to 0 so caller doesn't read a partial sum.
	got, complete = tm.SumDurationsFor("p", []string{"TestA", "TestMissing"})
	if got != 0 || complete {
		t.Errorf("SumDurationsFor(p, [A,Missing]) = (%v, %v), want (0, false)", got, complete)
	}

	// Empty test list → not complete (avoid the "all 0 of 0 → use 0 timeout" trap).
	got, complete = tm.SumDurationsFor("p", nil)
	if got != 0 || complete {
		t.Errorf("SumDurationsFor(p, nil) = (%v, %v), want (0, false)", got, complete)
	}

	// Nil receiver tolerated.
	var nilTm *TestMap
	got, complete = nilTm.SumDurationsFor("p", []string{"TestA"})
	if got != 0 || complete {
		t.Errorf("nil.SumDurationsFor = (%v, %v), want (0, false)", got, complete)
	}
}

// TestTestMapIngestResultUpdatesBothMaps kills STATEMENT_REMOVE on the
// recordDuration call inside the BuildTestMap collect loop. Without
// this assertion any path that drops the duration recording would be
// invisible — the index map still gets populated by addBlocks.
func TestTestMapIngestResultUpdatesBothMaps(t *testing.T) {
	tm := &TestMap{
		index:     map[string]map[testKey]bool{},
		durations: map[testKey]time.Duration{},
	}
	tm.ingestResult(testCoverage{
		pkg:      "p",
		testName: "TestA",
		duration: 25 * time.Millisecond,
		blocks: []Block{
			{File: "f.go", StartLine: 5, EndLine: 5, Count: 1},
		},
	})

	if got := tm.durations[testKey{pkg: "p", name: "TestA"}]; got != 25*time.Millisecond {
		t.Errorf("durations not populated by ingestResult; got %v want 25ms — STATEMENT_REMOVE on the recordDuration call would zero this", got)
	}
	if !tm.index["f.go:5"][testKey{pkg: "p", name: "TestA"}] {
		t.Errorf("addBlocks side of ingestResult missing the f.go:5 → TestA edge; index=%v", tm.index)
	}
}

func TestTestMapRecordDurationAccumulates(t *testing.T) {
	// Same (pkg, name) recorded twice — the per-test entry must
	// accumulate, not overwrite. Mirrors the documented behavior contract
	// on recordDuration.
	tm := &TestMap{
		durations: map[testKey]time.Duration{},
	}
	tm.recordDuration("p", "TestA", 10*time.Millisecond)
	tm.recordDuration("p", "TestA", 5*time.Millisecond)
	tm.recordDuration("p", "TestB", 7*time.Millisecond)

	if got := tm.durations[testKey{pkg: "p", name: "TestA"}]; got != 15*time.Millisecond {
		t.Errorf("durations[p,TestA] = %v, want 15ms (10ms + 5ms)", got)
	}
	if got := tm.durations[testKey{pkg: "p", name: "TestB"}]; got != 7*time.Millisecond {
		t.Errorf("durations[p,TestB] = %v, want 7ms", got)
	}

	// Zero or negative durations are dropped (sentinel "no measurement").
	tm.recordDuration("p", "TestC", 0)
	if _, ok := tm.durations[testKey{pkg: "p", name: "TestC"}]; ok {
		t.Errorf("zero duration recorded into durations map")
	}
	tm.recordDuration("p", "TestC", -time.Second)
	if _, ok := tm.durations[testKey{pkg: "p", name: "TestC"}]; ok {
		t.Errorf("negative duration recorded into durations map")
	}
}

// TestNewTestMapForTestingPopulatesAllMaps exercises the helper from
// inside the coverage package so its lines appear in this package's
// own coverage profile. Without this, NewTestMapForTesting is only
// driven from runner-package tests and shows up as NOT_COVERED in
// gomutants's own self-mutation runs (each go-test binary instruments
// only its own package's source).
func TestNewTestMapForTestingPopulatesAllMaps(t *testing.T) {
	tm := NewTestMapForTesting(
		map[[2]string]time.Duration{
			{"p", "TestA"}: 30 * time.Millisecond,
			{"p", "TestB"}: 70 * time.Millisecond,
			{"q", "TestA"}: 100 * time.Millisecond, // same name, different pkg
		},
		map[string][]TestRef{
			"f.go:10": {{Pkg: "p", Name: "TestA"}, {Pkg: "p", Name: "TestB"}},
			"g.go:1":  {{Pkg: "p", Name: "TestA"}},
		},
	)

	// Per-test durations land in the right (pkg, name) cells.
	if got := tm.durations[testKey{pkg: "p", name: "TestA"}]; got != 30*time.Millisecond {
		t.Errorf("durations[p,TestA]=%v, want 30ms — STATEMENT_REMOVE on the recordDuration call would zero this", got)
	}
	if got := tm.durations[testKey{pkg: "q", name: "TestA"}]; got != 100*time.Millisecond {
		t.Errorf("durations[q,TestA]=%v, want 100ms — cross-package isolation must hold", got)
	}

	// Cover index entries are converted from []TestRef to set[testKey]bool.
	if !tm.index["f.go:10"][testKey{pkg: "p", name: "TestA"}] || !tm.index["f.go:10"][testKey{pkg: "p", name: "TestB"}] {
		t.Errorf("f.go:10 → {TestA, TestB} edges missing; got %v — STATEMENT_REMOVE on the index assignment would empty this", tm.index["f.go:10"])
	}
	if !tm.index["g.go:1"][testKey{pkg: "p", name: "TestA"}] {
		t.Errorf("g.go:1 → TestA edge missing; got %v", tm.index["g.go:1"])
	}

	// Empty inputs produce an empty-but-usable TestMap (no nil maps).
	empty := NewTestMapForTesting(nil, nil)
	if empty.durations == nil || empty.index == nil {
		t.Errorf("NewTestMapForTesting(nil, nil) left a nil internal map; future writes would panic")
	}
}

func TestProcessWorkRecordsDurationOnlyEntries(t *testing.T) {
	// A test that produces no coverage blocks but takes nonzero wall-time
	// must still surface its duration to the caller — the runner will
	// execute it on every mutant in its package and the per-package sum
	// must reflect that work.
	orig := runCompiledTestFunc
	defer func() { runCompiledTestFunc = orig }()
	runCompiledTestFunc = func(_ context.Context, _ *compiledPkg, _, _ string, _ time.Duration) ([]Block, time.Duration, error) {
		return nil, 50 * time.Millisecond, nil
	}

	work := make(chan testEntry, 1)
	results := make(chan testCoverage, 1)
	work <- testEntry{name: "TestNoCovButSlow", pkg: "p"}
	close(work)

	pkgBins := map[string]*compiledPkg{
		"p": {binPath: "x", importPath: "p", dir: t.TempDir()},
	}
	processWork(context.Background(), work, pkgBins, filepath.Join(t.TempDir(), "p.cov"), 0, nil, results)
	close(results)

	got := 0
	for tc := range results {
		got++
		if tc.duration != 50*time.Millisecond {
			t.Errorf("forwarded duration = %v, want 50ms", tc.duration)
		}
	}
	if got != 1 {
		t.Errorf("got %d duration-only results, want 1", got)
	}
}

func TestRunPattern(t *testing.T) {
	tests := []struct {
		input []string
		want  string
	}{
		{nil, ""},
		{[]string{}, ""},
		{[]string{"TestA"}, "^(TestA)$"},
		{[]string{"TestA", "TestB"}, "^(TestA|TestB)$"},
		{[]string{"TestSpecial.Name"}, `^(TestSpecial\.Name)$`},
	}
	for _, tc := range tests {
		got := RunPattern(tc.input)
		if got != tc.want {
			t.Errorf("RunPattern(%v) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestProcessWorkContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	work := make(chan testEntry, 2)
	results := make(chan testCoverage, 10)

	// Send work items, then cancel context.
	work <- testEntry{name: "TestA", pkg: "unknown"}
	work <- testEntry{name: "TestB", pkg: "unknown"}
	cancel()
	close(work)

	// No compiled packages — cp will be nil, exercising the nil check.
	processWork(ctx, work, map[string]*compiledPkg{}, filepath.Join(t.TempDir(), "p.cov"), 0, nil, results)
	close(results)

	// Should complete without hanging.
	for range results {
	}
}

func TestProcessWorkNilPkg(t *testing.T) {
	ctx := context.Background()
	work := make(chan testEntry, 1)
	results := make(chan testCoverage, 1)

	// Package not in pkgBins — cp == nil path.
	work <- testEntry{name: "TestA", pkg: "missing"}
	close(work)

	processWork(ctx, work, map[string]*compiledPkg{}, filepath.Join(t.TempDir(), "p.cov"), 0, nil, results)
	close(results)

	if len(results) != 0 {
		t.Error("expected no results for nil package")
	}
}

func TestFeedWorkContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	work := make(chan testEntry) // Unbuffered — will block on send.
	tests := []testEntry{{name: "TestA", pkg: "pkg"}}

	// Should not hang — context cancelled means it takes the ctx.Done() path.
	feedWork(ctx, tests, work)

	// Channel should be closed.
	_, ok := <-work
	if ok {
		t.Error("expected work channel to be closed")
	}
}

func TestFeedWorkNormal(t *testing.T) {
	ctx := context.Background()
	work := make(chan testEntry, 3)
	tests := []testEntry{
		{name: "TestA", pkg: "pkg"},
		{name: "TestB", pkg: "pkg"},
	}

	feedWork(ctx, tests, work)

	received := 0
	for range work {
		received++
	}
	if received != 2 {
		t.Errorf("expected 2 test entries, got %d", received)
	}
}

// TestAddBlocksContinuesPastCount0 kills INVERT_LOOP_CTRL on the `continue`
// in addBlocks (testmap.go:132). Mutated to `break`, hitting any Count==0
// block aborts the entire block walk so later Count>0 blocks never make
// it into the index.
func TestAddBlocksContinuesPastCount0(t *testing.T) {
	tm := &TestMap{index: make(map[string]map[testKey]bool)}
	tm.addBlocks("p", "TestX", []Block{
		// Count==0 first — must be skipped, not break.
		{File: "f.go", StartLine: 1, EndLine: 1, Count: 0},
		// Count>0 covering line 10 — must be indexed.
		{File: "f.go", StartLine: 10, EndLine: 10, Count: 1},
	})
	if _, ok := tm.index["f.go:10"]; !ok {
		t.Errorf("index missing f.go:10 — addBlocks must continue past a Count==0 block; got index keys %v", keysOf(tm.index))
	}
}

func keysOf(m map[string]map[testKey]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestProcessWorkReturnsImmediatelyOnCancelledCtx kills BRANCH_IF on the
// `if ctx.Err() != nil { return }` body in processWork (testmap.go:147).
// With the body elided, the loop falls through to runCompiledTestFunc.
// Stub it and assert no calls happen when ctx is cancelled before dispatch.
func TestProcessWorkReturnsImmediatelyOnCancelledCtx(t *testing.T) {
	orig := runCompiledTestFunc
	defer func() { runCompiledTestFunc = orig }()
	var calls atomic.Int32
	runCompiledTestFunc = func(_ context.Context, _ *compiledPkg, _, _ string, _ time.Duration) ([]Block, time.Duration, error) {
		calls.Add(1)
		return nil, 0, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	work := make(chan testEntry, 1)
	results := make(chan testCoverage, 1)
	work <- testEntry{name: "TestA", pkg: "pkg1"}
	close(work)

	pkgBins := map[string]*compiledPkg{
		"pkg1": {binPath: "x", importPath: "pkg1", dir: t.TempDir()},
	}
	processWork(ctx, work, pkgBins, filepath.Join(t.TempDir(), "p.cov"), 0, nil, results)
	close(results)

	if got := calls.Load(); got != 0 {
		t.Errorf("runCompiledTestFunc called %d times after ctx cancel — BRANCH_IF on the ctx-return body lets execution fall through", got)
	}
}

// TestProcessWorkContinuesPastEmptyBlocks kills INVERT_LOOP_CTRL on the
// `continue` for `len(blocks) == 0` (testmap.go:138). Mutated to `break`,
// a single empty-blocks test ends the worker before later non-empty tests
// run, so their results never reach the channel.
func TestProcessWorkContinuesPastEmptyBlocks(t *testing.T) {
	orig := runCompiledTestFunc
	defer func() { runCompiledTestFunc = orig }()
	runCompiledTestFunc = func(_ context.Context, _ *compiledPkg, testName, _ string, _ time.Duration) ([]Block, time.Duration, error) {
		if testName == "TestEmpty" {
			return nil, 0, nil
		}
		return []Block{{File: "f.go", StartLine: 1, EndLine: 1, Count: 1}}, 0, nil
	}

	work := make(chan testEntry, 2)
	results := make(chan testCoverage, 2)
	work <- testEntry{name: "TestEmpty", pkg: "pkg1"}
	work <- testEntry{name: "TestNonEmpty", pkg: "pkg1"}
	close(work)

	pkgBins := map[string]*compiledPkg{
		"pkg1": {binPath: "x", importPath: "pkg1", dir: t.TempDir()},
	}
	processWork(context.Background(), work, pkgBins, filepath.Join(t.TempDir(), "p.cov"), 0, nil, results)
	close(results)

	count := 0
	for range results {
		count++
	}
	if count != 1 {
		t.Errorf("got %d results, want 1 — INVERT_LOOP_CTRL turns the empty-blocks `continue` into `break`, killing the next test", count)
	}
}

// TestProcessWorkContinuesPastNilCp kills INVERT_LOOP_CTRL on the
// `continue` for `cp == nil` (testmap.go:152). Mutated to `break`, a single
// missing-pkg test stops the worker — later valid tests never run.
func TestProcessWorkContinuesPastNilCp(t *testing.T) {
	orig := runCompiledTestFunc
	defer func() { runCompiledTestFunc = orig }()
	var calls atomic.Int32
	runCompiledTestFunc = func(_ context.Context, cp *compiledPkg, _, _ string, _ time.Duration) ([]Block, time.Duration, error) {
		calls.Add(1)
		return nil, 0, nil
	}

	work := make(chan testEntry, 2)
	results := make(chan testCoverage, 1)
	work <- testEntry{name: "TestUnknownPkg", pkg: "missing"}
	work <- testEntry{name: "TestKnownPkg", pkg: "pkg1"}
	close(work)

	pkgBins := map[string]*compiledPkg{
		"pkg1": {binPath: "x", importPath: "pkg1", dir: t.TempDir()},
	}
	processWork(context.Background(), work, pkgBins, filepath.Join(t.TempDir(), "p.cov"), 0, nil, results)
	close(results)

	if got := calls.Load(); got != 1 {
		t.Errorf("runCompiledTestFunc called %d times, want 1 — INVERT_LOOP_CTRL turns the cp==nil `continue` into `break`, killing later tests", got)
	}
}

// TestProcessWorkSkipsEmptyBlocks kills CONDITIONALS_NEGATION on the
// `if len(blocks) == 0` guard in processWork (post-refactor). Without the
// guard, every test result — including those with no coverage blocks —
// gets sent to the results channel, polluting the map with empty entries.
func TestProcessWorkSkipsEmptyBlocks(t *testing.T) {
	orig := runCompiledTestFunc
	defer func() { runCompiledTestFunc = orig }()
	runCompiledTestFunc = func(_ context.Context, _ *compiledPkg, _, _ string, _ time.Duration) ([]Block, time.Duration, error) {
		return nil, 0, nil // simulate test that produced no coverage blocks
	}

	work := make(chan testEntry, 1)
	results := make(chan testCoverage, 1)
	work <- testEntry{name: "TestNoCoverage", pkg: "pkg1"}
	close(work)

	pkgBins := map[string]*compiledPkg{
		"pkg1": {binPath: "x", importPath: "pkg1", dir: t.TempDir()},
	}
	processWork(context.Background(), work, pkgBins, filepath.Join(t.TempDir(), "p.cov"), 0, nil, results)
	close(results)

	count := 0
	for range results {
		count++
	}
	if count != 0 {
		t.Errorf("got %d results, want 0 — empty-block tests must not produce a result entry", count)
	}
}

// TestBuildPkgBinsSkipsCompileFailure kills BRANCH_IF on the
// `if err != nil { continue }` body in buildPkgBins. Without the continue,
// pkgBins[pkg.importPath] = cp executes with cp==nil, leaking a nil entry
// into the map. Through processWork the difference is invisible (nil entry
// vs missing key both fail the cp==nil guard); inspecting pkgBins directly
// is what makes the mutant observable. It also pins which failures are
// reported: a real compile failure is, a package without test files isn't.
func TestBuildPkgBinsSkipsCompileFailure(t *testing.T) {
	orig := compileTestBinaryFunc
	defer func() { compileTestBinaryFunc = orig }()
	compileFailed := errors.New("compile failed")
	compileTestBinaryFunc = func(_ context.Context, _ string, _ BuildOptions, pkg resolvedPkg) (*compiledPkg, error) {
		switch pkg.importPath {
		case "fail":
			return nil, compileFailed
		case "notests":
			return nil, fmt.Errorf("test binary missing for notests: %w", errNoTestBinary)
		}
		return &compiledPkg{importPath: pkg.importPath, binPath: "x", dir: pkg.dir}, nil
	}

	bins, failures := buildPkgBins(context.Background(), "", BuildOptions{}, []resolvedPkg{
		{importPath: "fail"},
		{importPath: "notests"},
		{importPath: "ok"},
	})

	if _, ok := bins["fail"]; ok {
		t.Errorf("pkgBins should not contain failed pkg — BRANCH_IF on the err-continue body lets nil entries through; got bins=%v", bins)
	}
	if _, ok := bins["ok"]; !ok {
		t.Errorf("pkgBins should contain ok pkg, got bins=%v", bins)
	}
	if len(failures) != 1 || failures[0].err != compileFailed || failures[0].pkg.importPath != "fail" {
		t.Errorf("failures = %v, want only the compile failure (a package without test files is not one)", failures)
	}
}

// TestCompileTestBinaryReturnsErrOnRunFailure kills BRANCH_IF on the
// `if err := cmd.Run(); err != nil { return nil, ... }` body in
// compileTestBinary. `go test -c` against a bogus package fails, so under
// the original the function short-circuits to a compile error. Under
// mutation it falls through to statFileFunc and reads as a package
// without test files, which buildPkgBins would not report.
func TestCompileTestBinaryReturnsErrOnRunFailure(t *testing.T) {
	tmpDir := t.TempDir()
	pkg := resolvedPkg{importPath: "definitely.not.a.real/pkg/zzz", dir: t.TempDir()}

	cp, err := compileTestBinary(context.Background(), pkg.dir, BuildOptions{TmpDir: tmpDir}, pkg)
	if err == nil {
		t.Errorf("expected error from cmd.Run failure, got nil with cp=%+v — BRANCH_IF on the run-error return lets a stale binary masquerade as success", cp)
	}
	if err != nil && !strings.Contains(err.Error(), "go test -c") {
		t.Errorf("error should wrap `go test -c` failure, got: %v", err)
	}
	if errors.Is(err, errNoTestBinary) {
		t.Errorf("a failed compile must not read as a package without test files: %v", err)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Errorf("error should wrap the go command's exit error, got: %v", err)
	}
}

// TestCompileTestBinaryReturnsErrOnMissingBinary kills BRANCH_IF on the
// `if _, err := statFileFunc(binPath); err != nil { return nil, ... }`
// body in compileTestBinary. Stub statFileFunc to fail; under the original
// the missing-binary path returns an error. Under mutation execution falls
// through to the &compiledPkg{} return.
func TestCompileTestBinaryReturnsErrOnMissingBinary(t *testing.T) {
	dir := setupTestProject(t)
	tmpDir := t.TempDir()

	orig := statFileFunc
	defer func() { statFileFunc = orig }()
	statErr := errors.New("injected missing-binary")
	statFileFunc = func(string) (os.FileInfo, error) {
		return nil, statErr
	}

	cp, err := compileTestBinary(context.Background(), dir, BuildOptions{TmpDir: tmpDir}, resolvedPkg{importPath: "testmod", dir: dir})
	if err == nil {
		t.Errorf("expected error when statFileFunc fails, got nil with cp=%+v — BRANCH_IF on the missing-binary return elides the early exit", cp)
	}
	if err != nil && !strings.Contains(err.Error(), "test binary missing") {
		t.Errorf("error should wrap missing-binary failure, got: %v", err)
	}
	if !errors.Is(err, errNoTestBinary) || !errors.Is(err, statErr) {
		t.Errorf("a missing binary must wrap errNoTestBinary (so buildPkgBins doesn't report it) and the stat error: %v", err)
	}
}

// TestRunCompiledTestUsesPkgDirAsCwd kills STATEMENT_REMOVE on the
// `cmd.Dir = cp.dir` line. The compiled test binary opens
// "./testdata/sample.txt" — a path resolved against the process cwd.
// Under the original, cwd is set to cp.dir so the open succeeds and the
// test passes (coverage blocks recorded). Under mutation cmd.Dir stays
// empty so the binary runs from the test process's cwd; the file isn't
// there, the test fails, cmd.Run reports non-zero, and we get nil blocks.
func TestRunCompiledTestUsesPkgDirAsCwd(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"go.mod": "module cwdmod\n\ngo 1.26\n",
		"lib.go": "package cwdmod\n\nfunc Touch() {}\n",
		"lib_test.go": `package cwdmod
import (
	"os"
	"testing"
)
func TestNeedsCwd(t *testing.T) {
	Touch()
	if _, err := os.Stat("testdata/sample.txt"); err != nil {
		t.Fatalf("relative path resolved against wrong cwd: %v", err)
	}
}
`,
		"testdata/sample.txt": "ok\n",
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	tmpDir := t.TempDir()
	pkg := resolvedPkg{importPath: "cwdmod", dir: dir}
	cp, err := compileTestBinary(context.Background(), dir, BuildOptions{TmpDir: tmpDir}, pkg)
	if err != nil {
		t.Fatalf("compileTestBinary: %v", err)
	}

	profilePath := filepath.Join(tmpDir, "cwd.cov")
	blocks, _, _ := runCompiledTest(context.Background(), cp, "TestNeedsCwd", profilePath, 0)
	if len(blocks) == 0 {
		t.Errorf("expected coverage blocks; the test fails when cwd != cp.dir, so STATEMENT_REMOVE on `cmd.Dir = cp.dir` would zero this out")
	}
}

// TestBuildTestMapContinuesPastFailedCompile kills INVERT_LOOP_CTRL on the
// `continue` after a compile failure in buildPkgBins. Stub
// compileTestBinaryFunc to fail for the first package and succeed for the
// second; assert the second package's tests still drive runCompiledTestFunc,
// and that a partial failure is not an error and keeps the failed
// package's suite in scope: a failed -cover build may build for a mutant
// run, and its tests may kill.
func TestBuildTestMapContinuesPastFailedCompile(t *testing.T) {
	failDir := t.TempDir()
	ran := stubBuildTestMapDeps(t, nil, []resolvedPkg{
		{importPath: "pkg.fail", dir: failDir},
		{importPath: "pkg.ok", dir: t.TempDir()},
	})
	listTestsFunc = func(_ context.Context, bins map[string]*compiledPkg, _ time.Duration, _ int) ([]testEntry, []error) {
		var tests []testEntry
		for pkg := range bins {
			tests = append(tests, testEntry{name: "TestA", pkg: pkg})
		}
		return tests, nil
	}
	compileTestBinaryFunc = func(_ context.Context, _ string, _ BuildOptions, pkg resolvedPkg) (*compiledPkg, error) {
		if pkg.importPath == "pkg.fail" {
			return nil, errors.New("compile failed")
		}
		return &compiledPkg{binPath: "x", importPath: pkg.importPath, dir: pkg.dir}, nil
	}

	tm, err := BuildTestMap(context.Background(), t.TempDir(), []string{"./..."}, BuildOptions{TmpDir: t.TempDir(), Workers: 1})
	if err != nil {
		t.Fatalf("BuildTestMap: %v", err)
	}
	if got := atomic.LoadInt32(ran); got != 1 {
		t.Errorf("runCompiledTestFunc called %d times, want 1 — INVERT_LOOP_CTRL turns the compile-failure `continue` into `break`, dropping pkg.ok", got)
	}
	if got, want := tm.SuitePkgs("pkg.fail"), []Package{{ImportPath: "pkg.fail", Dir: failDir}}; !slices.Equal(got, want) {
		t.Errorf("SuitePkgs(pkg.fail) = %+v, want %+v", got, want)
	}
}

// TestBuildTestMapCrossPkgFollowsCoverPkg: another package's tests can
// kill a mutant only when the binaries were built with -coverpkg, so only
// then does SuitePkgs return more than the mutant's own package.
func TestBuildTestMapCrossPkgFollowsCoverPkg(t *testing.T) {
	for _, coverPkg := range []string{"", "./..."} {
		stubBuildTestMapDeps(t, nil, []resolvedPkg{
			{importPath: "pkg.a", dir: t.TempDir()},
			{importPath: "pkg.b", dir: t.TempDir()},
		})

		tm, err := BuildTestMap(context.Background(), t.TempDir(), []string{"./..."}, BuildOptions{CoverPkg: coverPkg, TmpDir: t.TempDir(), Workers: 1})
		if err != nil {
			t.Fatalf("BuildTestMap(coverpkg=%q): %v", coverPkg, err)
		}
		got := len(tm.SuitePkgs("pkg.b"))
		if want := map[string]int{"": 1, "./...": 2}[coverPkg]; got != want {
			t.Errorf("coverpkg=%q: SuitePkgs(pkg.b) has %d packages, want %d", coverPkg, got, want)
		}
	}
}

// TestBuildTestMapReadsTestDeps: with -coverpkg, a package's suite decides
// only the verdicts of mutants in packages its test binary links, read
// once for every package with tests, compiled or not, with the options.
func TestBuildTestMapReadsTestDeps(t *testing.T) {
	stubBuildTestMapDeps(t, nil, []resolvedPkg{
		{importPath: "pkg.fail", dir: t.TempDir()},
		{importPath: "pkg.ok", dir: t.TempDir()},
		{importPath: "pkg.dep", dir: t.TempDir()},
	})
	compileTestBinaryFunc = func(_ context.Context, _ string, _ BuildOptions, pkg resolvedPkg) (*compiledPkg, error) {
		if pkg.importPath == "pkg.fail" {
			return nil, errors.New("compile failed")
		}
		return &compiledPkg{binPath: "x", importPath: pkg.importPath, dir: pkg.dir}, nil
	}
	var calls []string
	testDepsFunc = func(_ context.Context, _ string, opts BuildOptions, pkgs []string) (map[string]map[string]bool, error) {
		calls = append(calls, opts.Tags+" "+strings.Join(pkgs, " "))
		return map[string]map[string]bool{"pkg.fail": {"pkg.dep": true}, "pkg.ok": {}, "pkg.dep": {}}, nil
	}

	tm, err := BuildTestMap(context.Background(), t.TempDir(), []string{"./..."}, BuildOptions{CoverPkg: "./...", Tags: "t", TmpDir: t.TempDir(), Workers: 1})
	if err != nil {
		t.Fatalf("BuildTestMap: %v", err)
	}
	if !slices.Equal(calls, []string{"t pkg.dep pkg.fail pkg.ok"}) {
		t.Errorf("testDeps calls = %q, want one for every package with the tags", calls)
	}
	if got := importPaths(tm.SuitePkgs("pkg.dep")); !slices.Equal(got, []string{"pkg.dep", "pkg.fail"}) {
		t.Errorf("SuitePkgs(pkg.dep) = %v, want pkg.dep and pkg.fail, whose tests link it", got)
	}
	if got := importPaths(tm.SuitePkgs("pkg.ok")); !slices.Equal(got, []string{"pkg.ok"}) {
		t.Errorf("SuitePkgs(pkg.ok) = %v, want only pkg.ok: no other package's tests link it", got)
	}
}

// TestSuites: every suite in scope, sorted, as a copy the caller can't
// use to change the map; a nil map has none.
func TestSuites(t *testing.T) {
	if got := (*TestMap)(nil).Suites(); got != nil {
		t.Errorf("nil map: Suites = %+v, want nil", got)
	}
	a, b := Package{ImportPath: "m/a", Dir: "/a"}, Package{ImportPath: "m/b", Dir: "/b"}
	tm := NewTestMapForTesting(nil, nil).WithSuitesForTesting(false, nil, b, a)
	got := tm.Suites()
	if want := []Package{a, b}; !slices.Equal(got, want) {
		t.Fatalf("Suites = %+v, want %+v", got, want)
	}
	got[0] = b
	if again := tm.Suites(); again[0] != a {
		t.Errorf("Suites after changing the returned slice = %+v, want the map unchanged", again)
	}
}

// TestTestBinaryEnv: a test binary's environment is gomutants' own with
// GOROOT's bin directory first on PATH and PWD the run's directory, as
// `go test` gives it. Go takes the last of a repeated variable, so the
// entries that count are the last ones.
func TestTestBinaryEnv(t *testing.T) {
	last := func(env []string, key string) (string, int) {
		val, n := "", 0
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, key+"="); ok {
				val, n = v, n+1
			}
		}
		return val, n
	}
	goroot, dir := filepath.Join("g", "root"), filepath.Join("pkg", "dir")
	bin := filepath.Join(goroot, "bin")
	sep := string(os.PathListSeparator)

	t.Setenv("PATH", "first"+sep+"second")
	env := TestBinaryEnv(goroot, dir)
	if got, _ := last(env, "PATH"); got != bin+sep+"first"+sep+"second" {
		t.Errorf("PATH = %q, want GOROOT/bin before the inherited PATH", got)
	}
	if got, _ := last(env, "PWD"); got != dir {
		t.Errorf("PWD = %q, want %q", got, dir)
	}
	if _, n := last(env, "PATH"); n != 2 {
		t.Errorf("PATH set %d times, want the inherited one and ours", n)
	}

	if got, n := last(TestBinaryEnv("", dir), "PATH"); got != "first"+sep+"second" || n != 1 {
		t.Errorf("no GOROOT: PATH = %q (set %d times), want it left alone", got, n)
	}

	t.Setenv("PATH", "")
	if got, _ := last(TestBinaryEnv(goroot, dir), "PATH"); got != bin {
		t.Errorf("empty PATH: PATH = %q, want GOROOT/bin alone", got)
	}
}

// TestGOROOT: the toolchain's GOROOT holds its bin directory; a `go` that
// can't run is an error.
func TestGOROOT(t *testing.T) {
	got, err := GOROOT(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(got, "bin")); err != nil || !fi.IsDir() {
		t.Errorf("GOROOT = %q, want a directory with a bin directory: %v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := GOROOT(ctx, t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Errorf("GOROOT with a cancelled context: err = %v, want one wrapping context.Canceled", err)
	}
}

// TestSuiteDir: a suite's directory by import path, wherever it sits in
// the sorted suites; none for a package outside them or on a nil map.
func TestSuiteDir(t *testing.T) {
	if dir, ok := (*TestMap)(nil).SuiteDir("m/a"); dir != "" || ok {
		t.Errorf("nil map: SuiteDir = (%q, %v), want none", dir, ok)
	}
	a := Package{ImportPath: "m/a", Dir: "/a"}
	b := Package{ImportPath: "m/b", Dir: "/b"}
	c := Package{ImportPath: "m/c", Dir: "/c"}
	tm := NewTestMapForTesting(nil, nil).WithSuitesForTesting(false, nil, c, a, b)
	cases := []struct {
		pkg, want string
		ok        bool
	}{
		{"m/a", "/a", true},
		{"m/b", "/b", true},
		{"m/c", "/c", true},
		{"m/0", "", false},
		{"m/ab", "", false},
		{"m/d", "", false},
	}
	for _, tc := range cases {
		if dir, ok := tm.SuiteDir(tc.pkg); dir != tc.want || ok != tc.ok {
			t.Errorf("SuiteDir(%s) = (%q, %v), want (%q, %v)", tc.pkg, dir, ok, tc.want, tc.ok)
		}
	}
}

// TestSuitePkgs pins which suites decide a mutant's verdict: its own
// package's without cross-package coverage; with it, every package
// (sorted) whose tests link the mutant's package or whose links are
// unknown, and its own; and nothing on a nil map.
func TestSuitePkgs(t *testing.T) {
	a := Package{ImportPath: "m/a", Dir: "/a"}
	b := Package{ImportPath: "m/b", Dir: "/b"}
	c := Package{ImportPath: "m/c", Dir: "/c"}
	base := NewTestMapForTesting(nil, nil)
	own := base.WithSuitesForTesting(false, nil, c, a)
	cross := base.WithSuitesForTesting(true, nil, c, a, b)
	linked := base.WithSuitesForTesting(true, map[string]map[string]bool{"m/a": {"m/x": true}, "m/b": {}}, c, a, b)
	cases := []struct {
		name string
		tm   *TestMap
		pkg  string
		want []Package
	}{
		{"own package", own, "m/a", []Package{a}},
		{"own package without tests", own, "m/b", nil},
		{"cross-package: links unknown, every package, sorted", cross, "m/z", []Package{a, b, c}},
		{"cross-package: linking packages and unknown links", linked, "m/x", []Package{a, c}},
		{"cross-package: own package even when not linked", linked, "m/b", []Package{b, c}},
		{"nil map", nil, "m/a", nil},
	}
	for _, tc := range cases {
		if got := tc.tm.SuitePkgs(tc.pkg); !slices.Equal(got, tc.want) {
			t.Errorf("%s: SuitePkgs(%q) = %+v, want %+v", tc.name, tc.pkg, got, tc.want)
		}
	}
}

// TestWithWarningsForTestingCopies: the test helper sets exactly the
// warnings given, on a copy, leaving the map it is called on as it is.
func TestWithWarningsForTestingCopies(t *testing.T) {
	base := newTestMap(false)
	got := base.WithWarningsForTesting("a", "b")
	if !slices.Equal(got.Warnings(), []string{"a", "b"}) || base.Warnings() != nil {
		t.Errorf("copy warns %q, original %q; want [a b] and none", got.Warnings(), base.Warnings())
	}
}

// TestWithSuitesForTestingCopies: the test helper leaves the map it is
// called on as it was, and sorts the suites it is given.
func TestWithSuitesForTestingCopies(t *testing.T) {
	base := NewTestMapForTesting(nil, nil)
	deps := map[string]map[string]bool{"m/a": {}}
	got := base.WithSuitesForTesting(true, deps, Package{ImportPath: "m/b"}, Package{ImportPath: "m/a"})
	if !slices.Equal(importPaths(got.suites), []string{"m/a", "m/b"}) || !got.crossPkg || got.testDeps == nil {
		t.Errorf("copy: suites %v, crossPkg %v, deps %v; want m/a and m/b sorted, cross-package, the deps", got.suites, got.crossPkg, got.testDeps)
	}
	if base.suites != nil || base.crossPkg || base.testDeps != nil {
		t.Errorf("original: suites %v, crossPkg %v, deps %v; want it unchanged", base.suites, base.crossPkg, base.testDeps)
	}
}

// TestProcessWorkSkipsStalledPackages: once a test times out alone, its
// package's remaining tests don't run; a test that fails alone doesn't
// stop them, and neither stops another package's. Neither reaches the map.
func TestProcessWorkSkipsStalledPackages(t *testing.T) {
	orig := runCompiledTestFunc
	t.Cleanup(func() { runCompiledTestFunc = orig })
	var ran []string
	runCompiledTestFunc = func(_ context.Context, _ *compiledPkg, name, _ string, _ time.Duration) ([]Block, time.Duration, error) {
		ran = append(ran, name)
		switch name {
		case "TestHang":
			return nil, time.Second, fmt.Errorf("%s: %w after 1s", name, errSoloTimeout)
		case "TestFail":
			return nil, time.Millisecond, errors.New("TestFail failed when run alone")
		}
		return nil, time.Millisecond, nil
	}

	entries := []testEntry{
		{name: "TestFail", pkg: "q"},
		{name: "TestQ", pkg: "q"},
		{name: "TestHang", pkg: "p"},
		{name: "TestP", pkg: "p"},
		{name: "TestX", pkg: "x"},
	}
	work := make(chan testEntry, len(entries))
	for _, e := range entries {
		work <- e
	}
	close(work)
	results := make(chan testCoverage, len(entries))
	stalled := &stalledPkgs{set: make(map[string]bool)}
	processWork(context.Background(), work, map[string]*compiledPkg{"p": {}, "q": {}, "x": {}}, filepath.Join(t.TempDir(), "p.cov"), 0, stalled, results)
	close(results)

	if want := []string{"TestFail", "TestQ", "TestHang", "TestX"}; !slices.Equal(ran, want) {
		t.Errorf("ran %v, want %v", ran, want)
	}
	var mapped []string
	for tc := range results {
		mapped = append(mapped, tc.testName)
	}
	if want := []string{"TestQ", "TestX"}; !slices.Equal(mapped, want) {
		t.Errorf("results = %v, want %v: a failed or timed-out run maps nothing", mapped, want)
	}
}

// stubBuildTestMapDeps swaps BuildTestMap's go-tool seams for stubs: every
// resolved package compiles, listTests returns `tests`, each compiled test
// run bumps the returned counter, every group of tests passes together,
// and the test deps can't be read. Restored on cleanup.
func stubBuildTestMapDeps(t *testing.T, tests []testEntry, resolved []resolvedPkg) *int32 {
	t.Helper()
	origCompile := compileTestBinaryFunc
	origResolve := resolvePackagesFunc
	origList := listTestsFunc
	origRun := runCompiledTestFunc
	origDeps := testDepsFunc
	origGroup := groupPassesFunc
	t.Cleanup(func() {
		groupPassesFunc = origGroup
		compileTestBinaryFunc = origCompile
		resolvePackagesFunc = origResolve
		listTestsFunc = origList
		runCompiledTestFunc = origRun
		testDepsFunc = origDeps
	})
	testDepsFunc = func(context.Context, string, BuildOptions, []string) (map[string]map[string]bool, error) {
		return nil, errors.New("deps not stubbed")
	}
	groupPassesFunc = func(context.Context, *compiledPkg, []string, time.Duration) bool {
		return true
	}

	resolvePackagesFunc = func(_ context.Context, _ string, _ []string, _ string) ([]resolvedPkg, error) {
		return resolved, nil
	}
	listTestsFunc = func(context.Context, map[string]*compiledPkg, time.Duration, int) ([]testEntry, []error) {
		return tests, nil
	}
	compileTestBinaryFunc = func(_ context.Context, _ string, _ BuildOptions, pkg resolvedPkg) (*compiledPkg, error) {
		return &compiledPkg{binPath: "x", importPath: pkg.importPath, dir: pkg.dir}, nil
	}
	var ran int32
	runCompiledTestFunc = func(_ context.Context, _ *compiledPkg, _, _ string, _ time.Duration) ([]Block, time.Duration, error) {
		atomic.AddInt32(&ran, 1)
		return nil, time.Millisecond, nil
	}
	return &ran
}

// TestBuildTestMapErrorsWhenNothingCompiles pins the all-compiles-failed
// diagnostic: an error with the failure count and the first failure, not
// a silently empty map. Kills BRANCH_IF / CONDITIONALS_NEGATION on the
// guard and STATEMENT_REMOVE on its return; the list and run counters kill
// a guard moved after listing or the worker pool.
func TestBuildTestMapErrorsWhenNothingCompiles(t *testing.T) {
	ran := stubBuildTestMapDeps(t, nil, []resolvedPkg{
		{importPath: "example.com/x", dir: t.TempDir()},
		{importPath: "example.com/y", dir: t.TempDir()},
	})
	boom := errors.New("boom")
	compileTestBinaryFunc = func(_ context.Context, _ string, _ BuildOptions, pkg resolvedPkg) (*compiledPkg, error) {
		return nil, fmt.Errorf("go test -c %s: %w", pkg.importPath, boom)
	}
	listed := false
	listTestsFunc = func(context.Context, map[string]*compiledPkg, time.Duration, int) ([]testEntry, []error) {
		listed = true
		return nil, nil
	}

	tm, err := BuildTestMap(context.Background(), t.TempDir(), []string{"./..."}, BuildOptions{TmpDir: t.TempDir(), Workers: 1})
	if err == nil {
		t.Fatal("BuildTestMap returned nil error with no test binary compiled — per-test routing would switch off silently")
	}
	if tm != nil {
		t.Errorf("BuildTestMap returned a non-nil map alongside the error: %+v", tm)
	}
	if !strings.Contains(err.Error(), "2 packages failed") || !strings.Contains(err.Error(), "go test -c example.com/x: boom") {
		t.Errorf("error %q should give the failure count and the first failure", err)
	}
	if !errors.Is(err, boom) {
		t.Errorf("error %q should wrap the first failure", err)
	}
	if listed || atomic.LoadInt32(ran) != 0 {
		t.Errorf("listed=%v runs=%d after every compile failed, want neither", listed, atomic.LoadInt32(ran))
	}
}

// TestBuildTestMapNoTestFilesIsNotAnError kills EXPRESSION_REMOVE on the
// `len(compileFailures) > 0` operand: a module whose packages have no test
// files compiles no binary, and that must stay a successful (empty) build.
func TestBuildTestMapNoTestFilesIsNotAnError(t *testing.T) {
	stubBuildTestMapDeps(t, nil, []resolvedPkg{{importPath: "example.com/x", dir: t.TempDir()}})
	compileTestBinaryFunc = func(context.Context, string, BuildOptions, resolvedPkg) (*compiledPkg, error) {
		return nil, fmt.Errorf("test binary missing: %w", errNoTestBinary)
	}

	tm, err := BuildTestMap(context.Background(), t.TempDir(), []string{"./..."}, BuildOptions{TmpDir: t.TempDir(), Workers: 1})
	if err != nil {
		t.Fatalf("BuildTestMap with no test files: %v", err)
	}
	if tm == nil {
		t.Fatal("BuildTestMap returned a nil map with no test files")
	}
}

// TestBuildTestMapReportsCancellationDuringCompile: a cancelled ctx fails
// every remaining compile, and the error must be the cancellation, not a
// compile diagnostic pointing at a problem that doesn't exist.
func TestBuildTestMapReportsCancellationDuringCompile(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ran := stubBuildTestMapDeps(t, nil, []resolvedPkg{{importPath: "example.com/x", dir: t.TempDir()}})
	compileTestBinaryFunc = func(context.Context, string, BuildOptions, resolvedPkg) (*compiledPkg, error) {
		cancel()
		return nil, errors.New("signal: killed")
	}

	tm, err := BuildTestMap(ctx, t.TempDir(), []string{"./..."}, BuildOptions{TmpDir: t.TempDir(), Workers: 1})
	if err != context.Canceled {
		t.Errorf("BuildTestMap error = %v, want context.Canceled", err)
	}
	if tm != nil || atomic.LoadInt32(ran) != 0 {
		t.Errorf("BuildTestMap went on after cancellation: map=%v runs=%d", tm, atomic.LoadInt32(ran))
	}
}

// TestBuildTestMapReportsCancellationDuringListing: same as above for a
// listing killed by cancellation, which fails every listing: the error is
// the cancellation, not the every-listing-failed diagnostic.
func TestBuildTestMapReportsCancellationDuringListing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ran := stubBuildTestMapDeps(t, nil, []resolvedPkg{{importPath: "example.com/x", dir: t.TempDir()}})
	listTestsFunc = func(context.Context, map[string]*compiledPkg, time.Duration, int) ([]testEntry, []error) {
		cancel()
		return nil, []error{errors.New("example.com/x: signal: killed")}
	}

	tm, err := BuildTestMap(ctx, t.TempDir(), []string{"./..."}, BuildOptions{TmpDir: t.TempDir(), Workers: 1})
	if err != context.Canceled {
		t.Errorf("BuildTestMap error = %v, want context.Canceled", err)
	}
	if tm != nil || atomic.LoadInt32(ran) != 0 {
		t.Errorf("BuildTestMap went on after cancellation: map=%v runs=%d", tm, atomic.LoadInt32(ran))
	}
}

// TestBuildTestMapKeepsUnlistedPackages: a package whose listing failed
// maps no tests but stays in scope, and the rest of the map still builds.
func TestBuildTestMapKeepsUnlistedPackages(t *testing.T) {
	badDir := t.TempDir()
	ran := stubBuildTestMapDeps(t, nil, []resolvedPkg{
		{importPath: "example.com/bad", dir: badDir},
		{importPath: "example.com/ok", dir: t.TempDir()},
	})
	listTestsFunc = func(context.Context, map[string]*compiledPkg, time.Duration, int) ([]testEntry, []error) {
		return []testEntry{{name: "TestA", pkg: "example.com/ok"}}, []error{errors.New("example.com/bad: listing its tests failed")}
	}

	tm, err := BuildTestMap(context.Background(), t.TempDir(), []string{"./..."}, BuildOptions{TmpDir: t.TempDir(), Workers: 1})
	if err != nil {
		t.Fatalf("BuildTestMap: %v", err)
	}
	if got, want := tm.SuitePkgs("example.com/bad"), []Package{{ImportPath: "example.com/bad", Dir: badDir}}; !slices.Equal(got, want) {
		t.Errorf("SuitePkgs(bad) = %+v, want %+v", got, want)
	}
	if got := atomic.LoadInt32(ran); got != 1 {
		t.Errorf("runCompiledTestFunc called %d times, want 1 (example.com/ok's TestA)", got)
	}
	want := []string{"listing the tests of 1 packages failed, so their mutants run their whole suites; first failure: example.com/bad: listing its tests failed"}
	if got := tm.Warnings(); !slices.Equal(got, want) {
		t.Errorf("Warnings = %q, want %q", got, want)
	}
}

// TestBuildTestMapErrorsWhenNothingLists: every listing failing is an
// error with the failure count and the first failure, not an empty map
// whose routing switches off without a word (#105).
func TestBuildTestMapErrorsWhenNothingLists(t *testing.T) {
	ran := stubBuildTestMapDeps(t, nil, []resolvedPkg{
		{importPath: "example.com/x", dir: t.TempDir()},
		{importPath: "example.com/y", dir: t.TempDir()},
	})
	boom := errors.New("boom")
	listTestsFunc = func(context.Context, map[string]*compiledPkg, time.Duration, int) ([]testEntry, []error) {
		return nil, []error{fmt.Errorf("example.com/x: %w", boom), errors.New("example.com/y: boom")}
	}

	tm, err := BuildTestMap(context.Background(), t.TempDir(), []string{"./..."}, BuildOptions{TmpDir: t.TempDir(), Workers: 1})
	if tm != nil || err == nil {
		t.Fatalf("BuildTestMap = (%v, %v), want an error and no map", tm, err)
	}
	if got, want := err.Error(), "no tests listed (2 packages failed); first failure: example.com/x: boom"; got != want {
		t.Errorf("error %q, want %q", got, want)
	}
	if !errors.Is(err, boom) {
		t.Errorf("error %q should wrap the first failure", err)
	}
	if got := atomic.LoadInt32(ran); got != 0 {
		t.Errorf("runCompiledTestFunc called %d times after every listing failed, want 0", got)
	}
}

// TestBuildTestMapKeepsSuitesWhenNothingRoutes: with -coverpkg, a build
// that can route no mutant still knows which packages have tests, and an
// importer's suite may be the only one that kills a mutant. So each way it
// fails — nothing compiles, the test-binary arguments can't be read,
// nothing lists — keeps the map with every suite deciding every verdict
// and the failure as a warning. A dropped map ran each mutant against its
// own package alone, and a package tested only by its importers read every
// mutant LIVED.
func TestBuildTestMapKeepsSuitesWhenNothingRoutes(t *testing.T) {
	xDir, yDir := t.TempDir(), t.TempDir()
	resolved := []resolvedPkg{{importPath: "example.com/x", dir: xDir}, {importPath: "example.com/y", dir: yDir}}
	suites := []Package{{ImportPath: "example.com/x", Dir: xDir}, {ImportPath: "example.com/y", Dir: yDir}}
	boom := errors.New("boom")
	cases := []struct {
		name  string
		opts  BuildOptions
		fail  func()
		cause string
	}{
		{"nothing compiles", BuildOptions{}, func() {
			compileTestBinaryFunc = func(_ context.Context, _ string, _ BuildOptions, pkg resolvedPkg) (*compiledPkg, error) {
				return nil, fmt.Errorf("go test -c %s: %w", pkg.importPath, boom)
			}
		}, "no test binary compiled (2 packages failed); first failure: go test -c example.com/x: boom"},
		{"test-binary arguments unread", BuildOptions{TestFlags: []string{"-short"}}, func() {
			testBinaryArgsFunc = func(context.Context, string, string, string, []string) ([]string, error) { return nil, boom }
		}, "reading the test-binary arguments for --test-flags: boom"},
		{"nothing lists", BuildOptions{}, func() {
			listTestsFunc = func(context.Context, map[string]*compiledPkg, time.Duration, int) ([]testEntry, []error) {
				return nil, []error{fmt.Errorf("example.com/x: %w", boom), errors.New("example.com/y: boom")}
			}
		}, "no tests listed (2 packages failed); first failure: example.com/x: boom"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ran := stubBuildTestMapDeps(t, nil, resolved)
			origArgs := testBinaryArgsFunc
			t.Cleanup(func() { testBinaryArgsFunc = origArgs })
			c.fail()

			opts := c.opts
			opts.CoverPkg, opts.TmpDir, opts.Workers = "./...", t.TempDir(), 1
			tm, err := BuildTestMap(context.Background(), t.TempDir(), []string{"./..."}, opts)
			checkUnroutedMap(t, tm, err, suites, c.cause)
			if got := atomic.LoadInt32(ran); got != 0 {
				t.Errorf("runCompiledTestFunc called %d times, want 0", got)
			}
		})
	}
}

// checkUnroutedMap checks what BuildTestMap returned after failing with
// cause (see unrouted): a map, every suite deciding every package's
// verdicts, and cause as its one warning.
func checkUnroutedMap(t *testing.T, tm *TestMap, err error, suites []Package, cause string) {
	t.Helper()
	if err != nil || tm == nil {
		t.Fatalf("BuildTestMap = (%v, %v), want a map", tm, err)
	}
	for _, pkg := range []string{"example.com/x", "example.com/y", "example.com/lib"} {
		if got := tm.SuitePkgs(pkg); !slices.Equal(got, suites) {
			t.Errorf("SuitePkgs(%s) = %+v, want every suite %+v", pkg, got, suites)
		}
	}
	want := []string{"routing mutants to their covering tests failed, so each runs its own package's whole suite and every survivor is re-checked against every package's suite: " + cause}
	if got := tm.Warnings(); !slices.Equal(got, want) {
		t.Errorf("Warnings = %q, want %q", got, want)
	}
}

// TestBuildTestMapWarnsWithoutTestDeps: links that can't be read leave
// every suite deciding every verdict, which the warning tells the user,
// with go list's error; read links warn of nothing.
func TestBuildTestMapWarnsWithoutTestDeps(t *testing.T) {
	stubBuildTestMapDeps(t, nil, []resolvedPkg{{importPath: "pkg.a", dir: t.TempDir()}})
	opts := BuildOptions{CoverPkg: "./...", TmpDir: t.TempDir(), Workers: 1}
	tm, err := BuildTestMap(context.Background(), t.TempDir(), []string{"./..."}, opts)
	if err != nil {
		t.Fatalf("BuildTestMap: %v", err)
	}
	want := []string{"reading what each package's tests link failed, so every survivor is re-checked against every package's suite: deps not stubbed"}
	if got := tm.Warnings(); !slices.Equal(got, want) {
		t.Errorf("Warnings = %q, want %q", got, want)
	}

	testDepsFunc = func(context.Context, string, BuildOptions, []string) (map[string]map[string]bool, error) {
		return map[string]map[string]bool{"pkg.a": {}}, nil
	}
	if tm, err = BuildTestMap(context.Background(), t.TempDir(), []string{"./..."}, opts); err != nil || tm.Warnings() != nil {
		t.Errorf("BuildTestMap = (warnings %q, %v), want no warnings", tm.Warnings(), err)
	}
}

// TestListTestsSortsFailures: the failures come back sorted by package,
// whatever order the listings finish in, so the first one reported is
// stable from run to run, each wrapping its listing's error.
func TestListTestsSortsFailures(t *testing.T) {
	orig := listBinTestsFunc
	t.Cleanup(func() { listBinTestsFunc = orig })
	boom := errors.New("boom")
	listBinTestsFunc = func(_ context.Context, cp *compiledPkg, _ time.Duration) ([]string, error) {
		if cp.importPath == "ok" {
			return []string{"TestA"}, nil
		}
		return nil, boom
	}
	bins := map[string]*compiledPkg{}
	for _, p := range []string{"d", "b", "ok", "c", "a"} {
		bins[p] = &compiledPkg{importPath: p}
	}
	tests, failures := listTests(context.Background(), bins, 0, 3)
	if !slices.Equal(tests, []testEntry{{name: "TestA", pkg: "ok"}}) {
		t.Errorf("tests = %+v, want ok's TestA", tests)
	}
	var got []string
	for _, f := range failures {
		got = append(got, f.Error())
		if !errors.Is(f, boom) {
			t.Errorf("failure %q should wrap the listing's error", f)
		}
	}
	if want := []string{"a: boom", "b: boom", "c: boom", "d: boom"}; !slices.Equal(got, want) {
		t.Errorf("failures = %q, want %q", got, want)
	}
}

// TestListTestsBoundsConcurrency: listings run in parallel, at most
// `workers` at a time. The stub holds each listing until two are in
// flight, so the peak is exactly 2 with workers=2: a missing bound lets
// all four through, and a bound of 1 never reaches 2.
func TestListTestsBoundsConcurrency(t *testing.T) {
	orig := listBinTestsFunc
	t.Cleanup(func() { listBinTestsFunc = orig })
	var (
		mu           sync.Mutex
		active, peak int
		two          = make(chan struct{})
		closeTwo     sync.Once
	)
	listBinTestsFunc = func(_ context.Context, cp *compiledPkg, _ time.Duration) ([]string, error) {
		mu.Lock()
		active++
		peak = max(peak, active)
		if active == 2 {
			closeTwo.Do(func() { close(two) })
		}
		mu.Unlock()
		select {
		case <-two:
		case <-time.After(5 * time.Second):
		}
		mu.Lock()
		active--
		mu.Unlock()
		return []string{"Test" + cp.importPath}, nil
	}
	bins := map[string]*compiledPkg{}
	for _, p := range []string{"A", "B", "C", "D"} {
		bins[p] = &compiledPkg{importPath: p}
	}

	var tests []testEntry
	// A slot that is never released blocks the loop for good. The deadline
	// stays under the per-mutant timeout so that mutant reads as killed.
	runWithDeadline(t, 5*time.Second, func() {
		tests, _ = listTests(context.Background(), bins, 0, 2)
	})
	if peak != 2 {
		t.Errorf("peak concurrent listings = %d, want 2", peak)
	}
	if len(tests) != 4 {
		t.Errorf("listTests = %+v, want one test per package", tests)
	}
}

// TestBuildTestMapForwardsTestTimeout pins that the per-run bound reaches
// both the listing and every per-test run, and the worker count reaches
// the listing.
func TestBuildTestMapForwardsTestTimeout(t *testing.T) {
	const timeout = 7 * time.Second
	stubBuildTestMapDeps(t, nil, []resolvedPkg{{importPath: "example.com/x", dir: t.TempDir()}})
	var listTimeout, runTimeout time.Duration
	var listWorkers int
	listTestsFunc = func(_ context.Context, _ map[string]*compiledPkg, d time.Duration, workers int) ([]testEntry, []error) {
		listTimeout, listWorkers = d, workers
		return []testEntry{{name: "TestA", pkg: "example.com/x"}}, nil
	}
	runCompiledTestFunc = func(_ context.Context, _ *compiledPkg, _, _ string, d time.Duration) ([]Block, time.Duration, error) {
		runTimeout = d
		return nil, time.Millisecond, nil
	}

	if _, err := BuildTestMap(context.Background(), t.TempDir(), []string{"./..."}, BuildOptions{TmpDir: t.TempDir(), Workers: 3, TestTimeout: timeout}); err != nil {
		t.Fatalf("BuildTestMap: %v", err)
	}
	if listWorkers != 3 {
		t.Errorf("listing ran with %d workers, want 3", listWorkers)
	}
	if listTimeout != timeout || runTimeout != timeout {
		t.Errorf("list timeout %v, run timeout %v; want both %v", listTimeout, runTimeout, timeout)
	}
}

// TestWithTestTimeout: zero means unbounded (kills `<= 0` → `< 0`, which
// would hand every run an already-expired deadline); a positive value sets
// a deadline.
func TestWithTestTimeout(t *testing.T) {
	ctx, cancel := withTestTimeout(context.Background(), 0)
	defer cancel()
	if _, ok := ctx.Deadline(); ok || ctx.Err() != nil {
		t.Errorf("timeout 0: ctx has a deadline or is done (err=%v), want unbounded", ctx.Err())
	}

	// The smallest positive timeout still bounds the run (kills `<= 0` →
	// `<= 1`).
	ctx, cancel = withTestTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	if _, ok := ctx.Deadline(); !ok {
		t.Error("timeout 1ns: ctx has no deadline")
	}
}

// TestBuildTestMapNoListedTestsIsNotAnError kills EXPRESSION_REMOVE on the
// `len(tests) > 0` operand of the no-binary guard: a module without tests
// lists nothing, and that must stay a successful (empty) build.
func TestBuildTestMapNoListedTestsIsNotAnError(t *testing.T) {
	stubBuildTestMapDeps(t, nil, []resolvedPkg{{importPath: "example.com/x", dir: t.TempDir()}})

	tm, err := BuildTestMap(context.Background(), t.TempDir(), []string{"./..."}, BuildOptions{TmpDir: t.TempDir(), Workers: 1})
	if err != nil {
		t.Fatalf("BuildTestMap with no listed tests: %v", err)
	}
	if tm == nil {
		t.Fatal("BuildTestMap returned a nil map with no listed tests")
	}
}

// TestParseTestList keeps only names -test.run can select. The input is
// verbatim -test.list output from a probe package whose TestMain printed
// "partial" without a newline (gluing it onto TestOne, which is lost),
// plus a log line, a benchmark whose name contains Test, and padding.
func TestParseTestList(t *testing.T) {
	out := "partialTestOne\n" +
		"TestTwo\n" +
		"BenchmarkX\n" +
		"BenchmarkTestY\n" +
		"FuzzF\n" +
		"ExampleA\n" +
		"2026/10/07 12:00:00 connecting to db\n" +
		"TestÜber_2\n" +
		"  TestPadded \n" +
		"log: also ends in TestTwo\n" +
		"\n" +
		"Test"
	want := []string{"TestTwo", "FuzzF", "ExampleA", "TestÜber_2", "TestPadded", "Test"}
	if names := parseTestList(out); !slices.Equal(names, want) {
		t.Errorf("parseTestList names = %q, want %q", names, want)
	}
}

// TestProcessWorkDropsFailures: a test that fails alone is left out of
// the map, timing and all: routing a mutant to it alone would fail with or
// without the mutant.
func TestProcessWorkDropsFailures(t *testing.T) {
	orig := runCompiledTestFunc
	t.Cleanup(func() { runCompiledTestFunc = orig })
	runCompiledTestFunc = func(context.Context, *compiledPkg, string, string, time.Duration) ([]Block, time.Duration, error) {
		return []Block{{File: "f.go", StartLine: 1, EndLine: 1, Count: 1}}, time.Millisecond, errors.New("TestA failed when run alone: exit status 1")
	}

	work := make(chan testEntry, 1)
	results := make(chan testCoverage, 1)
	work <- testEntry{name: "TestA", pkg: "pkg1"}
	close(work)
	processWork(context.Background(), work, map[string]*compiledPkg{"pkg1": {binPath: "x", importPath: "pkg1", dir: "/pkg1"}}, filepath.Join(t.TempDir(), "p.cov"), 0, nil, results)
	close(results)

	if got, ok := <-results; ok {
		t.Errorf("result = %+v, want none for a failed run", got)
	}
}

// TestParseTestBinaryArgs covers the shapes of `go test -n` output: plain
// (with the link line, which mentions the binary but doesn't start with
// it), JSON-framed under -json, and a Windows binary name.
func TestParseTestBinaryArgs(t *testing.T) {
	plain := "GOROOT='/go' /go/pkg/tool/link -o $WORK/b001/a.test -importcfg $WORK/b001/importcfg.link\n" +
		"go tool buildid -w $WORK/b001/a.test # internal\n" +
		"$WORK/b001/a.test -test.testlogfile=$WORK/b001/testlog.txt -test.paniconexit0 -test.gocoverdir=$WORK/b001/gocoverdir -test.timeout=10m0s -test.short=true -update -foo\n" +
		"rm -rf $WORK/b001/\n"
	jsonFramed := `{"ImportPath":"m/a.test","Action":"build-output","Output":"GOROOT='/go' link -o $WORK/b001/a.test\n"}` + "\n" +
		`{"ImportPath":"m/a","Action":"build-output","Output":"$WORK/b001/a.test -test.paniconexit0 -test.v=test2json -test.timeout=10m0s\n"}` + "\n"
	cases := []struct {
		name, out string
		want      []string
	}{
		{"plain", plain, []string{"-test.paniconexit0", "-test.short=true", "-update", "-foo"}},
		{"json", jsonFramed, []string{"-test.paniconexit0"}},
		{"windows", `$WORK\b001\a.test.exe -test.short=true` + "\n", []string{"-test.short=true"}},
		{"not a test binary", "$WORK/b001/tool -x\n$WORK/b001/a.test -test.v=true\n", []string{"-test.v=true"}},
		{"not in $WORK", "cp ./a.test x\n./a.test -bogus\n$WORK/b001/a.test -test.v=true\n", []string{"-test.v=true"}},
	}
	for _, c := range cases {
		got, err := parseTestBinaryArgs(c.out)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%s: parseTestBinaryArgs = (%q, %v), want %q", c.name, got, err, c.want)
		}
	}
	if _, err := parseTestBinaryArgs("\nrm -rf $WORK\n"); err == nil {
		t.Error("output without a test binary command: want an error")
	}
}

// TestBuildFlags: only build flags reach `go test -c`, with a value in the
// next field kept alongside, and nothing past -args or `--`.
func TestBuildFlags(t *testing.T) {
	cases := []struct {
		in, want []string
	}{
		{nil, nil},
		{[]string{"-short", "-rapid.checks=100", "-v"}, nil},
		{[]string{"-race", "--trimpath", "-tags=a,b", "-gcflags", "all=-N"}, []string{"-race", "--trimpath", "-tags=a,b", "-gcflags", "all=-N"}},
		{[]string{"-custom", "x", "-race", "-count", "1", "-ldflags=-s"}, []string{"-race", "-ldflags=-s"}},
		{[]string{"-test.race", "-n", "-cover", "-covermode=atomic"}, nil},
		{[]string{"-race", "-args", "-msan"}, []string{"-race"}},
		{[]string{"-tags", "-args", "-msan"}, []string{"-tags", "-args", "-msan"}},
		{[]string{"--", "-race"}, nil},
		{[]string{"-tags"}, []string{"-tags"}},
		{[]string{"--args", "-race"}, nil},
		{[]string{"xrace", "-race"}, []string{"-race"}},
	}
	for _, c := range cases {
		if got := BuildFlags(c.in); !slices.Equal(got, c.want) {
			t.Errorf("BuildFlags(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestSetTestArgs: no flags means no `go test -n` at all; with flags the
// arguments are read once, for the smallest import path, and given to
// every binary; a failure to read them fails the build.
func TestSetTestArgs(t *testing.T) {
	orig := testBinaryArgsFunc
	t.Cleanup(func() { testBinaryArgsFunc = orig })
	var calls []string
	testBinaryArgsFunc = func(_ context.Context, _, tags, pkg string, flags []string) ([]string, error) {
		calls = append(calls, pkg+" "+tags+" "+strings.Join(flags, " "))
		return []string{"-test.short=true"}, nil
	}
	bins := func() map[string]*compiledPkg {
		return map[string]*compiledPkg{"m/b": {importPath: "m/b"}, "m/a": {importPath: "m/a"}, "m/c": {importPath: "m/c"}}
	}

	b := bins()
	if err := setTestArgs(context.Background(), "", BuildOptions{}, b); err != nil || len(calls) != 0 || b["m/a"].testArgs != nil {
		t.Errorf("no flags: err %v, calls %v, args %v; want nothing done", err, calls, b["m/a"].testArgs)
	}
	if err := setTestArgs(context.Background(), "", BuildOptions{TestFlags: []string{"-short"}}, nil); err != nil || len(calls) != 0 {
		t.Errorf("no binaries: err %v, calls %v; want nothing done", err, calls)
	}

	b = bins()
	if err := setTestArgs(context.Background(), "", BuildOptions{Tags: "t", TestFlags: []string{"-short"}}, b); err != nil {
		t.Fatalf("setTestArgs: %v", err)
	}
	if !slices.Equal(calls, []string{"m/a t -short"}) {
		t.Errorf("go test -n calls = %q, want one for m/a with the tags and flags", calls)
	}
	for pkg, cp := range b {
		if !slices.Equal(cp.testArgs, []string{"-test.short=true"}) {
			t.Errorf("%s testArgs = %q, want the read arguments", pkg, cp.testArgs)
		}
	}

	boom := errors.New("boom")
	testBinaryArgsFunc = func(context.Context, string, string, string, []string) ([]string, error) { return nil, boom }
	ran := stubBuildTestMapDeps(t, nil, []resolvedPkg{{importPath: "m/a", dir: t.TempDir()}})
	if _, err := BuildTestMap(context.Background(), "", []string{"./..."}, BuildOptions{TmpDir: t.TempDir(), Workers: 1, TestFlags: []string{"-short"}}); !errors.Is(err, boom) || !strings.Contains(err.Error(), "--test-flags") {
		t.Errorf("BuildTestMap error = %v, want the wrapped failure", err)
	}
	if atomic.LoadInt32(ran) != 0 {
		t.Error("tests ran after the test-binary arguments couldn't be read")
	}
}

// TestProcessWorkForwardsTinyDurations: any positive duration is real work
// a mutant run will spend, so a 1ns run with no blocks is still forwarded.
func TestProcessWorkForwardsTinyDurations(t *testing.T) {
	orig := runCompiledTestFunc
	t.Cleanup(func() { runCompiledTestFunc = orig })
	runCompiledTestFunc = func(context.Context, *compiledPkg, string, string, time.Duration) ([]Block, time.Duration, error) {
		return nil, time.Nanosecond, nil
	}

	work := make(chan testEntry, 1)
	results := make(chan testCoverage, 1)
	work <- testEntry{name: "TestA", pkg: "pkg1"}
	close(work)
	processWork(context.Background(), work, map[string]*compiledPkg{"pkg1": {binPath: "x", importPath: "pkg1"}}, filepath.Join(t.TempDir(), "p.cov"), 0, nil, results)
	close(results)

	if got := <-results; got.testName != "TestA" || got.duration != time.Nanosecond {
		t.Errorf("result = %+v, want TestA's 1ns duration forwarded", got)
	}
}

// TestStalledPkgs: a package is stalled once added, others aren't, and a
// nil set records nothing.
func TestStalledPkgs(t *testing.T) {
	s := &stalledPkgs{set: make(map[string]bool)}
	s.add("p")
	if !s.has("p") || s.has("q") {
		t.Errorf("has(p)=%v has(q)=%v, want true, false", s.has("p"), s.has("q"))
	}
	var none *stalledPkgs
	none.add("p")
	if none.has("p") {
		t.Error("a nil stalledPkgs reported a stalled package")
	}
}

// TestBuildTestMapReadsTestDepsOnlyWhenNeeded: the links are read only
// when they can matter — with -coverpkg.
func TestBuildTestMapReadsTestDepsOnlyWhenNeeded(t *testing.T) {
	for coverPkg, want := range map[string]int{"": 0, "./...": 1} {
		stubBuildTestMapDeps(t, nil, []resolvedPkg{{importPath: "pkg.a", dir: t.TempDir()}})
		calls := 0
		testDepsFunc = func(context.Context, string, BuildOptions, []string) (map[string]map[string]bool, error) {
			calls++
			return nil, nil
		}
		if _, err := BuildTestMap(context.Background(), t.TempDir(), []string{"./..."}, BuildOptions{CoverPkg: coverPkg, TmpDir: t.TempDir(), Workers: 1}); err != nil {
			t.Fatalf("coverpkg=%q: BuildTestMap: %v", coverPkg, err)
		}
		if calls != want {
			t.Errorf("coverpkg=%q: testDeps called %d times, want %d", coverPkg, calls, want)
		}
	}
}
