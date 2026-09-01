package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/famarting/bigband/internal/config"
)

func writeEnvFile(t *testing.T, dir, name, body string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEnvFileParsingForms(t *testing.T) {
	dir := t.TempDir()
	f := writeEnvFile(t, dir, "creds.env", `
# a comment, and a blank line above
PLAIN=value
export EXPORTED=exported-value
DQUOTED="double quoted"
SQUOTED='single quoted'
  SPACED  =  trimmed
WITH_EQUALS=a=b=c
DOLLAR=pa$$word
`, 0o600)

	c := &config.Config{}
	got := c.EffectiveEnv(&config.Job{Name: "a", EnvFile: []string{f}})

	want := map[string]string{
		"PLAIN":       "value",
		"EXPORTED":    "exported-value",
		"DQUOTED":     "double quoted",
		"SQUOTED":     "single quoted",
		"SPACED":      "trimmed",
		"WITH_EQUALS": "a=b=c",
		// Not expanded: a credential file holds literals, and expanding would
		// mangle a password containing a dollar sign.
		"DOLLAR": "pa$$word",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: want %q, got %q", k, v, got[k])
		}
	}
}

func TestEnvFileRejectsMalformedLine(t *testing.T) {
	dir := t.TempDir()
	f := writeEnvFile(t, dir, "bad.env", "GOOD=1\nthis-line-has-no-equals\n", 0o600)
	c := &config.Config{Jobs: []*config.Job{{
		Name: "a", Schedule: "@every 1h", Folder: "/tmp", Prompt: "x",
		EnvFile: []string{f},
	}}}
	err := c.Validate()
	if err == nil {
		t.Fatal("want an error for a malformed env_file line, got nil")
	}
	if !strings.Contains(err.Error(), "KEY=VALUE") {
		t.Errorf("error should say what was expected, got %v", err)
	}
}

func TestEnvPrecedenceLayering(t *testing.T) {
	dir := t.TempDir()
	df := writeEnvFile(t, dir, "defaults.env", "K=from-defaults-file\nONLY_DF=x\n", 0o600)
	jf := writeEnvFile(t, dir, "entry.env", "K=from-entry-file\n", 0o600)

	c := &config.Config{Defaults: config.Defaults{
		EnvFile: []string{df},
		Env:     map[string]string{"K": "from-defaults-env"},
	}}
	got := c.EffectiveEnv(&config.Job{
		Name:    "a",
		EnvFile: []string{jf},
		Env:     map[string]string{"K": "from-entry-env"},
	})

	// lowest to highest: defaults.env_file, defaults.env, entry.env_file, entry.env
	if got["K"] != "from-entry-env" {
		t.Errorf("entry env: should win outright, got %q", got["K"])
	}
	if got["ONLY_DF"] != "x" {
		t.Errorf("key only in defaults env_file should survive, got %q", got["ONLY_DF"])
	}
}

func TestEntryEnvFileOverridesDefaultsEnv(t *testing.T) {
	dir := t.TempDir()
	jf := writeEnvFile(t, dir, "entry.env", "K=from-entry-file\n", 0o600)
	c := &config.Config{Defaults: config.Defaults{Env: map[string]string{"K": "from-defaults-env"}}}
	got := c.EffectiveEnv(&config.Job{Name: "a", EnvFile: []string{jf}})
	if got["K"] != "from-entry-file" {
		t.Errorf("entry env_file should override defaults env:, got %q", got["K"])
	}
}

func TestEnvVarIndirection(t *testing.T) {
	t.Setenv("BB_TEST_SECRET", "resolved-from-daemon-env")
	c := &config.Config{}
	got := c.EffectiveEnv(&config.Job{
		Name: "a",
		Env:  map[string]string{"BRACED": "${BB_TEST_SECRET}", "BARE": "$BB_TEST_SECRET"},
	})
	if got["BRACED"] != "resolved-from-daemon-env" {
		t.Errorf("${VAR} not expanded, got %q", got["BRACED"])
	}
	if got["BARE"] != "resolved-from-daemon-env" {
		t.Errorf("$VAR not expanded, got %q", got["BARE"])
	}
}

func TestEnvFileTildeExpansion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeEnvFile(t, home, "tilde.env", "FROM_TILDE=yes\n", 0o600)
	c := &config.Config{}
	got := c.EffectiveEnv(&config.Job{Name: "a", EnvFile: []string{"~/tilde.env"}})
	if got["FROM_TILDE"] != "yes" {
		t.Errorf("~ in env_file was not expanded, got %v", got)
	}
}

// A missing credential file must fail at load. Silently running without the
// value is the exact failure this mechanism exists to prevent.
func TestValidateRejectsMissingEnvFile(t *testing.T) {
	c := &config.Config{Jobs: []*config.Job{{
		Name: "a", Schedule: "@every 1h", Folder: "/tmp", Prompt: "x",
		EnvFile: []string{filepath.Join(t.TempDir(), "absent.env")},
	}}}
	err := c.Validate()
	if err == nil {
		t.Fatal("want an error for a missing env_file, got nil")
	}
	if !strings.Contains(err.Error(), "env_file") {
		t.Errorf("error should name env_file, got %v", err)
	}
}

func TestValidateRejectsWorldReadableEnvFile(t *testing.T) {
	dir := t.TempDir()
	f := writeEnvFile(t, dir, "loose.env", "K=v\n", 0o644)
	c := &config.Config{Jobs: []*config.Job{{
		Name: "a", Schedule: "@every 1h", Folder: "/tmp", Prompt: "x",
		EnvFile: []string{f},
	}}}
	err := c.Validate()
	if err == nil {
		t.Fatal("want an error for a group/world-readable env_file, got nil")
	}
	if !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("error should say how to fix it, got %v", err)
	}
}

func TestValidateRejectsUnresolvedEnvRef(t *testing.T) {
	c := &config.Config{Jobs: []*config.Job{{
		Name: "a", Schedule: "@every 1h", Folder: "/tmp", Prompt: "x",
		Env: map[string]string{"K": "${BB_DEFINITELY_NOT_SET_12345}"},
	}}}
	err := c.Validate()
	if err == nil {
		t.Fatal("want an error for an unresolvable ${VAR}, got nil")
	}
	if !strings.Contains(err.Error(), "BB_DEFINITELY_NOT_SET_12345") {
		t.Errorf("error should name the variable, got %v", err)
	}
}

func TestLoadParsesEnvFile(t *testing.T) {
	dir := t.TempDir()
	f := writeEnvFile(t, dir, "creds.env", "OPENAI_API_KEY=sk-from-file\n", 0o600)
	path := filepath.Join(dir, "config.yaml")
	body := fmt.Sprintf(`
jobs:
  - name: with-env-file
    schedule: "@every 1h"
    folder: /tmp
    prompt: hi
    env_file:
      - %s
`, f)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := c.EffectiveEnv(c.Jobs[0])
	if got["OPENAI_API_KEY"] != "sk-from-file" {
		t.Errorf("env_file did not reach EffectiveEnv, got %v", got)
	}
}
