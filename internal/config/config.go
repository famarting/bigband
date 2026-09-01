package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/famarting/bigband/internal/schedule"
	"github.com/famarting/bigband/internal/worktree"
	"gopkg.in/yaml.v3"
)

// folderOriginResolver returns the absolute primary working tree path for a
// folder (see worktree.OriginPath). Stored as a var so tests can inject a
// pure-path resolver and exercise CheckFolderAllowed without touching git.
var folderOriginResolver = worktree.OriginPath

var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9\-_]*$`)

// IsValidName reports whether s is a syntactically valid job or template name.
func IsValidName(s string) bool { return validName.MatchString(s) }

// Slugify returns a best-effort lowercase, dash-separated form of s suitable
// for use as a job name. Returns the empty string if no valid characters
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
	Templates []*Job   `yaml:"templates,omitempty"`
	Jobs      []*Job   `yaml:"jobs"`
}

// Defaults holds cluster-level defaults.
type Defaults struct {
	Shell      string   `yaml:"shell"`
	Timeout    Duration `yaml:"timeout"`
	RetainLogs int      `yaml:"retain_logs"`
	Jitter     Duration `yaml:"jitter"`
	Model      string   `yaml:"model"`
	Effort     string   `yaml:"effort"`
	// Agent selects the default agent provider for jobs that don't set one
	// explicitly. Empty falls back to DefaultAgent ("claude").
	Agent   string   `yaml:"agent,omitempty"`
	Folder  string   `yaml:"folder"`
	PreExec []string `yaml:"pre_exec"`
	// Env is passed to every entry. An entry's own env: overrides these per key.
	Env map[string]string `yaml:"env,omitempty"`
	// EphemeralRetention is how long IPC-submitted one-off job entries
	// (state + log dirs) are kept after their last run. Zero or unset
	// disables auto-pruning. Configured jobs (those in jobs:) are never
	// touched. Default: 168h (7 days).
	EphemeralRetention Duration `yaml:"ephemeral_retention"`
	// AllowedFolders, when non-empty, restricts which directories jobs may
	// run in. Each entry is a directory; a job's folder is permitted when its
	// resolved primary working tree (i.e. the source-of-truth repo even when
	// running inside a linked worktree) is equal to or a descendant of one of
	// these entries. Empty list means no restriction (default).
	AllowedFolders []string `yaml:"allowed_folders,omitempty"`
}

// Job is a single scheduled Claude Code job.
type Job struct {
	Name          string    `yaml:"name"`
	Schedule      string    `yaml:"schedule"`
	Folder        string    `yaml:"folder"`
	Enabled       *bool     `yaml:"enabled"`
	Worktree      *bool     `yaml:"worktree"`
	KeepWorktree  *bool     `yaml:"keep_worktree"`
	ReuseWorktree *bool     `yaml:"reuse_worktree"`
	PreExec       []string  `yaml:"pre_exec"`
	PostExec      []string  `yaml:"post_exec"`
	Prompt        string    `yaml:"prompt"`
	Timeout       *Duration `yaml:"timeout"`
	Jitter        *Duration `yaml:"jitter"`
	Model         string    `yaml:"model"`
	Effort        string    `yaml:"effort"`
	// Agent selects the agent provider for this job. Empty falls back to
	// Defaults.Agent and then to DefaultAgent ("claude").
	Agent string `yaml:"agent,omitempty"`

	// Env is passed to the agent process and to pre_exec/post_exec for this
	// entry, on top of the daemon's own environment. Values here override
	// inherited ones. Use it for per-entry credentials the work needs (a model
	// API key, say) that should not sit in the daemon's environment for
	// everything else. Merged over defaults.env, so one key can be overridden
	// without restating the rest.
	Env map[string]string `yaml:"env,omitempty"`

	// Resolved fields — populated after Validate.
	cronExpr       string
	jitterResolved time.Duration

	// In-memory only fields (never persisted to YAML).

	// ResumeSessionID, when non-empty, makes the runner pass --resume <id> to
	// the first claude invocation. Set by IPC submit for follow-ups.
	ResumeSessionID string `yaml:"-"`
	// Ephemeral marks a job that was constructed in-memory (e.g. via IPC
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

// IsOneOff returns true when the job has no schedule and fires exactly once.
func (j *Job) IsOneOff() bool { return j.Schedule == "" }

// ShouldUseWorktree returns true when the job should run inside a git
// worktree. Defaults to true for backwards compatibility — set worktree: false
// to run directly inside job.Folder instead.
func (j *Job) ShouldUseWorktree() bool {
	if j.Worktree != nil {
		return *j.Worktree
	}
	return true
}

// ShouldKeepWorktree returns true when the worktree should be preserved after
// the run. Defaults to true — the worktree stays for inspection until the next
// run starts, at which point CreateOrReplace discards it and creates a fresh
// snapshot of HEAD. Set keep_worktree: false to remove it at run end instead.
// reuse_worktree implies keep regardless of the explicit setting; without that
// the next run would have nothing to reuse.
func (j *Job) ShouldKeepWorktree() bool {
	if j.ShouldReuseWorktree() {
		return true
	}
	if j.KeepWorktree != nil {
		return *j.KeepWorktree
	}
	return true
}

// ShouldReuseWorktree returns true when an existing worktree should be reused
// as-is rather than replaced with a fresh snapshot of HEAD.
func (j *Job) ShouldReuseWorktree() bool {
	return j.ReuseWorktree != nil && *j.ReuseWorktree
}

// WorktreeMode returns a short label summarising how this job uses worktrees:
//   - ""          when the job runs directly in its folder
//   - "ephemeral" when a fresh worktree is created and removed each run
//   - "fresh"     when a fresh worktree is created each run and kept afterwards
//   - "reused"    when an existing worktree is reused across runs
func (j *Job) WorktreeMode() string {
	if !j.ShouldUseWorktree() {
		return ""
	}
	if j.ShouldReuseWorktree() {
		return "reused"
	}
	if j.ShouldKeepWorktree() {
		return "fresh"
	}
	return "ephemeral"
}

// CronExpr returns the resolved cron expression (populated after Validate).
func (j *Job) CronExpr() string { return j.cronExpr }

// JitterDuration returns the resolved jitter (populated after Validate).
func (j *Job) JitterDuration() time.Duration { return j.jitterResolved }

func (j *Job) ClearJitter() {
	j.jitterResolved = 0
	j.Jitter = nil
}

// IsEnabled returns true when the job is enabled (defaults true).
func (j *Job) IsEnabled() bool {
	return j.Enabled == nil || *j.Enabled
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
		Jitter:             Duration{5 * time.Minute},
		EphemeralRetention: Duration{7 * 24 * time.Hour},
	}
}

// Validate resolves and validates all jobs.
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
	for i, j := range c.Jobs {
		if j.Name == "" {
			return fmt.Errorf("job[%d]: name is required", i)
		}
		if !validName.MatchString(j.Name) {
			return fmt.Errorf("job %q: name must match [a-z0-9][a-z0-9-_]*", j.Name)
		}
		if seen[j.Name] {
			return fmt.Errorf("job %q: duplicate name", j.Name)
		}
		if templateNames[j.Name] {
			return fmt.Errorf("job %q: name collides with template of the same name", j.Name)
		}
		seen[j.Name] = true

		if j.Schedule != "" {
			parsed, hasJitter, err := schedule.Parse(j.Schedule)
			if err != nil {
				return fmt.Errorf("job %q schedule %q: %w", j.Name, j.Schedule, err)
			}
			j.cronExpr = parsed
			if hasJitter || (j.Jitter != nil && j.Jitter.Duration > 0) {
				if j.Jitter != nil && j.Jitter.Duration > 0 {
					j.jitterResolved = j.Jitter.Duration
				} else {
					j.jitterResolved = c.Defaults.Jitter.Duration
				}
			}
		}

		if j.Folder == "" {
			return fmt.Errorf("job %q: folder is required", j.Name)
		}
		expanded, err := ExpandFwsFolder(j.Folder)
		if err != nil {
			return fmt.Errorf("job %q: %w", j.Name, err)
		}
		j.Folder = expanded
		// When the resolved folder is an fws-managed feature workspace, default
		// worktree:false — the workspace is already its own isolation boundary
		// and bigband should run jobs directly inside it rather than nesting
		// another worktree underneath. Explicit worktree: settings are honoured.
		if j.Worktree == nil && IsFwsWorkspace(j.Folder) {
			f := false
			j.Worktree = &f
		}
		info, err := os.Stat(j.Folder)
		if err != nil {
			return fmt.Errorf("job %q folder %q: %w", j.Name, j.Folder, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("job %q folder %q: not a directory", j.Name, j.Folder)
		}
		if err := c.CheckFolderAllowed(j.Folder); err != nil {
			return fmt.Errorf("job %q: %w", j.Name, err)
		}
		if strings.TrimSpace(j.Prompt) == "" {
			return fmt.Errorf("job %q: prompt is required", j.Name)
		}
	}
	return nil
}

// ExpandFwsFolder resolves a folder value of the form `fws:<name>` by shelling
// out to the `fws` CLI. Non-prefixed values pass through unchanged. Returns a
// clear error if `fws` is not on PATH or the workspace name doesn't resolve.
func ExpandFwsFolder(folder string) (string, error) {
	if !strings.HasPrefix(folder, "fws:") {
		return folder, nil
	}
	name := strings.TrimSpace(strings.TrimPrefix(folder, "fws:"))
	if name == "" {
		return "", fmt.Errorf("folder %q: fws: prefix with empty workspace name", folder)
	}
	bin, err := exec.LookPath("fws")
	if err != nil {
		if home, herr := os.UserHomeDir(); herr == nil {
			cand := filepath.Join(home, "bin", "fws")
			if _, sterr := os.Stat(cand); sterr == nil {
				bin = cand
			}
		}
		if bin == "" {
			return "", fmt.Errorf("folder %q: fws CLI not found on PATH or ~/bin (needed to resolve fws:%s)", folder, name)
		}
	}
	out, err := exec.Command(bin, "resolve", name).Output()
	if err != nil {
		return "", fmt.Errorf("folder %q: fws resolve %s failed: %w", folder, name, err)
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		return "", fmt.Errorf("folder %q: fws resolve %s returned empty path", folder, name)
	}
	return path, nil
}

// IsFwsWorkspace reports whether folder is an fws-managed feature workspace,
// detected by the presence of a sidecar JSON file alongside the worktree dir
// (e.g. `~/projects/feature-worktrees/cloudgrid__foo/` has a sibling
// `cloudgrid__foo.json`).
func IsFwsWorkspace(folder string) bool {
	if folder == "" {
		return false
	}
	_, err := os.Stat(folder + ".json")
	return err == nil
}

// CheckFolderAllowed reports whether folder satisfies defaults.allowed_folders.
// When the allowlist is empty the check is a no-op (returns nil). Otherwise
// the folder is resolved to the *primary* working tree (so a worktree of an
// allowed repo is itself allowed) and that path must be equal to or a
// descendant of one of the allowed roots. Returns nil when allowed; a
// descriptive error when denied or when resolution fails.
func (c *Config) CheckFolderAllowed(folder string) error {
	if len(c.Defaults.AllowedFolders) == 0 {
		return nil
	}
	origin, err := folderOriginResolver(folder)
	if err != nil {
		return fmt.Errorf("resolving folder %q: %w", folder, err)
	}
	for _, root := range c.Defaults.AllowedFolders {
		rootResolved, err := resolveAllowedRoot(root)
		if err != nil {
			// Skip an unresolvable allowlist entry rather than failing the
			// check — log-only would be noisier than this. The other entries
			// still get a chance to match.
			continue
		}
		if pathContains(rootResolved, origin) {
			return nil
		}
	}
	return fmt.Errorf("folder %q (origin %q) is not under any defaults.allowed_folders entry", folder, origin)
}

// resolveAllowedRoot canonicalises an allowlist entry: absolute, with symlinks
// resolved when possible. Returns the cleaned absolute path even if the entry
// doesn't currently exist on disk.
func resolveAllowedRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return abs, nil
}

// pathContains reports whether child is equal to parent or located beneath it,
// using lexical comparison after Clean. Both inputs must be absolute. Avoids
// the false-positive in a naive HasPrefix where "/foo/bar" looks like a child
// of "/foo/ba".
func pathContains(parent, child string) bool {
	parent = filepath.Clean(parent)
	child = filepath.Clean(child)
	if parent == child {
		return true
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return !strings.HasPrefix(rel, "..") && rel != ".."
}

// Save writes the config back to path in YAML.
func (c *Config) Save(path string) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// JobByName returns the job with the given name or nil.
func (c *Config) JobByName(name string) *Job {
	for _, j := range c.Jobs {
		if j.Name == name {
			return j
		}
	}
	return nil
}

// TemplateByName returns the template with the given name or nil.
func (c *Config) TemplateByName(name string) *Job {
	for _, t := range c.Templates {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// FindJobOrTemplate returns the job or template with the given name, and a
// label indicating which it was ("job" or "template"). Returns nil if neither
// matches.
func (c *Config) FindJobOrTemplate(name string) (*Job, string) {
	if j := c.JobByName(name); j != nil {
		return j, "job"
	}
	if t := c.TemplateByName(name); t != nil {
		return t, "template"
	}
	return nil, ""
}

// DefaultAgent is the provider name used when neither the job nor the
// top-level defaults specify one.
const DefaultAgent = "claude"

// EffectiveAgent returns the agent provider name for a job: job-level when
// set, otherwise the top-level default, otherwise DefaultAgent.
func (c *Config) EffectiveAgent(j *Job) string {
	if j.Agent != "" {
		return j.Agent
	}
	if c.Defaults.Agent != "" {
		return c.Defaults.Agent
	}
	return DefaultAgent
}

// EffectiveModel returns the model for a job, falling back to the global
// default. Empty when neither is set — the agent provider picks its own
// default in that case.
// EffectiveEnv returns the environment for one entry: defaults.env with the
// entry's own env: merged over it, so a single key can be overridden without
// restating the others. Returns nil when neither sets anything, which callers
// treat as "inherit the daemon's environment unchanged".
func (c *Config) EffectiveEnv(j *Job) map[string]string {
	if len(c.Defaults.Env) == 0 && (j == nil || len(j.Env) == 0) {
		return nil
	}
	out := make(map[string]string, len(c.Defaults.Env)+len(j.Env))
	for k, v := range c.Defaults.Env {
		out[k] = v
	}
	if j != nil {
		for k, v := range j.Env {
			out[k] = v
		}
	}
	return out
}

func (c *Config) EffectiveModel(j *Job) string {
	if j.Model != "" {
		return j.Model
	}
	return c.Defaults.Model
}

// EffectiveEffort returns the thinking/effort budget for a job, falling back
// to the global default. Empty when neither is set.
func (c *Config) EffectiveEffort(j *Job) string {
	if j.Effort != "" {
		return j.Effort
	}
	return c.Defaults.Effort
}

// EffectiveTimeout returns the job timeout, falling back to the default.
func (c *Config) EffectiveTimeout(j *Job) time.Duration {
	if j.Timeout != nil && j.Timeout.Duration > 0 {
		return j.Timeout.Duration
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
