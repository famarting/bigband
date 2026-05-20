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
//  3. Surface assistant text blocks live as they appear, and stop once a
//     system/turn_duration record marks the turn as complete.
//
// This provider deliberately does NOT support self-rescheduling via
// ScheduleWakeup — the JSONL stream does not preserve enough structured
// invocation metadata for that to be reliable, so Result.Wakeup is always nil.
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

// Name is the registry identifier for this provider. Tasks select it via the
// per-task agent field (or future config); main.go registers it by blank
// import.
const Name = "claude-pty"

// binary is the CLI executable name; expected to be on PATH.
const binary = "claude"

// settleAfterTurn is how long we wait after the turn_duration record before
// sending EOT to claude. Lets the file finish flushing any trailing records
// (ai-title, permission-mode) so a follow-up resume sees a stable file.
const settleAfterTurn = 1200 * time.Millisecond

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

	var (
		finalMessage string
		lastSeenText string
		turnDone     bool
	)
	tailErr := tailSession(ctx, jsonlPath, startOffset, pty.exited, func(rec *sessionRecord) bool {
		if text := assistantText(rec); text != "" {
			lastSeenText = text
			emitAssistantText(log, live, text)
		}
		if isTurnTerminal(rec) {
			turnDone = true
			return true
		}
		return false
	})

	if turnDone {
		finalMessage = lastSeenText
		bannerf(log, live, "claude-pty: turn complete")
		// Settle briefly so trailing records hit disk; then nudge claude out
		// of its interactive prompt with EOT before close() kills it.
		select {
		case <-time.After(settleAfterTurn):
		case <-ctx.Done():
		}
		pty.sendEOT()
		return agent.Result{
			SessionID:    sessionID,
			FinalMessage: finalMessage,
			Wakeup:       nil, // intentional: not supported in PTY mode
		}, nil
	}

	if tailErr == nil {
		tailErr = errors.New("session ended without turn_duration marker")
	}
	bannerf(log, live, "claude-pty: aborted: %v", tailErr)
	return agent.Result{
		SessionID:    sessionID,
		FinalMessage: lastSeenText,
		Wakeup:       nil,
	}, tailErr
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
