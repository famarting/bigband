package cli

import (
	"github.com/famarting/bigband/internal/config"
	"github.com/famarting/bigband/internal/paths"
	"github.com/spf13/cobra"
)

// completeTaskNames is a ValidArgsFunction that completes configured task names.
func completeTaskNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		// Beyond the name position, fall through to default (file) completion
		// so commands like `worktree move <task> <dest>` can complete paths.
		return nil, cobra.ShellCompDirectiveDefault
	}
	cfg, err := config.LoadUnvalidated(paths.Config())
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names := make([]string, 0, len(cfg.Tasks))
	for _, t := range cfg.Tasks {
		names = append(names, t.Name)
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}

// completeTemplateNames completes configured template names.
func completeTemplateNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		// Beyond the name position, fall through to default (file) completion
		// so commands like `worktree move <task> <dest>` can complete paths.
		return nil, cobra.ShellCompDirectiveDefault
	}
	cfg, err := config.LoadUnvalidated(paths.Config())
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names := make([]string, 0, len(cfg.Templates))
	for _, t := range cfg.Templates {
		names = append(names, t.Name)
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}

// completeTaskOrTemplateNames completes both task and template names.
func completeTaskOrTemplateNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		// Beyond the name position, fall through to default (file) completion
		// so commands like `worktree move <task> <dest>` can complete paths.
		return nil, cobra.ShellCompDirectiveDefault
	}
	cfg, err := config.LoadUnvalidated(paths.Config())
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names := make([]string, 0, len(cfg.Tasks)+len(cfg.Templates))
	for _, t := range cfg.Tasks {
		names = append(names, t.Name)
	}
	for _, t := range cfg.Templates {
		names = append(names, t.Name)
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}
