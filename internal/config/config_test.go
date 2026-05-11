package config_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/famarting/bigband/internal/config"
)

const validYAML = `
defaults:
  shell: /bin/zsh
  timeout: 2h

tasks:
  - name: test-task
    schedule: "Weekdays at ~20:00"
    folder: %s
    prompt: "Do something useful."
    enabled: true
`

// writeConfig writes content to a fresh file under t.TempDir() and returns its
// path. t.TempDir handles cleanup.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bigband.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadValid(t *testing.T) {
	tmp := t.TempDir()
	path := writeConfig(t, fmt.Sprintf(validYAML, tmp))

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(cfg.Tasks))
	}
	task := cfg.Tasks[0]
	if task.Name != "test-task" {
		t.Errorf("name: got %q", task.Name)
	}
	if task.CronExpr() != "0 20 * * 1-5" {
		t.Errorf("cron: got %q", task.CronExpr())
	}
	if task.JitterDuration() == 0 {
		t.Error("expected jitter to be set")
	}
}

func TestLoadDuplicateName(t *testing.T) {
	tmp := t.TempDir()
	path := writeConfig(t, `
tasks:
  - name: foo
    schedule: "@daily"
    folder: `+tmp+`
    prompt: "a"
  - name: foo
    schedule: "@daily"
    folder: `+tmp+`
    prompt: "b"
`)

	if _, err := config.Load(path); err == nil {
		t.Fatal("expected error for duplicate name")
	}
}

func TestLoadTemplate(t *testing.T) {
	tmp := t.TempDir()
	path := writeConfig(t, `
templates:
  - name: ci-check
    folder: `+tmp+`
    prompt: "Check CI"
    pre_exec: ["echo hi"]

tasks:
  - name: real-task
    schedule: "@daily"
    folder: `+tmp+`
    prompt: "Do work"
`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Templates) != 1 {
		t.Fatalf("expected 1 template, got %d", len(cfg.Templates))
	}
	if cfg.TemplateByName("ci-check") == nil {
		t.Error("TemplateByName(ci-check) returned nil")
	}
	got, kind := cfg.FindTaskOrTemplate("real-task")
	if got == nil || kind != "task" {
		t.Errorf("FindTaskOrTemplate(real-task) = %v, %q; want task", got, kind)
	}
	got, kind = cfg.FindTaskOrTemplate("ci-check")
	if got == nil || kind != "template" {
		t.Errorf("FindTaskOrTemplate(ci-check) = %v, %q; want template", got, kind)
	}
}

func TestLoadTemplateNameCollidesWithTask(t *testing.T) {
	tmp := t.TempDir()
	path := writeConfig(t, `
templates:
  - name: shared
    folder: `+tmp+`
    prompt: "x"

tasks:
  - name: shared
    schedule: "@daily"
    folder: `+tmp+`
    prompt: "y"
`)

	if _, err := config.Load(path); err == nil {
		t.Fatal("expected error for name collision between task and template")
	}
}

func TestLoadTemplateMissingPrompt(t *testing.T) {
	tmp := t.TempDir()
	path := writeConfig(t, `
templates:
  - name: noprompt
    folder: `+tmp+`
`)

	if _, err := config.Load(path); err == nil {
		t.Fatal("expected error for template missing prompt")
	}
}

