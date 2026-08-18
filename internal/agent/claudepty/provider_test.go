package claudepty

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
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

// bgBashRec is one assistant record dispatching a background Bash call, and
// bgDoneRec its completion notification. Together they drive pendingBG in the
// Phase 2 tests below.
const (
	bgBashRec = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_bg","name":"Bash","input":{"command":"sleep 1","run_in_background":true}}]}}`
	bgDoneRec = `{"type":"attachment","attachment":{"type":"queued_command","commandMode":"task-notification","prompt":"<tool-use-id>toolu_bg</tool-use-id><status>completed</status>"}}`
)

// withStallWindow shrinks deferredStallWindow for the duration of a test so the
// Phase 2a waits resolve in milliseconds instead of an hour.
func withStallWindow(t *testing.T, d time.Duration) {
	t.Helper()
	prev := deferredStallWindow
	deferredStallWindow = d
	t.Cleanup(func() { deferredStallWindow = prev })
}

// appendLine appends one JSONL record to path, as claude would.
func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// deferringState returns a runState already in Phase 2a with one outstanding
// background task, plus the session file seeded with its dispatch record.
func deferringState(t *testing.T) (*runState, string, int64) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	appendLine(t, path, bgBashRec)
	s := newRunState(io.Discard, io.Discard)
	off, err := tailSession(context.Background(), path, 0, func() bool { return false }, func(r *sessionRecord) bool {
		s.visit(r)
		return len(s.pendingBG) > 0 // stop once the dispatch is tracked
	})
	if err != nil {
		t.Fatalf("seed tail: %v", err)
	}
	if len(s.pendingBG) != 1 {
		t.Fatalf("pendingBG = %v, want one outstanding task", s.pendingBG)
	}
	s.deferring = true
	return s, path, off
}

// TestAwaitDrain_ProgressExtendsPastOneWindow is the regression test for the
// bug this design fixes: Phase 2 used to share one absolute budget anchored at
// the first turn_duration, so a run that spent it on productive early work
// (grooming a backlog, opening a PR) got killed once it finally settled in to
// wait on CI. Here the session keeps writing records for several windows'
// worth of wall-clock, with no single gap exceeding one window — the wait must
// survive all of it and still see the drain.
func TestAwaitDrain_ProgressExtendsPastOneWindow(t *testing.T) {
	const (
		window = 200 * time.Millisecond
		gap    = 120 * time.Millisecond
		writes = 5
	)
	withStallWindow(t, window)
	s, path, off := deferringState(t)

	go func() {
		for i := 0; i < writes; i++ {
			time.Sleep(gap)
			appendLine(t, path, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"still working"}]}}`)
		}
		time.Sleep(gap)
		appendLine(t, path, bgDoneRec)
	}()

	start := time.Now()
	_, err := s.awaitDrain(context.Background(), path, off, func() bool { return false })
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("awaitDrain err = %v, want nil (progress should extend the wait)", err)
	}
	if len(s.pendingBG) != 0 {
		t.Errorf("pendingBG = %v, want drained", s.pendingBG)
	}
	if elapsed <= window {
		t.Errorf("elapsed %v <= one window %v; the test did not exercise the extension", elapsed, window)
	}
}

// TestAwaitDrain_SilenceEndsWait covers the other side of the contract: with
// nothing at all written, the wait must end after one full window rather than
// holding the session open to the job deadline.
func TestAwaitDrain_SilenceEndsWait(t *testing.T) {
	const window = 150 * time.Millisecond
	withStallWindow(t, window)
	s, path, off := deferringState(t)

	start := time.Now()
	_, err := s.awaitDrain(context.Background(), path, off, func() bool { return false })
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("awaitDrain err = nil, want a stall error")
	}
	if !strings.Contains(err.Error(), "no session activity") {
		t.Errorf("err = %v, want it to name the inactivity window", err)
	}
	if len(s.pendingBG) != 1 {
		t.Errorf("pendingBG = %v, want the task still outstanding", s.pendingBG)
	}
	// One window, not two: a stalled wait must not retry, and must not spin.
	if elapsed < window || elapsed > 4*window {
		t.Errorf("elapsed %v, want ~one window (%v)", elapsed, window)
	}
}

// TestAwaitDrain_ClaudeExitEndsWait verifies a dead producer ends the wait
// immediately — its background children died with it, so nothing can drain.
func TestAwaitDrain_ClaudeExitEndsWait(t *testing.T) {
	withStallWindow(t, 10*time.Second)
	s, path, off := deferringState(t)

	start := time.Now()
	_, err := s.awaitDrain(context.Background(), path, off, func() bool { return true })
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("awaitDrain err = nil, want an exit error")
	}
	if elapsed > 2*time.Second {
		t.Errorf("elapsed %v; a dead producer should end the wait promptly", elapsed)
	}
}

// TestAwaitDrain_ContextCancelEndsWait verifies the job deadline still wins:
// the sliding window can extend the wait indefinitely on paper, but never past
// the timeout the runner puts on ctx.
func TestAwaitDrain_ContextCancelEndsWait(t *testing.T) {
	withStallWindow(t, time.Hour)
	s, path, off := deferringState(t)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := s.awaitDrain(ctx, path, off, func() bool { return false })
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("awaitDrain err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("elapsed %v; the job deadline should end the wait promptly", elapsed)
	}
}
