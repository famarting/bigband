package cli

import (
	"github.com/famarting/bigband/internal/launchd"
	"github.com/spf13/cobra"
)

func NewInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Install the bigband LaunchAgent, or restart if already installed",
		RunE: func(cmd *cobra.Command, args []string) error {
			return launchd.Install(false)
		},
	}
}

func NewUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the bigband LaunchAgent",
		RunE: func(cmd *cobra.Command, args []string) error {
			return launchd.Uninstall()
		},
	}
}
