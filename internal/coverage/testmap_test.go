package coverage

import (
	"context"
	"errors"
	"os"
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
	runCompiledTestFunc = func(_ context.Context, _ *compiledPkg, _, _ string) ([]Block, time.Duration) {
		return nil, 50 * time.Millisecond
	}

	work := make(chan testEntry, 1)
	results := make(chan testCoverage, 1)
	work <- testEntry{name: "TestNoCovButSlow", pkg: "p"}
	close(work)

	pkgBins := map[string]*compiledPkg{
		"p": {binPath: "x", importPath: "p", dir: t.TempDir()},
	}
	processWork(context.Background(), work, pkgBins, t.TempDir(), 0, results)
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
	processWork(ctx, work, map[string]*compiledPkg{}, t.TempDir(), 0, results)
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

	processWork(ctx, work, map[string]*compiledPkg{}, t.TempDir(), 0, results)
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
	runCompiledTestFunc = func(_ context.Context, _ *compiledPkg, _, _ string) ([]Block, time.Duration) {
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
	processWork(ctx, work, pkgBins, t.TempDir(), 0, results)
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
	runCompiledTestFunc = func(_ context.Context, _ *compiledPkg, testName, _ string) ([]Block, time.Duration) {
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
	processWork(context.Background(), work, pkgBins, t.TempDir(), 0, results)
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
	runCompiledTestFunc = func(_ context.Context, cp *compiledPkg, _, _ string) ([]Block, time.Duration) {
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
	processWork(context.Background(), work, pkgBins, t.TempDir(), 0, results)
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
	runCompiledTestFunc = func(_ context.Context, _ *compiledPkg, _, _ string) ([]Block, time.Duration) {
		return nil, 0 // simulate test that produced no coverage blocks
	}

	work := make(chan testEntry, 1)
	results := make(chan testCoverage, 1)
	work <- testEntry{name: "TestNoCoverage", pkg: "pkg1"}
	close(work)

	pkgBins := map[string]*compiledPkg{
		"pkg1": {binPath: "x", importPath: "pkg1", dir: t.TempDir()},
	}
	processWork(context.Background(), work, pkgBins, t.TempDir(), 0, results)
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
// is what makes the mutant observable.
func TestBuildPkgBinsSkipsCompileFailure(t *testing.T) {
	orig := compileTestBinaryFunc
	defer func() { compileTestBinaryFunc = orig }()
	compileTestBinaryFunc = func(_ context.Context, _, _, _ string, _ string, pkg resolvedPkg) (*compiledPkg, error) {
		if pkg.importPath == "fail" {
			return nil, errors.New("compile failed")
		}
		return &compiledPkg{importPath: pkg.importPath, binPath: "x", dir: pkg.dir}, nil
	}

	bins := buildPkgBins(context.Background(), "", "", "", "", []resolvedPkg{
		{importPath: "fail"},
		{importPath: "ok"},
	})

	if _, ok := bins["fail"]; ok {
		t.Errorf("pkgBins should not contain failed pkg — BRANCH_IF on the err-continue body lets nil entries through; got bins=%v", bins)
	}
	if _, ok := bins["ok"]; !ok {
		t.Errorf("pkgBins should contain ok pkg, got bins=%v", bins)
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
	statFileFunc = func(string) (os.FileInfo, error) {
		return nil, errors.New("injected missing-binary")
	}

	cp, err := compileTestBinary(context.Background(), dir, tmpDir, "", "", resolvedPkg{importPath: "testmod", dir: dir})
	if err == nil {
		t.Errorf("expected error when statFileFunc fails, got nil with cp=%+v — BRANCH_IF on the missing-binary return elides the early exit", cp)
	}
	if err != nil && !strings.Contains(err.Error(), "test binary missing") {
		t.Errorf("error should wrap missing-binary failure, got: %v", err)
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
	blocks, _ := runCompiledTest(context.Background(), cp, "TestNeedsCwd", profilePath)
	if len(blocks) == 0 {
		t.Errorf("expected coverage blocks; the test fails when cwd != cp.dir, so STATEMENT_REMOVE on `cmd.Dir = cp.dir` would zero this out")
	}
}

// TestBuildTestMapContinuesPastFailedCompile kills INVERT_LOOP_CTRL on the
// `continue` after a compile failure in BuildTestMap. Stub
// compileTestBinaryFunc to fail for the first package and succeed for the
// second; assert the second package's tests still drive runCompiledTestFunc.
func TestBuildTestMapContinuesPastFailedCompile(t *testing.T) {
	origCompile := compileTestBinaryFunc
	origResolve := resolvePackagesFunc
	origList := listTestsFunc
	origRun := runCompiledTestFunc
	defer func() {
		compileTestBinaryFunc = origCompile
		resolvePackagesFunc = origResolve
		listTestsFunc = origList
		runCompiledTestFunc = origRun
	}()

	resolvePackagesFunc = func(_ context.Context, _ string, _ []string, _ string) ([]resolvedPkg, error) {
		return []resolvedPkg{
			{importPath: "pkg.fail", dir: t.TempDir()},
			{importPath: "pkg.ok", dir: t.TempDir()},
		}, nil
	}
	listTestsFunc = func(_ context.Context, _ string, _ []string, _ string) ([]testEntry, error) {
		return []testEntry{{name: "TestA", pkg: "pkg.ok"}}, nil
	}
	compileTestBinaryFunc = func(_ context.Context, _, _, _ string, _ string, pkg resolvedPkg) (*compiledPkg, error) {
		if pkg.importPath == "pkg.fail" {
			return nil, errors.New("compile failed")
		}
		return &compiledPkg{binPath: "x", importPath: pkg.importPath, dir: pkg.dir}, nil
	}
	var ranTests int32
	runCompiledTestFunc = func(_ context.Context, cp *compiledPkg, _, _ string) ([]Block, time.Duration) {
		atomic.AddInt32(&ranTests, 1)
		if cp.importPath != "pkg.ok" {
			t.Errorf("runCompiledTestFunc invoked with wrong pkg %q", cp.importPath)
		}
		return nil, 0
	}

	_, err := BuildTestMap(context.Background(), t.TempDir(), []string{"./..."}, "", "", t.TempDir(), 1)
	if err != nil {
		t.Fatalf("BuildTestMap: %v", err)
	}
	if got := atomic.LoadInt32(&ranTests); got != 1 {
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
	listTestsFunc = func(_ context.Context, _ string, _ []string, _ string) ([]testEntry, error) {
		return tests, nil
	}
	compileTestBinaryFunc = func(_ context.Context, _, _, _ string, _ string, pkg resolvedPkg) (*compiledPkg, error) {
		return &compiledPkg{binPath: "x", importPath: pkg.importPath, dir: pkg.dir}, nil
	}
	var ran int32
	runCompiledTestFunc = func(_ context.Context, _ *compiledPkg, _, _ string) ([]Block, time.Duration) {
		atomic.AddInt32(&ran, 1)
		return nil, time.Millisecond
	}
	return &ran
}

// TestBuildTestMapErrorsWhenNoListedTestHasBin pins the #105 diagnostic:
// tests listed under a package no compiled binary is keyed by (the pattern
// `./...` vs the import path) must surface as an error naming that package,
// not as a silently empty map. Kills BRANCH_IF / CONDITIONALS_NEGATION on
// the guard and STATEMENT_REMOVE on its return; the run counter kills a
// guard moved after the worker pool. A single listed test kills
// INTEGER_INCREMENT on `len(tests) > 0` (→ `> 1`).
func TestBuildTestMapErrorsWhenNoListedTestHasBin(t *testing.T) {
	ran := stubBuildTestMapDeps(t,
		[]testEntry{{name: "TestA", pkg: "./..."}},
		[]resolvedPkg{{importPath: "example.com/x", dir: t.TempDir()}})

	tm, err := BuildTestMap(context.Background(), t.TempDir(), []string{"./..."}, "", "", t.TempDir(), 1)
	if err == nil {
		t.Fatal("BuildTestMap returned nil error with no listed test matching a compiled binary — per-test routing would switch off silently")
	}
	if tm != nil {
		t.Errorf("BuildTestMap returned a non-nil map alongside the error: %+v", tm)
	}
	if !strings.Contains(err.Error(), "./...") || !strings.Contains(err.Error(), "1 listed tests") {
		t.Errorf("error %q should name the unmatched package and the listed-test count", err)
	}
	if got := atomic.LoadInt32(ran); got != 0 {
		t.Errorf("runCompiledTestFunc called %d times, want 0 — nothing can be mapped, so no test should run", got)
	}
}

// TestBuildTestMapNoListedTestsIsNotAnError kills EXPRESSION_REMOVE on the
// `len(tests) > 0` operand of the no-binary guard: a module without tests
// lists nothing, and that must stay a successful (empty) build.
func TestBuildTestMapNoListedTestsIsNotAnError(t *testing.T) {
	stubBuildTestMapDeps(t, nil, []resolvedPkg{{importPath: "example.com/x", dir: t.TempDir()}})

	tm, err := BuildTestMap(context.Background(), t.TempDir(), []string{"./..."}, "", "", t.TempDir(), 1)
	if err != nil {
		t.Fatalf("BuildTestMap with no listed tests: %v", err)
	}
	if tm == nil {
		t.Fatal("BuildTestMap returned a nil map with no listed tests")
	}
}

// TestBuildTestMapAnyMatchingBinIsEnough kills INVERT_LOOP_CTRL and
// return-value mutations in anyTestHasBin: one unmatched test listed ahead
// of a matched one must not trip the guard, and the matched test runs.
func TestBuildTestMapAnyMatchingBinIsEnough(t *testing.T) {
	ran := stubBuildTestMapDeps(t,
		[]testEntry{{name: "TestStray", pkg: "example.com/gone"}, {name: "TestA", pkg: "example.com/x"}},
		[]resolvedPkg{{importPath: "example.com/x", dir: t.TempDir()}})

	if _, err := BuildTestMap(context.Background(), t.TempDir(), []string{"./..."}, "", "", t.TempDir(), 1); err != nil {
		t.Fatalf("BuildTestMap: %v — one test with a binary must keep per-test routing on", err)
	}
	if got := atomic.LoadInt32(ran); got != 1 {
		t.Errorf("runCompiledTestFunc called %d times, want 1 (TestA only)", got)
	}
}

// TestListedPkgsDistinctSorted pins the diagnostic's package list: each
// package once, sorted, so the message is stable across runs.
func TestListedPkgsDistinctSorted(t *testing.T) {
	got := listedPkgs([]testEntry{
		{name: "T1", pkg: "b"}, {name: "T2", pkg: "a"}, {name: "T3", pkg: "b"},
	})
	if strings.Join(got, ",") != "a,b" {
		t.Errorf("listedPkgs = %v, want [a b]", got)
	}
}

// TestParseListTestsOutputContinuesPastOk kills INVERT_LOOP_CTRL on the
// `continue` for ok-prefixed/empty lines (testmap.go:253). Mutated to
// `break`, an "ok" status line in the middle of `go test -list` output
// would terminate the scan and drop later test names.
func TestParseListTestsOutputContinuesPastOk(t *testing.T) {
	in := strings.NewReader("TestA\nok  \tpkg\t0.001s\nTestB\nok  \tpkg2\t0.001s\n")
	got := parseListTestsOutput(in)
	if len(got) != 2 || got[0].name != "TestA" || got[1].name != "TestB" {
		t.Errorf("got %+v, want [TestA, TestB] — INVERT_LOOP_CTRL on the ok-line `continue` turns later test discovery into break", got)
	}
}

// TestParseListTestsOutputSkipsEmptyLine kills EXPRESSION_REMOVE on the
// `line == ""` operand of the skip guard. With it replaced by `false`,
// empty lines are no longer skipped and surface as testEntry{Name: ""}.
func TestParseListTestsOutputSkipsEmptyLine(t *testing.T) {
	in := strings.NewReader("TestA\n\nTestB\nok  \tpkg\t0.001s\n")
	got := parseListTestsOutput(in)
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2 (empty line skipped): %+v", len(got), got)
	}
	for _, e := range got {
		if e.name == "" {
			t.Errorf("empty test name leaked through — EXPRESSION_REMOVE replaces `line == \"\"` with false, which lets blank lines fall through")
		}
	}
}

// listOutputMultiPkg is verbatim `go test -list . ./...` output from a probe
// module: a package with tests (a), a second one (b), one whose test file
// declares no tests (emptytests), one whose init prints an "ok"-prefixed
// line (noisy), and one without test files (notests).
const listOutputMultiPkg = "TestA1\n" +
	"TestA2\n" +
	"ok  \texample.com/edge/a\t0.272s\n" +
	"TestB\n" +
	"ok  \texample.com/edge/b\t0.271s\n" +
	"ok  \texample.com/edge/emptytests\t0.273s\n" +
	"ok hello from init\n" +
	"TestNoisy\n" +
	"ok  \texample.com/edge/noisy\t0.273s\n" +
	"?   \texample.com/edge/notests\t[no test files]\n"

// TestParseListTestsOutputAttributesByImportPath is the #105 parser gate:
// every name is attributed to the import path on the summary line that
// follows its block, never to a pattern. Kills BRANCH_IF / CONDITIONALS_
// NEGATION on the summary case (names unattributed or attributed per line)
// and STATEMENT_REMOVE on `pending = pending[:0]` (a's names would leak
// into b, emptytests and noisy).
func TestParseListTestsOutputAttributesByImportPath(t *testing.T) {
	got := parseListTestsOutput(strings.NewReader(listOutputMultiPkg))
	want := []testEntry{
		{name: "TestA1", pkg: "example.com/edge/a"},
		{name: "TestA2", pkg: "example.com/edge/a"},
		{name: "TestB", pkg: "example.com/edge/b"},
		{name: "TestNoisy", pkg: "example.com/edge/noisy"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("parseListTestsOutput =\n  %+v\nwant\n  %+v", got, want)
	}
}

// TestParseListTestsOutputSkipsNoTestFilesLine kills EXPRESSION_REMOVE on
// the `"?"` prefix operand: a no-test-files line printed between two
// blocks would otherwise be buffered and attributed to the next package as
// a test name.
func TestParseListTestsOutputSkipsNoTestFilesLine(t *testing.T) {
	in := strings.NewReader("?   \texample.com/x/none\t[no test files]\n" +
		"TestB\n" +
		"ok  \texample.com/x/b\t0.01s\n")
	got := parseListTestsOutput(in)
	want := []testEntry{{name: "TestB", pkg: "example.com/x/b"}}
	if !slices.Equal(got, want) {
		t.Errorf("got %+v, want %+v — the `?` line leaked into the next package", got, want)
	}
}

// TestParseListTestsOutputSkipsTablessOkNoise kills EXPRESSION_REMOVE on
// the `"ok"` prefix operand: an init-time "ok ..." line has no tabs, so it
// is not a summary, and it must not be buffered as a test name either.
func TestParseListTestsOutputSkipsTablessOkNoise(t *testing.T) {
	in := strings.NewReader("ok hello from init\n" +
		"TestNoisy\n" +
		"ok  \texample.com/x/noisy\t0.01s\n")
	got := parseListTestsOutput(in)
	want := []testEntry{{name: "TestNoisy", pkg: "example.com/x/noisy"}}
	if !slices.Equal(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestParseListTestsOutputDropsUnterminatedNames pins that names with no
// summary line after them are dropped: there is no import path to run them
// under, and attributing them to anything else reintroduces #105.
func TestParseListTestsOutputDropsUnterminatedNames(t *testing.T) {
	in := strings.NewReader("TestA\nok  \texample.com/x/a\t0.01s\nTestOrphan\n")
	got := parseListTestsOutput(in)
	want := []testEntry{{name: "TestA", pkg: "example.com/x/a"}}
	if !slices.Equal(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestListSummaryPkg pins the summary-line recogniser. The bare "ok" case
// kills EXPRESSION_REMOVE on the `len(fields) < 2` guard (fields[1] would
// index out of range); the two-field case kills CONDITIONALS_BOUNDARY
// (`< 2` → `<= 2`); the padded case kills removing the TrimSpace.
func TestListSummaryPkg(t *testing.T) {
	cases := []struct {
		line    string
		wantPkg string
		wantOK  bool
	}{
		{"ok  \texample.com/a\t0.27s", "example.com/a", true},
		{"ok\texample.com/a", "example.com/a", true},
		{"ok", "", false},
		{"ok hello from init", "", false},
		{"?   \texample.com/n\t[no test files]", "", false},
		{"FAIL\texample.com/f\t0.1s", "", false},
		{"TestA", "", false},
	}
	for _, c := range cases {
		pkg, ok := listSummaryPkg(c.line)
		if pkg != c.wantPkg || ok != c.wantOK {
			t.Errorf("listSummaryPkg(%q) = (%q, %v), want (%q, %v)", c.line, pkg, ok, c.wantPkg, c.wantOK)
		}
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