func TestSlugify(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"my custom task", "my-custom-task"},
		{"  Hello  World  ", "hello-world"},
		{"Review Joni's Commit", "review-joni-s-commit"},
		{"already-good_name", "already-good_name"},
		{"UPPER", "upper"},
		{"trailing!!!", "trailing"},
		{"  ---  ", ""},
		{"a", "a"},
		{"123-abc", "123-abc"},
	}
	for _, c := range cases {
		if got := config.Slugify(c.in); got != c.want {
			t.Errorf("Slugify(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

func TestIsValidName(t *testing.T) {
	good := []string{"a", "abc", "a-b", "a_b", "task-1", "0task"}
	bad := []string{"", "Abc", "a b", "-leading", "_leading", "ünicode", "task!"}
	for _, s := range good {
		if !config.IsValidName(s) {
			t.Errorf("IsValidName(%q) = false; want true", s)
		}
	}
	for _, s := range bad {
		if config.IsValidName(s) {
			t.Errorf("IsValidName(%q) = true; want false", s)
		}
	}
}

func TestCheckFolderAllowed_EmptyAllowlistPermitsAll(t *testing.T) {
	cfg := &config.Config{}
	if err := cfg.CheckFolderAllowed(t.TempDir()); err != nil {
		t.Fatalf("empty allowlist must permit anything, got: %v", err)
	}
}

func TestCheckFolderAllowed_ExactMatch(t *testing.T) {
	root := realPath(t, t.TempDir())
	cfg := &config.Config{Defaults: config.Defaults{AllowedFolders: []string{root}}}
	if err := cfg.CheckFolderAllowed(root); err != nil {
		t.Fatalf("exact match must be allowed: %v", err)
	}
}

func TestCheckFolderAllowed_Subdir(t *testing.T) {
	root := realPath(t, t.TempDir())
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Defaults: config.Defaults{AllowedFolders: []string{root}}}
	if err := cfg.CheckFolderAllowed(sub); err != nil {
		t.Fatalf("subdir must be allowed: %v", err)
	}
}

func TestCheckFolderAllowed_SiblingPrefix(t *testing.T) {
	// /a/b should not match an allowlist of /a/ba.
	parent := realPath(t, t.TempDir())
	if err := os.MkdirAll(filepath.Join(parent, "ba"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(parent, "b"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Defaults: config.Defaults{AllowedFolders: []string{filepath.Join(parent, "ba")}}}
	if err := cfg.CheckFolderAllowed(filepath.Join(parent, "b")); err == nil {
		t.Fatal("expected denial: /a/b is not under /a/ba")
	}
}

func TestCheckFolderAllowed_Disjoint(t *testing.T) {
	a := realPath(t, t.TempDir())
	b := realPath(t, t.TempDir())
	cfg := &config.Config{Defaults: config.Defaults{AllowedFolders: []string{a}}}
	if err := cfg.CheckFolderAllowed(b); err == nil {
		t.Fatalf("expected denial for unrelated dir, got nil")
	}
}

// TestCheckFolderAllowed_GitWorktree exercises the worktree-awareness: a path
// inside a linked worktree must be permitted when the *primary* repo is on the
// allowlist, not the worktree path.
func TestCheckFolderAllowed_GitWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := realPath(t, t.TempDir())
	wt := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-wt")
	t.Cleanup(func() {
		_ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", wt).Run()
		_ = os.RemoveAll(wt)
	})
	gitInit(t, repo)
	if out, err := exec.Command("git", "-C", repo, "worktree", "add", "-b", "feature", wt).CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v\n%s", err, out)
	}

	// Allowlist contains the primary repo; the worktree path must be allowed
	// because OriginPath resolves it back to the primary.
	cfg := &config.Config{Defaults: config.Defaults{AllowedFolders: []string{repo}}}
	if err := cfg.CheckFolderAllowed(wt); err != nil {
		t.Fatalf("worktree of allowed repo must be allowed: %v", err)
	}

	// Allowlist contains an unrelated dir — worktree must be denied.
	other := realPath(t, t.TempDir())
	cfg = &config.Config{Defaults: config.Defaults{AllowedFolders: []string{other}}}
	if err := cfg.CheckFolderAllowed(wt); err == nil {
		t.Fatal("expected denial: worktree's primary is not on the allowlist")
	}
}

// gitInit makes dir a fresh repo with one commit so `worktree add` works.
func gitInit(t *testing.T, dir string) {
	t.Helper()
	for _, cmd := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.email=t@t", "-c", "user.name=t", "commit", "--allow-empty", "-q", "-m", "init"},
	} {
		args := append([]string{"-C", dir}, cmd...)
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", cmd, err, out)
		}
	}
}

// realPath resolves symlinks so comparisons match what OriginPath returns
// (e.g. macOS `/var` → `/private/var`).
func realPath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("evalsymlinks %s: %v", p, err)
	}
	return r
}

func TestLoadMissingPrompt(t *testing.T) {
	tmp := t.TempDir()
	path := writeConfig(t, `
tasks:
  - name: noprompt
    schedule: "@daily"
    folder: `+tmp+`
`)

	if _, err := config.Load(path); err == nil {
		t.Fatal("expected error for missing prompt")
	}
}
