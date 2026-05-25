package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/famarting/bigband/internal/config"
	"github.com/famarting/bigband/internal/events"
	"github.com/famarting/bigband/internal/ipc"
	"github.com/famarting/bigband/internal/paths"
	"github.com/famarting/bigband/internal/runner"
	"github.com/famarting/bigband/internal/state"
	"github.com/spf13/cobra"
)

func NewStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "stop <name>",
		Short:             "Stop a running job",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeJobNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			reply, err := ipc.Send(ipc.Cmd{Action: "stop", Job: name})
			if err != nil {
				return fmt.Errorf("cannot reach daemon: %w", err)
			}
			if !reply.OK {
				return fmt.Errorf("%s", reply.Error)
			}
			fmt.Printf("sent stop signal to job %q\n", name)
			return nil
		},
	}
}

func NewRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "run <name>",
		Short:             "Fire a job immediately",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeJobNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			// Try daemon first.
			reply, err := ipc.Send(ipc.Cmd{Action: "run", Job: name})
			if err == nil {
				if !reply.OK {
					return fmt.Errorf("daemon error: %s", reply.Error)
				}
				prevLog, _ := os.Readlink(paths.JobLogLatest(name))
				return waitAndFollowLog(name, prevLog)
			}
			// Daemon not running — run inline.
			fmt.Printf("Daemon not running; executing %q inline...\n", name)
			cfg, err := config.Load(paths.Config())
			if err != nil {
				return err
			}
			j := cfg.JobByName(name)
			if j == nil {
				return fmt.Errorf("job %q not found", name)
			}
			st, _ := state.Load()
			j.ClearJitter() // Don't apply jitter to manual runs.
			runner.Run(context.Background(), cfg, j, st, os.Stdout, events.NopPublisher{})
			return nil
		},
	}
}
