package discover

import (
	"context"
	"errors"
	"fmt"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gomutants/gomutants/internal/mutator"
)

func TestParseUnifiedDiffSingleHunk(t *testing.T) {
	in := `diff --git a/foo.go b/foo.go
--- a/foo.go
+++ b/foo.go
@@ -10,0 +11,2 @@ ctx
+added line
+another
`
	got, err := ParseUnifiedDiff(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]LineRange{
		"foo.go": {{Start: 11, End: 12}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseUnifiedDiffSingleLineNoCount(t *testing.T) {
	// `+11` with no count means count=1.
	in := `diff --git a/x.go b/x.go
--- a/x.go
+++ b/x.go
@@ -10 +11 @@
-old
+new
`
	got, err := ParseUnifiedDiff(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]LineRange{
		"x.go": {{Start: 11, End: 11}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseUnifiedDiffMultipleHunks(t *testing.T) {
	in := `diff --git a/pkg/a.go b/pkg/a.go
--- a/pkg/a.go
+++ b/pkg/a.go
@@ -5,0 +6,1 @@
+x
@@ -20,1 +21,3 @@
-y
+a
+b
+c
`
	got, err := ParseUnifiedDiff(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]LineRange{
		"pkg/a.go": {{Start: 6, End: 6}, {Start: 21, End: 23}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseUnifiedDiffDeletionOnlyHunkSkipped(t *testing.T) {
	// `+10,0` → deletion at position 10; nothing to mutate.
	in := `diff --git a/x.go b/x.go
--- a/x.go
+++ b/x.go
@@ -10,2 +10,0 @@
-gone
-also gone
`
	got, err := ParseUnifiedDiff(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected no ranges (deletion-only), got %v", got)
	}
}

func TestParseUnifiedDiffDeletedFileSkipped(t *testing.T) {
	in := `diff --git a/dead.go b/dead.go
deleted file mode 100644
--- a/dead.go
+++ /dev/null
@@ -1,3 +0,0 @@
-package x
-
-func F() {}
`
	got, err := ParseUnifiedDiff(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("deleted file should produce no ranges, got %v", got)
	}
}

func TestParseUnifiedDiffNewFile(t *testing.T) {
	in := `diff --git a/new.go b/new.go
new file mode 100644
--- /dev/null
+++ b/new.go
@@ -0,0 +1,3 @@
+package x
+
+func F() {}
`
	got, err := ParseUnifiedDiff(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]LineRange{
		"new.go": {{Start: 1, End: 3}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseUnifiedDiffRenameUsesNewPath(t *testing.T) {
	in := `diff --git a/old.go b/sub/new.go
similarity index 90%
rename from old.go
rename to sub/new.go
--- a/old.go
+++ b/sub/new.go
@@ -3 +3 @@
-old
+new
`
	got, err := ParseUnifiedDiff(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]LineRange{
		"sub/new.go": {{Start: 3, End: 3}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseUnifiedDiffMultipleFiles(t *testing.T) {
	in := `diff --git a/a.go b/a.go
--- a/a.go
+++ b/a.go
@@ -1 +1 @@
-x
+y
diff --git a/b.go b/b.go
--- a/b.go
+++ b/b.go
@@ -5,0 +6,2 @@
+m
+n
`
	got, err := ParseUnifiedDiff(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]LineRange{
		"a.go": {{Start: 1, End: 1}},
		"b.go": {{Start: 6, End: 7}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// errReader returns the canned bytes once, then a hard error. Used to
// exercise ParseUnifiedDiff's bufio.Scanner.Err() path.
type errReader struct {
	data []byte
	used bool
}

func (r *errReader) Read(p []byte) (int, error) {
	if r.used {
		return 0, fmt.Errorf("injected read error")
	}
	r.used = true
	n := copy(p, r.data)
	return n, nil
}

func TestParseUnifiedDiffScannerError(t *testing.T) {
	// Bytes without a trailing newline + an error on the next read makes
	// bufio.Scanner stop with sc.Err() non-nil.
	r := &errReader{data: []byte("diff --git a/x.go b/x.go")}
	_, err := ParseUnifiedDiff(r)
	if err == nil {
		t.Fatal("expected error from scanner")
	}
	if !strings.Contains(err.Error(), "reading diff") {
		t.Errorf("error should mention 'reading diff', got: %v", err)
	}
}

func TestFilterByDiffRelErrors(t *testing.T) {
	// filepath.Rel returns an error when one path is absolute and the
	// other is relative — exercises the rel == "" skip path.
	ranges := map[string][]LineRange{
		"a.go": {{Start: 1, End: 1}},
	}
	mutants := []mutator.Mutant{
		{ID: 1, File: "/abs/a.go", Line: 1},
		{ID: 2, File: "/abs/a.go", Line: 1},
	}
	got := FilterByDiff(mutants, ranges, "relative-root")
	if len(got) != 0 {
		t.Errorf("expected 0 mutants when Rel fails, got %d", len(got))
	}
}

func TestParseUnifiedDiffEmpty(t *testing.T) {
	got, err := ParseUnifiedDiff(strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("empty diff should yield no ranges, got %v", got)
	}
}

func TestParseHunkHeaderMalformed(t *testing.T) {
	cases := []string{
		"@@ no plus marker @@",
		"@@+10",               // "+" present but no space after the range token
		"@@ -1,2 +abc,def @@", // non-numeric start
		"@@ -1,2 +1,abc @@",   // non-numeric count
		"@@ -1,2 +0,1 @@",     // start <= 0
	}
	for _, c := range cases {
		if _, ok := parseHunkHeader(c); ok {
			t.Errorf("parseHunkHeader(%q) should have returned ok=false", c)
		}
	}
}

// TestParseUnifiedDiffTimestampStripped covers the tab-stripping path in
// the "+++" handler. Some diff producers append "\t<timestamp>" to file
// headers; the parser must trim that before comparing to "/dev/null" or
// using the path as a map key.
func TestParseUnifiedDiffTimestampStripped(t *testing.T) {
	in := "diff --git a/x.go b/x.go\n" +
		"--- a/x.go\t2024-01-01 00:00:00\n" +
		"+++ b/x.go\t2024-01-02 00:00:00\n" +
		"@@ -1 +1 @@\n" +
		"-old\n" +
		"+new\n"
	got, err := ParseUnifiedDiff(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]LineRange{
		"x.go": {{Start: 1, End: 1}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestStripDiffPrefix(t *testing.T) {
	cases := map[string]string{
		"a/foo.go":     "foo.go",
		"b/sub/foo.go": "sub/foo.go",
		"foo.go":       "foo.go", // diff.noprefix
		"":             "",
	}
	for in, want := range cases {
		if got := stripDiffPrefix(in); got != want {
			t.Errorf("stripDiffPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLineRangeContains(t *testing.T) {
	r := LineRange{Start: 5, End: 10}
	cases := map[int]bool{
		4:  false,
		5:  true,
		7:  true,
		10: true,
		11: false,
	}
	for line, want := range cases {
		if got := r.Contains(line); got != want {
			t.Errorf("Contains(%d) = %v, want %v", line, got, want)
		}
	}
}

func TestFilterByDiffKeepsInRange(t *testing.T) {
	gitRoot := "/repo"
	ranges := map[string][]LineRange{
		"pkg/a.go": {{Start: 10, End: 12}, {Start: 20, End: 20}},
	}
	mutants := []mutator.Mutant{
		{ID: 1, File: "/repo/pkg/a.go", Line: 9},  // before
		{ID: 2, File: "/repo/pkg/a.go", Line: 10}, // start of range
		{ID: 3, File: "/repo/pkg/a.go", Line: 12}, // end of range
		{ID: 4, File: "/repo/pkg/a.go", Line: 13}, // gap
		{ID: 5, File: "/repo/pkg/a.go", Line: 20}, // second range
		{ID: 6, File: "/repo/pkg/b.go", Line: 10}, // file not in diff
	}
	// Snapshot the input header so we can verify the function did not
	// truncate the caller's slice.
	inputLen := len(mutants)
	got := FilterByDiff(mutants, ranges, gitRoot)
	if len(mutants) != inputLen {
		t.Errorf("input slice was truncated: len=%d, want %d", len(mutants), inputLen)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 mutants kept, got %d: %+v", len(got), got)
	}
	wantLines := []int{10, 12, 20}
	wantIDs := []int{2, 3, 5}
	for i, m := range got {
		if m.Line != wantLines[i] {
			t.Errorf("mutant[%d].Line = %d, want %d", i, m.Line, wantLines[i])
		}
		if m.ID != wantIDs[i] {
			t.Errorf("mutant[%d].ID = %d, want %d (original IDs preserved)", i, m.ID, wantIDs[i])
		}
	}
}

func TestFilterByDiffEmptyRangesReturnsNone(t *testing.T) {
	mutants := []mutator.Mutant{
		{ID: 1, File: "/repo/a.go", Line: 1},
	}
	got := FilterByDiff(mutants, map[string][]LineRange{}, "/repo")
	if len(got) != 0 {
		t.Errorf("expected 0 mutants when ranges is empty, got %d", len(got))
	}
}

func TestFilterByDiffPathOutsideGitRoot(t *testing.T) {
	// File path outside git root: filepath.Rel succeeds but produces "../..."
	// which won't match any diff entry. Mutant is dropped.
	ranges := map[string][]LineRange{
		"a.go": {{Start: 1, End: 1}},
	}
	mutants := []mutator.Mutant{
		{ID: 1, File: "/elsewhere/a.go", Line: 1},
	}
	got := FilterByDiff(mutants, ranges, "/repo")
	if len(got) != 0 {
		t.Errorf("expected 0 mutants outside git root, got %d", len(got))
	}
}

// gitRepo is a throwaway repository for the tests that need real git
// history. The author and committer identities are pinned so `git commit`
// works on a machine with no global git config, such as a CI runner.
type gitRepo struct {
	t   *testing.T
	dir string
}

// newGitRepo initialises an empty repository on branch main, skipping the
// test when git is not installed.
func newGitRepo(t *testing.T) *gitRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	r := &gitRepo{t: t, dir: t.TempDir()}
	r.git("init", "-q", "-b", "main")
	return r
}

// git runs one git command in the repository, failing the test if it exits
// non-zero. The context is created per call rather than held on the struct:
// a stored Context outlives the call it belongs to and cannot be scoped or
// cancelled by the caller.
func (r *gitRepo) git(args ...string) {
	r.t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// write creates or overwrites a file in the working tree.
func (r *gitRepo) write(name, content string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, name), []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// commit stages the whole working tree and records it.
func (r *gitRepo) commit(message string) {
	r.t.Helper()
	r.git("add", ".")
	r.git("commit", "-q", "-m", message)
}

// TestRunGitDiffIntegration constructs a tiny git repo, makes a commit, edits
// a file in the working tree, then runs RunGitDiff against HEAD.
func TestRunGitDiffIntegration(t *testing.T) {
	r := newGitRepo(t)
	r.write("f.go", "package p\nfunc F() int { return 1 + 2 }\n")
	r.commit("init")
	// Edit line 2 in working tree.
	r.write("f.go", "package p\nfunc F() int { return 3 + 4 }\n")

	got, err := RunGitDiff(context.Background(), r.dir, "HEAD")
	if err != nil {
		t.Fatalf("RunGitDiff: %v", err)
	}
	if len(got["f.go"]) == 0 {
		t.Fatalf("expected ranges for f.go, got %v", got)
	}
	if got["f.go"][0].Start != 2 {
		t.Errorf("expected change on line 2, got %v", got["f.go"][0])
	}

	// GitRoot should resolve to dir (canonicalized — macOS /private/ etc.).
	root, err := GitRoot(context.Background(), r.dir)
	if err != nil {
		t.Fatalf("GitRoot: %v", err)
	}
	if !strings.HasSuffix(root, filepath.Base(r.dir)) {
		t.Errorf("GitRoot=%q, expected suffix %q", root, filepath.Base(r.dir))
	}
}

// TestRunGitDiffBadRef exercises the cmd.Run() error path: passing a ref
// that doesn't exist makes git exit non-zero, and the error must wrap
// stderr.
func TestRunGitDiffBadRef(t *testing.T) {
	r := newGitRepo(t)
	r.write("f.go", "package p\n")
	r.commit("init")

	_, err := RunGitDiff(context.Background(), r.dir, "definitely-not-a-real-ref")
	if err == nil {
		t.Fatal("expected error for nonexistent ref")
	}
	if !strings.Contains(err.Error(), "definitely-not-a-real-ref") {
		t.Errorf("error should mention the bad ref, got: %v", err)
	}
	// Friendlier-hint path: stderr contains "unknown revision" or
	// "bad revision" → we suggest `git fetch`.
	if !strings.Contains(err.Error(), "git fetch") {
		t.Errorf("error should hint at `git fetch` for unknown ref, got: %v", err)
	}
}

// TestRunGitDiffUsesMergeBase is the regression test for scoping a run to a
// pull request's own lines. It builds a repo where `main` has moved on after
// the feature branch forked, modifying a line in a file the branch also has.
// Against main's *tip* that line reads as changed — the branch still holds
// the pre-modification version — and gets attributed to work the branch
// never did. Against the merge base it is correctly absent.
//
// The edit on main has to modify an existing shared file rather than add a
// new one: a file absent from the branch shows up in a two-dot diff as a
// deletion, which contributes no ranges and would make this test vacuous.
func TestRunGitDiffUsesMergeBase(t *testing.T) {
	r := newGitRepo(t)
	r.write("ours.go", "package p\n\nfunc A() int { return 1 }\n")
	r.write("theirs.go", "package p\n\nfunc B() int { return 2 }\n")
	r.commit("init")

	// Fork the feature branch here.
	r.git("checkout", "-q", "-b", "feature")

	// main moves on, editing a file the branch also carries.
	r.git("checkout", "-q", "main")
	r.write("theirs.go", "package p\n\nfunc B() int { return 22 }\n")
	r.commit("their work")

	// The branch changes one line of its own, and stays behind main.
	r.git("checkout", "-q", "feature")
	r.write("ours.go", "package p\n\nfunc A() int { return 99 }\n")
	r.commit("our work")

	got, err := RunGitDiff(context.Background(), r.dir, "main")
	if err != nil {
		t.Fatalf("RunGitDiff: %v", err)
	}
	if len(got["ours.go"]) == 0 {
		t.Errorf("expected the branch's own change to ours.go, got %v", got)
	}
	if ranges, ok := got["theirs.go"]; ok {
		t.Errorf("theirs.go landed on main after the fork and must not be attributed to this branch, got %v", ranges)
	}
}

// TestRunGitDiffNoMergeBase covers the no-common-ancestor path. Reporting it
// is the point: the alternative — quietly falling back to a diff against the
// ref's tip — is the over-broad scope this whole change exists to remove, and
// silently producing it would be worse than saying so. The realistic cause is
// a shallow clone, so the error names that.
func TestRunGitDiffNoMergeBase(t *testing.T) {
	r := newGitRepo(t)
	r.write("f.go", "package p\n\nfunc A() int { return 1 }\n")
	r.commit("init")

	// An orphan branch shares no ancestor with main.
	r.git("checkout", "-q", "--orphan", "orphan")
	r.write("f.go", "package p\n\nfunc A() int { return 2 }\n")
	r.commit("orphan root")

	_, err := RunGitDiff(context.Background(), r.dir, "main")
	if err == nil {
		t.Fatal("expected an error when no merge base exists")
	}
	if !strings.Contains(err.Error(), "fetch-depth: 0") {
		t.Errorf("error should hint at deepening a shallow clone, got: %v", err)
	}
	// The hint wraps git's own failure rather than replacing it, so a caller
	// can still inspect the exit status behind the advice.
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Errorf("error should unwrap to git's *exec.ExitError, got %T", err)
	}
}

// TestRunGitDiffNonRepoErrorNoHint: when git fails for a reason other
// than a bad ref (e.g. not a git repo), we don't append the fetch hint.
func TestRunGitDiffNonRepoErrorNoHint(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", dir)
	_, err := RunGitDiff(context.Background(), dir, "HEAD")
	if err == nil {
		t.Fatal("expected error outside a git repo")
	}
	if strings.Contains(err.Error(), "git fetch") {
		t.Errorf("non-repo error should not carry the fetch hint, got: %v", err)
	}
	// Nor the shallow-clone hint: git failed before it ever looked for a
	// merge base, so pointing at fetch-depth would send the reader the
	// wrong way.
	if strings.Contains(err.Error(), "fetch-depth: 0") {
		t.Errorf("non-repo error should not carry the shallow-clone hint, got: %v", err)
	}
}

func TestGitRootNotARepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	// GIT_CEILING_DIRECTORIES stops git's upward .git search at dir, so
	// the test is deterministic even when run from inside a developer's
	// repo. Without it, git would walk up to the enclosing repo and
	// "succeed" with the wrong toplevel.
	t.Setenv("GIT_CEILING_DIRECTORIES", dir)
	_, err := GitRoot(context.Background(), dir)
	if err == nil {
		t.Fatal("expected error when not in a git repo")
	}
	// The error must include git's stderr so users can see why git
	// failed; this also pins down `cmd.Stderr = &stderr` so a mutation
	// that drops the assignment is detected.
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("error should include git stderr (\"not a git repository\"); got: %v", err)
	}
}

func TestLooksLikeBadRevision(t *testing.T) {
	cases := map[string]bool{
		"fatal: unknown revision 'x'":                    true,
		"fatal: bad revision 'x'":                        true,
		"fatal: ambiguous argument: not in working tree": false,
		"": false,
	}
	for in, want := range cases {
		if got := looksLikeBadRevision(in); got != want {
			t.Errorf("looksLikeBadRevision(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestParseUnifiedDiffLargeLine exercises the configured 16 MiB scanner
// buffer: a 200 KiB line exceeds bufio.Scanner's 64 KiB default, so any
// mutation that drops the sc.Buffer call would error out instead of
// parsing the diff.
func TestParseUnifiedDiffLargeLine(t *testing.T) {
	big := strings.Repeat("x", 200*1024)
	in := "+++ b/big.go\n@@ -1 +1 @@\n+" + big + "\n"
	got, err := ParseUnifiedDiff(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if r := got["big.go"]; len(r) != 1 || r[0].Start != 1 {
		t.Errorf("expected big.go ranges [{Start:1,End:1}], got %v", r)
	}
}

// TestParseUnifiedDiffPathWithLeadingTab pins down the `i >= 0` boundary
// on the IndexByte('\t') strip. Synthetic input "+++ \tname" puts the
// tab at index 0; with `>= 0` the path strips to empty (and the hunk is
// dropped); with `> 0` the leading tab survives and the hunk attaches
// to a bogus key.
func TestParseUnifiedDiffPathWithLeadingTab(t *testing.T) {
	in := "+++ \tx.go\n@@ -1 +1 @@\n+x\n"
	got, err := ParseUnifiedDiff(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("path stripped to empty must produce no ranges; got %v", got)
	}
}

// TestParseUnifiedDiffDevNullDoesNotAttachHunks checks the /dev/null
// branch resets `current` so a synthetic following hunk can't be
// attached to ranges["/dev/null"]. Real git emits +0,0 here, but we
// want to exercise the path code, not parseHunkHeader.
func TestParseUnifiedDiffDevNullDoesNotAttachHunks(t *testing.T) {
	in := "+++ /dev/null\n@@ -1 +1 @@\n+x\n"
	got, err := ParseUnifiedDiff(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("/dev/null target must not capture any hunks; got %v", got)
	}
}

// TestParseUnifiedDiffDevNullResetsCurrent ensures the `current = ""`
// assignment runs: without it, a /dev/null entry following a real file
// leaves `current` stale and a later hunk attaches to the wrong file.
func TestParseUnifiedDiffDevNullResetsCurrent(t *testing.T) {
	in := "+++ b/foo.go\n@@ -1 +1 @@\n+a\n" +
		"+++ /dev/null\n" +
		"@@ -2 +2 @@\n+b\n"
	got, err := ParseUnifiedDiff(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(got["foo.go"]) != 1 {
		t.Errorf("foo.go should keep exactly one hunk; got %v", got["foo.go"])
	}
}

// TestParseUnifiedDiffContinuesAfterDevNull pins down the `continue` on
// the /dev/null branch: turning it into a `break` would abort the
// scanner and drop everything after.
func TestParseUnifiedDiffContinuesAfterDevNull(t *testing.T) {
	in := "+++ /dev/null\n" +
		"+++ b/foo.go\n@@ -1 +1 @@\n+x\n"
	got, err := ParseUnifiedDiff(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if got["foo.go"] == nil {
		t.Errorf("foo.go must still parse after /dev/null entry; got %v", got)
	}
}

// TestParseUnifiedDiffHunkBeforeFileSkipped checks the `if current == ""
// {continue}` guard: without it, a stray @@ before any +++ would attach
// a hunk to ranges[""].
func TestParseUnifiedDiffHunkBeforeFileSkipped(t *testing.T) {
	in := "@@ -1 +1 @@\n+x\n"
	got, err := ParseUnifiedDiff(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("@@ before any +++ must not produce ranges; got %v", got)
	}
}

// TestParseUnifiedDiffContinuesPastUnboundHunk pairs with the test
// above: with `continue → break`, a later valid +++ would never be
// reached.
func TestParseUnifiedDiffContinuesPastUnboundHunk(t *testing.T) {
	in := "@@ -1 +1 @@\n" +
		"+++ b/foo.go\n@@ -2 +2 @@\n+x\n"
	got, err := ParseUnifiedDiff(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if got["foo.go"] == nil {
		t.Errorf("foo.go must parse after unbound hunk; got %v", got)
	}
}

// TestParseUnifiedDiffContinuesPastMalformedHunk pins down the
// `continue` after `parseHunkHeader` returns ok=false: a `break` here
// would drop every diff entry after the first malformed hunk.
func TestParseUnifiedDiffContinuesPastMalformedHunk(t *testing.T) {
	// First file's @@ has no '+' marker (parseHunkHeader → false);
	// second file's @@ is well-formed.
	in := "+++ b/foo.go\n" +
		"@@ -1 -1 @@\n" +
		"+++ b/bar.go\n" +
		"@@ -1 +1 @@\n+x\n"
	got, err := ParseUnifiedDiff(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if got["bar.go"] == nil {
		t.Errorf("bar.go must parse after malformed @@; got %v", got)
	}
}

// TestFilterByDiffEmptyRelDoesNotMatchEmptyKey pins down the `if rel ==
// "" {continue}` guard: without it, a Rel-failed mutant would
// erroneously match a (degenerate) "" key in the ranges map.
func TestFilterByDiffEmptyRelDoesNotMatchEmptyKey(t *testing.T) {
	ranges := map[string][]LineRange{
		"": {{Start: 1, End: 1}},
	}
	mutants := []mutator.Mutant{
		// Absolute target + relative basepath → Rel returns ("", err).
		{ID: 1, File: "/abs/a.go", Line: 1},
	}
	got := FilterByDiff(mutants, ranges, "rel-root")
	if len(got) != 0 {
		t.Errorf("Rel-failed mutant must not match a literal \"\" key; got %v", got)
	}
}

// TestFilterByDiffContinuesAfterEmptyRel pairs with the test above:
// with `continue → break`, a later valid mutant in the same loop would
// be silently dropped.
func TestFilterByDiffContinuesAfterEmptyRel(t *testing.T) {
	ranges := map[string][]LineRange{
		"good.go": {{Start: 1, End: 1}},
	}
	mutants := []mutator.Mutant{
		{ID: 1, File: "/abs/bad.go", Line: 1},      // Rel fails → rel=""
		{ID: 2, File: "rel-root/good.go", Line: 1}, // Rel succeeds → "good.go"
	}
	got := FilterByDiff(mutants, ranges, "rel-root")
	if len(got) != 1 || got[0].ID != 2 {
		t.Errorf("loop must continue past Rel failure; got %+v", got)
	}
}

// TestChangedSinceCWDInvariant pins the cross-CWD invariant of the
// --changed-since pipeline: regardless of which directory gomutants is
// invoked from (module root vs a nested subpackage), the absolute
// mutant.File paths produced by Discover must align with the
// repo-root-relative keys in the diff map after FilterByDiff's
// filepath.Rel(gitRoot, m.File) normalisation, so the same set of
// mutants survives the filter.
//
// The invariant holds because both inputs to that Rel are absolute:
// mutant.File via filepath.Join(pkg.Dir, …) where pkg.Dir comes from
// `go list -json` (always absolute), and gitRoot via `git rev-parse
// --show-toplevel` (always absolute). If anyone refactors Discover to
// stash a CWD-relative path on mutant.File — or computes gitRoot
// differently — this test fails for both subcases at once.
func TestChangedSinceCWDInvariant(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not available")
	}

	// Canonicalise the temp dir up front: on macOS t.TempDir() returns a
	// /var/... path while `git rev-parse --show-toplevel` and `go list`
	// resolve through the /private symlink. Without this, gitRoot and
	// mutant.File would sit on different canonical roots and Rel would
	// produce "../..." nonsense — failing the test for the wrong reason.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// No GIT_CEILING_DIRECTORIES: the in-tree `.git` created below stops
	// the upward search before it can reach any enclosing dev repo.
	ctx := context.Background()

	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// Module with a nested subpackage so we can vary the invocation cwd
	// between the module root and the subpackage itself.
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module testmod\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	subDir := filepath.Join(dir, "internal", "somepkg")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	srcPath := filepath.Join(subDir, "f.go")
	if err := os.WriteFile(srcPath,
		[]byte("package somepkg\n\nfunc F() int { return 1 + 2 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("init", "-q", "-b", "main")
	runGit("add", ".")
	runGit("commit", "-q", "-m", "init")

	// Working-tree edit on line 3 — ARITHMETIC_BASE produces a mutant
	// at the `+`, which must survive FilterByDiff against `HEAD`.
	if err := os.WriteFile(srcPath,
		[]byte("package somepkg\n\nfunc F() int { return 3 + 4 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name       string
		projectDir string
		patterns   []string
	}{
		{"from module root", dir, []string{"./internal/somepkg/..."}},
		{"from subpackage cwd", subDir, []string{"./..."}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertChangedSinceKeepsMutants(t, ctx, tc.projectDir, tc.patterns)
		})
	}
}

// assertChangedSinceKeepsMutants exercises the full --changed-since
// pipeline from projectDir and fails the test if FilterByDiff drops every
// discovered mutant. Extracted from TestChangedSinceCWDInvariant to keep
// the subtest body shallow (SonarCloud's go:S3776 cognitive-complexity
// gate fires hard on nested closures with multiple if-err branches).
func assertChangedSinceKeepsMutants(t *testing.T, ctx context.Context, projectDir string, patterns []string) {
	t.Helper()
	pkgs, err := ResolvePackages(ctx, projectDir, patterns, "")
	if err != nil {
		t.Fatalf("ResolvePackages: %v", err)
	}
	gitRoot, err := GitRoot(ctx, projectDir)
	if err != nil {
		t.Fatalf("GitRoot: %v", err)
	}
	result := Discover(token.NewFileSet(), pkgs,
		mutator.NewRegistry().Mutators(), gitRoot, "testmod")
	if len(result.Mutants) == 0 {
		t.Fatalf("Discover produced no mutants; expected at least ARITHMETIC_BASE on `3 + 4`")
	}
	ranges, err := RunGitDiff(ctx, projectDir, "HEAD")
	if err != nil {
		t.Fatalf("RunGitDiff: %v", err)
	}
	// The diff key shape is the contract FilterByDiff relies on; pin it
	// here so a change to ParseUnifiedDiff that subtly alters key form
	// gets caught with a precise failure rather than as an opaque
	// "0 mutants kept".
	const wantKey = "internal/somepkg/f.go"
	if _, ok := ranges[wantKey]; !ok {
		t.Fatalf("diff map missing key %q; got keys %v", wantKey, diffKeys(ranges))
	}
	kept := FilterByDiff(result.Mutants, ranges, gitRoot)
	if len(kept) == 0 {
		t.Fatalf("FilterByDiff dropped every mutant — likely a CWD/path-shape mismatch.\n"+
			"gitRoot: %s\nmutant.File sample: %s\ndiff keys: %v",
			gitRoot, sampleMutantFiles(result.Mutants), diffKeys(ranges))
	}
}

func diffKeys(m map[string][]LineRange) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func sampleMutantFiles(mutants []mutator.Mutant) string {
	seen := make(map[string]struct{})
	var out []string
	for _, m := range mutants {
		if _, ok := seen[m.File]; ok {
			continue
		}
		seen[m.File] = struct{}{}
		out = append(out, m.File)
		if len(out) >= 3 {
			break
		}
	}
	return strings.Join(out, ", ")
}

// TestFilterByDiffBreaksAfterFirstHunkMatch pins down the inner-loop
// `break`: turning it into `continue` would re-append the same mutant
// once per overlapping hunk that contains its line.
func TestFilterByDiffBreaksAfterFirstHunkMatch(t *testing.T) {
	ranges := map[string][]LineRange{
		"a.go": {{Start: 1, End: 5}, {Start: 3, End: 7}}, // overlap on lines 3-5
	}
	mutants := []mutator.Mutant{
		{ID: 1, File: "/repo/a.go", Line: 4}, // in both hunks
	}
	got := FilterByDiff(mutants, ranges, "/repo")
	if len(got) != 1 {
		t.Errorf("mutant in overlapping hunks must not be duplicated; got %d entries: %v", len(got), got)
	}
}
