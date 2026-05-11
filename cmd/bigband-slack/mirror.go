package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"

	"github.com/famarting/bigband/internal/paths"
	"github.com/famarting/bigband/pkg/bigbandext"
	"github.com/slack-go/slack"
	"github.com/spf13/cobra"
)

// newMirrorCmd registers `bigband-slack mirror <task>` — re-post the most
// recent completed run of a task to Slack using the current config rules.
//
// Useful for:
//   - testing a freshly-added mirror rule without re-running the task
//   - debugging "why didn't this post?" by re-running the same path with logs
//   - replaying a missed completion when the integration was down at the time
//
// This is a one-shot CLI command: it does NOT need bigband-slack daemon to be
// running. It reads events.jsonl directly, looks up the matching mirror rule
// from your config, and POSTs to Slack with the same formatting the live path
// would have used.
func newMirrorCmd() *cobra.Command {
	var (
		runID   string
		channel string
		dryRun  bool
	)
	cmd := &cobra.Command{
		Use:   "mirror <task-name>",
		Short: "Re-post a historical task completion to Slack (testing / debug)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			taskName := args[0]
			cfg, err := LoadConfig()
			if err != nil {
				return err
			}

			env, data, err := findCompletion(taskName, runID)
			if err != nil {
				return err
			}

			rule := cfg.MatchTask(taskName)
			if rule == nil {
				rule = &MirrorRule{OnFailure: true, IncludeStatus: true, OpenThread: true}
				if channel == "" {
					return fmt.Errorf("no mirror rule matches %q — pass --channel to override, or add a rule with `bigband-slack enable %s --channel ...`", taskName, taskName)
				}
			}
			postChannel := channel
			if postChannel == "" {
				postChannel = rule.Channel
			}
			if postChannel == "" {
				postChannel = cfg.Slack.DefaultChannel
			}
			if postChannel == "" {
				return fmt.Errorf("no channel for task %q (no rule channel, no --channel, no default_channel)", taskName)
			}

			// If the running daemon already linked this run to a thread, post
			// into that thread; otherwise the post will create a new one.
			threadTS := ""
			if store, err := LoadStore(); err == nil {
				if mapping := store.LookupRun(env.RunID); mapping.ThreadTS != "" {
					threadTS = mapping.ThreadTS
				}
			}

			text := formatCompletion(env, data, rule)

			fmt.Printf("task:     %s\n", env.TaskName)
			fmt.Printf("run:      %s\n", env.RunID)
			fmt.Printf("status:   %s\n", data.Status)
			fmt.Printf("channel:  %s\n", postChannel)
			if threadTS != "" {
				fmt.Printf("thread:   %s (existing)\n", threadTS)
			} else {
				fmt.Printf("thread:   (new)\n")
			}
			fmt.Println("---")
			fmt.Println(text)
			fmt.Println("---")

			if dryRun {
				fmt.Println("(dry run — not posting)")
				return nil
			}
			bot := cfg.Slack.ResolvedBotToken()
			if bot == "" {
				return fmt.Errorf("slack.bot_token is empty")
			}
			api := slack.New(bot)
			opts := []slack.MsgOption{
				slack.MsgOptionText(text, false),
				slack.MsgOptionDisableLinkUnfurl(),
			}
			if threadTS != "" {
				opts = append(opts, slack.MsgOptionTS(threadTS))
			}
			_, ts, err := api.PostMessage(postChannel, opts...)
			if err != nil {
				return fmt.Errorf("PostMessage: %w", err)
			}
			fmt.Printf("posted: channel=%s ts=%s\n", postChannel, ts)
			return nil
		},
	}
	cmd.Flags().StringVar(&runID, "run-id", "", "specific run id (default: latest completion for the task)")
	cmd.Flags().StringVar(&channel, "channel", "", "override the channel from the matched mirror rule")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would be posted without calling Slack")
	return cmd
}

// findCompletion scans events.jsonl for a task_run.completed envelope. When
// runID is empty, the most recent matching event wins. Order is the order
// events were appended; we walk the whole file because individual entries
// are tiny relative to typical events.jsonl sizes (retention bounds growth).
func findCompletion(taskName, runID string) (bigbandext.Envelope, bigbandext.TaskRunCompletedData, error) {
	f, err := os.Open(paths.EventsFile())
	if err != nil {
		return bigbandext.Envelope{}, bigbandext.TaskRunCompletedData{}, fmt.Errorf("open %s: %w", paths.EventsFile(), err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var latest *bigbandext.Envelope
	for scanner.Scan() {
		var env bigbandext.Envelope
		if err := json.Unmarshal(scanner.Bytes(), &env); err != nil {
			continue
		}
		if env.Type != bigbandext.TypeTaskRunCompleted {
			continue
		}
		if env.TaskName != taskName {
			continue
		}
		if runID != "" && env.RunID != runID {
			continue
		}
		e := env
		latest = &e
		if runID != "" {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return bigbandext.Envelope{}, bigbandext.TaskRunCompletedData{}, err
	}
	if latest == nil {
		if runID != "" {
			return bigbandext.Envelope{}, bigbandext.TaskRunCompletedData{}, fmt.Errorf("no task_run.completed found for task=%q run=%q", taskName, runID)
		}
		return bigbandext.Envelope{}, bigbandext.TaskRunCompletedData{}, fmt.Errorf("no task_run.completed found for task=%q", taskName)
	}
	var data bigbandext.TaskRunCompletedData
	if err := json.Unmarshal(latest.Data, &data); err != nil {
		return bigbandext.Envelope{}, bigbandext.TaskRunCompletedData{}, fmt.Errorf("decode payload: %w", err)
	}
	return *latest, data, nil
}
