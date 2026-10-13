//go:build linux

package runner

import (
	"bytes"
	"os"
	"strconv"
)

// procRoot is the procfs mount scanProcTable reads. Swappable so tests can
// point it at a fake tree.
var procRoot = "/proc"

// rssScanFunc scans the whole process table for cachedGroupRSS. Swappable
// so tests can drive the snapshot without touching the host's processes.
var rssScanFunc = scanProcTable

// scanProcTable reads every process's group and RSS straight from /proc,
// one stat file per process, rather than through ps: slim and distroless
// images ship no ps, and a failed exec once left the RSS cap silently off.
// procps can't select a process group by itself anyway (see
// worker_rss_ps.go, issue #107).
func scanProcTable() (rssTable, error) {
	dir, err := os.Open(procRoot)
	if err != nil {
		return nil, err
	}
	// gomutants:disable-next-line INTEGER_INCREMENT,INTEGER_DECREMENT reason="-2 and 0 read every name as -1 does: any n <= 0 means all"
	names, err := dir.Readdirnames(-1) // unsorted, unlike os.ReadDir
	_ = dir.Close()
	if err != nil {
		return nil, err
	}
	page := int64(os.Getpagesize())
	table := rssTable{}
	// gomutants:disable-next-line INTEGER_INCREMENT,INTEGER_DECREMENT reason="buffer capacity: any size well above a ~300-byte stat line reads the same"
	buf := make([]byte, 1024) // a stat line runs ~300 bytes
	for _, name := range names {
		// Only a pid has a stat to read. Every other entry is named by a
		// word (cpuinfo, sys, and self, which is gomutants itself, never a
		// mutant's group), so it is skipped before any I/O.
		if name[0] > '9' {
			continue
		}
		// pgrp 0 is a kernel thread's, or a stat that didn't parse: a
		// process that exited mid-scan has none to read.
		if g, rss := parseProcStat(readStat(procRoot+"/"+name+"/stat", buf)); g != 0 {
			table[g] += int64(rss) * page
		}
	}
	return table, nil
}

// readStat reads the stat file at path into buf and returns what it read,
// or nothing if the process has exited.
func readStat(path string, buf []byte) []byte {
	f, err := os.Open(path)
	if err == nil {
		n, _ := f.Read(buf)
		_ = f.Close()
		return buf[:n]
	}
	return nil
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
