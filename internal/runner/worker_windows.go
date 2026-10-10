//go:build windows

package runner

import (
	"os"
	"os/exec"
	"syscall"
)

// pgroupRSSBytes is a best-effort no-op on Windows. The Unix RSS monitor
// queries `ps -o rss= -g <pgid>`, which has no portable Windows equivalent.
// Returning 0 means the `pgroupRSSBytes(pgid) > maxSubprocRSSBytes` check
// in Worker.Test never fires, so runaway mutants on Windows are bounded
// only by the per-mutant context timeout. A future port could use job
// objects via golang.org/x/sys/windows; for now the timeout backstop is
// the contract.
func pgroupRSSBytes(_ int) int64 { return 0 }

// killPgroup terminates the child process by PID. On Windows there is no
// portable concept of a POSIX process group; applyProcessGroup puts the
// child in its own CREATE_NEW_PROCESS_GROUP, but Kill only signals the
// immediate process, not its descendants. Children of `go test` (the
// compiled test binary, vet, etc.) may leak if they outlive their parent.
// Acceptable for the typical case where `go test` itself fans out and
// waits on its children before exiting.
func killPgroup(pid int) {
	p, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = p.Kill()
}

// applyProcessGroup puts the child in a new Windows process group via
// CREATE_NEW_PROCESS_GROUP (0x00000200). This is the closest analog to
// Setpgid: it detaches the child from the parent's console-control-event
// propagation so a Ctrl-C on the parent doesn't immediately knock down
// the child, and it gives us a handle we could later target with a job
// object.
func applyProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// processGroup returns pid unchanged. Windows has no process-group ID
// equivalent to POSIX pgid, so killPgroup operates directly on pid.
func processGroup(pid int) int { return pid }
