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
//
// The scan also keeps each group's processes, for its groupBytes to sum
// their Pss (see groupPss).
func scanProcTable() (rssScan, error) {
	dir, err := os.Open(procRoot)
	if err != nil {
		return rssScan{}, err
	}
	// gomutants:disable-next-line INTEGER_INCREMENT,INTEGER_DECREMENT reason="-2 and 0 read every name as -1 does: any n <= 0 means all"
	names, err := dir.Readdirnames(-1) // unsorted, unlike os.ReadDir
	_ = dir.Close()
	if err != nil {
		return rssScan{}, err
	}
	page := int64(os.Getpagesize())
	table := rssTable{}
	procs := map[int][]procMem{}
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
			procs[g] = append(procs[g], procMem{pid: name, rss: int64(rss) * page})
		}
	}
	return rssScan{table: table, groupBytes: func(pgid int) int64 { return groupPss(procs[pgid]) }}, nil
}

// procMem is one process of a scanned group: its pid, as /proc names it,
// and its RSS in bytes, as its stat read.
type procMem struct {
	pid string
	rss int64
}

// groupPss returns the memory procs use together: the sum of each one's
// Pss, from /proc/<pid>/smaps_rollup. Pss splits a page that several
// processes map among them, so a page a group's processes share counts
// once in all, where their summed RSS counts it once per process (issue
// #127). A process whose Pss can't be read counts at its RSS: the file is
// missing before Linux 4.14 and in some sandboxes, unreadable for a
// process that made itself non-dumpable, and empty for a zombie or one
// that has exited since the scan. That reads high, never low, so the cap
// can still fire, and it is no outage of the probe.
func groupPss(procs []procMem) int64 {
	// gomutants:disable-next-line INTEGER_INCREMENT,INTEGER_DECREMENT reason="buffer capacity: any size well above a ~1 KiB smaps_rollup reads the same"
	buf := make([]byte, 4096) // smaps_rollup runs ~800 bytes
	var n int64
	for _, p := range procs {
		if kb, ok := parsePss(readStat(procRoot+"/"+p.pid+"/smaps_rollup", buf)); ok {
			n += kb << 10
		} else {
			n += p.rss
		}
	}
	return n
}

// parsePss returns the value, in kB, of the "Pss:" line of a
// /proc/<pid>/smaps_rollup, and whether it has one that parses. Only the
// line named exactly Pss counts: Pss_Anon, Pss_File, Pss_Shmem and
// Pss_Dirty are parts of it.
func parsePss(rollup []byte) (kb int64, ok bool) {
	for line := range bytes.Lines(rollup) {
		rest, found := bytes.CutPrefix(line, []byte("Pss:"))
		if !found {
			continue
		}
		f := bytes.Fields(rest) // "1234", "kB"
		if len(f) == 0 {
			return 0, false
		}
		v, err := strconv.ParseInt(string(f[0]), 10, 64)
		return v, err == nil
	}
	return 0, false
}

// readStat reads the /proc file at path (a stat, or a smaps_rollup) into
// buf and returns what it read, or nothing if the process has exited.
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
