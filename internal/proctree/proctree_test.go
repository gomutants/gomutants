package proctree

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// TestDrainDelay pins the documented default.
func TestDrainDelay(t *testing.T) {
	if DrainDelay != 5*time.Second {
		t.Errorf("DrainDelay = %v, want 5s", DrainDelay)
	}
}

// TestBoundSetsWaitDelay: Bound caps the wait for held-open output with
// DrainDelay as it is when cmd is bound.
func TestBoundSetsWaitDelay(t *testing.T) {
	orig := DrainDelay
	t.Cleanup(func() { DrainDelay = orig })
	DrainDelay = 123 * time.Millisecond
	cmd := exec.CommandContext(context.Background(), "true")
	Bound(cmd)
	if cmd.WaitDelay != 123*time.Millisecond {
		t.Errorf("WaitDelay = %v, want DrainDelay's 123ms", cmd.WaitDelay)
	}
}
