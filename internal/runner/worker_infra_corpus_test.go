package runner

// Regression corpus for infrastructure-error classification.
//
// Real `go test` output for host failures, paired with the ground truth a
// human would assign. It exists because the classifier reads a stream with
// two authors — the Go toolchain and the code under test — and every bug
// found in it so far was a case of trusting the wrong one. A signature list
// change that looks harmless in isolation shows up here as a flipped verdict.
//
// The host-failure cases and the "a test printed it itself" cases both matter:
// missing a host failure resurrects the false KILLED that issue #79 reported,
// and matching a test's own output silently drops a genuine kill out of the
// efficacy denominator. Several cases are taken from the patch proposed on
// that issue (chrisophus/gomutants, feat/infra-error-classification), whose
// wider signature list is what surfaced the build-phase gap.

import (
	"errors"
	"testing"

	"github.com/gomutants/gomutants/internal/mutator"
)

func TestInfraClassificationCorpus(t *testing.T) {
	anyErr := errors.New("exit status 1")
	cases := []struct {
		name           string
		stdout, stderr string
		want           mutator.MutantStatus
	}{
		// --- Host failures: must not be scored as a kill. ---
		{"ENOSPC while the toolchain writes an object file",
			"FAIL\ttestmod [build failed]\n",
			"write /tmp/go-build123/b001/x.o: no space left on device\n",
			mutator.StatusInfraError},
		// The quota'd sibling of ENOSPC: on a shared CI host with per-user
		// quotas this is how the box runs out of writable space, and it is
		// what gomutants' own temp-file writes hit first (EDQUOT).
		{"disk quota exhausted while the toolchain writes",
			"FAIL\ttestmod [build failed]\n",
			"write /tmp/go-build123/b001/x.o: disk quota exceeded\n",
			mutator.StatusInfraError},
		{"toolchain cannot fork the compiler",
			"FAIL\ttestmod [build failed]\n",
			"go: fork/exec /usr/local/go/pkg/tool/darwin_arm64/compile: resource temporarily unavailable\n",
			mutator.StatusInfraError},
		{"linker runs out of memory",
			"FAIL\ttestmod [build failed]\n",
			"/usr/bin/ld: out of memory allocating 8388608 bytes\n",
			mutator.StatusInfraError},
		{"Go runtime OOM in the test binary",
			"fatal error: runtime: out of memory\n", "",
			mutator.StatusInfraError},
		{"OS thread exhaustion",
			"runtime: failed to create new OS thread (have 8193 already; errno=11)\nfatal error: newosproc\n", "",
			mutator.StatusInfraError},
		{"file descriptor exhaustion",
			"", "open /tmp/x: too many open files\n",
			mutator.StatusInfraError},
		{"read-only file system",
			"", "open /tmp/go-build/x: read-only file system\n",
			mutator.StatusInfraError},
		{"signatures match case-insensitively",
			"", "Write /VAR/TMP/X: NO SPACE LEFT ON DEVICE\n",
			mutator.StatusInfraError},
		// Verbatim `go test` output for the issue #79 shape, captured by
		// SIGKILLing a test binary from inside itself. Note what is *not*
		// here: the exit error is the anyErr above, a plain "exit status 1".
		// `go test` is not the process that died, so the only trace of the
		// kill is this line — an earlier draft read the exit error alone and
		// classified the whole scenario it was written for as a kill.
		{"OOM-killer reaps the test binary under a surviving go test",
			"signal: killed\nFAIL\tkv\t0.405s\nFAIL\n", "",
			mutator.StatusInfraError},

		// --- The tested code printed the phrase: still a kill. ---
		{"test asserts on an ENOSPC message",
			"--- FAIL: TestDiskFull\n    disk_test.go:9: got \"no space left on device\", want nil\n", "",
			mutator.StatusKilled},
		{"test asserts on an EAGAIN message",
			"--- FAIL: TestRetry\n    retry_test.go:14: err = resource temporarily unavailable\n", "",
			mutator.StatusKilled},
		{"test logs an out-of-memory message",
			"--- FAIL: TestAlloc\n    alloc_test.go:7: cache rejected entry: out of memory\n", "",
			mutator.StatusKilled},
		{"test asserts on an EIO message",
			"--- FAIL: TestRead\n    io_test.go:3: want input/output error\n", "",
			mutator.StatusKilled},
		{"generic wording from a running test binary",
			"", "resource temporarily unavailable\n",
			mutator.StatusKilled},
		// A suite that processes `go test` output prints the build markers as
		// fixture data. That must not promote the surrounding test-authored
		// text to the toolchain-only tier — gomutants' own corpus here is
		// exactly such a suite, and got misclassified by an earlier draft.
		{"test echoes a build marker as fixture data",
			"--- FAIL: TestCorpus\n    x_test.go:9: stdout: \"FAIL\\tm [build failed]\\n\" stderr: \"out of memory\"\n", "",
			mutator.StatusKilled},
		// Verbatim output from a panic in a non-test goroutine. There is no
		// `--- FAIL: ` line anywhere in it — the binary aborted before `go
		// test` could print one — so the marker alone would hand this to the
		// test-phase list, which matches the panic value's "too many open
		// files" and excuses a mutation that dropped a `defer f.Close()`.
		{"background goroutine panics on a host error the mutation caused",
			"panic: open /tmp/x: too many open files\n\ngoroutine 35 [running]:\nkv.TestGoroutinePanic.func1()\n\t/tmp/kv/a_test.go:10 +0x2c\nFAIL\tkv\t0.333s\nFAIL\n", "",
			mutator.StatusKilled},

		// --- Neither: the existing classifications still win. ---
		{"compile error beats an infra-looking stderr",
			"FAIL\ttestmod [build failed]\n",
			"worker-0.go:5:2: undefined: Foo: no space left on device\n",
			mutator.StatusNotViable},
		{"ordinary test failure",
			"--- FAIL: TestAdd\n", "add_test.go:7: Add(1,2) != 3\n",
			mutator.StatusKilled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyTestOutcome(anyErr, false, nil, tc.stdout, tc.stderr, false); got != tc.want {
				t.Errorf("classified %v, want %v\nstdout: %q\nstderr: %q", got, tc.want, tc.stdout, tc.stderr)
			}
		})
	}
}

