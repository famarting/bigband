package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/famarting/bigband/internal/config"
	"github.com/famarting/bigband/internal/paths"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func NewTemplateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "template",
		Short: "Manage reusable job templates",
		Long: `Templates are reusable job definitions stored alongside jobs but
never scheduled. Use them as a starting point for new jobs via
'bigband add --from <template>'.`,
	}
	cmd.AddCommand(
		newTemplateListCmd(),
		newTemplateAddCmd(),
		newTemplateEditCmd(),
		newTemplateRmCmd(),
		newTemplateGetCmd(),
		newTemplateSaveCmd(),
	)
	return cmd
}

func newTemplateEditCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "edit <name>",
		Short:             "Edit a template's YAML in $EDITOR",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeTemplateNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			return editTemplate(args[0])
		},
	}
}

func editTemplate(name string) error {
	data, err := os.ReadFile(paths.Config())
	if err != nil {
		return err
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return err
	}
	templates, _ := raw["templates"].([]any)
	idx := -1
	for i, t := range templates {
		if m, ok := t.(map[string]any); ok && m["name"] == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("template %q not found", name)
	}

	tmplYAML, err := yaml.Marshal(templates[idx])
	if err != nil {
		return err
	}

	edited, err := editPromptInEditor(string(tmplYAML))
	if err != nil {
		return err
	}

	var updated map[string]any
	if err := yaml.Unmarshal([]byte(edited), &updated); err != nil {
		return fmt.Errorf("edited template is not valid YAML: %w", err)
	}
	if got, _ := updated["name"].(string); strings.TrimSpace(got) == "" {
		return fmt.Errorf("template name is required")
	}
	templates[idx] = updated
	raw["templates"] = templates

	out, err := yaml.Marshal(raw)
	if err != nil {
		return err
	}
	if err := os.WriteFile(paths.Config(), out, 0600); err != nil {
		return err
	}
	fmt.Printf("template %q updated\n", updated["name"])
	return nil
}

func newTemplateListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List configured templates",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(paths.Config())
			if err != nil {
				return err
			}
			if len(cfg.Templates) == 0 {
				fmt.Println("no templates configured")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tFOLDER\tSCHEDULE")
			for _, t := range cfg.Templates {
				sched := t.Schedule
				if sched == "" {
					sched = "-"
				}
				folder := t.Folder
				if folder == "" {
					folder = "-"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\n", t.Name, folder, sched)
			}
			return w.Flush()
		},
	}
}

func newTemplateAddCmd() *cobra.Command {
	var from string
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Add a template (interactive wizard)",
		RunE: func(cmd *cobra.Command, args []string) error {
			var seed *config.Job
			if from != "" {
				cfg, err := config.Load(paths.Config())
				if err != nil {
					return err
				}
				j, kind := cfg.FindJobOrTemplate(from)
				if j == nil {
					return fmt.Errorf("no job or template named %q", from)
				}
				fmt.Printf("seeding template from %s %q\n", kind, from)
				seed = j
			}
			return addTemplateWizard(seed)
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "seed wizard fields from an existing job or template")
	cmd.RegisterFlagCompletionFunc("from", completeJobOrTemplateNames)
	return cmd
}

func newTemplateRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "rm <name>",
		Short:             "Remove a template",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeTemplateNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			return removeTemplate(args[0])
		},
	}
}

func newTemplateGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "get <name>",
		Short:             "Print a template's full definition",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeTemplateNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(paths.Config())
			if err != nil {
				return err
			}
			t := cfg.TemplateByName(args[0])
			if t == nil {
				return fmt.Errorf("template %q not found", args[0])
			}
			out, err := yaml.Marshal(t)
			if err != nil {
				return err
			}
			fmt.Print(string(out))
			return nil
		},
	}
}

func newTemplateSaveCmd() *cobra.Command {
	var as string
	cmd := &cobra.Command{
		Use:               "save <job>",
		Short:             "Save an existing job as a template",
		Long:              "Copies a job's fields (minus schedule) into a new template. Use --as to rename.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeJobNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			jobName := args[0]
			tmplName := as
			if tmplName == "" {
				tmplName = jobName
			}
			tmplName, err := normalizeName(tmplName)
			if err != nil {
				return err
			}
			return saveJobAsTemplate(jobName, tmplName)
		},
	}
	cmd.Flags().StringVar(&as, "as", "", "name for the new template (defaults to the job's name)")
	return cmd
}

