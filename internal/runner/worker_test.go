package runner

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gomutants/gomutants/internal/coverage"
	"github.com/gomutants/gomutants/internal/mutator"
)

// TestPackageVarDefaults pins the default values of the worker's
// package-level vars and the maxCapturedOutput const. These literals
// live above any function body and aren't reachable by tests that
// override them — without an explicit pin, ARITHMETIC_BASE and
// INVERT_BITWISE mutants on the literals (e.g. `2 * 1024 * 1024 * 1024`,
// `1 << 20`) are unkillable.
func TestPackageVarDefaults(t *testing.T) {
	if got, want := maxSubprocRSSBytes, int64(2*1024*1024*1024); got != want {
		t.Errorf("maxSubprocRSSBytes = %d, want %d (2 GiB)", got, want)
	}
	if got, want := monitorPollInterval, 1*time.Second; got != want {
		t.Errorf("monitorPollInterval = %v, want %v", got, want)
	}
	if got, want := maxCapturedOutput, 1<<20; got != want {
		t.Errorf("maxCapturedOutput = %d, want %d (1 MiB)", got, want)
	}
}

func TestNewWorker(t *testing.T) {
	dir := t.TempDir()
	cache := map[string][]byte{"/src/file.go": []byte("package p\n")}

	w, err := NewWorker(0, dir, TimeoutPolicy{Global: 30 * time.Second}, cache, "/src", nil)
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	if w.id != 0 {
		t.Errorf("id=%d, want 0", w.id)
	}

	// Verify temp files were created.
	if _, err := os.Stat(w.tmpSrcPath); err != nil {
		t.Errorf("tmpSrcPath not created: %v", err)
	}
	if _, err := os.Stat(w.overlayPath); err != nil {
		t.Errorf("overlayPath not created: %v", err)
	}
}

func TestWorkerTestMissingSource(t *testing.T) {
	dir := t.TempDir()
	cache := map[string][]byte{} // Empty cache.

	w, err := NewWorker(0, dir, TimeoutPolicy{Global: 30 * time.Second}, cache, dir, nil)
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	m := mutator.Mutant{
		ID:     1,
		File:   "/nonexistent/file.go",
		Status: mutator.StatusPending,
	}

	result := w.Test(context.Background(), m)
	if result.Status != mutator.StatusNotViable {
		t.Errorf("Status=%v, want NOT_VIABLE for missing source", result.Status)
	}
	// Duration must be set even on early return paths.
	if result.Duration <= 0 {
		t.Errorf("Duration should be > 0 on early-return path, got %v", result.Duration)
	}
}

func TestWorkerTestInvalidPatch(t *testing.T) {
	dir := t.TempDir()
	src := []byte("package p\n")
	cache := map[string][]byte{"/src/file.go": src}

	w, err := NewWorker(0, dir, TimeoutPolicy{Global: 30 * time.Second}, cache, dir, nil)
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	m := mutator.Mutant{
		ID:          1,
		File:        "/src/file.go",
		StartOffset: 100, // Beyond file length.
		EndOffset:   200,
		Replacement: "x",
		Status:      mutator.StatusPending,
	}

	result := w.Test(context.Background(), m)
	if result.Status != mutator.StatusNotViable {
		t.Errorf("Status=%v, want NOT_VIABLE for invalid patch", result.Status)
	}
	if result.Duration <= 0 {
		t.Errorf("Duration should be > 0 on early-return path, got %v", result.Duration)
	}
}

func TestWorkerTestNotViable(t *testing.T) {
	// Create a small Go project that will fail to compile with the mutation.
	dir := t.TempDir()
	goMod := `module testmod

go 1.26
`
	src := `package testpkg

func Add(a, b int) int {
	return a + b
}
`
	testSrc := `package testpkg

import "testing"

func TestAdd(t *testing.T) {
	if Add(1, 2) != 3 {
		t.Fatal("wrong")
	}
}
`
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "add.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "add_test.go"), []byte(testSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	cache := map[string][]byte{filepath.Join(dir, "add.go"): []byte(src)}

	w, err := NewWorker(0, t.TempDir(), TimeoutPolicy{Global: 30 * time.Second}, cache, dir, nil)
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	// Replace entire file with code that has an undefined symbol (compile error).
	m := mutator.Mutant{
		ID:          1,
		File:        filepath.Join(dir, "add.go"),
		Pkg:         "testmod",
		StartOffset: 0,
		EndOffset:   len(src),
		Replacement: "package testpkg\n\nfunc Add(a, b int) int {\n\treturn UNDEFINED_SYMBOL\n}\n",
		Status:      mutator.StatusPending,
	}

	result := w.Test(context.Background(), m)
	if result.Status != mutator.StatusNotViable {
		t.Errorf("Status=%v, want NOT_VIABLE for compile error", result.Status)
	}
}

func TestWorkerTestKilled(t *testing.T) {
	dir := t.TempDir()
	goMod := `module testmod

go 1.26
`
	src := `package testpkg

func Add(a, b int) int {
	return a + b
}
`
	testSrc := `package testpkg

import "testing"

func TestAdd(t *testing.T) {
	if Add(1, 2) != 3 {
		t.Fatal("wrong")
	}
}
`
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "add.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "add_test.go"), []byte(testSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	cache := map[string][]byte{filepath.Join(dir, "add.go"): []byte(src)}

	w, err := NewWorker(0, t.TempDir(), TimeoutPolicy{Global: 30 * time.Second}, cache, dir, nil)
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	// Mutate + to - (test should fail → KILLED).
	plusIdx := 51 // "a + b" — the "+" position
	for i, c := range src {
		if c == '+' && i > 30 { // Skip package line
			plusIdx = i
			break
		}
	}

	m := mutator.Mutant{
		ID:          1,
		File:        filepath.Join(dir, "add.go"),
		Pkg:         "testmod",
		StartOffset: plusIdx,
		EndOffset:   plusIdx + 1,
		Replacement: "-",
		Status:      mutator.StatusPending,
	}

	result := w.Test(context.Background(), m)
	if result.Status != mutator.StatusKilled {
		t.Errorf("Status=%v, want KILLED", result.Status)
	}
	if result.Duration == 0 {
		t.Error("Duration should be > 0")
	}
}

