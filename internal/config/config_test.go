package config_test

import (
	"fmt"
	"os"
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

func TestLoadValid(t *testing.T) {
	tmp := t.TempDir()
	content := fmt.Sprintf(validYAML, tmp)
	f, _ := os.CreateTemp("", "bigband-*.yaml")
	f.WriteString(content)
	f.Close()
	defer os.Remove(f.Name())

	cfg, err := config.Load(f.Name())
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
	content := `
tasks:
  - name: foo
    schedule: "@daily"
    folder: ` + tmp + `
    prompt: "a"
  - name: foo
    schedule: "@daily"
    folder: ` + tmp + `
    prompt: "b"
`
	f, _ := os.CreateTemp("", "bigband-*.yaml")
	f.WriteString(content)
	f.Close()
	defer os.Remove(f.Name())

	_, err := config.Load(f.Name())
	if err == nil {
		t.Fatal("expected error for duplicate name")
	}
}

func TestLoadTemplate(t *testing.T) {
	tmp := t.TempDir()
	content := `
templates:
  - name: ci-check
    folder: ` + tmp + `
    prompt: "Check CI"
    pre_exec: ["echo hi"]

tasks:
  - name: real-task
    schedule: "@daily"
    folder: ` + tmp + `
    prompt: "Do work"
`
	f, _ := os.CreateTemp("", "bigband-*.yaml")
	f.WriteString(content)
	f.Close()
	defer os.Remove(f.Name())

	cfg, err := config.Load(f.Name())
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
	content := `
templates:
  - name: shared
    folder: ` + tmp + `
    prompt: "x"

tasks:
  - name: shared
    schedule: "@daily"
    folder: ` + tmp + `
    prompt: "y"
`
	f, _ := os.CreateTemp("", "bigband-*.yaml")
	f.WriteString(content)
	f.Close()
	defer os.Remove(f.Name())

	if _, err := config.Load(f.Name()); err == nil {
		t.Fatal("expected error for name collision between task and template")
	}
}

func TestLoadTemplateMissingPrompt(t *testing.T) {
	tmp := t.TempDir()
	content := `
templates:
  - name: noprompt
    folder: ` + tmp + `
`
	f, _ := os.CreateTemp("", "bigband-*.yaml")
	f.WriteString(content)
	f.Close()
	defer os.Remove(f.Name())

	if _, err := config.Load(f.Name()); err == nil {
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

func TestLoadMissingPrompt(t *testing.T) {
	tmp := t.TempDir()
	content := `
tasks:
  - name: noprompt
    schedule: "@daily"
    folder: ` + tmp + `
`
	f, _ := os.CreateTemp("", "bigband-*.yaml")
	f.WriteString(content)
	f.Close()
	defer os.Remove(f.Name())

	_, err := config.Load(f.Name())
	if err == nil {
		t.Fatal("expected error for missing prompt")
	}
}
