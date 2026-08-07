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

func TestRunState_BackgroundAgentTracked(t *testing.T) {
	s := newRunState(io.Discard, io.Discard)
	// A background Agent's tool_use input is indistinguishable from a
	// synchronous one (no run_in_background flag), so the dispatch alone must
	// NOT populate pendingBG.
	dispatch := parseRec(t, `{"type":"assistant","message":{"role":"assistant","content":[
        {"type":"tool_use","id":"toolu_a","name":"Agent","input":{"description":"d","subagent_type":"general-purpose","prompt":"p"}}
    ]}}`)
	s.visit(dispatch)
	if _, ok := s.pendingBG["toolu_a"]; ok {
		t.Errorf("agent dispatch input alone must not be tracked: %+v", s.pendingBG)
	}

	// The async-launch acknowledgement in the tool_result is the real signal.
	launched := parseRec(t, `{"type":"user","message":{"role":"user","content":[
        {"type":"tool_result","tool_use_id":"toolu_a","content":"Async agent launched successfully.\nagentId: a0abc (internal ID)"}
    ]}}`)
	s.visit(launched)
	if _, ok := s.pendingBG["toolu_a"]; !ok {
		t.Fatalf("pendingBG missing toolu_a after async-launch result: %+v", s.pendingBG)
	}

	// A synchronous Agent that returns its output inline must NOT be tracked.
	sync := parseRec(t, `{"type":"user","message":{"role":"user","content":[
        {"type":"tool_result","tool_use_id":"toolu_b","content":"Here are the results.\n## Task 1 ..."}
    ]}}`)
	s.visit(sync)
	if _, ok := s.pendingBG["toolu_b"]; ok {
		t.Errorf("synchronous agent result should not be tracked")
	}

	// The task-notification carrying the tool-use-id drains the entry.
	done := parseRec(t, `{"type":"attachment","attachment":{"type":"queued_command","commandMode":"task-notification","prompt":"<task-notification>\n<task-id>a0abc</task-id>\n<tool-use-id>toolu_a</tool-use-id>\n<status>completed</status>"}}`)
	s.visit(done)
	if _, ok := s.pendingBG["toolu_a"]; ok {
		t.Errorf("pendingBG should be drained after task-notification")
	}
}

func TestRunState_SecondRoundBackgroundAgentReDeferred(t *testing.T) {
	// Regression: a synthesis turn (Phase 2b) that itself dispatches a new
	// async agent must repopulate pendingBG so Run's Phase 2 loop re-enters
	// the drain wait rather than exiting with round-2 work still outstanding.
	s := newRunState(io.Discard, io.Discard)
	s.deferring = true
	s.awaitingSynthesis = true

	launch := parseRec(t, `{"type":"user","message":{"role":"user","content":[
        {"type":"tool_result","tool_use_id":"toolu_round2","content":"Async agent launched successfully. agentId: a999"}
    ]}}`)
	s.visit(launch)
	if _, ok := s.pendingBG["toolu_round2"]; !ok {
		t.Fatalf("round-2 async agent not tracked during synthesis: %+v", s.pendingBG)
	}
	// The synthesis turn ends; visit returns true (Run's loop then re-checks
	// pendingBG and re-enters Phase 2a), but the pending entry remains so the
	// run cannot terminate with round-2 work still outstanding.
	td := parseRec(t, `{"type":"system","subtype":"turn_duration"}`)
	if !s.visit(td) {
		t.Fatalf("turn_duration in phase 2b must return true")
	}
	if _, ok := s.pendingBG["toolu_round2"]; !ok {
		t.Errorf("round-2 pending entry must survive the synthesis turn_duration")
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

func TestRunState_Phase2bAwaitsSynthesisTurn(t *testing.T) {
	s := newRunState(io.Discard, io.Discard)
	// Enter Phase 2b: background work already drained, now waiting for the
	// follow-up synthesis turn that the completion notifications trigger.
	s.deferring = true
	s.awaitingSynthesis = true

	// The pre-synthesis lead-in must NOT end the tail, even though pendingBG
	// is empty — in Phase 2b only a turn_duration terminates.
	lead := parseRec(t, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"The fetch is ready. Let me read the results."}]}}`)
	if s.visit(lead) {
		t.Fatalf("phase 2b must not stop on a mid-turn lead-in")
	}

	// The real briefing arrives, then the synthesis turn terminates.
	briefing := parseRec(t, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Good morning. Here is your briefing."}]}}`)
	if s.visit(briefing) {
		t.Fatalf("phase 2b must not stop before the synthesis turn_duration")
	}
	td := parseRec(t, `{"type":"system","subtype":"turn_duration"}`)
	if !s.visit(td) {
		t.Fatalf("phase 2b must stop on the synthesis turn_duration")
	}
	if got := s.finalMessage(); got != "Good morning. Here is your briefing." {
		t.Errorf("finalMessage = %q, want the synthesis text", got)
	}
}

func TestRunState_FinalMessagePrefersCompletedTurn(t *testing.T) {
	s := newRunState(io.Discard, io.Discard)

	// Before any turn terminates, finalMessage falls back to lastSeenText.
	s.visit(parseRec(t, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"working"}]}}`))
	if got := s.finalMessage(); got != "working" {
		t.Errorf("pre-turn finalMessage = %q, want fallback to lastSeenText", got)
	}

	// A completed turn's trailing text becomes the snapshot.
	s.visit(parseRec(t, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done: final answer"}]}}`))
	s.visit(parseRec(t, `{"type":"system","subtype":"turn_duration"}`))
	if got := s.finalMessage(); got != "done: final answer" {
		t.Errorf("finalMessage after turn_duration = %q, want the completed-turn text", got)
	}

	// A subsequent mid-turn line that never reaches a turn boundary must NOT
	// override the last completed turn's text.
	s.visit(parseRec(t, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"still thinking..."}]}}`))
	if got := s.finalMessage(); got != "done: final answer" {
		t.Errorf("mid-turn line overrode completed turn: finalMessage = %q", got)
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
