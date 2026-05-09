package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/famarting/bigband/internal/config"
	"github.com/famarting/bigband/internal/ipc"
	"github.com/famarting/bigband/internal/paths"
	"github.com/famarting/bigband/internal/state"
	"github.com/spf13/cobra"
)

func NewGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "get <name>",
		Short:             "Show all config and state details for a task",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeTaskNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			return getTask(args[0])
		},
	}
}

func getTask(name string) error {
	// Look up in config and state in parallel; either alone is sufficient. A
	// task may be configured-only (added but never run), state-only (an
	// ephemeral submit_run that wasn't persisted to config.yaml), or both.
	cfg, _ := config.Load(paths.Config())
	st, _ := state.Load()
	var t *config.Task
	if cfg != nil {
		t = cfg.TaskByName(name)
	}
	ts := st.Get(name)
	if t == nil && ts.LastRun == nil {
		return fmt.Errorf("task %q not found in config or state", name)
	}

	nextRun := "-"
	if reply, err := ipc.Send(ipc.Cmd{Action: "status"}); err == nil && reply.OK {
		var payload ipc.StatusPayload
		if err := json.Unmarshal(reply.Payload, &payload); err == nil {
			for _, s := range payload.Tasks {
				if s.Name == name {
					nextRun = s.NextRun
					break
				}
			}
		}
	}

	row := func(label, value string) {
		fmt.Printf("  %-20s %s\n", label+":", value)
	}
	multirow := func(label string, lines []string) {
		if len(lines) == 0 {
			row(label, "-")
			return
		}
		for i, l := range lines {
			if i == 0 {
				row(label, l)
			} else {
				fmt.Printf("  %-20s %s\n", "", l)
			}
		}
	}

	fmt.Printf("Task: %s\n", name)

	if t != nil {
		fmt.Println("\nConfig")
		sched := t.Schedule
		if sched == "" {
			sched = "one-off"
		}
		row("schedule", sched)
		row("folder", t.Folder)
		row("enabled", boolLabel(t.IsEnabled()))
		row("worktree", boolLabel(t.ShouldUseWorktree()))
		row("keep_worktree", boolLabel(t.ShouldKeepWorktree()))
		row("reuse_worktree", boolLabel(t.ShouldReuseWorktree()))
		if t.Timeout != nil {
			row("timeout", t.Timeout.String())
		} else {
			row("timeout", cfg.Defaults.Timeout.String()+" (default)")
		}
		if t.Jitter != nil {
			row("jitter", t.Jitter.String())
		} else if cfg.Defaults.Jitter.Duration > 0 {
			row("jitter", cfg.Defaults.Jitter.String()+" (default)")
		}
		multirow("pre_exec", t.PreExec)
		multirow("post_exec", t.PostExec)
		if len(t.ExtraClaudeFlags) > 0 {
			row("extra_claude_flags", strings.Join(t.ExtraClaudeFlags, " "))
		}
		fmt.Printf("\n  prompt:\n")
		for line := range strings.SplitSeq(strings.TrimRight(t.Prompt, "\n"), "\n") {
			fmt.Printf("    %s\n", line)
		}
	} else {
		// Ephemeral one-off: not in config.yaml, only state knows it.
		fmt.Println("\nEphemeral (not in config.yaml)")
		row("folder", dash(ts.Folder))
	}

	fmt.Println("\nState")
	row("status", dash(string(ts.LastStatus)))
	if ts.LastRun != nil {
		row("last_run", ts.LastRun.Local().Format("2006-01-02 15:04:05"))
	} else {
		row("last_run", "-")
	}
	row("duration", dash(ts.LastDuration))
	row("next_run", nextRun)
	row("last_log", dash(ts.LastLog))
	row("last_reply", dash(ts.LastReplyFile))
	row("worktree", dash(ts.WorktreePath))
	row("session_id", dash(ts.SessionID))

	return nil
}

func boolLabel(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
