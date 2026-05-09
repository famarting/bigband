// Package extensions implements bigband's extension manifest model: parsing
// the per-directory manifest.yaml that declares a long-lived process the
// daemon should supervise, and the supervisor that spawns, restarts, and
// stops those processes.
//
// The manifest is bigband's *generic* contract for "spawn me and route my
// lifecycle." It deliberately contains no extension-specific fields (no
// slack:, no notify:): each extension keeps its own config under
// ~/.bigband-tasks/extensions/<name>/ and reads it at startup from its
// working_dir, exactly as before. The manifest only tells bigband how to
// invoke that binary and when to restart it.
package extensions

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ManifestFilename is the canonical filename a directory under
// ~/.bigband-tasks/extensions/<name>/ must contain to be discovered.
const ManifestFilename = "manifest.yaml"

// validName mirrors config.IsValidName but with a slightly tighter shape
// (lowercase + dashes only) since the name doubles as a directory name and
// part of process labels in logs.
var validName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// IsValidExtensionName reports whether s is acceptable as an extension name.
func IsValidExtensionName(s string) bool { return validName.MatchString(s) }

// Manifest is the on-disk shape of extensions/<name>/manifest.yaml. Unknown
// fields are rejected at parse time so typos surface immediately rather than
// being silently ignored.
type Manifest struct {
	// Name must match the parent directory name and validName.
	Name string `yaml:"name"`
	// Command is argv. The first element is resolved against the manifest's
	// resolved env PATH (or absolute path).
	Command []string `yaml:"command"`

	Enabled     *bool  `yaml:"enabled,omitempty"`
	Description string `yaml:"description,omitempty"`

	// WorkingDir defaults to the manifest's parent directory.
	WorkingDir string `yaml:"working_dir,omitempty"`
	// LogPath defaults to "<working_dir>/daemon.log".
	LogPath string `yaml:"log_path,omitempty"`

	// Env is the environment variables exported to the spawned process. Values
	// support a single interpolation form: ${env:NAME} reads NAME from the
	// daemon's process environment at spawn time. Unset NAMEs interpolate to
	// the empty string (logged once per spawn).
	Env map[string]string `yaml:"env,omitempty"`

	Restart RestartPolicy `yaml:"restart,omitempty"`

	// Subscribes is advisory in v1: the daemon does not enforce that the
	// extension only sees these event types. Reserved for future capability
	// gating. Documented purely so reviewers can see at a glance what an
	// extension consumes.
	Subscribes []string `yaml:"subscribes,omitempty"`

	// path is the absolute path the manifest was loaded from. Populated by
	// LoadManifest. Used to derive defaults and for diagnostics.
	path string `yaml:"-"`
}

// RestartPolicy controls how the supervisor reacts to a child exit.
type RestartPolicy struct {
	// Policy: always | on_failure | never. Default: on_failure.
	Policy string `yaml:"policy,omitempty"`
	// InitialBackoff is the wait before the first restart attempt. Default 1s.
	InitialBackoff Duration `yaml:"initial_backoff,omitempty"`
	// MaxBackoff caps exponential growth. Default 30s.
	MaxBackoff Duration `yaml:"max_backoff,omitempty"`
	// MaxConsecutiveFailures is the circuit-breaker threshold: after this many
	// rapid failures (no successful run between them) the supervisor gives up
	// and marks the extension FAILED until manually started or the manifest
	// is re-saved. Zero means "unlimited"; default 5.
	MaxConsecutiveFailures int `yaml:"max_consecutive_failures,omitempty"`
}

// Duration is a time.Duration that marshals to/from a human string ("2s"),
// matching the convention in internal/config.Duration but avoiding a package
// dependency.
type Duration struct{ time.Duration }

// MarshalYAML emits the duration as its String form.
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// UnmarshalYAML parses "1s", "30s", "5m" and similar.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	if value.Value == "" {
		return nil
	}
	dur, err := time.ParseDuration(value.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", value.Value, err)
	}
	d.Duration = dur
	return nil
}

// IsEnabled returns true when the manifest's enabled field is unset or true.
func (m *Manifest) IsEnabled() bool {
	return m.Enabled == nil || *m.Enabled
}

// Path returns the absolute path the manifest was loaded from.
func (m *Manifest) Path() string { return m.path }

// Dir returns the directory containing the manifest file.
func (m *Manifest) Dir() string { return filepath.Dir(m.path) }

// EffectiveWorkingDir returns the directory the spawned process should run in,
// applying the default (manifest dir) when unset.
func (m *Manifest) EffectiveWorkingDir() string {
	if m.WorkingDir != "" {
		return m.WorkingDir
	}
	return m.Dir()
}

