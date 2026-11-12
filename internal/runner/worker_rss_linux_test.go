//go:build linux

package runner

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// procStatLine builds a /proc/<pid>/stat line: comm as given, pgrp (field
// 5) and rss (field 24) set, and neighbours that differ from both so an
// off-by-one field index reads a different number.
func procStatLine(comm string, pgrp, rss int) string {
	return fmt.Sprintf("4242 (%s) S 1 %d 99 0 -1 4194560 100 0 0 0 0 0 0 0 20 0 1 0 12345 2273280 %d "+
		"18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 0 17 3 0 0 0 0 0 0 0 0 0 0 0 0 0", comm, pgrp, rss)
}

// TestParseProcStat pins the stat-line parse. Kills:
//   - INTEGER_INCREMENT / INTEGER_DECREMENT on the field indexes 3 and 22:
//     the neighbouring fields hold other numbers.
//   - CONDITIONALS_BOUNDARY / INTEGER_* on `len(f) < 23`: a line cut just
//     after rss must parse, one cut just before it must not (or panics).
//   - CONDITIONALS_BOUNDARY / INTEGER_* on `i < 0`: a ")" at index 0 must
//     parse, and no ")" at all must not panic.
func TestParseProcStat(t *testing.T) {
	full := procStatLine("sleep", 7, 300)
	fields := strings.Fields(full) // [0] pid, [1] comm, [2] state, …, [23] rss
	cases := []struct {
		name     string
		stat     string
		pgid     int
		rssPages int
	}{
		{"real line", full, 7, 300},
		{"comm with spaces and parens", procStatLine("a) (b c", 7, 300), 7, 300},
		{"close paren at index 0", full[strings.IndexByte(full, ')'):], 7, 300},
		{"cut right after rss", strings.Join(fields[:24], " "), 7, 300},
		{"cut right before rss", strings.Join(fields[:23], " "), 0, 0},
		{"non-numeric pgrp", strings.Replace(full, " S 1 7 ", " S 1 x ", 1), 0, 300},
		{"non-numeric rss", strings.Replace(full, " 2273280 300 ", " 2273280 x ", 1), 7, 0},
		{"no close paren", "4242 sleep S 1 7", 0, 0},
		{"empty (a stat that couldn't be read)", "", 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pgid, rss := parseProcStat([]byte(c.stat))
			if pgid != c.pgid || rss != c.rssPages {
				t.Errorf("parseProcStat(%q) = (%d, %d), want (%d, %d)", c.stat, pgid, rss, c.pgid, c.rssPages)
			}
		})
	}
}

// smapsRollup builds a /proc/<pid>/smaps_rollup body in the order Linux
// prints it (as 7.0 does), with Rss, Anonymous and Pss_Anon at the kB
// values given, and every other line at a value of its own, so a field
// matched by a prefix (Pss for Pss_Anon, Anon for Anonymous) or read from
// the wrong line reads a different number.
func smapsRollup(rss, anon, pssAnon string) string {
	return "aaaac62d0000-ffffd2ae5000 ---p 00000000 00:00 0                          [rollup]\n" +
		"Rss:             " + rss + " kB\n" +
		"Pss:                1172 kB\n" +
		"Pss_Dirty:            93 kB\n" +
		"Pss_Anon:        " + pssAnon + " kB\n" +
		"Pss_File:           1080 kB\n" +
		"Pss_Shmem:             3 kB\n" +
		"Shared_Clean:          4 kB\n" +
		"Shared_Dirty:          5 kB\n" +
		"Private_Clean:      1081 kB\n" +
		"Private_Dirty:        92 kB\n" +
		"Referenced:         1176 kB\n" +
		"Anonymous:       " + anon + " kB\n" +
		"KSM:                   6 kB\n" +
		"LazyFree:              7 kB\n" +
		"AnonHugePages:         8 kB\n" +
		"ShmemPmdMapped:        9 kB\n" +
		"FilePmdMapped:        10 kB\n" +
		"Shared_Hugetlb:       11 kB\n" +
		"Private_Hugetlb:      12 kB\n" +
		"Swap:                 13 kB\n" +
		"SwapPss:              14 kB\n" +
		"Locked:               15 kB\n"
}

