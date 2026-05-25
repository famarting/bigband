package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
)

// configTemplate is what `bigband-wake init` writes when the config file
// doesn't yet exist. The defaults are deliberately conservative: enabled
// starts false, so dropping the manifest in place is reversible — the
// supervisor will spawn the daemon, it'll log "enabled=false; idling", and
// nothing else happens until the user flips the flag.
const configTemplate = `# bigband-wake configuration.
#
# Opt-in: with enabled:false (the default) the extension does NOTHING.
# Flip enabled:true after running ` + "`bigband-wake setup`" + ` and following
# the printed sudoers instructions; pmset access via ` + "`sudo -n`" + ` is the
# only privileged operation this binary performs.

# Master switch. Set to true once setup is complete.
enabled: false

# Seconds to wake before each job's scheduled fire time. 60 gives the
# daemon time to thaw before cron triggers.
lead_seconds: 60

# Cap on the number of pmset wake entries this extension keeps registered
# at once. macOS allows up to 64 in total; 16 leaves headroom for entries
# added by other apps (Calendar, DoNotDisturb, etc.).
max_events: 16

# Safety-net cadence: even if every event-bus nudge is missed, reconcile
# from scratch this often. Minimum 1 minute.
reconcile_interval: 1h

# How long to hold an IOPMAssertion (the programmatic equivalent of
# ` + "`caffeinate -i`" + `) after we detect a wake-from-sleep transition. macOS may
# only dark-wake for ~30s in response to a pmset wake — too short for the
# bigband daemon's cron tick to fire. Holding this assertion keeps the
# laptop awake long enough for the scheduled job to launch. Set to "0" to
# disable.
assertion_duration: 45m
`

// manifestTemplate is the supervisor manifest. The bigband daemon discovers
// it via the manifest watcher and launches us automatically — no per-extension
// LaunchAgent. Restart policy mirrors bigband-slack.
const manifestTemplate = `# bigband-wake — extension manifest.
#
# The bigband daemon discovers this file and supervises bigband-wake as a
# child process. No separate LaunchAgent is needed — running ` + "`bigband install`" + `
# is enough.
#
# Verify after editing: bigband ext list

name: bigband-wake
description: "Wake-from-sleep scheduler — opt-in via extensions/bigband-wake/config.yaml (macOS only)"
command:
  - bigband-wake
  - daemon

env:
  PATH: /opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin

restart:
  policy: on_failure
  initial_backoff: 1s
  max_backoff: 30s
  max_consecutive_failures: 5

subscribes:
  - job_run.completed
  - config.reloaded
`

