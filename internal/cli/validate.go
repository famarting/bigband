package cli

import (
	"fmt"

	"github.com/famarting/bigband/internal/config"
	"github.com/famarting/bigband/internal/paths"
	"github.com/famarting/bigband/internal/schedule"
	"github.com/spf13/cobra"
)

func NewValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate [path]",
		Short: "Parse and validate the config, printing any errors",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath := paths.Config()
			if len(args) == 1 {
				cfgPath = args[0]
			}
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			fmt.Printf("Config OK — %d job(s), %d template(s)\n", len(cfg.Jobs), len(cfg.Templates))
			if len(cfg.Jobs) > 0 {
				fmt.Println("Schedules are UTC; `bigband list` shows next runs in local time.")
			}
			for _, j := range cfg.Jobs {
				cronExpr, hasJitter, _ := schedule.Parse(j.Schedule)
				jitterStr := ""
				if hasJitter {
					jt := cfg.Defaults.Jitter.Duration
					if j.JitterDuration() > 0 {
						jt = j.JitterDuration()
					}
					jitterStr = fmt.Sprintf(" (±%s jitter)", jt)
				}
				enabled := "enabled"
				if !j.IsEnabled() {
					enabled = "disabled"
				}
				fmt.Printf("  %-24s  %s → cron: %s%s  [%s]\n",
					j.Name, j.Schedule, cronExpr, jitterStr, enabled)
			}
			for _, t := range cfg.Templates {
				fmt.Printf("  %-24s  [template]\n", t.Name)
			}
			return nil
		},
	}
}

func NewConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Config file utilities",
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "path",
		Short: "Print the config file path",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println(paths.Config())
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "edit",
		Short: "Open the config file in $EDITOR",
		RunE: func(cmd *cobra.Command, args []string) error {
			return openEditor(paths.Config())
		},
	})

	return cmd
}
