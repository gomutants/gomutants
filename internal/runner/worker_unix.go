//go:build !windows

package runner

import (
	"bytes"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// psOutputFunc returns the RSS, in KB and one line per process, of every
// process in group pgid. Swappable so tests can drive pgroupRSSBytes
// without touching the real ps binary or the host's process table.
//
// The error is returned alongside the filtered output rather than checked
// here: pgroupRSSBytes discards the output on any error, so a branch would
// only add an equivalent mutant.
var psOutputFunc = func(pgid int) ([]byte, error) {
	out, err := exec.Command("ps", "-A", "-o", "pgid=,rss=").Output()
	return pgroupRSSColumn(out, pgid), err
}

// pgroupRSSColumn picks, from `ps -A -o pgid=,rss=` output, the RSS of each
// process whose pgid is pgid, one per line.
//
// ps can't select a process group itself on every Unix. macOS reads `-g` as
// a process group, but GNU ps reads a numeric `-g` list as session IDs, and
// procps has no other pgid selector. A mutant's group (see
// applyProcessGroup) has its own pgid but shares gomutants' session, so on
// Linux `ps -g <pgid>` matched nothing, the monitor read 0, and a runaway
// mutant was left to the kernel OOM-killer (issue #107). Listing every
// process and filtering here works on both.
func pgroupRSSColumn(table []byte, pgid int) []byte {
	want := []byte(strconv.Itoa(pgid))
	var col []byte
	for _, line := range bytes.Split(table, []byte("\n")) {
		f := bytes.Fields(line)
		if len(f) == 2 && bytes.Equal(f[0], want) {
			col = append(col, f[1]...)
			col = append(col, '\n')
		}
	}
	return col
}

// pgroupRSSBytes returns the summed RSS of all processes in the given PGID.
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
