package cli

import (
	"encoding/json"
	"fmt"

	"github.com/famarting/bigband/internal/ipc"
	"github.com/famarting/bigband/internal/state"
	"github.com/spf13/cobra"
)

// NewRerunCmd registers `bigband rerun <name>` — re-fire a previous job using
// the input parameters captured in state on its last run. The new run inherits
// the original folder, prompt, pre/post-exec, worktree mode, timeout, model,
// effort, and agent. It's the natural retry for an ephemeral submit whose log
// you don't want to scrape.
//
// Submission is always ephemeral so the original state row gets overwritten
// in place (same job name → same state slot), rather than leaving a trail of
// "<name>-2", "<name>-3" rows behind.
func NewRerunCmd() *cobra.Command {
	var triggeredBy string
	cmd := &cobra.Command{
		Use:               "rerun <name>",
		Short:             "Re-fire a job using its last recorded inputs (prompt, folder, agent, ...)",
		Args:              cobra.ExactArgs(1),
		GroupID:           "run",
		ValidArgsFunction: completeJobNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			st, err := state.Load()
			if err != nil {
				return fmt.Errorf("load state: %w", err)
			}
			js := st.Get(name)
			if js.LastRun == nil {
				return fmt.Errorf("job %q has no recorded state — nothing to rerun", name)
			}
			if js.Prompt == "" {
				return fmt.Errorf("job %q has no recorded prompt — likely last ran before rerun was added; use `bigband submit` with the prompt instead", name)
			}
			if js.Folder == "" {
				return fmt.Errorf("job %q has no recorded folder", name)
			}
			req := &ipc.SubmitRunRequest{
				Name:        name,
				Folder:      js.Folder,
				Prompt:      js.Prompt,
				PreExec:     js.PreExec,
				PostExec:    js.PostExec,
				Worktree:    js.Worktree,
				Timeout:     js.Timeout,
				Model:       js.Model,
				Effort:      js.Effort,
				Agent:       js.Agent,
				Ephemeral:   true,
				TriggeredBy: triggeredBy,
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
			fmt.Printf("rerun submitted: job=%s run_id=%s\n", out.JobName, out.RunID)
			return nil
		},
	}
	cmd.Flags().StringVar(&triggeredBy, "triggered-by", "cli:rerun", "free-form label for traceability")
	return cmd
}
