//go:build !windows

package runner

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// psOutputFunc returns ps's stdout for a given pgid. Swappable so tests
// can drive pgroupRSSBytes without touching the real ps binary or the
// host's process table.
var psOutputFunc = func(pgid int) ([]byte, error) {
	return exec.Command("ps", "-o", "rss=", "-g", strconv.Itoa(pgid)).Output()
}

// pgroupRSSBytes returns the summed RSS of all processes in the given PGID.
// Uses `ps -g` (BSD/macOS flag) which is also supported on Linux GNU ps.
//
// The accumulator is structured as a single `if err == nil` add so the
// continue-on-empty / continue-on-parse-error branches don't surface as
// equivalent mutants (in both, n stays 0 and total += 0 is a no-op).
func pgroupRSSBytes(pgid int) int64 {
	out, err := psOutputFunc(pgid)
	if err != nil {
		return 0
	}
	var total int64
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		n, err := strconv.ParseInt(strings.TrimSpace(line), 10, 64)
		if err == nil {
			total += n * 1024 // ps rss is KB
		}
	}
	return total
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
