//go:build !windows

package runner

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
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

// resetRSSProbe starts a run with a healthy probe: an outage is a property
// of one run, and one carried over from an earlier run in the same process
// would stay down, unreported, in this one.
func resetRSSProbe() {
	rssProbe.fails.Store(0)
	rssProbe.down.Store(false)
	rssSnapshot.mu.Lock()
	defer rssSnapshot.mu.Unlock()
	rssSnapshot.table, rssSnapshot.at = nil, time.Time{}
}

// rssTable maps a process group to its summed RSS in bytes. A group whose
// processes all read 0 (a zombie leader) is present at 0: listed, not
// missing.
type rssTable map[int]int64

// rssScan is one scan of the whole process table. table lists every group
// at its summed RSS. groupBytes, when set, measures one listed group more
// closely than that: on Linux it sums its processes' Pss, which splits a
// page shared by several processes among them, where RSS counts it once
// per process (issue #127: a headless browser's processes share most of
// their pages). It costs a read per process of the group, so it is made
// only for groups a monitor asks about, once per scan.
type rssScan struct {
	table      rssTable
	groupBytes func(pgid int) int64
}

// groupRSSFunc returns the memory, in bytes, that every process in group
// pgid uses together: their Pss on Linux, their RSS elsewhere. Swappable
// so tests can drive the RSS monitor without touching the host's process
// table.
var groupRSSFunc = cachedGroupRSS

// rssSnapshot is the last scan of the whole process table, shared by every
// worker's monitor. A scan reads every process on the host (21 ms and 9 MB
// at 3000 processes on Linux, a fork of ps elsewhere), and each worker
// scanning once a second for its own group multiplied that by the workers.
var rssSnapshot struct {
	mu    sync.Mutex
	table rssTable
	at    time.Time // when the scan behind table started; zero for none
	// groupBytes is the scan's (see rssScan), and measured holds the
	// groups it has measured, each now in table at its measure.
	groupBytes func(pgid int) int64
	measured   map[int]bool
}

// cachedGroupRSS reads group pgid's memory from the shared snapshot,
// scanning anew once it is older than half a poll interval, so all workers
// together scan at most twice an interval; ones that arrive mid-scan wait
// for it. A group missing from a scan that started before this call may
// have started since, so it gets a fresh scan before it reads as unlisted.
// A listed group is measured by the scan's groupBytes, if it has one, the
// first time it is asked for, and reads that from then on.
func cachedGroupRSS(pgid int) (int64, error) {
	asked := time.Now()
	rssSnapshot.mu.Lock()
	defer rssSnapshot.mu.Unlock()
	// gomutants:disable-next-line CONDITIONALS_BOUNDARY reason="elapsed time is never exactly half an interval, so > and >= scan alike"
	if time.Since(rssSnapshot.at) > monitorPollInterval/2 {
		if err := rescanRSS(); err != nil {
			return 0, err
		}
	}
	n, ok := rssSnapshot.table[pgid]
	if !ok && rssSnapshot.at.Before(asked) {
		if err := rescanRSS(); err != nil {
			return 0, err
		}
		n, ok = rssSnapshot.table[pgid]
	}
	if !ok {
		return 0, fmt.Errorf("group %d: %w", pgid, errGroupNotListed)
	}
	if rssSnapshot.groupBytes != nil && !rssSnapshot.measured[pgid] {
		n = rssSnapshot.groupBytes(pgid)
		rssSnapshot.table[pgid] = n
		rssSnapshot.measured[pgid] = true
	}
	return n, nil
}

// rescanRSS replaces the snapshot with a fresh scan; the caller holds
// rssSnapshot.mu. A failed scan is returned but never kept: several workers
// reading one kept failure would add up to an outage that never happened.
func rescanRSS() error {
	at := time.Now()
	scan, err := rssScanFunc()
	if err != nil {
		return err
	}
	rssSnapshot.table, rssSnapshot.at = scan.table, at
	rssSnapshot.groupBytes, rssSnapshot.measured = scan.groupBytes, map[int]bool{}
	return nil
}

// errGroupNotListed is what a groupRSSFunc returns when its scan lists no
// process of the group. That is no failure by itself, since the group may
// have exited since the monitor's tick, so pgroupRSSBytes checks whether the
// group is still alive before treating it as one.
var errGroupNotListed = errors.New("no process of the group is listed")

// pgroupRSSBytes returns the memory all processes in the given PGID use
// together (Pss on Linux, RSS elsewhere), read through groupRSSFunc from
// the shared scan of the process table (/proc on Linux, ps elsewhere).
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
