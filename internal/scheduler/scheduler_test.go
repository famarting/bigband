package scheduler_test

import (
	"fmt"
	"os"
	"sync/atomic"
	"testing"

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
