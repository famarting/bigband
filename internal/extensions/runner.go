package extensions

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

// runResult is what a single spawn returns to the supervisor.
type runResult struct {
	pid       int
	startedAt time.Time
	exitedAt  time.Time
	exitCode  int
	signal    string
	err       error
}

// Duration helper. The runtime portion of an exit, calculated from startedAt and exitedAt.
func (r runResult) duration() time.Duration { return r.exitedAt.Sub(r.startedAt) }

// spawnAndWait runs the manifest's command to completion. The provided ctx
// controls early termination: cancelling it sends SIGTERM to the process
// group and (after the cmd's WaitDelay) escalates to SIGKILL.
//
// Stdout and stderr are appended to logPath. Each spawn opens the file fresh,
// so an external rotator can rename(old) and the next spawn opens the new
// file — but within one spawn the handle is held open for the lifetime of the
// child.
//
// pidCh, when non-nil, receives the child's pid as soon as it has started.
// Used by the supervisor to publish the started event with the right pid
// without racing the child's exit.
func spawnAndWait(ctx context.Context, m *Manifest, env map[string]string, logPath string, pidCh chan<- int) runResult {
	startedAt := time.Now().UTC()

	c := exec.CommandContext(ctx, m.Command[0], m.Command[1:]...) //nolint:gosec // command is from a user-authored manifest
	c.Dir = m.EffectiveWorkingDir()
	// Inherit the daemon's env (HOME, USER, LANG, etc.) and let the manifest
	// override or add. Mirrors launchd's "EnvironmentVariables augments inherited
	// env" semantics: if you only listed PATH in the manifest, you still get
	// HOME from the daemon. Stops bigband-slack and friends from panicking on
	// `os.UserHomeDir`.
	c.Env = mergeEnv(os.Environ(), env)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	killProcessGroupOnCancel(c)

	if err := os.MkdirAll(filepath.Dir(logPath), 0700); err != nil {
		return runResult{startedAt: startedAt, exitedAt: time.Now().UTC(), err: fmt.Errorf("creating log dir: %w", err)}
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return runResult{startedAt: startedAt, exitedAt: time.Now().UTC(), err: fmt.Errorf("opening log: %w", err)}
	}
	defer logFile.Close()

	// Mark each spawn with a delimiter so operators tailing the log can see
	// where one process ended and the next began.
	_, _ = fmt.Fprintf(logFile, "\n--- bigband-supervisor: spawning %v at %s ---\n",
		m.Command, startedAt.Format(time.RFC3339))
	c.Stdout = logFile
	c.Stderr = logFile

	if err := c.Start(); err != nil {
		return runResult{startedAt: startedAt, exitedAt: time.Now().UTC(), err: fmt.Errorf("start: %w", err)}
	}
	pid := c.Process.Pid
	if pidCh != nil {
		// Non-blocking send: if the supervisor already moved on, don't stall.
		select {
		case pidCh <- pid:
		default:
		}
	}

	waitErr := c.Wait()
	exitedAt := time.Now().UTC()
	res := runResult{pid: pid, startedAt: startedAt, exitedAt: exitedAt}

	switch {
	case waitErr == nil:
		res.exitCode = 0
	case errors.As(waitErr, new(*exec.ExitError)):
		var ee *exec.ExitError
		_ = errors.As(waitErr, &ee)
		res.exitCode = ee.ExitCode()
		// On macOS / Linux, signal-terminated processes report ExitCode == -1.
		if status, ok := ee.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			res.signal = status.Signal().String()
		}
	default:
		res.err = waitErr
	}
	_, _ = fmt.Fprintf(logFile, "--- bigband-supervisor: exited code=%d signal=%q duration=%s ---\n",
		res.exitCode, res.signal, res.duration().Round(time.Millisecond))
	_ = logFile.Sync()
	return res
}

// killProcessGroupOnCancel arranges for context cancellation to deliver
// SIGTERM to the entire process group, not just the direct child. Mirrors the
// pattern in internal/runner/runner.go (line 471) so subprocesses fork-execed
// by the extension don't leak. WaitDelay caps how long Wait blocks after
// Cancel before escalating to SIGKILL.
//
// Must be called after c.SysProcAttr is set.
func killProcessGroupOnCancel(c *exec.Cmd) {
	c.Cancel = func() error {
		if c.Process == nil {
			return os.ErrProcessDone
		}
		return syscall.Kill(-c.Process.Pid, syscall.SIGTERM)
	}
	c.WaitDelay = 5 * time.Second
}

// mergeEnv layers a manifest's env over the daemon's inherited env. Keys
// present in override replace the corresponding base entry; other base entries
// pass through unchanged. Keys present only in override are appended. Output
// is sorted so identical inputs produce identical output (test friendliness).
func mergeEnv(base []string, override map[string]string) []string {
	merged := make(map[string]string, len(base)+len(override))
	for _, kv := range base {
		eq := indexEq(kv)
		if eq < 0 {
			continue
		}
		merged[kv[:eq]] = kv[eq+1:]
	}
	for k, v := range override {
		merged[k] = v
	}
	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(merged))
	for _, k := range keys {
		out = append(out, k+"="+merged[k])
	}
	return out
}

// indexEq returns the index of the first '=' in kv, or -1.
func indexEq(kv string) int {
	for i := 0; i < len(kv); i++ {
		if kv[i] == '=' {
			return i
		}
	}
	return -1
}

// discardWriter silences subprocess output. Reserved for future uses (e.g. a
// "no log" manifest option). Currently unused.
var _ io.Writer = io.Discard
