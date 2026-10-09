package report

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gomutants/gomutants/internal/mutator"
)

// barWidth is the width of the progress bar in cells. Shared between
// the live OnResult bar and the idle heartbeat placeholder so the two
// align column-for-column.
const barWidth = 30

// heartbeatInterval is the cadence at which the idle "(compiling)" line
// re-paints so the elapsed counter advances. var (not const) so tests can
// shrink it without forcing a real-time wait. Tests that mutate this var
// must NOT call t.Parallel() — the package-level write would race.
var heartbeatInterval = time.Second

// Terminal handles progress output to the terminal.
type Terminal struct {
	w       io.Writer
	isTTY   bool
	mu      sync.Mutex
	total   int
	done    int
	start   time.Time
	verbose bool
	quiet   bool

	// hbStop signals the heartbeat goroutine (if any) to exit; hbDone
	// closes once it has. Both are nil unless StartHeartbeat actually
	// started a goroutine (gated on TTY + non-quiet + non-verbose).
	// hbStopOnce protects close(hbStop) so OnResult and the deferred
	// StopHeartbeat can both call it safely.
	hbStop     chan struct{}
	hbDone     chan struct{}
	hbStopOnce sync.Once
}

// NewTerminal creates a terminal progress reporter.
func NewTerminal(w io.Writer, total int, verbose, quiet bool) *Terminal {
	isTTY := false
	if f, ok := w.(*os.File); ok {
		if info, err := f.Stat(); err == nil {
			isTTY = info.Mode()&os.ModeCharDevice != 0
		}
	}
	return &Terminal{
		w:       w,
		isTTY:   isTTY,
		total:   total,
		start:   time.Now(),
		verbose: verbose,
		quiet:   quiet,
	}
}

// Header prints the initial info banner.
func (t *Terminal) Header(version string, target string, workers int, mutatorCount int) {
	if t.quiet {
		return
	}
	fmt.Fprintf(t.w, "gomutants v%s\n\n", version)
	fmt.Fprintf(t.w, "Target: %s\n", target)
	fmt.Fprintf(t.w, "Workers: %d | Mutations: %d types enabled\n\n", workers, mutatorCount)
}

// Phase prints a phase status line.
func (t *Terminal) Phase(msg string) {
	if t.quiet {
		return
	}
	fmt.Fprintf(t.w, "%s", msg)
}

// PhaseDone completes a phase line.
func (t *Terminal) PhaseDone(msg string) {
	if t.quiet {
		return
	}
	fmt.Fprintf(t.w, " %s\n", msg)
}

// Info prints one informational line, such as a report path. Like the
// phase lines it is silenced by quiet; the summary is not.
func (t *Terminal) Info(format string, args ...any) {
	if t.quiet {
		return
	}
	fmt.Fprintf(t.w, format+"\n", args...)
}

// StartHeartbeat begins painting an idle progress line every
// heartbeatInterval so the user sees forward motion while workers
// compile the first per-package test binary (no OnResult fires until
// the first mutant completes — a 10-60s silence on a non-trivial
// project). OnResult auto-stops the heartbeat when the first result
// arrives; callers should still defer StopHeartbeat in case zero
// mutants ever complete (e.g., upstream cancel, or all-cached run).
//
// Second call is a no-op: hbStopOnce fires on the first StopHeartbeat,
// so a re-spawned goroutine could never be stopped — the hbStop != nil
// guard prevents the leak.
//
// No-op when quiet, verbose, off-TTY, or total == 0 — those modes
// either suppress output entirely or print line-per-mutant, where a
// \r-based spinner would corrupt the transcript.
func (t *Terminal) StartHeartbeat() {
	if t.quiet || t.verbose || !t.isTTY || t.total == 0 {
		return
	}
	t.mu.Lock()
	if t.hbStop != nil {
		t.mu.Unlock()
		return
	}
	t.hbStop = make(chan struct{})
	t.hbDone = make(chan struct{})
	stop, done := t.hbStop, t.hbDone
	t.mu.Unlock()
	// Pass channels as args so the goroutine never races on t.hbStop /
	// t.hbDone field reads — the guard above prevents reassignment, but
	// keeping the goroutine's view local makes the invariant obvious.
	go t.heartbeatLoop(stop, done)
}

func (t *Terminal) heartbeatLoop(stop, done chan struct{}) {
	defer close(done)
	t.paintIdleLine() // immediate paint so the line shows up without waiting one tick
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			t.paintIdleLine()
		}
	}
}

// paintIdleLine renders a 0%-progress placeholder with a "(compiling)"
// suffix. The done>0 guard is defensive: OnResult always stops the
// heartbeat (waiting on <-hbDone, so the loop goroutine has exited)
// before it acquires t.mu to increment done, so under the current
// call order a tick can never observe done>0. Kept as an invariant
// pin so a future reordering of OnResult cannot silently clobber a
// live progress frame with an idle placeholder.
func (t *Terminal) paintIdleLine() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done > 0 {
		return
	}
	elapsed := time.Since(t.start).Round(time.Second)
	fmt.Fprintf(t.w, "\rTesting mutants [%s] 0/%d  0%%  %s  (compiling)", strings.Repeat(" ", barWidth), t.total, elapsed)
}

