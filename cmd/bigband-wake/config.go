package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/famarting/bigband/internal/paths"
	"gopkg.in/yaml.v3"
)

// extensionName is both the directory name under ~/.bigband/extensions/
// and the manifest's `name` field; they must match per the extension contract.
const extensionName = "bigband-wake"

// Config is the on-disk shape of
// ~/.bigband/extensions/bigband-wake/config.yaml. Opt-in: with
// Enabled=false (the default after init) the daemon loop does nothing beyond
// log a single line, so dropping the manifest in is safe.
type Config struct {
	// Enabled is the global opt-in. False (or unset) means the daemon will
	// not call pmset at all.
	Enabled bool `yaml:"enabled"`

	// LeadSeconds is how long before each scheduled fire time the wake event
	// is requested. Default 60.
	LeadSeconds int `yaml:"lead_seconds,omitempty"`

	// MaxEvents caps the number of pmset entries this extension owns at any
	// one time. macOS allows up to 64 total; we leave plenty of headroom for
	// user-added entries by defaulting to 16.
	MaxEvents int `yaml:"max_events,omitempty"`

	// ReconcileInterval is the safety-net cadence at which the daemon
	// re-derives the wake set even without an event nudging it. Default 1h.
	ReconcileInterval string `yaml:"reconcile_interval,omitempty"`

	// AssertionDuration is how long to hold an IOPMAssertion preventing idle
	// sleep after we detect a wake-from-sleep transition. Defaults to 45m,
	// which gives the bigband daemon plenty of time to thaw and fire the
	// scheduled cron tick before macOS would otherwise re-sleep. Set to "0"
	// to disable assertion-holding entirely.
	AssertionDuration string `yaml:"assertion_duration,omitempty"`
}

// LeadDuration returns LeadSeconds as a duration with a sensible default.
func (c *Config) LeadDuration() time.Duration {
	if c.LeadSeconds <= 0 {
		return 60 * time.Second
	}
	return time.Duration(c.LeadSeconds) * time.Second
}

// MaxEventsValue returns MaxEvents with a default of 16. Hard-capped at 32 so
// a typo in the config can't bury the user's manual pmset entries.
func (c *Config) MaxEventsValue() int {
	if c.MaxEvents <= 0 {
		return 16
	}
	if c.MaxEvents > 32 {
		return 32
	}
	return c.MaxEvents
}

// ReconcileEvery returns ReconcileInterval parsed with a default of 1h.
func (c *Config) ReconcileEvery() time.Duration {
	const def = 1 * time.Hour
	if c.ReconcileInterval == "" {
		return def
	}
	d, err := time.ParseDuration(c.ReconcileInterval)
	if err != nil || d < time.Minute {
		return def
	}
	return d
}

// AssertionHold returns AssertionDuration parsed, with a default of 45m. An
// explicit "0" disables assertion-holding (returns 0). Unparseable values
// fall back to the default rather than failing loud because this is a
// resilience knob — we'd rather always have *some* hold than refuse to start.
func (c *Config) AssertionHold() time.Duration {
	const def = 45 * time.Minute
	if c.AssertionDuration == "" {
		return def
	}
	d, err := time.ParseDuration(c.AssertionDuration)
	if err != nil {
		return def
	}
	if d < 0 {
		return 0
	}
	return d
}

// ConfigPath returns the canonical config path.
func ConfigPath() string {
	return filepath.Join(paths.Root(), "extensions", extensionName, "config.yaml")
}

// StatePath returns the canonical state-file path.
func StatePath() string {
	return filepath.Join(paths.Root(), "extensions", extensionName, "state.json")
}

// DaemonLogPath is the launchd-managed stdout/stderr destination, matching
// the manifest default. Surfaced here so `bigband-wake status` can print it.
func DaemonLogPath() string {
	return filepath.Join(paths.Root(), "extensions", extensionName, "daemon.log")
}

// ManifestPath is where `bigband-wake init` writes the supervisor manifest.
func ManifestPath() string {
	return filepath.Join(paths.Root(), "extensions", extensionName, "manifest.yaml")
}

// LoadConfig reads and parses config.yaml. Missing file is an error so we
// can never accidentally activate without a written-down config; the user
// runs `bigband-wake init` first.
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
	return cfg, nil
}