func TestWorkerTestLived(t *testing.T) {
	dir := t.TempDir()
	goMod := `module testmod

go 1.26
`
	// This function's test doesn't check the operator, so the mutant survives.
	src := `package testpkg

func Add(a, b int) int {
	return a + b
}
`
	testSrc := `package testpkg

import "testing"

func TestAdd(t *testing.T) {
	// Weak test: doesn't verify the result.
	_ = Add(1, 2)
}
`
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "add.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "add_test.go"), []byte(testSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	cache := map[string][]byte{filepath.Join(dir, "add.go"): []byte(src)}

	w, err := NewWorker(0, t.TempDir(), TimeoutPolicy{Global: 30 * time.Second}, cache, dir, nil)
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	plusIdx := 0
	for i, c := range src {
		if c == '+' && i > 30 {
			plusIdx = i
			break
		}
	}

	m := mutator.Mutant{
		ID:          1,
		File:        filepath.Join(dir, "add.go"),
		Pkg:         "testmod",
		StartOffset: plusIdx,
		EndOffset:   plusIdx + 1,
		Replacement: "-",
		Status:      mutator.StatusPending,
	}

	result := w.Test(context.Background(), m)
	if result.Status != mutator.StatusLived {
		t.Errorf("Status=%v, want LIVED", result.Status)
	}
	// With no coverage map the routed run was the whole package, so there
	// is nothing to re-check.
	if result.Rechecked {
		t.Error("Rechecked=true, want false: the routed run already ran the whole package")
	}
	// The all-lived tail must still stamp Duration — STATEMENT_REMOVE on
	// `m.Duration = time.Since(start)` would leave it at zero.
	if result.Duration <= 0 {
		t.Errorf("Duration=%v on LIVED mutant, want > 0", result.Duration)
	}
}

func TestWorkerTestTimeout(t *testing.T) {
	dir := t.TempDir()
	goMod := `module testmod

go 1.26
`
	src := `package testpkg

func Add(a, b int) int {
	return a + b
}
`
	// Test that will run forever.
	testSrc := `package testpkg

import "testing"
import "time"

func TestAdd(t *testing.T) {
	time.Sleep(10 * time.Minute)
}
`
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "add.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "add_test.go"), []byte(testSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	cache := map[string][]byte{filepath.Join(dir, "add.go"): []byte(src)}

	// Very short timeout.
	w, err := NewWorker(0, t.TempDir(), TimeoutPolicy{Global: 3 * time.Second}, cache, dir, nil)
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	plusIdx := 0
	for i, c := range src {
		if c == '+' && i > 30 {
			plusIdx = i
			break
		}
	}

	m := mutator.Mutant{
		ID:          1,
		File:        filepath.Join(dir, "add.go"),
		Pkg:         "testmod",
		StartOffset: plusIdx,
		EndOffset:   plusIdx + 1,
		Replacement: "-",
		Status:      mutator.StatusPending,
	}

	result := w.Test(context.Background(), m)
	if result.Status != mutator.StatusTimedOut {
		t.Errorf("Status=%v, want TIMED_OUT", result.Status)
	}
}

// TestClampPositive directly exercises the d <= 0 boundary that drives
// nonZeroSince. Driving nonZeroSince itself is racy because time.Since on
// a just-captured time.Now() returns a small positive duration on real
// clocks, hiding `<` ↔ `<=` mutations.
func TestClampPositive(t *testing.T) {
	cases := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"zero", 0, time.Nanosecond},
		{"negative", -1 * time.Second, time.Nanosecond},
		{"tiny positive", time.Nanosecond, time.Nanosecond},
		{"normal", 5 * time.Millisecond, 5 * time.Millisecond},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := clampPositive(c.in); got != c.want {
				t.Errorf("clampPositive(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// TestNewWorkerWriteFailures kills BRANCH_IF on both write-error returns
// in NewWorker (lines 119 / 122). Stub writeFileFunc to fail at the
// requested call index; the original returns the error, the elided body
// falls through to a successful-looking *Worker.
func TestNewWorkerWriteFailures(t *testing.T) {
	for _, tt := range []struct {
		name     string
		failCall int32
	}{
		{"first write fails (tmpSrc)", 1},
		{"second write fails (overlay)", 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			orig := writeFileFunc
			defer func() { writeFileFunc = orig }()
			var calls atomic.Int32
			writeFileFunc = func(name string, data []byte, perm os.FileMode) error {
				if calls.Add(1) == tt.failCall {
					return errors.New("inject")
				}
				return os.WriteFile(name, data, perm)
			}
			w, err := NewWorker(0, t.TempDir(), TimeoutPolicy{Global: time.Second}, nil, "/", nil)
			if err == nil {
				t.Errorf("got nil error, want injected failure on call %d (BRANCH_IF on err-return elides early exit, returning %+v)", tt.failCall, w)
			}
		})
	}
}

// TestWorkerTestWriteFailures kills BRANCH_IF on the two write paths
// inside Worker.Test (tmpSrc patched / overlay JSON). Stub writeFileFunc
// so the patched-source write fails on the second sequence of calls
// (NewWorker writes once for each of tmpSrc and overlay first).
func TestWorkerTestWriteFailures(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "f.go")
	src := []byte("package p\nvar X = 1\n")
	if err := os.WriteFile(srcPath, src, 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name        string
		failOnIndex int32 // call index at which to inject failure (post-NewWorker)
	}{
		{"patched-source write fails", 1},
		{"overlay-JSON write fails", 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cache := map[string][]byte{srcPath: src}
			origWrite := writeFileFunc
			defer func() { writeFileFunc = origWrite }()
			// Phase 1: NewWorker sets up the worker with two writes
			// (tmpSrc placeholder, overlay placeholder). Let those through.
			// Phase 2: count Test's writes and fail on the requested index.
			var phase atomic.Int32
			var testCalls atomic.Int32
			writeFileFunc = func(name string, data []byte, perm os.FileMode) error {
				if phase.Load() < 2 {
					phase.Add(1)
					return os.WriteFile(name, data, perm)
				}
				if testCalls.Add(1) == tt.failOnIndex {
					return errors.New("inject")
				}
				return os.WriteFile(name, data, perm)
			}

			w, err := NewWorker(0, t.TempDir(), TimeoutPolicy{Global: 5 * time.Second}, cache, dir, nil)
			if err != nil {
				t.Fatalf("NewWorker: %v", err)
			}

			m := mutator.Mutant{
				ID: 1, File: srcPath, Pkg: "p",
				StartOffset: len(src) - 1, EndOffset: len(src),
				Replacement: "X", Status: mutator.StatusPending,
			}
			start := time.Now()
			result := w.Test(context.Background(), m)
			elapsed := time.Since(start)

			if result.Status != mutator.StatusNotViable {
				t.Errorf("Status=%v, want NotViable — BRANCH_IF on the write-error body falls through to go test", result.Status)
			}
			// Early-return path must still set Duration — STATEMENT_REMOVE
			// on `m.Duration = nonZeroSince(start)` would leave it at zero.
			if result.Duration <= 0 {
				t.Errorf("Duration=%v on early-return path; want > 0 — STATEMENT_REMOVE drops the assignment", result.Duration)
			}
			// Early-return path is essentially instant; falling through
			// would attempt a real `go test` invocation that easily takes
			// hundreds of ms even on a tiny package.
			if elapsed > 200*time.Millisecond {
				t.Errorf("elapsed=%v on early-return path — BRANCH_IF lets execution continue past the write failure", elapsed)
			}
		})
	}
}

