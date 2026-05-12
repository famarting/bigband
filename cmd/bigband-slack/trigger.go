package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// newTriggerCmd registers `bigband-slack trigger` and its subcommands. These
// manage inbound trigger channels (Slack message → bigband run) symmetrically
// with `enable` / `disable` for outbound mirror rules.
func newTriggerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trigger",
		Short: "Inbound trigger channel operations",
	}
	cmd.AddCommand(newTriggerListCmd(), newTriggerAddCmd(), newTriggerRmCmd())
	return cmd
}

func newTriggerListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List configured trigger channels",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := LoadConfig()
			if err != nil {
				return err
			}
			if len(cfg.TriggerChannels) == 0 {
				fmt.Println("(no trigger channels — bigband-slack will not fire any runs from Slack messages)")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "CHANNEL\tFOLDER\tMENTION\tFREEFORM\tCOMMANDS")
			for _, t := range cfg.TriggerChannels {
				ch := t.Channel
				if !strings.HasPrefix(ch, "#") && !looksLikeChannelID(ch) {
					ch = "#" + ch
				}
				fmt.Fprintf(w, "%s\t%s\t%v\t%v\t%d\n", ch, t.Folder, t.RequireMention, t.AllowFreeformPrompt, len(t.Commands))
			}
			return w.Flush()
		},
	}
}

func newTriggerAddCmd() *cobra.Command {
	var (
		channel        string
		folder         string
		requireMention bool
		freeform       bool
	)
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Add a trigger channel rule (Slack message → bigband run)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if channel == "" || folder == "" {
				return fmt.Errorf("--channel and --folder are required")
			}
			if _, err := os.Stat(folder); err != nil {
				return fmt.Errorf("--folder %q: %w", folder, err)
			}
			cfg, err := LoadConfig()
			if err != nil {
				return err
			}
			// Replace any existing rule for the same channel — keeps the list
			// deduped without forcing the user to rm first.
			out := cfg.TriggerChannels[:0]
			for _, t := range cfg.TriggerChannels {
				if channelEquivalent(t.Channel, channel) {
					continue
				}
				out = append(out, t)
			}
			out = append(out, TriggerChannel{
				Channel:             channel,
				Folder:              folder,
				RequireMention:      requireMention,
				AllowFreeformPrompt: freeform,
			})
			cfg.TriggerChannels = out
			if err := SaveConfig(cfg); err != nil {
				return err
			}
			fmt.Printf("trigger added: %s → %s\n", channel, folder)
			return nil
		},
	}
	cmd.Flags().StringVar(&channel, "channel", "", "slack channel name (with or without leading #) or channel ID (required)")
	cmd.Flags().StringVar(&folder, "folder", "", "directory submitted runs execute in (required)")
	cmd.Flags().BoolVar(&requireMention, "require-mention", true, "only act on @<bot> messages")
	cmd.Flags().BoolVar(&freeform, "allow-freeform", true, "treat unrecognised messages as ephemeral submit_run prompts")
	return cmd
}

func newTriggerRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <channel>",
		Short: "Remove a trigger channel rule",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			channel := args[0]
			cfg, err := LoadConfig()
			if err != nil {
				return err
			}
			before := len(cfg.TriggerChannels)
			out := cfg.TriggerChannels[:0]
			for _, t := range cfg.TriggerChannels {
				if channelEquivalent(t.Channel, channel) {
					continue
				}
				out = append(out, t)
			}
			if len(out) == before {
				return fmt.Errorf("no trigger channel matched %q", channel)
			}
			cfg.TriggerChannels = out
			if err := SaveConfig(cfg); err != nil {
				return err
			}
			fmt.Printf("trigger removed: %s\n", channel)
			return nil
		},
	}
}

// channelEquivalent compares two channel identifiers ignoring a leading "#".
func channelEquivalent(a, b string) bool {
	return strings.TrimPrefix(a, "#") == strings.TrimPrefix(b, "#")
}

// looksLikeChannelID returns true for things shaped like a Slack channel ID
// (uppercase, starts with C/G/D, no spaces). Used purely for cosmetic "#"
// prefixing in the list output.
func looksLikeChannelID(s string) bool {
	if len(s) < 2 {
		return false
	}
	switch s[0] {
	case 'C', 'G', 'D':
	default:
		return false
	}
	for _, r := range s[1:] {
		if !((r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z')) {
			return false
		}
	}
	return true
}
