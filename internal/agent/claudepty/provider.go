// Package claudepty implements an alternate Claude Code provider that drives
// the CLI through a pseudo-terminal rather than --output-format stream-json.
//
// Approach (inspired by github.com/kcosr/claude-pty-wrapper):
//
//  1. Spawn `claude` inside a PTY in interactive mode, with the prompt as a
//     positional argument and a known session UUID (either pre-allocated for a
//     fresh run, or the caller's resume id).
//  2. Tail the durable session JSONL file at
//     ~/.claude/projects/<encoded-cwd>/<session-id>.jsonl
//     from its size at spawn time.
//  3. Surface assistant text blocks live as they appear, and treat
//     system/turn_duration as a SOFT completion: if Claude scheduled deferred
//     work in that same turn (Bash run_in_background:true, or ScheduleWakeup),
//     keep tailing past turn_duration so the bg processes — which die with
//     claude — get a chance to actually run, and any follow-up assistant
//     turn that lands during the wait gets captured. Bound by deferredMaxWait.
//
// ScheduleWakeup is honoured: its parameters are parsed from the tool_use
// block in the JSONL and surfaced as Result.Wakeup, so the runner re-invokes
// this provider with the resume session id after the requested delay (same
// contract as the stream-json claude provider).
//
// Imports for side-effects register the provider under the name "claude-pty".
package claudepty

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/famarting/bigband/internal/agent"
)

// Name is the registry identifier for this provider. Jobs select it via the
// per-job agent field (or future config); main.go registers it by blank
// import.
const Name = "claude-pty"

// binary is the CLI executable name; expected to be on PATH.
const binary = "claude"

// settleAfterTurn is how long we wait after the turn_duration record before
// sending EOT to claude. Lets the file finish flushing any trailing records
// (ai-title, permission-mode) so a follow-up resume sees a stable file.
const settleAfterTurn = 1200 * time.Millisecond

// deferredMaxWait caps how long we keep claude alive past its first
// turn_duration when Claude scheduled background work in that turn. Long
// enough to cover realistic Drive-polling / CI-watching loops; short enough
// to bound runaway jobs. Currently not user-configurable.
const deferredMaxWait = 30 * time.Minute

func init() {
	agent.Register(provider{})
}

type provider struct{}

func (provider) Name() string { return Name }

func (provider) Run(ctx context.Context, req agent.Request) (agent.Result, error) {
	log := req.LogWriter
	if log == nil {
		log = io.Discard
	}
	live := req.Live
	if live == nil {
		live = io.Discard
	}

	sessionID := req.ResumeSessionID
	if sessionID == "" {
		id, err := newSessionUUID()
		if err != nil {
			return agent.Result{}, fmt.Errorf("generate session id: %w", err)
		}
		sessionID = id
	}

	jsonlPath, err := sessionFilePath(req.WorkDir, sessionID)
	if err != nil {
		return agent.Result{}, err
	}

	startOffset, err := fileSize(jsonlPath)
	if err != nil {
		return agent.Result{}, fmt.Errorf("snapshot session file size: %w", err)
	}
	if req.ResumeSessionID != "" && startOffset == 0 {
		if _, statErr := os.Stat(jsonlPath); statErr != nil {
			return agent.Result{}, fmt.Errorf("resume session %s not found at %s", sessionID, jsonlPath)
		}
	}

	args, err := buildArgs(req, sessionID)
	if err != nil {
		return agent.Result{}, err
	}
	// claude shows an interactive workspace-trust dialog the first time it's
	// launched in a new cwd. We discard PTY output, so that dialog would hang
	// invisibly until the job deadline — pre-stamp trust in ~/.claude.json.
	if err := ensureProjectTrusted(req.WorkDir); err != nil {
		bannerf(log, live, "claude-pty: warn: could not pre-trust workdir: %v", err)
	}
	bannerf(log, live, "claude-pty: spawning %s %s", binary, strings.Join(args, " "))
	bannerf(log, live, "claude-pty: session=%s tailing=%s", sessionID, jsonlPath)

	// The PTY output is intentionally dropped — it's full of ANSI cursor
	// motion and TUI redraws that would only clutter the log. All structured
	// content reaches us via the durable JSONL file instead.
	pty, err := startPty(ctx, binary, req.WorkDir, args, io.Discard)
	if err != nil {
		return agent.Result{}, err
	}
	defer pty.close()

	state := newRunState(log, live)

	// Phase 1: tail until the first turn_duration. Visit returns true on that
	// record; everything else is captured into state.
	offset, tailErr := tailSession(ctx, jsonlPath, startOffset, pty.exited, state.visit)
	if !state.turnDone {
		if tailErr == nil {
			tailErr = errors.New("session ended without turn_duration marker")
		}
		bannerf(log, live, "claude-pty: aborted: %v", tailErr)
		return agent.Result{
			SessionID:    sessionID,
			FinalMessage: state.lastSeenText,
			Wakeup:       state.agentWakeup(),
		}, tailErr
	}

	// Phase 2: if Claude scheduled deferred work in this turn, keep tailing
	// past turn_duration so any background bash actually gets to run (those
	// processes are in claude's process group and would die at EOT). Any
	// follow-up assistant turns that land during the wait also get captured;
	// state.visit returns true again when pendingBG drains to zero. Otherwise
	// the deferCtx deadline ends the wait.
	if len(state.pendingBG) > 0 {
		bannerf(log, live, "claude-pty: %d background task(s) still running; deferring exit up to %s", len(state.pendingBG), deferredMaxWait)
		state.deferring = true
		state.turnDone = false
		deferCtx, cancel := context.WithTimeout(ctx, deferredMaxWait)
		_, _ = tailSession(deferCtx, jsonlPath, offset, pty.exited, state.visit)
		cancel()
		if len(state.pendingBG) == 0 {
			bannerf(log, live, "claude-pty: background work complete")
		} else {
			bannerf(log, live, "claude-pty: deferred-wait deadline reached; %d background task(s) still pending", len(state.pendingBG))
		}
	}

	if summary := state.turnSummary(); summary != "" {
		bannerf(log, live, "claude-pty: turn complete (%s)", summary)
	} else {
		bannerf(log, live, "claude-pty: turn complete")
	}
	// Settle briefly so trailing records hit disk; then nudge claude out of
	// its interactive prompt with EOT before close() kills it.
	select {
	case <-time.After(settleAfterTurn):
	case <-ctx.Done():
	}
	pty.sendEOT()
	return agent.Result{
		SessionID:    sessionID,
		FinalMessage: state.lastSeenText,
		Wakeup:       state.agentWakeup(),
	}, nil
}

