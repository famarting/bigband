package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/famarting/bigband/internal/config"
	"github.com/famarting/bigband/internal/ipc"
	"github.com/famarting/bigband/internal/paths"
	"github.com/famarting/bigband/internal/state"
	"github.com/spf13/cobra"
)

// NewPruneCmd registers `bigband prune` — drops ephemeral one-off state
// entries and their log directories. Configured tasks are never touched.
//
// When the daemon is running we route through `forget` IPC so the daemon's
// in-memory map is updated too (otherwise it would clobber our edit on the
// next state save). Logs are deleted CLI-side; the daemon doesn't manage
// log files for ephemerals beyond writing them.
func NewPruneCmd() *cobra.Command {
	var (
		olderThan string
		keepLogs  bool
		dryRun    bool
	)
	cmd := &cobra.Command{
		Use:     "prune",
		Short:   "Remove ephemeral one-off task state and logs older than a cutoff",
		GroupID: "config",
		RunE: func(cmd *cobra.Command, args []string) error {
			cutoffDur := 7 * 24 * time.Hour
			if olderThan != "" {
				d, err := time.ParseDuration(olderThan)
				if err != nil {
					return fmt.Errorf("invalid --older-than %q: %w", olderThan, err)
				}
				cutoffDur = d
			}
			cutoff := time.Now().Add(-cutoffDur)

			cfg, _ := config.Load(paths.Config())
			st, err := state.Load()
			if err != nil {
				return err
			}
			configured := map[string]bool{}
			if cfg != nil {
				for _, t := range cfg.Tasks {
					configured[t.Name] = true
				}
				for _, t := range cfg.Templates {
					configured[t.Name] = true
				}
			}

			// Find candidates without mutating yet (so dry-run is honest).
			var candidates []string
			for name, ts := range st.Tasks {
				if configured[name] || ts == nil || ts.RunningPID != 0 {
					continue
				}
				if ts.LastRun == nil || ts.LastRun.Before(cutoff) {
					candidates = append(candidates, name)
				}
			}
			if len(candidates) == 0 {
				fmt.Printf("nothing to prune (cutoff %s)\n", cutoff.Format(time.RFC3339))
				return nil
			}
			if dryRun {
				fmt.Printf("would prune %d ephemeral task(s) older than %s:\n", len(candidates), cutoff.Format(time.RFC3339))
				for _, n := range candidates {
					fmt.Printf("  %s\n", n)
				}
				return nil
			}

			daemonUp := false
			if reply, err := ipc.Send(ipc.Cmd{Action: "ping"}); err == nil && reply.OK {
				daemonUp = true
			}

			pruned := 0
			for _, name := range candidates {
				if daemonUp {
					reply, err := ipc.Send(ipc.Cmd{Action: "forget", Task: name})
					if err != nil {
						fmt.Printf("warning: forget %s: %v\n", name, err)
						continue
					}
					if !reply.OK {
						fmt.Printf("warning: forget %s rejected: %s\n", name, reply.Error)
						continue
					}
				} else {
					if err := st.RemoveTask(name); err != nil {
						fmt.Printf("warning: remove state %s: %v\n", name, err)
						continue
					}
				}
				if !keepLogs {
					dir := paths.TaskLogDir(name)
					if err := os.RemoveAll(dir); err != nil {
						fmt.Printf("warning: remove logs %s: %v\n", dir, err)
					}
				}
				pruned++
			}
			fmt.Printf("pruned %d ephemeral task(s) (cutoff %s)\n", pruned, cutoff.Format(time.RFC3339))
			return nil
		},
	}
	cmd.Flags().StringVar(&olderThan, "older-than", "", "duration cutoff (default 168h / 7 days)")
	cmd.Flags().BoolVar(&keepLogs, "keep-logs", false, "keep log directories (default: remove them)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "list candidates without removing anything")
	return cmd
}
