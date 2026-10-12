//go:build !windows

package runner

import (
	"fmt"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
)

// rssProbeWarned records that pgroupRSSBytes has reported a failed probe,
// so a run with several workers polling every second warns once, not once
// per poll.
var rssProbeWarned atomic.Bool

// pgroupRSSBytes returns the summed RSS of all processes in the given PGID,
// read by the platform's groupRSSFunc (/proc on Linux, ps elsewhere).
//
// A probe that fails reads as 0, which leaves the RSS cap unable to fire,
// so the first failure is reported rather than swallowed: a missing ps
// once disabled the cap with no trace but the kernel's OOM kills.
// groupRSSFunc returns 0 with its error, so there is no separate early
// return; it would only be an equivalent mutant.
func pgroupRSSBytes(pgid int) int64 {
	n, err := groupRSSFunc(pgid)
	if err != nil && rssProbeWarned.CompareAndSwap(false, true) {
		fmt.Fprintf(os.Stderr, "gomutants: can't read mutant memory, so the per-mutant RSS cap is off for this run: %v\n", err)
	}
	return n
}

// syscallKillFunc is the indirection used by killPgroup; swappable so tests
// can verify the negative-pgid sign flip without sending real signals.
var syscallKillFunc = syscall.Kill

// syscallGetpgidFunc is the indirection used by processGroup. Swappable so
// a test can simulate the macOS race window where Getpgid transiently fails
// — exercising the cmd.Process.Pid fallback path.
var syscallGetpgidFunc = syscall.Getpgid

// killPgroup sends SIGKILL to the entire process group.
func killPgroup(pgid int) {
	_ = syscallKillFunc(-pgid, syscall.SIGKILL)
}

// applyProcessGroup configures cmd so its children land in a new process
// group, letting killPgroup tear down the whole tree by negating the pgid.
func applyProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// processGroup resolves the process group leader for pid. With Setpgid:true,
// Go invokes setpgid in the parent on Linux before returning from Start, but
// on macOS it happens in the child post-fork, so there's a brief window
// where cmd.Process.Pid and the group leader's pid may differ. On error,
// fall back to pid so killPgroup still targets *something* (better to kill
// the leader than nothing).
func processGroup(pid int) int {
	pgid, err := syscallGetpgidFunc(pid)
	if err != nil {
		return pid
	}
	return pgid
}