// subtestName makes a signature or errno text safe to use as a subtest name.
var subtestName = strings.NewReplacer(" ", "_", "/", "_")

// infraInjection is one error a stubbed write can return, with the subtest
// name it should run under.
type infraInjection struct {
	name string
	err  error
}

// infraWriteCase is one (injected error, failing write) pair for
// TestWorkerTestInfrastructureWriteFailures. failOnIndex is the 1-based
// writeFileFunc call to fail: 1 is the patched source, 2 is the overlay.
type infraWriteCase struct {
	srcPath     string
	src         []byte
	root        string
	injected    error
	failOnIndex int32
}

// runInfraWriteFailure asserts that a write failure carrying an
// infrastructure signature classifies the mutant as InfraError rather than
// NotViable. Split out of the test body so the nested loops and closures
// don't stack into one over-complex function.
func runInfraWriteFailure(t *testing.T, tc infraWriteCase) {
	t.Helper()

	w, err := NewWorker(0, t.TempDir(), TimeoutPolicy{Global: 5 * time.Second}, map[string][]byte{tc.srcPath: tc.src}, tc.root, nil)
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	origWrite := writeFileFunc
	defer func() { writeFileFunc = origWrite }()
	var calls atomic.Int32
	writeFileFunc = func(name string, data []byte, perm os.FileMode) error {
		if calls.Add(1) == tc.failOnIndex {
			return tc.injected
		}
		return os.WriteFile(name, data, perm)
	}

	result := w.Test(context.Background(), mutator.Mutant{
		ID: 1, File: tc.srcPath, Pkg: "p",
		StartOffset: len(tc.src) - 1, EndOffset: len(tc.src),
		Replacement: "X", Status: mutator.StatusPending,
	})
	if result.Status != mutator.StatusInfraError {
		t.Errorf("Status=%v, want InfraError", result.Status)
	}
	if result.Duration <= 0 {
		t.Errorf("Duration=%v, want > 0", result.Duration)
	}
}

// infraInjections is every error shape setupErrorStatus must recognize: each
// errno as os.WriteFile really returns it (wrapped in a *fs.PathError, so the
// test exercises errors.Is unwrapping rather than a bare errno), plus one
// error that lost its errno on the way up and is recognizable only by text.
func infraInjections() []infraInjection {
	out := make([]infraInjection, 0, len(infrastructureErrnos)+1)
	for _, errno := range infrastructureErrnos {
		out = append(out, infraInjection{
			name: subtestName.Replace(errno.Error()),
			err:  &fs.PathError{Op: "write", Path: "worker-0.go", Err: errno},
		})
	}
	return append(out,
		infraInjection{
			name: "message_only_no_errno",
			err:  errors.New("INJECTED: NO SPACE LEFT ON DEVICE"),
		},
		// Generic wording: safe to match here because an error value from
		// gomutants' own syscall is never authored by the code under test.
		infraInjection{
			name: "message_only_generic_wording",
			err:  errors.New("INJECTED: OUT OF MEMORY"),
		})
}

func TestWorkerTestInfrastructureWriteFailures(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "f.go")
	src := []byte("package p\nvar X = 1\n")
	if err := os.WriteFile(srcPath, src, 0o644); err != nil {
		t.Fatal(err)
	}

	writes := []struct {
		name        string
		failOnIndex int32
	}{
		{"patched source", 1},
		{"overlay", 2},
	}

	for _, injection := range infraInjections() {
		for _, write := range writes {
			t.Run(injection.name+"/"+write.name, func(t *testing.T) {
				runInfraWriteFailure(t, infraWriteCase{
					srcPath:     srcPath,
					src:         src,
					root:        dir,
					injected:    injection.err,
					failOnIndex: write.failOnIndex,
				})
			})
		}
	}
}

// TestShortFlagFromEnv kills CONDITIONALS_NEGATION on the
// `os.Getenv("GOMUTANTS_TEST_SHORT") == "1"` check.
func TestShortFlagFromEnv(t *testing.T) {
	for _, tt := range []struct {
		env  string
		want bool
	}{
		{"", false},
		{"0", false},
		{"true", false},
		{"1", true},
	} {
		t.Run("env="+tt.env, func(t *testing.T) {
			t.Setenv("GOMUTANTS_TEST_SHORT", tt.env)
			if got := ShortFlagFromEnv(); got != tt.want {
				t.Errorf("env=%q: got %v, want %v — CONDITIONALS_NEGATION on `==` flips this", tt.env, got, tt.want)
			}
		})
	}
}

