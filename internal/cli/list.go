package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/famarting/bigband/internal/config"
	"github.com/famarting/bigband/internal/ipc"
	"github.com/famarting/bigband/internal/paths"
	"github.com/famarting/bigband/internal/state"
	"github.com/robfig/cron/v3"
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
			sortBy, _ := cmd.Flags().GetString("sort")
			byNextRun, err := parseListSort(sortBy)
			if err != nil {
				return err
			}
			reply, err := ipc.Send(ipc.Cmd{Action: "status"})
			if err == nil && reply.OK {
				if asJSON {
					return printJobListJSON(reply, all, byNextRun)
				}
				return printJobList(reply, all, byNextRun)
			}
			if !asJSON {
				fmt.Fprintln(os.Stderr, "daemon not running — next-run times computed from the schedule")
			}
			if asJSON {
				return printOfflineJobListJSON(all, byNextRun)
			}
			return printOfflineJobList(all, byNextRun)
		},
	}
	cmd.Flags().Bool("all", false, "include ephemeral (one-off submitted) jobs")
	cmd.Flags().Bool("json", false, "emit jobs as JSON (for scripting / fws integration)")
	cmd.Flags().String("sort", sortByName, "sort order: name|next (next = soonest next run first)")
	cmd.RegisterFlagCompletionFunc("sort", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{sortByName, sortByNext}, cobra.ShellCompDirectiveNoFileComp
	})
	return cmd
}

const (
	sortByName = "name"
	sortByNext = "next"
)

// parseListSort maps the --sort value onto "sort by next run?".
func parseListSort(v string) (bool, error) {
	switch v {
	case sortByName:
		return false, nil
	case sortByNext, "next-run":
		return true, nil
	default:
		return false, fmt.Errorf("invalid --sort %q: want %s or %s", v, sortByName, sortByNext)
	}
}

// nextRunLayout is the format both the daemon and the offline path use to
// render next-run timestamps.
const nextRunLayout = "2006-01-02 15:04:05"

// lessByNextRun orders two jobs by their NEXT RUN cell. Real timestamps sort
// chronologically and come first; status words ("pending", "running", "done",
// "disabled", "-") have no time, so they sink to the bottom and fall back to
// name order.
func lessByNextRun(nameA, nextA, nameB, nextB string) bool {
	ta, errA := time.ParseInLocation(nextRunLayout, nextA, time.Local)
	tb, errB := time.ParseInLocation(nextRunLayout, nextB, time.Local)
	switch {
	case errA == nil && errB == nil:
		if !ta.Equal(tb) {
			return ta.Before(tb)
		}
	case (errA == nil) != (errB == nil):
		return errA == nil
	}
	return nameA < nameB
}

// offlineNextRun computes a job's next fire time without the daemon, using the
// same UTC interpretation and local-time rendering as scheduler.NextRuns.
// Jitter is not included — it's drawn at run time, exactly as in the daemon.
func offlineNextRun(j *config.Job) string {
	expr := j.CronExpr()
	if expr == "" {
		return "-"
	}
	sched, err := cron.ParseStandard(expr)
	if err != nil {
		return "-"
	}
	next := sched.Next(time.Now().In(time.UTC))
	if next.IsZero() {
		return "-"
	}
	return next.Local().Format(nextRunLayout)
}

// offlineNextRunCell renders the NEXT RUN cell for a configured job without
// the daemon, mirroring the daemon's own wording: disabled jobs read
// "disabled", one-off jobs "pending"/"done", the rest a computed timestamp.
func offlineNextRunCell(j *config.Job, js state.JobState) string {
	switch {
	case !j.IsEnabled():
		return "disabled"
	case j.IsOneOff():
		if js.LastRun == nil {
			return "pending"
		}
		return "done"
	default:
		return offlineNextRun(j)
	}
}

// ephemeralNextRunCell renders the NEXT RUN cell for an ephemeral submission,
// which has no schedule — only a terminal or in-flight state.
func ephemeralNextRunCell(lastStatus state.RunStatus) string {
	if lastStatus == state.StatusRunning {
		return "running"
	}
	return "done"
}

func printJobListJSON(reply *ipc.Reply, all, byNextRun bool) error {
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
	sort.Slice(out, func(i, j int) bool {
		if byNextRun {
			return lessByNextRun(out[i].Name, out[i].NextRun, out[j].Name, out[j].NextRun)
		}
		return out[i].Name < out[j].Name
	})
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func printOfflineJobListJSON(all, byNextRun bool) error {
	cfg, err := config.Load(paths.Config())
	if err != nil {
		return err
	}
	st, _ := state.Load()

	type entry struct {
		Name         string `json:"name"`
		Schedule     string `json:"schedule"`
		Enabled      bool   `json:"enabled"`
		NextRun      string `json:"next_run"`
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
			NextRun:      offlineNextRunCell(j, js),
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
				NextRun:   ephemeralNextRunCell(js.LastStatus),
				Folder:    js.Folder,
				Ephemeral: true,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if byNextRun {
			return lessByNextRun(out[i].Name, out[i].NextRun, out[j].Name, out[j].NextRun)
		}
		return out[i].Name < out[j].Name
	})
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func printJobList(reply *ipc.Reply, all, byNextRun bool) error {
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
	if byNextRun {
		// Chronological order across every section — the partitioning above
		// only decides which jobs are listed, not their order.
		sort.Slice(jobs, func(i, j int) bool {
			return lessByNextRun(jobs[i].Name, jobs[i].NextRun, jobs[j].Name, jobs[j].NextRun)
		})
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

func printOfflineJobList(all, byNextRun bool) error {
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
		js := st.Get(j.Name)
		if j.IsOneOff() {
			sched = "one-off"
		}
		r := row{j.Name, sched, enabled, offlineNextRunCell(j, js), runDir(js.WorktreePath, j.Folder, j.WorktreeMode())}
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
		ephemerals = append(ephemerals, row{name, "one-off", "yes", ephemeralNextRunCell(js.LastStatus), runDir(js.WorktreePath, js.Folder, "")})
	}

	rows := append(scheduled, oneOff...)
	if all {
		rows = append(rows, ephemerals...)
	}
	if byNextRun {
		sort.Slice(rows, func(i, j int) bool {
			return lessByNextRun(rows[i].name, rows[i].nextRun, rows[j].name, rows[j].nextRun)
		})
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSCHEDULE (UTC)\tENABLED\tNEXT RUN (LOCAL)\tRUN DIR")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.name, r.sched, r.enabled, r.nextRun, r.dir)
	}
	return w.Flush()
}
