// Package runner executes a single bigband job: pre-exec → agent → post-exec.
//
// The "agent" step is delegated to an agent.Agent implementation (Claude Code
// by default; other providers register themselves via blank imports in main).
// This package only deals with the generic lifecycle — worktrees, pre/post
// shells, the optional wakeup retry loop, lifecycle events — never with
// provider-specific protocols.
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

	"github.com/famarting/bigband/internal/agent"
	"github.com/famarting/bigband/internal/config"
	"github.com/famarting/bigband/internal/events"
	"github.com/famarting/bigband/internal/paths"
	"github.com/famarting/bigband/internal/proc"
	"github.com/famarting/bigband/internal/state"
	"github.com/famarting/bigband/internal/worktree"
)

// Run executes the job pipeline in the caller's goroutine. It is intended to
// be called from a goroutine (e.g. the cron handler).
// out receives a live copy of all output in addition to the log file;
// pass io.Discard when live streaming is not needed.
// pub receives lifecycle events; pass events.NopPublisher{} to disable.
// Cancelling ctx terminates any in-progress shell command or Claude invocation.
func Run(ctx context.Context, cfg *config.Config, job *config.Job, st *state.State, out io.Writer, pub events.Publisher) {
	if pub == nil {
		pub = events.NopPublisher{}
	}
	jitter := job.JitterDuration()
	if jitter > 0 {
		sleep := time.Duration(rand.Int63n(int64(jitter)))
		fmt.Fprintf(out, "[%s] sleeping for %s before starting (jitter)\n", job.Name, sleep.Round(time.Second))
		select {
		case <-time.After(sleep):
		case <-ctx.Done():
			log.Printf("bigband: job %q cancelled during jitter sleep — not started", job.Name)
			return
		}
	}

	release, acquired := state.Lock(job.Name)
	if !acquired {
		fmt.Fprintf(out, "[%s] skipped — previous run still active\n", job.Name)
		return
	}
	defer release()

	ts := job.RunTimestamp
	if ts == "" {
		ts = time.Now().UTC().Format("2006-01-02T15-04-05Z")
	}
	logPath, lf, err := openLog(job.Name, ts)
	if err != nil {
		fmt.Fprintf(out, "[%s] cannot open log file: %v\n", job.Name, err)
		return
	}
	defer lf.Close()

	w := io.MultiWriter(lf, out)
	logger := log.New(w, fmt.Sprintf("[%s] ", job.Name), log.LstdFlags)
	started := time.Now()

	if err := st.SetRunning(job.Name, os.Getpid(), job.Folder); err != nil {
		logger.Printf("WARNING: state update failed: %v", err)
	}
	// Snapshot the run's input parameters into state so `bigband rerun` can
	// reproduce the invocation later — critical for ephemeral submits, whose
	// prompt would otherwise only live in the (prunable) log file.
	timeoutStr := ""
	if job.Timeout != nil {
		timeoutStr = job.Timeout.String()
	}
	if err := st.SetJobParams(job.Name, state.JobParams{
		Prompt:   job.Prompt,
		PreExec:  job.PreExec,
		PostExec: job.PostExec,
		Worktree: job.Worktree,
		Timeout:  timeoutStr,
		Model:    job.Model,
		Effort:   job.Effort,
		Agent:    job.Agent,
	}); err != nil {
		logger.Printf("WARNING: state update failed: %v", err)
	}

	logger.Printf("=== START job=%s folder=%s log=%s", job.Name, job.Folder, logPath)
	logger.Printf("  schedule: %s", job.Schedule)
	logger.Printf("  prompt:   %s", strings.TrimSpace(job.Prompt))

	// runID is job-name + log timestamp. Stable per run, used to correlate
	// every event for this run.
	runID := job.Name + "/" + filepath.Base(strings.TrimSuffix(logPath, ".log"))
	pub.Publish(events.Envelope{
		Type:        events.TypeJobRunStarted,
		RunID:       runID,
		JobName:     job.Name,
		TriggeredBy: job.TriggeredBy,
		Data: events.MustData(events.JobRunStartedData{
			Folder:     job.Folder,
			Schedule:   job.Schedule,
			OneOff:     job.IsOneOff(),
			Worktree:   job.ShouldUseWorktree(),
			Resume:     job.ResumeSessionID != "",
			ResumeFrom: job.ResumeSessionID,
			Ephemeral:  job.Ephemeral,
		}),
	})

	// Declare all variables before any goto so the compiler is happy.
	var (
		status     = state.StatusOK
		repoRoot   string
		wtPath     string
		runDir     = job.Folder // updated after worktree creation; fallback is original folder
		sessionID  string
		finalMsg   string // last non-empty assistant text from the final turn
		replyPath  string // sidecar file holding finalMsg, written before post_exec
		jobTimeout = cfg.EffectiveTimeout(job)
	)

	// Pre-exec runs in the original folder so that commands like "git pull"
	// update the main-branch repo before the worktree is created from HEAD.
	if len(job.PreExec) > 0 {
		logger.Println("--- pre_exec ---")
	}
	for _, cmd := range job.PreExec {
		if err := runShell(ctx, cfg, cmd, job.Folder, w, jobTimeout); err != nil {
			logger.Printf("pre_exec failed: %v", err)
			status = state.StatusPreFailed
			pub.Publish(events.Envelope{
				Type:    events.TypeJobRunPreFailed,
				RunID:   runID,
				JobName: job.Name,
				Data: events.MustData(events.JobRunPreFailedData{
					Command: cmd,
					Error:   err.Error(),
				}),
			})
			goto postExec
		}
	}

	// Worktree is created after pre_exec, so any "git pull" has already run
	// and HEAD is up-to-date before we snapshot it into the worktree.
	runDir, repoRoot, wtPath = resolveRunDir(job, w, logger, st)
	if wtPath != "" {
		pub.Publish(events.Envelope{
			Type:    events.TypeJobRunWorktreeReady,
			RunID:   runID,
			JobName: job.Name,
			Data: events.MustData(events.JobRunWorktreeReadyData{
				WorktreePath: wtPath,
				RunDir:       runDir,
			}),
		})
	}

	// Agent step — runs in a loop when the provider returns a Wakeup
	// (e.g. Claude's ScheduleWakeup tool). Providers that don't self-reschedule
	// return Wakeup=nil and the loop exits after one iteration.
	{
		agentName := cfg.EffectiveAgent(job)
		ag, agErr := agent.Get(agentName)
		if agErr != nil {
			logger.Printf("agent lookup failed: %v", agErr)
			status = state.StatusFailed
			goto postExec
		}
		logger.Printf("--- %s ---", ag.Name())

		timeout := cfg.EffectiveTimeout(job)
		deadline := time.Now().Add(timeout)
		// One overall deadline bounds every turn (initial + wakeups), so each
		// agent.Run inherits whatever time is left. The provider does not
		// need to re-apply a per-call timeout.
		agentCtx, cancel := context.WithDeadline(ctx, deadline)
		defer cancel()

		baseReq := agent.Request{
			WorkDir:    runDir,
			Model:      cfg.EffectiveModel(job),
			Effort:     cfg.EffectiveEffort(job),
			ExtraFlags: []string{},
			LogWriter:  lf,
			Live:       out,
		}

		// job.ResumeSessionID is set by IPC submit for follow-up runs that
		// continue a previous agent session. Empty for normal scheduled runs.
		initialResume := job.ResumeSessionID
		if initialResume != "" {
			logger.Printf("resuming session %s", initialResume)
		}

		var sessionAnnounced bool
		announceSession := func() {
			if sessionAnnounced || sessionID == "" {
				return
			}
			pub.Publish(events.Envelope{
				Type:    events.TypeClaudeSessionStarted,
				RunID:   runID,
				JobName: job.Name,
				Data:    events.MustData(events.ClaudeSessionStartedData{SessionID: sessionID}),
			})
			sessionAnnounced = true
		}
		emitTurn := func(msg string) {
			pub.Publish(events.Envelope{
				Type:    events.TypeClaudeTurnCompleted,
				RunID:   runID,
				JobName: job.Name,
				Data: events.MustData(events.ClaudeTurnCompletedData{
					FinalMessage: msg,
					SessionID:    sessionID,
				}),
			})
		}

		req := baseReq
		req.Prompt = job.Prompt
		req.ResumeSessionID = initialResume
		res, runErr := ag.Run(agentCtx, req)
		if res.SessionID != "" {
			sessionID = res.SessionID
		} else if initialResume != "" {
			sessionID = initialResume
		}
		announceSession()
		emitTurn(res.FinalMessage)
		if res.FinalMessage != "" {
			finalMsg = res.FinalMessage
		}

		for res.Wakeup != nil && runErr == nil {
			if sessionID != "" {
				if err2 := st.SetSessionID(job.Name, sessionID); err2 != nil {
					logger.Printf("WARNING: state update failed: %v", err2)
				}
			}
			delay := res.Wakeup.Delay
			remaining := time.Until(deadline)
			if delay >= remaining {
				logger.Printf("scheduled wakeup in %s would exceed job timeout — stopping", delay.Round(time.Second))
				break
			}
			resumePrompt := res.Wakeup.Prompt
			if resumePrompt == "" || resumePrompt == "<<autonomous-loop-dynamic>>" {
				resumePrompt = job.Prompt
			}
			pub.Publish(events.Envelope{
				Type:    events.TypeClaudeWakeup,
				RunID:   runID,
				JobName: job.Name,
				Data: events.MustData(events.ClaudeWakeupData{
					DelaySeconds: int(delay / time.Second),
					Prompt:       resumePrompt,
				}),
			})
			logger.Printf("%s scheduled wakeup in %s — sleeping before resuming session %s", ag.Name(), delay.Round(time.Second), sessionID)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				logger.Printf("cancelled during wakeup sleep — not resuming")
				runErr = ctx.Err()
			}
			if runErr != nil {
				break
			}

			req = baseReq
			req.Prompt = resumePrompt
			req.ResumeSessionID = sessionID
			res, runErr = ag.Run(agentCtx, req)
			if res.SessionID != "" {
				sessionID = res.SessionID
			}
			announceSession()
			emitTurn(res.FinalMessage)
			if res.FinalMessage != "" {
				finalMsg = res.FinalMessage
			}
		}

		if sessionID != "" {
			if err := st.SetSessionID(job.Name, sessionID); err != nil {
				logger.Printf("WARNING: state update failed: %v", err)
			}
		}
		if runErr != nil {
			logger.Printf("%s failed: %v", ag.Name(), runErr)
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
	// Persist the agent's final assistant message to a sidecar file alongside
	// the log so post_exec scripts and downstream integrations can read it
	// without reparsing the raw log stream. Empty when the run produced no
	// text (e.g. ended on a tool call) — surface that, don't fabricate.
	if finalMsg != "" {
		replyPath = strings.TrimSuffix(logPath, ".log") + ".reply.txt"
		if err := os.WriteFile(replyPath, []byte(finalMsg), 0600); err != nil {
			logger.Printf("WARNING: failed to write reply file: %v", err)
			replyPath = ""
		}
	}

	// Post-exec runs in runDir (worktree when available) so it can inspect or
	// commit whatever the agent produced. Falls back to job.Folder if no
	// worktree.
	if len(job.PostExec) > 0 {
		logger.Println("--- post_exec ---")
	}
	env := []string{
		"BIGBAND_STATUS=" + string(status),
		"BIGBAND_LOG=" + logPath,
		"BIGBAND_JOB=" + job.Name,
		"BIGBAND_WORKTREE=" + wtPath,
		"BIGBAND_REPLY_FILE=" + replyPath,
		"BIGBAND_SESSION_ID=" + sessionID,
	}
	for _, cmd := range job.PostExec {
		if err := runShellWithEnv(ctx, cfg, cmd, runDir, w, jobTimeout, env); err != nil {
			logger.Printf("post_exec error: %v", err)
		}
	}

	// Worktree cleanup.
	if wtPath != "" {
		if job.ShouldKeepWorktree() {
			logger.Printf("keeping worktree %s", wtPath)
		} else {
			if err := worktree.Remove(repoRoot, wtPath); err != nil {
				logger.Printf("WARNING: failed to remove worktree %s: %v", wtPath, err)
			} else {
				logger.Printf("removed worktree %s", wtPath)
				_ = st.SetWorktreePath(job.Name, "")
			}
		}
	}

	dur := time.Since(started)
	logger.Printf("=== END status=%s duration=%s", status, dur.Round(time.Second))

	if err := st.SetDone(job.Name, status, dur, logPath, replyPath); err != nil {
		logger.Printf("WARNING: state update failed: %v", err)
	}

	pub.Publish(events.Envelope{
		Type:        events.TypeJobRunCompleted,
		RunID:       runID,
		JobName:     job.Name,
		TriggeredBy: job.TriggeredBy,
		Data: events.MustData(events.JobRunCompletedData{
			Status:       string(status),
			FinalMessage: finalMsg,
			LogPath:      logPath,
			ReplyFile:    replyPath,
			SessionID:    sessionID,
			Folder:       job.Folder,
			WorktreePath: wtPath,
			DurationMS:   dur.Milliseconds(),
		}),
	})

	trimLogs(job.Name, cfg.Defaults.RetainLogs)
}

// resolveRunDir creates a git worktree for the job and returns:
//   - runDir: where pre_exec / claude / post_exec should run
//   - repoRoot: the repo root (needed for later worktree removal)
//   - wtPath: the worktree path (empty if no worktree was created)
//
// On any failure it falls back gracefully to job.Folder.
func resolveRunDir(job *config.Job, w io.Writer, logger *log.Logger, st *state.State) (runDir, repoRoot, wtPath string) {
	runDir = job.Folder
	if !job.ShouldUseWorktree() {
		logger.Printf("NOTE: worktree disabled — running in original folder %s", job.Folder)
		return
	}
	root, err := worktree.RepoRoot(job.Folder)
	if err != nil {
		logger.Printf("NOTE: skipping worktree — %v", err)
		return
	}
	repoRoot = root

	wt := worktree.DefaultPath(repoRoot, job.Name)

	// With reuse_worktree, skip re-creation if the worktree already exists so
	// that Claude picks up whatever state the previous run left behind.
	if job.ShouldReuseWorktree() {
		if _, err := os.Stat(wt); err == nil {
			dir := worktree.SubDir(repoRoot, wt, job.Folder)
			if _, err := os.Stat(dir); err == nil {
				wtPath = wt
				runDir = dir
				logger.Printf("reusing existing worktree %s", wtPath)
				if err := st.SetWorktreeKept(job.Name, wtPath, job.ShouldKeepWorktree()); err != nil {
					logger.Printf("WARNING: state update failed: %v", err)
				}
				return
			}
		}
		// Worktree missing (first run, or was manually removed) — fall through
		// to CreateOrReplace to build it fresh.
	}

	if err := worktree.CreateOrReplace(repoRoot, wt, job.Name, w); err != nil {
		logger.Printf("WARNING: worktree creation failed: %v — running in original folder", err)
		return
	}

	dir := worktree.SubDir(repoRoot, wt, job.Folder)
	if _, err := os.Stat(dir); err != nil {
		logger.Printf("WARNING: worktree subdir %s missing — running in original folder", dir)
		_ = worktree.Remove(repoRoot, wt)
		return
	}

	wtPath = wt
	runDir = dir
	logger.Printf("created worktree %s", wtPath)
	if err := st.SetWorktreeKept(job.Name, wtPath, job.ShouldKeepWorktree()); err != nil {
		logger.Printf("WARNING: state update failed: %v", err)
	}
	return
}

func openLog(jobName, ts string) (string, *os.File, error) {
	dir := paths.JobLogDir(jobName)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", nil, err
	}
	logPath := filepath.Join(dir, ts+".log")
	// 0600 explicitly: log files contain Claude's full output (prompts,
	// tool I/O, final messages) and could legitimately include secrets the
	// job was asked to handle. Defense-in-depth alongside the 0700 dir.
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return "", nil, err
	}
	// Update latest symlink atomically: create a tmp symlink then rename
	// over the old one so readers never see a missing target.
	latest := paths.JobLogLatest(jobName)
	tmp := latest + ".tmp"
	if err := os.Symlink(logPath, tmp); err == nil {
		_ = os.Rename(tmp, latest)
	}
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
	proc.KillProcessGroupOnCancel(c)
	if len(extra) > 0 {
		c.Env = append(os.Environ(), extra...)
	}
	return c.Run()
}

func trimLogs(jobName string, retain int) {
	if retain <= 0 {
		return
	}
	dir := paths.JobLogDir(jobName)
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
