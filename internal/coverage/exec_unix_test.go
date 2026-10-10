//go:build !windows

package coverage

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gomutants/gomutants/internal/proctree"
)

// spawnModule's TestMain starts `sleep` with the test binary's stdout, so
// the sleep holds the listing's output pipe open, records its pid in
// $SLEEP_PID_FILE, then hangs (in a sleep: the runtime would end a bare
// select{} as a deadlock). With $SLEEP_OWN_GROUP set the sleep leaves
// the binary's process group.
var spawnModule = map[string]string{
	"go.mod": "module spawnmod\n\ngo 1.26\n",
	"h.go":   "package spawnmod\n",
	"h_test.go": `package spawnmod

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	cmd := exec.Command("sleep", "60")
	cmd.Stdout = os.Stdout
	if os.Getenv("SLEEP_OWN_GROUP") != "" {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	if err := cmd.Start(); err != nil {
		os.Exit(2)
	}
	os.WriteFile(os.Getenv("SLEEP_PID_FILE"), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
	time.Sleep(time.Hour)
}

func TestH(t *testing.T) {}
`,
}

// listHanging lists spawnModule's tests with a short timeout and returns
// how long the listing took and the pid of the sleep it started, which is
// killed on cleanup in case it survived.
func listHanging(t *testing.T, ownGroup bool) (time.Duration, int) {
	t.Helper()
	dir := writeModule(t, spawnModule)
	cp := compileFixture(t, dir, "spawnmod")
	pidFile := filepath.Join(t.TempDir(), "pid")
	t.Setenv("SLEEP_PID_FILE", pidFile)
	if ownGroup {
		t.Setenv("SLEEP_OWN_GROUP", "1")
	}

	start := time.Now()
	if _, err := listBinTests(context.Background(), cp, 300*time.Millisecond); err == nil {
		t.Error("listBinTests of a hanging TestMain: want an error")
	}
	took := time.Since(start)

	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("reading the sleep's pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	return took, pid
}

// TestListBinTestsKillsProcessTree: a timed-out listing kills the processes
// the binary started too. Killing the binary alone left the sleep running,
// holding the output pipe, so the listing waited for it.
func TestListBinTestsKillsProcessTree(t *testing.T) {
	orig := proctree.DrainDelay
	t.Cleanup(func() { proctree.DrainDelay = orig })
	proctree.DrainDelay = time.Minute

	took, pid := listHanging(t, false)
	if took > 10*time.Second {
		t.Errorf("listing took %s, want it cut off near its 300ms timeout", took)
	}
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatal("the sleep the test binary started outlived the listing")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestListBinTestsStopsWaitingForEscapedOutput: a process that left the
// binary's group survives the kill and still holds the output pipe; the
// listing stops waiting for it after proctree.DrainDelay.
func TestListBinTestsStopsWaitingForEscapedOutput(t *testing.T) {
	orig := proctree.DrainDelay
	t.Cleanup(func() { proctree.DrainDelay = orig })
	proctree.DrainDelay = 200 * time.Millisecond

	if took, _ := listHanging(t, true); took > 10*time.Second {
		t.Errorf("listing took %s, want it to stop waiting soon after its timeout", took)
	}
}

// leakModule's TestMain starts a `sleep` holding the binary's stdout,
// records its pid in $LEAK_PID_DIR, and then runs the tests normally, so
// the binary exits while the sleep keeps its output open.
var leakModule = map[string]string{
	"go.mod": "module leakmod\n\ngo 1.26\n",
	"l.go":   "package leakmod\n\nfunc F() int { return 1 }\n",
	"l_test.go": `package leakmod

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

func TestMain(m *testing.M) {
	cmd := exec.Command("sleep", "60")
	cmd.Stdout = os.Stdout
	if err := cmd.Start(); err != nil {
		os.Exit(2)
	}
	pid := strconv.Itoa(cmd.Process.Pid)
	os.WriteFile(filepath.Join(os.Getenv("LEAK_PID_DIR"), pid), nil, 0o644)
	os.Exit(m.Run())
}

func TestF(t *testing.T) { F() }

func TestG(t *testing.T) { F() }
`,
}

// TestRunsSurviveLeftoverOutput: a binary that succeeds but leaves a
// process holding its output open still lists fine once proctree.DrainDelay
// gives up on that output — the binary's own is complete — and runs fine,
// as a per-test run reads no output.
func TestRunsSurviveLeftoverOutput(t *testing.T) {
	orig := proctree.DrainDelay
	t.Cleanup(func() { proctree.DrainDelay = orig })
	proctree.DrainDelay = 100 * time.Millisecond
	pidDir := t.TempDir()
	t.Setenv("LEAK_PID_DIR", pidDir)
	t.Cleanup(func() {
		entries, _ := os.ReadDir(pidDir)
		for _, e := range entries {
			if pid, err := strconv.Atoi(e.Name()); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	dir := writeModule(t, leakModule)
	cp := compileFixture(t, dir, "leakmod")

	if names, err := listBinTests(context.Background(), cp, 0); err != nil || len(names) != 2 {
		t.Errorf("listBinTests = (%v, %v), want both tests", names, err)
	}
	if blocks, _, err := runCompiledTest(context.Background(), cp, "TestF", filepath.Join(t.TempDir(), "f.cov"), 0); err != nil || len(blocks) == 0 {
		t.Errorf("runCompiledTest = (%d blocks, %v), want TestF's coverage", len(blocks), err)
	}
}

// fakeGoFirstOnPath puts first on PATH a `go` that reports itself as
// "fake" to `go version` and hands every other command to the real `go`,
// so builds still work: a test that sees "fake" ran a `go` other than the
// toolchain's.
func fakeGoFirstOnPath(t *testing.T) {
	t.Helper()
	real, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = version ]; then echo fake; exit 0; fi\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// goVersionModule's test fails unless the `go` it runs is a real one.
var goVersionModule = map[string]string{
	"go.mod": "module gover\n\ngo 1.26\n",
	"g.go":   "package gover\n\nfunc F() int { return 1 }\n",
	"g_test.go": `package gover

import (
	"os/exec"
	"strings"
	"testing"
)

func TestGo(t *testing.T) {
	F()
	out, err := exec.Command("go", "version").Output()
	if err != nil || strings.TrimSpace(string(out)) == "fake" {
		t.Fatalf("go version = (%q, %v), want the toolchain's", out, err)
	}
}
`,
}

// TestRunsUseToolchainGo: a test that runs `go` gets the toolchain that
// built it, as under `go test`, which puts GOROOT/bin first on PATH, not
// whichever `go` is first there: one that differs fails the test when run
// alone, dropping it from the map.
func TestRunsUseToolchainGo(t *testing.T) {
	fakeGoFirstOnPath(t)
	goroot, err := GOROOT(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cp := compileFixture(t, writeModule(t, goVersionModule), "gover")
	profile := filepath.Join(t.TempDir(), "g.cov")
	if _, _, err := runCompiledTest(context.Background(), cp, "TestGo", profile, 0); err == nil {
		t.Fatal("without GOROOT, TestGo passed: the fake go isn't first on PATH, so this test proves nothing")
	}
	cp.goroot = goroot
	if _, _, err := runCompiledTest(context.Background(), cp, "TestGo", profile, 0); err != nil {
		t.Errorf("runCompiledTest with GOROOT = %v, want TestGo to run the toolchain's go", err)
	}
}
