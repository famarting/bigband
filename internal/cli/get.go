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
		Short:             "Show all config and state details for a job",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeJobNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			return getJob(args[0])
		},
	}
}

func getJob(name string) error {
	// Look up in config and state in parallel; either alone is sufficient. A
	// job may be configured-only (added but never run), state-only (an
	// ephemeral submit_run that wasn't persisted to config.yaml), or both.
	cfg, _ := config.Load(paths.Config())
	st, _ := state.Load()
	var j *config.Job
	if cfg != nil {
		j = cfg.JobByName(name)
	}
	js := st.Get(name)
	if j == nil && js.LastRun == nil {
		return fmt.Errorf("job %q not found in config or state", name)
	}

	nextRun := "-"
	if reply, err := ipc.Send(ipc.Cmd{Action: "status"}); err == nil && reply.OK {
		var payload ipc.StatusPayload
		if err := json.Unmarshal(reply.Payload, &payload); err == nil {
			for _, s := range payload.Jobs {
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

	fmt.Printf("Job: %s\n", name)

	if j != nil {
		fmt.Println("\nConfig")
		sched := j.Schedule
		schedLabel := "schedule (UTC)"
		if sched == "" {
			sched = "one-off"
			schedLabel = "schedule"
		}
		row(schedLabel, sched)
		row("folder", j.Folder)
		row("enabled", boolLabel(j.IsEnabled()))
		row("worktree", boolLabel(j.ShouldUseWorktree()))
		row("keep_worktree", boolLabel(j.ShouldKeepWorktree()))
		row("reuse_worktree", boolLabel(j.ShouldReuseWorktree()))
		if j.Timeout != nil {
			row("timeout", j.Timeout.String())
		} else {
			row("timeout", cfg.Defaults.Timeout.String()+" (default)")
		}
		if j.Jitter != nil {
			row("jitter", j.Jitter.String())
		} else if cfg.Defaults.Jitter.Duration > 0 {
			row("jitter", cfg.Defaults.Jitter.String()+" (default)")
		}
		row("agent", agentDisplay(cfg, j))
		row("model", inheritedDisplay(j.Model, cfg.Defaults.Model))
		row("effort", inheritedDisplay(j.Effort, cfg.Defaults.Effort))
		multirow("pre_exec", j.PreExec)
		multirow("post_exec", j.PostExec)
		fmt.Printf("\n  prompt:\n")
		for line := range strings.SplitSeq(strings.TrimRight(j.Prompt, "\n"), "\n") {
			fmt.Printf("    %s\n", line)
		}
	} else {
		// Ephemeral one-off: not in config.yaml, only state knows it.
		fmt.Println("\nEphemeral (not in config.yaml)")
		row("folder", dash(js.Folder))
		row("agent", dash(js.Agent))
		if js.Model != "" {
			row("model", js.Model)
		}
		if js.Effort != "" {
			row("effort", js.Effort)
		}
		if js.Timeout != "" {
			row("timeout", js.Timeout)
		}
		if js.Worktree != nil {
			row("worktree", boolLabel(*js.Worktree))
		}
		multirow("pre_exec", js.PreExec)
		multirow("post_exec", js.PostExec)
		if js.Prompt != "" {
			fmt.Printf("\n  prompt:\n")
			for line := range strings.SplitSeq(strings.TrimRight(js.Prompt, "\n"), "\n") {
				fmt.Printf("    %s\n", line)
			}
		}
	}

	fmt.Println("\nState")
	row("status", dash(string(js.LastStatus)))
	if js.LastRun != nil {
		row("last_run (local)", js.LastRun.Local().Format("2006-01-02 15:04:05"))
	} else {
		row("last_run (local)", "-")
	}
	row("duration", dash(js.LastDuration))
	row("next_run (local)", nextRun)
	row("last_log", dash(js.LastLog))
	row("last_reply", dash(js.LastReplyFile))
	row("worktree", dash(js.WorktreePath))
	row("session_id", dash(js.SessionID))

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

// inheritedDisplay renders a string-valued job field, falling back to the
// defaults value with a "(default)" tag, or "-" when neither is set.
func inheritedDisplay(jobVal, defaultVal string) string {
	if jobVal != "" {
		return jobVal
	}
	if defaultVal != "" {
		return defaultVal + " (default)"
	}
	return "-"
}

// agentDisplay renders the resolved agent for a job, tagging where the value
// comes from: explicit job field, defaults.agent, or the built-in fallback.
func agentDisplay(cfg *config.Config, j *config.Job) string {
	switch {
	case j.Agent != "":
		return j.Agent
	case cfg.Defaults.Agent != "":
		return cfg.Defaults.Agent + " (default)"
	default:
		return config.DefaultAgent + " (built-in default)"
	}
}
