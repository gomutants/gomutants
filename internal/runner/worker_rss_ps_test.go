//go:build !windows && !linux

package runner

import (
	"errors"
	"maps"
	"os/exec"
	"testing"
)

// TestParsePsTable pins the per-group sum over `ps -A -o pgid=,rss=`
// output that stands in for `ps -g` (issue #107). Kills:
//   - CONDITIONALS_NEGATION / BRANCH_IF on `len(f) != 2` and on the pgid
//     parse: a short or long row panics or lands somewhere, or a row whose
//     pgid doesn't parse lands in group 0.
//   - REMOVE_SELF_ASSIGNMENTS / INVERT_ASSIGNMENTS on `+=`: only the last
//     row of a group counts, or the sum goes negative.
//   - ARITHMETIC_BASE on `* 1024`: KB read as bytes or worse.
func TestParsePsTable(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want rssTable
	}{
		{"sums each group's rows apart", "  1 500\n 12 100\n 34 900\n 12 200\n", rssTable{1: 500 * 1024, 12: 300 * 1024, 34: 900 * 1024}},
		{"column padding as ps prints it", "    12     4096\n", rssTable{12: 4096 * 1024}},
		{"rows with the wrong field count skipped", "12\n12 100 7\n12 50\n", rssTable{12: 50 * 1024}},
		{"non-numeric rss reads as 0, group still listed", "12 abc\n13 5\n", rssTable{12: 0, 13: 5 * 1024}},
		{"non-numeric pgid skipped", "x 5\n12 1\n", rssTable{12: 1024}},
		{"no trailing newline", "12 7", rssTable{12: 7 * 1024}},
		{"empty output", "", rssTable{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parsePsTable([]byte(c.out)); !maps.Equal(got, c.want) {
				t.Errorf("parsePsTable(%q) = %v, want %v", c.out, got, c.want)
			}
		})
	}
}

// TestScanPsTableExecFails kills RETURN_ERROR_NIL / BRANCH_IF on the exec
// error: with no ps binary the scan must report the exec failure itself.
// With the early return gone, the empty output would read as an empty
// table, every group unlisted, which pgroupRSSBytes keeps quiet about once
// the group has exited.
func TestScanPsTableExecFails(t *testing.T) {
	orig := psCommand
	t.Cleanup(func() { psCommand = orig })
	psCommand = "gomutants-no-such-ps"

	if got, err := scanPsTable(); !errors.Is(err, exec.ErrNotFound) || got != nil {
		t.Errorf("scanPsTable with no ps = (%v, %v), want (nil, exec.ErrNotFound)", got, err)
	}
}
