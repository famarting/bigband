package proc

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// AcquireInstanceLock takes an exclusive non-blocking flock on path and writes
// the current PID into the file. Returns a release function on success.
//
// On contention it returns the PID recorded in the file (or 0 if unreadable)
// alongside an error, so callers can produce a clear "already running as
// pid=N" message — the symptom we want to surface loudly, since the alternative
// (two processes racing on shared state) is silent and corrupting.
//
// The lock is owned by the file descriptor, so a crashed holder releases its
// lock automatically when the kernel closes the fd. The PID inside the file is
// informational only.
func AcquireInstanceLock(path string) (release func(), holderPID int, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, 0, fmt.Errorf("create lock dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, 0, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder := readPID(f)
		f.Close()
		return nil, holder, fmt.Errorf("another instance is running (lock %s held): %w", path, err)
	}

	// Successfully locked. Rewrite the file with our PID so operators can see
	// who owns it. Truncate first because the previous holder's PID may be
	// longer than ours.
	if err := f.Truncate(0); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		return nil, 0, fmt.Errorf("truncate lock file: %w", err)
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		return nil, 0, fmt.Errorf("write pid: %w", err)
	}

	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		// Best-effort: remove the file so a stale PID isn't left behind. The
		// flock itself was the source of truth for liveness; the file content
		// is only there for human debugging.
		_ = os.Remove(path)
	}, 0, nil
}

// readPID returns the PID written into an already-open lock file, or 0 if it
// is empty / unparseable.
func readPID(f *os.File) int {
	buf := make([]byte, 32)
	n, _ := f.ReadAt(buf, 0)
	if n <= 0 {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil {
		return 0
	}
	return pid
}
