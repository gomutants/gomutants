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
// The scan also keeps each group's processes, for its groupBytes to
// measure them by their smaps_rollup (see groupMem).
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
	return rssScan{table: table, groupBytes: func(pgid int) int64 { return groupMem(procs[pgid]) }}, nil
}

// procMem is one process of a scanned group: its pid, as /proc names it,
// and its RSS in bytes, as its stat read.
type procMem struct {
	pid string
	rss int64
}

// groupMem returns the memory procs use together, from each one's
// /proc/<pid>/smaps_rollup, counting a page they share once, where their
// summed RSS counts it once per process (issue #127: a headless browser's
// processes share most of their pages). It splits no page with another
// group, so what other workers' mutants run at the same time doesn't
// change it:
//   - Anonymous memory (heaps, stacks) counts at its Pss, which splits a
//     page several processes map among them. Only fork shares an anonymous
//     page, so its sharers are all in the group, and their shares add up
//     to the page once.
//   - File-backed and shmem pages (binaries, libraries, mapped files)
//     count as the largest process maps them. Their Pss would be split
//     with every process on the host that maps the same file, another
//     group's too, so a group would read less while another worker's
//     mutant maps its binaries and more once that exits. The largest
//     process maps most of what its group shares (a headless browser's
//     binary and libraries), and misses what other processes alone map.
//
// A process whose smaps_rollup can't be read counts at its RSS: the file
// is missing before Linux 4.14 and in some sandboxes, has no Pss_Anon
// before 5.3, is unreadable for a process that made itself non-dumpable,
// and empty for a zombie or one that has exited since the scan. That reads
// high, never low, so the cap can still fire, and it is no outage of the
// probe.
func groupMem(procs []procMem) int64 {
	// gomutants:disable-next-line INTEGER_INCREMENT,INTEGER_DECREMENT reason="buffer capacity: any size well above a ~1 KiB smaps_rollup reads the same"
	buf := make([]byte, 4096) // smaps_rollup runs ~800 bytes
	var own, mapped int64
	for _, p := range procs {
		m, ok := parseRollup(readStat(procRoot+"/"+p.pid+"/smaps_rollup", buf))
		if !ok {
			own += p.rss
			continue
		}
		own += m.pssAnon
		mapped = max(mapped, m.rss-m.anon)
	}
	return own + mapped
}

// rollupMem is what groupMem reads of a smaps_rollup, in bytes: Rss, the
// Anonymous part of it, and that part's Pss.
type rollupMem struct {
	rss, anon, pssAnon int64
}

// parseRollup returns the Rss, Anonymous and Pss_Anon lines of a
// /proc/<pid>/smaps_rollup, in bytes, and whether it has each of them, with
// a value that parses. A line counts by its whole name: Pss is not
// Pss_Anon, nor AnonHugePages Anonymous.
func parseRollup(rollup []byte) (m rollupMem, ok bool) {
	var seen int // a bit per field
	for line := range bytes.Lines(rollup) {
		name, rest, _ := bytes.Cut(line, []byte(":"))
		var dst *int64
		var bit int
		switch string(name) {
		case "Rss":
			dst, bit = &m.rss, 1
		case "Anonymous":
			dst, bit = &m.anon, 2
		case "Pss_Anon":
			dst, bit = &m.pssAnon, 4
		default:
			continue
		}
		f := bytes.Fields(rest) // "1234", "kB"
		if len(f) == 0 {
			return rollupMem{}, false
		}
		kb, err := strconv.Atoi(string(f[0]))
		if err != nil {
			return rollupMem{}, false
		}
		*dst = int64(kb) << 10
		seen |= bit
	}
	if seen != 7 {
		return rollupMem{}, false
	}
	return m, true
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
