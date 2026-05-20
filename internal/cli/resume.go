package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/famarting/bigband/internal/agent"
	"github.com/famarting/bigband/internal/config"
	"github.com/famarting/bigband/internal/paths"
	"github.com/famarting/bigband/internal/state"
	"github.com/spf13/cobra"
)

func NewResumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "resume <task>",
		Short:             "Resume the agent session from a task's last run in its worktree",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeTaskNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			return resumeTask(cmd.Context(), args[0])
		},
	}
}

func resumeTask(ctx context.Context, name string) error {
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

	// Resolve the agent the same way the runner does: prefer task-level, then
	// defaults.agent, then DefaultAgent. Falling back to DefaultAgent on a
	// config error keeps `resume` usable when the config has drifted.
	agentName := config.DefaultAgent
	if cfg, err := config.Load(paths.Config()); err == nil {
		if t := cfg.TaskByName(name); t != nil {
			agentName = cfg.EffectiveAgent(t)
		} else if cfg.Defaults.Agent != "" {
			agentName = cfg.Defaults.Agent
		}
	}

	if ts.SessionID != "" {
		fmt.Printf("Resuming session %s in %s\n", ts.SessionID, ts.WorktreePath)
	} else {
		fmt.Printf("No session ID recorded; continuing most recent session in %s\n", ts.WorktreePath)
	}

	ag, err := agent.Get(agentName)
	if err != nil {
		return err
	}
	// ResumeInteractive replaces the current process; on success it does not
	// return.
	return ag.ResumeInteractive(ctx, ts.SessionID, ts.WorktreePath)
}
