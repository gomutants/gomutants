//go:build !windows

package runner

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
)

// rssProbeOutageAfter is how many probe failures in a row, counted across
// workers, make an outage worth reporting: a lone fork failing under memory
// pressure costs one poll of the cap, and the next poll may well work.
const rssProbeOutageAfter = 3

// rssProbe is the probe's health, shared by every worker's monitor so a run
// polling from several workers every second reports an outage once, not
// once per poll. Workers race on fails, so a report can land a failure
// early or late; it only decides when a line is printed.
var rssProbe struct {
	fails atomic.Int32 // failures in a row since the last success
	down  atomic.Bool  // an outage was reported and hasn't ended
}

// errGroupNotListed is what a groupRSSFunc returns when its scan lists no
// process of the group. That is no failure by itself, since the group may
// have exited since the monitor's tick, so pgroupRSSBytes checks whether the
// group is still alive before treating it as one.
var errGroupNotListed = errors.New("no process of the group is listed")

// pgroupRSSBytes returns the summed RSS of all processes in the given PGID,
// read by the platform's groupRSSFunc (/proc on Linux, ps elsewhere).
//
// A probe that fails reads as 0, which leaves the RSS cap unable to fire,
// so an outage is reported rather than swallowed: a missing ps once
// disabled the cap with no trace but the kernel's OOM kills. A probe that
// runs but can't see a group that is still alive (a /proc from another pid
// namespace) is the same failure: it is how issue #107 went unnoticed,
// with ps matching no rows. An outage is reported when rssProbeOutageAfter
// failures in a row start it and again when a success ends it, never per
// poll; probing goes on throughout, so the cap returns with the probe.
func pgroupRSSBytes(pgid int) int64 {
	n, err := groupRSSFunc(pgid)
	if err == nil {
		rssProbe.fails.Store(0)
		if rssProbe.down.CompareAndSwap(true, false) {
			fmt.Fprintln(os.Stderr, "gomutants: can read mutant memory again, so the per-mutant RSS cap is back on")
		}
		return n
	}
	if errors.Is(err, errGroupNotListed) && !groupAlive(pgid) {
		// The group exited between the tick and the scan. The probe had
		// nothing to see, so this neither ends an outage nor adds to one.
		return 0
	}
	if rssProbe.fails.Add(1) >= rssProbeOutageAfter && rssProbe.down.CompareAndSwap(false, true) {
		fmt.Fprintf(os.Stderr, "gomutants: can't read mutant memory, so the per-mutant RSS cap is off until it can: %v\n", err)
	}
	return 0
}

// groupAlive reports whether any process is still in group pgid: signal 0
// checks without sending anything, and only ESRCH means the group is gone.
// It calls syscall.Kill directly, not syscallKillFunc, so tests that stub
// and count kills see only the monitor's SIGKILLs.
func groupAlive(pgid int) bool {
	// gomutants:disable-next-line INTEGER_DECREMENT reason="signal -1: an invalid signal is EINVAL for a live group, which reads as alive as 0's success does, and Linux reports an empty group's ESRCH before checking the signal"
	return syscall.Kill(-pgid, 0) != syscall.ESRCH
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
