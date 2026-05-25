package cli

import (
	"github.com/famarting/bigband/internal/config"
	"github.com/famarting/bigband/internal/paths"
	"github.com/famarting/bigband/internal/state"
	"github.com/spf13/cobra"
)

// completeJobNames is a ValidArgsFunction that completes configured job
// names plus any ephemeral one-offs that exist in state. Ephemerals are
// deduped against configured names.
func completeJobNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		// Beyond the name position, fall through to default (file) completion
		// so commands like `worktree move <job> <dest>` can complete paths.
		return nil, cobra.ShellCompDirectiveDefault
	}
	cfg, _ := config.LoadUnvalidated(paths.Config())
	seen := map[string]bool{}
	var names []string
	if cfg != nil {
		for _, j := range cfg.Jobs {
			if !seen[j.Name] {
				seen[j.Name] = true
				names = append(names, j.Name)
			}
		}
	}
	if st, err := state.Load(); err == nil {
		for n := range st.Jobs {
			if !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}

// completeConfiguredJobNames completes only jobs that exist in config.yaml
// (i.e. excludes ephemeral state-only entries). Use for commands that operate
// on config, such as edit, enable, and disable.
func completeConfiguredJobNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveDefault
	}
	cfg, _ := config.LoadUnvalidated(paths.Config())
	if cfg == nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names := make([]string, 0, len(cfg.Jobs))
	for _, j := range cfg.Jobs {
		names = append(names, j.Name)
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}

// completeTemplateNames completes configured template names.
func completeTemplateNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		// Beyond the name position, fall through to default (file) completion
		// so commands like `worktree move <job> <dest>` can complete paths.
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

// completeJobOrTemplateNames completes both job and template names.
func completeJobOrTemplateNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		// Beyond the name position, fall through to default (file) completion
		// so commands like `worktree move <job> <dest>` can complete paths.
		return nil, cobra.ShellCompDirectiveDefault
	}
	cfg, err := config.LoadUnvalidated(paths.Config())
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names := make([]string, 0, len(cfg.Jobs)+len(cfg.Templates))
	for _, j := range cfg.Jobs {
		names = append(names, j.Name)
	}
	for _, t := range cfg.Templates {
		names = append(names, t.Name)
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}
