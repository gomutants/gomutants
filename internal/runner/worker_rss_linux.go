//go:build linux

package runner

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// procRoot is the procfs mount procGroupRSS scans. Swappable so tests can
// point it at a fake tree.
var procRoot = "/proc"

// groupRSSFunc returns the summed RSS, in bytes, of every process in group
// pgid. Swappable so tests can drive the RSS monitor without touching the
// host's process table.
var groupRSSFunc = procGroupRSS

// procGroupRSS reads the group's RSS straight from /proc, one stat file per
// process, rather than through ps: slim and distroless images ship no ps,
// and a failed exec once left the RSS cap silently off. procps can't select
// a process group by itself anyway (see worker_rss_ps.go, issue #107).
func procGroupRSS(pgid int) (int64, error) {
	ents, err := os.ReadDir(procRoot)
	if err != nil {
		return 0, err
	}
	var pages int64
	found := false
	for _, e := range ents {
		// Entries that aren't pids (cpuinfo, sys) have no stat file, and a
		// process that exits mid-scan no longer has one: either way
		// ReadFile leaves stat nil, which parses as no group. self and
		// thread-self are gomutants itself, whose group is never a mutant's.
		stat, _ := os.ReadFile(filepath.Join(procRoot, e.Name(), "stat"))
		if g, rss := parseProcStat(stat); g == pgid {
			found = true // even at 0 pages: a zombie leader is still listed
			pages += int64(rss)
		}
	}
	if !found {
		return 0, fmt.Errorf("%s: %w", procRoot, errGroupNotListed)
	}
	return pages * int64(os.Getpagesize()), nil
}

// parseProcStat returns the pgrp and rss (in pages) fields of a
// /proc/<pid>/stat line, or zeros if it isn't one. A field that doesn't
// parse reads as 0: pgid 0 is no mutant's group, and 0 pages adds nothing.
func parseProcStat(stat []byte) (pgid, rssPages int) {
	// comm (field 2) is parenthesized and may itself hold spaces and ")",
	// so fields are counted from the last ")".
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, 0
	}
	f := bytes.Fields(stat[i:]) // f[0] is ")", f[n] is field n+2
	if len(f) < 23 {
		return 0, 0
	}
	pgid, _ = strconv.Atoi(string(f[3]))      // field 5, pgrp
	rssPages, _ = strconv.Atoi(string(f[22])) // field 24, rss
	return pgid, rssPages
}
