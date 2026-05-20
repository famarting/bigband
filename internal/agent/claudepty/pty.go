package claudepty

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/creack/pty"
)

// defaultPtySize roughly matches a comfortable terminal. The exact dimensions
// don't matter for our purposes (we don't render the PTY output) — Claude just
// needs a sane TIOCGWINSZ on startup.
const (
	defaultPtyCols = 120
	defaultPtyRows = 40
)

// ptySession wraps a child process attached to a pseudo-terminal.
type ptySession struct {
	cmd  *exec.Cmd
	tty  *os.File      // master side; reads PTY output, writes to PTY input
	done chan struct{} // closed when the child has exited
}

// exited reports whether the underlying process has already exited. Used by
// the tail loop to break out of polling when claude dies unexpectedly.
func (s *ptySession) exited() bool {
	if s == nil || s.done == nil {
		return true
	}
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// startPty spawns command with args inside a PTY rooted at workDir. The PTY's
// output is consumed by a goroutine that copies to drain (set to io.Discard
// when the caller doesn't want it persisted). The process is killed if ctx
// fires before close is called.
func startPty(ctx context.Context, command, workDir string, args []string, drain io.Writer) (*ptySession, error) {
	if drain == nil {
		drain = io.Discard
	}
	c := exec.Command(command, args...)
	c.Dir = workDir
	c.Env = append(os.Environ(), "TERM=xterm-256color")
	tty, err := pty.StartWithSize(c, &pty.Winsize{Cols: defaultPtyCols, Rows: defaultPtyRows})
	if err != nil {
		return nil, fmt.Errorf("start pty: %w", err)
	}

	go func() { _, _ = io.Copy(drain, tty) }()

	done := make(chan struct{})
	go func() {
		_ = c.Wait()
		close(done)
	}()

	if ctx != nil {
		go func() {
			select {
			case <-ctx.Done():
				if c.Process != nil {
					_ = c.Process.Kill()
				}
			case <-done:
			}
		}()
	}
	return &ptySession{cmd: c, tty: tty, done: done}, nil
}

// sendEOT writes Ctrl-D into the PTY, which tells claude's interactive prompt
// to exit. We send it twice with a small gap because the first one only flushes
// any partially-typed input; the second one actually ends the session.
func (s *ptySession) sendEOT() {
	if s.tty == nil {
		return
	}
	_, _ = s.tty.Write([]byte{0x04})
}

// close shuts down the PTY and the child process. It forces a kill if claude
// is still alive after the caller's earlier EOT attempt, then waits for the
// reaper goroutine to confirm the exit. Always safe to call multiple times.
func (s *ptySession) close() {
	if s == nil {
		return
	}
	if s.cmd != nil && s.cmd.Process != nil && !s.exited() {
		_ = s.cmd.Process.Kill()
	}
	if s.done != nil {
		<-s.done
	}
	if s.tty != nil {
		_ = s.tty.Close()
		s.tty = nil
	}
}

// newSessionUUID returns a random UUIDv4 string. Used when starting a fresh
// session so we know the JSONL path before claude creates it. Pulled from
// crypto/rand so two concurrent runs in the same cwd don't collide.
func newSessionUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
