package claudepty

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestTailSession_ResumeOffsetAfterVisitStop verifies that when visit stops the
// tail mid-buffer, the returned offset points at the start of the next
// unconsumed line — so a second tail from that offset sees the remaining
// records rather than skipping them. This guards the drain+synthesis co-arrival
// race: a drain notification and the synthesis turn's records can land in one
// read burst, and the stop on the notification must not discard the rest.
func TestTailSession_ResumeOffsetAfterVisitStop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	// Three complete records written in one shot (single read burst).
	lines := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"one"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"two"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"three"}]}}
`
	if err := os.WriteFile(path, []byte(lines), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// First pass: stop as soon as we see "two".
	var seen1 []string
	off, err := tailSession(context.Background(), path, 0, func() bool { return true }, func(r *sessionRecord) bool {
		if txt := assistantText(r); txt != "" {
			seen1 = append(seen1, txt)
			return txt == "two"
		}
		return false
	})
	if err != nil {
		t.Fatalf("first tail err: %v", err)
	}
	if len(seen1) != 2 || seen1[0] != "one" || seen1[1] != "two" {
		t.Fatalf("first pass saw %v, want [one two]", seen1)
	}

	// Second pass from the returned offset: must see exactly "three", proving
	// the record batched after the stopping line was neither dropped nor
	// re-delivered.
	var seen2 []string
	if _, err := tailSession(context.Background(), path, off, func() bool { return true }, func(r *sessionRecord) bool {
		if txt := assistantText(r); txt != "" {
			seen2 = append(seen2, txt)
		}
		return false
	}); err == nil {
		t.Fatalf("second tail expected childDead error, got nil")
	}
	if len(seen2) != 1 || seen2[0] != "three" {
		t.Errorf("second pass saw %v, want [three] (no drop, no re-delivery)", seen2)
	}
}
