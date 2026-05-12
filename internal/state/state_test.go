package state_test

import (
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

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

func TestStateSaveLoad(t *testing.T) {
	withTempHome(t)
	st, err := state.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := st.SetRunning("my-task", 42, "/some/folder"); err != nil {
		t.Fatalf("SetRunning: %v", err)
	}
	st2, err := state.Load()
	if err != nil {
		t.Fatalf("Load2: %v", err)
	}
	ts := st2.Get("my-task")
	if ts.RunningPID != 42 {
		t.Errorf("want PID 42, got %d", ts.RunningPID)
	}
	if ts.Folder != "/some/folder" {
		t.Errorf("want folder /some/folder, got %q", ts.Folder)
	}
}

func TestStateCorruptFile(t *testing.T) {
	withTempHome(t)
	if err := os.WriteFile(paths.StateFile(), []byte("not json {{{"), 0600); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	st, err := state.Load()
	if err != nil {
		t.Fatalf("Load should not fail on corrupt file: %v", err)
	}
	if len(st.Tasks) != 0 {
		t.Errorf("expected empty state on corrupt file, got %d tasks", len(st.Tasks))
	}
}

func TestSetRunningConcurrent(t *testing.T) {
	withTempHome(t)
	st, err := state.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	const n = 10
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("task-%d", i)
			if err := st.SetRunning(name, i+1, "/folder"); err != nil {
				t.Errorf("SetRunning %s: %v", name, err)
			}
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("task-%d", i)
		ts := st.Get(name)
		if ts.RunningPID != i+1 {
			t.Errorf("task %s: want PID %d, got %d", name, i+1, ts.RunningPID)
		}
	}
}

func TestSetDoneConcurrent(t *testing.T) {
	withTempHome(t)
	st, err := state.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for i := 0; i < 10; i++ {
		if err := st.SetRunning(fmt.Sprintf("task-%d", i), i+1, ""); err != nil {
			t.Fatalf("SetRunning: %v", err)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = st.SetDone(fmt.Sprintf("task-%d", i), state.StatusOK, time.Second, "/log", "")
		}(i)
	}
	wg.Wait()
	for i := 0; i < 10; i++ {
		ts := st.Get(fmt.Sprintf("task-%d", i))
		if ts.RunningPID != 0 {
			t.Errorf("task %d: RunningPID should be 0 after SetDone", i)
		}
	}
}

func TestRemoveStaleEphemerals_BasicPrune(t *testing.T) {
	withTempHome(t)
	st, err := state.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cutoff := time.Now()
	old := cutoff.Add(-time.Hour)
	for i := 0; i < 5; i++ {
		st.Tasks[fmt.Sprintf("old-%d", i)] = &state.TaskState{LastRun: &old}
	}
	removed := st.RemoveStaleEphemerals(nil, cutoff)
	if len(removed) != 5 {
		t.Errorf("want 5 removed, got %d", len(removed))
	}
}

func TestRemoveStaleEphemerals_SkipsConfigured(t *testing.T) {
	withTempHome(t)
	st, err := state.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cutoff := time.Now()
	old := cutoff.Add(-time.Hour)
	st.Tasks["cfg-task"] = &state.TaskState{LastRun: &old}
	st.Tasks["ephemeral"] = &state.TaskState{LastRun: &old}
	removed := st.RemoveStaleEphemerals(map[string]bool{"cfg-task": true}, cutoff)
	if len(removed) != 1 || removed[0].Name != "ephemeral" {
		t.Errorf("expected ephemeral removed, got %v", removed)
	}
	if _, ok := st.Tasks["cfg-task"]; !ok {
		t.Error("configured task must not be removed")
	}
}

func TestRemoveStaleEphemerals_SkipsRunning(t *testing.T) {
	withTempHome(t)
	st, err := state.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cutoff := time.Now()
	old := cutoff.Add(-time.Hour)
	st.Tasks["running-task"] = &state.TaskState{LastRun: &old, RunningPID: 999}
	removed := st.RemoveStaleEphemerals(nil, cutoff)
	if len(removed) != 0 {
		t.Errorf("running task should not be removed, got %v", removed)
	}
}

func TestRemoveStaleEphemerals_SkipsKeepWorktree(t *testing.T) {
	withTempHome(t)
	st, err := state.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cutoff := time.Now()
	old := cutoff.Add(-time.Hour)
	st.Tasks["keep-wt"] = &state.TaskState{LastRun: &old, KeepWorktree: true, WorktreePath: "/some/wt"}
	removed := st.RemoveStaleEphemerals(nil, cutoff)
	if len(removed) != 0 {
		t.Errorf("KeepWorktree task should not be removed")
	}
}

func TestRemoveStaleEphemerals_RespectsRetention(t *testing.T) {
	withTempHome(t)
	st, err := state.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cutoff := time.Now().Add(-1 * time.Hour)
	ancient := cutoff.Add(-time.Hour)
	fresh := time.Now()
	st.Tasks["old"] = &state.TaskState{LastRun: &ancient}
	st.Tasks["fresh"] = &state.TaskState{LastRun: &fresh}
	removed := st.RemoveStaleEphemerals(nil, cutoff)
	if len(removed) != 1 || removed[0].Name != "old" {
		t.Errorf("expected only 'old' removed, got %v", removed)
	}
}

func TestLockExclusion(t *testing.T) {
	withTempHome(t)
	const taskName = "exclusive-task"
	var (
		mu      sync.Mutex
		holding int
		wg      sync.WaitGroup
	)
	const goroutines = 10
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, ok := state.Lock(taskName)
			if !ok {
				return
			}
			defer release()
			mu.Lock()
			holding++
			if holding > 1 {
				t.Errorf("multiple goroutines holding lock concurrently (holding=%d)", holding)
			}
			mu.Unlock()
			time.Sleep(2 * time.Millisecond)
			mu.Lock()
			holding--
			mu.Unlock()
		}()
	}
	wg.Wait()
}