// TestMakeCmdGOMAXPROCSEnv kills BRANCH_IF, CONDITIONALS_BOUNDARY,
// CONDITIONALS_NEGATION, and STATEMENT_REMOVE on the
// `if w.childGOMAXPROCS > 0 { cmd.Env = append(...) }` block.
func TestMakeCmdGOMAXPROCSEnv(t *testing.T) {
	t.Run("zero leaves Env nil", func(t *testing.T) {
		w := &Worker{projectDir: ".", policy: TimeoutPolicy{Global: time.Second}, childGOMAXPROCS: 0}
		cmd, _, _ := w.makeCmd(context.Background(), "go", w.projectDir, []string{"version"})
		if cmd.Env != nil {
			t.Errorf("Env=%v; want nil — CONDITIONALS_BOUNDARY `> 0` → `>= 0` would set env even at zero", cmd.Env)
		}
	})
	t.Run("non-zero sets GOMAXPROCS", func(t *testing.T) {
		w := &Worker{projectDir: "/proj", policy: TimeoutPolicy{Global: time.Second}, childGOMAXPROCS: 3}
		cmd, _, _ := w.makeCmd(context.Background(), "go", w.projectDir, []string{"version"})
		if cmd.Env == nil {
			t.Fatal("Env is nil; want GOMAXPROCS override — BRANCH_IF on the body or STATEMENT_REMOVE on the assignment drops it")
		}
		if !envContains(cmd.Env, "GOMAXPROCS=3") {
			t.Errorf("Env missing GOMAXPROCS=3: %v", cmd.Env)
		}
		if !envContains(cmd.Env, "PWD=/proj") {
			t.Errorf("Env missing PWD=/proj: %v", cmd.Env)
		}
	})
}

// TestBinEnv: a test binary runs in `go test`'s environment for it —
// GOROOT/bin first on PATH, PWD its directory — plus the GOMAXPROCS cap
// only when there is one.
func TestBinEnv(t *testing.T) {
	t.Setenv("PATH", "inherited")
	want := "PATH=" + filepath.Join("/goroot", "bin") + string(os.PathListSeparator) + "inherited"
	for _, procs := range []int{0, 3} {
		w := &Worker{goroot: "/goroot", childGOMAXPROCS: procs}
		env := w.binEnv("/pkg")
		if !envContains(env, want) || !envContains(env, "PWD=/pkg") {
			t.Errorf("childGOMAXPROCS=%d: env lacks %s or PWD=/pkg: %v", procs, want, env)
		}
		if got, wantCap := envContains(env, "GOMAXPROCS=3"), procs > 0; got != wantCap {
			t.Errorf("childGOMAXPROCS=%d: GOMAXPROCS=3 in env = %v, want %v", procs, got, wantCap)
		}
		if extra := len(env) - len(coverage.TestBinaryEnv(w.goroot, "/pkg")); extra != min(procs, 1) {
			t.Errorf("childGOMAXPROCS=%d: %d entries beyond go test's, want %d", procs, extra, min(procs, 1))
		}
	}
}

func envContains(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}

// TestWorkerTestStartFailureClassifiesNotViable kills BRANCH_IF on the
// command-start error body. Stub startCommandFunc so Start fails
// deterministically with an error carrying no infrastructure signature.
// With the body elided, Getpgid runs against a nil cmd.Process and panics;
// the original returns NotViable cleanly. Also asserts the diagnostic
// Fprintf surfaces in stderr (kills STATEMENT_REMOVE on the log line).
func TestWorkerTestStartFailureClassifiesNotViable(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "f.go")
	src := []byte("package p\nvar X = 1\n")
	if err := os.WriteFile(srcPath, src, 0o644); err != nil {
		t.Fatal(err)
	}
	cache := map[string][]byte{srcPath: src}

	origStart := startCommandFunc
	defer func() { startCommandFunc = origStart }()
	startCommandFunc = func(*exec.Cmd) error { return errors.New("injected start failure") }

	w, err := NewWorker(0, t.TempDir(), TimeoutPolicy{Global: 5 * time.Second}, cache, dir, nil)
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	m := mutator.Mutant{
		ID: 1, File: srcPath, Pkg: "p",
		StartOffset: len(src) - 1, EndOffset: len(src),
		Replacement: "X", Status: mutator.StatusPending,
	}
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Worker.Test panicked on cmd.Start failure: %v — BRANCH_IF on the err-return body elides the early exit and Getpgid(nil.Pid) panics", r)
		}
	}()
	var result mutator.Mutant
	captured := captureStderr(t, func() {
		result = w.Test(context.Background(), m)
	})
	if result.Status != mutator.StatusNotViable {
		t.Errorf("Status=%v, want NotViable on cmd.Start failure", result.Status)
	}
	if result.Duration <= 0 {
		t.Errorf("Duration=%v, want > 0", result.Duration)
	}
	if !strings.Contains(captured, "cmd.Start failed") {
		t.Errorf("stderr missing the cmd.Start diagnostic; got: %q — STATEMENT_REMOVE on the Fprintf elides the log", captured)
	}
}

func TestWorkerTestStartFailureClassifiesInfrastructureErrors(t *testing.T) {
	w := &Worker{id: 7, projectDir: ".", binDir: t.TempDir()}
	for _, injection := range infraInjections() {
		t.Run(injection.name, func(t *testing.T) {
			origStart := startCommandFunc
			defer func() { startCommandFunc = origStart }()
			startCommandFunc = func(*exec.Cmd) error { return injection.err }

			var got mutator.MutantStatus
			captured := captureStderr(t, func() {
				_, got = w.buildBin(context.Background(), "p")
			})
			if got != mutator.StatusInfraError {
				t.Errorf("Status=%v, want InfraError", got)
			}
			if !strings.Contains(captured, "INFRA ERROR") {
				t.Error("stderr missing INFRA ERROR classification")
			}
		})
	}
}

// TestNonZeroSinceSleep kills CONDITIONALS_NEGATION on `d <= 0` (line 60):
// mutated `d > 0` takes the Nanosecond branch on every normal call, so
// the returned duration would be exactly 1 ns even after a real sleep.
func TestNonZeroSinceSleep(t *testing.T) {
	start := time.Now()
	time.Sleep(5 * time.Millisecond)
	d := nonZeroSince(start)
	if d < 5*time.Millisecond {
		t.Errorf("nonZeroSince after 5ms sleep = %v, want >= 5ms (mutation returns 1ns)", d)
	}
}

// TestNonZeroSinceFuture kills the BRANCH_IF on `{ return time.Nanosecond }`:
// a start time in the future yields d <= 0 from time.Since. The original
// returns time.Nanosecond (>0) so callers can use 0 as a "never set"
// sentinel. Under BRANCH_IF the body is elided and 0 or negative leaks out.
func TestNonZeroSinceFuture(t *testing.T) {
	future := time.Now().Add(1 * time.Hour)
	d := nonZeroSince(future)
	if d <= 0 {
		t.Errorf("nonZeroSince(future) = %v, want > 0 (sentinel positive duration)", d)
	}
}

