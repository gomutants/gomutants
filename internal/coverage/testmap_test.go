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
	runCompiledTestFunc = func(_ context.Context, _ *compiledPkg, _, _ string, _ time.Duration) ([]Block, time.Duration, bool, error) {
		return nil, 50 * time.Millisecond, false, nil
	}

	work := make(chan testEntry, 1)
	results := make(chan testCoverage, 1)
	work <- testEntry{name: "TestNoCovButSlow", pkg: "p"}
	close(work)

	pkgBins := map[string]*compiledPkg{
		"p": {binPath: "x", importPath: "p", dir: t.TempDir()},
	}
	processWork(context.Background(), work, pkgBins, t.TempDir(), 0, 0, nil, results)
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
	processWork(ctx, work, map[string]*compiledPkg{}, t.TempDir(), 0, 0, nil, results)
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

	processWork(ctx, work, map[string]*compiledPkg{}, t.TempDir(), 0, 0, nil, results)
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
	runCompiledTestFunc = func(_ context.Context, _ *compiledPkg, _, _ string, _ time.Duration) ([]Block, time.Duration, bool, error) {
		calls.Add(1)
		return nil, 0, false, nil
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
	processWork(ctx, work, pkgBins, t.TempDir(), 0, 0, nil, results)
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
	runCompiledTestFunc = func(_ context.Context, _ *compiledPkg, testName, _ string, _ time.Duration) ([]Block, time.Duration, bool, error) {
		if testName == "TestEmpty" {
			return nil, 0, false, nil
		}
		return []Block{{File: "f.go", StartLine: 1, EndLine: 1, Count: 1}}, 0, false, nil
	}

	work := make(chan testEntry, 2)
	results := make(chan testCoverage, 2)
	work <- testEntry{name: "TestEmpty", pkg: "pkg1"}
	work <- testEntry{name: "TestNonEmpty", pkg: "pkg1"}
	close(work)

	pkgBins := map[string]*compiledPkg{
		"pkg1": {binPath: "x", importPath: "pkg1", dir: t.TempDir()},
	}
	processWork(context.Background(), work, pkgBins, t.TempDir(), 0, 0, nil, results)
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
	runCompiledTestFunc = func(_ context.Context, cp *compiledPkg, _, _ string, _ time.Duration) ([]Block, time.Duration, bool, error) {
		calls.Add(1)
		return nil, 0, false, nil
	}

	work := make(chan testEntry, 2)
	results := make(chan testCoverage, 1)
	work <- testEntry{name: "TestUnknownPkg", pkg: "missing"}
	work <- testEntry{name: "TestKnownPkg", pkg: "pkg1"}
	close(work)

	pkgBins := map[string]*compiledPkg{
		"pkg1": {binPath: "x", importPath: "pkg1", dir: t.TempDir()},
	}
	processWork(context.Background(), work, pkgBins, t.TempDir(), 0, 0, nil, results)
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
	runCompiledTestFunc = func(_ context.Context, _ *compiledPkg, _, _ string, _ time.Duration) ([]Block, time.Duration, bool, error) {
		return nil, 0, false, nil // simulate test that produced no coverage blocks
	}

	work := make(chan testEntry, 1)
	results := make(chan testCoverage, 1)
	work <- testEntry{name: "TestNoCoverage", pkg: "pkg1"}
	close(work)

	pkgBins := map[string]*compiledPkg{
		"pkg1": {binPath: "x", importPath: "pkg1", dir: t.TempDir()},
	}
	processWork(context.Background(), work, pkgBins, t.TempDir(), 0, 0, nil, results)
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
	blocks, _, _, _ := runCompiledTest(context.Background(), cp, "TestNeedsCwd", profilePath, 0)
	if len(blocks) == 0 {
		t.Errorf("expected coverage blocks; the test fails when cwd != cp.dir, so STATEMENT_REMOVE on `cmd.Dir = cp.dir` would zero this out")
	}
}

