package state

import (
	"os"
	"syscall"

	"github.com/famarting/bigband/internal/paths"
)

// Lock tries to acquire an exclusive flock on the task's lock file.
// Returns a release function and true if successful; false if already locked.
func Lock(taskName string) (release func(), acquired bool) {
	if err := os.MkdirAll(paths.StateDir(), 0700); err != nil {
		return nil, false
	}
	path := paths.TaskLockFile(taskName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, false
	}
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		f.Close()
		return nil, false
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck
		f.Close()
	}, true
}
