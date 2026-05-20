// Package claude implements the Claude Code provider for bigband's agent
// abstraction. Importing this package for side-effects (blank import)
// registers the provider under the name "claude" in the agent registry.
package claude

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"syscall"
	"time"

	"github.com/famarting/bigband/internal/agent"
	"github.com/famarting/bigband/internal/proc"
)

// Name is the registry identifier for this provider.
const Name = "claude"

// binary is the CLI executable name; expected to be on PATH.
const binary = "claude"

// coreFlags are non-negotiable flags bigband needs for output parsing.
// Always prepended; user-supplied flags follow.
var coreFlags = []string{"-p", "--output-format", "stream-json", "--verbose", "--include-partial-messages"}

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

	c := exec.CommandContext(ctx, binary, buildArgs(req)...)
	c.Dir = req.WorkDir
	// Raw NDJSON is discarded once parsed: plain rendering goes to the log,
	// colorized rendering to live (when live is a TTY).
	sw := newStreamWriter(io.Discard, log, live, isTerminal(live))
	c.Stdout = sw
	c.Stderr = io.MultiWriter(log, live)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	proc.KillProcessGroupOnCancel(c)

	runErr := c.Run()

	_, sessionID := sw.getResult()
	var wakeup *agent.WakeupRequest
	if w := sw.getWakeup(); w != nil {
		wakeup = &agent.WakeupRequest{
			Delay:  time.Duration(w.DelaySeconds) * time.Second,
			Prompt: w.Prompt,
		}
	}
	return agent.Result{
		SessionID:    sessionID,
		FinalMessage: sw.getFinalMessage(),
		Wakeup:       wakeup,
	}, runErr
}

func buildArgs(req agent.Request) []string {
	args := slices.Clone(coreFlags)
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.Effort != "" {
		args = append(args, "--effort", req.Effort)
	}
	args = append(args, req.ExtraFlags...)
	if req.ResumeSessionID != "" {
		args = append(args, "--resume", req.ResumeSessionID)
	}
	args = append(args, req.Prompt)
	return args
}

func (provider) ResumeInteractive(_ context.Context, sessionID, workDir string) error {
	var args []string
	if sessionID != "" {
		args = []string{"--resume", sessionID}
	} else {
		// No recorded session ID — fall back to claude's "continue most
		// recent session in this directory" mode.
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
