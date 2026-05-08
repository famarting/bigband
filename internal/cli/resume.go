package cli

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/famarting/bigband/internal/config"
	"github.com/famarting/bigband/internal/paths"
	"github.com/famarting/bigband/internal/state"
	"github.com/spf13/cobra"
)

func NewResumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "resume <task>",
		Short:             "Resume the Claude session from a task's last run in its worktree",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeTaskNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			return resumeTask(args[0])
		},
	}
}

func resumeTask(name string) error {
	st, err := state.Load()
	if err != nil {
		return err
	}
	ts := st.Get(name)

	if ts.WorktreePath == "" {
		return fmt.Errorf("task %q has no tracked worktree — run the task first (or check that keep_worktree is not false)", name)
	}
	if _, err := os.Stat(ts.WorktreePath); err != nil {
		return fmt.Errorf("worktree %s no longer exists on disk — use 'bigband worktree rm %s' to clear the stale reference", ts.WorktreePath, name)
	}

	cfg, err := config.Load(paths.Config())
	if err != nil {
		return err
	}
	if cfg.TaskByName(name) == nil {
		return fmt.Errorf("task %q not found in config", name)
	}

	var claudeArgs []string
	if ts.SessionID != "" {
		claudeArgs = []string{"--resume", ts.SessionID}
		fmt.Printf("Resuming session %s in %s\n", ts.SessionID, ts.WorktreePath)
	} else {
		claudeArgs = []string{"--continue"}
		fmt.Printf("No session ID recorded; continuing most recent session in %s\n", ts.WorktreePath)
	}

	claude, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("claude not found in PATH: %w", err)
	}

	if err := os.Chdir(ts.WorktreePath); err != nil {
		return fmt.Errorf("cannot chdir to worktree: %w", err)
	}

	// Replace the current process with an interactive Claude session.
	return syscall.Exec(claude, append([]string{"claude"}, claudeArgs...), os.Environ())
}
