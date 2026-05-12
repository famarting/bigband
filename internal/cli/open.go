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
	st, _ := state.Load()
	ts := st.Get(name)

	dir := ""
	if t := cfg.TaskByName(name); t != nil {
		dir = t.Folder
	}
	if ts.WorktreePath != "" {
		dir = ts.WorktreePath
	} else if dir == "" {
		dir = ts.Folder
	}
	if dir == "" {
		return fmt.Errorf("task %q not found", name)
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
