package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/famarting/bigband/internal/config"
	"github.com/famarting/bigband/internal/ipc"
	"github.com/famarting/bigband/internal/paths"
	"github.com/famarting/bigband/internal/state"
	"github.com/famarting/bigband/internal/worktree"
	"github.com/spf13/cobra"
)

// NewPruneCmd registers `bigband prune` — drops ephemeral one-off state
// entries and their log directories. Configured jobs are never touched.
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
		Short:   "Remove ephemeral one-off job state and logs older than a cutoff",
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
				for _, j := range cfg.Jobs {
					configured[j.Name] = true
				}
				for _, t := range cfg.Templates {
					configured[t.Name] = true
				}
			}

			// Find candidates without mutating yet (so dry-run is honest).
			// Snapshot Folder + WorktreePath here so we can clean up the
			// worktree after the state row is removed (forget IPC and
			// RemoveJob both drop the row without touching disk).
			type candidate struct {
				name         string
				folder       string
				worktreePath string
			}
			var candidates []candidate
			for name, js := range st.Jobs {
				if configured[name] || js == nil || js.RunningPID != 0 {
					continue
				}
				// Respect keep_worktree — owners rely on
				// the worktree surviving past the run's completion. Skip auto-pruning
				// these; they require an explicit `bigband rm`.
				if js.KeepWorktree && js.WorktreePath != "" {
					continue
				}
				if js.LastRun == nil || js.LastRun.Before(cutoff) {
					candidates = append(candidates, candidate{
						name:         name,
						folder:       js.Folder,
						worktreePath: js.WorktreePath,
					})
				}
			}
			if len(candidates) == 0 {
				fmt.Printf("nothing to prune (cutoff %s)\n", cutoff.Format(time.RFC3339))
				return nil
			}
			if dryRun {
				fmt.Printf("would prune %d ephemeral job(s) older than %s:\n", len(candidates), cutoff.Format(time.RFC3339))
				for _, c := range candidates {
					if c.worktreePath != "" {
						fmt.Printf("  %s (worktree %s)\n", c.name, c.worktreePath)
					} else {
						fmt.Printf("  %s\n", c.name)
					}
				}
				return nil
			}

			daemonUp := false
			if reply, err := ipc.Send(ipc.Cmd{Action: "ping"}); err == nil && reply.OK {
				daemonUp = true
			}

			pruned := 0
			for _, c := range candidates {
				if daemonUp {
					reply, err := ipc.Send(ipc.Cmd{Action: "forget", Job: c.name})
					if err != nil {
						fmt.Printf("warning: forget %s: %v\n", c.name, err)
						continue
					}
					if !reply.OK {
						fmt.Printf("warning: forget %s rejected: %s\n", c.name, reply.Error)
						continue
					}
				} else {
					if err := st.RemoveJob(c.name); err != nil {
						fmt.Printf("warning: remove state %s: %v\n", c.name, err)
						continue
					}
				}
				if !keepLogs {
					dir := paths.JobLogDir(c.name)
					if err := os.RemoveAll(dir); err != nil {
						fmt.Printf("warning: remove logs %s: %v\n", dir, err)
					}
				}
				if c.worktreePath != "" {
					removeEphemeralWorktree(c.name, c.folder, c.worktreePath)
				}
				pruned++
			}
			fmt.Printf("pruned %d ephemeral job(s) (cutoff %s)\n", pruned, cutoff.Format(time.RFC3339))
			return nil
		},
	}
	cmd.Flags().StringVar(&olderThan, "older-than", "", "duration cutoff (default 168h / 7 days)")
	cmd.Flags().BoolVar(&keepLogs, "keep-logs", false, "keep log directories (default: remove them)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "list candidates without removing anything")
	return cmd
}

// removeEphemeralWorktree deletes the worktree an ephemeral job owned. Only
// invoked for ephemerals (configured jobs are filtered out upstream), and
// worktree.Remove guards against a stray path: it requires the basename to
// match "<repo>-bb-<job>" and sit as a sibling of the repo root.
func removeEphemeralWorktree(name, folder, wtPath string) {
	if folder == "" {
		fmt.Printf("warning: cannot remove worktree %s for %s: no recorded folder\n", wtPath, name)
		return
	}
	if _, err := os.Stat(wtPath); err != nil {
		return
	}
	repoRoot, err := worktree.RepoRoot(folder)
	if err != nil {
		fmt.Printf("warning: cannot remove worktree %s for %s: %v\n", wtPath, name, err)
		return
	}
	if err := worktree.Remove(repoRoot, wtPath); err != nil {
		fmt.Printf("warning: remove worktree %s for %s: %v\n", wtPath, name, err)
		return
	}
	fmt.Printf("removed worktree %s\n", wtPath)
}
