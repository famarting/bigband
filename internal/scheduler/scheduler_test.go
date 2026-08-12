package scheduler_test

import (
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/famarting/bigband/internal/config"
	"github.com/famarting/bigband/internal/scheduler"
)

func TestSchedulerDiff(t *testing.T) {
	var fired atomic.Int64
	sched := scheduler.New(func(_ *config.Config, _ *config.Job) {
		fired.Add(1)
	}, func(_ string) bool { return false })
	defer sched.Stop()

	// A minimal config with a job that fires @every 1s for testing.
	cfg := buildConfig(t, "@every 1s")
	sched.Reload(cfg)

	next := sched.NextRuns()
	if len(next) != 1 {
		t.Fatalf("expected 1 scheduled job, got %d", len(next))
	}

	// Reloading with the same config should not change entry count.
	sched.Reload(cfg)
	next2 := sched.NextRuns()
	if len(next2) != 1 {
		t.Fatalf("expected 1 scheduled job after reload, got %d", len(next2))
	}

	// Reload with empty config should remove all entries.
	emptyCfg := &config.Config{}
	sched.Reload(emptyCfg)
	if len(sched.NextRuns()) != 0 {
		t.Error("expected 0 jobs after empty reload")
	}
}

// TestSchedulerUsesUTC pins the scheduler to UTC: "0 12 * * *" must mean noon
// UTC, whatever the machine's local timezone is. NextRuns renders in local
// time, so the expectation is that instant converted, not "12:00".
func TestSchedulerUsesUTC(t *testing.T) {
	sched := scheduler.New(func(_ *config.Config, _ *config.Job) {}, func(_ string) bool { return false })
	defer sched.Stop()

	sched.Reload(buildConfig(t, "0 12 * * *"))

	got, ok := sched.NextRuns()["test"]
	if !ok {
		t.Fatal("job not scheduled")
	}

	now := time.Now().UTC()
	want := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)
	if !want.After(now) {
		want = want.AddDate(0, 0, 1)
	}
	if wantStr := want.Local().Format("2006-01-02 15:04:05"); got != wantStr {
		t.Errorf("next run: got %q, want %q (noon UTC in local time)", got, wantStr)
	}
}

func buildConfig(t *testing.T, sched string) *config.Config {
	t.Helper()
	tmp := t.TempDir()
	raw := fmt.Sprintf(`
jobs:
  - name: test
    schedule: %q
    folder: %s
    prompt: "hello"
`, sched, tmp)
	f, err := os.CreateTemp("", "*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(raw)
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })

	cfg, err := config.Load(f.Name())
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	return cfg
}
