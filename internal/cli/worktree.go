package cli

import (
	"fmt"

	"github.com/famarting/bigband/internal/config"
	"github.com/famarting/bigband/internal/paths"
	"github.com/famarting/bigband/internal/state"
	"github.com/famarting/bigband/internal/worktree"
	"github.com/spf13/cobra"
)

func NewWorktreeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "worktree",
		Short: "Manage git worktrees created by job runs",
	}
	cmd.AddCommand(
		newWorktreeMoveCmd(),
		newWorktreeRmCmd(),
	)
	return cmd
}

func newWorktreeMoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "move <job> <dest>",
		Short:             "Move a job's worktree to a new location",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeJobNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			jobName, dest := args[0], args[1]

			st, err := state.Load()
			if err != nil {
				return err
			}
			js := st.Get(jobName)
			if js.WorktreePath == "" {
				return fmt.Errorf("no tracked worktree for job %q", jobName)
			}

			folder := js.Folder
			if cfg, err := config.Load(paths.Config()); err == nil {
				if j := cfg.JobByName(jobName); j != nil {
					folder = j.Folder
				}
			}
			if folder == "" {
				return fmt.Errorf("job %q: no folder recorded — cannot resolve repo root", jobName)
			}

			repoRoot, err := worktree.RepoRoot(folder)
			if err != nil {
				return err
			}

			if err := worktree.Move(repoRoot, js.WorktreePath, dest); err != nil {
				return err
			}

			if err := st.SetWorktreePath(jobName, dest); err != nil {
				return fmt.Errorf("moved worktree but failed to update state: %w", err)
			}

			fmt.Printf("moved %s → %s\n", js.WorktreePath, dest)
			return nil
		},
	}
}

func newWorktreeRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "rm <job>",
		Short:             "Remove a job's tracked worktree",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeJobNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			jobName := args[0]

			st, err := state.Load()
			if err != nil {
				return err
			}
			js := st.Get(jobName)
			if js.WorktreePath == "" {
				return fmt.Errorf("no tracked worktree for job %q", jobName)
			}

			folder := js.Folder
			if cfg, err := config.Load(paths.Config()); err == nil {
				if j := cfg.JobByName(jobName); j != nil {
					folder = j.Folder
				}
			}
			if folder == "" {
				return fmt.Errorf("job %q: no folder recorded — cannot resolve repo root", jobName)
			}

			repoRoot, err := worktree.RepoRoot(folder)
			if err != nil {
				return err
			}

			if err := worktree.Remove(repoRoot, js.WorktreePath); err != nil {
				return err
			}

			if err := st.SetWorktreePath(jobName, ""); err != nil {
				return fmt.Errorf("removed worktree but failed to update state: %w", err)
			}

			fmt.Printf("removed %s\n", js.WorktreePath)
			return nil
		},
	}
}
