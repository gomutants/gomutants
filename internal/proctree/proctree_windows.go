//go:build windows

package proctree

import "os/exec"

// killTreeOnCancel leaves cmd as it is: Windows has no process group to
// kill, so cancellation kills cmd alone, exec's default. The WaitDelay
// Bound sets still bounds a wait on output held open by a process cmd
// started.
func killTreeOnCancel(*exec.Cmd) {
	// Intentionally empty: see above.
}
