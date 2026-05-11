// Package runner executes a single bigband task: pre-exec → claude → post-exec.
package runner

import (
	"context"
	"errors"
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
	"github.com/famarting/bigband/internal/events"
	"github.com/famarting/bigband/internal/paths"
	"github.com/famarting/bigband/internal/state"
	"github.com/famarting/bigband/internal/worktree"
)

// Run executes the task pipeline in the caller's goroutine. It is intended to
// be called from a goroutine (e.g. the cron handler).
// out receives a live copy of all output in addition to the log file;
// pass io.Discard when live streaming is not needed.
// pub receives lifecycle events; pass events.NopPublisher{} to disable.
// Cancelling ctx terminates any in-progress shell command or Claude invocation.
func Run(ctx context.Context, cfg *config.Config, task *config.Task, st *state.State, out io.Writer, pub events.Publisher) {
	if pub == nil {
		pub = events.NopPublisher{}
	}
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

	ts := task.RunTimestamp
	if ts == "" {
		ts = time.Now().UTC().Format("2006-01-02T15-04-05Z")
	}
	logPath, lf, err := openLog(task.Name, ts)
	if err != nil {
		fmt.Fprintf(out, "[%s] cannot open log file: %v\n", task.Name, err)
		return
	}
	defer lf.Close()

	w := io.MultiWriter(lf, out)
	logger := log.New(w, fmt.Sprintf("[%s] ", task.Name), log.LstdFlags)
	started := time.Now()

	if err := st.SetRunning(task.Name, os.Getpid(), task.Folder); err != nil {
		logger.Printf("WARNING: state update failed: %v", err)
	}

	logger.Printf("=== START task=%s folder=%s log=%s", task.Name, task.Folder, logPath)
	logger.Printf("  schedule: %s", task.Schedule)
	logger.Printf("  prompt:   %s", strings.TrimSpace(task.Prompt))

	// runID is task-name + log timestamp. Stable per run, used to correlate
	// every event for this run.
	runID := task.Name + "/" + filepath.Base(strings.TrimSuffix(logPath, ".log"))
	pub.Publish(events.Envelope{
		Type:        events.TypeTaskRunStarted,
		RunID:       runID,
		TaskName:    task.Name,
		TriggeredBy: task.TriggeredBy,
		Data: events.MustData(events.TaskRunStartedData{
			Folder:     task.Folder,
			Schedule:   task.Schedule,
			OneOff:     task.IsOneOff(),
			Worktree:   task.ShouldUseWorktree(),
			Resume:     task.ResumeSessionID != "",
			ResumeFrom: task.ResumeSessionID,
			Ephemeral:  task.Ephemeral,
		}),
	})

	// Declare all variables before any goto so the compiler is happy.
	var (
		status    = state.StatusOK
		repoRoot  string
		wtPath    string
		runDir    = task.Folder // updated after worktree creation; fallback is original folder
		sessionID string
		finalMsg  string // last non-empty assistant text from the final turn
		replyPath string // sidecar file holding finalMsg, written before post_exec
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
			pub.Publish(events.Envelope{
				Type:     events.TypeTaskRunPreFailed,
				RunID:    runID,
				TaskName: task.Name,
				Data: events.MustData(events.TaskRunPreFailedData{
					Command: cmd,
					Error:   err.Error(),
				}),
			})
			goto postExec
		}
	}

	// Worktree is created after pre_exec, so any "git pull" has already run
	// and HEAD is up-to-date before we snapshot it into the worktree.
	runDir, repoRoot, wtPath = resolveRunDir(task, w, logger, st)
	if wtPath != "" {
		pub.Publish(events.Envelope{
			Type:     events.TypeTaskRunWorktreeReady,
			RunID:    runID,
			TaskName: task.Name,
			Data: events.MustData(events.TaskRunWorktreeReadyData{
				WorktreePath: wtPath,
				RunDir:       runDir,
			}),
		})
	}

	// Claude — runs in a loop when Claude calls ScheduleWakeup to self-pace.
	{
		logger.Println("--- claude ---")
		flags := cfg.EffectiveClaudeFlags(task)
		timeout := cfg.EffectiveTimeout(task)
		deadline := time.Now().Add(timeout)

		var (
			wakeup *WakeupRequest
			runErr error
			msg    string
		)
		// task.ResumeSessionID is set by IPC submit for follow-up runs that
		// continue a previous Claude session. Empty for normal scheduled runs.
		initialResume := task.ResumeSessionID
		if initialResume != "" {
			logger.Printf("resuming session %s", initialResume)
		}
		var sessionAnnounced bool
		announceSession := func() {
			if sessionAnnounced || sessionID == "" {
				return
			}
			pub.Publish(events.Envelope{
				Type:     events.TypeClaudeSessionStarted,
				RunID:    runID,
				TaskName: task.Name,
				Data:     events.MustData(events.ClaudeSessionStartedData{SessionID: sessionID}),
			})
			sessionAnnounced = true
		}
		emitTurn := func() {
			pub.Publish(events.Envelope{
				Type:     events.TypeClaudeTurnCompleted,
				RunID:    runID,
				TaskName: task.Name,
				Data: events.MustData(events.ClaudeTurnCompletedData{
					FinalMessage: msg,
					SessionID:    sessionID,
				}),
			})
		}
		sessionID, wakeup, msg, runErr = runClaude(ctx, flags, task.Prompt, initialResume, runDir, lf, out, timeout)
		if sessionID == "" && initialResume != "" {
			sessionID = initialResume
		}
		announceSession()
		emitTurn()
		if msg != "" {
			finalMsg = msg
		}
		for wakeup != nil && runErr == nil {
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
			pub.Publish(events.Envelope{
				Type:     events.TypeClaudeWakeup,
				RunID:    runID,
				TaskName: task.Name,
				Data: events.MustData(events.ClaudeWakeupData{
					DelaySeconds: wakeup.DelaySeconds,
					Prompt:       resumePrompt,
				}),
			})
			logger.Printf("claude scheduled wakeup in %s — sleeping before resuming session %s", delay.Round(time.Second), sessionID)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				logger.Printf("cancelled during wakeup sleep — not resuming")
				runErr = ctx.Err()
			}
			if runErr != nil {
				break
			}
			remaining = time.Until(deadline)
			sessionID, wakeup, msg, runErr = runClaude(ctx, flags, resumePrompt, sessionID, runDir, lf, out, remaining)
			announceSession()
			emitTurn()
			if msg != "" {
				finalMsg = msg
			}
		}

		if sessionID != "" {
			if err := st.SetSessionID(task.Name, sessionID); err != nil {
				logger.Printf("WARNING: state update failed: %v", err)
			}
		}
		if runErr != nil {
			logger.Printf("claude failed: %v", runErr)
			switch {
			case errors.Is(ctx.Err(), context.Canceled):
				status = state.StatusStopped
			case errors.Is(runErr, context.DeadlineExceeded):
				status = state.StatusTimeout
			default:
				status = state.StatusFailed
			}
		}
	}