// TestBuildTestMapContinuesPastFailedCompile kills INVERT_LOOP_CTRL on the
// `continue` after a compile failure in buildPkgBins. Stub
// compileTestBinaryFunc to fail for the first package and succeed for the
// second; assert the second package's tests still drive runCompiledTestFunc
// and that a partial failure is not an error but unmaps the failed package.
func TestBuildTestMapContinuesPastFailedCompile(t *testing.T) {
	failDir := t.TempDir()
	ran := stubBuildTestMapDeps(t, nil, []resolvedPkg{
		{importPath: "pkg.fail", dir: failDir},
		{importPath: "pkg.ok", dir: t.TempDir()},
	})
	listTestsFunc = func(_ context.Context, bins map[string]*compiledPkg, _ time.Duration, _ int) ([]testEntry, map[string]string) {
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
	// pkg.fail's tests can't be mapped, so its mutants must run it in full
	// rather than route to nothing.
	got := tm.Unmapped()
	if len(got) != 1 || got[0].ImportPath != "pkg.fail" || got[0].Dir != failDir || got[0].Reason != "its test binary failed to compile" {
		t.Errorf("Unmapped() = %+v, want only pkg.fail with its dir and the compile reason", got)
	}
}

// TestBuildTestMapCrossPkgFollowsCoverPkg: an unmapped package's tests can
// cover a mutant elsewhere only when the binaries were built with
// -coverpkg, so only then does FullRunPkgs return it for another package.
func TestBuildTestMapCrossPkgFollowsCoverPkg(t *testing.T) {
	for _, coverPkg := range []string{"", "./..."} {
		stubBuildTestMapDeps(t, nil, []resolvedPkg{
			{importPath: "pkg.fail", dir: t.TempDir()},
			{importPath: "pkg.ok", dir: t.TempDir()},
		})
		compileTestBinaryFunc = func(_ context.Context, _ string, _ BuildOptions, pkg resolvedPkg) (*compiledPkg, error) {
			if pkg.importPath == "pkg.fail" {
				return nil, errors.New("compile failed")
			}
			return &compiledPkg{binPath: "x", importPath: pkg.importPath, dir: pkg.dir}, nil
		}

		tm, err := BuildTestMap(context.Background(), t.TempDir(), []string{"./..."}, BuildOptions{CoverPkg: coverPkg, TmpDir: t.TempDir(), Workers: 1})
		if err != nil {
			t.Fatalf("BuildTestMap(coverpkg=%q): %v", coverPkg, err)
		}
		got := len(tm.FullRunPkgs("pkg.ok"))
		if want := map[string]int{"": 0, "./...": 1}[coverPkg]; got != want {
			t.Errorf("coverpkg=%q: FullRunPkgs(pkg.ok) has %d packages, want %d", coverPkg, got, want)
		}
	}
}

// TestBuildTestMapReadsTestDeps: with -coverpkg, an unmapped package runs
// in full only for mutants in packages its test binary links, read once
// for all unmapped packages with the tags.
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
	testDepsFunc = func(_ context.Context, _, tags string, pkgs []string) (map[string]map[string]bool, error) {
		calls = append(calls, tags+" "+strings.Join(pkgs, " "))
		return map[string]map[string]bool{"pkg.fail": {"pkg.dep": true}}, nil
	}

	tm, err := BuildTestMap(context.Background(), t.TempDir(), []string{"./..."}, BuildOptions{CoverPkg: "./...", Tags: "t", TmpDir: t.TempDir(), Workers: 1})
	if err != nil {
		t.Fatalf("BuildTestMap: %v", err)
	}
	if !slices.Equal(calls, []string{"t pkg.fail"}) {
		t.Errorf("testDeps calls = %q, want one for pkg.fail with the tags", calls)
	}
	if got := tm.FullRunPkgs("pkg.dep"); len(got) != 1 || got[0].ImportPath != "pkg.fail" {
		t.Errorf("FullRunPkgs(pkg.dep) = %+v, want pkg.fail, whose tests link it", got)
	}
	if got := tm.FullRunPkgs("pkg.ok"); len(got) != 0 {
		t.Errorf("FullRunPkgs(pkg.ok) = %+v, want none: pkg.fail's tests don't link it", got)
	}
}

