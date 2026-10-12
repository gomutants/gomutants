//go:build !windows

package proctree

import (
	"os/exec"
	"syscall"
)

// syscallKillFunc is the kill killTreeOnCancel sends the group; a var so a
// test can make it fail.
var syscallKillFunc = syscall.Kill

// killTreeOnCancel starts cmd in a process group of its own and, when its
// context ends, kills the whole group rather than cmd alone, so a process
// cmd started can't outlive it.
func killTreeOnCancel(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		// On macOS the child joins its group only after fork, so just
		// after Start the group may not exist yet: kill cmd alone then.
		if syscallKillFunc(-cmd.Process.Pid, syscall.SIGKILL) != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}

// FailGroupKillForTesting makes the group kill of a Bound command's cancel
// fail, as it can on macOS just after Start, so the cancel kills the
// command alone, until restore is called.
func FailGroupKillForTesting() (restore func()) {
	orig := syscallKillFunc
	syscallKillFunc = func(int, syscall.Signal) error { return syscall.ESRCH }
	return func() { syscallKillFunc = orig }
}