func newInitCmd() *cobra.Command {
	var (
		forceConfig   bool
		forceManifest bool
		force         bool
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Scaffold the bigband-wake config and manifest",
		Long: `Writes config.yaml and manifest.yaml into
~/.bigband/extensions/bigband-wake/. Neither file is overwritten by
default. Use --force-config or --force-manifest to overwrite.

After init, run ` + "`bigband-wake setup`" + ` and follow the printed
sudoers instructions, then flip ` + "`enabled: true`" + ` in config.yaml.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if force {
				forceConfig = true
				forceManifest = true
			}
			cfgPath := ConfigPath()
			manifestPath := ManifestPath()
			if err := os.MkdirAll(filepath.Dir(cfgPath), 0700); err != nil {
				return fmt.Errorf("creating dir: %w", err)
			}
			if _, err := os.Stat(cfgPath); err == nil && !forceConfig {
				fmt.Printf("kept existing %s (pass --force-config to overwrite)\n", cfgPath)
			} else {
				if err := os.WriteFile(cfgPath, []byte(configTemplate), 0600); err != nil {
					return fmt.Errorf("writing config: %w", err)
				}
				fmt.Printf("wrote %s\n", cfgPath)
			}
			if _, err := os.Stat(manifestPath); err != nil || forceManifest {
				if err := os.WriteFile(manifestPath, []byte(manifestTemplate), 0600); err != nil {
					return fmt.Errorf("writing manifest: %w", err)
				}
				fmt.Printf("wrote %s\n", manifestPath)
			} else {
				fmt.Printf("kept existing %s (pass --force-manifest to overwrite)\n", manifestPath)
			}
			fmt.Println()
			fmt.Println("Next steps:")
			fmt.Println("  1. Run `bigband-wake setup` and follow the sudoers instructions.")
			fmt.Println("  2. Edit config.yaml and set `enabled: true`.")
			fmt.Println("  3. Ensure bigband itself is installed: `bigband install`.")
			fmt.Println("  4. Verify: `bigband ext list` should show bigband-wake.")
			fmt.Println("  5. Test the full path end-to-end: `bigband-wake test`.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite both config and manifest")
	cmd.Flags().BoolVar(&forceConfig, "force-config", false, "overwrite config.yaml")
	cmd.Flags().BoolVar(&forceManifest, "force-manifest", false, "overwrite manifest.yaml")
	return cmd
}

func newSetupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "Print the sudoers stanza that grants narrowly-scoped pmset access",
		Long: `bigband-wake calls ` + "`sudo -n pmset schedule wake/cancel ...`" + ` to register
wake events. The -n flag refuses to prompt for a password; a sudoers stanza
must therefore allow this exact verb without one.

This command prints the recommended stanza and the commands to install it.
It does NOT run sudo itself — installation is a one-time manual step.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Print(SudoersStanza())
			return nil
		},
	}
}

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show owned wake entries plus the full pmset schedule",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := LoadState()
			if err != nil {
				return err
			}
			fmt.Printf("Owned wake events (%d):\n", len(st.Events))
			if len(st.Events) == 0 {
				fmt.Println("  (none)")
			}
			for _, e := range st.Sorted() {
				fmt.Printf("  - job=%-30s wake=%s  fire=%s\n",
					e.Job,
					e.WakeAt.Local().Format("2006-01-02 15:04:05"),
					e.FireAt.Local().Format("2006-01-02 15:04:05"))
			}
			fmt.Println()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			out, err := dumpPmsetSched(ctx)
			fmt.Println("pmset -g sched output:")
			if err != nil {
				fmt.Printf("  (pmset failed: %v)\n", err)
			}
			if out != "" {
				fmt.Print(out)
			}
			return nil
		},
	}
}

func newClearCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "clear",
		Short: "Cancel every wake currently owned by bigband-wake",
		Long: `Cancels every wake entry in this extension's state.json by calling
` + "`sudo -n pmset schedule cancel wake ...`" + ` for each. User-added pmset
entries are NOT touched — only the events bigband-wake created itself.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := LoadState()
			if err != nil {
				return err
			}
			if len(st.Events) == 0 {
				fmt.Println("nothing to cancel")
				return nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			r := &reconciler{state: st}
			r.cfg.Store(&Config{})
			r.cancelAll(ctx)
			fmt.Printf("cancelAll left %d events in state (failed cancels retained for retry)\n", len(r.state.Events))
			return nil
		},
	}
}

func newTestCmd() *cobra.Command {
	var delay time.Duration
	cmd := &cobra.Command{
		Use:   "test",
		Short: "Schedule a one-off wake N seconds from now to verify sudo+pmset",
		Long: `Smoke-test the full sudoers → pmset path by scheduling a single wake
event a short time in the future. The event is NOT recorded in state.json
so it won't be touched by the daemon's reconcile.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			when := time.Now().Add(delay).Local()
			if err := schedulePmsetWake(ctx, when); err != nil {
				return err
			}
			fmt.Printf("scheduled wake at %s\n", when.Format("2006-01-02 15:04:05"))
			fmt.Println("verify with `pmset -g sched`, and unplug the magsafe / close the lid to confirm wake-from-sleep actually fires")
			return nil
		},
	}
	cmd.Flags().DurationVar(&delay, "in", 2*time.Minute, "how far in the future to schedule the test wake")
	return cmd
}
