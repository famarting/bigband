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

// jobWith returns a minimally valid entry, so Validate exercises the env rules
// rather than failing on an unrelated missing field.
func jobWith(j *config.Job) *config.Config {
	j.Name = "a"
	j.Schedule = "@every 1h"
	j.Folder = "/tmp"
	j.Prompt = "x"
	return &config.Config{Jobs: []*config.Job{j}}
}

func mustResolve(t *testing.T, c *config.Config, j *config.Job) map[string]string {
	t.Helper()
	got, err := c.ResolveEnv(j)
	if err != nil {
		t.Fatalf("ResolveEnv: %v", err)
	}
	return got
}

// ---------------------------------------------------------------- resolution

func TestResolveEnvNilWhenUnset(t *testing.T) {
	c := &config.Config{}
	got, err := c.ResolveEnv(&config.Job{Name: "a"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("want nil when nothing is configured, got %v", got)
	}
}

func TestResolveEnvEntryOverridesDefaultsPerKey(t *testing.T) {
	c := &config.Config{Defaults: config.Defaults{Env: map[string]string{
		"SHARED": "from-defaults",
		"KEPT":   "from-defaults",
	}}}
	got := mustResolve(t, c, &config.Job{
		Name: "a",
		Env:  map[string]string{"SHARED": "from-entry", "OWN": "x"},
	})
	if got["SHARED"] != "from-entry" {
		t.Errorf("entry should win for SHARED, got %q", got["SHARED"])
	}
	if got["KEPT"] != "from-defaults" {
		t.Errorf("unrelated default key should survive, got %q", got["KEPT"])
	}
	if got["OWN"] != "x" {
		t.Errorf("entry-only key missing, got %q", got["OWN"])
	}
}

// The merge must not write back into Defaults.Env — two entries with their own
// env: would otherwise contaminate each other through the shared map.
func TestResolveEnvDoesNotMutateDefaults(t *testing.T) {
	defs := map[string]string{"A": "1"}
	c := &config.Config{Defaults: config.Defaults{Env: defs}}
	_ = mustResolve(t, c, &config.Job{Name: "a", Env: map[string]string{"A": "2", "B": "3"}})
	if defs["A"] != "1" || len(defs) != 1 {
		t.Fatalf("defaults were mutated: %v", defs)
	}
}

func TestResolveEnvPrecedenceLayering(t *testing.T) {
	dir := t.TempDir()
	df := writeEnvFile(t, dir, "defaults.env", "K=from-defaults-file\nONLY_DF=x\n", 0o600)
	jf := writeEnvFile(t, dir, "entry.env", "K=from-entry-file\n", 0o600)

	c := &config.Config{Defaults: config.Defaults{
		EnvFile: []string{df},
		Env:     map[string]string{"K": "from-defaults-env"},
	}}
	got := mustResolve(t, c, &config.Job{
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

func TestResolveEnvEntryFileOverridesDefaultsEnv(t *testing.T) {
	dir := t.TempDir()
	jf := writeEnvFile(t, dir, "entry.env", "K=from-entry-file\n", 0o600)
	c := &config.Config{Defaults: config.Defaults{Env: map[string]string{"K": "from-defaults-env"}}}
	got := mustResolve(t, c, &config.Job{Name: "a", EnvFile: []string{jf}})
	if got["K"] != "from-entry-file" {
		t.Errorf("entry env_file should override defaults env:, got %q", got["K"])
	}
}

// ResolveEnv reads files, so unlike the EffectiveX helpers it must report a
// failure instead of quietly omitting the keys.
func TestResolveEnvReportsUnreadableFile(t *testing.T) {
	c := &config.Config{}
	_, err := c.ResolveEnv(&config.Job{
		Name:    "a",
		EnvFile: []string{filepath.Join(t.TempDir(), "absent.env")},
	})
	if err == nil {
		t.Fatal("want an error when an env_file cannot be read, got nil")
	}
	if !strings.Contains(err.Error(), "env_file") {
		t.Errorf("error should name env_file, got %v", err)
	}
}

// No variable expansion: a credential is taken literally, so a value containing
// $NAME survives intact. An earlier revision expanded these and silently
// corrupted any secret containing a dollar sign.
func TestResolveEnvDoesNotExpandVariables(t *testing.T) {
	t.Setenv("HOME", "/Users/victim")
	t.Setenv("BB_TEST_SET", "expanded")
	c := &config.Config{}
	got := mustResolve(t, c, &config.Job{Name: "a", Env: map[string]string{
		"LITERAL_HOME":  "myS3cret$HOME!2024",
		"LITERAL_BRACE": "${BB_TEST_SET}",
		"LITERAL_DOLLR": "pa$$word",
	}})
	for k, want := range map[string]string{
		"LITERAL_HOME":  "myS3cret$HOME!2024",
		"LITERAL_BRACE": "${BB_TEST_SET}",
		"LITERAL_DOLLR": "pa$$word",
	} {
		if got[k] != want {
			t.Errorf("%s: value must be literal; want %q, got %q", k, want, got[k])
		}
	}
}

// ------------------------------------------------------------ file parsing

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
EMPTY=
DOLLAR=pa$$word
`, 0o600)

	c := &config.Config{}
	got := mustResolve(t, c, &config.Job{Name: "a", EnvFile: []string{f}})

	want := map[string]string{
		"PLAIN":       "value",
		"EXPORTED":    "exported-value",
		"DQUOTED":     "double quoted",
		"SQUOTED":     "single quoted",
		"SPACED":      "trimmed",
		"WITH_EQUALS": "a=b=c",
		"EMPTY":       "",
		"DOLLAR":      "pa$$word",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: want %q, got %q", k, v, got[k])
		}
	}
}

func TestEnvFileDuplicateKeyLastWins(t *testing.T) {
	dir := t.TempDir()
	f := writeEnvFile(t, dir, "dup.env", "K=first\nK=second\n", 0o600)
	got := mustResolve(t, &config.Config{}, &config.Job{Name: "a", EnvFile: []string{f}})
	if got["K"] != "second" {
		t.Errorf("last occurrence should win, got %q", got["K"])
	}
}

func TestEnvFileCRLF(t *testing.T) {
	dir := t.TempDir()
	f := writeEnvFile(t, dir, "crlf.env", "K=v\r\nJ=w\r\n", 0o600)
	got := mustResolve(t, &config.Config{}, &config.Job{Name: "a", EnvFile: []string{f}})
	if got["K"] != "v" || got["J"] != "w" {
		t.Errorf("CRLF should parse cleanly, got %v", got)
	}
}

func TestEnvFileRejectsMalformedLine(t *testing.T) {
	dir := t.TempDir()
	f := writeEnvFile(t, dir, "bad.env", "GOOD=1\nthis-line-has-no-equals\n", 0o600)
	err := jobWith(&config.Job{EnvFile: []string{f}}).Validate()
	if err == nil {
		t.Fatal("want an error for a malformed env_file line, got nil")
	}
	if !strings.Contains(err.Error(), "KEY=VALUE") {
		t.Errorf("error should say what was expected, got %v", err)
	}
}

// An unterminated quote used to be kept as part of the value, baking a stray
// quote into the credential and failing later as an auth error instead of here.
func TestEnvFileRejectsUnterminatedQuote(t *testing.T) {
	for _, body := range []string{
		"API_KEY=\"sk-abc123\n",
		"API_KEY='sk-abc123\n",
		"API_KEY=\"mismatched'\n",
	} {
		dir := t.TempDir()
		f := writeEnvFile(t, dir, "q.env", body, 0o600)
		err := jobWith(&config.Job{EnvFile: []string{f}}).Validate()
		if err == nil {
			t.Errorf("want an error for %q, got nil", strings.TrimSpace(body))
			continue
		}
		if !strings.Contains(err.Error(), "does not close") {
			t.Errorf("error should explain the unterminated quote, got %v", err)
		}
	}
}

func TestEnvFileTildeExpansion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeEnvFile(t, home, "tilde.env", "FROM_TILDE=yes\n", 0o600)
	got := mustResolve(t, &config.Config{}, &config.Job{Name: "a", EnvFile: []string{"~/tilde.env"}})
	if got["FROM_TILDE"] != "yes" {
		t.Errorf("~ in env_file was not expanded, got %v", got)
	}
}

// ------------------------------------------------------------- validation

func TestValidateRejectsMissingEnvFile(t *testing.T) {
	err := jobWith(&config.Job{EnvFile: []string{filepath.Join(t.TempDir(), "absent.env")}}).Validate()
	if err == nil {
		t.Fatal("want an error for a missing env_file, got nil")
	}
	if !strings.Contains(err.Error(), "env_file") {
		t.Errorf("error should name env_file, got %v", err)
	}
}

func TestValidateEnvFileModeBoundaries(t *testing.T) {
	cases := []struct {
		mode    os.FileMode
		wantErr bool
	}{
		{0o600, false},
		{0o400, false},
		{0o640, true}, // group read
		{0o604, true}, // other read
		{0o644, true},
		{0o660, true},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		f := writeEnvFile(t, dir, "m.env", "K=v\n", tc.mode)
		// WriteFile is subject to umask, so force the mode we mean to test.
		if err := os.Chmod(f, tc.mode); err != nil {
			t.Fatal(err)
		}
		err := jobWith(&config.Job{EnvFile: []string{f}}).Validate()
		if tc.wantErr && err == nil {
			t.Errorf("mode %04o: want rejection, got nil", tc.mode)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("mode %04o: want acceptance, got %v", tc.mode, err)
		}
	}
}

func TestValidateRejectsRelativeEnvFile(t *testing.T) {
	err := jobWith(&config.Job{EnvFile: []string{"creds/openai.env"}}).Validate()
	if err == nil {
		t.Fatal("want an error for a relative env_file path, got nil")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("error should explain the requirement, got %v", err)
	}
}

// post_exec trusts BIGBAND_STATUS to decide whether the run succeeded, so an
// entry must not be able to define it.
func TestValidateRejectsReservedEnvPrefix(t *testing.T) {
	for _, k := range []string{"BIGBAND_STATUS", "BIGBAND_REPLY_FILE", "BIGBAND_ANYTHING"} {
		err := jobWith(&config.Job{Env: map[string]string{k: "spoofed"}}).Validate()
		if err == nil {
			t.Errorf("%s: want rejection, got nil", k)
			continue
		}
		if !strings.Contains(err.Error(), "reserved") {
			t.Errorf("%s: error should say it is reserved, got %v", k, err)
		}
	}
}

// Templates carry Env/EnvFile like entries do, and were previously unchecked.
func TestValidateChecksTemplates(t *testing.T) {
	c := &config.Config{Templates: []*config.Job{{
		Name:    "tmpl",
		Prompt:  "x",
		EnvFile: []string{filepath.Join(t.TempDir(), "absent.env")},
	}}}
	err := c.Validate()
	if err == nil {
		t.Fatal("want an error for a template with a bad env_file, got nil")
	}
	if !strings.Contains(err.Error(), "template") {
		t.Errorf("error should name the template scope, got %v", err)
	}
}

func TestValidateChecksDefaults(t *testing.T) {
	c := &config.Config{Defaults: config.Defaults{
		Env: map[string]string{"BIGBAND_STATUS": "spoofed"},
	}}
	err := c.Validate()
	if err == nil {
		t.Fatal("want an error for a reserved key in defaults, got nil")
	}
	if !strings.Contains(err.Error(), "defaults") {
		t.Errorf("error should name the defaults scope, got %v", err)
	}
}

// ------------------------------------------------------------------- Load

func TestLoadParsesEnvAndEnvFile(t *testing.T) {
	dir := t.TempDir()
	f := writeEnvFile(t, dir, "creds.env", "OPENAI_API_KEY=sk-from-file\nSHARED=from-file\n", 0o600)
	path := filepath.Join(dir, "config.yaml")
	body := fmt.Sprintf(`
jobs:
  - name: with-env
    schedule: "@every 1h"
    folder: /tmp
    prompt: hi
    env_file:
      - %s
    env:
      SHARED: from-entry
`, f)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := mustResolve(t, c, c.Jobs[0])
	if got["OPENAI_API_KEY"] != "sk-from-file" {
		t.Errorf("env_file did not reach ResolveEnv, got %v", got)
	}
	if got["SHARED"] != "from-entry" {
		t.Errorf("env: should override env_file, got %q", got["SHARED"])
	}
}

// A config that sets nothing must resolve to nil, so existing setups spawn
// with an untouched environment.
func TestLoadWithoutEnvIsUnaffected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := `
jobs:
  - name: plain
    schedule: "@every 1h"
    folder: /tmp
    prompt: hi
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got, err := c.ResolveEnv(c.Jobs[0])
	if err != nil || got != nil {
		t.Fatalf("want (nil, nil) for a config with no env, got (%v, %v)", got, err)
	}
}
