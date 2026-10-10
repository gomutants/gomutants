//go:build !windows

package proctree

import (
	"context"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// waitKilled fails t unless cmd exits, killed, within 10s of cancel.
func waitKilled(t *testing.T, cmd *exec.Cmd, cancel context.CancelFunc) {
	t.Helper()
	cancel()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a killed command exited cleanly")
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("cancelling didn't kill the command")
	}
}

// TestBoundKillsProcessTree: cancelling a Bound command kills the
// processes it started too, so one holding its output open can't keep
// the wait going until DrainDelay. With cmd alone killed, the sleep would
// hold stdout for 30s and the wait would last the minute's drain delay.
func TestBoundKillsProcessTree(t *testing.T) {
	orig := DrainDelay
	t.Cleanup(func() { DrainDelay = orig })
	DrainDelay = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 30 & sleep 30")
	cmd.Stdout = new(nopWriter)
	Bound(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // Let sh start its sleeps.
	start := time.Now()
	waitKilled(t, cmd, cancel)
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("wait took %v, want the whole group killed at once", took)
	}
}

// TestBoundFallsBackToProcess: when the group can't be signalled (on
// macOS, just after Start, the child may not have joined it yet), the
// cancel kills the command alone.
func TestBoundFallsBackToProcess(t *testing.T) {
	t.Cleanup(FailGroupKillForTesting())
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sleep", "60")
	Bound(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitKilled(t, cmd, cancel)
}

// TestFailGroupKillForTestingRestores: restore puts the real kill back.
func TestFailGroupKillForTestingRestores(t *testing.T) {
	restore := FailGroupKillForTesting()
	if syscallKillFunc(0, 0) == nil {
		t.Error("group kill succeeded while failing it for testing")
	}
	restore()
	if err := syscallKillFunc(syscall.Getpid(), 0); err != nil {
		t.Errorf("after restore, signal 0 to this process = %v, want the real kill", err)
	}
}

// TestBoundKeepsSysProcAttr: Bound adds its process group to attributes
// cmd already has rather than replacing them.
func TestBoundKeepsSysProcAttr(t *testing.T) {
	cmd := exec.Command("true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	Bound(cmd)
	if !cmd.SysProcAttr.Setsid || !cmd.SysProcAttr.Setpgid {
		t.Errorf("SysProcAttr = %+v, want Setsid kept and Setpgid set", cmd.SysProcAttr)
	}
}

// nopWriter discards what it's given; as a non-*os.File, it makes exec
// copy the command's output through a pipe that the wait drains.
type nopWriter struct{}

func (*nopWriter) Write(p []byte) (int, error) { return len(p), nil }
