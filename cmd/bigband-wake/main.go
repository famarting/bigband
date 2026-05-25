// Command bigband-wake is a macOS-only bigband extension that keeps the
// system's pmset wake schedule in sync with the next firing of each enabled
// scheduled bigband job. The goal: a closed-lid MacBook on AC wakes itself a
// minute before each job fires, runs it, and goes back to sleep — so cron
// firings aren't silently lost while the laptop is asleep.
//
// It is a separate binary that talks to the bigband daemon only via the
// public IPC + events contracts:
//
//   - subscribes to job_run.completed / config.reloaded events to know when
//     to recompute the wake schedule
//   - calls Status to read each job's next_run
//   - shells out to `sudo -n pmset schedule …` to register / cancel wakes
//
// Configuration lives at ~/.bigband/extensions/bigband-wake/config.yaml.
// pmset access is granted via a narrowly-scoped sudoers stanza printed by
// `bigband-wake setup`; the bigband core daemon never invokes sudo or pmset
// directly. This keeps privilege escalation contained to ~this~ binary.
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
		Use:   "bigband-wake",
		Short: "Wake-from-sleep scheduler for bigband (macOS)",
		Long: `bigband-wake keeps macOS's pmset wake schedule in sync with bigband's
cron schedule so the laptop wakes itself moments before each job fires.

It is a separate process from the bigband daemon and communicates only via
the documented IPC and events contracts. Configuration lives at
~/.bigband/extensions/bigband-wake/config.yaml.

Opt-in: with enabled:false (the default after init) the extension does
nothing. Run ` + "`bigband-wake init`" + ` to scaffold config + manifest, then
` + "`bigband-wake setup`" + ` to print the sudoers stanza that grants
narrowly-scoped pmset access.`,
		SilenceUsage: true,
	}

	root.AddGroup(
		&cobra.Group{ID: "config", Title: "Config:"},
		&cobra.Group{ID: "ops", Title: "Operations:"},
	)

	addCmd := func(group string, cmds ...*cobra.Command) {
		for _, c := range cmds {
			c.GroupID = group
			root.AddCommand(c)
		}
	}

	// daemon is the supervisor entry point — hidden from help. The bigband
	// daemon spawns this via the manifest at extensions/bigband-wake/.
	root.AddCommand(newDaemonCmd())

	addCmd("config", newInitCmd(), newSetupCmd())
	addCmd("ops", newStatusCmd(), newClearCmd(), newTestCmd(), newVerifyCmd())

	return root
}
