//go:build !windows && !linux

package runner

import (
	"bytes"
	"fmt"
	"os/exec"
	"strconv"
)

// psCommand is the ps binary psGroupRSS runs, by absolute path so nothing
// on PATH (the project under test can put its own dirs there) stands in
// for it. ps is /bin/ps on macOS and the BSDs, and /bin links to /usr/bin
// where it lives elsewhere. Swappable so a test can make the exec fail.
var psCommand = "/bin/ps"

// groupRSSFunc returns the summed RSS, in bytes, of every process in group
// pgid. Swappable so tests can drive the RSS monitor without touching the
// real ps binary or the host's process table.
var groupRSSFunc = psGroupRSS

// psGroupRSS lists every process with `ps -A` and sums the group's rows.
//
// ps can't select a process group itself on every Unix. macOS reads `-g` as
// a process group, but GNU ps reads a numeric `-g` list as session IDs, and
// procps has no other pgid selector. A mutant's group (see
// applyProcessGroup) has its own pgid but shares gomutants' session, so on
// Linux `ps -g <pgid>` matched nothing, the monitor read 0, and a runaway
// mutant was left to the kernel OOM-killer (issue #107). Linux now reads
// /proc instead (worker_rss_linux.go); listing and filtering here keeps the
// other Unixes off `-g` too.
func psGroupRSS(pgid int) (int64, error) {
	out, err := exec.Command(psCommand, "-A", "-o", "pgid=,rss=").Output()
	if err != nil {
		return 0, err
	}
	total, found := sumPgroupRSS(out, pgid)
	if !found {
		return 0, fmt.Errorf("%s -A: %w", psCommand, errGroupNotListed)
	}
	return total, nil
}

// sumPgroupRSS sums, in bytes, the RSS of each row of `ps -A -o pgid=,rss=`
// output whose pgid is pgid, and reports whether any row was the group's.
// An rss that doesn't parse reads as 0; its row still counts as found.
func sumPgroupRSS(table []byte, pgid int) (total int64, found bool) {
	want := []byte(strconv.Itoa(pgid))
	for _, line := range bytes.Split(table, []byte("\n")) {
		f := bytes.Fields(line)
		if len(f) == 2 && bytes.Equal(f[0], want) {
			found = true
			kb, _ := strconv.Atoi(string(f[1]))
			total += int64(kb) * 1024 // ps rss is KB
		}
	}
	return total, found
}
