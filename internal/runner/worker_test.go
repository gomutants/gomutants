package runner

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/szhekpisov/gomutants/internal/coverage"
	"github.com/szhekpisov/gomutants/internal/mutator"
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
			if got := shortFlagFromEnv(); got != tt.want {
				t.Errorf("env=%q: got %v, want %v — CONDITIONALS_NEGATION on `==` flips this", tt.env, got, tt.want)
			}
		})
	}
}

// TestMakeTestCmdGOMAXPROCSEnv kills BRANCH_IF, CONDITIONALS_BOUNDARY,
// CONDITIONALS_NEGATION, and STATEMENT_REMOVE on the
// `if w.childGOMAXPROCS > 0 { cmd.Env = append(...) }` block.
func TestMakeTestCmdGOMAXPROCSEnv(t *testing.T) {
	t.Run("zero leaves Env nil", func(t *testing.T) {
		w := &Worker{projectDir: ".", policy: TimeoutPolicy{Global: time.Second}, childGOMAXPROCS: 0}
		cmd, _, _ := w.makeTestCmd(context.Background(), []string{"version"})
		if cmd.Env != nil {
			t.Errorf("Env=%v; want nil — CONDITIONALS_BOUNDARY `> 0` → `>= 0` would set env even at zero", cmd.Env)
		}
	})
	t.Run("non-zero sets GOMAXPROCS", func(t *testing.T) {
		w := &Worker{projectDir: "/proj", policy: TimeoutPolicy{Global: time.Second}, childGOMAXPROCS: 3}
		cmd, _, _ := w.makeTestCmd(context.Background(), []string{"version"})
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
	w := &Worker{id: 7, projectDir: "."}
	for _, injection := range infraInjections() {
		t.Run(injection.name, func(t *testing.T) {
			origStart := startCommandFunc
			defer func() { startCommandFunc = origStart }()
			startCommandFunc = func(*exec.Cmd) error { return injection.err }

			var got mutator.MutantStatus
			captured := captureStderr(t, func() {
				got = w.runMutantTest(context.Background(), invocation{path: "go"})
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

// TestBuildTestArgsShortFlag kills CONDITIONALS_NEGATION / BRANCH_IF on
// the GOMUTANTS_TEST_SHORT gate: passing short=true must add "-short" to
// the command line; short=false must omit it. We assert both directions.
func TestBuildTestArgsShortFlag(t *testing.T) {
	w := &Worker{policy: TimeoutPolicy{Global: time.Second}, overlayPath: "/tmp/o.json"}
	m := mutator.Mutant{Pkg: "mymod"}

	withShort := w.buildTestArgs(m, true, time.Second)
	if !containsStr(withShort, "-short") {
		t.Errorf("short=true: args %v missing -short", withShort)
	}
	withoutShort := w.buildTestArgs(m, false, time.Second)
	if containsStr(withoutShort, "-short") {
		t.Errorf("short=false: args %v should not contain -short", withoutShort)
	}
}

// TestBuildTestArgsTestCPU kills BRANCH_IF / CONDITIONALS_BOUNDARY on the
// `if w.testCPU > 0` gate. With testCPU=2 the args must include `-cpu=2`;
// with testCPU=0 the `-cpu=` arg must be absent (let go test default to
// GOMAXPROCS, matching gremlins).
func TestBuildTestArgsTestCPU(t *testing.T) {
	m := mutator.Mutant{Pkg: "mymod"}

	wOn := &Worker{testCPU: 2, policy: TimeoutPolicy{Global: time.Second}, overlayPath: "/tmp/o.json"}
	argsOn := wOn.buildTestArgs(m, false, time.Second)
	if !containsStr(argsOn, "-cpu=2") {
		t.Errorf("testCPU=2: args %v missing -cpu=2", argsOn)
	}

	wOff := &Worker{testCPU: 0, policy: TimeoutPolicy{Global: time.Second}, overlayPath: "/tmp/o.json"}
	argsOff := wOff.buildTestArgs(m, false, time.Second)
	if anyHasPrefix(argsOff, "-cpu=") {
		t.Errorf("testCPU=0: args %v should not contain -cpu=", argsOff)
	}
}

// TestBuildTestArgsTags kills BRANCH_IF / CONDITIONALS_NEGATION on the
// `if w.tags != ""` gate. With tags set the args must include the
// `-tags=<value>` forwarded to the inner go test; with tags empty the
// `-tags=` arg must be absent.
func TestBuildTestArgsTags(t *testing.T) {
	m := mutator.Mutant{Pkg: "mymod"}

	wOn := &Worker{tags: "integration,debug", policy: TimeoutPolicy{Global: time.Second}, overlayPath: "/tmp/o.json"}
	argsOn := wOn.buildTestArgs(m, false, time.Second)
	if !containsStr(argsOn, "-tags=integration,debug") {
		t.Errorf("tags set: args %v missing -tags=integration,debug", argsOn)
	}

	wOff := &Worker{tags: "", policy: TimeoutPolicy{Global: time.Second}, overlayPath: "/tmp/o.json"}
	argsOff := wOff.buildTestArgs(m, false, time.Second)
	if anyHasPrefix(argsOff, "-tags=") {
		t.Errorf("tags empty: args %v should not contain -tags=", argsOff)
	}
}

// TestBuildTestArgsTestFlags kills STATEMENT_REMOVE on the
// `append(args, w.testFlags...)` line and pins the ordering contract:
// user flags land after every flag we set ourselves (so a user value for
// a flag we also pass wins under Go's last-occurrence rule) *and* after
// the package argument.
//
// Trailing the package is issue #75. The first flag `go test` does not
// recognize marks the package list as already seen, so a package name
// after it is forwarded to the test binary as a positional argument and
// `go test` falls back to `.`. With `-rapid.checks=20` ahead of the
// package, the working directory gets tested, the run exits 0, and every
// mutant is recorded LIVED. Go reads the last occurrence of a repeated
// flag regardless of where the package sits, so the override rule the
// original ordering protected survives the move.
func TestBuildTestArgsTestFlags(t *testing.T) {
	m := mutator.Mutant{Pkg: "mymod"}

	wOn := &Worker{
		testFlags:   []string{"-rapid.checks=20", "-race"},
		policy:      TimeoutPolicy{Global: time.Second},
		overlayPath: "/tmp/o.json",
	}
	args := wOn.buildTestArgs(m, true, time.Second)
	for _, want := range []string{"-rapid.checks=20", "-race"} {
		if !containsStr(args, want) {
			t.Errorf("testFlags set: args %v missing %q", args, want)
		}
	}
	// Order: after -short (the last flag baseTestArgs sets), so a prepend
	// that put them ahead of -timeout/-overlay is caught.
	shortIdx := indexOfStr(args, "-short")
	flagIdx := indexOfStr(args, "-rapid.checks=20")
	if shortIdx < 0 || flagIdx < shortIdx {
		t.Errorf("user flags must follow -short; -short at %d, -rapid.checks at %d in %v", shortIdx, flagIdx, args)
	}
	// And after the package: anything else lets a test-binary flag eat it.
	pkgIdx := indexOfStr(args, "mymod")
	if pkgIdx < 0 || flagIdx < pkgIdx {
		t.Errorf("user flags must follow the package argument; mymod at %d, -rapid.checks at %d in %v", pkgIdx, flagIdx, args)
	}
	// User flags stay contiguous at the tail, in the order given.
	if got := args[len(args)-2:]; got[0] != "-rapid.checks=20" || got[1] != "-race" {
		t.Errorf("user flags must trail in order, got %v in %v", got, args)
	}

	wOff := &Worker{policy: TimeoutPolicy{Global: time.Second}, overlayPath: "/tmp/o.json"}
	argsOff := wOff.buildTestArgs(m, false, time.Second)
	if containsStr(argsOff, "-rapid.checks=20") {
		t.Errorf("testFlags unset: args %v must not gain user flags", argsOff)
	}
}

// TestBuildTestArgsRunFilterPrecedesPackage pins the other half of the
// issue #75 ordering: the `-run` filter sits ahead of the package
// argument, so nothing in --test-flags can come between them.
//
// `-run` is not at risk from an unrecognized user flag on its own — the
// go command keeps claiming its own flags past one it does not know, and
// only positional arguments after that point are demoted. It *is* at risk
// from a user `-args`, which ends the go command's claiming outright and
// would forward `-run` to the test binary, where the flag is spelled
// `-test.run` and the bare name is rejected. Keeping the filter ahead of
// the package puts it ahead of every user field, so neither case reaches
// it.
func TestBuildTestArgsRunFilterPrecedesPackage(t *testing.T) {
	w := &Worker{
		testFlags:   []string{"-custom.iterations=5"},
		policy:      TimeoutPolicy{Global: time.Second},
		overlayPath: "/tmp/o.json",
		testMap: coverage.NewTestMapForTesting(nil, map[string][]coverage.TestRef{
			"add.go:3": {{Pkg: "mymod", Name: "TestAdd"}},
		}),
	}
	m := mutator.Mutant{Pkg: "mymod", CoverageFile: "add.go", Line: 3}
	args := w.buildTestArgs(m, false, time.Second)

	runIdx := indexOfPrefix(args, "-run=")
	pkgIdx := indexOfStr(args, "mymod")
	userIdx := indexOfStr(args, "-custom.iterations=5")
	if runIdx < 0 {
		t.Fatalf("no -run filter in %v — test map routing did not apply", args)
	}
	if runIdx >= pkgIdx || pkgIdx >= userIdx {
		t.Errorf("want -run < package < user flags, got -run at %d, mymod at %d, user flag at %d in %v",
			runIdx, pkgIdx, userIdx, args)
	}
}

// TestTestInvocationsTestFlags pins that the cross-package (integration)
// builder forwards user flags too — it composes baseTestArgs per covering
// package, so a regression there would silently drop the flags on exactly
// the runs that are most expensive.
func TestTestInvocationsTestFlags(t *testing.T) {
	w := &Worker{
		testFlags:   []string{"-short"},
		policy:      TimeoutPolicy{Global: time.Second},
		overlayPath: "/tmp/o.json",
	}
	invs := w.testInvocations(mutator.Mutant{Pkg: "mymod"}, false, time.Second)
	if len(invs) != 1 {
		t.Fatalf("want 1 invocation with no testMap, got %d: %v", len(invs), invs)
	}
	if !containsStr(invs[0].args, "-short") {
		t.Errorf("invocation %v missing forwarded -short", invs[0].args)
	}
}

// TestTestInvocationsTestFlagsTrailPackage extends the issue #75 ordering
// to the cross-package builder, which appends user flags on its own rather
// than inheriting them from baseTestArgs. Each routed invocation carries a
// package argument, so each one can swallow it independently.
func TestTestInvocationsTestFlagsTrailPackage(t *testing.T) {
	w := &Worker{
		testFlags:   []string{"-custom.iterations=5"},
		policy:      TimeoutPolicy{Global: time.Second},
		overlayPath: "/tmp/o.json",
		testMap: coverage.NewTestMapForTesting(nil, map[string][]coverage.TestRef{
			"add.go:3": {
				{Pkg: "mymod", Name: "TestAdd"},
				{Pkg: "mymod/other", Name: "TestIntegration"},
			},
		}),
	}
	m := mutator.Mutant{Pkg: "mymod", CoverageFile: "add.go", Line: 3}
	invs := w.testInvocations(m, false, time.Second)
	if len(invs) != 2 {
		t.Fatalf("want one invocation per covering package, got %d: %v", len(invs), invs)
	}
	// Zip against the packages the router is expected to emit, in order, so
	// the package argument is identified by name rather than inferred from
	// the user flag's position — a regression that moved the flags would
	// otherwise be checked against whatever element happened to precede them.
	wantPkgs := []string{"mymod", "mymod/other"}
	for i, inv := range invs {
		args := inv.args
		if args[len(args)-1] != "-custom.iterations=5" {
			t.Errorf("user flag must trail, got last arg %q in %v", args[len(args)-1], args)
		}
		if got := args[len(args)-2]; got != wantPkgs[i] {
			t.Errorf("want package %q immediately before the user flags, got %q in %v",
				wantPkgs[i], got, args)
		}
		runIdx, pkgIdx := indexOfPrefix(args, "-run="), indexOfStr(args, wantPkgs[i])
		if runIdx < 0 || runIdx >= pkgIdx {
			t.Errorf("want -run before the package argument, got -run at %d, %s at %d in %v",
				runIdx, wantPkgs[i], pkgIdx, args)
		}
	}
}

// TestBuildTestArgsPackageArgLast kills STATEMENT_REMOVE on
// `args = append(args, m.Pkg)`: removing that line leaves the command
// without a package target. With no user flags set the package is the
// final arg, so asserting on the tail catches both the removal and any
// reordering. (TestBuildTestArgsTestFlags covers where the package lands
// once --test-flags push it off the end.)
func TestBuildTestArgsPackageArgLast(t *testing.T) {
	w := &Worker{policy: TimeoutPolicy{Global: time.Second}, overlayPath: "/tmp/o.json"}
	m := mutator.Mutant{Pkg: "example.com/mod/sub"}
	args := w.buildTestArgs(m, false, time.Second)
	if len(args) == 0 || args[len(args)-1] != "example.com/mod/sub" {
		t.Errorf("last arg = %q, want package import path; full args: %v",
			args[len(args)-1], args)
	}
	// Also: -timeout, -overlay, -failfast, -count=1, -vet=off must all be present.
	for _, want := range []string{"-failfast", "-count=1", "-vet=off"} {
		if !containsStr(args, want) {
			t.Errorf("args missing %q: %v", want, args)
		}
	}
	if !anyHasPrefix(args, "-overlay=") {
		t.Errorf("args missing -overlay=…: %v", args)
	}
	if !anyHasPrefix(args, "-timeout=") {
		t.Errorf("args missing -timeout=…: %v", args)
	}
}

// TestBuildTestArgsWithTestMap kills CONDITIONALS_NEGATION / BRANCH_IF on
// `if w.testMap != nil`. With a non-nil map that actually contains the
// mutant's (file, line), the command line must include `-run=<regex>`.
// With no map, no -run should appear. Under either mutation, the -run
// flag would be either missing (when it should appear) or leak (via a
// nil-deref panic in the negation case).
func TestBuildTestArgsWithTestMap(t *testing.T) {
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

	tm, err := coverage.BuildTestMap(context.Background(), dir, []string{"testmod"}, "", "", t.TempDir(), 1)
	if err != nil {
		t.Fatalf("BuildTestMap: %v", err)
	}

	wWith := &Worker{testMap: tm, policy: TimeoutPolicy{Global: time.Second}, overlayPath: "/tmp/o.json"}
	wWithout := &Worker{policy: TimeoutPolicy{Global: time.Second}, overlayPath: "/tmp/o.json"}
	m := mutator.Mutant{
		CoverageFile: "testmod/add.go",
		Line:         3,
		Pkg:          "testmod",
	}
	// With map: -run=<pattern> must appear.
	argsWith := wWith.buildTestArgs(m, false, time.Second)
	if !anyHasPrefix(argsWith, "-run=") {
		t.Errorf("testMap non-nil with matching entry: expected -run= in %v", argsWith)
	}
	// Without map: -run= must not appear.
	argsWithout := wWithout.buildTestArgs(m, false, time.Second)
	if anyHasPrefix(argsWithout, "-run=") {
		t.Errorf("testMap nil: -run= must be absent, got %v", argsWithout)
	}
	// With map but no matches for this (file, line): -run= must not appear.
	// Kills CONDITIONALS_BOUNDARY on `len(tests) > 0` — mutated `>= 0` would
	// always enter the branch and append -run= with an empty pattern.
	mMiss := mutator.Mutant{CoverageFile: "unknown/file.go", Line: 9999, Pkg: "testmod"}
	argsMiss := wWith.buildTestArgs(mMiss, false, time.Second)
	if anyHasPrefix(argsMiss, "-run=") {
		t.Errorf("testMap non-nil but no matches: -run= must be absent (len(tests)>0 guard), got %v", argsMiss)
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
		stdout     string
		stderr     string
		want       mutator.MutantStatus
	}{
		{"memkilled beats infrastructure error", anyErr, true, context.DeadlineExceeded, "FATAL ERROR: OUT OF MEMORY", "", mutator.StatusTimedOut},
		// memKilled with otherwise-clean outcome: if the BRANCH_IF on the
		// memKilled early return is elided, execution falls through to
		// `runErr == nil → Lived`. Asserting TimedOut here kills that
		// mutation.
		{"memkilled alone still wins", nil, true, nil, "", "", mutator.StatusTimedOut},
		{"success beats infrastructure error", nil, false, nil, "FATAL ERROR: OUT OF MEMORY", "", mutator.StatusLived},
		{"timeout beats infrastructure error", anyErr, false, context.DeadlineExceeded, "", "FATAL ERROR: OUT OF MEMORY", mutator.StatusTimedOut},
		{"compile failure => not viable", anyErr, false, nil,
			"FAIL\ttestmod [build failed]\nFATAL ERROR: OUT OF MEMORY\n", "worker-0.go:5:2: undefined: Foo\n", mutator.StatusNotViable},
		{"setup failure => not viable", anyErr, false, nil,
			"FAIL\ttestmod [setup failed]\n", "worker-0.go:5:2: cannot use\nFATAL ERROR: OUT OF MEMORY\n", mutator.StatusNotViable},
		{"stderr compile regex but no [build failed] in stdout => killed", anyErr, false, nil,
			"--- FAIL: TestX\nadd_test.go:7: wrong\n", "worker-0.go:5:2: undefined\n", mutator.StatusKilled},
		{"[build failed] in stdout but no compile regex in stderr => killed", anyErr, false, nil,
			"FAIL [build failed]\n", "", mutator.StatusKilled},
		{"normal test failure => killed", anyErr, false, nil,
			"--- FAIL: TestAdd\n", "add_test.go:7: Add(1,2) != 3\n", mutator.StatusKilled},
		// Neither signal gomutants sends reaches here: the RSS monitor's is
		// memKilled and the deadline's is DeadlineExceeded, both already
		// TIMED OUT. A SIGKILL with no test output to explain it came from
		// the kernel, a cgroup, or the CI runner.
		{"unexplained signal killed => infra error", errors.New("signal: killed"), false, nil,
			"", "", mutator.StatusInfraError},
		// ... unless a test reported the mutation first, in which case the
		// process being reaped afterwards changes nothing.
		{"reported failure beats an unexplained signal killed", errors.New("signal: killed"), false, nil,
			"--- FAIL: TestAdd\n", "", mutator.StatusKilled},
		// The tested code's own output lands on the same stdout as the test
		// framework's. A test that reported a failure detected the mutation,
		// so a signature it printed itself must not launder the kill into a
		// non-result — this kills the negation of the `--- FAIL: ` guard.
		{"reported test failure beats infrastructure signature", anyErr, false, nil,
			"--- FAIL: TestDiskFull\n    disk_test.go:9: got \"no space left on device\", want nil\n", "", mutator.StatusKilled},
		// The shape issue #79 actually produces: the OOM-killer takes the test
		// binary (the biggest RSS in the cgroup), not the `go` process
		// supervising it, so `go test` survives to report the death on stdout
		// and exits 1. runErr says nothing.
		{"go test reports the binary's SIGKILL on stdout => infra error", anyErr, false, nil,
			"signal: killed\nFAIL\ttestmod\t0.4s\nFAIL\n", "", mutator.StatusInfraError},
		// Anchored to the line start, because the tested code writes to this
		// stream too: quoted inside a test's own message it is just text.
		{"a test quoting signal: killed mid-line stays killed", anyErr, false, nil,
			"    x_test.go:9: exec failed: signal: killed\n", "", mutator.StatusKilled},
		// A panic outside the test goroutine aborts the binary before `go test`
		// can print a per-test failure line, so `--- FAIL: ` alone would read a
		// detected mutation as a host problem.
		{"goroutine panic quoting a host error stays killed", anyErr, false, nil,
			"panic: open /tmp/x: too many open files\n\ngoroutine 35 [running]:\nFAIL\ttestmod\t0.3s\n", "", mutator.StatusKilled},
		// The runtime's own abort is not a panic and must still be readable as
		// the host failure it is.
		{"runtime fatal error is not vetoed as a panic", anyErr, false, nil,
			"fatal error: out of memory\n\ngoroutine 1 [running]:\n", "", mutator.StatusInfraError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyTestOutcome(tc.runErr, tc.memKilled, tc.testCtxErr, tc.stdout, tc.stderr, false)
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
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

func TestClassifyTestOutcomeInfrastructureSignatures(t *testing.T) {
	for _, signature := range testPhaseInfraSignatures {
		signatureName := subtestName.Replace(signature)
		for _, stream := range []struct {
			name           string
			stdout, stderr string
		}{
			{"stdout", "go test: " + strings.ToUpper(signature), ""},
			{"stderr", "", "go test: " + strings.ToUpper(signature)},
		} {
			t.Run(signatureName+"/"+stream.name, func(t *testing.T) {
				got := classifyTestOutcome(errors.New("exit status 1"), false, nil, stream.stdout, stream.stderr, false)
				if got != mutator.StatusInfraError {
					t.Errorf("got %v, want InfraError in %s", got, stream.name)
				}
			})
		}
	}
}

// TestClassifyTestOutcomeBuildPhaseSignatures covers the wider tier: before
// the test binary runs there is no code-under-test output on the streams, so
// wordings that would be ambiguous during a test run are unambiguous here.
// The cases are the two real-world failures the qualified list alone reports
// as KILLED, which is the bug this status exists to fix.
func TestClassifyTestOutcomeBuildPhaseSignatures(t *testing.T) {
	anyErr := errors.New("exit status 1")
	tests := []struct {
		name           string
		stdout, stderr string
		want           mutator.MutantStatus
	}{
		{"toolchain cannot fork the compiler (EAGAIN)",
			"FAIL\ttestmod [build failed]\n",
			"go: fork/exec /usr/local/go/pkg/tool/darwin_arm64/compile: resource temporarily unavailable\n",
			mutator.StatusInfraError},
		{"linker OOM, unprefixed wording",
			"FAIL\ttestmod [build failed]\n",
			"/usr/bin/ld: out of memory allocating 8388608 bytes\n",
			mutator.StatusInfraError},
		{"setup phase counts too",
			"FAIL\ttestmod [setup failed]\n", "out of memory\n",
			mutator.StatusInfraError},
		// The same generic wording without a build/setup marker came from a
		// running test binary and must not be trusted — this is the case the
		// tier split exists to keep apart, and it kills the `buildPhase &&`
		// conjunction on the wide-list branch.
		{"same wording during a test run stays killed",
			"", "resource temporarily unavailable\n", mutator.StatusKilled},
		{"build failure with no signature stays killed",
			"FAIL\ttestmod [build failed]\n", "some other build problem\n", mutator.StatusKilled},
		// A reported test failure outranks the build-phase tier: some test
		// detected the mutation, so this is a kill regardless. The markers
		// are only text on stdout, and a suite that processes `go test`
		// output prints them as fixture data — this kills a reordering that
		// lets the wide list see test-authored text.
		{"reported test failure outranks the build-phase tier",
			"--- FAIL: TestX\nFAIL\ttestmod [build failed]\n", "out of memory\n",
			mutator.StatusKilled},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyTestOutcome(anyErr, false, nil, tc.stdout, tc.stderr, false)
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// Test helpers.
func containsStr(xs []string, target string) bool {
	for _, x := range xs {
		if x == target {
			return true
		}
	}
	return false
}

// indexOfStr returns the position of target in xs, or -1. Used where a
// test asserts relative argv ordering, not just membership.
func indexOfStr(xs []string, target string) int {
	for i, x := range xs {
		if x == target {
			return i
		}
	}
	return -1
}

// indexOfPrefix is indexOfStr for args whose value isn't known to the
// caller, such as the generated `-run=^(TestAdd)$` filter.
func indexOfPrefix(xs []string, prefix string) int {
	for i, x := range xs {
		if strings.HasPrefix(x, prefix) {
			return i
		}
	}
	return -1
}

func anyHasPrefix(xs []string, prefix string) bool {
	for _, x := range xs {
		if strings.HasPrefix(x, prefix) {
			return true
		}
	}
	return false
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
