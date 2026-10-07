//go:build windows

package coverage

import "os/exec"

// killTreeOnCancel leaves cmd as it is: Windows has no process group to
// kill, so cancellation kills the binary alone, exec's default. The
// WaitDelay testBinaryCmd sets still bounds a wait on output held open by
// a process the binary started.
func killTreeOnCancel(*exec.Cmd) {}
