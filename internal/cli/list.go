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
		Short:   "List configured jobs with their schedule and next run",
		RunE: func(cmd *cobra.Command, args []string) error {
			all, _ := cmd.Flags().GetBool("all")
			asJSON, _ := cmd.Flags().GetBool("json")
			reply, err := ipc.Send(ipc.Cmd{Action: "status"})
			if err == nil && reply.OK {
				if asJSON {
					return printJobListJSON(reply, all)
				}
				return printJobList(reply, all)
			}
			if !asJSON {
				fmt.Fprintln(os.Stderr, "daemon not running — next-run times unavailable")
			}
			if asJSON {
				return printOfflineJobListJSON(all)
			}
			return printOfflineJobList(all)
		},
	}
	cmd.Flags().Bool("all", false, "include ephemeral (one-off submitted) jobs")
	cmd.Flags().Bool("json", false, "emit jobs as JSON (for scripting / fws integration)")
	return cmd
}

func printJobListJSON(reply *ipc.Reply, all bool) error {
	var payload ipc.StatusPayload
	if err := json.Unmarshal(reply.Payload, &payload); err != nil {
		return err
	}
	out := make([]ipc.JobStatus, 0, len(payload.Jobs))
	for _, j := range payload.Jobs {
		if j.Ephemeral && !all {
			continue
		}
		out = append(out, j)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func printOfflineJobListJSON(all bool) error {
	cfg, err := config.Load(paths.Config())
	if err != nil {
		return err
	}
	st, _ := state.Load()

	type entry struct {
		Name         string `json:"name"`
		Schedule     string `json:"schedule"`
		Enabled      bool   `json:"enabled"`
		Folder       string `json:"folder"`
		WorktreePath string `json:"worktree_path,omitempty"`
		WorktreeMode string `json:"worktree_mode,omitempty"`
		Ephemeral    bool   `json:"ephemeral,omitempty"`
	}

	out := make([]entry, 0, len(cfg.Jobs))
	seen := map[string]bool{}
	for _, j := range cfg.Jobs {
		seen[j.Name] = true
		js := st.Get(j.Name)
		out = append(out, entry{
			Name:         j.Name,
			Schedule:     j.Schedule,
			Enabled:      j.IsEnabled(),
			Folder:       j.Folder,
			WorktreePath: js.WorktreePath,
			WorktreeMode: j.WorktreeMode(),
		})
	}
	if all {
		for name, js := range st.Jobs {
			if seen[name] {
				continue
			}
			if js.LastRun == nil {
				continue
			}
			out = append(out, entry{
				Name:      name,
				Enabled:   true,
				Folder:    js.Folder,
				Ephemeral: true,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func printJobList(reply *ipc.Reply, all bool) error {
	var payload ipc.StatusPayload
	if err := json.Unmarshal(reply.Payload, &payload); err != nil {
		return err
	}

	// Partition: scheduled → one-off configured → ephemerals.
	var scheduled, oneOff, ephemerals []ipc.JobStatus
	for _, j := range payload.Jobs {
		switch {
		case j.Ephemeral:
			ephemerals = append(ephemerals, j)
		case j.Schedule != "":
			scheduled = append(scheduled, j)
		default:
			oneOff = append(oneOff, j)
		}
	}
	sort.Slice(scheduled, func(i, j int) bool { return scheduled[i].Name < scheduled[j].Name })
	sort.Slice(oneOff, func(i, j int) bool { return oneOff[i].Name < oneOff[j].Name })
	sort.Slice(ephemerals, func(i, j int) bool { return ephemerals[i].Name < ephemerals[j].Name })

	jobs := append(scheduled, oneOff...)
	if all {
		jobs = append(jobs, ephemerals...)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSCHEDULE (UTC)\tENABLED\tNEXT RUN (LOCAL)\tRUN DIR")
	for _, j := range jobs {
		sched := j.Schedule
		if sched == "" {
			sched = "one-off"
		}
		enabled := "yes"
		if !j.Enabled {
			enabled = "no"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", j.Name, sched, enabled, j.NextRun, runDir(j.WorktreePath, j.Folder, j.WorktreeMode))
	}
	return w.Flush()
}

// runDir picks the directory to display alongside the worktree mode: the
// active worktree path if recorded, otherwise the configured job folder. The
// mode (when non-empty) is appended in parentheses so the reader can tell at a
// glance whether the job uses a worktree and how it manages it.
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

func printOfflineJobList(all bool) error {
	cfg, err := config.Load(paths.Config())
	if err != nil {
		return err
	}
	st, _ := state.Load()

	type row struct{ name, sched, enabled, nextRun, dir string }

	var scheduled, oneOff []row
	seen := map[string]bool{}
	for _, j := range cfg.Jobs {
		seen[j.Name] = true
		enabled := "yes"
		if !j.IsEnabled() {
			enabled = "no"
		}
		sched := j.Schedule
		nextRun := "-"
		js := st.Get(j.Name)
		if j.IsOneOff() {
			sched = "one-off"
			if js.LastRun == nil {
				nextRun = "pending"
			} else {
				nextRun = "done"
			}
		}
		r := row{j.Name, sched, enabled, nextRun, runDir(js.WorktreePath, j.Folder, j.WorktreeMode())}
		if j.IsOneOff() {
			oneOff = append(oneOff, r)
		} else {
			scheduled = append(scheduled, r)
		}
	}
	sort.Slice(scheduled, func(i, j int) bool { return scheduled[i].name < scheduled[j].name })
	sort.Slice(oneOff, func(i, j int) bool { return oneOff[i].name < oneOff[j].name })

	// Collect ephemeral submissions (state-only entries not in config.yaml).
	var ephemeralNames []string
	for name := range st.Jobs {
		if !seen[name] {
			ephemeralNames = append(ephemeralNames, name)
		}
	}
	sort.Strings(ephemeralNames)
	var ephemerals []row
	for _, name := range ephemeralNames {
		js := st.Get(name)
		if js.LastRun == nil {
			continue
		}
		nextRun := "done"
		if js.LastStatus == state.StatusRunning {
			nextRun = "running"
		}
		ephemerals = append(ephemerals, row{name, "one-off", "yes", nextRun, runDir(js.WorktreePath, js.Folder, "")})
	}

	rows := append(scheduled, oneOff...)
	if all {
		rows = append(rows, ephemerals...)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSCHEDULE (UTC)\tENABLED\tNEXT RUN (LOCAL)\tRUN DIR")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.name, r.sched, r.enabled, r.nextRun, r.dir)
	}
	return w.Flush()
}