// TestFullRunPkgs pins which unmapped packages run in full for a mutant:
// its own package without cross-package coverage; with it, every unmapped
// package (sorted) whose tests link the mutant's package or whose links
// are unknown; and nothing on a nil map.
func TestFullRunPkgs(t *testing.T) {
	a := UnmappedPkg{ImportPath: "m/a", Dir: "/a", Reason: "ra"}
	b := UnmappedPkg{ImportPath: "m/b", Dir: "/b", Reason: "rb"}
	c := UnmappedPkg{ImportPath: "m/c", Dir: "/c", Reason: "rc"}
	own := NewTestMapForTesting(nil, nil).WithUnmappedForTesting(false, c, a)
	cross := NewTestMapForTesting(nil, nil).WithUnmappedForTesting(true, c, a, b)
	linked := NewTestMapForTesting(nil, nil).WithUnmappedForTesting(true, c, a, b)
	linked.testDeps = map[string]map[string]bool{"m/a": {"m/x": true}, "m/b": {"m/y": true}}

	cases := []struct {
		name string
		tm   *TestMap
		pkg  string
		want []UnmappedPkg
	}{
		{"nil map", nil, "m/a", nil},
		{"own package unmapped", own, "m/a", []UnmappedPkg{a}},
		{"own package mapped, others ignored", own, "m/b", nil},
		{"cross-package: every unmapped package, sorted", cross, "m/z", []UnmappedPkg{a, b, c}},
		{"cross-package: linking packages and unknown links", linked, "m/x", []UnmappedPkg{a, c}},
		{"cross-package: own package even when not linked", linked, "m/b", []UnmappedPkg{b, c}},
	}
	for _, tc := range cases {
		if got := tc.tm.FullRunPkgs(tc.pkg); !slices.Equal(got, tc.want) {
			t.Errorf("%s: FullRunPkgs(%q) = %+v, want %+v", tc.name, tc.pkg, got, tc.want)
		}
	}
	if got := (*TestMap)(nil).Unmapped(); got != nil {
		t.Errorf("nil map: Unmapped() = %+v, want nil", got)
	}
}

// TestMarkUnmappedKeepsEarliestReason: of a package's reasons, the one
// from the test listed first is kept whatever order they arrive in, and a
// reason about the whole package (order -1) beats any test's.
func TestMarkUnmappedKeepsEarliestReason(t *testing.T) {
	tm := newTestMap(false)
	steps := []struct {
		reason string
		order  int
		want   string
	}{
		{"third", 3, "third"},
		{"first", 1, "first"},
		{"second", 2, "first"},
		{"again", 1, "first"},
		{"package", -1, "package"},
	}
	for _, st := range steps {
		tm.markUnmapped("m/a", "/a", st.reason, st.order)
		if got := tm.Unmapped(); len(got) != 1 || got[0].Reason != st.want {
			t.Errorf("after %q at %d: Unmapped() = %+v, want reason %q", st.reason, st.order, got, st.want)
		}
	}
}

// TestUnmappedIsACopy: a caller changing what Unmapped returns must not
// change the map, which workers read without locks.
func TestUnmappedIsACopy(t *testing.T) {
	tm := newTestMap(true)
	tm.markUnmapped("m/a", "/a", "r", -1)
	tm.Unmapped()[0].Reason = "changed"
	if got := tm.FullRunPkgs("m/x"); len(got) != 1 || got[0].Reason != "r" {
		t.Errorf("FullRunPkgs = %+v, want m/a's reason unchanged", got)
	}
}

// TestWithUnmappedForTestingCopies: the test helper leaves the map it is
// called on as it was.
func TestWithUnmappedForTestingCopies(t *testing.T) {
	base := NewTestMapForTesting(nil, nil)
	got := base.WithUnmappedForTesting(true, UnmappedPkg{ImportPath: "m/a"})
	if len(got.Unmapped()) != 1 || !got.crossPkg {
		t.Errorf("copy: Unmapped() = %+v, crossPkg %v; want m/a, cross-package", got.Unmapped(), got.crossPkg)
	}
	if len(base.Unmapped()) != 0 || base.crossPkg {
		t.Errorf("original: Unmapped() = %+v, crossPkg %v; want it unchanged", base.Unmapped(), base.crossPkg)
	}
}

