package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/famarting/bigband/internal/config"
	"github.com/famarting/bigband/internal/ipc"
	"github.com/famarting/bigband/internal/paths"
	"github.com/famarting/bigband/internal/state"
	"github.com/spf13/cobra"
)

func NewListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List configured tasks with their schedule and next run",
		RunE: func(cmd *cobra.Command, args []string) error {
			all, _ := cmd.Flags().GetBool("all")
			reply, err := ipc.Send(ipc.Cmd{Action: "status"})
			if err == nil && reply.OK {
				return printTaskList(reply, all)
			}
			fmt.Fprintln(os.Stderr, "daemon not running — next-run times unavailable")
			return printOfflineTaskList(all)
		},
	}
	cmd.Flags().Bool("all", false, "include ephemeral (one-off submitted) tasks")
	return cmd
}

func printTaskList(reply *ipc.Reply, all bool) error {
	var payload ipc.StatusPayload
	if err := json.Unmarshal(reply.Payload, &payload); err != nil {
		return err
	}

	// Partition: scheduled → one-off configured → ephemerals.
	var scheduled, oneOff, ephemerals []ipc.TaskStatus
	for _, t := range payload.Tasks {
		switch {
		case t.Ephemeral:
			ephemerals = append(ephemerals, t)
		case t.Schedule != "":
			scheduled = append(scheduled, t)
		default:
			oneOff = append(oneOff, t)
		}
	}
	sort.Slice(scheduled, func(i, j int) bool { return scheduled[i].Name < scheduled[j].Name })
	sort.Slice(oneOff, func(i, j int) bool { return oneOff[i].Name < oneOff[j].Name })
	sort.Slice(ephemerals, func(i, j int) bool { return ephemerals[i].Name < ephemerals[j].Name })

	tasks := append(scheduled, oneOff...)
	if all {
		tasks = append(tasks, ephemerals...)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSCHEDULE\tENABLED\tNEXT RUN\tRUN DIR")
	for _, t := range tasks {
		sched := t.Schedule
		if sched == "" {
			sched = "one-off"
		}
		enabled := "yes"
		if !t.Enabled {
			enabled = "no"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", t.Name, sched, enabled, t.NextRun, runDir(t.WorktreePath, t.Folder, t.WorktreeMode))
	}
	return w.Flush()
}

// runDir picks the directory to display alongside the worktree mode: the
// active worktree path if recorded, otherwise the configured task folder. The
// mode (when non-empty) is appended in parentheses so the reader can tell at a
// glance whether the task uses a worktree and how it manages it.
func runDir(worktreePath, folder, mode string) string {
	dir := worktreePath
	if dir == "" {
		dir = folder
	}
	if dir == "" {
		return "-"
	}
	if mode != "" {
		return dir + " (" + mode + ")"
	}
	return dir
}

func printOfflineTaskList(all bool) error {
	cfg, err := config.Load(paths.Config())
	if err != nil {
		return err
	}
	st, _ := state.Load()

	type row struct{ name, sched, enabled, nextRun, dir string }

	var scheduled, oneOff []row
	seen := map[string]bool{}
	for _, t := range cfg.Tasks {
		seen[t.Name] = true
		enabled := "yes"
		if !t.IsEnabled() {
			enabled = "no"
		}
		sched := t.Schedule
		nextRun := "-"
		ts := st.Get(t.Name)
		if t.IsOneOff() {
			sched = "one-off"
			if ts.LastRun == nil {
				nextRun = "pending"
			} else {
				nextRun = "done"
			}
		}
		r := row{t.Name, sched, enabled, nextRun, runDir(ts.WorktreePath, t.Folder, t.WorktreeMode())}
		if t.IsOneOff() {
			oneOff = append(oneOff, r)
		} else {
			scheduled = append(scheduled, r)
		}
	}
	sort.Slice(scheduled, func(i, j int) bool { return scheduled[i].name < scheduled[j].name })
	sort.Slice(oneOff, func(i, j int) bool { return oneOff[i].name < oneOff[j].name })

	// Collect ephemeral submissions (state-only entries not in config.yaml).
	var ephemeralNames []string
	for name := range st.Tasks {
		if !seen[name] {
			ephemeralNames = append(ephemeralNames, name)
		}
	}
	sort.Strings(ephemeralNames)
	var ephemerals []row
	for _, name := range ephemeralNames {
		ts := st.Get(name)
		if ts.LastRun == nil {
			continue
		}
		nextRun := "done"
		if ts.LastStatus == state.StatusRunning {
			nextRun = "running"
		}
		ephemerals = append(ephemerals, row{name, "one-off", "yes", nextRun, runDir(ts.WorktreePath, ts.Folder, "")})
	}

	rows := append(scheduled, oneOff...)
	if all {
		rows = append(rows, ephemerals...)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSCHEDULE\tENABLED\tNEXT RUN\tRUN DIR")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.name, r.sched, r.enabled, r.nextRun, r.dir)
	}
	return w.Flush()
}