// TestParseRollup pins the smaps_rollup parse. Kills a field matched by a
// prefix or read from a neighbouring line, the kB-to-bytes shift dropped
// or reversed, the ok results flipped, the field index off by one, and a
// rollup missing a field read as whole, a field given twice counted for
// it too.
func TestParseRollup(t *testing.T) {
	const ki = 1 << 10
	cases := []struct {
		name   string
		rollup string
		want   rollupMem
		ok     bool
	}{
		{"real rollup", smapsRollup("1176", "92", "91"), rollupMem{1176 * ki, 92 * ki, 91 * ki}, true},
		{"any order, no trailing newline", "Pss_Anon: 3 kB\nAnonymous: 2 kB\nRss: 1 kB", rollupMem{ki, 2 * ki, 3 * ki}, true},
		{"no unit", "Rss: 1\nAnonymous: 2\nPss_Anon: 3\n", rollupMem{ki, 2 * ki, 3 * ki}, true},
		{"near the top of the int64 range", "Rss: 4503599627370496 kB\nAnonymous: 0 kB\nPss_Anon: 0 kB\n", rollupMem{1 << 62, 0, 0}, true},
		{"no Pss_Anon (before Linux 5.3)", "Rss: 1 kB\nPss: 1 kB\nAnonymous: 1 kB\nAnonHugePages: 0 kB\n", rollupMem{}, false},
		{"no Anonymous", "Rss: 1 kB\nPss_Anon: 1 kB\nAnonHugePages: 0 kB\n", rollupMem{}, false},
		{"no Rss", "Pss: 1 kB\nAnonymous: 1 kB\nPss_Anon: 1 kB\n", rollupMem{}, false},
		{"a field twice doesn't stand in for a missing one", "Rss: 1 kB\nRss: 1 kB\nAnonymous: 1 kB\n", rollupMem{}, false},
		{"a field without a value", "Rss:\nAnonymous: 1 kB\nPss_Anon: 1 kB\n", rollupMem{}, false},
		{"a non-numeric value", smapsRollup("x", "92", "91"), rollupMem{}, false},
		{"indented names are not the lines", "  Rss: 1 kB\nAnonymous: 1 kB\nPss_Anon: 1 kB\n", rollupMem{}, false},
		{"empty (a zombie, or one that has exited)", "", rollupMem{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if m, ok := parseRollup([]byte(c.rollup)); m != c.want || ok != c.ok {
				t.Errorf("parseRollup(%q) = (%+v, %v), want (%+v, %v)", c.rollup, m, ok, c.want, c.ok)
			}
		})
	}
}

