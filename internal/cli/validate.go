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
			fmt.Printf("Config OK — %d task(s), %d template(s)\n", len(cfg.Tasks), len(cfg.Templates))
			for _, t := range cfg.Tasks {
				cronExpr, hasJitter, _ := schedule.Parse(t.Schedule)
				jitterStr := ""
				if hasJitter {
					j := cfg.Defaults.Jitter.Duration
					if t.JitterDuration() > 0 {
						j = t.JitterDuration()
					}
					jitterStr = fmt.Sprintf(" (±%s jitter)", j)
				}
				enabled := "enabled"
				if !t.IsEnabled() {
					enabled = "disabled"
				}
				fmt.Printf("  %-24s  %s → cron: %s%s  [%s]\n",
					t.Name, t.Schedule, cronExpr, jitterStr, enabled)
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