// The RSS monitor and the per-mutant deadline decide before any signature is
// read: a host failure gomutants itself caused is a TIMED OUT, not an
// environment problem to be excused.
func TestInfraClassificationCorpusPrecedence(t *testing.T) {
	oom := "fatal error: out of memory\n"
	if got := classifyTestOutcome(errors.New("signal: killed"), true, nil, oom, "", false); got != mutator.StatusTimedOut {
		t.Errorf("memKilled with an OOM signature = %v, want TimedOut", got)
	}
	if got := classifyTestOutcome(nil, false, nil, oom, "", false); got != mutator.StatusLived {
		t.Errorf("passing tests with an OOM signature = %v, want Lived", got)
	}
}

// A stream that lost its tail to maxCapturedOutput cannot be read as "no test
// reported a failure": the `--- FAIL: ` line may be among the dropped bytes
// while an infra-looking phrase the test printed earlier survives. Such a
// mutant stays KILLED rather than being excused as an environment problem.
func TestInfraClassificationTruncatedOutputStaysKilled(t *testing.T) {
	anyErr := errors.New("exit status 1")
	chatty := "some_test.go:12: got \"no space left on device\"\n"
	if got := classifyTestOutcome(anyErr, false, nil, chatty, "", true); got != mutator.StatusKilled {
		t.Errorf("truncated stdout with an infra signature = %v, want Killed", got)
	}
	// The same stream captured whole has nothing vetoing the signature.
	if got := classifyTestOutcome(anyErr, false, nil, chatty, "", false); got != mutator.StatusInfraError {
		t.Errorf("whole stdout with an infra signature = %v, want InfraError", got)
	}

	// The unexplained-SIGKILL path reads the same veto: with the tail gone,
	// the `--- FAIL: ` line that would settle this as a kill may be among the
	// dropped bytes.
	sigkilled := errors.New("signal: killed")
	if got := classifyTestOutcome(sigkilled, false, nil, "ok\ttestmod\n", "", true); got != mutator.StatusKilled {
		t.Errorf("truncated stdout with an unexplained SIGKILL = %v, want Killed", got)
	}
	if got := classifyTestOutcome(sigkilled, false, nil, "ok\ttestmod\n", "", false); got != mutator.StatusInfraError {
		t.Errorf("whole stdout with an unexplained SIGKILL = %v, want InfraError", got)
	}
}

// cappedBuffer must set truncated exactly when it drops bytes — the flag is
// what the classifier above trusts, so an off-by-one here would silently
// re-enable the laundering it prevents.
func TestCappedBufferReportsTruncation(t *testing.T) {
	var whole cappedBuffer
	whole.Write(make([]byte, maxCapturedOutput))
	if whole.truncated {
		t.Error("a write that exactly fills the cap reported truncation")
	}
	whole.Write([]byte("x"))
	if !whole.truncated {
		t.Error("a write past the cap did not report truncation")
	}
}