// TestProcessWorkSkipsAfterEarlierFailure: once a test fails alone, its
// package's tests listed after it don't run, while those listed before it
// still do — one of them may be the earlier failure to report.
func TestProcessWorkSkipsAfterEarlierFailure(t *testing.T) {
	orig := runCompiledTestFunc
	t.Cleanup(func() { runCompiledTestFunc = orig })
	var ran []string
	runCompiledTestFunc = func(_ context.Context, _ *compiledPkg, name, _ string, _ time.Duration) ([]Block, time.Duration, bool, error) {
		ran = append(ran, name)
		if name == "TestB" {
			return nil, time.Millisecond, false, errors.New("TestB failed when run alone")
		}
		return nil, time.Millisecond, false, nil
	}

	entries := []testEntry{
		{name: "TestB", pkg: "p", order: 1},
		{name: "TestC", pkg: "p", order: 2},
		{name: "TestA", pkg: "p", order: 0},
		{name: "TestX", pkg: "q", order: 5},
	}
	work := make(chan testEntry, len(entries))
	for _, e := range entries {
		work <- e
	}
	close(work)
	results := make(chan testCoverage, len(entries))
	failed := &firstFailures{at: make(map[string]int)}
	processWork(context.Background(), work, map[string]*compiledPkg{"p": {}, "q": {}}, t.TempDir(), 0, 0, failed, results)
	close(results)

	if want := []string{"TestB", "TestA", "TestX"}; !slices.Equal(ran, want) {
		t.Errorf("ran %v, want %v", ran, want)
	}
	var orders []int
	for tc := range results {
		orders = append(orders, tc.order)
	}
	if want := []int{1, 0, 5}; !slices.Equal(orders, want) {
		t.Errorf("result orders = %v, want each run's listing order %v", orders, want)
	}
}

// TestSkipScanner reads skip lines from -test.v output however it is
// split into writes: top-level skips only, with an overlong line cut
// rather than kept whole.
func TestSkipScanner(t *testing.T) {
	out := "=== RUN   TestA\n--- SKIP: TestA (0.00s)\n    x_test.go:3: not ready\n" +
		"=== RUN   TestB\n    --- SKIP: TestB/sub (0.00s)\n--- PASS: TestB (0.00s)\n" +
		strings.Repeat("y", 3*maxScannedLine) + "\n--- SKIP: TestC (0.01s)\n--- SKIP: TestD"
	for _, size := range []int{1, 7, len(out)} {
		var s skipScanner
		for chunk := range slices.Chunk([]byte(out), size) {
			if n, err := s.Write(chunk); n != len(chunk) || err != nil {
				t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len(chunk))
			}
		}
		if want := []string{"TestA", "TestC"}; !slices.Equal(s.skipped, want) {
			t.Errorf("writes of %d: skipped = %q, want %q", size, s.skipped, want)
		}
		if len(s.line) > maxScannedLine {
			t.Errorf("writes of %d: kept %d bytes of one line", size, len(s.line))
		}
	}
}

