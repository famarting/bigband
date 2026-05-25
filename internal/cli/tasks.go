package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/famarting/bigband/internal/config"
	"github.com/famarting/bigband/internal/ipc"
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
		Short: "Add a new job (interactive wizard)",
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
				fmt.Printf("seeding from %s %q\n", kind, from)
				seed = j
			}
			return addJobWizard(seed)
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "seed wizard fields from an existing job or template")
	cmd.RegisterFlagCompletionFunc("from", completeJobOrTemplateNames)
	return cmd
}

func addJobWizard(seed *config.Job) error {
	r := newReader()
	ask := makeAsk(r)
	askMulti := makeAskMulti(r)

	name, err := askName(r, "Job name (e.g. morning-triage)")
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
		fmt.Println("  → one-off job: will fire immediately when added")
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

	defaultWorktree := true
	if seed != nil && seed.Worktree != nil {
		defaultWorktree = *seed.Worktree
	}
	useWorktree, err := askYesNo(r, "Use a git worktree?", defaultWorktree)
	if err != nil {
		return err
	}

	var defaultTimeout time.Duration
	if seed != nil && seed.Timeout != nil {
		defaultTimeout = seed.Timeout.Duration
	} else if cfg, err := config.Load(paths.Config()); err == nil {
		defaultTimeout = cfg.Defaults.Timeout.Duration
	}
	timeoutPrompt := "Timeout (e.g. 30m, 2h)"
	if defaultTimeout > 0 {
		timeoutPrompt = fmt.Sprintf("%s [%s]", timeoutPrompt, defaultTimeout)
	}
	var timeoutStr string
	for {
		raw, err := ask(timeoutPrompt)
		if err != nil {
			return err
		}
		raw = strings.TrimSpace(raw)
		if raw == "" {
			break
		}
		if _, err := time.ParseDuration(raw); err != nil {
			fmt.Printf("  invalid duration: %v\n", err)
			continue
		}
		timeoutStr = raw
		break
	}

	// Agent provider — blank inherits defaults.agent (or DefaultAgent when that
	// is also unset). We don't validate against the registry here to avoid
	// pulling agent registrations into the config package; an unknown name
	// surfaces at run time as a clear "no provider registered as X" error.
	defaultAgent := ""
	if seed != nil && seed.Agent != "" {
		defaultAgent = seed.Agent
	} else if cfg, err := config.Load(paths.Config()); err == nil && cfg.Defaults.Agent != "" {
		defaultAgent = cfg.Defaults.Agent
	}
	agentPrompt := "Agent (e.g. claude, claude-pty)"
	if defaultAgent != "" {
		agentPrompt = fmt.Sprintf("%s [%s]", agentPrompt, defaultAgent)
	} else {
		agentPrompt = fmt.Sprintf("%s [inherit default: %s]", agentPrompt, config.DefaultAgent)
	}
	agentChoice, err := ask(agentPrompt)
	if err != nil {
		return err
	}
	agentChoice = strings.TrimSpace(agentChoice)

	job := map[string]any{
		"name":    name,
		"folder":  folder,
		"enabled": true,
		"prompt":  prompt,
	}
	if !isOneOff {
		job["schedule"] = sched
	}
	if len(preExec) > 0 {
		job["pre_exec"] = preExec
	}
	if len(postExec) > 0 {
		job["post_exec"] = postExec
	}
	if timeoutStr != "" {
		job["timeout"] = timeoutStr
	}
	if agentChoice != "" {
		job["agent"] = agentChoice
	}
	job["worktree"] = useWorktree
	if seed != nil {
		if useWorktree && seed.KeepWorktree != nil {
			job["keep_worktree"] = *seed.KeepWorktree
		}
		if useWorktree && seed.ReuseWorktree != nil {
			job["reuse_worktree"] = *seed.ReuseWorktree
		}
		if seed.Jitter != nil {
			job["jitter"] = seed.Jitter.String()
		}
		if len(seed.ExtraClaudeFlags) > 0 {
			job["extra_claude_flags"] = seed.ExtraClaudeFlags
		}
	}

	fmt.Println()
	fmt.Println("Job to create:")
	fmt.Printf("  name:     %s\n", name)
	if !isOneOff {
		fmt.Printf("  schedule: %s\n", sched)
	} else {
		fmt.Printf("  schedule: (one-off, fires immediately)\n")
	}
	fmt.Printf("  folder:   %s\n", folder)
	fmt.Printf("  worktree: %t\n", useWorktree)
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
	if timeoutStr != "" {
		fmt.Printf("  timeout:  %s\n", timeoutStr)
	} else if defaultTimeout > 0 {
		fmt.Printf("  timeout:  %s (default)\n", defaultTimeout)
	}
	switch {
	case agentChoice != "":
		fmt.Printf("  agent:    %s\n", agentChoice)
	case defaultAgent != "":
		fmt.Printf("  agent:    %s (default)\n", defaultAgent)
	default:
		fmt.Printf("  agent:    %s (built-in default)\n", config.DefaultAgent)
	}
	fmt.Println()

	ok, err := askConfirm(r, "Create this job?")
	if err != nil {
		return err
	}
	if !ok {
		fmt.Println("Aborted.")
		return nil
	}

	if err := appendJob(job); err != nil {
		return err
	}
	if isOneOff {
		return waitAndFollowLog(name, "")
	}
	// First-run hint: if the daemon isn't reachable, the user just defined a
	// scheduled job that nothing will execute. Tell them how to fix it.
	if reply, err := ipc.Send(ipc.Cmd{Action: "ping"}); err != nil || reply == nil || !reply.OK {
		fmt.Println()
		fmt.Println("The bigband daemon doesn't appear to be running yet.")
		fmt.Println("Next: `bigband install` to start it as a LaunchAgent (auto-starts on login).")
	}
	return nil
}

