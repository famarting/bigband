package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// configTemplate is the scaffolded config.yaml written by `bigband-slack init`.
// Comments explain each section so users (and coding agents) can edit it
// without consulting docs. mirror and trigger_channels are intentionally empty
// so the integration starts in the safe opt-in posture.
//
// __FOLDER__ is replaced at write time with a sensible default ($HOME/work).
const configTemplate = `# bigband-slack configuration.
#
# Opt-in by default: with empty mirror[] and trigger_channels[], this binary
# does NOTHING. Add rules below to mirror task completions into Slack and / or
# fire bigband runs from Slack messages.
#
# Tokens may be inlined (insecure), env:NAME (read $NAME at startup), or
# file:/abs/path (read first line of file). Prefer env: or file: in production.

slack:
  app_token: env:SLACK_APP_TOKEN          # xapp-... — Socket Mode app-level token
  bot_token: env:SLACK_BOT_TOKEN          # xoxb-... — Bot User OAuth Token
  default_channel: ""                     # fallback channel name when a rule omits one

# Outbound: which task runs are mirrored to Slack, and how. First match wins.
# Each rule needs at least one of "task" (exact name or simple glob) or "tasks"
# (list). Omit or set enabled:false to opt out.
#
# Wildcard note: only "*" is supported as a wildcard character. It may appear
# as a prefix ("*-report"), suffix ("daily-*"), or standalone ("*") to match
# everything. Full glob patterns like "**", "?", or character classes "[...]"
# are NOT supported and will be treated as literal characters.
mirror: []
# Example rule (uncomment + edit):
# mirror:
#   - task: morning-brief
#     channel: "#daily"
#     open_thread: true                   # post final message as a new thread
#     include_status: true                # prepend "ok in 4m12s"
#     on_failure: false                   # also post on non-success runs
#   - tasks: ["report-*", "alert-*"]
#     channel: "#reports"
#     open_thread: true

# Inbound: which Slack channels can fire bigband runs.
trigger_channels: []
# Example trigger (uncomment + edit):
# trigger_channels:
#   - channel: "#bigband-control"
#     folder: __FOLDER__
#     require_mention: true
#     allow_freeform_prompt: true         # plain message → ephemeral submit_run
#     commands:
#       - match: "^run (?P<task>\\S+)$"
#         action: run                     # bigband run <task>
#       - match: "^task (?P<name>\\S+):\\s*(?P<prompt>.+)"
#         action: submit

# Thread-reply behaviour
threads:
  enabled: true                           # if false, replies are ignored
  resume_with_session: true               # use ParentSessionID; otherwise fresh ephemeral
`

func renderConfigTemplate() string {
	folder := "/path/to/your/repo"
	if home, err := os.UserHomeDir(); err == nil {
		folder = filepath.Join(home, "work")
	}
	return strings.ReplaceAll(configTemplate, "__FOLDER__", folder)
}

// manifestTemplate is written by `bigband-slack init` so the bigband daemon
// can supervise this binary without a separate LaunchAgent. The user only
// needs to run `bigband install` once for the whole system.
const manifestTemplate = `# bigband-slack — extension manifest.
#
# The bigband daemon discovers this file and supervises bigband-slack as a
# child process. No separate LaunchAgent is needed — running ` + "`bigband install`" + `
# is enough.
#
# Verify after editing: bigband ext list

name: bigband-slack
description: "Slack mirror — opt-in per task in extensions/bigband-slack/config.yaml"
command:
  - bigband-slack
  - daemon

env:
  PATH: ${env:PATH}
  SLACK_APP_TOKEN: ${env:SLACK_APP_TOKEN}
  SLACK_BOT_TOKEN: ${env:SLACK_BOT_TOKEN}

restart:
  policy: on_failure
  initial_backoff: 1s
  max_backoff: 30s
  max_consecutive_failures: 5

subscribes:
  - claude.session_started
  - task_run.worktree_ready
  - task_run.completed
`

// ManifestPath returns the canonical manifest path for bigband-slack.
func ManifestPath() string {
	return filepath.Join(filepath.Dir(ConfigPath()), "manifest.yaml")
}

func newInitCmd() *cobra.Command {
	var (
		forceConfig   bool
		forceManifest bool
		force         bool
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Scaffold the bigband-slack config, manifest, and state directory",
		Long: `Writes config.yaml (containing your Slack rules) and manifest.yaml (which
the bigband daemon uses to supervise this binary) into
~/.bigband-tasks/extensions/bigband-slack/.

By default neither file is overwritten if it already exists. Use:
  --force-manifest   to refresh just manifest.yaml (safe; no rules)
  --force-config     to overwrite config.yaml WITH THE TEMPLATE (DESTROYS your rules)
  --force            equivalent to --force-config --force-manifest`,
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
				fmt.Printf("kept existing %s (pass --force-config to overwrite — destroys your rules)\n", cfgPath)
			} else {
				if err := os.WriteFile(cfgPath, []byte(renderConfigTemplate()), 0600); err != nil {
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
			fmt.Println("  1. Create a Slack app with Socket Mode enabled.")
			fmt.Println("     https://api.slack.com/apps  →  From scratch")
			fmt.Println("     Bot scopes: chat:write, channels:history, channels:read,")
			fmt.Println("                 groups:history, groups:read, app_mentions:read, users:read")
			fmt.Println("     App-level token scope: connections:write")
			fmt.Println("     Event subscriptions: app_mention, message.channels, message.groups")
			fmt.Println("  2. Export tokens (or replace env: refs in the config with file:/path):")
			fmt.Println("       export SLACK_APP_TOKEN=xapp-...")
			fmt.Println("       export SLACK_BOT_TOKEN=xoxb-...")
			fmt.Println("  3. Add a mirror rule:    bigband-slack enable <task> --channel '#bigband'")
			fmt.Println("  4. Make sure bigband itself is installed: bigband install")
			fmt.Println("     The daemon will pick up this manifest within ~300ms.")
			fmt.Println("  5. Verify:               bigband ext list")
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite both config and manifest (DESTROYS your rules)")
	cmd.Flags().BoolVar(&forceConfig, "force-config", false, "overwrite config.yaml — DESTROYS your existing rules")
	cmd.Flags().BoolVar(&forceManifest, "force-manifest", false, "overwrite manifest.yaml only (safe; no rules in it)")
	return cmd
}