// stubBuildTestMapDeps swaps BuildTestMap's go-tool seams for stubs: every
// resolved package compiles, listTests returns `tests`, each compiled test
// run bumps the returned counter, and the test deps can't be read.
// Restored on cleanup.
func stubBuildTestMapDeps(t *testing.T, tests []testEntry, resolved []resolvedPkg) *int32 {
	t.Helper()
	origCompile := compileTestBinaryFunc
	origResolve := resolvePackagesFunc
	origList := listTestsFunc
	origRun := runCompiledTestFunc
	origDeps := testDepsFunc
	t.Cleanup(func() {
		compileTestBinaryFunc = origCompile
		resolvePackagesFunc = origResolve
		listTestsFunc = origList
		runCompiledTestFunc = origRun
		testDepsFunc = origDeps
	})
	testDepsFunc = func(context.Context, string, string, []string) (map[string]map[string]bool, error) {
		return nil, errors.New("deps not stubbed")
	}

	resolvePackagesFunc = func(_ context.Context, _ string, _ []string, _ string) ([]resolvedPkg, error) {
		return resolved, nil
	}
	listTestsFunc = func(context.Context, map[string]*compiledPkg, time.Duration, int) ([]testEntry, map[string]string) {
		return tests, nil
	}
	compileTestBinaryFunc = func(_ context.Context, _ string, _ BuildOptions, pkg resolvedPkg) (*compiledPkg, error) {
		return &compiledPkg{binPath: "x", importPath: pkg.importPath, dir: pkg.dir}, nil
	}
	var ran int32
	runCompiledTestFunc = func(_ context.Context, _ *compiledPkg, _, _ string, _ time.Duration) ([]Block, time.Duration, bool, error) {
		atomic.AddInt32(&ran, 1)
		return nil, time.Millisecond, false, nil
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
	listTestsFunc = func(context.Context, map[string]*compiledPkg, time.Duration, int) ([]testEntry, map[string]string) {
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
// listing killed by cancellation.
func TestBuildTestMapReportsCancellationDuringListing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ran := stubBuildTestMapDeps(t, nil, []resolvedPkg{{importPath: "example.com/x", dir: t.TempDir()}})
	listTestsFunc = func(context.Context, map[string]*compiledPkg, time.Duration, int) ([]testEntry, map[string]string) {
		cancel()
		return []testEntry{{name: "TestA", pkg: "example.com/x"}}, map[string]string{"example.com/x": "listing its tests failed: signal: killed"}
	}

	tm, err := BuildTestMap(ctx, t.TempDir(), []string{"./..."}, BuildOptions{TmpDir: t.TempDir(), Workers: 1})
	if err != context.Canceled {
		t.Errorf("BuildTestMap error = %v, want context.Canceled", err)
	}
	if tm != nil || atomic.LoadInt32(ran) != 0 {
		t.Errorf("BuildTestMap went on after cancellation: map=%v runs=%d", tm, atomic.LoadInt32(ran))
	}
}

// TestBuildTestMapUnmapsListingFailures: a package whose listing failed is
// unmapped with the listing's reason, and the rest of the map still builds.
func TestBuildTestMapUnmapsListingFailures(t *testing.T) {
	badDir := t.TempDir()
	ran := stubBuildTestMapDeps(t, nil, []resolvedPkg{
		{importPath: "example.com/bad", dir: badDir},
		{importPath: "example.com/ok", dir: t.TempDir()},
	})
	listTestsFunc = func(context.Context, map[string]*compiledPkg, time.Duration, int) ([]testEntry, map[string]string) {
		return []testEntry{{name: "TestA", pkg: "example.com/ok"}}, map[string]string{"example.com/bad": "listing its tests failed: exit status 3"}
	}

	tm, err := BuildTestMap(context.Background(), t.TempDir(), []string{"./..."}, BuildOptions{TmpDir: t.TempDir(), Workers: 1})
	if err != nil {
		t.Fatalf("BuildTestMap: %v", err)
	}
	want := []UnmappedPkg{{ImportPath: "example.com/bad", Dir: badDir, Reason: "listing its tests failed: exit status 3"}}
	if got := tm.Unmapped(); !slices.Equal(got, want) {
		t.Errorf("Unmapped() = %+v, want %+v", got, want)
	}
	if got := atomic.LoadInt32(ran); got != 1 {
		t.Errorf("runCompiledTestFunc called %d times, want 1 (example.com/ok's TestA)", got)
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

	var (
		tests    []testEntry
		unmapped map[string]string
	)
	// A slot that is never released blocks the loop for good. The deadline
	// stays under the per-mutant timeout so that mutant reads as killed.
	runWithDeadline(t, 5*time.Second, func() {
		tests, unmapped = listTests(context.Background(), bins, 0, 2)
	})
	if peak != 2 {
		t.Errorf("peak concurrent listings = %d, want 2", peak)
	}
	if len(tests) != 4 || len(unmapped) != 0 {
		t.Errorf("listTests = %+v, unmapped %v; want one test per package", tests, unmapped)
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
	listTestsFunc = func(_ context.Context, _ map[string]*compiledPkg, d time.Duration, workers int) ([]testEntry, map[string]string) {
		listTimeout, listWorkers = d, workers
		return []testEntry{{name: "TestA", pkg: "example.com/x"}}, nil
	}
	runCompiledTestFunc = func(_ context.Context, _ *compiledPkg, _, _ string, d time.Duration) ([]Block, time.Duration, bool, error) {
		runTimeout = d
		return nil, time.Millisecond, false, nil
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

// TestParseTestList keeps only names -test.run can select and reports the
// first line that looks like output glued onto a test name. The input is
// verbatim -test.list output from a probe package whose TestMain printed
// "partial" without a newline (gluing it onto TestOne), plus a log line,
// a benchmark whose name contains Test, and padding.
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
	names, glued := parseTestList(out)
	want := []string{"TestTwo", "FuzzF", "ExampleA", "TestÜber_2", "TestPadded", "Test"}
	if !slices.Equal(names, want) {
		t.Errorf("parseTestList names = %q, want %q", names, want)
	}
	if glued != "partialTestOne" {
		t.Errorf("parseTestList glued = %q, want the first glued line %q", glued, "partialTestOne")
	}
	if _, glued := parseTestList("TestA\nBenchmarkTestB\nBenchmarkC\nnoise\n"); glued != "" {
		t.Errorf("clean listing reported glued line %q", glued)
	}
}

// TestProcessWorkForwardsFailures: a solo run that failed must reach the
// map even with no blocks and no duration, carrying the reason and the
// package dir, since it unmaps the package.
func TestProcessWorkForwardsFailures(t *testing.T) {
	orig := runCompiledTestFunc
	t.Cleanup(func() { runCompiledTestFunc = orig })
	runCompiledTestFunc = func(context.Context, *compiledPkg, string, string, time.Duration) ([]Block, time.Duration, bool, error) {
		return nil, 0, false, errors.New("TestA failed when run alone: exit status 1")
	}

	work := make(chan testEntry, 1)
	results := make(chan testCoverage, 1)
	work <- testEntry{name: "TestA", pkg: "pkg1"}
	close(work)
	processWork(context.Background(), work, map[string]*compiledPkg{"pkg1": {binPath: "x", importPath: "pkg1", dir: "/pkg1"}}, t.TempDir(), 0, 0, nil, results)
	close(results)

	got := <-results
	want := testCoverage{pkg: "pkg1", dir: "/pkg1", testName: "TestA", failure: "TestA failed when run alone: exit status 1"}
	if got.pkg != want.pkg || got.dir != want.dir || got.testName != want.testName || got.failure != want.failure {
		t.Errorf("result = %+v, want %+v", got, want)
	}
}

// TestIngestResultUnmapsOnFailure: only a failed run unmaps its package.
func TestIngestResultUnmapsOnFailure(t *testing.T) {
	tm := newTestMap(false)
	tm.ingestResult(testCoverage{pkg: "ok", dir: "/ok", testName: "TestA", duration: time.Millisecond})
	tm.ingestResult(testCoverage{pkg: "bad", dir: "/bad", testName: "TestB", duration: time.Millisecond, failure: "TestB failed when run alone"})
	want := []UnmappedPkg{{ImportPath: "bad", Dir: "/bad", Reason: "TestB failed when run alone"}}
	if got := tm.Unmapped(); !slices.Equal(got, want) {
		t.Errorf("Unmapped() = %+v, want %+v", got, want)
	}
	if _, ok := tm.SumDurationsFor("bad", []string{"TestB"}); !ok {
		t.Error("a failed run's duration should still be recorded")
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
	}
	for _, c := range cases {
		if got := buildFlags(c.in); !slices.Equal(got, c.want) {
			t.Errorf("buildFlags(%q) = %q, want %q", c.in, got, c.want)
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
	runCompiledTestFunc = func(context.Context, *compiledPkg, string, string, time.Duration) ([]Block, time.Duration, bool, error) {
		return nil, time.Nanosecond, false, nil
	}

	work := make(chan testEntry, 1)
	results := make(chan testCoverage, 1)
	work <- testEntry{name: "TestA", pkg: "pkg1"}
	close(work)
	processWork(context.Background(), work, map[string]*compiledPkg{"pkg1": {binPath: "x", importPath: "pkg1"}}, t.TempDir(), 0, 0, nil, results)
	close(results)

	if got := <-results; got.testName != "TestA" || got.duration != time.Nanosecond {
		t.Errorf("result = %+v, want TestA's 1ns duration forwarded", got)
	}
}

// TestCheckSoloSkipsRunFailure: when running the tests up to a
// skipped-alone one fails, the package is unmapped at the first such test,
// with the failure as the reason; a package without solo skips isn't run.
func TestCheckSoloSkipsRunFailure(t *testing.T) {
	bins := map[string]*compiledPkg{
		"p": {binPath: filepath.Join(t.TempDir(), "missing.test"), dir: t.TempDir()},
	}
	tests := []testEntry{
		{name: "TestA", pkg: "p", order: 0},
		{name: "TestB", pkg: "p", order: 1},
		{name: "TestC", pkg: "p", order: 2},
		{name: "TestQ", pkg: "q", order: 0},
	}
	skips := map[string][]testEntry{"p": {tests[2], tests[1]}}
	got := checkSoloSkips(context.Background(), bins, tests, skips, 0, 2)
	f, ok := got["p"]
	if len(got) != 1 || !ok || f.order != 1 ||
		!strings.HasPrefix(f.reason, "TestB skips when run alone, and running it after the tests listed before it failed: ") {
		t.Errorf("checkSoloSkips = %+v, want p unmapped at TestB with the run's failure", got)
	}
}
