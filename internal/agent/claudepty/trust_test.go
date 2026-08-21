package claudepty

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// withFakeHome points os.UserHomeDir to a temp directory for the duration of
// the test. Returns the temp dir so the test can write fixtures into it.
func withFakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

// realTempDir returns a temp dir with its symlinks resolved. On macOS
// t.TempDir() hands back a path under /var, itself a symlink to /private/var,
// and trust entries are keyed on the physical path claude reports as its cwd —
// so the expectation has to be the resolved one.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	return dir
}

// readJSON unmarshals path into a generic map for assertions.
func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return m
}

func TestEnsureProjectTrusted_CreatesFileWhenMissing(t *testing.T) {
	home := withFakeHome(t)
	workDir := realTempDir(t)

	if err := ensureProjectTrusted(workDir); err != nil {
		t.Fatalf("ensureProjectTrusted: %v", err)
	}

	cfg := readJSON(t, filepath.Join(home, ".claude.json"))
	projects, ok := cfg["projects"].(map[string]any)
	if !ok {
		t.Fatalf("projects map missing: %+v", cfg)
	}
	entry, ok := projects[workDir].(map[string]any)
	if !ok {
		t.Fatalf("project entry for %s missing: %+v", workDir, projects)
	}
	if got, _ := entry[trustField].(bool); !got {
		t.Errorf("%s = %v, want true", trustField, entry[trustField])
	}
}

func TestEnsureProjectTrusted_PreservesOtherKeys(t *testing.T) {
	home := withFakeHome(t)
	workDir := realTempDir(t)
	otherDir := "/some/other/dir"

	// Pre-existing config with unrelated top-level + per-project state we must
	// not lose.
	cfg := map[string]any{
		"numStartups": float64(42),
		"theme":       "dark",
		"projects": map[string]any{
			otherDir: map[string]any{
				"hasTrustDialogAccepted": true,
				"lastCost":               1.23,
			},
		},
	}
	cfgPath := filepath.Join(home, ".claude.json")
	if err := writeClaudeConfigAtomic(cfgPath, cfg); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	if err := ensureProjectTrusted(workDir); err != nil {
		t.Fatalf("ensureProjectTrusted: %v", err)
	}

	got := readJSON(t, cfgPath)
	if got["numStartups"] != float64(42) {
		t.Errorf("numStartups dropped: got %v", got["numStartups"])
	}
	if got["theme"] != "dark" {
		t.Errorf("theme dropped: got %v", got["theme"])
	}
	projects, _ := got["projects"].(map[string]any)
	other, _ := projects[otherDir].(map[string]any)
	if v, _ := other[trustField].(bool); !v {
		t.Errorf("existing project trust flag dropped")
	}
	if v, _ := other["lastCost"].(float64); v != 1.23 {
		t.Errorf("existing project lastCost dropped: got %v", other["lastCost"])
	}
	entry, _ := projects[workDir].(map[string]any)
	if v, _ := entry[trustField].(bool); !v {
		t.Errorf("new project trust flag not set")
	}
}

func TestEnsureProjectTrusted_FastPathNoOpWhenAlreadyTrusted(t *testing.T) {
	home := withFakeHome(t)
	workDir := realTempDir(t)

	// Seed with trust already true. Then make the file read-only so any
	// attempted write would fail loudly — proving the fast path skipped I/O.
	cfgPath := filepath.Join(home, ".claude.json")
	seed := map[string]any{
		"projects": map[string]any{
			workDir: map[string]any{
				trustField: true,
			},
		},
	}
	if err := writeClaudeConfigAtomic(cfgPath, seed); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	if err := os.Chmod(home, 0500); err != nil {
		t.Fatalf("chmod home: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(home, 0700) })

	if err := ensureProjectTrusted(workDir); err != nil {
		t.Fatalf("ensureProjectTrusted (fast path): %v", err)
	}
}

func TestEnsureProjectTrusted_ResolvesRelativePath(t *testing.T) {
	withFakeHome(t)
	workDir := t.TempDir()

	// Run from inside the workDir so "." resolves to its absolute path.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(workDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	if err := ensureProjectTrusted("."); err != nil {
		t.Fatalf("ensureProjectTrusted: %v", err)
	}

	cfgPath, _ := claudeGlobalConfigPath()
	cfg := readJSON(t, cfgPath)
	projects, ok := cfg["projects"].(map[string]any)
	if !ok || len(projects) != 1 {
		t.Fatalf("expected exactly one project entry, got %+v", projects)
	}
	for key := range projects {
		if !filepath.IsAbs(key) {
			t.Errorf("project entry keyed on non-absolute path %q", key)
		}
		if key == "." {
			t.Errorf("project entry keyed on literal %q instead of resolved abs", key)
		}
	}
}
