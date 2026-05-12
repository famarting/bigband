package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestConfigDefaults ensures the unset / zero-value config paths produce the
// documented defaults. These are the values that ship with the template
// after `bigband-wake init`, so a regression here means the docs lie.
func TestConfigDefaults(t *testing.T) {
	c := &Config{}
	if got := c.LeadDuration(); got != 60*time.Second {
		t.Fatalf("LeadDuration() default = %v, want 60s", got)
	}
	if got := c.MaxEventsValue(); got != 16 {
		t.Fatalf("MaxEventsValue() default = %d, want 16", got)
	}
	if got := c.ReconcileEvery(); got != time.Hour {
		t.Fatalf("ReconcileEvery() default = %v, want 1h", got)
	}
}

func TestConfigMaxEventsCap(t *testing.T) {
	// Hard cap so a typo can't make us evict user-added pmset entries.
	c := &Config{MaxEvents: 999}
	if got := c.MaxEventsValue(); got != 32 {
		t.Fatalf("MaxEventsValue() cap = %d, want 32", got)
	}
}

func TestConfigReconcileMinimum(t *testing.T) {
	// Anything sub-minute is treated as a typo and falls back to the default.
	c := &Config{ReconcileInterval: "30s"}
	if got := c.ReconcileEvery(); got != time.Hour {
		t.Fatalf("sub-minute interval should fall back to default, got %v", got)
	}
}

// TestStateRoundTrip verifies state.json survives a save/load cycle byte-for
// -byte at the field level. A bug here means a daemon restart drops events
// we still own and we can no longer cancel them by exact time.
func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BIGBAND_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "extensions", "bigband-wake"), 0700); err != nil {
		t.Fatal(err)
	}

	wakeAt := time.Date(2026, 5, 11, 5, 59, 0, 0, time.Local)
	fireAt := wakeAt.Add(60 * time.Second)
	in := &State{Events: []WakeEvent{
		{Task: "my-buddy-kickoff", WakeAt: wakeAt, FireAt: fireAt, ScheduledAt: time.Now()},
	}}
	if err := in.Save(); err != nil {
		t.Fatal(err)
	}
	out, err := LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(out.Events))
	}
	got := out.Events[0]
	if got.Task != "my-buddy-kickoff" {
		t.Fatalf("Task = %q, want my-buddy-kickoff", got.Task)
	}
	if !got.WakeAt.Equal(wakeAt) {
		t.Fatalf("WakeAt = %v, want %v", got.WakeAt, wakeAt)
	}
	if !got.FireAt.Equal(fireAt) {
		t.Fatalf("FireAt = %v, want %v", got.FireAt, fireAt)
	}
}

func TestStateLoadMissingReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BIGBAND_HOME", dir)
	st, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState on missing path: %v", err)
	}
	if len(st.Events) != 0 {
		t.Fatalf("expected empty state, got %d events", len(st.Events))
	}
}

// TestSudoersStanzaShape pins the user-facing format so a refactor doesn't
// accidentally drop the begin/end markers (they're what users copy/paste
// around) or the verify command (the only way to confirm sudoers worked).
func TestSudoersStanzaShape(t *testing.T) {
	s := SudoersStanza()
	for _, want := range []string{
		"----- begin stanza -----",
		"----- end stanza -----",
		"Cmnd_Alias BIGBAND_WAKE_PMSET",
		"NOPASSWD: BIGBAND_WAKE_PMSET",
		"sudo -n",
		"-g sched",
		"sudo rm",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("stanza missing %q", want)
		}
	}
}
