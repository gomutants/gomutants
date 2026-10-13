//go:build !windows && !linux

package runner

import (
	"errors"
	"os/exec"
	"testing"
)

// TestSumPgroupRSS pins the filter-and-sum over `ps -A -o pgid=,rss=`
// output that stands in for `ps -g` (issue #107). Kills:
//   - CONDITIONALS_NEGATION / INVERT_LOGICAL on `len(f) == 2 &&
//     bytes.Equal(...)`: other groups' rows leak in, or a short row panics.
//   - REMOVE_SELF_ASSIGNMENTS / INVERT_ASSIGNMENTS on `total +=`: only the
//     last row counts, or the sum goes negative.
//   - ARITHMETIC_BASE on `* 1024`: KB read as bytes or worse.
//   - `found = true` removed: a listed group reads as unlisted, and a live
//     mutant's group would warn the cap off.
func TestSumPgroupRSS(t *testing.T) {
	cases := []struct {
		name  string
		table string
		pgid  int
		want  int64
		found bool
	}{
		{"keeps only the group's rows", "  1 500\n 12 100\n 34 900\n 12 200\n", 12, 300 * 1024, true},
		{"pgid compared whole, not by prefix or suffix", "123 100\n 312 200\n  1 300\n", 12, 0, false},
		{"column padding as ps prints it", "    12     4096\n", 12, 4096 * 1024, true},
		{"rows with the wrong field count skipped", "12\n12 100 7\n12 50\n", 12, 50 * 1024, true},
		{"non-numeric rss reads as 0", "12 abc\n12 5\n", 12, 5 * 1024, true},
		{"a 0-rss row (a zombie) is still found", "12 0\n", 12, 0, true},
		{"no match is 0, not found", "1 500\n", 12, 0, false},
		{"empty table is 0, not found", "", 12, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, found := sumPgroupRSS([]byte(c.table), c.pgid)
			if got != c.want || found != c.found {
				t.Errorf("sumPgroupRSS(%q, %d) = (%d, %v), want (%d, %v)", c.table, c.pgid, got, found, c.want, c.found)
			}
		})
	}
}

// TestPsGroupRSSExecFails kills RETURN_ERROR_NIL / BRANCH_IF on the exec
// error: with no ps binary the probe must report the exec failure itself.
// With the early return gone, the empty output would read as an unlisted
// group, which pgroupRSSBytes keeps quiet about once the group has exited.
func TestPsGroupRSSExecFails(t *testing.T) {
	orig := psCommand
	t.Cleanup(func() { psCommand = orig })
	psCommand = "gomutants-no-such-ps"

	if got, err := psGroupRSS(1); !errors.Is(err, exec.ErrNotFound) || got != 0 {
		t.Errorf("psGroupRSS with no ps = (%d, %v), want (0, exec.ErrNotFound)", got, err)
	}
}

// TestPsGroupRSSNoGroup runs the real ps for a group no process has. It
// must report the group as unlisted rather than as an empty one, so
// pgroupRSSBytes can tell a probe that can't see a live group from one
// that has exited. Kills `!found` negated in psGroupRSS.
func TestPsGroupRSSNoGroup(t *testing.T) {
	if got, err := psGroupRSS(deadPgid); !errors.Is(err, errGroupNotListed) || got != 0 {
		t.Errorf("psGroupRSS(unlisted group) = (%d, %v), want (0, errGroupNotListed)", got, err)
	}
}
