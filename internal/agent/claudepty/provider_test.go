package claudepty

import (
	"encoding/json"
	"io"
	"testing"
	"time"
)

// parseRec is a small helper for visit-state tests: it unmarshals a JSONL
// line into a sessionRecord. Panics on failure since test inputs are static.
func parseRec(t *testing.T, line string) *sessionRecord {
	t.Helper()
	var rec sessionRecord
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return &rec
}

func TestRunState_AssistantTextCaptured(t *testing.T) {
	s := newRunState(io.Discard, io.Discard)
	rec := parseRec(t, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hello"}]}}`)
	if s.visit(rec) {
		t.Fatalf("text-only record should not stop tailing")
	}
	if s.lastSeenText != "hello" {
		t.Errorf("lastSeenText = %q, want %q", s.lastSeenText, "hello")
	}
}

func TestRunState_BackgroundBashTracked(t *testing.T) {
	s := newRunState(io.Discard, io.Discard)
	bg := parseRec(t, `{"type":"assistant","message":{"role":"assistant","content":[
        {"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"sleep 60","run_in_background":true}}
    ]}}`)
	s.visit(bg)
	if _, ok := s.pendingBG["toolu_1"]; !ok {
		t.Fatalf("pendingBG missing toolu_1: %+v", s.pendingBG)
	}

	// Foreground bash must NOT be tracked.
	fg := parseRec(t, `{"type":"assistant","message":{"role":"assistant","content":[
        {"type":"tool_use","id":"toolu_2","name":"Bash","input":{"command":"ls"}}
    ]}}`)
	s.visit(fg)
	if _, ok := s.pendingBG["toolu_2"]; ok {
		t.Errorf("foreground bash should not be tracked")
	}

	// Completion notification should drain.
	done := parseRec(t, `{"type":"attachment","attachment":{"type":"queued_command","commandMode":"task-notification","prompt":"<tool-use-id>toolu_1</tool-use-id><status>completed</status>"}}`)
	s.visit(done)
	if _, ok := s.pendingBG["toolu_1"]; ok {
		t.Errorf("pendingBG should be drained after task-notification")
	}
}

func TestRunState_ScheduleWakeupCaptured(t *testing.T) {
	s := newRunState(io.Discard, io.Discard)
	rec := parseRec(t, `{"type":"assistant","message":{"role":"assistant","content":[
        {"type":"tool_use","id":"tw","name":"ScheduleWakeup","input":{"delaySeconds":600,"prompt":"continue","reason":"poll"}}
    ]}}`)
	s.visit(rec)
	w := s.agentWakeup()
	if w == nil || w.Delay != 600*time.Second || w.Prompt != "continue" {
		t.Errorf("agentWakeup = %+v, want delay=600s prompt=continue", w)
	}
}

func TestRunState_TurnDurationPhase1StopsImmediately(t *testing.T) {
	s := newRunState(io.Discard, io.Discard)
	td := parseRec(t, `{"type":"system","subtype":"turn_duration"}`)
	if !s.visit(td) {
		t.Fatalf("phase 1 turn_duration must return true")
	}
	if !s.turnDone {
		t.Errorf("turnDone must be set")
	}
}

func TestRunState_TurnDurationDeferredHoldsForPendingBG(t *testing.T) {
	s := newRunState(io.Discard, io.Discard)
	s.deferring = true
	s.pendingBG["toolu_1"] = struct{}{}

	td := parseRec(t, `{"type":"system","subtype":"turn_duration"}`)
	if s.visit(td) {
		t.Fatalf("phase 2 turn_duration with pendingBG must NOT return true")
	}
	if !s.turnDone {
		t.Errorf("turnDone should still be marked")
	}

	// Once the bg task completes, visit should return true on the next record.
	done := parseRec(t, `{"type":"attachment","attachment":{"type":"queued_command","commandMode":"task-notification","prompt":"<tool-use-id>toolu_1</tool-use-id>"}}`)
	if !s.visit(done) {
		t.Fatalf("phase 2 should stop when pendingBG drains to zero")
	}
}

func TestRunState_SidechainAssistantIgnored(t *testing.T) {
	s := newRunState(io.Discard, io.Discard)
	side := parseRec(t, `{"type":"assistant","isSidechain":true,"message":{"role":"assistant","content":[
        {"type":"text","text":"sub-agent"},
        {"type":"tool_use","id":"x","name":"Bash","input":{"run_in_background":true}}
    ]}}`)
	s.visit(side)
	if s.lastSeenText != "" {
		t.Errorf("sidechain text should not be surfaced; got %q", s.lastSeenText)
	}
	if len(s.pendingBG) != 0 {
		t.Errorf("sidechain bg should not be tracked; got %+v", s.pendingBG)
	}
}
