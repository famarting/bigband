package extensions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeManifest(t *testing.T, dir, name, body string) string {
	t.Helper()
	d := filepath.Join(dir, name)
	if err := os.MkdirAll(d, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	p := filepath.Join(d, ManifestFilename)
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func TestLoadManifest_OK(t *testing.T) {
	tmp := t.TempDir()
	p := writeManifest(t, tmp, "foo", `
name: foo
command: [echo, hi]
description: a test
env:
  KEY: value
`)
	m, err := LoadManifest(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if m.Name != "foo" {
		t.Errorf("name = %q, want foo", m.Name)
	}
	if !m.IsEnabled() {
		t.Errorf("default enabled should be true")
	}
	if m.EffectiveWorkingDir() != filepath.Join(tmp, "foo") {
		t.Errorf("working_dir default wrong: %s", m.EffectiveWorkingDir())
	}
	if m.EffectiveLogPath() != filepath.Join(tmp, "foo", "daemon.log") {
		t.Errorf("log_path default wrong: %s", m.EffectiveLogPath())
	}
	r := m.EffectiveRestart()
	if r.Policy != "on_failure" || r.InitialBackoff.Duration != time.Second {
		t.Errorf("restart defaults wrong: %+v", r)
	}
}

func TestLoadManifest_NameMismatch(t *testing.T) {
	tmp := t.TempDir()
	p := writeManifest(t, tmp, "foo", "name: bar\ncommand: [echo]\n")
	if _, err := LoadManifest(p); err == nil || !strings.Contains(err.Error(), "parent directory") {
		t.Fatalf("want parent-dir error, got %v", err)
	}
}

func TestLoadManifest_UnknownField(t *testing.T) {
	tmp := t.TempDir()
	p := writeManifest(t, tmp, "foo", "name: foo\ncommand: [x]\nbogus: 1\n")
	if _, err := LoadManifest(p); err == nil {
		t.Fatal("want error on unknown field")
	}
}

func TestLoadManifest_BadRestartPolicy(t *testing.T) {
	tmp := t.TempDir()
	p := writeManifest(t, tmp, "foo", `
name: foo
command: [echo]
restart:
  policy: maybe
`)
	if _, err := LoadManifest(p); err == nil || !strings.Contains(err.Error(), "policy") {
		t.Fatalf("want policy error, got %v", err)
	}
}

func TestLoadManifest_EmptyCommand(t *testing.T) {
	tmp := t.TempDir()
	p := writeManifest(t, tmp, "foo", "name: foo\ncommand: []\n")
	if _, err := LoadManifest(p); err == nil || !strings.Contains(err.Error(), "command") {
		t.Fatalf("want command error, got %v", err)
	}
}

func TestResolvedEnv(t *testing.T) {
	m := &Manifest{
		Env: map[string]string{
			"PLAIN":  "value",
			"INTERP": "prefix-${env:USER}-suffix",
			"MISS":   "x${env:NOT_SET}y",
			"PASS":   "literal-${other:thing}-passes",
		},
	}
	lookup := func(name string) string {
		if name == "USER" {
			return "alice"
		}
		return ""
	}
	got, missing := m.ResolvedEnv(lookup)
	if got["PLAIN"] != "value" {
		t.Errorf("PLAIN: %q", got["PLAIN"])
	}
	if got["INTERP"] != "prefix-alice-suffix" {
		t.Errorf("INTERP: %q", got["INTERP"])
	}
	if got["MISS"] != "xy" {
		t.Errorf("MISS: %q", got["MISS"])
	}
	if got["PASS"] != "literal-${other:thing}-passes" {
		t.Errorf("PASS: %q", got["PASS"])
	}
	if len(missing) != 1 || missing[0] != "NOT_SET" {
		t.Errorf("missing: %v", missing)
	}
}

func TestComputeBackoff(t *testing.T) {
	policy := RestartPolicy{
		InitialBackoff:         Duration{Duration: 1 * time.Second},
		MaxBackoff:             Duration{Duration: 30 * time.Second},
		MaxConsecutiveFailures: 5,
	}
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{0, 1 * time.Second},
		{1, 1 * time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
		{5, 16 * time.Second},
		{6, 30 * time.Second}, // capped
		{20, 30 * time.Second},
	}
	for _, c := range cases {
		if got := computeBackoff(policy, c.failures); got != c.want {
			t.Errorf("computeBackoff(%d) = %s; want %s", c.failures, got, c.want)
		}
	}
}

func TestSpecEqual(t *testing.T) {
	a := &Manifest{
		Name:    "foo",
		Command: []string{"echo", "hi"},
		Env:     map[string]string{"K": "v"},
		Restart: RestartPolicy{Policy: "always"},
	}
	b := &Manifest{
		Name:    "foo",
		Command: []string{"echo", "hi"},
		Env:     map[string]string{"K": "v"},
		Restart: RestartPolicy{Policy: "always"},
	}
	if !specEqual(a, b) {
		t.Fatal("identical specs should be equal")
	}
	b.Description = "different" // cosmetic — should not affect equality
	if !specEqual(a, b) {
		t.Fatal("description should not affect specEqual")
	}
	b.Command = []string{"echo", "bye"}
	if specEqual(a, b) {
		t.Fatal("command change should break specEqual")
	}
}
