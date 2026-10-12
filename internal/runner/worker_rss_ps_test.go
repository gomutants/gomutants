//go:build !windows && !linux

package runner

import "testing"

// TestSumPgroupRSS pins the filter-and-sum over `ps -A -o pgid=,rss=`
// output that stands in for `ps -g` (issue #107). Kills:
//   - CONDITIONALS_NEGATION / INVERT_LOGICAL on `len(f) == 2 &&
//     bytes.Equal(...)`: other groups' rows leak in, or a short row panics.
//   - REMOVE_SELF_ASSIGNMENTS / INVERT_ASSIGNMENTS on `total +=`: only the
//     last row counts, or the sum goes negative.
//   - ARITHMETIC_BASE on `* 1024`: KB read as bytes or worse.
func TestSumPgroupRSS(t *testing.T) {
	cases := []struct {
		name  string
		table string
		pgid  int
		want  int64
	}{
		{"keeps only the group's rows", "  1 500\n 12 100\n 34 900\n 12 200\n", 12, 300 * 1024},
		{"pgid compared whole, not by prefix or suffix", "123 100\n 312 200\n  1 300\n", 12, 0},
		{"column padding as ps prints it", "    12     4096\n", 12, 4096 * 1024},
		{"rows with the wrong field count skipped", "12\n12 100 7\n12 50\n", 12, 50 * 1024},
		{"non-numeric rss reads as 0", "12 abc\n12 5\n", 12, 5 * 1024},
		{"no match is 0", "1 500\n", 12, 0},
		{"empty table is 0", "", 12, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sumPgroupRSS([]byte(c.table), c.pgid); got != c.want {
				t.Errorf("sumPgroupRSS(%q, %d) = %d, want %d", c.table, c.pgid, got, c.want)
			}
		})
	}
}

// TestPsGroupRSSExecFails kills RETURN_ERROR_NIL / BRANCH_IF on the exec
// error: with no ps binary the probe must report the failure, or the RSS
// cap goes silently off.
func TestPsGroupRSSExecFails(t *testing.T) {
	orig := psCommand
	t.Cleanup(func() { psCommand = orig })
	psCommand = "gomutants-no-such-ps"

	if got, err := psGroupRSS(1); err == nil || got != 0 {
		t.Errorf("psGroupRSS with no ps = (%d, %v), want (0, error)", got, err)
	}
}
