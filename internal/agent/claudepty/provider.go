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
//     work in that same turn (Bash or Agent with run_in_background:true, or
//     ScheduleWakeup), keep tailing past turn_duration so the bg processes —
//     which die with claude — get a chance to actually run. This happens in
//     two sub-phases: Phase 2a waits for the background work to drain, then
//     Phase 2b waits for the follow-up "synthesis" turn the completion
//     notifications trigger — the turn in which claude reads the results and
//     composes its real final message. Capturing that turn (not the pre-drain
//     lead-in) is what makes FinalMessage correct. Phase 2 is bounded by
//     progress, not by a fixed budget: each wait gets a fresh
//     deferredStallWindow that any new session record resets, so an agent that
//     keeps working holds the session open, while an idle one still times out.
//     Phase 2b additionally gives up after a shorter quiet window
//     (synthesisQuietWindow) when no follow-up turn materialises. The job
//     timeout the runner puts on ctx is the hard bound throughout.
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

// deferredStallWindow bounds how long we keep claude alive with *no new
// session records* while background work is still outstanding. It is an
// inactivity window, not a total budget: every record resets it, so an agent
// that keeps making progress — draining one batch, reading the results,
// dispatching the next — holds the session open for as long as it keeps
// working, and only a genuinely idle session (a background task that never
// reports, or a hung claude) times out. A single absolute budget instead
// killed multi-stage runs mid-workflow, because the early productive phases
// spent the budget the final wait needed.
//
// Sized to cover the quiet stretch of a realistic CI-watching poller, which
// writes nothing to the session between arming the wait and its completion
// notification. The job's own timeout is the hard bound — the runner puts it
// on ctx, so Phase 2 can never outlive the run. Currently not
// user-configurable; a var only so tests can shrink it.
var deferredStallWindow = 60 * time.Minute

