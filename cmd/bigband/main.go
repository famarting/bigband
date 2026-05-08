package main

import (
	"fmt"
	"os"
	"runtime/debug"

	"github.com/famarting/bigband/internal/cli"
	"github.com/spf13/cobra"
)

// version is overridden at build time via -ldflags "-X main.version=...".
// Falls back to the build-info module version (when installed via `go install`)
// or "dev" otherwise.
var version = ""

func resolveVersion() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

func main() {
	if err := newRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:     "bigband",
		Short:   "Scheduled Claude Code task runner",
		Version: resolveVersion(),
		Long: `bigband schedules and runs Claude Code prompts on a cron schedule.

Config lives at ~/.bigband-tasks/config.yaml
Logs live at   ~/.bigband-tasks/logs/<task>/

Quick start:
  bigband add              # add your first task
  bigband install          # install as a launchd service (auto-starts on login)
  bigband list             # list configured tasks and schedules
  bigband status           # show recent execution history
  bigband logs <task> -f   # tail a run`,
		SilenceUsage: true,
	}

	root.AddGroup(
		&cobra.Group{ID: "daemon", Title: "Service:"},
		&cobra.Group{ID: "tasks", Title: "Tasks:"},
		&cobra.Group{ID: "run", Title: "Running:"},
		&cobra.Group{ID: "config", Title: "Config:"},
	)

	addCmd := func(group string, cmds ...*cobra.Command) {
		for _, c := range cmds {
			c.GroupID = group
			root.AddCommand(c)
		}
	}

	addCmd("daemon", cli.NewDaemonCmd(), cli.NewInstallCmd(), cli.NewUninstallCmd(), cli.NewDaemonLogsCmd())
	addCmd("tasks", cli.NewStatusCmd(), cli.NewListCmd(), cli.NewGetCmd(), cli.NewAddCmd(), cli.NewTemplateCmd(), cli.NewEditCmd(), cli.NewRmCmd(), cli.NewEnableCmd(), cli.NewDisableCmd(), cli.NewOpenCmd(), cli.NewWorktreeCmd())
	addCmd("run", cli.NewRunCmd(), cli.NewStopCmd(), cli.NewLogsCmd(), cli.NewResumeCmd())
	addCmd("config", cli.NewValidateCmd(), cli.NewConfigCmd())

	return root
}
