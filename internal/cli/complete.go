package cli

import (
	"github.com/famarting/bigband/internal/config"
	"github.com/famarting/bigband/internal/paths"
	"github.com/famarting/bigband/internal/state"
	"github.com/spf13/cobra"
)

// completeTaskNames is a ValidArgsFunction that completes configured task
// names plus any ephemeral one-offs that exist in state. Ephemerals are
// deduped against configured names.
func completeTaskNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		// Beyond the name position, fall through to default (file) completion
		// so commands like `worktree move <task> <dest>` can complete paths.
		return nil, cobra.ShellCompDirectiveDefault
	}
	cfg, _ := config.LoadUnvalidated(paths.Config())
	seen := map[string]bool{}
	var names []string
	if cfg != nil {
		for _, t := range cfg.Tasks {
			if !seen[t.Name] {
				seen[t.Name] = true
				names = append(names, t.Name)
			}
		}
	}
	if st, err := state.Load(); err == nil {
		for n := range st.Tasks {
			if !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
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