// synthesisQuietWindow bounds how long Phase 2b waits with no new session
// records before concluding that no follow-up "synthesis" turn is coming.
// After background work drains, the completion notifications normally prompt
// claude to consume the results and compose its real final message within a
// few seconds; if nothing is written for this long we assume the background
// work was fire-and-forget (no follow-up turn) and stop. Progress resets the
// window, so a genuinely long synthesis turn is still captured in full (up to
// the job deadline on ctx).
const synthesisQuietWindow = 45 * time.Second

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
			FinalMessage: state.finalMessage(),
			Wakeup:       state.agentWakeup(),
		}, tailErr
	}

	// Phase 2: if Claude scheduled deferred work in this turn, keep tailing
	// past turn_duration so any background task actually gets to run (those
	// processes are in claude's process group and would die at EOT). This runs
	// as repeated drain→synthesis cycles: a synthesis turn may itself dispatch
	// a fresh round of background work, and we must not exit while any is still
	// outstanding. Every cycle gets its own deferredStallWindow rather than
	// drawing on one shared budget — see the constant — so the bound is the
	// job deadline on ctx plus a full window of session silence.
	if len(state.pendingBG) > 0 {
		state.deferring = true
		for len(state.pendingBG) > 0 {
			// Stop deferring when the parent (job-level) context is cancelled
			// or hits its own deadline, or claude has died. In both cases
			// tailSession returns instantly, so re-entering the loop would only
			// spin; awaitDrain checks the same causes, and this guard covers
			// re-entry after a synthesis turn.
			if ctx.Err() != nil || pty.exited() {
				bannerf(log, live, "claude-pty: deferred wait ended; %d background task(s) still pending", len(state.pendingBG))
				break
			}
			bannerf(log, live, "claude-pty: %d background task(s) still running; deferring exit while the session stays active (idle cap %s)", len(state.pendingBG), deferredStallWindow)
			// Phase 2a: keep claude alive until the current batch drains.
			state.awaitingSynthesis = false
			state.turnDone = false
			var drainErr error
			offset, drainErr = state.awaitDrain(ctx, jsonlPath, offset, pty.exited)
			if len(state.pendingBG) > 0 {
				bannerf(log, live, "claude-pty: deferred wait cannot proceed (%v); %d background task(s) still pending", drainErr, len(state.pendingBG))
				break
			}
			bannerf(log, live, "claude-pty: background work complete")
			// Phase 2b: draining does NOT mean claude is done — the completion
			// notifications prompt it to read the results and compose its real
			// final message in a follow-up turn. Returning now would capture
			// the pre-drain lead-in ("...let me read the results") instead of
			// the synthesis. Wait for that follow-up turn's turn_duration,
			// giving up only after synthesisQuietWindow of no new records so
			// fire-and-forget background work (no follow-up turn) doesn't idle
			// out the rest of the budget. If that synthesis turn dispatched a
			// new round of background work, the outer loop re-enters Phase 2a.
			offset = state.awaitSynthesis(ctx, jsonlPath, offset, pty.exited)
			if state.turnDone {
				bannerf(log, live, "claude-pty: synthesis turn captured")
			} else {
				bannerf(log, live, "claude-pty: no synthesis turn within %s; using last completed turn", synthesisQuietWindow)
			}
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
		FinalMessage: state.finalMessage(),
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
	// deferring is set once Phase 2 starts. In that mode a turn_duration no
	// longer ends the tail on its own: Phase 2a keeps tailing until pendingBG
	// drains to zero, and Phase 2b (awaitingSynthesis) keeps tailing until the
	// follow-up synthesis turn produces its own turn_duration.
	deferring bool
	// awaitingSynthesis is set for Phase 2b, after background work has drained.
	// The completion notifications trigger a follow-up turn in which claude
	// composes its real final message; in this mode visit returns true on the
	// next turn_duration (the synthesis turn's terminal) rather than on the
	// now-empty pendingBG set.
	awaitingSynthesis bool
	// finalText is lastSeenText snapshotted at each turn_duration, i.e. the
	// last assistant text of the most recently *completed* turn. FinalMessage
	// prefers this over lastSeenText so a mid-turn lead-in ("...let me read the
	// results") that never reached a turn boundary can't win.
	finalText string
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
			// Background bash (run_in_background:true) returns immediately and
			// reports completion later as a task-notification attachment
			// (drained by bgCompletionToolUseID below). Background Agent
			// dispatches look synchronous in the input and are detected from
			// their tool_result instead — see the toolResults loop below.
			if b.ID != "" && isBackgroundToolInput(b.Input) {
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
		// A background (async) Agent dispatch is only detectable here: its
		// tool_use input is indistinguishable from a synchronous call, but the
		// result acknowledges an async launch. Track it so Phase 2 waits for
		// the subagent's eventual task-notification rather than exiting on the
		// pre-result turn.
		if r.ToolUseID != "" && isAsyncAgentLaunch(toolResultText(&r)) {
			s.pendingBG[r.ToolUseID] = struct{}{}
		}
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
		// Snapshot the text of this now-completed turn; FinalMessage prefers it.
		s.finalText = s.lastSeenText
		if rec.DurationMs > 0 {
			s.turnDurationMs = rec.DurationMs
		}
		if !s.deferring {
			return true // Phase 1: first turn complete.
		}
		if s.awaitingSynthesis {
			return true // Phase 2b: the post-drain synthesis turn finished.
		}
	}
	// Phase 2a: stop as soon as background work drains so the caller can start
	// Phase 2b. Not while awaitingSynthesis — that phase exits on turn_duration.
	if s.deferring && !s.awaitingSynthesis && len(s.pendingBG) == 0 {
		return true
	}
	return false
}

// awaitDrain runs Phase 2a: keep claude alive until the current batch of
// background tasks drains. visit returns true the moment pendingBG hits zero.
//
// Each tail attempt is bounded by its own deferredStallWindow, and any new
// session bytes buy a fresh one. That is what keeps a multi-stage run alive: a
// job that grooms a backlog, opens a PR and only then settles in to watch CI is
// still making progress the whole time, so the long final wait starts with a
// full window rather than with whatever a single shared budget had left.
//
// It returns the offset reached and a nil error once pendingBG is empty. On any
// other stop — a full window of silence, the job deadline, claude exiting, an
// unreadable session file — it returns a non-nil error saying why, with
// pendingBG still non-empty.
func (s *runState) awaitDrain(ctx context.Context, path string, offset int64, childDead func() bool) (int64, error) {
	for {
		stallCtx, cancel := context.WithTimeout(ctx, deferredStallWindow)
		newOffset, err := tailSession(stallCtx, path, offset, childDead, s.visit)
		cancel()
		progressed := newOffset > offset
		offset = newOffset
		switch {
		case len(s.pendingBG) == 0:
			return offset, nil
		case ctx.Err() != nil:
			return offset, ctx.Err()
		case childDead != nil && childDead():
			return offset, errors.New("claude exited with background work outstanding")
		case !progressed:
			// A full window with nothing written, or a read error that made no
			// progress (e.g. the session file became permanently unreadable):
			// either way another attempt would only spin.
			if err == nil || errors.Is(err, context.DeadlineExceeded) {
				return offset, fmt.Errorf("no session activity for %s", deferredStallWindow)
			}
			return offset, err
		}
	}
}

// awaitSynthesis runs Phase 2b: after background work has drained, tail for the
// follow-up turn in which claude composes its real final message. It returns
// the byte offset reached. It stops when that turn produces a turn_duration
// (setting turnDone via visit), or after synthesisQuietWindow elapses with no
// new bytes written — the signal that no follow-up turn is coming. Any new
// bytes reset the quiet window, so a long synthesis turn is captured in full,
// bounded only by the job deadline on ctx.
func (s *runState) awaitSynthesis(ctx context.Context, path string, offset int64, childDead func() bool) int64 {
	s.awaitingSynthesis = true
	s.turnDone = false
	for {
		quietCtx, cancelQuiet := context.WithTimeout(ctx, synthesisQuietWindow)
		newOffset, _ := tailSession(quietCtx, path, offset, childDead, s.visit)
		cancelQuiet()
		progressed := newOffset > offset
		offset = newOffset
		// Synthesis turn completed, job context expired, or a full quiet window
		// passed with no new bytes — in every case, stop.
		if s.turnDone || ctx.Err() != nil || !progressed {
			return offset
		}
	}
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

// finalMessage returns the message to surface as Result.FinalMessage. It
// prefers finalText — the last assistant text of the most recently completed
// turn — falling back to lastSeenText only when no turn ever terminated (e.g.
// the abort path, where finalText is still empty).
func (s *runState) finalMessage() string {
	if s.finalText != "" {
		return s.finalText
	}
	return s.lastSeenText
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
