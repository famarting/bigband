// Package agent defines the interface implemented by coding-agent providers
// (Claude Code, Codex, OpenCode, …) and the registry the runner uses to look
// them up by name.
//
// Providers wrap a CLI binary: they construct argv from the structured
// Request, spawn the subprocess, parse its provider-specific output stream,
// and return a structured Result. The runner is provider-agnostic — it only
// consumes Result fields and, when Wakeup is non-nil, re-invokes Run.
package agent

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

// Agent is one coding-agent provider.
type Agent interface {
	// Name returns the provider's short identifier ("claude", "codex", …).
	Name() string

	// Run executes one turn against the agent and returns its result.
	// Cancelling ctx terminates the underlying subprocess. The runner sets
	// any overall deadline on ctx before calling; providers don't need to
	// re-apply a timeout.
	//
	// Output is split:
	//   - req.LogWriter receives the raw subprocess stream (for the .log file)
	//   - req.Live receives a pretty-rendered version (for terminals / sinks)
	// Both may be nil; providers must treat nil as io.Discard.
	Run(ctx context.Context, req Request) (Result, error)

	// ResumeInteractive replaces the current process with an interactive
	// agent session for sessionID in workDir. Implementations typically use
	// syscall.Exec so the user gets a real TTY. On success this call does
	// not return; a non-nil error means the exec failed.
	ResumeInteractive(ctx context.Context, sessionID, workDir string) error
}

// Request describes one invocation of an agent.
type Request struct {
	// Prompt is the user instruction for this turn.
	Prompt string
	// WorkDir is the directory the agent runs in (typically a worktree).
	WorkDir string
	// ResumeSessionID, when non-empty, asks the agent to resume the given
	// session. The value is opaque to bigband — providers interpret it.
	ResumeSessionID string
	// Model selects the underlying model. Free-form; the provider validates.
	// Empty means use the provider's default.
	Model string
	// Effort selects a thinking/reasoning budget. Free-form per provider.
	// Empty means use the provider's default.
	Effort string
	// ExtraFlags is appended to the provider's constructed argv verbatim.
	// Use sparingly — provider-specific syntax.
	ExtraFlags []string
	// Env is added to the agent subprocess's environment, on top of the
	// daemon's own. Values here win. Nil means inherit unchanged.
	Env map[string]string
	// LogWriter receives the raw subprocess stream.
	LogWriter io.Writer
	// Live receives a pretty-rendered version of the run.
	Live io.Writer
}

// Result captures the structured outcome of one agent invocation.
type Result struct {
	// SessionID identifies the agent session. Opaque to bigband; round-tripped
	// back into Request.ResumeSessionID on follow-up runs. Empty when the
	// provider has no concept of a session or did not start one.
	SessionID string
	// FinalMessage is the agent's last assistant text. Empty when the run
	// ended without producing user-visible text (e.g. on a tool call).
	FinalMessage string
	// Wakeup, when non-nil, asks the runner to re-invoke this agent after
	// Delay with Prompt. Used by Claude's ScheduleWakeup tool; providers
	// that don't support self-rescheduling return nil.
	Wakeup *WakeupRequest
}

// WakeupRequest asks the runner to re-invoke the agent after a delay.
type WakeupRequest struct {
	Delay  time.Duration
	Prompt string
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Agent{}
)

// Register makes a registered under its Name(). Intended for package init()
// functions. Panics on duplicate registration so misconfiguration fails loudly
// at startup.
func Register(a Agent) {
	registryMu.Lock()
	defer registryMu.Unlock()
	name := a.Name()
	if _, dup := registry[name]; dup {
		panic("agent: duplicate registration for " + name)
	}
	registry[name] = a
}

// Get returns the registered provider or an error if name is unknown.
func Get(name string) (Agent, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	a, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("agent: no provider registered as %q", name)
	}
	return a, nil
}
