// Package runner executes a single bigband task: pre-exec → claude → post-exec.
package runner

import (
	"context"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/famarting/bigband/internal/config"
	"github.com/famarting/bigband/internal/paths"
	"github.com/famarting/bigband/internal/state"
	"github.com/famarting/bigband/internal/worktree"
)

// Run executes the task pipeline in the caller's goroutine. It is intended to
// be called from a goroutine (e.g. the cron handler).
// out receives a live copy of all output in addition to the log file;
// pass io.Discard when live streaming is not needed.
// Cancelling ctx terminates any in-progress shell command or Claude invocation.
func Run(ctx context.Context, cfg *config.Config, task *config.Task, st *state.State, out io.Writer) {
	jitter := task.JitterDuration()
	if jitter > 0 {
		sleep := time.Duration(rand.Int63n(int64(jitter)))
		fmt.Fprintf(out, "[%s] sleeping for %s before starting (jitter)\n", task.Name, sleep.Round(time.Second))
		select {
		case <-time.After(sleep):
		case <-ctx.Done():
			log.Printf("bigband: task %q cancelled during jitter sleep — not started", task.Name)
			return
		}
	}

	release, acquired := state.Lock(task.Name)
	if !acquired {
		fmt.Fprintf(out, "[%s] skipped — previous run still active\n", task.Name)
		return
	}
	defer release()

	logPath, lf, err := openLog(task.Name)
	if err != nil {
		fmt.Fprintf(out, "[%s] cannot open log file: %v\n", task.Name, err)
		return
	}
	defer lf.Close()

	w := io.MultiWriter(lf, out)
	logger := log.New(w, fmt.Sprintf("[%s] ", task.Name), log.LstdFlags)
	started := time.Now()

	if err := st.SetRunning(task.Name, os.Getpid()); err != nil {
		logger.Printf("WARNING: state update failed: %v", err)
	}

	logger.Printf("=== START task=%s folder=%s log=%s", task.Name, task.Folder, logPath)
	logger.Printf("  schedule: %s", task.Schedule)
	logger.Printf("  prompt:   %s", strings.TrimSpace(task.Prompt))

	// Declare all variables before any goto so the compiler is happy.
	var (
		status   = state.StatusOK
		repoRoot string
		wtPath   string
		runDir   = task.Folder // updated after worktree creation; fallback is original folder
	)

	// Pre-exec runs in the original folder so that commands like "git pull"
	// update the main-branch repo before the worktree is created from HEAD.
	if len(task.PreExec) > 0 {
		logger.Println("--- pre_exec ---")
	}
	for _, cmd := range task.PreExec {
		if err := runShell(ctx, cfg, cmd, task.Folder, w, 30*time.Minute); err != nil {
			logger.Printf("pre_exec failed: %v", err)
			status = state.StatusPreFailed
			goto postExec
		}
	}

	// Worktree is created after pre_exec, so any "git pull" has already run
	// and HEAD is up-to-date before we snapshot it into the worktree.
	runDir, repoRoot, wtPath = resolveRunDir(task, w, logger, st)

	// Claude — runs in a loop when Claude calls ScheduleWakeup to self-pace.
	{
		logger.Println("--- claude ---")
		flags := cfg.EffectiveClaudeFlags(task)
		timeout := cfg.EffectiveTimeout(task)
		deadline := time.Now().Add(timeout)

		sessionID, wakeup, err := runClaude(ctx, flags, task.Prompt, "", runDir, w, timeout)
		for wakeup != nil && err == nil {
			if sessionID != "" {
				if err2 := st.SetSessionID(task.Name, sessionID); err2 != nil {
					logger.Printf("WARNING: state update failed: %v", err2)
				}
			}
			delay := time.Duration(wakeup.DelaySeconds) * time.Second
			remaining := time.Until(deadline)
			if delay >= remaining {
				logger.Printf("scheduled wakeup in %s would exceed task timeout — stopping", delay.Round(time.Second))
				break
			}
			resumePrompt := wakeup.Prompt
			if resumePrompt == "" || resumePrompt == "<<autonomous-loop-dynamic>>" {
				resumePrompt = task.Prompt
			}
			logger.Printf("claude scheduled wakeup in %s — sleeping before resuming session %s", delay.Round(time.Second), sessionID)
			time.Sleep(delay)
			remaining = time.Until(deadline)
			sessionID, wakeup, err = runClaude(ctx, flags, resumePrompt, sessionID, runDir, w, remaining)
		}

		if sessionID != "" {
			if err := st.SetSessionID(task.Name, sessionID); err != nil {
				logger.Printf("WARNING: state update failed: %v", err)
			}
		}
		if err != nil {
			logger.Printf("claude failed: %v", err)
			switch {
			case ctx.Err() != nil:
				status = state.StatusStopped
			case strings.Contains(err.Error(), "context deadline exceeded"):
				status = state.StatusTimeout
			default:
				status = state.StatusFailed
			}
		}
	}

postExec:
	// Post-exec runs in runDir (worktree when available) so it can inspect or
	// commit whatever Claude produced. Falls back to task.Folder if no worktree.
	if len(task.PostExec) > 0 {
		logger.Println("--- post_exec ---")
	}
	env := []string{
		"BIGBAND_STATUS=" + string(status),
		"BIGBAND_LOG=" + logPath,
		"BIGBAND_TASK=" + task.Name,
		"BIGBAND_WORKTREE=" + wtPath,
	}
	for _, cmd := range task.PostExec {
		if err := runShellWithEnv(ctx, cfg, cmd, runDir, w, 10*time.Minute, env); err != nil {
			logger.Printf("post_exec error: %v", err)
		}
	}

	// Worktree cleanup.
	if wtPath != "" {
		if task.ShouldKeepWorktree() {
			logger.Printf("keeping worktree %s", wtPath)
		} else {
			if err := worktree.Remove(repoRoot, wtPath); err != nil {
				logger.Printf("WARNING: failed to remove worktree %s: %v", wtPath, err)
			} else {
				logger.Printf("removed worktree %s", wtPath)
				_ = st.SetWorktreePath(task.Name, "")
			}
		}
	}

	dur := time.Since(started)
	logger.Printf("=== END status=%s duration=%s", status, dur.Round(time.Second))

	if err := st.SetDone(task.Name, status, dur, logPath); err != nil {
		logger.Printf("WARNING: state update failed: %v", err)
	}

	trimLogs(task.Name, cfg.Defaults.RetainLogs)
}

