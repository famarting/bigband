package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/famarting/bigband/internal/schedule"
	"gopkg.in/yaml.v3"
)

var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9\-_]*$`)

// IsValidName reports whether s is a syntactically valid task or template name.
func IsValidName(s string) bool { return validName.MatchString(s) }

// Slugify returns a best-effort lowercase, dash-separated form of s suitable
// for use as a task name. Returns the empty string if no valid characters
// remain.
func Slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevDash := true // suppress leading dashes
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case r == '-' || r == '_':
			b.WriteRune(r)
			prevDash = (r == '-')
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.TrimRight(b.String(), "-_")
	return out
}

// Config is the top-level config file structure.
type Config struct {
	Defaults  Defaults `yaml:"defaults"`
	Templates []*Task  `yaml:"templates,omitempty"`
	Tasks     []*Task  `yaml:"tasks"`
}

// Defaults holds cluster-level defaults.
type Defaults struct {
	Shell      string   `yaml:"shell"`
	Timeout    Duration `yaml:"timeout"`
	RetainLogs int      `yaml:"retain_logs"`
	Jitter     Duration `yaml:"jitter"`
	Model      string   `yaml:"model"`
	Effort     string   `yaml:"effort"`
	Folder     string   `yaml:"folder"`
	PreExec    []string `yaml:"pre_exec"`
	// EphemeralRetention is how long IPC-submitted one-off task entries
	// (state + log dirs) are kept after their last run. Zero or unset
	// disables auto-pruning. Configured tasks (those in tasks:) are never
	// touched. Default: 168h (7 days).
	EphemeralRetention Duration `yaml:"ephemeral_retention"`
}

// Task is a single scheduled Claude Code job.
type Task struct {
	Name             string    `yaml:"name"`
	Schedule         string    `yaml:"schedule"`
	Folder           string    `yaml:"folder"`
	Enabled          *bool     `yaml:"enabled"`
	Worktree         *bool     `yaml:"worktree"`
	KeepWorktree     *bool     `yaml:"keep_worktree"`
	ReuseWorktree    *bool     `yaml:"reuse_worktree"`
	PreExec          []string  `yaml:"pre_exec"`
	PostExec         []string  `yaml:"post_exec"`
	Prompt           string    `yaml:"prompt"`
	Timeout          *Duration `yaml:"timeout"`
	Jitter           *Duration `yaml:"jitter"`
	Model            string    `yaml:"model"`
	Effort           string    `yaml:"effort"`
	ExtraClaudeFlags []string  `yaml:"extra_claude_flags"`

	// Resolved fields — populated after Validate.
	cronExpr       string
	jitterResolved time.Duration

	// In-memory only fields (never persisted to YAML).

	// ResumeSessionID, when non-empty, makes the runner pass --resume <id> to
	// the first claude invocation. Set by IPC submit for follow-ups.
	ResumeSessionID string `yaml:"-"`
	// Ephemeral marks a task that was constructed in-memory (e.g. via IPC
	// submit) and must never be written back to config.yaml.
	Ephemeral bool `yaml:"-"`
	// TriggeredBy is a free-form label describing what caused this run
	// (e.g. "slack:thread:123"). Surfaced in events for traceability.
	TriggeredBy string `yaml:"-"`
	// RunTimestamp pins the timestamp the runner uses for this run's log
	// filename and run id. Set by IPC submit so the synchronously-returned
	// run id matches the run id later carried on lifecycle events. Empty for
	// scheduler-driven runs (the runner generates one at start).
	RunTimestamp string `yaml:"-"`
}

// IsOneOff returns true when the task has no schedule and fires exactly once.
func (t *Task) IsOneOff() bool { return t.Schedule == "" }

// ShouldUseWorktree returns true when the task should run inside a git
// worktree. Defaults to true for backwards compatibility — set worktree: false
// to run directly inside task.Folder instead.
func (t *Task) ShouldUseWorktree() bool {
	if t.Worktree != nil {
		return *t.Worktree
	}
	return true
}

// ShouldKeepWorktree returns true when the worktree should be preserved after
// the run. Defaults to true — the worktree stays for inspection until the next
// run starts, at which point CreateOrReplace discards it and creates a fresh
// snapshot of HEAD. Set keep_worktree: false to remove it at run end instead.
func (t *Task) ShouldKeepWorktree() bool {
	if t.KeepWorktree != nil {
		return *t.KeepWorktree
	}
	return true || t.ShouldReuseWorktree()
}

// ShouldReuseWorktree returns true when an existing worktree should be reused
// as-is rather than replaced with a fresh snapshot of HEAD.
func (t *Task) ShouldReuseWorktree() bool {
	return t.ReuseWorktree != nil && *t.ReuseWorktree
}

// WorktreeMode returns a short label summarising how this task uses worktrees:
//   - ""          when the task runs directly in its folder
//   - "ephemeral" when a fresh worktree is created and removed each run
//   - "fresh"     when a fresh worktree is created each run and kept afterwards
//   - "reused"    when an existing worktree is reused across runs
func (t *Task) WorktreeMode() string {
	if !t.ShouldUseWorktree() {
		return ""
	}
	if t.ShouldReuseWorktree() {
		return "reused"
	}
	if t.ShouldKeepWorktree() {
		return "fresh"
	}
	return "ephemeral"
}

// CronExpr returns the resolved cron expression (populated after Validate).
func (t *Task) CronExpr() string { return t.cronExpr }

// JitterDuration returns the resolved jitter (populated after Validate).
func (t *Task) JitterDuration() time.Duration { return t.jitterResolved }

func (t *Task) ClearJitter() {
	t.jitterResolved = 0
	t.Jitter = nil
}

// IsEnabled returns true when the task is enabled (defaults true).
func (t *Task) IsEnabled() bool {
	return t.Enabled == nil || *t.Enabled
}

// Load parses the config at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	return parse(data)
}

// LoadUnvalidated parses the config without running Validate. It's used by
// shell-completion functions, which only need the list of names and shouldn't
// silently disappear when an unrelated validation error breaks Load.
func LoadUnvalidated(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	cfg := &Config{}
	cfg.Defaults = defaultDefaults()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	return cfg, nil
}

func parse(data []byte) (*Config, error) {
	cfg := &Config{}
	cfg.Defaults = defaultDefaults()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func defaultDefaults() Defaults {
	return Defaults{
		Shell:              "/bin/sh",
		Timeout:            Duration{45 * time.Minute},
		RetainLogs:         50,
		Jitter:             Duration{15 * time.Minute},
		EphemeralRetention: Duration{7 * 24 * time.Hour},
	}
}

// coreClaudeFlags are the flags bigband requires for its output parsing.
var coreClaudeFlags = []string{"-p", "--output-format", "stream-json", "--verbose", "--include-partial-messages"}

// Validate resolves and validates all tasks.
func (c *Config) Validate() error {
	if c.Defaults.Shell == "" {
		c.Defaults.Shell = "/bin/sh"
	}
	if c.Defaults.RetainLogs == 0 {
		c.Defaults.RetainLogs = 50
	}
	if c.Defaults.Timeout.Duration == 0 {
		c.Defaults.Timeout = Duration{45 * time.Minute}
	}
	templateNames := map[string]bool{}
	for i, t := range c.Templates {
		if t.Name == "" {
			return fmt.Errorf("template[%d]: name is required", i)
		}
		if !validName.MatchString(t.Name) {
			return fmt.Errorf("template %q: name must match [a-z0-9][a-z0-9-_]*", t.Name)
		}
		if templateNames[t.Name] {
			return fmt.Errorf("template %q: duplicate name", t.Name)
		}
		templateNames[t.Name] = true
		if t.Schedule != "" {
			if _, _, err := schedule.Parse(t.Schedule); err != nil {
				return fmt.Errorf("template %q schedule %q: %w", t.Name, t.Schedule, err)
			}
		}
		if strings.TrimSpace(t.Prompt) == "" {
			return fmt.Errorf("template %q: prompt is required", t.Name)
		}
	}

	seen := map[string]bool{}
	for i, t := range c.Tasks {
		if t.Name == "" {
			return fmt.Errorf("task[%d]: name is required", i)
		}
		if !validName.MatchString(t.Name) {
			return fmt.Errorf("task %q: name must match [a-z0-9][a-z0-9-_]*", t.Name)
		}
		if seen[t.Name] {
			return fmt.Errorf("task %q: duplicate name", t.Name)
		}
		if templateNames[t.Name] {
			return fmt.Errorf("task %q: name collides with template of the same name", t.Name)
		}
		seen[t.Name] = true

		if t.Schedule != "" {
			parsed, hasJitter, err := schedule.Parse(t.Schedule)
			if err != nil {
				return fmt.Errorf("task %q schedule %q: %w", t.Name, t.Schedule, err)
			}
			t.cronExpr = parsed
			if hasJitter || (t.Jitter != nil && t.Jitter.Duration > 0) {
				if t.Jitter != nil && t.Jitter.Duration > 0 {
					t.jitterResolved = t.Jitter.Duration
				} else {
					t.jitterResolved = c.Defaults.Jitter.Duration
				}
			}
		}

		if t.Folder == "" {
			return fmt.Errorf("task %q: folder is required", t.Name)
		}
		info, err := os.Stat(t.Folder)
		if err != nil {
			return fmt.Errorf("task %q folder %q: %w", t.Name, t.Folder, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("task %q folder %q: not a directory", t.Name, t.Folder)
		}
		if strings.TrimSpace(t.Prompt) == "" {
			return fmt.Errorf("task %q: prompt is required", t.Name)
		}
	}
	return nil
}

// Save writes the config back to path in YAML.
func (c *Config) Save(path string) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// TaskByName returns the task with the given name or nil.
func (c *Config) TaskByName(name string) *Task {
	for _, t := range c.Tasks {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// TemplateByName returns the template with the given name or nil.
func (c *Config) TemplateByName(name string) *Task {
	for _, t := range c.Templates {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// FindTaskOrTemplate returns the task or template with the given name, and a
// label indicating which it was ("task" or "template"). Returns nil if neither
// matches.
func (c *Config) FindTaskOrTemplate(name string) (*Task, string) {
	if t := c.TaskByName(name); t != nil {
		return t, "task"
	}
	if t := c.TemplateByName(name); t != nil {
		return t, "template"
	}
	return nil, ""
}

// EffectiveClaudeFlags returns the merged flag list for a task.
// Core flags are always included. Model and effort follow task > global > omit.
func (c *Config) EffectiveClaudeFlags(t *Task) []string {
	flags := append([]string{}, coreClaudeFlags...)
	model := t.Model
	if model == "" {
		model = c.Defaults.Model
	}
	if model != "" {
		flags = append(flags, "--model", model)
	}
	effort := t.Effort
	if effort == "" {
		effort = c.Defaults.Effort
	}
	if effort != "" {
		flags = append(flags, "--effort", effort)
	}
	return append(flags, t.ExtraClaudeFlags...)
}

// EffectiveTimeout returns the task timeout, falling back to the default.
func (c *Config) EffectiveTimeout(t *Task) time.Duration {
	if t.Timeout != nil && t.Timeout.Duration > 0 {
		return t.Timeout.Duration
	}
	return c.Defaults.Timeout.Duration
}

// EffectiveShell returns the shell to use.
func (c *Config) EffectiveShell() string {
	if c.Defaults.Shell != "" {
		return c.Defaults.Shell
	}
	return "/bin/sh"
}

// Duration is a time.Duration that marshals/unmarshals as a human string.
type Duration struct{ time.Duration }

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	dur, err := time.ParseDuration(value.Value)
	if err != nil {
		return err
	}
	d.Duration = dur
	return nil
}
