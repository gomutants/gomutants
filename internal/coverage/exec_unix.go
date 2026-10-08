//go:build !windows

package coverage

import (
	"os/exec"
	"syscall"
)

// syscallKillFunc is the kill killTreeOnCancel sends; a var so a test can
// make the group kill fail.
var syscallKillFunc = syscall.Kill

// killTreeOnCancel starts cmd in a process group of its own and, when its
// context ends, kills the whole group rather than cmd alone, so a process
// a test started can't outlive a run cut off by its timeout.
func killTreeOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// On macOS the child joins its group only after fork, so just
		// after Start the group may not exist yet: kill cmd alone then.
		if syscallKillFunc(-cmd.Process.Pid, syscall.SIGKILL) != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}
