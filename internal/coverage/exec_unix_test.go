//go:build !windows

package coverage

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
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
	orig := pipeDrainDelay
	t.Cleanup(func() { pipeDrainDelay = orig })
	pipeDrainDelay = time.Minute

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
// listing stops waiting for it after pipeDrainDelay.
func TestListBinTestsStopsWaitingForEscapedOutput(t *testing.T) {
	orig := pipeDrainDelay
	t.Cleanup(func() { pipeDrainDelay = orig })
	pipeDrainDelay = 200 * time.Millisecond

	if took, _ := listHanging(t, true); took > 10*time.Second {
		t.Errorf("listing took %s, want it to stop waiting soon after its timeout", took)
	}
}