// resolveRunDir creates a git worktree for the task and returns:
//   - runDir: where pre_exec / claude / post_exec should run
//   - repoRoot: the repo root (needed for later worktree removal)
//   - wtPath: the worktree path (empty if no worktree was created)
//
// On any failure it falls back gracefully to task.Folder.
func resolveRunDir(task *config.Task, w io.Writer, logger *log.Logger, st *state.State) (runDir, repoRoot, wtPath string) {
	runDir = task.Folder
	root, err := worktree.RepoRoot(task.Folder)
	if err != nil {
		logger.Printf("NOTE: skipping worktree — %v", err)
		return
	}
	repoRoot = root

	wt := worktree.DefaultPath(repoRoot, task.Name)

	// With reuse_worktree, skip re-creation if the worktree already exists so
	// that Claude picks up whatever state the previous run left behind.
	if task.ShouldReuseWorktree() {
		if _, err := os.Stat(wt); err == nil {
			dir := worktree.SubDir(repoRoot, wt, task.Folder)
			if _, err := os.Stat(dir); err == nil {
				wtPath = wt
				runDir = dir
				logger.Printf("reusing existing worktree %s", wtPath)
				if err := st.SetWorktreePath(task.Name, wtPath); err != nil {
					logger.Printf("WARNING: state update failed: %v", err)
				}
				return
			}
		}
		// Worktree missing (first run, or was manually removed) — fall through
		// to CreateOrReplace to build it fresh.
	}

	if err := worktree.CreateOrReplace(repoRoot, wt, task.Name, w); err != nil {
		logger.Printf("WARNING: worktree creation failed: %v — running in original folder", err)
		return
	}

	dir := worktree.SubDir(repoRoot, wt, task.Folder)
	if _, err := os.Stat(dir); err != nil {
		logger.Printf("WARNING: worktree subdir %s missing — running in original folder", dir)
		_ = worktree.Remove(repoRoot, wt)
		return
	}

	wtPath = wt
	runDir = dir
	logger.Printf("created worktree %s", wtPath)
	if err := st.SetWorktreePath(task.Name, wtPath); err != nil {
		logger.Printf("WARNING: state update failed: %v", err)
	}
	return
}

