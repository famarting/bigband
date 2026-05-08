package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/famarting/bigband/internal/config"
	"github.com/famarting/bigband/internal/paths"
	"github.com/famarting/bigband/internal/schedule"
	"github.com/famarting/bigband/internal/state"
	"github.com/famarting/bigband/internal/worktree"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func NewAddCmd() *cobra.Command {
	var from string
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Add a new task (interactive wizard)",
		RunE: func(cmd *cobra.Command, args []string) error {
			var seed *config.Task
			if from != "" {
				cfg, err := config.Load(paths.Config())
				if err != nil {
					return err
				}
				t, kind := cfg.FindTaskOrTemplate(from)
				if t == nil {
					return fmt.Errorf("no task or template named %q", from)
				}
				fmt.Printf("seeding from %s %q\n", kind, from)
				seed = t
			}
			return addTaskWizard(seed)
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "seed wizard fields from an existing task or template")
	cmd.RegisterFlagCompletionFunc("from", completeTaskOrTemplateNames)
	return cmd
}

func addTaskWizard(seed *config.Task) error {
	r := newReader()
	ask := makeAsk(r)
	askMulti := makeAskMulti(r)

	name, err := askName(r, "Task name (e.g. morning-triage)")
	if err != nil {
		return err
	}
	if err := validateUniqueName(name); err != nil {
		return err
	}

	defaultSched := ""
	if seed != nil {
		defaultSched = seed.Schedule
	}
	schedPrompt := "Schedule (e.g. 'Weekdays at ~20:00', '@every 1h', '0 8 * * *') — leave blank for one-off"
	if defaultSched != "" {
		schedPrompt = fmt.Sprintf("%s [%s]", schedPrompt, defaultSched)
	}
	sched, err := ask(schedPrompt)
	if err != nil {
		return err
	}
	if strings.TrimSpace(sched) == "" {
		sched = defaultSched
	}
	isOneOff := strings.TrimSpace(sched) == ""
	if !isOneOff {
		cronExpr, hasJitter, err := schedule.Parse(sched)
		if err != nil {
			return fmt.Errorf("invalid schedule: %w", err)
		}
		fmt.Printf("  → cron: %s", cronExpr)
		if hasJitter {
			fmt.Print(" (with jitter)")
		}
		fmt.Println()
	} else {
		fmt.Println("  → one-off task: will fire immediately when added")
	}

	defaultFolder, _ := os.Getwd()
	if seed != nil && seed.Folder != "" {
		defaultFolder = seed.Folder
	} else if cfg, err := config.Load(paths.Config()); err == nil && cfg.Defaults.Folder != "" {
		defaultFolder = cfg.Defaults.Folder
	}
	folder, err := ask(fmt.Sprintf("Folder (absolute path) [%s]", defaultFolder))
	if err != nil {
		return err
	}
	if strings.TrimSpace(folder) == "" {
		folder = defaultFolder
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
	} else if cfg, err := config.Load(paths.Config()); err == nil && len(cfg.Defaults.PreExec) > 0 {
		defaultPreExec = cfg.Defaults.PreExec
	}
	preExec, _ := askMulti(fmt.Sprintf("Pre-exec commands [default: %v]", defaultPreExec))
	if len(preExec) == 0 {
		preExec = defaultPreExec
	}
	defaultPostExec := []string{}
	if seed != nil {
		defaultPostExec = seed.PostExec
	}
	postPrompt := "Post-exec commands"
	if len(defaultPostExec) > 0 {
		postPrompt = fmt.Sprintf("%s [default: %v]", postPrompt, defaultPostExec)
	}
	postExec, _ := askMulti(postPrompt)
	if len(postExec) == 0 {
		postExec = defaultPostExec
	}

	task := map[string]any{
		"name":    name,
		"folder":  folder,
		"enabled": true,
		"prompt":  prompt,
	}
	if !isOneOff {
		task["schedule"] = sched
	}
	if len(preExec) > 0 {
		task["pre_exec"] = preExec
	}
	if len(postExec) > 0 {
		task["post_exec"] = postExec
	}
	if seed != nil {
		if seed.KeepWorktree != nil {
			task["keep_worktree"] = *seed.KeepWorktree
		}
		if seed.ReuseWorktree != nil {
			task["reuse_worktree"] = *seed.ReuseWorktree
		}
		if seed.Timeout != nil {
			task["timeout"] = seed.Timeout.String()
		}
		if seed.Jitter != nil {
			task["jitter"] = seed.Jitter.String()
		}
		if len(seed.ExtraClaudeFlags) > 0 {
			task["extra_claude_flags"] = seed.ExtraClaudeFlags
		}
	}

	fmt.Println()
	fmt.Println("Task to create:")
	fmt.Printf("  name:     %s\n", name)
	if !isOneOff {
		fmt.Printf("  schedule: %s\n", sched)
	} else {
		fmt.Printf("  schedule: (one-off, fires immediately)\n")
	}
	fmt.Printf("  folder:   %s\n", folder)
	promptPreview := strings.TrimSpace(prompt)
	if nl := strings.IndexByte(promptPreview, '\n'); nl >= 0 {
		promptPreview = promptPreview[:nl] + " …"
	} else if len(promptPreview) > 80 {
		promptPreview = promptPreview[:80] + " …"
	}
	fmt.Printf("  prompt:   %s\n", promptPreview)
	if len(preExec) > 0 {
		fmt.Printf("  pre_exec: %v\n", preExec)
	}
	if len(postExec) > 0 {
		fmt.Printf("  post_exec: %v\n", postExec)
	}
	fmt.Println()

	ok, err := askConfirm(r, "Create this task?")
	if err != nil {
		return err
	}
	if !ok {
		fmt.Println("Aborted.")
		return nil
	}

	if err := appendTask(task); err != nil {
		return err
	}
	if isOneOff {
		return waitAndFollowLog(name, "")
	}
	return nil
}