// TestCappedBufferCapsAtMax kills ARITHMETIC_BASE and INVERT_NEGATIVES on
// `maxCapturedOutput - len(c.buf)` (line 78): mutated `+` makes remaining
// always large, so the buffer grows past its cap. We write 2× the cap and
// assert the stored bytes don't exceed the cap.
func TestCappedBufferCapsAtMax(t *testing.T) {
	var c cappedBuffer
	chunk := make([]byte, 64*1024) // 64 KiB chunks
	for range 40 {                 // 40 * 64 KiB = 2.5 MiB, well above 1 MiB cap
		n, _ := c.Write(chunk)
		if n != len(chunk) {
			t.Errorf("Write returned n=%d, want %d (must report full length to satisfy io.Writer)", n, len(chunk))
		}
	}
	if len(c.buf) > maxCapturedOutput {
		t.Errorf("buf grew to %d bytes, exceeds cap %d — capping arithmetic broken", len(c.buf), maxCapturedOutput)
	}
	// Must have captured at least something up to the cap.
	if len(c.buf) == 0 {
		t.Errorf("buf is empty after writes — cap check too aggressive")
	}
}

// TestCappedBufferPartialFinalWrite kills patterns that mishandle the
// "final write exceeds remaining" branch. After writing cap-1 bytes, a
// second write of 10 bytes should fill to exactly the cap (1 byte taken
// from the second chunk).
func TestCappedBufferPartialFinalWrite(t *testing.T) {
	var c cappedBuffer
	first := make([]byte, maxCapturedOutput-1)
	c.Write(first)
	if len(c.buf) != maxCapturedOutput-1 {
		t.Fatalf("after first write: len=%d, want %d", len(c.buf), maxCapturedOutput-1)
	}
	// Second write: 10 bytes, but only 1 byte of remaining capacity.
	n, _ := c.Write([]byte("0123456789"))
	if n != 10 {
		t.Errorf("Write n=%d, want 10 (must report full input length)", n)
	}
	if len(c.buf) != maxCapturedOutput {
		t.Errorf("after partial write: len=%d, want %d (cap)", len(c.buf), maxCapturedOutput)
	}
}

// TestCappedBufferWriteAtCap kills mutations on the `remaining > 0` guard:
// once buf is at the cap, further writes must be no-ops but still return
// the input length (to satisfy the io.Writer contract).
func TestCappedBufferWriteAtCap(t *testing.T) {
	var c cappedBuffer
	c.buf = make([]byte, maxCapturedOutput)
	before := len(c.buf)
	n, err := c.Write([]byte("extra"))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if n != 5 {
		t.Errorf("Write n=%d, want 5", n)
	}
	if len(c.buf) != before {
		t.Errorf("buf grew past cap: len=%d, was %d", len(c.buf), before)
	}
}

// TestCappedBufferString kills trivial mutations on the String() accessor
// (e.g., STATEMENT_REMOVE on the return) by exercising it on real data.
func TestCappedBufferString(t *testing.T) {
	var c cappedBuffer
	c.Write([]byte("hello"))
	if got := c.String(); got != "hello" {
		t.Errorf("String() = %q, want %q", got, "hello")
	}
}