// runState carries the per-Run state the tail callback mutates. Centralising
// this here keeps Run() readable and makes the same visit function safe to
// reuse across Phase 1 and Phase 2 (only the exit conditions differ).
type runState struct {
	log, live    io.Writer
	pendingBG    map[string]struct{}
	wakeup       *wakeupRequest
	lastSeenText string
	turnDone     bool
	// deferring is set once Phase 2 starts. In that mode visit returns true
	// when pendingBG drains to zero; turn_duration is informational only,
	// since a follow-up turn may still arrive.
	deferring bool
	// totalOutputTokens accumulates output_tokens across every assistant
	// message in the run so the turn-complete banner can report the
	// cumulative cost.
	totalOutputTokens int
	// lastCacheRead is the most recent cache_read_input_tokens observation,
	// which approximates the working context size as of the latest turn.
	lastCacheRead int
	// turnDurationMs is the duration reported on the most recent
	// system/turn_duration record, in milliseconds.
	turnDurationMs int64
	// prevTodos holds the content→status snapshot from the last TodoWrite,
	// so the next TodoWrite can render only the changes.
	prevTodos map[string]string
}

func newRunState(log, live io.Writer) *runState {
	return &runState{
		log:       log,
		live:      live,
		pendingBG: map[string]struct{}{},
	}
}

// visit is the tail callback. It returns true only when the caller should
// stop tailing: at the first turn_duration in Phase 1, or when all background
// tasks have drained in Phase 2.
func (s *runState) visit(rec *sessionRecord) bool {
	for _, t := range assistantThinkingTexts(rec) {
		emitThinking(s.log, s.live, t)
	}
	if text := assistantText(rec); text != "" {
		s.lastSeenText = text
		emitAssistantText(s.log, s.live, text)
	}
	for _, b := range assistantToolUses(rec) {
		switch b.Name {
		case "ScheduleWakeup":
			if w := parseWakeup(b.Input); w != nil {
				s.wakeup = w
			}
		case "Bash":
			if b.ID != "" && isBackgroundBashInput(b.Input) {
				s.pendingBG[b.ID] = struct{}{}
			}
		case "TodoWrite":
			next := todoStates(b.Input)
			emitToolUse(s.log, s.live, b)
			emitTodoDelta(s.log, s.live, s.prevTodos, next)
			s.prevTodos = next
			continue
		}
		emitToolUse(s.log, s.live, b)
	}
	for _, r := range toolResults(rec) {
		emitToolResult(s.log, s.live, r)
	}
	if u := assistantUsage(rec); u != nil {
		s.totalOutputTokens += u.OutputTokens
		if u.CacheReadInputTokens > 0 {
			s.lastCacheRead = u.CacheReadInputTokens
		}
	}
	if id := bgCompletionToolUseID(rec); id != "" {
		delete(s.pendingBG, id)
	}
	if isTurnTerminal(rec) {
		s.turnDone = true
		if rec.DurationMs > 0 {
			s.turnDurationMs = rec.DurationMs
		}
		if !s.deferring {
			return true
		}
	}
	if s.deferring && len(s.pendingBG) == 0 {
		return true
	}
	return false
}

