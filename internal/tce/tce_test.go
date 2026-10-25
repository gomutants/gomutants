package tce

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gomutants/gomutants/internal/mutator"
)

func TestNormalizeAsm_StripsHeaderAndTrailingWhitespace(t *testing.T) {
	const ip = "github.com/foo/bar"
	// The `# <importpath>` header and trailing whitespace are incidental and
	// must not affect the hash; the instruction lines must.
	base := normalizeAsm([]byte("ADD R1,R0\nMOV x,y\n"), ip)
	withHeaderAndWS := normalizeAsm([]byte("# "+ip+"\nADD R1,R0  \nMOV x,y\t\n"), ip)
	if base != withHeaderAndWS {
		t.Errorf("header / trailing whitespace changed the hash:\n base=%s\n  hw=%s", base, withHeaderAndWS)
	}

	// A real instruction difference must change the hash.
	diff := normalizeAsm([]byte("SUB R1,R0\nMOV x,y\n"), ip)
	if base == diff {
		t.Error("ADD vs SUB produced the same hash; normalizeAsm is destroying signal")
	}
}

func TestBuildArgs(t *testing.T) {
	const ip = "example.com/m/pkg"

	ref := buildArgs(ip, "", "")
	want := []string{"build", "-o", os.DevNull, "-gcflags=" + ip + "=-S", ip}
	if !equalArgs(ref, want) {
		t.Errorf("reference args = %v, want %v", ref, want)
	}

	mut := buildArgs(ip, "/tmp/ov.json", "integration")
	wantMut := []string{"build", "-o", os.DevNull, "-gcflags=" + ip + "=-S", "-overlay=/tmp/ov.json", "-tags=integration", ip}
	if !equalArgs(mut, wantMut) {
		t.Errorf("mutant args = %v, want %v", mut, wantMut)
	}
}

