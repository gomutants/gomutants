//go:build linux

package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// TestProcGroupRSS runs procGroupRSS over a fake procfs. Kills a match
// that leaks other groups' rows (`!=`, `||`), a sum that keeps only the
// last row (REMOVE_SELF_ASSIGNMENTS) or subtracts, a `/` for the page
// multiply, and a probe that hides an unreadable /proc (RETURN_ERROR_NIL),
// which is what would leave the RSS cap silently off.
func TestProcGroupRSS(t *testing.T) {
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
	write("34/stat", procStatLine("pkg.test", 7, 50))
	write("56/stat", procStatLine("other", 8, 1000))
	// Not a pid, so no stat file under it.
	write("cpuinfo", "processor : 0\n")
	// A pid that exited mid-scan: its dir is listed, its stat is gone.
	if err := os.Mkdir(filepath.Join(root, "78"), 0o755); err != nil {
		t.Fatal(err)
	}
	procRoot = root

	got, err := procGroupRSS(7)
	if err != nil {
		t.Fatalf("procGroupRSS: %v", err)
	}
	if want := int64(150 * os.Getpagesize()); got != want {
		t.Errorf("procGroupRSS(7) = %d, want %d (pages 100+50 of group 7 only)", got, want)
	}

	procRoot = filepath.Join(root, "missing")
	if got, err := procGroupRSS(7); err == nil || got != 0 {
		t.Errorf("procGroupRSS with no procfs = (%d, %v), want (0, error)", got, err)
	}
}
