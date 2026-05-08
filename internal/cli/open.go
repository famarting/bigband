package cli

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/famarting/bigband/internal/config"
	"github.com/famarting/bigband/internal/paths"
	"github.com/famarting/bigband/internal/state"
	"github.com/spf13/cobra"
)

func NewOpenCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "open <task>",
		Short:             "Open a task's worktree (or folder) in $VISUAL / code",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeTaskNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			return openTask(args[0])
		},
	}
}

func openTask(name string) error {
	cfg, err := config.Load(paths.Config())
	if err != nil {
		return err
	}
	t := cfg.TaskByName(name)
	if t == nil {
		return fmt.Errorf("task %q not found", name)
	}

	dir := t.Folder
	st, _ := state.Load()
	if ts := st.Get(name); ts.WorktreePath != "" {
		dir = ts.WorktreePath
	}

	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = "code"
	}
	c := exec.Command(editor, dir)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}