func openLog(taskName string) (string, *os.File, error) {
	dir := paths.TaskLogDir(taskName)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", nil, err
	}
	ts := time.Now().UTC().Format("2006-01-02T15-04-05Z")
	logPath := filepath.Join(dir, ts+".log")
	f, err := os.Create(logPath)
	if err != nil {
		return "", nil, err
	}
	// Update latest symlink.
	latest := paths.TaskLogLatest(taskName)
	_ = os.Remove(latest)
	_ = os.Symlink(logPath, latest)
	return logPath, f, nil
}

func runShell(ctx context.Context, cfg *config.Config, cmd, dir string, w io.Writer, timeout time.Duration) error {
	return runShellWithEnv(ctx, cfg, cmd, dir, w, timeout, nil)
}

func runShellWithEnv(ctx context.Context, cfg *config.Config, cmd, dir string, w io.Writer, timeout time.Duration, extra []string) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c := exec.CommandContext(ctx, cfg.EffectiveShell(), "-c", cmd)
	c.Dir = dir
	c.Stdout = w
	c.Stderr = w
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	killProcessGroupOnCancel(c)
	if len(extra) > 0 {
		c.Env = append(os.Environ(), extra...)
	}
	return c.Run()
}

func runClaude(ctx context.Context, flags []string, prompt, resumeSessionID, dir string, w io.Writer, timeout time.Duration) (sessionID string, wakeup *WakeupRequest, err error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var args []string
	if resumeSessionID != "" {
		args = append(append([]string{}, flags...), "--resume", resumeSessionID, prompt)
	} else {
		args = append(append([]string{}, flags...), prompt)
	}
	c := exec.CommandContext(ctx, "claude", args...)
	c.Dir = dir
	// Parse stream-json output: raw NDJSON is discarded, formatted activity
	// goes to w (log file + optional live terminal).
	sw := newStreamWriter(io.Discard, w)
	c.Stdout = sw
	c.Stderr = w
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	killProcessGroupOnCancel(c)
	err = c.Run()
	_, sessionID = sw.getResult()
	wakeup = sw.getWakeup()
	return sessionID, wakeup, err
}

// killProcessGroupOnCancel arranges for context cancellation (timeout or stop)
// to deliver SIGTERM to the entire process group, not just the direct child.
// Without this, a shell that fork-execs subprocesses leaks them when the parent
// shell is killed: exec.CommandContext's default Cancel only signals the
// command's own pid. Setpgid:true (set by the caller) makes the child a pgroup
// leader, so we can address the whole tree as -pgid.
//
// WaitDelay bounds how long Run waits after Cancel before forcibly killing
// (SIGKILL) anything still hanging on. Must be called after c.SysProcAttr is set.
func killProcessGroupOnCancel(c *exec.Cmd) {
	c.Cancel = func() error {
		if c.Process == nil {
			return os.ErrProcessDone
		}
		return syscall.Kill(-c.Process.Pid, syscall.SIGTERM)
	}
	c.WaitDelay = 5 * time.Second
}

func trimLogs(taskName string, retain int) {
	if retain <= 0 {
		return
	}
	dir := paths.TaskLogDir(taskName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var logs []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".log") && e.Name() != "latest.log" {
			logs = append(logs, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(logs)
	if len(logs) > retain {
		for _, old := range logs[:len(logs)-retain] {
			os.Remove(old)
		}
	}
}
