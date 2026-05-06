package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/famarting/bigband/internal/config"
	"github.com/famarting/bigband/internal/ipc"
	"github.com/famarting/bigband/internal/paths"
	"github.com/famarting/bigband/internal/state"
	"github.com/spf13/cobra"
)

func NewListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List configured tasks with their schedule and next run",
		RunE: func(cmd *cobra.Command, args []string) error {
			reply, err := ipc.Send(ipc.Cmd{Action: "status"})
			if err == nil && reply.OK {
				return printTaskList(reply)
			}
			fmt.Fprintln(os.Stderr, "daemon not running — next-run times unavailable")
			return printOfflineTaskList()
		},
	}
}

func printTaskList(reply *ipc.Reply) error {
	var payload ipc.StatusPayload
	if err := json.Unmarshal(reply.Payload, &payload); err != nil {
		return err
	}
	fmt.Printf("bigband daemon  uptime: %s\n\n", payload.Uptime)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSCHEDULE\tENABLED\tNEXT RUN")
	for _, t := range payload.Tasks {
		sched := t.Schedule
		if sched == "" {
			sched = "one-off"
		}
		enabled := "yes"
		if !t.Enabled {
			enabled = "no"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", t.Name, sched, enabled, t.NextRun)
	}
	return w.Flush()
}

func printOfflineTaskList() error {
	cfg, err := config.Load(paths.Config())
	if err != nil {
		return err
	}
	st, _ := state.Load()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSCHEDULE\tENABLED\tNEXT RUN")
	for _, t := range cfg.Tasks {
		enabled := "yes"
		if !t.IsEnabled() {
			enabled = "no"
		}
		sched := t.Schedule
		nextRun := "-"
		if t.IsOneOff() {
			sched = "one-off"
			ts := st.Get(t.Name)
			if ts.LastRun == nil {
				nextRun = "pending"
			} else {
				nextRun = "done"
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", t.Name, sched, enabled, nextRun)
	}
	return w.Flush()
}
