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

func TestTestMapPackageDuration(t *testing.T) {
	tm := &TestMap{
		pkgDurations: map[string]time.Duration{
			"p": 500 * time.Millisecond,
			"q": time.Second,
		},
	}

	if got := tm.PackageDuration("p"); got != 500*time.Millisecond {
		t.Errorf("PackageDuration(p) = %v, want 500ms", got)
	}
	if got := tm.PackageDuration("missing"); got != 0 {
		t.Errorf("PackageDuration(missing) = %v, want 0", got)
	}

	var nilTm *TestMap
	if got := nilTm.PackageDuration("p"); got != 0 {
		t.Errorf("nil.PackageDuration = %v, want 0", got)
	}
}

// TestTestMapIngestResultUpdatesBothMaps kills STATEMENT_REMOVE on the
// recordDuration call inside the BuildTestMap collect loop. Without
// this assertion any path that drops the duration recording would be
// invisible — the index map still gets populated by addBlocks.
func TestTestMapIngestResultUpdatesBothMaps(t *testing.T) {
	tm := &TestMap{
		index:        map[string]map[testKey]bool{},
		durations:    map[testKey]time.Duration{},
		pkgDurations: map[string]time.Duration{},
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
	if got := tm.pkgDurations["p"]; got != 25*time.Millisecond {
		t.Errorf("pkgDurations not populated; got %v want 25ms", got)
	}
	if !tm.index["f.go:5"][testKey{pkg: "p", name: "TestA"}] {
		t.Errorf("addBlocks side of ingestResult missing the f.go:5 → TestA edge; index=%v", tm.index)
	}
}

func TestTestMapRecordDurationAccumulates(t *testing.T) {
	// Same (pkg, name) recorded twice — the per-package sum and the per-test
	// entry must both accumulate, not overwrite. Mirrors the documented
	// behavior contract on recordDuration.
	tm := &TestMap{
		durations:    map[testKey]time.Duration{},
		pkgDurations: map[string]time.Duration{},
	}
	tm.recordDuration("p", "TestA", 10*time.Millisecond)
	tm.recordDuration("p", "TestA", 5*time.Millisecond)
	tm.recordDuration("p", "TestB", 7*time.Millisecond)

	if got := tm.durations[testKey{pkg: "p", name: "TestA"}]; got != 15*time.Millisecond {
		t.Errorf("durations[p,TestA] = %v, want 15ms (10ms + 5ms)", got)
	}
	if got := tm.pkgDurations["p"]; got != 22*time.Millisecond {
		t.Errorf("pkgDurations[p] = %v, want 22ms", got)
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

	// Per-package sums roll up correctly.
	if got := tm.pkgDurations["p"]; got != 100*time.Millisecond {
		t.Errorf("pkgDurations[p]=%v, want 100ms (30+70)", got)
	}
	if got := tm.pkgDurations["q"]; got != 100*time.Millisecond {
		t.Errorf("pkgDurations[q]=%v, want 100ms", got)
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
	if empty.durations == nil || empty.pkgDurations == nil || empty.index == nil {
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
	runCompiledTestFunc = func(_ context.Context, _ *compiledPkg, _, _ string, _ time.Duration) ([]Block, time.Duration) {
		return nil, 50 * time.Millisecond
	}

	work := make(chan testEntry, 1)
	results := make(chan testCoverage, 1)
	work <- testEntry{name: "TestNoCovButSlow", pkg: "p"}
	close(work)

	pkgBins := map[string]*compiledPkg{
		"p": {binPath: "x", importPath: "p", dir: t.TempDir()},
	}
	processWork(context.Background(), work, pkgBins, t.TempDir(), 0, 0, results)
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
	processWork(ctx, work, map[string]*compiledPkg{}, t.TempDir(), 0, 0, results)
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

	processWork(ctx, work, map[string]*compiledPkg{}, t.TempDir(), 0, 0, results)
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
	runCompiledTestFunc = func(_ context.Context, _ *compiledPkg, _, _ string, _ time.Duration) ([]Block, time.Duration) {
		calls.Add(1)
		return nil, 0
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
	processWork(ctx, work, pkgBins, t.TempDir(), 0, 0, results)
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
	runCompiledTestFunc = func(_ context.Context, _ *compiledPkg, testName, _ string, _ time.Duration) ([]Block, time.Duration) {
		if testName == "TestEmpty" {
			return nil, 0
		}
		return []Block{{File: "f.go", StartLine: 1, EndLine: 1, Count: 1}}, 0
	}

	work := make(chan testEntry, 2)
	results := make(chan testCoverage, 2)
	work <- testEntry{name: "TestEmpty", pkg: "pkg1"}
	work <- testEntry{name: "TestNonEmpty", pkg: "pkg1"}
	close(work)

	pkgBins := map[string]*compiledPkg{
		"pkg1": {binPath: "x", importPath: "pkg1", dir: t.TempDir()},
	}
	processWork(context.Background(), work, pkgBins, t.TempDir(), 0, 0, results)
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
	runCompiledTestFunc = func(_ context.Context, cp *compiledPkg, _, _ string, _ time.Duration) ([]Block, time.Duration) {
		calls.Add(1)
		return nil, 0
	}

	work := make(chan testEntry, 2)
	results := make(chan testCoverage, 1)
	work <- testEntry{name: "TestUnknownPkg", pkg: "missing"}
	work <- testEntry{name: "TestKnownPkg", pkg: "pkg1"}
	close(work)

	pkgBins := map[string]*compiledPkg{
		"pkg1": {binPath: "x", importPath: "pkg1", dir: t.TempDir()},
	}
	processWork(context.Background(), work, pkgBins, t.TempDir(), 0, 0, results)
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
	runCompiledTestFunc = func(_ context.Context, _ *compiledPkg, _, _ string, _ time.Duration) ([]Block, time.Duration) {
		return nil, 0 // simulate test that produced no coverage blocks
	}

	work := make(chan testEntry, 1)
	results := make(chan testCoverage, 1)
	work <- testEntry{name: "TestNoCoverage", pkg: "pkg1"}
	close(work)

	pkgBins := map[string]*compiledPkg{
		"pkg1": {binPath: "x", importPath: "pkg1", dir: t.TempDir()},
	}
	processWork(context.Background(), work, pkgBins, t.TempDir(), 0, 0, results)
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
	compileTestBinaryFunc = func(_ context.Context, _, _, _ string, _ string, pkg resolvedPkg) (*compiledPkg, error) {
		switch pkg.importPath {
		case "fail":
			return nil, compileFailed
		case "notests":
			return nil, fmt.Errorf("test binary missing for notests: %w", errNoTestBinary)
		}
		return &compiledPkg{importPath: pkg.importPath, binPath: "x", dir: pkg.dir}, nil
	}

	bins, failures := buildPkgBins(context.Background(), "", "", "", "", []resolvedPkg{
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
	if len(failures) != 1 || failures[0] != compileFailed {
		t.Errorf("failures = %v, want only the compile failure (a package without test files is not one)", failures)
	}
}

// TestCompileTestBinaryReturnsErrOnRunFailure kills BRANCH_IF on the
// `if err := cmd.Run(); err != nil { return nil, ... }` body in
// compileTestBinary. We pre-stage a stale file at the expected output
// path; `go test -c` against a bogus package fails, so under the original
// the function short-circuits to error. Under mutation it falls through to
// statFileFunc, which finds the stale file and returns *compiledPkg, nil.
func TestCompileTestBinaryReturnsErrOnRunFailure(t *testing.T) {
	tmpDir := t.TempDir()
	pkg := resolvedPkg{importPath: "definitely.not.a.real/pkg/zzz", dir: t.TempDir()}
	binPath := filepath.Join(tmpDir, "testbin-"+sanitize(pkg.importPath)+".test")
	if err := os.WriteFile(binPath, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	cp, err := compileTestBinary(context.Background(), pkg.dir, tmpDir, "", "", pkg)
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

	cp, err := compileTestBinary(context.Background(), dir, tmpDir, "", "", resolvedPkg{importPath: "testmod", dir: dir})
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
	cp, err := compileTestBinary(context.Background(), dir, tmpDir, "", "", pkg)
	if err != nil {
		t.Fatalf("compileTestBinary: %v", err)
	}

	profilePath := filepath.Join(tmpDir, "cwd.cov")
	blocks, _ := runCompiledTest(context.Background(), cp, "TestNeedsCwd", profilePath, 0)
	if len(blocks) == 0 {
		t.Errorf("expected coverage blocks; the test fails when cwd != cp.dir, so STATEMENT_REMOVE on `cmd.Dir = cp.dir` would zero this out")
	}
}

// TestBuildTestMapContinuesPastFailedCompile kills INVERT_LOOP_CTRL on the
// `continue` after a compile failure in buildPkgBins. Stub
// compileTestBinaryFunc to fail for the first package and succeed for the
// second; assert the second package's tests still drive runCompiledTestFunc
// and that a partial failure is not an error.
func TestBuildTestMapContinuesPastFailedCompile(t *testing.T) {
	ran := stubBuildTestMapDeps(t, nil, []resolvedPkg{
		{importPath: "pkg.fail", dir: t.TempDir()},
		{importPath: "pkg.ok", dir: t.TempDir()},
	})
	listTestsFunc = func(_ context.Context, bins map[string]*compiledPkg, _ time.Duration) ([]testEntry, error) {
		var tests []testEntry
		for pkg := range bins {
			tests = append(tests, testEntry{name: "TestA", pkg: pkg})
		}
		return tests, nil
	}
	compileTestBinaryFunc = func(_ context.Context, _, _, _ string, _ string, pkg resolvedPkg) (*compiledPkg, error) {
		if pkg.importPath == "pkg.fail" {
			return nil, errors.New("compile failed")
		}
		return &compiledPkg{binPath: "x", importPath: pkg.importPath, dir: pkg.dir}, nil
	}

	_, err := BuildTestMap(context.Background(), t.TempDir(), []string{"./..."}, "", "", t.TempDir(), 1, 0)
	if err != nil {
		t.Fatalf("BuildTestMap: %v", err)
	}
	if got := atomic.LoadInt32(ran); got != 1 {
		t.Errorf("runCompiledTestFunc called %d times, want 1 — INVERT_LOOP_CTRL turns the compile-failure `continue` into `break`, dropping pkg.ok", got)
	}
}

// stubBuildTestMapDeps swaps BuildTestMap's go-tool seams for stubs: every
// resolved package compiles, listTests returns `tests`, and each compiled
// test run bumps the returned counter. Restored on cleanup.
func stubBuildTestMapDeps(t *testing.T, tests []testEntry, resolved []resolvedPkg) *int32 {
	t.Helper()
	origCompile := compileTestBinaryFunc
	origResolve := resolvePackagesFunc
	origList := listTestsFunc
	origRun := runCompiledTestFunc
	t.Cleanup(func() {
		compileTestBinaryFunc = origCompile
		resolvePackagesFunc = origResolve
		listTestsFunc = origList
		runCompiledTestFunc = origRun
	})

	resolvePackagesFunc = func(_ context.Context, _ string, _ []string, _ string) ([]resolvedPkg, error) {
		return resolved, nil
	}
	listTestsFunc = func(_ context.Context, _ map[string]*compiledPkg, _ time.Duration) ([]testEntry, error) {
		return tests, nil
	}
	compileTestBinaryFunc = func(_ context.Context, _, _, _ string, _ string, pkg resolvedPkg) (*compiledPkg, error) {
		return &compiledPkg{binPath: "x", importPath: pkg.importPath, dir: pkg.dir}, nil
	}
	var ran int32
	runCompiledTestFunc = func(_ context.Context, _ *compiledPkg, _, _ string, _ time.Duration) ([]Block, time.Duration) {
		atomic.AddInt32(&ran, 1)
		return nil, time.Millisecond
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
	compileTestBinaryFunc = func(_ context.Context, _, _, _ string, _ string, pkg resolvedPkg) (*compiledPkg, error) {
		return nil, fmt.Errorf("go test -c %s: %w", pkg.importPath, boom)
	}
	listed := false
	listTestsFunc = func(context.Context, map[string]*compiledPkg, time.Duration) ([]testEntry, error) {
		listed = true
		return nil, nil
	}

	tm, err := BuildTestMap(context.Background(), t.TempDir(), []string{"./..."}, "", "", t.TempDir(), 1, 0)
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
	compileTestBinaryFunc = func(context.Context, string, string, string, string, resolvedPkg) (*compiledPkg, error) {
		return nil, fmt.Errorf("test binary missing: %w", errNoTestBinary)
	}

	tm, err := BuildTestMap(context.Background(), t.TempDir(), []string{"./..."}, "", "", t.TempDir(), 1, 0)
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
	compileTestBinaryFunc = func(context.Context, string, string, string, string, resolvedPkg) (*compiledPkg, error) {
		cancel()
		return nil, errors.New("signal: killed")
	}

	tm, err := BuildTestMap(ctx, t.TempDir(), []string{"./..."}, "", "", t.TempDir(), 1, 0)
	if err != context.Canceled {
		t.Errorf("BuildTestMap error = %v, want context.Canceled", err)
	}
	if tm != nil || atomic.LoadInt32(ran) != 0 {
		t.Errorf("BuildTestMap went on after cancellation: map=%v runs=%d", tm, atomic.LoadInt32(ran))
	}
}

// TestBuildTestMapReportsCancellationDuringListing: same as above for a
// listing killed by cancellation, and an uncancelled listing failure keeps
// its own wrapped error.
func TestBuildTestMapReportsCancellationDuringListing(t *testing.T) {
	listErr := errors.New("signal: killed")
	for _, cancelled := range []bool{true, false} {
		ctx, cancel := context.WithCancel(context.Background())
		stubBuildTestMapDeps(t, nil, []resolvedPkg{{importPath: "example.com/x", dir: t.TempDir()}})
		listTestsFunc = func(context.Context, map[string]*compiledPkg, time.Duration) ([]testEntry, error) {
			if cancelled {
				cancel()
			}
			return nil, listErr
		}

		_, err := BuildTestMap(ctx, t.TempDir(), []string{"./..."}, "", "", t.TempDir(), 1, 0)
		cancel()
		switch {
		case cancelled && err != context.Canceled:
			t.Errorf("cancelled: error = %v, want context.Canceled", err)
		case !cancelled && (!errors.Is(err, listErr) || !strings.Contains(err.Error(), "listing tests")):
			t.Errorf("not cancelled: error = %v, want the wrapped listing failure", err)
		}
	}
}

// TestBuildTestMapForwardsTestTimeout pins that the per-run bound reaches
// both the listing and every per-test run.
func TestBuildTestMapForwardsTestTimeout(t *testing.T) {
	const timeout = 7 * time.Second
	stubBuildTestMapDeps(t, nil, []resolvedPkg{{importPath: "example.com/x", dir: t.TempDir()}})
	var listTimeout, runTimeout time.Duration
	listTestsFunc = func(_ context.Context, _ map[string]*compiledPkg, d time.Duration) ([]testEntry, error) {
		listTimeout = d
		return []testEntry{{name: "TestA", pkg: "example.com/x"}}, nil
	}
	runCompiledTestFunc = func(_ context.Context, _ *compiledPkg, _, _ string, d time.Duration) ([]Block, time.Duration) {
		runTimeout = d
		return nil, time.Millisecond
	}

	if _, err := BuildTestMap(context.Background(), t.TempDir(), []string{"./..."}, "", "", t.TempDir(), 1, timeout); err != nil {
		t.Fatalf("BuildTestMap: %v", err)
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

	tm, err := BuildTestMap(context.Background(), t.TempDir(), []string{"./..."}, "", "", t.TempDir(), 1, 0)
	if err != nil {
		t.Fatalf("BuildTestMap with no listed tests: %v", err)
	}
	if tm == nil {
		t.Fatal("BuildTestMap returned a nil map with no listed tests")
	}
}

// TestParseTestList keeps only names -test.run can select. The input is
// verbatim -test.list output from a probe package whose TestMain printed
// "partial" without a newline (gluing it onto the first name), plus a log
// line and padding. Kills mutations of the filter and the TrimSpace.
func TestParseTestList(t *testing.T) {
	out := "partialTestOne\n" +
		"TestTwo\n" +
		"BenchmarkX\n" +
		"FuzzF\n" +
		"ExampleA\n" +
		"2026/10/07 12:00:00 connecting to db\n" +
		"TestÜber_2\n" +
		"  TestPadded \n" +
		"\n" +
		"Test"
	want := []string{"TestTwo", "FuzzF", "ExampleA", "TestÜber_2", "TestPadded", "Test"}
	if got := parseTestList(out); !slices.Equal(got, want) {
		t.Errorf("parseTestList = %q, want %q", got, want)
	}
}

func TestSanitize(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"github.com/foo/bar", "github_com_foo_bar"},
		{"simple", "simple"},
		{"path/to/pkg", "path_to_pkg"},
		{"with spaces", "with_spaces"},
		{"back\\slash", "back_slash"},
	}
	for _, tc := range tests {
		got := sanitize(tc.input)
		if got != tc.want {
			t.Errorf("sanitize(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