// TestWorkerTestParentCtxCancel verifies that a parent-context
// cancellation (Ctrl-C, upstream deadline) is NOT classified as Killed.
// The worker should preserve the incoming Status (Pending) + zero
// Duration so the pool surfaces the mutant as not tested.
//
// Cost: ~300-500 ms per run — the inner test binary sleeps until the
// parent ctx fires. Keep this in mind when adding similar patterns.
func TestWorkerTestParentCtxCancel(t *testing.T) {
	dir := t.TempDir()
	goMod := "module testmod\n\ngo 1.26\n"
	src := "package testpkg\n\nfunc Add(a, b int) int { return a + b }\n"
	testSrc := "package testpkg\n\nimport (\n\t\"testing\"\n\t\"time\"\n)\n\nfunc TestSlow(t *testing.T) { time.Sleep(30 * time.Second) }\n"

	for name, body := range map[string]string{
		"go.mod": goMod, "add.go": src, "add_test.go": testSrc,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cache := map[string][]byte{filepath.Join(dir, "add.go"): []byte(src)}
	w, err := NewWorker(0, t.TempDir(), TimeoutPolicy{Global: 30 * time.Second}, cache, dir, nil)
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	plusIdx := 0
	for i, c := range src {
		if c == '+' && i > 30 {
			plusIdx = i
			break
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	m := mutator.Mutant{
		ID: 1, File: filepath.Join(dir, "add.go"), Pkg: "testmod",
		StartOffset: plusIdx, EndOffset: plusIdx + 1, Replacement: "-",
		Status: mutator.StatusPending,
	}

	// Cancel mid-run: the test binary above sleeps 30s, so parent-ctx
	// cancellation fires before the test returns naturally.
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()

	result := w.Test(ctx, m)
	if result.Status != mutator.StatusPending {
		t.Errorf("Status=%v, want Pending — parent-ctx cancel must not produce a terminal classification", result.Status)
	}
	// Invariant: Pending ⇒ Duration==0. Otherwise the report shows a
	// "not tested" mutant with an execution time, which is misleading.
	if result.Duration != 0 {
		t.Errorf("Duration=%v on cancelled (Pending) mutant, want 0", result.Duration)
	}
}

// TestBinFlags pins the `go test` flags a mutant's test binaries are run
// as (their binary arguments are read from them; see binArgsCache), and
// kills the mutations on each gate:
//   - BRANCH_IF / CONDITIONALS_NEGATION on `if short`: -short must appear
//     exactly when short is set.
//   - BRANCH_IF / CONDITIONALS_BOUNDARY on `if w.testCPU > 0`: -cpu=N must
//     appear exactly when testCPU is positive, matching gremlins in
//     leaving `go test` to default to GOMAXPROCS otherwise.
//   - STATEMENT_REMOVE on the append of w.testFlags: the user's flags must
//     appear, last, in the order given, so a user value for a flag we
//     also pass wins under Go's last-occurrence rule.
func TestBinFlags(t *testing.T) {
	cases := []struct {
		name string
		w    *Worker
		shrt bool
		want []string
	}{
		{"defaults", &Worker{}, false, []string{"-failfast"}},
		{"short", &Worker{}, true, []string{"-failfast", "-short"}},
		{"test cpu", &Worker{testCPU: 2}, false, []string{"-failfast", "-cpu=2"}},
		{"one test cpu", &Worker{testCPU: 1}, false, []string{"-failfast", "-cpu=1"}},
		{"everything", &Worker{testCPU: 2, testFlags: []string{"-rapid.checks=20", "-race"}}, true,
			[]string{"-failfast", "-cpu=2", "-short", "-rapid.checks=20", "-race"}},
	}
	for _, tc := range cases {
		if got := tc.w.binFlags(tc.shrt); !slices.Equal(got, tc.want) {
			t.Errorf("%s: binFlags = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestBuildArgs pins the `go test -c` argv that builds a mutant's test
// binary. Only the build flags among --test-flags reach it: `go test -c`
// rejects a flag it doesn't know, such as -rapid.checks=20, which reaches
// the binary through binFlags instead. They trail the package (issue #75:
// a flag `go test` doesn't recognize ahead of the package would demote it
// to a positional argument). -tags appears exactly when tags are set,
// which kills BRANCH_IF / CONDITIONALS_NEGATION on `if w.tags != ""`.
func TestBuildArgs(t *testing.T) {
	w := &Worker{overlayPath: "/tmp/o.json", tags: "integration,debug", testFlags: []string{"-rapid.checks=20", "-race", "-short"}}
	want := []string{"test", "-c", "-o", "/tmp/w.test", "-vet=off", "-overlay=/tmp/o.json", "-tags=integration,debug", "example.com/mod/sub", "-race"}
	if got := w.buildArgs("example.com/mod/sub", "/tmp/w.test"); !slices.Equal(got, want) {
		t.Errorf("buildArgs = %q, want %q", got, want)
	}
	w = &Worker{overlayPath: "/tmp/o.json"}
	want = []string{"test", "-c", "-o", "/tmp/w.test", "-vet=off", "-overlay=/tmp/o.json", "example.com/mod/sub"}
	if got := w.buildArgs("example.com/mod/sub", "/tmp/w.test"); !slices.Equal(got, want) {
		t.Errorf("buildArgs without tags or flags = %q, want %q", got, want)
	}
}

// TestRunArgs: a run filtered to some tests passes the binary its
// -test.run filter ahead of the arguments every run gets, as a positional
// argument among those (after a user's -args) ends the binary's flag
// parsing; a run of the whole package passes no filter.
func TestRunArgs(t *testing.T) {
	binArgs := []string{"-test.paniconexit0", "-test.failfast=true", "-args", "-custom"}
	want := append([]string{"-test.run=^(TestA|TestB)$"}, binArgs...)
	if got := runArgs([]string{"TestA", "TestB"}, binArgs); !slices.Equal(got, want) {
		t.Errorf("runArgs(filtered) = %q, want %q", got, want)
	}
	if got := runArgs(nil, binArgs); !slices.Equal(got, binArgs) {
		t.Errorf("runArgs(whole package) = %q, want %q", got, binArgs)
	}
}

// TestRoutedRunWithTestMap kills CONDITIONALS_NEGATION / BRANCH_IF on the
// routing through a real coverage map. With a map that contains the
// mutant's (file, line), the run must be filtered to its covering tests;
// with no map, or none for that position, the whole package runs.
func TestRoutedRunWithTestMap(t *testing.T) {
	dir := t.TempDir()
	mustWrite := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("go.mod", "module testmod\n\ngo 1.26\n")
	mustWrite("add.go", "package testmod\n\nfunc Add(a, b int) int { return a + b }\n")
	mustWrite("add_test.go", "package testmod\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) { if Add(1, 2) != 3 { t.Fatal(\"wrong\") } }\n")

	tm, err := coverage.BuildTestMap(context.Background(), dir, []string{"testmod"}, coverage.BuildOptions{TmpDir: t.TempDir(), Workers: 1})
	if err != nil {
		t.Fatalf("BuildTestMap: %v", err)
	}

	m := mutator.Mutant{CoverageFile: "testmod/add.go", Line: 3, Pkg: "testmod"}
	if got := onlyRun(t, &Worker{testMap: tm}, m); !slices.Equal(got.tests, []string{"TestAdd"}) {
		t.Errorf("map with an entry: tests = %q, want [TestAdd]", got.tests)
	}
	if got := onlyRun(t, &Worker{}, m); got.tests != nil {
		t.Errorf("no map: tests = %q, want nil (the whole package)", got.tests)
	}
	// Kills CONDITIONALS_BOUNDARY on `len(groups) == 0`-style guards: a
	// position the map doesn't hold must fall back to the whole package,
	// not an empty filter.
	miss := mutator.Mutant{CoverageFile: "unknown/file.go", Line: 9999, Pkg: "testmod"}
	if got := onlyRun(t, &Worker{testMap: tm}, miss); got.tests != nil {
		t.Errorf("map without an entry: tests = %q, want nil (the whole package)", got.tests)
	}
}

// TestClassifyTestOutcome covers every branch of the classifier.
// Kills BRANCH_IF on the memKilled short-circuit, the runErr==nil
// Lived return, the DeadlineExceeded arm, and both EXPRESSION_REMOVE
// mutations on the `compileErrorRe && ([build failed] || [setup failed])`
// predicate.
func TestClassifyTestOutcome(t *testing.T) {
	anyErr := errors.New("exit status 1")
	tests := []struct {
		name       string
		runErr     error
		memKilled  bool
		testCtxErr error
		output     string
		want       mutator.MutantStatus
	}{
		{"memkilled beats infrastructure error", anyErr, true, context.DeadlineExceeded, "FATAL ERROR: OUT OF MEMORY", mutator.StatusTimedOut},
		// memKilled with otherwise-clean outcome: if the BRANCH_IF on the
		// memKilled early return is elided, execution falls through to
		// `runErr == nil → Lived`. Asserting TimedOut here kills that
		// mutation.
		{"memkilled alone still wins", nil, true, nil, "", mutator.StatusTimedOut},
		{"success beats infrastructure error", nil, false, nil, "FATAL ERROR: OUT OF MEMORY", mutator.StatusLived},
		{"timeout beats infrastructure error", anyErr, false, context.DeadlineExceeded, "FATAL ERROR: OUT OF MEMORY", mutator.StatusTimedOut},
		// The binary was built beforehand, so a compile diagnostic or a
		// `[build failed]` in its output is a test's own text: no verdict
		// on whether the mutant compiles.
		{"a test printing a compile diagnostic stays killed", anyErr, false, nil,
			"FAIL\ttestmod [build failed]\nworker-0.go:5:2: undefined: Foo\n", mutator.StatusKilled},
		// Nor does that marker promote the generic wordings a failed build
		// is trusted with: printed by a test that exits through log.Fatal,
		// with no `--- FAIL: ` line, they are its own words.
		{"a test printing a build marker and a generic phrase stays killed", anyErr, false, nil,
			"FAIL\ttestmod [build failed]\nresource temporarily unavailable\n", mutator.StatusKilled},
		{"normal test failure => killed", anyErr, false, nil,
			"--- FAIL: TestAdd\nadd_test.go:7: Add(1,2) != 3\n", mutator.StatusKilled},
		// Neither signal gomutants sends reaches here: the RSS monitor's is
		// memKilled and the deadline's is DeadlineExceeded, both already
		// TIMED OUT. A SIGKILL with no test output to explain it came from
		// the kernel, a cgroup, or the CI runner.
		{"unexplained signal killed => infra error", errors.New("signal: killed"), false, nil,
			"", mutator.StatusInfraError},
		// ... unless a test reported the mutation first, in which case the
		// process being reaped afterwards changes nothing.
		{"reported failure beats an unexplained signal killed", errors.New("signal: killed"), false, nil,
			"--- FAIL: TestAdd\n", mutator.StatusKilled},
		// The tested code's own output lands on the same stream as the test
		// framework's. A test that reported a failure detected the mutation,
		// so a signature it printed itself must not launder the kill into a
		// non-result — this kills the negation of the `--- FAIL: ` guard.
		{"reported test failure beats infrastructure signature", anyErr, false, nil,
			"--- FAIL: TestDiskFull\n    disk_test.go:9: got \"no space left on device\", want nil\n", mutator.StatusKilled},
		// The shape issue #79 produced under `go test`: the OOM-killer took
		// the test binary and `go test` reported its death on stdout, with
		// runErr a plain exit status.
		{"a SIGKILL reported on the output => infra error", anyErr, false, nil,
			"signal: killed\nFAIL\ttestmod\t0.4s\nFAIL\n", mutator.StatusInfraError},
		// Anchored to the line start, because the tested code writes to this
		// stream too: quoted inside a test's own message it is just text.
		{"a test quoting signal: killed mid-line stays killed", anyErr, false, nil,
			"    x_test.go:9: exec failed: signal: killed\n", mutator.StatusKilled},
		// A panic outside the test goroutine aborts the binary before it can
		// print a per-test failure line, so `--- FAIL: ` alone would read a
		// detected mutation as a host problem.
		{"goroutine panic quoting a host error stays killed", anyErr, false, nil,
			"panic: open /tmp/x: too many open files\n\ngoroutine 35 [running]:\nFAIL\ttestmod\t0.3s\n", mutator.StatusKilled},
		// The runtime's own abort is not a panic and must still be readable as
		// the host failure it is.
		{"runtime fatal error is not vetoed as a panic", anyErr, false, nil,
			"fatal error: out of memory\n\ngoroutine 1 [running]:\n", mutator.StatusInfraError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyTestOutcome(tc.runErr, tc.memKilled, tc.testCtxErr, tc.output, false)
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestClassifyBuildFailure covers every branch of the build classifier.
// The build's output is all the toolchain's, so unlike a test run's it
// needs no `[build failed]` marker to trust a compile diagnostic, and the
// generic build-phase signatures count.
func TestClassifyBuildFailure(t *testing.T) {
	anyErr := errors.New("exit status 1")
	tests := []struct {
		name           string
		runErr         error
		memKilled      bool
		stdout, stderr string
		want           mutator.MutantStatus
	}{
		{"memkilled beats a compile error", anyErr, true, "", "worker-0.go:5:2: undefined: Foo\n", mutator.StatusTimedOut},
		{"compile error => not viable", anyErr, false, "", "# testmod\nworker-0.go:5:2: undefined: Foo\n", mutator.StatusNotViable},
		{"compile error beats an infra signature", anyErr, false, "", "worker-0.go:5:2: out of memory\n", mutator.StatusNotViable},
		{"generic build-phase signature => infra error", anyErr, false, "", "go: fork/exec compile: resource temporarily unavailable\n", mutator.StatusInfraError},
		{"test-phase signature => infra error", anyErr, false, "no space left on device\n", "", mutator.StatusInfraError},
		{"unexplained signal killed => infra error", errors.New("signal: killed"), false, "", "", mutator.StatusInfraError},
		{"killed linker on stderr => infra error", anyErr, false, "", "testmod.test: /usr/local/go/pkg/tool/linux_amd64/link: signal: killed\n", mutator.StatusInfraError},
		{"killed compiler on stderr => infra error", anyErr, false, "", "testmod: /usr/local/go/pkg/tool/linux_amd64/compile: signal: killed\n", mutator.StatusInfraError},
		{"anything else => killed", anyErr, false, "", "go: something unexpected\n", mutator.StatusKilled},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyBuildFailure(tc.runErr, tc.memKilled, tc.stdout, tc.stderr); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBinArgsCache: the arguments are read once, for the package of the
// first call, and a failed read isn't kept, so a later call reads again.
func TestBinArgsCache(t *testing.T) {
	orig := testBinaryArgsFunc
	t.Cleanup(func() { testBinaryArgsFunc = orig })
	var pkgs []string
	fail := true
	testBinaryArgsFunc = func(_ context.Context, _, _, pkg string, _ []string) ([]string, error) {
		pkgs = append(pkgs, pkg)
		if fail {
			return nil, errors.New("boom")
		}
		return []string{"-test.paniconexit0"}, nil
	}
	c := &binArgsCache{}
	if _, err := c.get(context.Background(), ".", "", "m/a", nil); err == nil {
		t.Fatal("first get: want the read's error")
	}
	fail = false
	for _, pkg := range []string{"m/b", "m/c"} {
		got, err := c.get(context.Background(), ".", "", pkg, nil)
		if err != nil || !slices.Equal(got, []string{"-test.paniconexit0"}) {
			t.Errorf("get(%s) = (%q, %v), want the read arguments", pkg, got, err)
		}
	}
	if want := []string{"m/a", "m/b"}; !slices.Equal(pkgs, want) {
		t.Errorf("reads for %v, want %v: one failed, then one kept", pkgs, want)
	}
}

// TestWorkerTestBinaryStderrIsMerged: a test binary reports a panic on
// stderr, which `go test` merged into the stdout the classifier reads for
// a test's own failure markers. A goroutine that prints a host error and
// panics is a kill (see panicMarker); with stderr read apart, the printed
// phrase alone would turn it into an infrastructure error.
func TestWorkerTestBinaryStderrIsMerged(t *testing.T) {
	dir := t.TempDir()
	src := "package testpkg\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n"
	testSrc := "package testpkg\n\nimport (\n\t\"fmt\"\n\t\"testing\"\n\t\"time\"\n)\n\n" +
		"func TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n" +
		"\t\tgo func() {\n\t\t\tfmt.Println(\"open /tmp/x: too many open files\")\n\t\t\tpanic(\"giving up\")\n\t\t}()\n" +
		"\t\ttime.Sleep(time.Second)\n\t}\n}\n"
	for name, body := range map[string]string{"go.mod": "module testmod\n\ngo 1.26\n", "add.go": src, "add_test.go": testSrc} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(dir, "add.go")
	w, err := NewWorker(0, t.TempDir(), TimeoutPolicy{Global: 30 * time.Second}, map[string][]byte{file: []byte(src)}, dir, nil)
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	plus := strings.Index(src, "+")
	m := mutator.Mutant{ID: 1, File: file, Pkg: "testmod", StartOffset: plus, EndOffset: plus + 1, Replacement: "-", Status: mutator.StatusPending}
	if got := w.Test(context.Background(), m); got.Status != mutator.StatusKilled {
		t.Errorf("Status=%v, want KILLED by the goroutine's panic", got.Status)
	}
}

// opaqueWrappedErr rewrites the message of the error it wraps while keeping
// it reachable through errors.Is — what a caller that reports its own context
// (`fmt.Errorf("staging mutant %d: %v", id, err)` and friends) leaves behind.
type opaqueWrappedErr struct{ err error }

func (o opaqueWrappedErr) Error() string { return "staging the mutant failed" }
func (o opaqueWrappedErr) Unwrap() error { return o.err }

// TestIsInfrastructureErrSeesThroughRewrittenMessages pins what the errno
// comparison buys over the message scan: every recognized errno must still be
// found when the text no longer names it. Without it the scan alone decides,
// which is exactly the spoofable matching this classifier avoids.
func TestIsInfrastructureErrSeesThroughRewrittenMessages(t *testing.T) {
	for _, errno := range infrastructureErrnos {
		t.Run(subtestName.Replace(errno.Error()), func(t *testing.T) {
			if !isInfrastructureErr(opaqueWrappedErr{errno}) {
				t.Errorf("errno %v hidden behind a rewritten message was not recognized", errno)
			}
		})
	}
	t.Run("unrelated error stays unrecognized", func(t *testing.T) {
		if isInfrastructureErr(opaqueWrappedErr{errors.New("bad overlay JSON")}) {
			t.Error("an ordinary wrapped error must not read as an infrastructure failure")
		}
	})
}

// TestClassifyTestOutcomeInfrastructureSignatures: every qualified
// signature in a test binary's output, in any case, is a host failure.
func TestClassifyTestOutcomeInfrastructureSignatures(t *testing.T) {
	for _, signature := range testPhaseInfraSignatures {
		t.Run(subtestName.Replace(signature), func(t *testing.T) {
			got := classifyTestOutcome(errors.New("exit status 1"), false, nil, "open /tmp/x: "+strings.ToUpper(signature), false)
			if got != mutator.StatusInfraError {
				t.Errorf("got %v, want InfraError", got)
			}
		})
	}
}

// TestClassifyTestOutcomeIgnoresBuildPhaseSignatures: the generic wordings
// a failed build is trusted with (see classifyBuildFailure) are a test's
// own words in a test binary's output, with or without a build marker
// beside them, which can only be a test's text too.
func TestClassifyTestOutcomeIgnoresBuildPhaseSignatures(t *testing.T) {
	for _, signature := range buildPhaseInfraSignatures {
		for _, marker := range []string{"", "FAIL\ttestmod [build failed]\n", "FAIL\ttestmod [setup failed]\n"} {
			t.Run(subtestName.Replace(signature+" "+marker), func(t *testing.T) {
				if got := classifyTestOutcome(errors.New("exit status 1"), false, nil, marker+signature+"\n", false); got != mutator.StatusKilled {
					t.Errorf("got %v, want Killed", got)
				}
			})
		}
	}
}

// TestClassifyBuildFailureSignatures: no test has run when a build fails,
// so both signature lists, the generic wordings too, mean the host failed.
func TestClassifyBuildFailureSignatures(t *testing.T) {
	for _, signature := range append(slices.Clone(buildPhaseInfraSignatures), testPhaseInfraSignatures...) {
		t.Run(subtestName.Replace(signature), func(t *testing.T) {
			if got := classifyBuildFailure(errors.New("exit status 1"), false, "", "go: "+strings.ToUpper(signature)+"\n"); got != mutator.StatusInfraError {
				t.Errorf("got %v, want InfraError", got)
			}
		})
	}
}

func TestCompileErrorRegex(t *testing.T) {
	tests := []struct {
		input string
		match bool
	}{
		{"./file.go:10:5: undefined: foo", true},
		{"main.go:1:1: expected declaration", true},
		{"FAIL\ttestmod\t0.001s", false},
		{"ok  \ttestmod\t0.001s", false},
	}
	for _, tc := range tests {
		if got := compileErrorRe.MatchString(tc.input); got != tc.match {
			t.Errorf("compileErrorRe.Match(%q) = %v, want %v", tc.input, got, tc.match)
		}
	}
}

// onlyRun returns the single per-package run routed for m, failing the
// test if routing produced any other number.
func onlyRun(t *testing.T, w *Worker, m mutator.Mutant) pkgRun {
	t.Helper()
	runs := w.routedInvocations(m)
	if len(runs) != 1 {
		t.Fatalf("got %d runs, want 1: %v", len(runs), runs)
	}
	return runs[0]
}