// EffectiveLogPath returns the absolute path stdout/stderr should be appended
// to, applying the default when unset.
func (m *Manifest) EffectiveLogPath() string {
	if m.LogPath != "" {
		return m.LogPath
	}
	return filepath.Join(m.EffectiveWorkingDir(), "daemon.log")
}

// EffectiveRestart returns Restart with defaults applied for any zero fields.
func (m *Manifest) EffectiveRestart() RestartPolicy {
	r := m.Restart
	if r.Policy == "" {
		r.Policy = "on_failure"
	}
	if r.InitialBackoff.Duration <= 0 {
		r.InitialBackoff = Duration{Duration: 1 * time.Second}
	}
	if r.MaxBackoff.Duration <= 0 {
		r.MaxBackoff = Duration{Duration: 30 * time.Second}
	}
	if r.InitialBackoff.Duration > r.MaxBackoff.Duration {
		r.MaxBackoff = r.InitialBackoff
	}
	if r.MaxConsecutiveFailures == 0 {
		r.MaxConsecutiveFailures = 5
	}
	return r
}

// ResolvedEnv returns the env map with ${env:NAME} placeholders interpolated
// against lookup. lookup is typically os.Getenv; tests can pass a fake.
//
// Unknown placeholders resolve to the empty string and their names are
// returned as the second value so the caller can log them once per spawn.
func (m *Manifest) ResolvedEnv(lookup func(string) string) (map[string]string, []string) {
	if lookup == nil {
		lookup = os.Getenv
	}
	out := make(map[string]string, len(m.Env))
	var unresolved []string
	for k, v := range m.Env {
		out[k], unresolved = interpolateEnvValue(v, lookup, unresolved)
	}
	return out, unresolved
}

// interpolateEnvValue replaces every ${env:NAME} occurrence in v with lookup(NAME).
// Unrecognised placeholders pass through unchanged so e.g. ${other:thing} is
// preserved (we own only the env: namespace).
func interpolateEnvValue(v string, lookup func(string) string, unresolved []string) (string, []string) {
	const open = "${env:"
	var b strings.Builder
	for {
		before, rest, found := strings.Cut(v, open)
		b.WriteString(before)
		if !found {
			break
		}
		name, after, closed := strings.Cut(rest, "}")
		if !closed {
			// Unterminated placeholder — leave it as-is.
			b.WriteString(open)
			v = rest
			continue
		}
		val := lookup(name)
		if val == "" {
			unresolved = append(unresolved, name)
		}
		b.WriteString(val)
		v = after
	}
	return b.String(), unresolved
}

// LoadManifest reads, parses, and validates the manifest at path. The returned
// Manifest has Path populated. Validation includes the "name == parent dir"
// rule, so passing a path under any extensions/<name>/ is sufficient.
func LoadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading manifest: %w", err)
	}
	m := &Manifest{}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(m); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolving %s: %w", path, err)
	}
	m.path = abs
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("invalid manifest %s: %w", path, err)
	}
	return m, nil
}

// Validate checks the manifest against the schema rules described on Manifest.
// Returns the first error encountered.
func (m *Manifest) Validate() error {
	if m.Name == "" {
		return fmt.Errorf("name is required")
	}
	if !IsValidExtensionName(m.Name) {
		return fmt.Errorf("name %q must match [a-z][a-z0-9-]*", m.Name)
	}
	if m.path != "" {
		parent := filepath.Base(filepath.Dir(m.path))
		if parent != m.Name {
			return fmt.Errorf("name %q must equal parent directory name %q", m.Name, parent)
		}
	}
	if len(m.Command) == 0 {
		return fmt.Errorf("command must be non-empty")
	}
	for i, arg := range m.Command {
		if arg == "" {
			return fmt.Errorf("command[%d] is empty", i)
		}
	}
	switch m.Restart.Policy {
	case "", "always", "on_failure", "never":
	default:
		return fmt.Errorf("restart.policy %q must be one of: always, on_failure, never", m.Restart.Policy)
	}
	if m.Restart.InitialBackoff.Duration < 0 {
		return fmt.Errorf("restart.initial_backoff must be >= 0")
	}
	if m.Restart.MaxBackoff.Duration < 0 {
		return fmt.Errorf("restart.max_backoff must be >= 0")
	}
	if m.Restart.MaxConsecutiveFailures < 0 {
		return fmt.Errorf("restart.max_consecutive_failures must be >= 0")
	}
	return nil
}
