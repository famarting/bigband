// Package proc holds small Unix-process helpers shared across bigband.
package proc

import (
	"os"
	"syscall"
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