// turnSummary builds the "tokens / context / duration" tail appended to the
// turn-complete banner. Returns "" when no usage data was observed (e.g. on
// abort paths before any assistant message arrived).
func (s *runState) turnSummary() string {
	if s.totalOutputTokens == 0 && s.lastCacheRead == 0 && s.turnDurationMs == 0 {
		return ""
	}
	parts := make([]string, 0, 3)
	if s.totalOutputTokens > 0 {
		parts = append(parts, fmt.Sprintf("%s out", humanTokens(s.totalOutputTokens)))
	}
	if s.lastCacheRead > 0 {
		parts = append(parts, fmt.Sprintf("%s ctx", humanTokens(s.lastCacheRead)))
	}
	if s.turnDurationMs > 0 {
		parts = append(parts, time.Duration(s.turnDurationMs*int64(time.Millisecond)).Round(100*time.Millisecond).String())
	}
	return strings.Join(parts, " / ")
}

// humanTokens formats a token count compactly: 1234 → "1.2k", 71270 → "71k".
func humanTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 10_000:
		return fmt.Sprintf("%dk", n/1000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// agentWakeup converts the captured wakeupRequest (delaySeconds) into the
// runner-facing agent.WakeupRequest (time.Duration). Returns nil when Claude
// did not call ScheduleWakeup.
func (s *runState) agentWakeup() *agent.WakeupRequest {
	if s.wakeup == nil {
		return nil
	}
	return &agent.WakeupRequest{
		Delay:  time.Duration(s.wakeup.DelaySeconds) * time.Second,
		Prompt: s.wakeup.Prompt,
	}
}

// buildArgs constructs the claude argv for one PTY run. We pass the prompt as
// a positional, which makes claude run it in "auto-submit" interactive mode:
// the input arrives via the command line, claude processes it, and then it
// would normally idle at its prompt — we wake up on turn_duration via the
// JSONL tail and send EOT to make it exit.
func buildArgs(req agent.Request, sessionID string) ([]string, error) {
	var args []string
	if req.ResumeSessionID != "" {
		args = append(args, "--resume", req.ResumeSessionID)
	} else {
		args = append(args, "--session-id", sessionID)
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.Effort != "" {
		args = append(args, "--effort", req.Effort)
	}
	args = append(args, slices.Clone(req.ExtraFlags)...)
	args = append(args, "--", req.Prompt)
	return args, nil
}

// emitAssistantText writes one assistant text block to both the log file and
// the live writer. Live output is bracketed in a cyan dot for parity with the
// claude provider's rendering, but without ANSI color codes in the log file.
func emitAssistantText(log, live io.Writer, text string) {
	text = strings.TrimRight(text, "\n")
	fmt.Fprintf(log, "[%s] ● %s\n", time.Now().Format("15:04:05"), text)
	fmt.Fprintf(live, "\x1b[1m● \x1b[0m%s\n", text)
}

// bannerf writes a status line to both the log and the live stream, plain
// for the log and dimmed for live. Used for spawn / completion notices.
func bannerf(log, live io.Writer, format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	fmt.Fprintf(log, "[%s] %s\n", time.Now().Format("15:04:05"), line)
	fmt.Fprintf(live, "\x1b[2m%s\x1b[0m\n", line)
}

func (provider) ResumeInteractive(_ context.Context, sessionID, workDir string) error {
	var args []string
	if sessionID != "" {
		args = []string{"--resume", sessionID}
	} else {
		args = []string{"--continue"}
	}
	bin, err := exec.LookPath(binary)
	if err != nil {
		return fmt.Errorf("claude not found in PATH: %w", err)
	}
	if err := os.Chdir(workDir); err != nil {
		return fmt.Errorf("cannot chdir to %s: %w", workDir, err)
	}
	return syscall.Exec(bin, append([]string{binary}, args...), os.Environ())
}
