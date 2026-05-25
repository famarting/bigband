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
	return &config.Config{Jobs: []*config.Job{}}
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

// --- buildSubmittedJob ---

func TestBuildSubmittedJob_Basic(t *testing.T) {
	withTempHome(t)
	folder := t.TempDir()
	job, runID, err := buildSubmittedJob(
		&ipc.SubmitRunRequest{Folder: folder, Prompt: "do the thing", Name: "my-job"},
		emptyConfig(), loadEmptyState(t),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if job.Name != "my-job" {
		t.Errorf("name: want my-job, got %s", job.Name)
	}
	if job.Folder != folder {
		t.Errorf("folder: want %s, got %s", folder, job.Folder)
	}
	if runID == "" {
		t.Error("runID should not be empty")
	}
}

func TestBuildSubmittedJob_RandomName(t *testing.T) {
	withTempHome(t)
	job, _, err := buildSubmittedJob(
		&ipc.SubmitRunRequest{Folder: t.TempDir(), Prompt: "do something"},
		emptyConfig(), loadEmptyState(t),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(job.Name, "oneoff-") {
		t.Errorf("auto-generated name should start with 'oneoff-', got %q", job.Name)
	}
}

func TestBuildSubmittedJob_InvalidFolder(t *testing.T) {
	withTempHome(t)
	_, _, err := buildSubmittedJob(
		&ipc.SubmitRunRequest{Folder: "/does/not/exist/at/all", Prompt: "do something"},
		emptyConfig(), loadEmptyState(t),
	)
	if err == nil {
		t.Error("expected error for non-existent folder")
	}
}

func TestBuildSubmittedJob_EmptyPrompt(t *testing.T) {
	withTempHome(t)
	_, _, err := buildSubmittedJob(
		&ipc.SubmitRunRequest{Folder: t.TempDir(), Prompt: "   "},
		emptyConfig(), loadEmptyState(t),
	)
	if err == nil {
		t.Error("expected error for whitespace-only prompt")
	}
}

func TestBuildSubmittedJob_InvalidName(t *testing.T) {
	withTempHome(t)
	_, _, err := buildSubmittedJob(
		&ipc.SubmitRunRequest{Folder: t.TempDir(), Prompt: "do something", Name: "bad name"},
		emptyConfig(), loadEmptyState(t),
	)
	if err == nil {
		t.Error("expected error for name with space")
	}
}

func TestBuildSubmittedJob_CollidesWithRunning(t *testing.T) {
	withTempHome(t)
	folder := t.TempDir()
	st := loadEmptyState(t)
	if err := st.SetRunning("my-job", 9999, folder); err != nil {
		t.Fatalf("SetRunning: %v", err)
	}
	_, _, err := buildSubmittedJob(
		&ipc.SubmitRunRequest{Folder: folder, Prompt: "do something", Name: "my-job"},
		emptyConfig(), st,
	)
	if err == nil {
		t.Error("expected error for name collision with running job")
	}
}

func TestBuildSubmittedJob_MissingFolder(t *testing.T) {
	withTempHome(t)
	_, _, err := buildSubmittedJob(
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
		Jobs: []*config.Job{
			{Name: "sched1", Schedule: "daily at 20:00"},
			{Name: "one-off"},
			{Name: "disabled", Enabled: &disabled},
			{Name: "sched2", Schedule: "weekly"},
		},
		Templates: []*config.Job{
			{Name: "tpl1"},
			{Name: "tpl2"},
		},
	}
	p := configReloadedPayload(cfg)
	if p.JobCount != 4 {
		t.Errorf("JobCount: want 4, got %d", p.JobCount)
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

func TestSummarizeEvent_JobRunStarted(t *testing.T) {
	env := events.Envelope{
		Type: events.TypeJobRunStarted,
		Data: mustJSON(events.JobRunStartedData{Folder: "/my/repo", Schedule: "daily"}),
	}
	s := summarizeEvent(env)
	if !strings.Contains(s, "folder=") {
		t.Errorf("summary should contain folder=, got %q", s)
	}
	if !strings.Contains(s, "schedule=") {
		t.Errorf("summary should contain schedule=, got %q", s)
	}
}

func TestSummarizeEvent_JobRunCompleted(t *testing.T) {
	env := events.Envelope{
		Type: events.TypeJobRunCompleted,
		Data: mustJSON(events.JobRunCompletedData{Status: "ok", DurationMS: 5000}),
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