// appendJob appends a raw job map to the config file.
func appendJob(job map[string]any) error {
	cfgPath := paths.Config()

	// Ensure the config file exists with at least an empty jobs list.
	if _, err := os.Stat(cfgPath); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(paths.Root(), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(cfgPath, []byte("jobs: []\n"), 0600); err != nil {
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
	jobs, _ := raw["jobs"].([]any)
	jobs = append(jobs, job)
	raw["jobs"] = jobs

	out, err := yaml.Marshal(raw)
	if err != nil {
		return err
	}
	if err := os.WriteFile(cfgPath, out, 0600); err != nil {
		return err
	}
	fmt.Printf("Job %q added to %s\n", job["name"], cfgPath)
	return nil
}

func NewEditCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "edit [name]",
		Short:             "Edit a job (or the whole config) in $EDITOR",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeConfiguredJobNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return openEditor(paths.Config())
			}
			return editJob(args[0])
		},
	}
}

func editJob(name string) error {
	data, err := os.ReadFile(paths.Config())
	if err != nil {
		return err
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return err
	}
	jobs, _ := raw["jobs"].([]any)
	jobIdx := -1
	for i, j := range jobs {
		if m, ok := j.(map[string]any); ok && m["name"] == name {
			jobIdx = i
			break
		}
	}
	if jobIdx < 0 {
		return fmt.Errorf("job %q not found", name)
	}

	jobYAML, err := yaml.Marshal(jobs[jobIdx])
	if err != nil {
		return err
	}

	edited, err := editPromptInEditor(string(jobYAML))
	if err != nil {
		return err
	}

	var updated map[string]any
	if err := yaml.Unmarshal([]byte(edited), &updated); err != nil {
		return fmt.Errorf("edited job is not valid YAML: %w", err)
	}
	jobs[jobIdx] = updated
	raw["jobs"] = jobs

	out, err := yaml.Marshal(raw)
	if err != nil {
		return err
	}
	return os.WriteFile(paths.Config(), out, 0600)
}

func NewRmCmd() *cobra.Command {
	var purgeLogs bool
	cmd := &cobra.Command{
		Use:               "rm <name>",
		Short:             "Remove a job and clean up its worktree (and state, for ephemeral one-offs)",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeJobNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			return closeJob(args[0], purgeLogs)
		},
	}
	cmd.Flags().BoolVar(&purgeLogs, "purge-logs", false, "also delete the job's logs directory")
	return cmd
}

func removeJob(name string) error {
	data, err := os.ReadFile(paths.Config())
	if err != nil {
		return err
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return err
	}
	jobs, _ := raw["jobs"].([]any)
	filtered := jobs[:0]
	found := false
	for _, j := range jobs {
		m, ok := j.(map[string]any)
		if ok && m["name"] == name {
			found = true
			continue
		}
		filtered = append(filtered, j)
	}
	if !found {
		return fmt.Errorf("job %q not found", name)
	}
	raw["jobs"] = filtered
	out, err := yaml.Marshal(raw)
	if err != nil {
		return err
	}
	return os.WriteFile(paths.Config(), out, 0600)
}

func NewEnableCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "enable <name>",
		Short:             "Enable a job",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeConfiguredJobNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			return setEnabled(args[0], true)
		},
	}
}

func NewDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "disable <name>",
		Short:             "Disable a job",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeConfiguredJobNames,
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
	jobs, _ := raw["jobs"].([]any)
	found := false
	for _, j := range jobs {
		m, ok := j.(map[string]any)
		if ok && m["name"] == name {
			m["enabled"] = enabled
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("job %q not found", name)
	}
	raw["jobs"] = jobs
	out, err := yaml.Marshal(raw)
	if err != nil {
		return err
	}
	return os.WriteFile(paths.Config(), out, 0600)
}

func closeJob(name string, purgeLogs bool) error {
	st, err := state.Load()
	if err != nil {
		return err
	}
	js := st.Get(name)

	cfg, _ := config.Load(paths.Config())
	var configured *config.Job
	if cfg != nil {
		configured = cfg.JobByName(name)
	}
	hasState := js.LastRun != nil || js.SessionID != "" || js.WorktreePath != "" || js.Folder != "" || js.RunningPID != 0
	if configured == nil && !hasState {
		return fmt.Errorf("job %q not found", name)
	}

	// Refuse to remove a job with a live in-flight run.
	if js.RunningPID != 0 && processAlive(js.RunningPID) {
		return fmt.Errorf("job %q is still running (pid %d) — stop it first with `bigband stop %s`", name, js.RunningPID, name)
	}

	// Worktree cleanup. Resolve the main repo root from whichever folder we
	// know about. Falls back to `os.RemoveAll` when no repo can be located —
	// preserves disk-space recovery even for orphaned legacy entries.
	if js.WorktreePath != "" {
		sourceFolder := ""
		if configured != nil {
			sourceFolder = configured.Folder
		}
		if sourceFolder == "" {
			sourceFolder = js.Folder
		}
		repoRoot := ""
		if sourceFolder != "" {
			if root, err := worktree.RepoRoot(sourceFolder); err == nil {
				repoRoot = root
			}
		}
		if repoRoot != "" {
			if err := worktree.Remove(repoRoot, js.WorktreePath); err != nil {
				fmt.Printf("warning: could not remove worktree %s: %v\n", js.WorktreePath, err)
			} else {
				fmt.Printf("removed worktree %s\n", js.WorktreePath)
			}
		} else if _, err := os.Stat(js.WorktreePath); err == nil {
			// We can't resolve the repo root, so we can't safely run
			// `git worktree remove` or fall back to `os.RemoveAll` — without a
			// repo root we cannot prove the path is a bigband-owned sibling
			// (`<repo>-bb-<job>`). A basename-only heuristic would still accept
			// unrelated paths that happen to contain "-bb-" (e.g. an fws feature
			// workspace whose branch name includes those characters), so refuse
			// outright and let the user clean up manually if they're sure.
			fmt.Printf("warning: refusing to remove worktree %q — cannot resolve its repo root, so bigband can't prove the path is one of its own worktrees.\n", js.WorktreePath)
			fmt.Printf("         Inspect the path; if it really is a stale bigband worktree, delete it manually and run `bigband rm` again.\n")
		}
	}

	// Config removal — only when the job was actually in config.yaml.
	if configured != nil {
		if err := removeJob(name); err != nil {
			return err
		}
		fmt.Printf("removed %q from config.yaml\n", name)
	}

	// State entry removal. Route through the daemon when it's running so its
	// in-memory state map is updated too — otherwise the daemon would clobber
	// our deletion on the next state save (SetRunning, SetDone, etc.) and the
	// entry would reappear in `bb list` / `bb status`.
	if hasState {
		reply, ipcErr := ipc.Send(ipc.Cmd{Action: "forget", Job: name})
		switch {
		case ipcErr != nil:
			// Daemon unreachable — safe to edit state.json directly.
			if err := st.RemoveJob(name); err != nil {
				fmt.Printf("warning: could not remove state entry: %v\n", err)
			} else {
				fmt.Printf("removed state entry for %q (daemon offline)\n", name)
			}
		case !reply.OK:
			fmt.Printf("warning: daemon refused state removal: %s\n", reply.Error)
		default:
			fmt.Printf("removed state entry for %q\n", name)
		}
	}

	// Optional log directory purge.
	if purgeLogs {
		dir := paths.JobLogDir(name)
		if _, err := os.Stat(dir); err == nil {
			if err := os.RemoveAll(dir); err != nil {
				fmt.Printf("warning: could not remove logs %s: %v\n", dir, err)
			} else {
				fmt.Printf("removed logs %s\n", dir)
			}
		}
	} else {
		dir := paths.JobLogDir(name)
		if _, err := os.Stat(dir); err == nil {
			fmt.Printf("logs preserved at %s (use --purge-logs to delete)\n", dir)
		}
	}

	return nil
}

// processAlive reports whether a pid corresponds to a running process. Used
// to refuse `bigband rm` on jobs with a live runner.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
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