// StopHeartbeat halts the heartbeat goroutine (if any) and clears its
// trailing "(compiling)" suffix from the line so the next progress-bar
// paint starts from a clean slate. Idempotent via sync.Once; safe to
// call from any goroutine and multiple times.
func (t *Terminal) StopHeartbeat() {
	t.hbStopOnce.Do(func() {
		// Snapshot under t.mu so the read pairs with StartHeartbeat's
		// write — establishes happens-before without relying on caller
		// goroutine topology. Release before <-done to avoid deadlock
		// with the heartbeat goroutine, which acquires t.mu to paint.
		t.mu.Lock()
		stop, done := t.hbStop, t.hbDone
		t.mu.Unlock()
		if stop == nil {
			return
		}
		close(stop)
		<-done
		// Clear-to-end-of-line: the idle placeholder ends with
		// "  (compiling)" which is wider than the live progress bar,
		// so a bare \r-overwrite would leave that suffix on screen.
		t.mu.Lock()
		defer t.mu.Unlock()
		fmt.Fprint(t.w, "\r\033[K")
	})
}

// OnResult is the callback for each completed mutant.
func (t *Terminal) OnResult(m mutator.Mutant) {
	if t.quiet {
		return
	}
	// Stop the heartbeat before locking t.mu: the heartbeat goroutine
	// also acquires t.mu when painting, so blocking on its exit while
	// holding the lock would deadlock.
	t.StopHeartbeat()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.done++

	if t.verbose {
		fmt.Fprintf(t.w, "  [%d/%d] %s %s:%d:%d %s → %s (%s)\n",
			t.done, t.total,
			m.Status.String(),
			m.RelFile, m.Line, m.Col,
			m.Original, m.Replacement,
			m.Duration.Round(time.Millisecond))
		return
	}

	if t.isTTY {
		elapsed := time.Since(t.start).Round(time.Second)
		pctDone := 0
		filled := 0
		if t.total > 0 {
			pctDone = t.done * 100 / t.total
			filled = barWidth * t.done / t.total
		}
		var bar [barWidth]byte
		for i := range barWidth {
			if i < filled {
				bar[i] = '='
			} else {
				bar[i] = ' '
			}
		}
		fmt.Fprintf(t.w, "\rTesting mutants [%s] %d/%d  %d%%  %s", bar[:], t.done, t.total, pctDone, elapsed)
	}
}

// Summary prints the final summary.
func (t *Terminal) Summary(r *Report) {
	if t.isTTY && !t.verbose {
		fmt.Fprintln(t.w) // Newline after progress bar.
	}

	fmt.Fprintln(t.w)
	fmt.Fprintf(t.w, "  Killed:       %d  (%.1f%%)\n", r.MutantsKilled, pct(r.MutantsKilled, r.MutantsKilled+r.MutantsLived))
	fmt.Fprintf(t.w, "  Lived:        %d  (%.1f%%)\n", r.MutantsLived, pct(r.MutantsLived, r.MutantsKilled+r.MutantsLived))
	fmt.Fprintf(t.w, "  Not covered:  %d\n", r.MutantsNotCovered)
	fmt.Fprintf(t.w, "  Not viable:   %d\n", r.MutantsNotViable)
	fmt.Fprintf(t.w, "  Timed out:    %d\n", r.MutantsTimedOut)
	if r.MutantsInfraError > 0 {
		fmt.Fprintf(t.w, "  Infra errors: %d  (environment failure, not a mutant result — rerun to confirm)\n", r.MutantsInfraError)
	}
	if r.MutantsCached > 0 {
		fmt.Fprintf(t.w, "  Cached:       %d  (skipped)\n", r.MutantsCached)
	}
	if r.MutantsRechecked > 0 {
		fmt.Fprintf(t.w, "  Re-checked:   %d  (%d killed by tests the coverage map missed)\n", r.MutantsRechecked, r.MutantsRecheckKilled)
	}
	if r.MutantsSuppressed > 0 {
		fmt.Fprintf(t.w, "  Suppressed:   %d  (%s)\n", r.MutantsSuppressed, suppressedSources(r))
	}
	if r.MutantsEquivalent > 0 {
		fmt.Fprintf(t.w, "  Equivalent:   %d  (compiler-proven)\n", r.MutantsEquivalent)
	}
	fmt.Fprintf(t.w, "  Efficacy:     %.2f%%\n", r.TestEfficacy)
	fmt.Fprintln(t.w)
}

// suppressedSources labels the Suppressed line with where the mutants
// went. One source names itself; both split the count, so a reader can
// tell how much of the drop came from a project-wide --exclude-calls
// policy rather than annotations written at the site.
func suppressedSources(r *Report) string {
	byDirectives := r.MutantsSuppressed - r.MutantsSuppressedByCalls
	if r.MutantsSuppressedByCalls == 0 {
		return "directives"
	}
	if byDirectives == 0 {
		return "exclude-calls"
	}
	return fmt.Sprintf("%d directives, %d exclude-calls", byDirectives, r.MutantsSuppressedByCalls)
}

func pct(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) / float64(total) * 100
}
