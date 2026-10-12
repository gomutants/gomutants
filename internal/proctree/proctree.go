// Package proctree bounds how long a command and the processes it started
// can outlast the command's context.
package proctree

import (
	"os/exec"
	"time"
)

// DrainDelay bounds how long a command waits, once it has exited or been
// killed, for output pipes a process it started still holds open. A var
// so tests can shorten it.
var DrainDelay = 5 * time.Second

// Bound makes the end of cmd's context kill cmd along with every process
// it started (see killTreeOnCancel), and caps with DrainDelay the wait for
// output held open by one that escaped, so a command that hangs can't
// outlast its context for long. Call it before cmd starts.
func Bound(cmd *exec.Cmd) {
	killTreeOnCancel(cmd)
	cmd.WaitDelay = DrainDelay
}
