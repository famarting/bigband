package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/famarting/bigband/internal/config"
	"github.com/famarting/bigband/internal/ipc"
	"github.com/famarting/bigband/internal/paths"
	"github.com/famarting/bigband/internal/runner"
	"github.com/famarting/bigband/internal/state"
	"github.com/spf13/cobra"
)

func NewStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "stop <name>",
		Short:             "Stop a running task",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeTaskNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			reply, err := ipc.Send(ipc.Cmd{Action: "stop", Task: name})
			if err != nil {
				return fmt.Errorf("cannot reach daemon: %w", err)
			}
			if !reply.OK {
				return fmt.Errorf("%s", reply.Error)
			}
			fmt.Printf("sent stop signal to task %q\n", name)
			return nil
		},
	}
}

func NewRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "run <name>",
		Short:             "Fire a task immediately",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeTaskNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			// Try daemon first.
			reply, err := ipc.Send(ipc.Cmd{Action: "run", Task: name})
			if err == nil {
				if !reply.OK {
					return fmt.Errorf("daemon error: %s", reply.Error)
				}
				prevLog, _ := os.Readlink(paths.TaskLogLatest(name))
				return waitAndFollowLog(name, prevLog)
			}
			// Daemon not running — run inline.
			fmt.Printf("Daemon not running; executing %q inline...\n", name)
			cfg, err := config.Load(paths.Config())
			if err != nil {
				return err
			}
			t := cfg.TaskByName(name)
			if t == nil {
				return fmt.Errorf("task %q not found", name)
			}
			st, _ := state.Load()
			t.ClearJitter() // Don't apply jitter to manual runs.
			runner.Run(context.Background(), cfg, t, st, os.Stdout)
			return nil
		},
	}
}
