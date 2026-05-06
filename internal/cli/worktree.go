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
		Short: "Manage git worktrees created by task runs",
	}
	cmd.AddCommand(
		newWorktreeMoveCmd(),
		newWorktreeRmCmd(),
	)
	return cmd
}

func newWorktreeMoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "move <task> <dest>",
		Short:             "Move a task's worktree to a new location",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeTaskNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			taskName, dest := args[0], args[1]

			st, err := state.Load()
			if err != nil {
				return err
			}
			ts := st.Get(taskName)
			if ts.WorktreePath == "" {
				return fmt.Errorf("no tracked worktree for task %q", taskName)
			}

			cfg, err := config.Load(paths.Config())
			if err != nil {
				return err
			}
			t := cfg.TaskByName(taskName)
			if t == nil {
				return fmt.Errorf("task %q not found", taskName)
			}

			repoRoot, err := worktree.RepoRoot(t.Folder)
			if err != nil {
				return err
			}

			if err := worktree.Move(repoRoot, ts.WorktreePath, dest); err != nil {
				return err
			}

			if err := st.SetWorktreePath(taskName, dest); err != nil {
				return fmt.Errorf("moved worktree but failed to update state: %w", err)
			}

			fmt.Printf("moved %s → %s\n", ts.WorktreePath, dest)
			return nil
		},
	}
}

func newWorktreeRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "rm <task>",
		Short:             "Remove a task's tracked worktree",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeTaskNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			taskName := args[0]

			st, err := state.Load()
			if err != nil {
				return err
			}
			ts := st.Get(taskName)
			if ts.WorktreePath == "" {
				return fmt.Errorf("no tracked worktree for task %q", taskName)
			}

			cfg, err := config.Load(paths.Config())
			if err != nil {
				return err
			}
			t := cfg.TaskByName(taskName)
			if t == nil {
				return fmt.Errorf("task %q not found", taskName)
			}

			repoRoot, err := worktree.RepoRoot(t.Folder)
			if err != nil {
				return err
			}

			if err := worktree.Remove(repoRoot, ts.WorktreePath); err != nil {
				return err
			}

			if err := st.SetWorktreePath(taskName, ""); err != nil {
				return fmt.Errorf("removed worktree but failed to update state: %w", err)
			}

			fmt.Printf("removed %s\n", ts.WorktreePath)
			return nil
		},
	}
}
