package extensions

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/famarting/bigband/internal/events"
)

// recordingPublisher captures every published envelope. Used to assert
// extension lifecycle events without touching disk.
type recordingPublisher struct {
	mu  sync.Mutex
	got []events.Envelope
}

func (r *recordingPublisher) Publish(env events.Envelope) {
	r.mu.Lock()
	r.got = append(r.got, env)
	r.mu.Unlock()
}

func (r *recordingPublisher) snapshot() []events.Envelope {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]events.Envelope, len(r.got))
	copy(out, r.got)
	return out
}

func (r *recordingPublisher) types() []events.Type {
	out := []events.Type{}
	for _, e := range r.snapshot() {
		out = append(out, e.Type)
	}
	return out
}

// silentLogger discards log output during tests.
func silentLogger() *log.Logger { return log.New(os.NewFile(0, os.DevNull), "", 0) }

// makeManifest writes a manifest.yaml under tmp/<name>/ and returns a parsed
// *Manifest already validated. Reuses LoadManifest so the same path the
// daemon uses is exercised.
func makeManifest(t *testing.T, tmp, name string, body string) *Manifest {
	t.Helper()
	dir := filepath.Join(tmp, name)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	p := filepath.Join(dir, ManifestFilename)
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	m, err := LoadManifest(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return m
}

// waitFor polls fn until it returns true or timeout elapses. Used in place of
// time.Sleep so tests stay fast.
func waitFor(t *testing.T, timeout time.Duration, fn func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestSupervisor_StartsAndStops(t *testing.T) {
	SetHealthyThreshold(50 * time.Millisecond)
	defer SetHealthyThreshold(30 * time.Second)

	tmp := t.TempDir()
	m := makeManifest(t, tmp, "alpha", `
name: alpha
command: [/bin/sh, -c, "sleep 5"]
restart:
  policy: never
`)
	pub := &recordingPublisher{}
	sup := NewSupervisor(tmp, pub, silentLogger())
	sup.Apply(m)

	// Wait for the started event to fire.
	if !waitFor(t, 2*time.Second, func() bool {
		for _, ev := range pub.snapshot() {
			if ev.Type == events.TypeExtensionStarted {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("never saw extension.started; got types=%v", pub.types())
	}

	// Now stop and confirm the supervise loop exits.
	if err := sup.Stop("alpha"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool {
		for _, v := range sup.List() {
			if v.Name == "alpha" && (v.Status == string(StatusStopped)) {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("never reached stopped; list=%+v", sup.List())
	}
}

func TestSupervisor_RestartsOnCrash(t *testing.T) {
	SetHealthyThreshold(50 * time.Millisecond)
	defer SetHealthyThreshold(30 * time.Second)

	tmp := t.TempDir()
	// Exit 1 immediately. The restart policy is on_failure (default).
	m := makeManifest(t, tmp, "beta", `
name: beta
command: [/bin/sh, -c, "exit 1"]
restart:
  policy: on_failure
  initial_backoff: 10ms
  max_backoff: 20ms
  max_consecutive_failures: 3
`)
	pub := &recordingPublisher{}
	sup := NewSupervisor(tmp, pub, silentLogger())
	sup.Apply(m)

	// Should circuit-break after 3 failures and end FAILED.
	if !waitFor(t, 3*time.Second, func() bool {
		for _, v := range sup.List() {
			if v.Name == "beta" && v.Status == string(StatusFailed) {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("never circuit-broke; list=%+v", sup.List())
	}

	// We should see at least one extension.exited with will_restart=true and
	// a final extension.exited with will_restart=false (the one that broke).
	var sawRestart, sawFinal bool
	for _, ev := range pub.snapshot() {
		if ev.Type != events.TypeExtensionExited {
			continue
		}
		var d events.ExtensionExitedData
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if d.WillRestart {
			sawRestart = true
		} else {
			sawFinal = true
		}
	}
	if !sawRestart {
		t.Errorf("expected at least one extension.exited will_restart=true")
	}
	if !sawFinal {
		t.Errorf("expected a final extension.exited will_restart=false")
	}
}

func TestSupervisor_NeverRestartsOnSuccessWhenPolicyOnFailure(t *testing.T) {
	SetHealthyThreshold(50 * time.Millisecond)
	defer SetHealthyThreshold(30 * time.Second)

	tmp := t.TempDir()
	m := makeManifest(t, tmp, "gamma", `
name: gamma
command: [/bin/sh, -c, "exit 0"]
restart:
  policy: on_failure
  initial_backoff: 10ms
  max_backoff: 20ms
`)
	pub := &recordingPublisher{}
	sup := NewSupervisor(tmp, pub, silentLogger())
	sup.Apply(m)

	if !waitFor(t, 2*time.Second, func() bool {
		for _, v := range sup.List() {
			if v.Name == "gamma" && v.Status == string(StatusStopped) {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("never reached stopped after success; list=%+v", sup.List())
	}
	// Restarts should be 0 (no restart on a clean exit when policy is on_failure).
	for _, v := range sup.List() {
		if v.Name == "gamma" && v.Restarts != 0 {
			t.Errorf("restarts=%d, want 0", v.Restarts)
		}
	}
}

func TestSupervisor_ApplyDisabledStops(t *testing.T) {
	SetHealthyThreshold(50 * time.Millisecond)
	defer SetHealthyThreshold(30 * time.Second)

	tmp := t.TempDir()
	m := makeManifest(t, tmp, "delta", `
name: delta
command: [/bin/sh, -c, "sleep 30"]
restart:
  policy: never
`)
	pub := &recordingPublisher{}
	sup := NewSupervisor(tmp, pub, silentLogger())
	sup.Apply(m)

	if !waitFor(t, 2*time.Second, func() bool {
		for _, v := range sup.List() {
			if v.Name == "delta" && v.Status == string(StatusRunning) {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("never reached running; list=%+v", sup.List())
	}

	// Re-apply with enabled: false.
	disabled := false
	m.Enabled = &disabled
	sup.Apply(m)

	if !waitFor(t, 5*time.Second, func() bool {
		for _, v := range sup.List() {
			if v.Name == "delta" && v.Status == string(StatusStopped) {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("never reached stopped after disable; list=%+v", sup.List())
	}
}

