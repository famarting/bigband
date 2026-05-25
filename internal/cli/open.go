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
		Use:               "open <job>",
		Short:             "Open a job's worktree (or folder) in $VISUAL / code",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeJobNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			return openJob(args[0])
		},
	}
}

func openJob(name string) error {
	cfg, err := config.Load(paths.Config())
	if err != nil {
		return err
	}
	st, _ := state.Load()
	js := st.Get(name)

	dir := ""
	if j := cfg.JobByName(name); j != nil {
		dir = j.Folder
	}
	if js.WorktreePath != "" {
		dir = js.WorktreePath
	} else if dir == "" {
		dir = js.Folder
	}
	if dir == "" {
		return fmt.Errorf("job %q not found", name)
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
