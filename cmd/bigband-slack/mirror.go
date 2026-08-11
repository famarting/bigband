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

// newMirrorCmd registers `bigband-slack mirror <job>` — re-post the most
// recent completed run of a job to Slack using the current config rules.
//
// Useful for:
//   - testing a freshly-added mirror rule without re-running the job
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
		Use:   "mirror <job-name>",
		Short: "Re-post a historical job completion to Slack (testing / debug)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			jobName := args[0]
			cfg, err := LoadConfig()
			if err != nil {
				return err
			}

			env, data, err := findCompletion(jobName, runID)
			if err != nil {
				return err
			}

			rule := cfg.MatchJob(jobName)
			if rule == nil {
				if channel == "" {
					return fmt.Errorf("no mirror rule matches %q — pass --channel to override, or add a rule with `bigband-slack enable %s --channel ...`", jobName, jobName)
				}
				// No config rule matched; synthesise a minimal one so formatCompletion
				// has sensible defaults. Only IncludeStatus is read by this code path —
				// OnFailure and OpenThread are not checked by the mirror command.
				rule = &MirrorRule{IncludeStatus: true}
			}
			postChannel := channel
			if postChannel == "" {
				postChannel = rule.Channel
			}
			if postChannel == "" {
				postChannel = cfg.Slack.DefaultChannel
			}
			if postChannel == "" {
				return fmt.Errorf("no channel for job %q (no rule channel, no --channel, no default_channel)", jobName)
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

			fmt.Printf("job:      %s\n", env.JobName)
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
			for i, chunk := range splitForSlack(text, slackMaxMessageChars) {
				opts := []slack.MsgOption{
					slack.MsgOptionText(chunk, false),
					slack.MsgOptionDisableLinkUnfurl(),
				}
				if threadTS != "" {
					opts = append(opts, slack.MsgOptionTS(threadTS))
				}
				_, ts, err := api.PostMessage(postChannel, opts...)
				if err != nil {
					return fmt.Errorf("PostMessage chunk %d: %w", i+1, err)
				}
				fmt.Printf("posted: channel=%s ts=%s\n", postChannel, ts)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&runID, "run-id", "", "specific run id (default: latest completion for the job)")
	cmd.Flags().StringVar(&channel, "channel", "", "override the channel from the matched mirror rule")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would be posted without calling Slack")
	return cmd
}

// findCompletion scans events.jsonl for a job_run.completed envelope. When
// runID is empty, the most recent matching event wins. Order is the order
// events were appended; we walk the whole file because individual entries
// are tiny relative to typical events.jsonl sizes (retention bounds growth).
func findCompletion(jobName, runID string) (bigbandext.Envelope, bigbandext.JobRunCompletedData, error) {
	f, err := os.Open(paths.EventsFile())
	if err != nil {
		return bigbandext.Envelope{}, bigbandext.JobRunCompletedData{}, fmt.Errorf("open %s: %w", paths.EventsFile(), err)
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
		if env.Type != bigbandext.TypeJobRunCompleted {
			continue
		}
		if env.JobName != jobName {
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
		return bigbandext.Envelope{}, bigbandext.JobRunCompletedData{}, err
	}
	if latest == nil {
		if runID != "" {
			return bigbandext.Envelope{}, bigbandext.JobRunCompletedData{}, fmt.Errorf("no job_run.completed found for job=%q run=%q", jobName, runID)
		}
		return bigbandext.Envelope{}, bigbandext.JobRunCompletedData{}, fmt.Errorf("no job_run.completed found for job=%q", jobName)
	}
	var data bigbandext.JobRunCompletedData
	if err := json.Unmarshal(latest.Data, &data); err != nil {
		return bigbandext.Envelope{}, bigbandext.JobRunCompletedData{}, fmt.Errorf("decode payload: %w", err)
	}
	return *latest, data, nil
}