postExec:
	// Persist Claude's final assistant message to a sidecar file alongside the
	// log so post_exec scripts and downstream integrations can read it without
	// reparsing the stream-json log. Empty when the run produced no text
	// (e.g. ended on a tool call) — surface that, don't fabricate.
	if finalMsg != "" {
		replyPath = strings.TrimSuffix(logPath, ".log") + ".reply.txt"
		if err := os.WriteFile(replyPath, []byte(finalMsg), 0600); err != nil {
			logger.Printf("WARNING: failed to write reply file: %v", err)
			replyPath = ""
		}
	}

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
		"BIGBAND_REPLY_FILE=" + replyPath,
		"BIGBAND_SESSION_ID=" + sessionID,
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

	if err := st.SetDone(task.Name, status, dur, logPath, replyPath); err != nil {
		logger.Printf("WARNING: state update failed: %v", err)
	}

	pub.Publish(events.Envelope{
		Type:        events.TypeTaskRunCompleted,
		RunID:       runID,
		TaskName:    task.Name,
		TriggeredBy: task.TriggeredBy,
		Data: events.MustData(events.TaskRunCompletedData{
			Status:       string(status),
			FinalMessage: finalMsg,
			LogPath:      logPath,
			ReplyFile:    replyPath,
			SessionID:    sessionID,
			Folder:       task.Folder,
			WorktreePath: wtPath,
			DurationMS:   dur.Milliseconds(),
		}),
	})

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
	if !task.ShouldUseWorktree() {
		logger.Printf("NOTE: worktree disabled — running in original folder %s", task.Folder)
		return
	}
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

func openLog(taskName, ts string) (string, *os.File, error) {
	dir := paths.TaskLogDir(taskName)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", nil, err
	}
	logPath := filepath.Join(dir, ts+".log")
	// 0600 explicitly: log files contain Claude's full output (prompts,
	// tool I/O, final messages) and could legitimately include secrets the
	// task was asked to handle. Defense-in-depth alongside the 0700 dir.
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
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

func runClaude(ctx context.Context, flags []string, prompt, resumeSessionID, dir string, log, live io.Writer, timeout time.Duration) (sessionID string, wakeup *WakeupRequest, finalMessage string, err error) {
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
	// Parse stream-json output: raw NDJSON is discarded, plain rendering goes
	// to the log file, colorized rendering to the live writer when it's a TTY.
	sw := newStreamWriter(io.Discard, log, live, isTerminal(live))
	c.Stdout = sw
	c.Stderr = io.MultiWriter(log, live)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	killProcessGroupOnCancel(c)
	err = c.Run()
	_, sessionID = sw.getResult()
	wakeup = sw.getWakeup()
	finalMessage = sw.getFinalMessage()
	return sessionID, wakeup, finalMessage, err
}

// isTerminal returns true when w is an *os.File backed by a character device
// (i.e. a TTY rather than a pipe or regular file). Used to decide whether to
// emit ANSI color escapes.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
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
			_ = os.Remove(old)
		}
	}
}