// TestScanProcTable runs scanProcTable over a fake procfs. Kills a sum
// that keeps only the last row of a group (REMOVE_SELF_ASSIGNMENTS) or
// subtracts, a `/` for the page multiply, a name filter that drops pids
// starting with 9 (CONDITIONALS_BOUNDARY) or reads non-pids (self
// would count a group twice), a table that keeps pgrp 0, and a scan that
// hides an unreadable or unlistable /proc (RETURN_ERROR_NIL, BRANCH_IF),
// which would leave the RSS cap silently off.
//
// Its groupBytes sums each process's Pss_Anon and adds the largest of
// their file-backed and shmem pages (Rss less Anonymous), or counts a
// process at its RSS where its smaps_rollup can't be read: kills the
// fallback dropped (a process without a rollup would count 0, which can
// keep a runaway under the cap) or ending the sum (break for continue: the
// processes after it would count 0), the mapped pages summed or the smallest
// kept instead of the largest, Anonymous counted in place of Pss_Anon or
// added back to the mapped pages, a sum that keeps only one process, the
// processes kept under the wrong group, and the measure read into the
// listing as well.
func TestScanProcTable(t *testing.T) {
	orig := procRoot
	t.Cleanup(func() { procRoot = orig })

	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("12/stat", procStatLine("go", 7, 100))
	write("12/smaps_rollup", smapsRollup("1000", "600", "300")) // maps 400 kB
	write("13/stat", procStatLine("chrome", 7, 80))
	write("13/smaps_rollup", smapsRollup("900", "200", "150")) // maps 700 kB
	write("34/stat", procStatLine("pkg.test", 7, 50))          // no smaps_rollup
	write("56/stat", procStatLine("other", 8, 1000))
	write("56/smaps_rollup", smapsRollup("x", "1", "1"))
	// A zombie leader: 0 pages, but its group is listed.
	write("90/stat", procStatLine("zombie", 10, 0))
	// A kernel thread: pgrp 0, no group of ours.
	write("2/stat", procStatLine("kthreadd", 0, 0))
	// Not pids, so never read; self is gomutants' own pid in a real /proc.
	write("self/stat", procStatLine("self", 7, 4000))
	write("cpuinfo", "processor : 0\n")
	// A pid that exited mid-scan: its dir is listed, its stat is gone.
	if err := os.Mkdir(filepath.Join(root, "78"), 0o755); err != nil {
		t.Fatal(err)
	}
	procRoot = root

	got, err := scanProcTable()
	page := int64(os.Getpagesize())
	want := rssTable{7: 230 * page, 8: 1000 * page, 10: 0}
	if err != nil || !maps.Equal(got.table, want) {
		t.Fatalf("scanProcTable() = (%v, %v), want (%v, nil)", got.table, err, want)
	}
	for pgid, want := range map[int]int64{7: (300+150+700)<<10 + 50*page, 8: 1000 * page, 10: 0} {
		if n := got.groupBytes(pgid); n != want {
			t.Errorf("groupBytes(%d) = %d, want %d", pgid, n, want)
		}
	}

	// The scan lists a group's processes in readdir order, so the order
	// they are measured in is pinned here: one counted at its RSS comes
	// first, and the ones after it still count.
	ordered := []procMem{{pid: "34", rss: 50 * page}, {pid: "12"}, {pid: "13"}}
	if n, want := groupMem(ordered), int64(300+150+700)<<10+50*page; n != want {
		t.Errorf("groupMem(%v) = %d, want %d", ordered, n, want)
	}

	// A procfs that opens but can't be listed is reported too.
	procRoot = filepath.Join(root, "cpuinfo")
	if got, err := scanProcTable(); !errors.Is(err, syscall.ENOTDIR) || got.table != nil {
		t.Errorf("scanProcTable on a file = (%v, %v), want (nil, ENOTDIR)", got.table, err)
	}

	// An unreadable procfs is reported as itself, not as an empty table:
	// pgroupRSSBytes stays silent about an unlisted group that has exited,
	// but a probe that can't read /proc must warn regardless.
	procRoot = filepath.Join(root, "missing")
	if got, err := scanProcTable(); !errors.Is(err, fs.ErrNotExist) || got.table != nil {
		t.Errorf("scanProcTable with no procfs = (%v, %v), want (nil, ErrNotExist)", got.table, err)
	}
}

// sharedMapEnv names the file a TestSharedMapHelper process maps; unset,
// the helper is an ordinary test that skips.
const sharedMapEnv = "GOMUTANTS_TEST_SHARED_MAP"

