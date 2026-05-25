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
	if err := st.SetRunning("my-job", 42, "/some/folder"); err != nil {
		t.Fatalf("SetRunning: %v", err)
	}
	st2, err := state.Load()
	if err != nil {
		t.Fatalf("Load2: %v", err)
	}
	js := st2.Get("my-job")
	if js.RunningPID != 42 {
		t.Errorf("want PID 42, got %d", js.RunningPID)
	}
	if js.Folder != "/some/folder" {
		t.Errorf("want folder /some/folder, got %q", js.Folder)
	}
}

func TestSetJobParamsPersistsAcrossReload(t *testing.T) {
	withTempHome(t)
	st, err := state.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	f := false
	params := state.JobParams{
		Prompt:   "do the thing",
		PreExec:  []string{"git pull"},
		PostExec: []string{"echo done"},
		Worktree: &f,
		Timeout:  "2h",
		Model:    "opus",
		Effort:   "high",
		Agent:    "claude-pty",
	}
	if err := st.SetJobParams("oneoff", params); err != nil {
		t.Fatalf("SetJobParams: %v", err)
	}
	// Mutate caller's slices to confirm state holds its own copy.
	params.PreExec[0] = "MUTATED"
	params.PostExec[0] = "MUTATED"

	st2, err := state.Load()
	if err != nil {
		t.Fatalf("Load2: %v", err)
	}
	js := st2.Get("oneoff")
	if js.Prompt != "do the thing" {
		t.Errorf("prompt = %q", js.Prompt)
	}
	if len(js.PreExec) != 1 || js.PreExec[0] != "git pull" {
		t.Errorf("pre_exec = %v (caller mutation leaked into state)", js.PreExec)
	}
	if len(js.PostExec) != 1 || js.PostExec[0] != "echo done" {
		t.Errorf("post_exec = %v", js.PostExec)
	}
	if js.Worktree == nil || *js.Worktree {
		t.Errorf("worktree should be *false, got %v", js.Worktree)
	}
	if js.Timeout != "2h" || js.Model != "opus" || js.Effort != "high" || js.Agent != "claude-pty" {
		t.Errorf("scalar fields mismatch: %+v", js)
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
	if len(st.Jobs) != 0 {
		t.Errorf("expected empty state on corrupt file, got %d jobs", len(st.Jobs))
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
			name := fmt.Sprintf("job-%d", i)
			if err := st.SetRunning(name, i+1, "/folder"); err != nil {
				t.Errorf("SetRunning %s: %v", name, err)
			}
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("job-%d", i)
		js := st.Get(name)
		if js.RunningPID != i+1 {
			t.Errorf("job %s: want PID %d, got %d", name, i+1, js.RunningPID)
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
		if err := st.SetRunning(fmt.Sprintf("job-%d", i), i+1, ""); err != nil {
			t.Fatalf("SetRunning: %v", err)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = st.SetDone(fmt.Sprintf("job-%d", i), state.StatusOK, time.Second, "/log", "")
		}(i)
	}
	wg.Wait()
	for i := 0; i < 10; i++ {
		js := st.Get(fmt.Sprintf("job-%d", i))
		if js.RunningPID != 0 {
			t.Errorf("job %d: RunningPID should be 0 after SetDone", i)
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
		st.Jobs[fmt.Sprintf("old-%d", i)] = &state.JobState{LastRun: &old}
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
	st.Jobs["cfg-job"] = &state.JobState{LastRun: &old}
	st.Jobs["ephemeral"] = &state.JobState{LastRun: &old}
	removed := st.RemoveStaleEphemerals(map[string]bool{"cfg-job": true}, cutoff)
	if len(removed) != 1 || removed[0].Name != "ephemeral" {
		t.Errorf("expected ephemeral removed, got %v", removed)
	}
	if _, ok := st.Jobs["cfg-job"]; !ok {
		t.Error("configured job must not be removed")
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
	st.Jobs["running-job"] = &state.JobState{LastRun: &old, RunningPID: 999}
	removed := st.RemoveStaleEphemerals(nil, cutoff)
	if len(removed) != 0 {
		t.Errorf("running job should not be removed, got %v", removed)
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
	st.Jobs["keep-wt"] = &state.JobState{LastRun: &old, KeepWorktree: true, WorktreePath: "/some/wt"}
	removed := st.RemoveStaleEphemerals(nil, cutoff)
	if len(removed) != 0 {
		t.Errorf("KeepWorktree job should not be removed")
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
	st.Jobs["old"] = &state.JobState{LastRun: &ancient}
	st.Jobs["fresh"] = &state.JobState{LastRun: &fresh}
	removed := st.RemoveStaleEphemerals(nil, cutoff)
	if len(removed) != 1 || removed[0].Name != "old" {
		t.Errorf("expected only 'old' removed, got %v", removed)
	}
}

func TestLockExclusion(t *testing.T) {
	withTempHome(t)
	const jobName = "exclusive-job"
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
			release, ok := state.Lock(jobName)
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
