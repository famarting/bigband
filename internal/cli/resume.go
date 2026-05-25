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
		Use:               "resume <job>",
		Short:             "Resume the agent session from a job's last run (in its worktree, or its folder if it ran in-place)",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeJobNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			return resumeJob(cmd.Context(), args[0])
		},
	}
}

func resumeJob(ctx context.Context, name string) error {
	st, err := state.Load()
	if err != nil {
		return err
	}
	js := st.Get(name)

	// Prefer the worktree when one was tracked. Fall back to the folder the run
	// executed in (recorded as js.Folder) so jobs that ran in-place — either
	// because use_worktree was false or because the worktree was cleaned up
	// after the run — remain resumable from their original directory.
	runDir := js.WorktreePath
	fromWorktree := runDir != ""
	if runDir == "" {
		runDir = js.Folder
	}
	if runDir == "" {
		return fmt.Errorf("job %q has no tracked run directory — run the job first", name)
	}
	if _, err := os.Stat(runDir); err != nil {
		if fromWorktree {
			return fmt.Errorf("worktree %s no longer exists on disk — use 'bigband worktree rm %s' to clear the stale reference", runDir, name)
		}
		return fmt.Errorf("run directory %s no longer exists on disk", runDir)
	}

	// Resolve the agent the same way the runner does: prefer job-level, then
	// defaults.agent, then DefaultAgent. Falling back to DefaultAgent on a
	// config error keeps `resume` usable when the config has drifted.
	agentName := config.DefaultAgent
	if cfg, err := config.Load(paths.Config()); err == nil {
		if j := cfg.JobByName(name); j != nil {
			agentName = cfg.EffectiveAgent(j)
		} else if cfg.Defaults.Agent != "" {
			agentName = cfg.Defaults.Agent
		}
	}

	if js.SessionID != "" {
		fmt.Printf("Resuming session %s in %s\n", js.SessionID, runDir)
	} else {
		fmt.Printf("No session ID recorded; continuing most recent session in %s\n", runDir)
	}

	ag, err := agent.Get(agentName)
	if err != nil {
		return err
	}
	// ResumeInteractive replaces the current process; on success it does not
	// return.
	return ag.ResumeInteractive(ctx, js.SessionID, runDir)
}
