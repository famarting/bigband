package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

func newRulesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rules",
		Short: "Mirror rule operations",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List configured mirror rules",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := LoadConfig()
			if err != nil {
				return err
			}
			if len(cfg.Mirror) == 0 {
				fmt.Println("(no mirror rules — bigband-slack is mirroring nothing)")
				return nil
			}
			for i, r := range cfg.Mirror {
				patterns := r.Patterns()
				if len(patterns) == 0 {
					patterns = []string{"(empty)"}
				}
				enabled := "enabled"
				if !r.IsEnabled() {
					enabled = "disabled"
				}
				fmt.Printf("[%d] %s → %s (%s, thread=%v)\n", i, strings.Join(patterns, ","), r.Channel, enabled, r.OpenThread)
			}
			return nil
		},
	})
	return cmd
}

func newEnableCmd() *cobra.Command {
	var (
		channel string
		thread  bool
	)
	cmd := &cobra.Command{
		Use:   "enable <task>",
		Short: "Add an enabled mirror rule for a task",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := LoadConfig()
			if err != nil {
				return err
			}
			name := args[0]
			// Replace any existing rule for this exact task.
			out := cfg.Mirror[:0]
			for _, r := range cfg.Mirror {
				if r.Task == name {
					continue
				}
				out = append(out, r)
			}
			t := true
			out = append(out, MirrorRule{
				Task:       name,
				Channel:    channel,
				OpenThread: thread,
				Enabled:    &t,
			})
			cfg.Mirror = out
			if err := SaveConfig(cfg); err != nil {
				return err
			}
			fmt.Printf("enabled mirror for %s → %s\n", name, channel)
			return nil
		},
	}
	cmd.Flags().StringVar(&channel, "channel", "", "slack channel (required)")
	cmd.Flags().BoolVar(&thread, "thread", true, "post final message in a new thread")
	cmd.MarkFlagRequired("channel") //nolint:errcheck
	return cmd
}

func newDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disable <task>",
		Short: "Disable mirror rule for a task",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := LoadConfig()
			if err != nil {
				return err
			}
			name := args[0]
			found := false
			for i := range cfg.Mirror {
				if cfg.Mirror[i].Task == name {
					f := false
					cfg.Mirror[i].Enabled = &f
					found = true
				}
			}
			if !found {
				return fmt.Errorf("no mirror rule found for %q", name)
			}
			if err := SaveConfig(cfg); err != nil {
				return err
			}
			fmt.Printf("disabled mirror for %s\n", name)
			return nil
		},
	}
}
