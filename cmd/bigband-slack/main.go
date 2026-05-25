// Command bigband-slack is the Slack reference integration for bigband.
//
// It is a separate binary that talks to the bigband daemon only via the
// public IPC + events contracts:
//
//   - subscribes to lifecycle events to mirror completed runs to Slack
//   - calls submit_run to fire one-off jobs from Slack messages
//   - calls submit_run with parent_session_id to follow up in threads
//
// Configuration lives at ~/.bigband/extensions/bigband-slack/config.yaml.
// By default the binary mirrors NOTHING — only jobs listed in mirror rules
// post to Slack. This keeps bigband core 100% Slack-agnostic and makes the
// integration explicitly opt-in.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func main() {
	if err := newRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "bigband-slack",
		Short: "Slack integration sidecar for bigband",
		Long: `bigband-slack mirrors completed bigband job runs into Slack threads
and triggers new bigband runs from Slack messages.

It is a separate process from the bigband daemon and only communicates via
the documented IPC and events contracts. Configuration lives at
~/.bigband/extensions/bigband-slack/config.yaml — by default nothing
is mirrored until you opt jobs in via mirror rules.`,
		SilenceUsage: true,
	}

	root.AddGroup(
		&cobra.Group{ID: "config", Title: "Config:"},
		&cobra.Group{ID: "daemon", Title: "Service:"},
	)

	addCmd := func(group string, cmds ...*cobra.Command) {
		for _, c := range cmds {
			c.GroupID = group
			root.AddCommand(c)
		}
	}

	// `daemon` is the supervisor entry point — hidden from help. The bigband
	// daemon spawns this via the manifest at extensions/bigband-slack/. Users
	// interact via `bigband ext list` / `bigband ext logs bigband-slack`.
	root.AddCommand(newDaemonCmd())

	addCmd("config", newInitCmd(), newRulesCmd(), newEnableCmd(), newDisableCmd(), newTriggerCmd())
	addCmd("daemon", newMirrorCmd())

	return root
}