func addTemplateWizard(seed *config.Job) error {
	r := newReader()
	ask := makeAsk(r)
	askMulti := makeAskMulti(r)

	name, err := askName(r, "Template name (e.g. ci-healthz)")
	if err != nil {
		return err
	}
	if err := validateUniqueName(name); err != nil {
		return err
	}

	defaultFolder := ""
	if seed != nil {
		defaultFolder = seed.Folder
	}
	if defaultFolder == "" {
		if cfg, err := config.Load(paths.Config()); err == nil && cfg.Defaults.Folder != "" {
			defaultFolder = cfg.Defaults.Folder
		}
	}
	folderPrompt := "Folder (absolute path)"
	if defaultFolder != "" {
		folderPrompt = fmt.Sprintf("%s [%s]", folderPrompt, defaultFolder)
	}
	folder, err := ask(folderPrompt)
	if err != nil {
		return err
	}
	if strings.TrimSpace(folder) == "" {
		folder = defaultFolder
	}

	defaultSched := ""
	if seed != nil {
		defaultSched = seed.Schedule
	}
	schedPrompt := "Default schedule (optional, e.g. '@daily')"
	if defaultSched != "" {
		schedPrompt = fmt.Sprintf("%s [%s]", schedPrompt, defaultSched)
	}
	sched, _ := ask(schedPrompt)
	if strings.TrimSpace(sched) == "" {
		sched = defaultSched
	}

	seedPrompt := ""
	if seed != nil {
		seedPrompt = seed.Prompt
	}
	prompt, err := editPromptInEditor(seedPrompt)
	if err != nil {
		return fmt.Errorf("editing prompt: %w", err)
	}
	if strings.TrimSpace(prompt) == "" {
		return fmt.Errorf("prompt is required")
	}

	defaultPreExec := []string{}
	if seed != nil && len(seed.PreExec) > 0 {
		defaultPreExec = seed.PreExec
	}
	preExec, _ := askMulti(fmt.Sprintf("Pre-exec commands [default: %v]", defaultPreExec))
	if len(preExec) == 0 {
		preExec = defaultPreExec
	}

	defaultPostExec := []string{}
	if seed != nil {
		defaultPostExec = seed.PostExec
	}
	postExec, _ := askMulti(fmt.Sprintf("Post-exec commands [default: %v]", defaultPostExec))
	if len(postExec) == 0 {
		postExec = defaultPostExec
	}

	defaultWorktree := true
	if seed != nil && seed.Worktree != nil {
		defaultWorktree = *seed.Worktree
	}
	useWorktree, err := askYesNo(r, "Use a git worktree?", defaultWorktree)
	if err != nil {
		return err
	}

	tmpl := map[string]any{
		"name":     name,
		"folder":   folder,
		"prompt":   prompt,
		"worktree": useWorktree,
	}
	if sched != "" {
		tmpl["schedule"] = sched
	}
	if len(preExec) > 0 {
		tmpl["pre_exec"] = preExec
	}
	if len(postExec) > 0 {
		tmpl["post_exec"] = postExec
	}
	if seed != nil {
		if useWorktree && seed.KeepWorktree != nil {
			tmpl["keep_worktree"] = *seed.KeepWorktree
		}
		if useWorktree && seed.ReuseWorktree != nil {
			tmpl["reuse_worktree"] = *seed.ReuseWorktree
		}
		if seed.Timeout != nil {
			tmpl["timeout"] = seed.Timeout.String()
		}
		if seed.Jitter != nil {
			tmpl["jitter"] = seed.Jitter.String()
		}
	}

	return appendTemplate(tmpl)
}

func appendTemplate(tmpl map[string]any) error {
	cfgPath := paths.Config()

	if _, err := os.Stat(cfgPath); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(paths.Root(), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(cfgPath, []byte("jobs: []\n"), 0600); err != nil {
			return err
		}
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return err
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw == nil {
		raw = map[string]any{}
	}
	templates, _ := raw["templates"].([]any)
	for _, existing := range templates {
		if m, ok := existing.(map[string]any); ok && m["name"] == tmpl["name"] {
			return fmt.Errorf("template %q already exists", tmpl["name"])
		}
	}
	templates = append(templates, tmpl)
	raw["templates"] = templates

	out, err := yaml.Marshal(raw)
	if err != nil {
		return err
	}
	if err := os.WriteFile(cfgPath, out, 0600); err != nil {
		return err
	}
	fmt.Printf("Template %q added to %s\n", tmpl["name"], cfgPath)
	return nil
}

func removeTemplate(name string) error {
	data, err := os.ReadFile(paths.Config())
	if err != nil {
		return err
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return err
	}
	templates, _ := raw["templates"].([]any)
	filtered := templates[:0]
	found := false
	for _, t := range templates {
		m, ok := t.(map[string]any)
		if ok && m["name"] == name {
			found = true
			continue
		}
		filtered = append(filtered, t)
	}
	if !found {
		return fmt.Errorf("template %q not found", name)
	}
	raw["templates"] = filtered
	out, err := yaml.Marshal(raw)
	if err != nil {
		return err
	}
	if err := os.WriteFile(paths.Config(), out, 0600); err != nil {
		return err
	}
	fmt.Printf("removed template %q\n", name)
	return nil
}

func saveJobAsTemplate(jobName, tmplName string) error {
	data, err := os.ReadFile(paths.Config())
	if err != nil {
		return err
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return err
	}
	jobs, _ := raw["jobs"].([]any)
	var src map[string]any
	for _, j := range jobs {
		if m, ok := j.(map[string]any); ok && m["name"] == jobName {
			src = m
			break
		}
	}
	if src == nil {
		return fmt.Errorf("job %q not found", jobName)
	}
	for _, j := range jobs {
		if m, ok := j.(map[string]any); ok && m["name"] == tmplName {
			return fmt.Errorf("a job named %q already exists; pass --as <name> to use a different template name", tmplName)
		}
	}

	// Copy fields except name (renamed) and schedule (templates strip schedule
	// to avoid surprise scheduling on instantiation; user can opt back in).
	tmpl := map[string]any{}
	for k, v := range src {
		if k == "name" || k == "schedule" || k == "enabled" {
			continue
		}
		tmpl[k] = v
	}
	tmpl["name"] = tmplName

	templates, _ := raw["templates"].([]any)
	for _, existing := range templates {
		if m, ok := existing.(map[string]any); ok && m["name"] == tmplName {
			return fmt.Errorf("template %q already exists", tmplName)
		}
	}
	templates = append(templates, tmpl)
	raw["templates"] = templates

	out, err := yaml.Marshal(raw)
	if err != nil {
		return err
	}
	if err := os.WriteFile(paths.Config(), out, 0600); err != nil {
		return err
	}
	fmt.Printf("saved job %q as template %q\n", jobName, tmplName)
	return nil
}
