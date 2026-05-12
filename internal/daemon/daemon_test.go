package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/famarting/bigband/internal/config"
	"github.com/famarting/bigband/internal/events"
	"github.com/famarting/bigband/internal/ipc"
	"github.com/famarting/bigband/internal/paths"
	"github.com/famarting/bigband/internal/state"
)

func withTempHome(t *testing.T) {
	t.Helper()
	t.Setenv("BIGBAND_HOME", t.TempDir())
	if err := paths.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}
}

func emptyConfig() *config.Config {
	return &config.Config{Tasks: []*config.Task{}}
}

func loadEmptyState(t *testing.T) *state.State {
	t.Helper()
	st, err := state.Load()
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	return st
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// --- buildSubmittedTask ---

func TestBuildSubmittedTask_Basic(t *testing.T) {
	withTempHome(t)
	folder := t.TempDir()
	task, runID, err := buildSubmittedTask(
		&ipc.SubmitRunRequest{Folder: folder, Prompt: "do the thing", Name: "my-task"},
		emptyConfig(), loadEmptyState(t),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task.Name != "my-task" {
		t.Errorf("name: want my-task, got %s", task.Name)
	}
	if task.Folder != folder {
		t.Errorf("folder: want %s, got %s", folder, task.Folder)
	}
	if runID == "" {
		t.Error("runID should not be empty")
	}
}

func TestBuildSubmittedTask_RandomName(t *testing.T) {
	withTempHome(t)
	task, _, err := buildSubmittedTask(
		&ipc.SubmitRunRequest{Folder: t.TempDir(), Prompt: "do something"},
		emptyConfig(), loadEmptyState(t),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(task.Name, "oneoff-") {
		t.Errorf("auto-generated name should start with 'oneoff-', got %q", task.Name)
	}
}

func TestBuildSubmittedTask_InvalidFolder(t *testing.T) {
	withTempHome(t)
	_, _, err := buildSubmittedTask(
		&ipc.SubmitRunRequest{Folder: "/does/not/exist/at/all", Prompt: "do something"},
		emptyConfig(), loadEmptyState(t),
	)
	if err == nil {
		t.Error("expected error for non-existent folder")
	}
}

func TestBuildSubmittedTask_EmptyPrompt(t *testing.T) {
	withTempHome(t)
	_, _, err := buildSubmittedTask(
		&ipc.SubmitRunRequest{Folder: t.TempDir(), Prompt: "   "},
		emptyConfig(), loadEmptyState(t),
	)
	if err == nil {
		t.Error("expected error for whitespace-only prompt")
	}
}

func TestBuildSubmittedTask_InvalidName(t *testing.T) {
	withTempHome(t)
	_, _, err := buildSubmittedTask(
		&ipc.SubmitRunRequest{Folder: t.TempDir(), Prompt: "do something", Name: "bad name"},
		emptyConfig(), loadEmptyState(t),
	)
	if err == nil {
		t.Error("expected error for name with space")
	}
}

func TestBuildSubmittedTask_CollidesWithRunning(t *testing.T) {
	withTempHome(t)
	folder := t.TempDir()
	st := loadEmptyState(t)
	if err := st.SetRunning("my-task", 9999, folder); err != nil {
		t.Fatalf("SetRunning: %v", err)
	}
	_, _, err := buildSubmittedTask(
		&ipc.SubmitRunRequest{Folder: folder, Prompt: "do something", Name: "my-task"},
		emptyConfig(), st,
	)
	if err == nil {
		t.Error("expected error for name collision with running task")
	}
}

func TestBuildSubmittedTask_MissingFolder(t *testing.T) {
	withTempHome(t)
	_, _, err := buildSubmittedTask(
		&ipc.SubmitRunRequest{Folder: "", Prompt: "do something"},
		emptyConfig(), loadEmptyState(t),
	)
	if err == nil {
		t.Error("expected error for empty folder")
	}
}

// --- configReloadedPayload ---

func TestConfigReloadedPayload_Counts(t *testing.T) {
	disabled := false
	cfg := &config.Config{
		Tasks: []*config.Task{
			{Name: "sched1", Schedule: "daily at 20:00"},
			{Name: "one-off"},
			{Name: "disabled", Enabled: &disabled},
			{Name: "sched2", Schedule: "weekly"},
		},
		Templates: []*config.Task{
			{Name: "tpl1"},
			{Name: "tpl2"},
		},
	}
	p := configReloadedPayload(cfg)
	if p.TaskCount != 4 {
		t.Errorf("TaskCount: want 4, got %d", p.TaskCount)
	}
	if p.ScheduledCount != 2 {
		t.Errorf("ScheduledCount: want 2, got %d", p.ScheduledCount)
	}
	if p.OneOffCount != 1 {
		t.Errorf("OneOffCount: want 1, got %d", p.OneOffCount)
	}
	if p.DisabledCount != 1 {
		t.Errorf("DisabledCount: want 1, got %d", p.DisabledCount)
	}
	if p.TemplatesCount != 2 {
		t.Errorf("TemplatesCount: want 2, got %d", p.TemplatesCount)
	}
}

// --- summarizeEvent ---

func TestSummarizeEvent_TaskRunStarted(t *testing.T) {
	env := events.Envelope{
		Type: events.TypeTaskRunStarted,
		Data: mustJSON(events.TaskRunStartedData{Folder: "/my/repo", Schedule: "daily"}),
	}
	s := summarizeEvent(env)
	if !strings.Contains(s, "folder=") {
		t.Errorf("summary should contain folder=, got %q", s)
	}
	if !strings.Contains(s, "schedule=") {
		t.Errorf("summary should contain schedule=, got %q", s)
	}
}

func TestSummarizeEvent_TaskRunCompleted(t *testing.T) {
	env := events.Envelope{
		Type: events.TypeTaskRunCompleted,
		Data: mustJSON(events.TaskRunCompletedData{Status: "ok", DurationMS: 5000}),
	}
	s := summarizeEvent(env)
	if !strings.Contains(s, "status=ok") {
		t.Errorf("summary should contain status=ok, got %q", s)
	}
	if !strings.Contains(s, "duration=") {
		t.Errorf("summary should contain duration=, got %q", s)
	}
}

func TestSummarizeEvent_UnknownType(t *testing.T) {
	env := events.Envelope{Type: "unknown.event.type"}
	if s := summarizeEvent(env); s != "" {
		t.Errorf("unknown type should return empty string, got %q", s)
	}
}
