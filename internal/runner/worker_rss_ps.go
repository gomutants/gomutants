//go:build !windows && !linux

package runner

import (
	"bytes"
	"os/exec"
	"strconv"
)

// psCommand is the ps binary scanPsTable runs, by absolute path so nothing
// on PATH (the project under test can put its own dirs there) stands in
// for it. ps is /bin/ps on macOS and the BSDs, and /bin links to /usr/bin
// where it lives elsewhere. Swappable so a test can make the exec fail.
var psCommand = "/bin/ps"

// rssScanFunc scans the whole process table for cachedGroupRSS. Swappable
// so tests can drive the snapshot without running ps.
var rssScanFunc = scanPsTable

// scanPsTable lists every process with `ps -A` and sums each group's rows.
//
// ps can't select a process group itself on every Unix. macOS reads `-g` as
// a process group, but GNU ps reads a numeric `-g` list as session IDs, and
// procps has no other pgid selector. A mutant's group (see
// applyProcessGroup) has its own pgid but shares gomutants' session, so on
// Linux `ps -g <pgid>` matched nothing, the monitor read 0, and a runaway
// mutant was left to the kernel OOM-killer (issue #107). Linux now reads
// /proc instead (worker_rss_linux.go); listing every group here keeps the
// other Unixes off `-g` too, and one listing serves every worker.
func scanPsTable() (rssTable, error) {
	out, err := exec.Command(psCommand, "-A", "-o", "pgid=,rss=").Output()
	if err != nil {
		return nil, err
	}
	return parsePsTable(out), nil
}

// parsePsTable sums, in bytes, the RSS of each group in `ps -A -o
// pgid=,rss=` output. A row whose rss doesn't parse adds 0 but still lists
// its group; a row without exactly two fields, or whose pgid doesn't
// parse, is skipped.
func parsePsTable(out []byte) rssTable {
	table := rssTable{}
	for line := range bytes.Lines(out) {
		f := bytes.Fields(line)
		if len(f) != 2 {
			continue
		}
		pgid, err := strconv.Atoi(string(f[0]))
		if err != nil {
			continue
		}
		kb, _ := strconv.Atoi(string(f[1]))
		table[pgid] += int64(kb) * 1024 // ps rss is KB
	}
	return table
}