func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// writeTempModule writes a one-package module whose `Zero` function the
// compiler folds (x + 0 == x - 0) and whose `Sum` it does not. Returns the
// module dir, the absolute source path, and its bytes.
func writeTempModule(t *testing.T) (dir, srcAbs string, src []byte) {
	t.Helper()
	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module tcetest\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src = []byte("package tcetest\n\nfunc Zero(x int) int { return x + 0 }\n\nfunc Sum(a, b int) int { return a + b }\n")
	srcAbs = filepath.Join(dir, "m.go")
	if err := os.WriteFile(srcAbs, src, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, srcAbs, src
}

// plusMutant builds a Mutant that replaces the `+` inside `marker` with
// `-`, locating it by the byte offset of marker in src.
func plusMutant(t *testing.T, srcAbs string, src []byte, marker string) mutator.Mutant {
	t.Helper()
	i := bytes.Index(src, []byte(marker))
	if i < 0 {
		t.Fatalf("marker %q not found in source", marker)
	}
	plus := i + bytes.IndexByte(src[i:i+len(marker)], '+')
	if src[plus] != '+' {
		t.Fatalf("expected '+' at offset %d, got %q", plus, src[plus])
	}
	return mutator.Mutant{
		Type:        mutator.ArithmeticBase,
		File:        srcAbs,
		Pkg:         "tcetest",
		Original:    "+",
		Replacement: "-",
		StartOffset: plus,
		EndOffset:   plus + 1,
		Status:      mutator.StatusLived,
	}
}

func requireGo(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
}

func TestDetector_Check_EquivalentVsKillable(t *testing.T) {
	requireGo(t)
	dir, srcAbs, src := writeTempModule(t)
	d := NewDetector(dir, "", map[string][]byte{srcAbs: src})

	tmpSrc := filepath.Join(t.TempDir(), "w.go")
	overlay := filepath.Join(t.TempDir(), "ov.json")

	// x + 0 → x - 0 : the compiler folds both to x, so the assembly is
	// identical → EQUIVALENT.
	eq, err := d.Check(context.Background(), plusMutant(t, srcAbs, src, "x + 0"), tmpSrc, overlay)
	if err != nil {
		t.Fatalf("Check(equivalent): %v", err)
	}
	if !eq {
		t.Error("x+0 → x-0 should be detected as equivalent")
	}

	// a + b → a - b : a real behavior change → assembly differs → NOT
	// equivalent. This is the critical soundness assertion.
	eq, err = d.Check(context.Background(), plusMutant(t, srcAbs, src, "a + b"), tmpSrc, overlay)
	if err != nil {
		t.Fatalf("Check(killable): %v", err)
	}
	if eq {
		t.Error("a+b → a-b must NOT be equivalent (would hide a real test gap)")
	}
}

func TestDetector_Run_FlipsOnlyEquivalentSurvivors(t *testing.T) {
	requireGo(t)
	dir, srcAbs, src := writeTempModule(t)
	d := NewDetector(dir, "", map[string][]byte{srcAbs: src})

	mutants := []mutator.Mutant{
		plusMutant(t, srcAbs, src, "x + 0"), // equivalent → flip
		plusMutant(t, srcAbs, src, "a + b"), // killable → stay LIVED
	}
	// A non-LIVED mutant must be ignored by the pass entirely.
	killed := plusMutant(t, srcAbs, src, "x + 0")
	killed.Status = mutator.StatusKilled
	mutants = append(mutants, killed)

	if n := d.Run(context.Background(), mutants, 2, t.TempDir(), nil); n != 1 {
		t.Errorf("Run returned %d, want 1 equivalent flip", n)
	}

	if mutants[0].Status != mutator.StatusEquivalent {
		t.Errorf("equivalent survivor not flipped: got %s", mutants[0].Status)
	}
	if mutants[1].Status != mutator.StatusLived {
		t.Errorf("killable survivor wrongly changed: got %s", mutants[1].Status)
	}
	if mutants[2].Status != mutator.StatusKilled {
		t.Errorf("non-LIVED mutant must be untouched: got %s", mutants[2].Status)
	}
}

// --- error paths ---
//
// Each test asserts the SPECIFIC error so that removing the guard (which
// lets execution fall through to a later, different failure) changes the
// observed error and fails the assertion. Asserting only `err != nil`
// leaves these guards as surviving mutants, since the deferred failure is
// also non-nil.

const cachedSrc = "package p\n\nvar X = 1\n" // the `1` is at offset 18

func cachedDetector(t *testing.T) *Detector {
	t.Helper()
	return NewDetector(t.TempDir(), "", map[string][]byte{"/p.go": []byte(cachedSrc)})
}

// validMutant points at the `1` literal in cachedSrc with offsets that
// patch cleanly, so the only failure is the one a test injects.
func validMutant() mutator.Mutant {
	return mutator.Mutant{File: "/p.go", Pkg: "x", StartOffset: 18, EndOffset: 19, Replacement: "2", Status: mutator.StatusLived}
}

func TestCheck_SourceNotCached(t *testing.T) {
	d := NewDetector(t.TempDir(), "", map[string][]byte{}) // empty cache
	// Valid empty-range patch on the (missing) source, so a removed guard
	// would proceed to a *different* failure rather than re-erroring here.
	m := mutator.Mutant{File: "/nope.go", Pkg: "x", StartOffset: 0, EndOffset: 0, Replacement: "x"}
	_, err := d.Check(context.Background(), m, filepath.Join(t.TempDir(), "w.go"), filepath.Join(t.TempDir(), "ov.json"))
	if err == nil || !strings.Contains(err.Error(), "not cached") {
		t.Errorf("want a 'not cached' error, got %v", err)
	}
}

func TestCheck_PatchOutOfRange(t *testing.T) {
	d := cachedDetector(t)
	m := mutator.Mutant{File: "/p.go", Pkg: "x", StartOffset: 0, EndOffset: 9999, Replacement: "-"}
	_, err := d.Check(context.Background(), m, filepath.Join(t.TempDir(), "w.go"), filepath.Join(t.TempDir(), "ov.json"))
	if err == nil || !strings.Contains(err.Error(), "patch:") {
		t.Errorf("want a 'patch:' error, got %v", err)
	}
}

func TestCheck_TmpSourceWriteFails(t *testing.T) {
	d := cachedDetector(t)
	defer swapWriteFile(failWriteSuffix(".go"))() // overlay (.json) still writes
	_, err := d.Check(context.Background(), validMutant(), filepath.Join(t.TempDir(), "w.go"), filepath.Join(t.TempDir(), "ov.json"))
	if !errors.Is(err, errInjected) {
		t.Errorf("want injected write error, got %v", err)
	}
}

func TestCheck_OverlayWriteFails(t *testing.T) {
	d := cachedDetector(t)
	defer swapWriteFile(failWriteSuffix(".json"))() // tmp source (.go) still writes
	_, err := d.Check(context.Background(), validMutant(), filepath.Join(t.TempDir(), "w.go"), filepath.Join(t.TempDir(), "ov.json"))
	if !errors.Is(err, errInjected) {
		t.Errorf("want injected overlay-write error, got %v", err)
	}
}

func TestWriteOverlay_MarshalFails(t *testing.T) {
	d := cachedDetector(t)
	defer swapMarshal(func(any) ([]byte, error) { return nil, errInjected })()
	_, err := d.Check(context.Background(), validMutant(), filepath.Join(t.TempDir(), "w.go"), filepath.Join(t.TempDir(), "ov.json"))
	if !errors.Is(err, errInjected) {
		t.Errorf("want injected marshal error, got %v", err)
	}
}

// TestCheck_ReferenceError seeds a sticky reference error and stubs the
// mutant compile to succeed: correct code returns the reference error,
// while dropping the guard would swallow it and return (false, nil).
func TestCheck_ReferenceError(t *testing.T) {
	d := cachedDetector(t)
	seedRefErr(d, "x", errInjected)
	defer swapExec(succeedExec)()
	_, err := d.Check(context.Background(), validMutant(), filepath.Join(t.TempDir(), "w.go"), filepath.Join(t.TempDir(), "ov.json"))
	if !errors.Is(err, errInjected) {
		t.Errorf("want the seeded reference error to propagate, got %v", err)
	}
}

// TestCheck_MutantCompileError seeds a good reference, then the mutant
// compile (stubbed) fails — covers the compileHash error branch.
func TestCheck_MutantCompileError(t *testing.T) {
	d := cachedDetector(t)
	seedRef(d, "x", "deadbeef")
	defer swapExec(failingExec)()
	_, err := d.Check(context.Background(), validMutant(), filepath.Join(t.TempDir(), "w.go"), filepath.Join(t.TempDir(), "ov.json"))
	if err == nil || !strings.Contains(err.Error(), "go build -S") {
		t.Errorf("want a 'go build -S' compile error, got %v", err)
	}
}

func TestRun_NoSurvivors(t *testing.T) {
	d := NewDetector(t.TempDir(), "", map[string][]byte{})
	// No LIVED mutants → nothing checked, no compile, no panic, zero flips.
	if n := d.Run(context.Background(), []mutator.Mutant{{Status: mutator.StatusKilled}}, 0, t.TempDir(), nil); n != 0 {
		t.Errorf("Run returned %d with no survivors, want 0", n)
	}
}

// TestRun_CancelledContext: with a pre-cancelled context the worker hits
// the ctx.Err() guard and returns *before* calling onResult. Dropping the
// guard would process the item and invoke onResult — so asserting zero
// callbacks kills that guard.
func TestRun_CancelledContext(t *testing.T) {
	d := cachedDetector(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ms := []mutator.Mutant{validMutant()}
	var calls int32
	d.Run(ctx, ms, 1, t.TempDir(), func(mutator.Mutant) { atomic.AddInt32(&calls, 1) })
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Errorf("onResult called %d times under cancelled ctx, want 0", n)
	}
	if ms[0].Status != mutator.StatusLived {
		t.Errorf("cancelled run must leave status LIVED, got %s", ms[0].Status)
	}
}

func TestRun_CallbackAndDefaultWorkers(t *testing.T) {
	requireGo(t)
	dir, srcAbs, src := writeTempModule(t)
	dg := NewDetector(dir, "", map[string][]byte{srcAbs: src})
	ms := []mutator.Mutant{plusMutant(t, srcAbs, src, "x + 0")}
	var got int
	// workers=0 exercises the `max(1, workers)` floor.
	if n := dg.Run(context.Background(), ms, 0, t.TempDir(), func(mutator.Mutant) { got++ }); n != 1 {
		t.Errorf("Run returned %d, want 1", n)
	}
	if got != 1 {
		t.Errorf("onResult called %d times, want 1", got)
	}
	if ms[0].Status != mutator.StatusEquivalent {
		t.Errorf("equivalent survivor not flipped under default workers: got %s", ms[0].Status)
	}
}

// TestRun_SerializesCallback runs two survivors through a single worker
// with a callback. The unlock after onResult is required: dropping it
// deadlocks the worker on its next iteration (non-reentrant mutex), so
// this asserts both callbacks complete.
func TestRun_SerializesCallback(t *testing.T) {
	requireGo(t)
	dir, srcAbs, src := writeTempModule(t)
	dg := NewDetector(dir, "", map[string][]byte{srcAbs: src})
	ms := []mutator.Mutant{
		plusMutant(t, srcAbs, src, "x + 0"), // equivalent
		plusMutant(t, srcAbs, src, "a + b"), // killable, stays LIVED
	}
	var calls int32
	dg.Run(context.Background(), ms, 1, t.TempDir(), func(mutator.Mutant) { atomic.AddInt32(&calls, 1) })
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("onResult called %d times, want 2", n)
	}
}

