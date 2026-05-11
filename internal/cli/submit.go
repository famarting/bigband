package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/famarting/bigband/internal/config"
	"github.com/famarting/bigband/internal/ipc"
	"github.com/famarting/bigband/internal/paths"
	"github.com/famarting/bigband/internal/state"
	"github.com/spf13/cobra"
)

// NewSubmitCmd registers `bigband submit` — fire a one-off run with an inline
// task definition, no config.yaml edit required. Useful for integrations that
// drive bigband from outside the YAML (Slack, webhooks, ad-hoc scripts).
func NewSubmitCmd() *cobra.Command {
	var (
		folder          string
		prompt          string
		name            string
		ephemeral       bool
		parentSessionID string
		timeout         string
		model           string
		effort          string
		triggeredBy     string
		preExec         []string
		postExec        []string
		noWorktree      bool
	)
	cmd := &cobra.Command{
		Use:     "submit",
		Short:   "Submit a one-off run to the daemon (no config.yaml edit)",
		GroupID: "run",
		RunE: func(cmd *cobra.Command, args []string) error {
			if folder == "" || prompt == "" {
				return fmt.Errorf("--folder and --prompt are required")
			}
			req := &ipc.SubmitRunRequest{
				Name:            name,
				Folder:          folder,
				Prompt:          prompt,
				PreExec:         preExec,
				PostExec:        postExec,
				ParentSessionID: parentSessionID,
				Ephemeral:       ephemeral,
				TriggeredBy:     triggeredBy,
				Timeout:         timeout,
				Model:           model,
				Effort:          effort,
			}
			if noWorktree {
				f := false
				req.Worktree = &f
			}
			reply, err := ipc.Send(ipc.Cmd{Action: "submit", Submit: req})
			if err != nil {
				return fmt.Errorf("cannot reach daemon: %w", err)
			}
			if !reply.OK {
				return fmt.Errorf("daemon error: %s", reply.Error)
			}
			var out ipc.SubmitRunReply
			if err := json.Unmarshal(reply.Payload, &out); err != nil {
				return fmt.Errorf("decoding reply: %w", err)
			}
			fmt.Printf("submitted: task=%s run_id=%s\n", out.TaskName, out.RunID)
			return nil
		},
	}
	cmd.Flags().StringVar(&folder, "folder", "", "directory the run executes in (required)")
	cmd.Flags().StringVar(&prompt, "prompt", "", "Claude prompt to run (required)")
	cmd.Flags().StringVar(&name, "name", "", "explicit task name (auto-generated when blank)")
	cmd.Flags().BoolVar(&ephemeral, "ephemeral", true, "do not persist the task to config.yaml")
	cmd.Flags().StringVar(&parentSessionID, "parent-session-id", "", "resume an existing Claude session id (--resume)")
	cmd.Flags().StringVar(&timeout, "timeout", "", "task timeout (e.g. 30m)")
	cmd.Flags().StringVar(&model, "model", "", "Claude model override")
	cmd.Flags().StringVar(&effort, "effort", "", "Claude effort override")
	cmd.Flags().StringVar(&triggeredBy, "triggered-by", "", "free-form label for traceability")
	cmd.Flags().StringSliceVar(&preExec, "pre-exec", nil, "shell command to run before claude (repeatable)")
	cmd.Flags().StringSliceVar(&postExec, "post-exec", nil, "shell command to run after claude (repeatable)")
	cmd.Flags().BoolVar(&noWorktree, "no-worktree", false, "run directly in --folder without creating a git worktree")
	return cmd
}

// NewFollowupCmd registers `bigband followup <task> "<prompt>"` — sugar for
// submit with parent_session_id resolved from the task's recorded SessionID
// and folder taken from the task's worktree (or its configured folder).
func NewFollowupCmd() *cobra.Command {
	var (
		ephemeral   bool
		triggeredBy string
	)
	cmd := &cobra.Command{
		Use:               "followup <task> <prompt>",
		Short:             "Send a follow-up prompt to a task's last Claude session",
		Args:              cobra.ExactArgs(2),
		GroupID:           "run",
		ValidArgsFunction: completeTaskNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			prompt := args[1]
			// Look in config first, fall back to state. Ephemeral one-offs
			// only exist in state — they never appear in config.yaml.
			cfg, _ := config.Load(paths.Config())
			st, _ := state.Load()
			ts := st.Get(name)
			var configuredFolder string
			if cfg != nil {
				if t := cfg.TaskByName(name); t != nil {
					configuredFolder = t.Folder
				}
			}
			if configuredFolder == "" && ts.LastRun == nil {
				return fmt.Errorf("task %q not found in config or state", name)
			}
			if ts.SessionID == "" {
				return fmt.Errorf("task %q has no recorded session id — run it at least once first", name)
			}
			// Folder preference order:
			//   1. existing worktree path (preserves cwd identical to the
			//      original session)
			//   2. state.Folder (recorded by SetRunning on first run since
			//      this field was added)
			//   3. configured task folder (config.yaml)
			folder := ""
			if ts.WorktreePath != "" {
				if _, err := os.Stat(ts.WorktreePath); err == nil {
					folder = ts.WorktreePath
				}
			}
			if folder == "" {
				folder = ts.Folder
			}
			if folder == "" {
				folder = configuredFolder
			}
			if folder == "" {
				return fmt.Errorf("task %q has no recorded folder — use `bigband submit --folder ... --parent-session-id %s` instead", name, ts.SessionID)
			}
			// Resuming a session in a fresh worktree is almost always wrong:
			// the session was born in a specific filesystem state, so the
			// follow-up either runs inside the original task's worktree (if
			// state still has it) or directly in the recorded folder. Never
			// create a fresh worktree on resume.
			noWorktree := false
			req := &ipc.SubmitRunRequest{
				Folder:          folder,
				Prompt:          prompt,
				ParentSessionID: ts.SessionID,
				Worktree:        &noWorktree,
				Ephemeral:       ephemeral,
				TriggeredBy:     triggeredBy,
			}
			reply, err := ipc.Send(ipc.Cmd{Action: "submit", Submit: req})
			if err != nil {
				return fmt.Errorf("cannot reach daemon: %w", err)
			}
			if !reply.OK {
				return fmt.Errorf("daemon error: %s", reply.Error)
			}
			var out ipc.SubmitRunReply
			if err := json.Unmarshal(reply.Payload, &out); err != nil {
				return fmt.Errorf("decoding reply: %w", err)
			}
			fmt.Printf("followup submitted: task=%s run_id=%s\n", out.TaskName, out.RunID)
			return nil
		},
	}
	cmd.Flags().BoolVar(&ephemeral, "ephemeral", true, "do not persist the follow-up as a new task")
	cmd.Flags().StringVar(&triggeredBy, "triggered-by", "cli:followup", "free-form label for traceability")
	return cmd
}