// appendTask appends a raw task map to the config file.
func appendTask(task map[string]any) error {
	cfgPath := paths.Config()

	// Ensure the config file exists with at least an empty tasks list.
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		if err := os.MkdirAll(paths.Root(), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(cfgPath, []byte("tasks: []\n"), 0600); err != nil {
			return err
		}
	}

	// Read raw YAML.
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
	tasks, _ := raw["tasks"].([]any)
	tasks = append(tasks, task)
	raw["tasks"] = tasks

	out, err := yaml.Marshal(raw)
	if err != nil {
		return err
	}
	if err := os.WriteFile(cfgPath, out, 0600); err != nil {
		return err
	}
	fmt.Printf("Task %q added to %s\n", task["name"], cfgPath)
	return nil
}

func NewEditCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "edit [name]",
		Short:             "Edit a task (or the whole config) in $EDITOR",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeTaskNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return openEditor(paths.Config())
			}
			return editTask(args[0])
		},
	}
}

func editTask(name string) error {
	data, err := os.ReadFile(paths.Config())
	if err != nil {
		return err
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return err
	}
	tasks, _ := raw["tasks"].([]any)
	taskIdx := -1
	for i, t := range tasks {
		if m, ok := t.(map[string]any); ok && m["name"] == name {
			taskIdx = i
			break
		}
	}
	if taskIdx < 0 {
		return fmt.Errorf("task %q not found", name)
	}

	taskYAML, err := yaml.Marshal(tasks[taskIdx])
	if err != nil {
		return err
	}

	edited, err := editPromptInEditor(string(taskYAML))
	if err != nil {
		return err
	}

	var updated map[string]any
	if err := yaml.Unmarshal([]byte(edited), &updated); err != nil {
		return fmt.Errorf("edited task is not valid YAML: %w", err)
	}
	tasks[taskIdx] = updated
	raw["tasks"] = tasks

	out, err := yaml.Marshal(raw)
	if err != nil {
		return err
	}
	return os.WriteFile(paths.Config(), out, 0600)
}

func NewRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "rm <name>",
		Short:             "Remove a task and clean up its worktree",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeTaskNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			return closeTask(args[0])
		},
	}
}

func removeTask(name string) error {
	data, err := os.ReadFile(paths.Config())
	if err != nil {
		return err
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return err
	}
	tasks, _ := raw["tasks"].([]any)
	filtered := tasks[:0]
	found := false
	for _, t := range tasks {
		m, ok := t.(map[string]any)
		if ok && m["name"] == name {
			found = true
			continue
		}
		filtered = append(filtered, t)
	}
	if !found {
		return fmt.Errorf("task %q not found", name)
	}
	raw["tasks"] = filtered
	out, err := yaml.Marshal(raw)
	if err != nil {
		return err
	}
	return os.WriteFile(paths.Config(), out, 0600)
}

func NewEnableCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "enable <name>",
		Short:             "Enable a task",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeTaskNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			return setEnabled(args[0], true)
		},
	}
}

func NewDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "disable <name>",
		Short:             "Disable a task",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeTaskNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			return setEnabled(args[0], false)
		},
	}
}

func setEnabled(name string, enabled bool) error {
	data, err := os.ReadFile(paths.Config())
	if err != nil {
		return err
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return err
	}
	tasks, _ := raw["tasks"].([]any)
	found := false
	for _, t := range tasks {
		m, ok := t.(map[string]any)
		if ok && m["name"] == name {
			m["enabled"] = enabled
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("task %q not found", name)
	}
	raw["tasks"] = tasks
	out, err := yaml.Marshal(raw)
	if err != nil {
		return err
	}
	return os.WriteFile(paths.Config(), out, 0600)
}

func closeTask(name string) error {
	st, err := state.Load()
	if err != nil {
		return err
	}
	ts := st.Get(name)

	if ts.WorktreePath != "" {
		cfg, err := config.Load(paths.Config())
		if err != nil {
			return err
		}
		t := cfg.TaskByName(name)
		if t != nil {
			if repoRoot, err := worktree.RepoRoot(t.Folder); err == nil {
				if err := worktree.Remove(repoRoot, ts.WorktreePath); err != nil {
					fmt.Printf("warning: could not remove worktree %s: %v\n", ts.WorktreePath, err)
				} else {
					fmt.Printf("removed worktree %s\n", ts.WorktreePath)
				}
			}
		}
		_ = st.SetWorktreePath(name, "")
	}

	if err := removeTask(name); err != nil {
		return err
	}
	fmt.Printf("closed task %q\n", name)
	return nil
}

func editPromptInEditor(initial string) (string, error) {
	f, err := os.CreateTemp("", "bigband-prompt-*.txt")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	if initial != "" {
		f.WriteString(initial)
	}
	f.Close()

	if err := openEditor(f.Name()); err != nil {
		return "", err
	}
	data, err := os.ReadFile(f.Name())
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(data), "\n"), nil
}

func openEditor(path string) error {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "code"
	}
	args := []string{path}
	if editor == "code" {
		args = append([]string{"--wait"}, args...)
	}
	c := exec.Command(editor, args...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}