// TestSharedMapHelper is the process TestScanProcTableSharedPages starts,
// not a test of its own: it maps the file named by sharedMapEnv, reads
// every page so each is resident in it, says "ready", and holds the
// mapping until its stdin closes.
func TestSharedMapHelper(t *testing.T) {
	path := os.Getenv(sharedMapEnv)
	if path == "" {
		t.Skip("helper process for TestScanProcTableSharedPages")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	b, err := syscall.Mmap(int(f.Fd()), 0, int(st.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		t.Fatal(err)
	}
	var sum byte
	for i := 0; i < len(b); i += os.Getpagesize() {
		sum += b[i]
	}
	fmt.Println("ready", sum) // sum is printed so the reads aren't dropped
	_, _ = io.Copy(io.Discard, os.Stdin)
}

// sharedMapFile writes a mib MiB file for TestSharedMapHelper processes to
// map, its pages filled so none reads as a zero page.
func sharedMapFile(t *testing.T, mib int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shared.bin")
	data := make([]byte, mib<<20)
	for i := range data {
		data[i] = byte(i*7 + 1)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// startSharedMapGroup starts n TestSharedMapHelper processes mapping path,
// in a new process group of their own, as a mutant's test binary and the
// processes its tests start are. It returns once each has the whole file
// resident, with the group's id and a stop that ends them all (also run at
// cleanup). It skips the test where the group's smaps_rollup can't be read
// (before Linux 5.3, in some sandboxes, a test binary that can't be read
// and so isn't dumpable): there groupBytes reads its summed RSS, as it
// should, and a check of how it counts shared pages has nothing to test.
func startSharedMapGroup(t *testing.T, path string, n int) (pgid int, stop func()) {
	t.Helper()
	var cmds []*exec.Cmd
	var stdins []io.Closer
	stop = func() {
		for _, in := range stdins {
			_ = in.Close()
		}
		for _, cmd := range cmds {
			_ = cmd.Wait()
		}
		cmds, stdins = nil, nil
	}
	t.Cleanup(stop)
	for range n {
		cmd := exec.Command(os.Args[0], "-test.run=^TestSharedMapHelper$", "-test.count=1")
		cmd.Env = append(os.Environ(), sharedMapEnv+"="+path)
		// The first process leads a new group and the others join it.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pgid}
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds, stdins = append(cmds, cmd), append(stdins, stdin)
		if pgid == 0 {
			pgid = cmd.Process.Pid
		}
		line, err := bufio.NewReader(stdout).ReadString('\n')
		if !strings.HasPrefix(line, "ready") {
			t.Fatalf("helper %d didn't map the file: %q, %v", cmd.Process.Pid, line, err)
		}
	}
	rollup, err := os.ReadFile(fmt.Sprintf("/proc/%d/smaps_rollup", pgid))
	if _, ok := parseRollup(rollup); !ok {
		t.Skipf("can't read a helper's smaps_rollup here (%v), so groups read at their summed RSS", err)
	}
	return pgid, stop
}

// TestScanProcTableSharedPages reproduces issue #127: a group whose
// processes share pages, as headless Chromium's zygote-forked processes
// do, was measured at each shared page once per process that maps it.
// Here sharedMapProcs processes of one group map the same sharedMapMiB
// file, so its real use is one copy of the file plus each process's own
// pages, while summed RSS counts the file sharedMapProcs times.
//
// The group's measure is checked against its summed RSS from the same
// scan, not a fixed bound: each helper's own memory varies (a -race
// binary has some 25 MiB of its own), but the sharedMapProcs-1 extra
// copies of the file are what summed RSS adds, and the measure must drop
// at least three quarters of them.
func TestScanProcTableSharedPages(t *testing.T) {
	const sharedMapProcs, sharedMapMiB = 8, 64
	pgid, _ := startSharedMapGroup(t, sharedMapFile(t, sharedMapMiB), sharedMapProcs)

	scan, err := scanProcTable()
	if err != nil {
		t.Fatal(err)
	}
	rss, got := scan.table[pgid]>>20, scan.groupBytes(pgid)>>20
	const extraCopies = (sharedMapProcs - 1) * sharedMapMiB
	if rss-got < extraCopies*3/4 {
		t.Errorf("%d processes sharing one %d MiB mapping read %d MiB, against %d MiB of summed RSS, want at least %d MiB less: "+
			"shared pages are counted once per process (#127)", sharedMapProcs, sharedMapMiB, got, rss, extraCopies*3/4)
	}
	// The group is still measured at all: one copy of the file is in it.
	if got < sharedMapMiB {
		t.Errorf("%d processes mapping a %d MiB file read %d MiB, want at least that", sharedMapProcs, sharedMapMiB, got)
	}
}

// TestScanProcTableOtherGroupsSharing: a group reads the same while another
// group maps the same file, as two workers' mutants map one browser binary
// and its libraries. Summing each process's Pss read group A at a quarter
// of the file while group B's three processes mapped it too, and at the
// whole file once they exited, so whether a mutant near the cap was killed
// depended on what other workers ran beside it. Kills a measure that
// splits file-backed pages with other groups (Pss, Pss_File).
func TestScanProcTableOtherGroupsSharing(t *testing.T) {
	const mib = 64
	path := sharedMapFile(t, mib)
	a, _ := startSharedMapGroup(t, path, 1)
	measure := func() int64 {
		t.Helper()
		scan, err := scanProcTable()
		if err != nil {
			t.Fatal(err)
		}
		return scan.groupBytes(a) >> 20
	}
	alone := measure()
	_, stopB := startSharedMapGroup(t, path, 3)
	beside := measure()
	stopB()
	// Group A's own memory may move a little between scans (its runtime
	// is idle, not stopped); a split file would move it by mib*3/4.
	if d := beside - alone; d < -mib/8 || d > mib/8 {
		t.Errorf("group A, one process mapping a %d MiB file, read %d MiB alone and %d MiB while another group mapped the file too, want within %d MiB",
			mib, alone, beside, mib/8)
	}
	if alone < mib {
		t.Errorf("group A, mapping a %d MiB file, read %d MiB, want at least that", mib, alone)
	}
}
