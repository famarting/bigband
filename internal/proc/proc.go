// Package proc holds small Unix-process helpers shared across bigband.
package proc

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Alive reports whether a process with the given pid is running. Uses
// kill(pid, 0) which delivers no signal but reports the process's existence
// (and our permission to address it). Returns false on any error.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// KillProcessGroupOnCancel arranges for context cancellation (timeout or stop)
// to deliver SIGTERM to the entire process group, not just the direct child.
// Without this, a shell that fork-execs subprocesses leaks them when the parent
// shell is killed: exec.CommandContext's default Cancel only signals the
// command's own pid. Callers must set c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
// before invoking this so the child becomes a process-group leader.
//
// WaitDelay bounds how long Run waits after Cancel before forcibly killing
// (SIGKILL) anything still hanging on.
func KillProcessGroupOnCancel(c *exec.Cmd) {
	c.Cancel = func() error {
		if c.Process == nil {
			return os.ErrProcessDone
		}
		return syscall.Kill(-c.Process.Pid, syscall.SIGTERM)
	}
	c.WaitDelay = 5 * time.Second
}