func TestMutantLess_AllDimensions(t *testing.T) {
	mk := func(pkg, file string, off int) mutator.Mutant {
		return mutator.Mutant{Pkg: pkg, File: file, StartOffset: off}
	}
	// Each case is chosen so that removing the corresponding guard changes
	// the result via fall-through (not just re-deriving the same bool).
	cases := []struct {
		name string
		a, b mutator.Mutant
		want bool
	}{
		{"pkg<", mk("a", "z", 9), mk("b", "a", 0), true},   // drop pkg< → false
		{"pkg>", mk("b", "a", 0), mk("a", "z", 9), false},  // drop pkg> → file a<z → true
		{"file<", mk("a", "a", 9), mk("a", "b", 0), true},  // drop file< → 9<0 → false
		{"file>", mk("a", "b", 0), mk("a", "a", 9), false}, // drop file> → 0<9 → true
		{"offset<", mk("a", "f", 1), mk("a", "f", 2), true},
		{"offset==", mk("a", "f", 2), mk("a", "f", 2), false}, // kills the final < → <= boundary
	}
	for _, tc := range cases {
		if got := mutantLess(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: mutantLess = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestReferenceHash_MemoizedOncePerPackage kills the memo-store mutant:
// two Checks on the same package must compile the reference exactly once.
func TestReferenceHash_MemoizedOncePerPackage(t *testing.T) {
	requireGo(t)
	dir, srcAbs, src := writeTempModule(t)
	d := NewDetector(dir, "", map[string][]byte{srcAbs: src})

	var refCompiles int32
	defer swapExec(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		hasOverlay := false
		for _, a := range args {
			if strings.HasPrefix(a, "-overlay=") {
				hasOverlay = true
			}
		}
		if !hasOverlay { // a reference (un-overlaid) compile
			atomic.AddInt32(&refCompiles, 1)
		}
		return exec.CommandContext(ctx, name, args...)
	})()

	m := plusMutant(t, srcAbs, src, "x + 0")
	tmp := filepath.Join(t.TempDir(), "w.go")
	ov := filepath.Join(t.TempDir(), "ov.json")
	for range 2 {
		if _, err := d.Check(context.Background(), m, tmp, ov); err != nil {
			t.Fatalf("Check: %v", err)
		}
	}
	if n := atomic.LoadInt32(&refCompiles); n != 1 {
		t.Errorf("reference compiled %d times across 2 Checks, want 1 (memoized)", n)
	}
}

// TestNormalizeAsm_LineBoundaryMatters kills the per-line newline-write
// mutant: two inputs with the same concatenation but different line splits
// must hash differently.
func TestNormalizeAsm_LineBoundaryMatters(t *testing.T) {
	if normalizeAsm([]byte("AB\nC"), "p") == normalizeAsm([]byte("A\nBC"), "p") {
		t.Error("normalizeAsm ignored line boundaries (newline separator dropped)")
	}
}

// TestNormalizeAsm_OnlyStripsExactHeader pins the exact-header anchor: the
// real `# <importpath>` package header is dropped (it's identical across the
// reference and mutant builds), but any other '#'-prefixed line must stay in
// the hash — a data dump that begins with '#' can carry real signal, so
// stripping it would be over-detection. Kills the bytes.Equal-guard mutants.
func TestNormalizeAsm_OnlyStripsExactHeader(t *testing.T) {
	const ip = "github.com/foo/bar"
	if normalizeAsm([]byte("# "+ip+"\nA"), ip) != normalizeAsm([]byte("A"), ip) {
		t.Error("exact package header was not stripped")
	}
	if normalizeAsm([]byte("# other\nA"), ip) == normalizeAsm([]byte("A"), ip) {
		t.Error("a non-header '#' line was wrongly stripped (over-detection risk)")
	}
}

var errInjected = errorString("injected")

type errorString string

func (e errorString) Error() string { return string(e) }

// failWriteSuffix injects errInjected for paths ending in suffix and writes
// normally otherwise, so a test can fail just the tmp-source (.go) or just
// the overlay (.json) write.
func failWriteSuffix(suffix string) func(string, []byte, os.FileMode) error {
	return func(p string, b []byte, m os.FileMode) error {
		if strings.HasSuffix(p, suffix) {
			return errInjected
		}
		return os.WriteFile(p, b, m)
	}
}

func failingExec(ctx context.Context, _ string, _ ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "false") // exits non-zero → cmd.Run errors
}

func succeedExec(ctx context.Context, _ string, _ ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "true") // exits zero, empty stderr
}

func swapWriteFile(f func(string, []byte, os.FileMode) error) func() {
	orig := writeFileFunc
	writeFileFunc = f
	return func() { writeFileFunc = orig }
}

func swapMarshal(f func(any) ([]byte, error)) func() {
	orig := marshalFunc
	marshalFunc = f
	return func() { marshalFunc = orig }
}

func swapExec(f func(context.Context, string, ...string) *exec.Cmd) func() {
	orig := execCommandContext
	execCommandContext = f
	return func() { execCommandContext = orig }
}

// seedRef marks a package's reference hash as already computed, so
// referenceHash returns it without compiling.
func seedRef(d *Detector, pkg, hash string) {
	r := &refResult{}
	r.once.Do(func() { r.hash = hash })
	d.refs[pkg] = r
}

// seedRefErr marks a package's reference as already failed, so
// referenceHash returns err without compiling.
func seedRefErr(d *Detector, pkg string, err error) {
	r := &refResult{}
	r.once.Do(func() { r.err = err })
	d.refs[pkg] = r
}
