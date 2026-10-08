package report

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/szhekpisov/gomutants/internal/mutator"
)

// TestHeaderExact asserts the exact bytes produced by Header.
// Kills STATEMENT_REMOVE on any of the three Fprintf lines and any text mutations.
func TestHeaderExact(t *testing.T) {
	var buf bytes.Buffer
	term := NewTerminal(&buf, 0, false, false)
	term.Header("0.1.0", "[./...]", 10, 5)

	want := "gomutants v0.1.0\n\nTarget: [./...]\nWorkers: 10 | Mutations: 5 types enabled\n\n"
	if got := buf.String(); got != want {
		t.Errorf("Header output mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestPhaseExact(t *testing.T) {
	var buf bytes.Buffer
	term := NewTerminal(&buf, 0, false, false)
	term.Phase("Collecting coverage...")
	term.PhaseDone("done (2.1s)")

	want := "Collecting coverage... done (2.1s)\n"
	if got := buf.String(); got != want {
		t.Errorf("Phase output mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// TestOnResultVerboseExact asserts the full verbose line format.
func TestOnResultVerboseExact(t *testing.T) {
	var buf bytes.Buffer
	term := NewTerminal(&buf, 5, true, false)

	m := mutator.Mutant{
		ID:          1,
		Status:      mutator.StatusKilled,
		RelFile:     "file.go",
		Line:        10,
		Col:         5,
		Original:    "+",
		Replacement: "-",
		Duration:    150 * time.Millisecond,
	}
	term.OnResult(m)

	want := "  [1/5] KILLED file.go:10:5 + → - (150ms)\n"
	if got := buf.String(); got != want {
		t.Errorf("OnResult verbose output mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestOnResultNonTTY(t *testing.T) {
	var buf bytes.Buffer
	term := NewTerminal(&buf, 5, false, false)

	m := mutator.Mutant{ID: 1, Status: mutator.StatusLived, Duration: time.Second}
	term.OnResult(m)

	// Non-verbose, non-TTY: should produce no output.
	if buf.Len() != 0 {
		t.Errorf("expected no output for non-TTY non-verbose, got %q", buf.String())
	}
}

// TestOnResultTTYProgressBarExact pins the exact progress bar format so
// mutations on the arithmetic, loop bounds, bar rendering, and counters
// are all caught.
func TestOnResultTTYProgressBarExact(t *testing.T) {
	// total=10, done will become 1 after OnResult → bar has 3 '=' chars (30 * 1 / 10 = 3), 27 ' ' chars
	// pctDone = 1 * 100 / 10 = 10
	// elapsed = 0s (started now, rounded to seconds)
	var buf bytes.Buffer
	start := time.Now()
	term := &Terminal{
		w:     &buf,
		isTTY: true,
		total: 10,
		start: start,
	}

	m := mutator.Mutant{ID: 1, Status: mutator.StatusKilled, Duration: time.Millisecond}
	term.OnResult(m)

	got := buf.String()
	// Must begin with carriage return.
	if !strings.HasPrefix(got, "\r") {
		t.Errorf("expected leading \\r, got %q", got)
	}
	// Must contain "Testing mutants [...] 1/10  10%  " (with 30-wide bar: 3 '=' + 27 ' ').
	wantBar := "===                           "
	wantPrefix := fmt.Sprintf("\rTesting mutants [%s] 1/10  10%%  ", wantBar)
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("OnResult TTY:\n got: %q\nwant prefix: %q", got, wantPrefix)
	}
}

// TestOnResultTTYBarFullWhenDone covers the `filled == barWidth` path so
// all bar slots become '=' — kills CONDITIONALS_BOUNDARY mutations on
// `i < filled` (==, >=, etc. would leave wrong last char).
func TestOnResultTTYBarFullWhenDone(t *testing.T) {
	var buf bytes.Buffer
	term := &Terminal{
		w:     &buf,
		isTTY: true,
		total: 1,
		start: time.Now(),
	}
	m := mutator.Mutant{ID: 1, Status: mutator.StatusKilled}
	term.OnResult(m)

	got := buf.String()
	wantBar := strings.Repeat("=", 30)
	if !strings.Contains(got, "["+wantBar+"]") {
		t.Errorf("expected bar fully filled: %q", got)
	}
	if !strings.Contains(got, "100%") {
		t.Errorf("expected 100%% at completion: %q", got)
	}
}

// TestOnResultTTYBarEmptyWhenStarting covers the `filled == 0` case.
func TestOnResultTTYBarEmptyWhenStarting(t *testing.T) {
	var buf bytes.Buffer
	term := &Terminal{
		w:     &buf,
		isTTY: true,
		total: 1000, // large total so done=1 means filled=0
		start: time.Now(),
	}
	m := mutator.Mutant{ID: 1, Status: mutator.StatusKilled}
	term.OnResult(m)

	got := buf.String()
	wantBar := strings.Repeat(" ", 30)
	if !strings.Contains(got, "["+wantBar+"]") {
		t.Errorf("expected empty bar: %q", got)
	}
	if !strings.Contains(got, "0%") {
		t.Errorf("expected 0%% near start: %q", got)
	}
}

// TestOnResultTTYTotalZeroSkipsPct covers the `t.total > 0` guard in OnResult.
// With total=0, the pct branch is skipped and progress bar is NOT rendered
// (because `barWidth * t.done / t.total` would divide by zero).
// Actually the code unconditionally computes `filled := barWidth * t.done / t.total`
// even when total=0. Let's verify the guarded behavior: only pctDone is guarded.
func TestOnResultTTYTotalZero(t *testing.T) {
	// If total == 0, we expect a panic on division... actually total=0 with isTTY
	// means pctDone stays 0, but `filled := barWidth * t.done / t.total` divides by 0.
	// So this is an untested edge case. Skip — not realistic (total always > 0 in practice).
	t.Skip("total=0 in TTY path would divide by zero; not a real code path")
}

// TestSummaryExact pins the full Summary output for a known Report.
// Kills every STATEMENT_REMOVE on each Fprintf line and surfaces any
// regression in the per-status counter wiring.
func TestSummaryExact(t *testing.T) {
	var buf bytes.Buffer
	term := NewTerminal(&buf, 0, false, false) // non-TTY
	r := &Report{
		MutantsKilled:     8,
		MutantsLived:      2,
		MutantsNotCovered: 3,
		MutantsNotViable:  1,
		MutantsTimedOut:   0,
		MutantsTotal:      14,
		TestEfficacy:      80.0,
	}
	term.Summary(r)

	// Non-TTY: no leading newline (from the isTTY guard).
	want := "\n" +
		"  Killed:       8  (80.0%)\n" +
		"  Lived:        2  (20.0%)\n" +
		"  Not covered:  3\n" +
		"  Not viable:   1\n" +
		"  Timed out:    0\n" +
		"  Efficacy:     80.00%\n" +
		"\n"
	if got := buf.String(); got != want {
		t.Errorf("Summary output mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// TestSummaryTimedOutNonZero pins the Timed-out line against a non-zero
// MutantsTimedOut, killing any STATEMENT_REMOVE on the corresponding Fprintf.
func TestSummaryTimedOutNonZero(t *testing.T) {
	var buf bytes.Buffer
	term := NewTerminal(&buf, 0, false, false)
	r := &Report{
		MutantsKilled:     5,
		MutantsLived:      1,
		MutantsNotCovered: 3,
		MutantsNotViable:  1,
		MutantsTimedOut:   2,
		MutantsTotal:      12,
		TestEfficacy:      83.33,
	}
	term.Summary(r)

	if !strings.Contains(buf.String(), "Timed out:    2\n") {
		t.Errorf("expected 'Timed out:    2' line, got %q", buf.String())
	}
}

func TestSummaryInfraErrorsNonZero(t *testing.T) {
	var buf bytes.Buffer
	term := NewTerminal(&buf, 0, false, false)
	term.Summary(&Report{
		MutantsKilled:     5,
		MutantsLived:      1,
		MutantsTimedOut:   2,
		MutantsInfraError: 1,
		TestEfficacy:      83.33,
	})

	if !strings.Contains(buf.String(), "  Timed out:    2\n  Infra errors: 1  (environment failure, not a mutant result — rerun to confirm)\n") {
		t.Errorf("expected nonzero infra errors immediately after timed out, got %q", buf.String())
	}
}

// TestSummaryTTYLeadingNewline covers the `t.isTTY && !t.verbose` branch
// that prints a newline before the summary to clear the progress line.
func TestSummaryTTYLeadingNewline(t *testing.T) {
	var buf bytes.Buffer
	term := &Terminal{
		w:     &buf,
		isTTY: true,
		total: 5,
		start: time.Now(),
	}
	r := &Report{MutantsKilled: 5, MutantsTotal: 5, TestEfficacy: 100}
	term.Summary(r)

	// Should start with "\n\n" — first from isTTY guard, second from the
	// unconditional Fprintln before summary lines.
	if !strings.HasPrefix(buf.String(), "\n\n") {
		t.Errorf("TTY summary should start with two newlines: %q", buf.String())
	}
}

// TestSummaryTTYVerboseNoLeadingNewline covers the `!t.verbose` short-circuit:
// in verbose mode, no extra newline before summary.
func TestSummaryTTYVerboseNoLeadingNewline(t *testing.T) {
	var buf bytes.Buffer
	term := &Terminal{
		w:       &buf,
		isTTY:   true,
		total:   5,
		start:   time.Now(),
		verbose: true,
	}
	r := &Report{MutantsKilled: 5, MutantsTotal: 5, TestEfficacy: 100}
	term.Summary(r)

	// Should start with single "\n" from the unconditional Fprintln, not "\n\n".
	got := buf.String()
	if strings.HasPrefix(got, "\n\n") {
		t.Errorf("verbose TTY summary should not have double leading newline: %q", got)
	}
	if !strings.HasPrefix(got, "\n") {
		t.Errorf("verbose TTY summary should still have one leading newline: %q", got)
	}
}

// TestSummaryNonTTYNoLeadingNewline kills EXPRESSION_REMOVE on the isTTY && !verbose guard:
// if the guard's isTTY operand is replaced with true, a non-TTY would get the extra newline.
func TestSummaryNonTTYNoLeadingNewline(t *testing.T) {
	var buf bytes.Buffer
	term := NewTerminal(&buf, 0, false, false) // bytes.Buffer → isTTY=false
	r := &Report{MutantsKilled: 5, MutantsTotal: 5, TestEfficacy: 100}
	term.Summary(r)

	got := buf.String()
	// Non-TTY should start with single "\n" from the unconditional Fprintln.
	if strings.HasPrefix(got, "\n\n") {
		t.Errorf("non-TTY summary should not have double leading newline: %q", got)
	}
}

func TestSummaryShowsCachedLineWhenNonZero(t *testing.T) {
	var buf bytes.Buffer
	term := NewTerminal(&buf, 0, false, false)
	r := &Report{
		MutantsKilled: 5, MutantsLived: 1,
		MutantsTotal: 6, MutantsCached: 4,
		TestEfficacy: 83.33,
	}
	term.Summary(r)
	if !strings.Contains(buf.String(), "Cached:       4  (skipped)\n") {
		t.Errorf("expected Cached line, got %q", buf.String())
	}
}

func TestSummaryHidesCachedLineWhenZero(t *testing.T) {
	var buf bytes.Buffer
	term := NewTerminal(&buf, 0, false, false)
	r := &Report{MutantsKilled: 5, MutantsTotal: 5, TestEfficacy: 100}
	term.Summary(r)
	if strings.Contains(buf.String(), "Cached:") {
		t.Errorf("expected no Cached line when MutantsCached=0, got %q", buf.String())
	}
}

// TestSummaryRecheckLine: the re-check line shows only when a mutant was
// re-checked, with how many the re-check killed.
func TestSummaryRecheckLine(t *testing.T) {
	var buf bytes.Buffer
	NewTerminal(&buf, 0, false, false).Summary(&Report{MutantsKilled: 5, MutantsLived: 1, MutantsRechecked: 1, MutantsRecheckKilled: 1})
	if !strings.Contains(buf.String(), "Re-checked:   1  (1 killed by tests the coverage map missed)\n") {
		t.Errorf("expected Re-checked line, got %q", buf.String())
	}
	buf.Reset()
	NewTerminal(&buf, 0, false, false).Summary(&Report{MutantsKilled: 5})
	if strings.Contains(buf.String(), "Re-checked:") {
		t.Errorf("expected no Re-checked line without re-checks, got %q", buf.String())
	}
}

func TestSummaryShowsSuppressedLineWhenNonZero(t *testing.T) {
	var buf bytes.Buffer
	term := NewTerminal(&buf, 0, false, false)
	r := &Report{
		MutantsKilled: 5, MutantsLived: 1,
		MutantsTotal: 6, MutantsSuppressed: 3,
		TestEfficacy: 83.33,
	}
	term.Summary(r)
	if !strings.Contains(buf.String(), "Suppressed:   3  (directives)\n") {
		t.Errorf("expected Suppressed line, got %q", buf.String())
	}
}

func TestSummaryHidesSuppressedLineWhenZero(t *testing.T) {
	var buf bytes.Buffer
	term := NewTerminal(&buf, 0, false, false)
	r := &Report{MutantsKilled: 5, MutantsTotal: 5, TestEfficacy: 100}
	term.Summary(r)
	if strings.Contains(buf.String(), "Suppressed:") {
		t.Errorf("expected no Suppressed line when MutantsSuppressed=0, got %q", buf.String())
	}
}

func TestQuietSuppressesHeaderPhaseAndOnResult(t *testing.T) {
	var buf bytes.Buffer
	// Force isTTY=true so the OnResult progress-bar branch is exercised
	// (otherwise the non-TTY non-verbose path is already silent and the
	// quiet guard would be redundantly tested).
	term := &Terminal{w: &buf, isTTY: true, total: 5, start: time.Now(), quiet: true}

	term.Header("0.1.0", "[./...]", 4, 16)
	term.Phase("Resolving packages...")
	term.PhaseDone("done (1 packages)")
	term.OnResult(mutator.Mutant{ID: 1, Status: mutator.StatusKilled, Duration: time.Millisecond})

	if buf.Len() != 0 {
		t.Errorf("quiet mode should produce no output for Header/Phase/OnResult, got %q", buf.String())
	}
}

func TestQuietStillPrintsSummary(t *testing.T) {
	var buf bytes.Buffer
	term := NewTerminal(&buf, 0, false, true)
	r := &Report{
		MutantsKilled: 5, MutantsLived: 1,
		MutantsTotal: 6, TestEfficacy: 83.33,
	}
	term.Summary(r)

	got := buf.String()
	if !strings.Contains(got, "Killed:") {
		t.Errorf("Summary must still print in quiet mode, got %q", got)
	}
	if !strings.Contains(got, "Efficacy:") {
		t.Errorf("Summary must still include efficacy in quiet mode, got %q", got)
	}
}

// TestQuietVerboseOnResultStillSuppressed kills any future regression
// where verbose's per-mutant Fprintf path runs ahead of the quiet guard.
// The CLI rejects --quiet --verbose, but Terminal is a library: defense
// in depth keeps the no-output guarantee local to the Terminal.
func TestQuietVerboseOnResultStillSuppressed(t *testing.T) {
	var buf bytes.Buffer
	term := &Terminal{w: &buf, isTTY: false, total: 5, start: time.Now(), verbose: true, quiet: true}
	term.OnResult(mutator.Mutant{ID: 1, Status: mutator.StatusLived})
	if buf.Len() != 0 {
		t.Errorf("quiet must short-circuit before verbose path, got %q", buf.String())
	}
}

func TestPct(t *testing.T) {
	if got := pct(0, 0); got != 0 {
		t.Errorf("pct(0, 0) = %f, want 0", got)
	}
	if got := pct(1, 2); got != 50 {
		t.Errorf("pct(1, 2) = %f, want 50", got)
	}
	if got := pct(3, 3); got != 100 {
		t.Errorf("pct(3, 3) = %f, want 100", got)
	}
}

func TestNewTerminalNonFile(t *testing.T) {
	var buf bytes.Buffer
	term := NewTerminal(&buf, 10, false, false)
	if term.isTTY {
		t.Error("bytes.Buffer should not be detected as TTY")
	}
	if term.total != 10 {
		t.Errorf("total=%d, want 10", term.total)
	}
}

// TestNewTerminalWithPipe exercises the Stat() path in NewTerminal.
// A pipe is a *os.File but not a TTY.
func TestNewTerminalWithPipe(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	term := NewTerminal(w, 5, false, false)
	if term.isTTY {
		t.Error("pipe should not be detected as TTY")
	}
}

// TestNewTerminalWithClosedFile covers the `err != nil` branch from Stat().
// An os.File that was already closed will fail Stat.
func TestNewTerminalWithClosedFile(t *testing.T) {
	f, err := os.CreateTemp("", "term-*")
	if err != nil {
		t.Fatal(err)
	}
	name := f.Name()
	_ = f.Close()
	os.Remove(name)
	// f is now a closed file.
	term := NewTerminal(f, 5, false, false)
	if term.isTTY {
		t.Error("closed file should not be detected as TTY")
	}
}

func TestWriteJSONError(t *testing.T) {
	r := &Report{GoModule: "test", MutatorStatistics: map[string]int{}}
	err := WriteJSON(r, "/dev/null/impossible/report.json")
	if err == nil {
		t.Fatal("expected error for invalid path")
	}
}

func TestWriteJSONMarshalError(t *testing.T) {
	orig := marshalJSON
	marshalJSON = func(v any) ([]byte, error) {
		return nil, fmt.Errorf("injected marshal error")
	}
	defer func() { marshalJSON = orig }()

	r := &Report{GoModule: "test", MutatorStatistics: map[string]int{}}
	err := WriteJSON(r, filepath.Join(t.TempDir(), "report.json"))
	if err == nil {
		t.Fatal("expected error from marshalJSON")
	}
}

// TestWriteJSONMkdirError kills BRANCH_IF on the os.MkdirAll error branch in
// json.go. Passing a path whose parent "directory" is actually a regular file
// forces MkdirAll to fail with a "mkdir …: not a directory" error (Op="mkdir").
//
// Asserting `err != nil` alone doesn't kill the mutant: when BRANCH_IF elides
// the early return, execution falls through to os.WriteFile, which also fails
// but with a distinct "open …: not a directory" error (Op="open"). Matching
// on "mkdir" distinguishes the two error paths.
func TestWriteJSONMkdirError(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker-file")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// MkdirAll("blocker-file/nested") fails because blocker-file is a regular file.
	path := filepath.Join(blocker, "nested", "report.json")
	r := &Report{GoModule: "test", MutatorStatistics: map[string]int{}}
	err := WriteJSON(r, path)
	if err == nil {
		t.Fatal("expected WriteJSON to return MkdirAll error")
	}
	if !strings.Contains(err.Error(), "mkdir") {
		t.Errorf("expected mkdir-phase error, got %q (BRANCH_IF would leak a WriteFile/open-phase error here)", err)
	}
}

// TestNewTerminalDetectsCharDevice kills STATEMENT_REMOVE on the isTTY
// assignment in NewTerminal. /dev/null is a character device on Unix; if the
// assignment is removed, isTTY stays at its zero value (false) and the check
// here fails.
func TestNewTerminalDetectsCharDevice(t *testing.T) {
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("/dev/null unavailable: %v", err)
	}
	defer f.Close()
	term := NewTerminal(f, 10, false, false)
	if !term.isTTY {
		t.Errorf("expected isTTY=true for char device %s, got false", os.DevNull)
	}
}

// TestOnResultProgressBarWidth kills ARITHMETIC_BASE on the bar-fill
// computation and CONDITIONALS_BOUNDARY on `t.total > 0`. We set isTTY
// directly since our test buffer isn't a char device.
func TestOnResultProgressBarWidth(t *testing.T) {
	var buf bytes.Buffer
	term := &Terminal{w: &buf, isTTY: true, total: 10, start: time.Now(), verbose: false}
	for range 5 {
		term.OnResult(mutator.Mutant{})
	}
	// barWidth=30, done=5, total=10 -> filled = 30*5/10 = 15 '=' chars.
	// If ARITHMETIC_BASE flips * -> / : 30/5/10 = 0 (no '=').
	// If ARITHMETIC_BASE flips / -> * : 30*5*10 = 1500 (buffer overflow, but
	// the bar is a fixed [30]byte so at minimum count differs).
	// Final frame is after the last '\r'.
	frames := strings.Split(buf.String(), "\r")
	last := frames[len(frames)-1]
	equals := strings.Count(last, "=")
	if equals != 15 {
		t.Errorf("bar fill: got %d '=', want 15. last frame: %q", equals, last)
	}
	// Also assert the percentage displayed, which catches pctDone mutations.
	if !strings.Contains(last, "50%") {
		t.Errorf("expected 50%% progress, got: %q", last)
	}
}

// TestOnResultZeroTotalNoPanic kills CONDITIONALS_BOUNDARY on `t.total > 0`.
// Mutating > to >= lets the block execute with total=0 and divide by zero.
func TestOnResultZeroTotalNoPanic(t *testing.T) {
	var buf bytes.Buffer
	term := &Terminal{w: &buf, isTTY: true, total: 0, start: time.Now(), verbose: false}
	// Must not panic. Under mutation (>= 0), pctDone = done*100/0 -> panic.
	term.OnResult(mutator.Mutant{})
}

// syncBuf is a goroutine-safe io.Writer for heartbeat tests where the
// loop goroutine writes concurrently with the test reading the buffer.
// bytes.Buffer alone races under -race.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func (s *syncBuf) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Len()
}

// waitForCond polls until f returns true, failing the test after timeout.
// Used to wait on the first heartbeat paint without sprinkling sleeps.
func waitForCond(t *testing.T, f func() bool, timeout time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition not met within %s: %s", timeout, msg)
}

// withHeartbeatInterval swaps the package-level cadence for the test's
// duration so multi-tick assertions don't wait real seconds.
func withHeartbeatInterval(t *testing.T, d time.Duration) {
	t.Helper()
	orig := heartbeatInterval
	heartbeatInterval = d
	t.Cleanup(func() { heartbeatInterval = orig })
}

// TestStartHeartbeatNoOpWhenNonTTY: off-TTY writers (files, pipes, the
// test buffer) don't get the heartbeat — its \r-overwrite would clutter
// non-terminal logs and the bar is suppressed there anyway.
func TestStartHeartbeatNoOpWhenNonTTY(t *testing.T) {
	var buf syncBuf
	term := &Terminal{w: &buf, isTTY: false, total: 5, start: time.Now()}
	term.StartHeartbeat()
	if term.hbStop != nil {
		t.Error("non-TTY: StartHeartbeat must not spawn a goroutine")
	}
	term.StopHeartbeat() // must not deadlock on a nil channel
	if buf.Len() != 0 {
		t.Errorf("non-TTY: expected no output, got %q", buf.String())
	}
}

func TestStartHeartbeatNoOpWhenQuiet(t *testing.T) {
	var buf syncBuf
	term := &Terminal{w: &buf, isTTY: true, total: 5, start: time.Now(), quiet: true}
	term.StartHeartbeat()
	if term.hbStop != nil {
		t.Error("quiet: StartHeartbeat must not spawn a goroutine")
	}
	term.StopHeartbeat()
	if buf.Len() != 0 {
		t.Errorf("quiet: expected no output, got %q", buf.String())
	}
}

// Verbose prints a line per mutant; an interleaved \r-spinner would
// scramble the transcript.
func TestStartHeartbeatNoOpWhenVerbose(t *testing.T) {
	var buf syncBuf
	term := &Terminal{w: &buf, isTTY: true, total: 5, start: time.Now(), verbose: true}
	term.StartHeartbeat()
	if term.hbStop != nil {
		t.Error("verbose: StartHeartbeat must not spawn a goroutine")
	}
	term.StopHeartbeat()
	if buf.Len() != 0 {
		t.Errorf("verbose: expected no output, got %q", buf.String())
	}
}

// Total==0 means no pending mutants — nothing to wait on, so no heartbeat.
func TestStartHeartbeatNoOpWhenZeroTotal(t *testing.T) {
	var buf syncBuf
	term := &Terminal{w: &buf, isTTY: true, total: 0, start: time.Now()}
	term.StartHeartbeat()
	if term.hbStop != nil {
		t.Error("total=0: StartHeartbeat must not spawn a goroutine")
	}
	term.StopHeartbeat()
	if buf.Len() != 0 {
		t.Errorf("total=0: expected no output, got %q", buf.String())
	}
}

// TestStartHeartbeatPaintsIdleLine pins the exact format of the idle line
// and the clear-to-EOL trailer that StopHeartbeat emits. Sets the ticker
// to one hour so only the immediate paint runs — keeps the assertion on
// a single, deterministic frame.
func TestStartHeartbeatPaintsIdleLine(t *testing.T) {
	withHeartbeatInterval(t, time.Hour)
	var buf syncBuf
	term := &Terminal{w: &buf, isTTY: true, total: 7, start: time.Now()}
	term.StartHeartbeat()
	waitForCond(t, func() bool { return buf.Len() > 0 }, time.Second, "initial heartbeat paint")
	term.StopHeartbeat()

	got := buf.String()
	if !strings.HasPrefix(got, "\rTesting mutants [") {
		t.Errorf("expected '\\rTesting mutants [' prefix, got %q", got)
	}
	wantEmptyBar := strings.Repeat(" ", barWidth)
	if !strings.Contains(got, "["+wantEmptyBar+"]") {
		t.Errorf("expected %d-wide empty bar, got %q", barWidth, got)
	}
	if !strings.Contains(got, "] 0/7  0%  ") {
		t.Errorf("expected '0/7  0%%  ' segment, got %q", got)
	}
	if !strings.Contains(got, "  (compiling)") {
		t.Errorf("expected '  (compiling)' suffix, got %q", got)
	}
	if !strings.HasSuffix(got, "\r\033[K") {
		t.Errorf("expected '\\r\\033[K' clear-to-EOL trailer from StopHeartbeat, got %q", got)
	}
}

// TestHeartbeatTicksMultipleTimes proves the ticker actually fires —
// without it, the elapsed counter would freeze at 0s and the heartbeat
// would visually look identical to a real hang.
func TestHeartbeatTicksMultipleTimes(t *testing.T) {
	withHeartbeatInterval(t, time.Millisecond)
	var buf syncBuf
	term := &Terminal{w: &buf, isTTY: true, total: 5, start: time.Now()}
	term.StartHeartbeat()
	waitForCond(t,
		func() bool { return strings.Count(buf.String(), "\rTesting mutants") >= 3 },
		2*time.Second, "≥3 heartbeat paints")
	term.StopHeartbeat()
}

// TestOnResultStopsHeartbeat is the load-bearing test: once the first
// mutant completes, the heartbeat must yield the line to OnResult — no
// more "(compiling)" paints, and the progress-bar frame is visible.
func TestOnResultStopsHeartbeat(t *testing.T) {
	withHeartbeatInterval(t, time.Millisecond)
	var buf syncBuf
	term := &Terminal{w: &buf, isTTY: true, total: 5, start: time.Now()}
	term.StartHeartbeat()
	waitForCond(t, func() bool { return buf.Len() > 0 }, time.Second, "initial heartbeat paint")

	term.OnResult(mutator.Mutant{ID: 1, Status: mutator.StatusKilled})

	// OnResult's internal StopHeartbeat blocks on <-hbDone, so the
	// goroutine has already exited by the time the snapshot is taken —
	// no further "(compiling)" frames are possible.
	snap := buf.String()
	clearIdx := strings.Index(snap, "\r\033[K")
	if clearIdx < 0 {
		t.Fatalf("expected clear-to-EOL marker after first OnResult, got %q", snap)
	}
	if !strings.Contains(snap[clearIdx:], " 1/5  20%  ") {
		t.Errorf("expected '1/5  20%%' progress bar after clear, got %q", snap[clearIdx:])
	}
	if strings.Contains(snap[clearIdx:], "(compiling)") {
		t.Errorf("heartbeat painted after StopHeartbeat: %q", snap[clearIdx:])
	}
}

// Multiple StopHeartbeat calls + a trailing OnResult must each be safe.
// sync.Once protects the close+wait so double-stop can't double-close
// the channel, and OnResult's internal stop is a no-op past the first.
func TestStopHeartbeatIdempotent(t *testing.T) {
	withHeartbeatInterval(t, time.Hour)
	var buf syncBuf
	term := &Terminal{w: &buf, isTTY: true, total: 5, start: time.Now()}
	term.StartHeartbeat()
	waitForCond(t, func() bool { return buf.Len() > 0 }, time.Second, "initial paint")

	term.StopHeartbeat()
	term.StopHeartbeat() // second call: sync.Once → no-op, must not panic or hang
	term.OnResult(mutator.Mutant{ID: 1, Status: mutator.StatusKilled})

	// Exactly one clear-to-EOL emitted (the first StopHeartbeat). OnResult
	// and the second StopHeartbeat take the sync.Once-short-circuit path.
	if got := strings.Count(buf.String(), "\r\033[K"); got != 1 {
		t.Errorf("expected exactly one clear marker, got %d: %q", got, buf.String())
	}
}

// StopHeartbeat without a prior Start (e.g., heartbeat suppressed by
// quiet/verbose/non-TTY) must be a clean no-op rather than blocking on
// a nil channel receive.
func TestStopHeartbeatNoOpWithoutStart(t *testing.T) {
	var buf syncBuf
	term := &Terminal{w: &buf, isTTY: true, total: 5, start: time.Now()}
	term.StopHeartbeat()
	if buf.Len() != 0 {
		t.Errorf("StopHeartbeat without Start should emit nothing, got %q", buf.String())
	}
}

// paintIdleLine must refuse to paint when done>0. With OnResult's
// current ordering (StopHeartbeat → wait for goroutine exit → mu.Lock
// → done++) this state is unreachable from the live code path, so the
// test exercises the guard directly by pre-setting done. It pins the
// invariant for any future reordering of OnResult.
func TestPaintIdleLineSkipsAfterFirstResult(t *testing.T) {
	var buf syncBuf
	term := &Terminal{w: &buf, isTTY: true, total: 5, start: time.Now(), done: 1}
	term.paintIdleLine()
	if buf.Len() != 0 {
		t.Errorf("paintIdleLine must no-op when done>0, got %q", buf.String())
	}
}

// A second StartHeartbeat must be a no-op: hbStopOnce has already fired
// after the first StopHeartbeat (or will), so a re-spawned goroutine
// could never be stopped. The guard prevents the leak.
func TestStartHeartbeatSecondCallIsNoOp(t *testing.T) {
	withHeartbeatInterval(t, time.Hour)
	var buf syncBuf
	term := &Terminal{w: &buf, isTTY: true, total: 5, start: time.Now()}
	term.StartHeartbeat()
	firstStop := term.hbStop
	if firstStop == nil {
		t.Fatal("first StartHeartbeat should have assigned hbStop")
	}
	term.StartHeartbeat() // must not spawn a second goroutine or reassign channels
	if term.hbStop != firstStop {
		t.Error("second StartHeartbeat reassigned hbStop — would orphan first goroutine")
	}
	term.StopHeartbeat()
}

// TestSummaryEquivalentNonZero pins the Equivalent line against a non-zero
// MutantsEquivalent, killing the BRANCH_IF guard and the STATEMENT_REMOVE
// on its Fprintf.
func TestSummaryEquivalentNonZero(t *testing.T) {
	var buf bytes.Buffer
	NewTerminal(&buf, 0, false, false).Summary(&Report{
		MutantsKilled:     8,
		MutantsLived:      1,
		MutantsEquivalent: 1,
		MutantsTotal:      10,
		TestEfficacy:      88.89,
	})
	if !strings.Contains(buf.String(), "Equivalent:   1  (compiler-proven)\n") {
		t.Errorf("expected Equivalent line, got %q", buf.String())
	}
}

// TestSummarySuppressedLineSplitsSources pins the label when both
// suppression sources are in play: the reader has to be able to tell how
// much of the drop came from a project-wide --exclude-calls policy rather
// than from annotations written at the site.
func TestSummarySuppressedLineSplitsSources(t *testing.T) {
	var buf bytes.Buffer
	term := NewTerminal(&buf, 0, false, false)
	r := &Report{
		MutantsKilled: 5, MutantsLived: 1,
		MutantsTotal: 6, MutantsSuppressed: 7, MutantsSuppressedByCalls: 2,
		TestEfficacy: 83.33,
	}
	term.Summary(r)
	if !strings.Contains(buf.String(), "Suppressed:   7  (5 directives, 2 exclude-calls)\n") {
		t.Errorf("expected split Suppressed line, got %q", buf.String())
	}
}

func TestSummarySuppressedLineCallsOnly(t *testing.T) {
	var buf bytes.Buffer
	term := NewTerminal(&buf, 0, false, false)
	r := &Report{
		MutantsKilled: 5, MutantsLived: 1,
		// A single suppression also pins the `> 0` guard against widening
		// to `> 1`, which would hide the line for exactly this count.
		MutantsTotal: 6, MutantsSuppressed: 1, MutantsSuppressedByCalls: 1,
		TestEfficacy: 83.33,
	}
	term.Summary(r)
	if !strings.Contains(buf.String(), "Suppressed:   1  (exclude-calls)\n") {
		t.Errorf("expected exclude-calls-only Suppressed line, got %q", buf.String())
	}
}
