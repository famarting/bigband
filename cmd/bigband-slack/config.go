package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/famarting/bigband/internal/paths"
	"gopkg.in/yaml.v3"
)

// Config is the bigband-slack configuration. Lives at
// ~/.bigband-tasks/extensions/bigband-slack/config.yaml.
//
// Opt-in: an empty Mirror list means no task ever posts to Slack. Empty
// TriggerChannels means no Slack message ever fires a bigband run.
type Config struct {
	Slack           SlackAuth        `yaml:"slack"`
	Mirror          []MirrorRule     `yaml:"mirror,omitempty"`
	TriggerChannels []TriggerChannel `yaml:"trigger_channels,omitempty"`
	Threads         ThreadConfig     `yaml:"threads"`
	// Retention is how long Run / Task / Thread mappings live in the local
	// store after their last interaction. Default 168h (7 days). Zero
	// disables auto-pruning.
	Retention string `yaml:"retention,omitempty"`
}

// RetentionDuration parses Retention with a sensible default.
func (c *Config) RetentionDuration() time.Duration {
	const def = 7 * 24 * time.Hour
	if c.Retention == "" {
		return def
	}
	d, err := time.ParseDuration(c.Retention)
	if err != nil || d < 0 {
		return def
	}
	return d
}

// SlackAuth holds Slack credentials. AppToken and BotToken are kept in their
// on-disk form (an inline token, or an "env:NAME" / "file:/path" reference)
// so SaveConfig round-trips the file without leaking resolved secrets back to
// disk. Use ResolvedAppToken / ResolvedBotToken at runtime.
type SlackAuth struct {
	AppToken       string `yaml:"app_token"`
	BotToken       string `yaml:"bot_token"`
	DefaultChannel string `yaml:"default_channel,omitempty"`

	// Resolved values populated by Config.resolve(); never marshalled.
	resolvedApp string `yaml:"-"`
	resolvedBot string `yaml:"-"`
}

// ResolvedAppToken returns the runtime app token (after env:/file: resolution).
func (s SlackAuth) ResolvedAppToken() string { return s.resolvedApp }

// ResolvedBotToken returns the runtime bot token (after env:/file: resolution).
func (s SlackAuth) ResolvedBotToken() string { return s.resolvedBot }

// MirrorRule says "when a task run completes, post the final message to this
// channel". Either Task (single name/glob) or Tasks (list) must be set.
// First matching rule wins. Set Enabled=false to opt out.
type MirrorRule struct {
	Task          string   `yaml:"task,omitempty"`
	Tasks         []string `yaml:"tasks,omitempty"`
	Channel       string   `yaml:"channel,omitempty"`
	OpenThread    bool     `yaml:"open_thread,omitempty"`
	IncludeStatus bool     `yaml:"include_status,omitempty"`
	OnFailure     bool     `yaml:"on_failure,omitempty"`
	Enabled       *bool    `yaml:"enabled,omitempty"`
}

// IsEnabled returns true when the rule is enabled (default true).
func (r MirrorRule) IsEnabled() bool { return r.Enabled == nil || *r.Enabled }

// Patterns returns the union of Task and Tasks, normalised.
func (r MirrorRule) Patterns() []string {
	var out []string
	if r.Task != "" {
		out = append(out, r.Task)
	}
	out = append(out, r.Tasks...)
	return out
}

// TriggerChannel routes inbound Slack messages to bigband runs.
type TriggerChannel struct {
	Channel             string           `yaml:"channel"`
	Folder              string           `yaml:"folder"`
	RequireMention      bool             `yaml:"require_mention,omitempty"`
	AllowFreeformPrompt bool             `yaml:"allow_freeform_prompt,omitempty"`
	Commands            []TriggerCommand `yaml:"commands,omitempty"`
}

// TriggerCommand maps a regex to an action.
type TriggerCommand struct {
	Match  string `yaml:"match"`
	Action string `yaml:"action"`           // run | submit
	Folder string `yaml:"folder,omitempty"` // override channel folder for this command
}

// ThreadConfig governs thread-reply behaviour.
type ThreadConfig struct {
	Enabled           bool `yaml:"enabled"`
	ResumeWithSession bool `yaml:"resume_with_session"`
}

// ConfigPath returns the canonical config path.
func ConfigPath() string {
	return filepath.Join(paths.Root(), "extensions", "bigband-slack", "config.yaml")
}

// StatePath returns the canonical state-file path (thread mapping persistence).
func StatePath() string {
	return filepath.Join(paths.Root(), "extensions", "bigband-slack", "state.json")
}

// DaemonLogPath returns the path bigband-slack writes its launchd-managed
// stdout/stderr to.
func DaemonLogPath() string {
	return filepath.Join(paths.Root(), "extensions", "bigband-slack", "daemon.log")
}

// LoadConfig reads and resolves the bigband-slack config. Missing file is an
// error — operators must explicitly create one via `bigband-slack rules list`
// or by hand. This preserves the opt-in posture.
func LoadConfig() (*Config, error) {
	path := ConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if err := cfg.resolve(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// SaveConfig writes cfg back to disk, preserving the file's parent directory.
func SaveConfig(cfg *Config) error {
	path := ConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// resolve substitutes env: and file: token references into runtime-only
// fields, leaving the on-disk strings untouched so a later SaveConfig
// round-trips the file without leaking the resolved secret to disk.
func (c *Config) resolve() error {
	app, err := resolveSecret(c.Slack.AppToken)
	if err != nil {
		return fmt.Errorf("slack.app_token: %w", err)
	}
	bot, err := resolveSecret(c.Slack.BotToken)
	if err != nil {
		return fmt.Errorf("slack.bot_token: %w", err)
	}
	c.Slack.resolvedApp = app
	c.Slack.resolvedBot = bot
	return nil
}

func resolveSecret(s string) (string, error) {
	switch {
	case strings.HasPrefix(s, "env:"):
		name := strings.TrimPrefix(s, "env:")
		v := os.Getenv(name)
		if v == "" {
			return "", fmt.Errorf("env var %s is empty", name)
		}
		return v, nil
	case strings.HasPrefix(s, "file:"):
		path := strings.TrimPrefix(s, "file:")
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(data)), nil
	default:
		return s, nil
	}
}

// MatchTask returns the first MirrorRule that matches name, honouring rule
// order and the Enabled flag. Returns nil if no rule matches or the matching
// rule is disabled (the latter is an explicit opt-out).
func (c *Config) MatchTask(name string) *MirrorRule {
	for i := range c.Mirror {
		r := &c.Mirror[i]
		for _, pat := range r.Patterns() {
			if matchPattern(pat, name) {
				if !r.IsEnabled() {
					return nil
				}
				return r
			}
		}
	}
	return nil
}

// matchPattern is a tiny glob: "*" matches anything, "prefix-*" / "*-suffix"
// supported, and exact match otherwise. Sufficient for v1 routing.
func matchPattern(pattern, name string) bool {
	if pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, "*") && strings.HasPrefix(name, strings.TrimSuffix(pattern, "*")) {
		return true
	}
	if strings.HasPrefix(pattern, "*") && strings.HasSuffix(name, strings.TrimPrefix(pattern, "*")) {
		return true
	}
	return pattern == name
}
