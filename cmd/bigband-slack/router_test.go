package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/famarting/bigband/pkg/bigbandext"
)

// --- mock Slack client ---

type mockSlack struct {
	posted []slackPost
}

type slackPost struct {
	channel  string
	text     string
	threadTS string
}

func (m *mockSlack) PostMessage(channel, text, threadTS string) (string, error) {
	m.posted = append(m.posted, slackPost{channel, text, threadTS})
	return "ts-new", nil
}

// --- test helpers ---

func newTestSlackStore(t *testing.T) *Store {
	t.Helper()
	t.Setenv("BIGBAND_HOME", t.TempDir())
	store, err := LoadStore()
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	return store
}

func newSlackRouter(t *testing.T) (*Router, *mockSlack) {
	t.Helper()
	slack := &mockSlack{}
	store := newTestSlackStore(t)
	return &Router{store: store, slack: slack, bb: nil, cfg: &Config{}}, slack
}

// startSlackFakeDaemon starts a minimal IPC server that accepts submit
// actions and replies with a fake SubmitRunReply. Returns a connected Client.
func startSlackFakeDaemon(t *testing.T) *bigbandext.Client {
	t.Helper()
	// Use a short temp dir to avoid macOS socket path length limit (~104 chars).
	dir, err := os.MkdirTemp("", "bbslack")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		seq := 0
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			seq++
			go handleSlackFakeConn(conn, seq)
		}
	}()
	return bigbandext.NewClient(sockPath, time.Second)
}

func handleSlackFakeConn(conn net.Conn, _ int) {
	defer conn.Close()
	type genericCmd struct {
		Action string `json:"action"`
	}
	var c genericCmd
	if err := json.NewDecoder(conn).Decode(&c); err != nil {
		return
	}
	taskName := "oneoff-fake"
	runID := taskName + "/2024-01-01T00-00-00Z"
	payload, _ := json.Marshal(bigbandext.SubmitRunReply{
		RunID: runID, TaskName: taskName, LogPath: "/tmp/fake.log",
	})
	type reply struct {
		OK      bool            `json:"ok"`
		Payload json.RawMessage `json:"payload,omitempty"`
	}
	_ = json.NewEncoder(conn).Encode(reply{OK: true, Payload: payload})
}

// --- HandleEvent: ClaudeSessionStarted ---

func TestHandleEvent_SessionStarted_Valid(t *testing.T) {
	r, _ := newSlackRouter(t)
	data := bigbandext.ClaudeSessionStartedData{SessionID: "session-abc"}
	raw, _ := json.Marshal(data)
	r.HandleEvent(bigbandext.Envelope{
		Type:     bigbandext.TypeClaudeSessionStarted,
		TaskName: "my-task",
		Data:     raw,
	})
	if mapping := r.store.Tasks["my-task"]; mapping.SessionID != "session-abc" {
		t.Errorf("want session-abc stored, got %q", mapping.SessionID)
	}
}

func TestHandleEvent_SessionStarted_MalformedJSON(t *testing.T) {
	r, slack := newSlackRouter(t)
	r.HandleEvent(bigbandext.Envelope{
		Type:     bigbandext.TypeClaudeSessionStarted,
		TaskName: "my-task",
		Data:     []byte("not json {{{"),
	})
	if len(slack.posted) != 0 {
		t.Errorf("no Slack post expected on malformed JSON, got %d", len(slack.posted))
	}
}

// --- HandleEvent: TaskRunCompleted ---

func TestHandleEvent_Completed_PostsToSlack(t *testing.T) {
	r, slack := newSlackRouter(t)
	r.cfg = &Config{
		Mirror: []MirrorRule{
			{Task: "my-task", Channel: "C12345", OnFailure: true, IncludeStatus: true},
		},
	}
	data := bigbandext.TaskRunCompletedData{Status: "ok", DurationMS: 1000, FinalMessage: "All done!"}
	raw, _ := json.Marshal(data)
	r.HandleEvent(bigbandext.Envelope{
		Type:     bigbandext.TypeTaskRunCompleted,
		TaskName: "my-task",
		RunID:    "my-task/2024-01-01T00-00-00Z",
		Data:     raw,
	})
	if len(slack.posted) == 0 {
		t.Error("expected a Slack message to be posted")
	} else if slack.posted[0].channel != "C12345" {
		t.Errorf("want channel C12345, got %s", slack.posted[0].channel)
	}
}

func TestHandleEvent_Completed_NoMatchingRule(t *testing.T) {
	r, slack := newSlackRouter(t)
	r.cfg = &Config{Mirror: []MirrorRule{}}
	data := bigbandext.TaskRunCompletedData{Status: "ok", FinalMessage: "Done"}
	raw, _ := json.Marshal(data)
	r.HandleEvent(bigbandext.Envelope{
		Type:     bigbandext.TypeTaskRunCompleted,
		TaskName: "unmatched-task",
		RunID:    "unmatched-task/2024-01-01T00-00-00Z",
		Data:     raw,
	})
	if len(slack.posted) != 0 {
		t.Errorf("expected no Slack post, got %d", len(slack.posted))
	}
}

// --- HandleSlackMessage: thread reply ---

func TestHandleSlackMessage_ThreadFound(t *testing.T) {
	r, _ := newSlackRouter(t)
	r.cfg = &Config{Threads: ThreadConfig{Enabled: true}}
	r.bb = startSlackFakeDaemon(t)

	// Seed store with a thread mapping (LinkTaskMeta before LinkRun mirrors
	// the real event order: folder/session staged before completion is posted).
	_ = r.store.LinkTaskMeta("my-task", t.TempDir(), "")
	_ = r.store.SetTaskSessionID("my-task", "session-xyz")
	_ = r.store.LinkRun("run-123", "my-task", "C-chan", "thread-ts", "session-xyz", true)

	msg := SlackMessage{
		Channel: "C-chan", User: "U1", Text: "follow up", TS: "ts-new", ThreadTS: "thread-ts",
	}
	if handled := r.HandleSlackMessage(msg); !handled {
		t.Error("expected message to be handled")
	}
}

func TestHandleSlackMessage_ThreadNotFound(t *testing.T) {
	r, slack := newSlackRouter(t)
	r.cfg = &Config{Threads: ThreadConfig{Enabled: true}}

	msg := SlackMessage{
		Channel: "C-chan", User: "U1", Text: "some text", TS: "ts-1", ThreadTS: "unknown-thread",
	}
	handled := r.HandleSlackMessage(msg)
	if handled {
		t.Error("expected false for unknown thread")
	}
	if len(slack.posted) != 0 {
		t.Errorf("expected no reply for unknown thread (silently ignored), got %d posts", len(slack.posted))
	}
}

// --- runChannelCommand: group validation ---

func TestRunChannelCommand_Valid(t *testing.T) {
	r, _ := newSlackRouter(t)
	r.bb = startSlackFakeDaemon(t)

	ch := &TriggerChannel{Channel: "C-trigger", Folder: t.TempDir()}
	cmd := &TriggerCommand{
		Match:  `^submit (?P<name>[a-z]+) (?P<prompt>.+)$`,
		Action: "submit",
		Folder: t.TempDir(),
	}
	re := regexp.MustCompile(cmd.Match)
	match := re.FindStringSubmatch("submit mytask do the thing")
	if match == nil {
		t.Fatal("regex did not match")
	}
	msg := SlackMessage{Channel: "C-trigger", TS: "ts-1"}
	// Should not panic; submit goes to fake daemon.
	r.runChannelCommand(ch, cmd, re, match, msg)
}

func TestRunChannelCommand_EmptyName(t *testing.T) {
	r, slack := newSlackRouter(t)
	ch := &TriggerChannel{Channel: "C-trigger", Folder: t.TempDir()}
	cmd := &TriggerCommand{Match: `^submit (?P<prompt>.+)$`, Action: "submit"}
	re := regexp.MustCompile(cmd.Match)
	match := re.FindStringSubmatch("submit do the thing")
	r.runChannelCommand(ch, cmd, re, match, SlackMessage{Channel: "C-trigger", TS: "ts-1"})
	if len(slack.posted) == 0 {
		t.Error("expected error ack posted when name group is missing")
	}
}

func TestRunChannelCommand_EmptyPrompt(t *testing.T) {
	// Regex with no capture groups: both name and prompt resolve to "", triggering
	// the "requires both a name and a prompt" guard before any Submit call.
	r, slack := newSlackRouter(t)
	ch := &TriggerChannel{Channel: "C-trigger", Folder: t.TempDir()}
	cmd := &TriggerCommand{Match: `^submit$`, Action: "submit"}
	re := regexp.MustCompile(cmd.Match)
	match := re.FindStringSubmatch("submit")
	r.runChannelCommand(ch, cmd, re, match, SlackMessage{Channel: "C-trigger", TS: "ts-1"})
	if len(slack.posted) == 0 {
		t.Error("expected error ack when name and prompt are both empty")
	}
}

// --- channelMatches ---

func TestChannelMatches(t *testing.T) {
	cases := []struct {
		rule, id, name string
		want           bool
	}{
		{"my-channel", "C-id", "my-channel", true},
		{"C-id", "C-id", "my-channel", true},
		{"#my-channel", "C-id", "my-channel", true},
		{"other", "C-id", "my-channel", false},
		{"", "C-id", "my-channel", false},
	}
	for _, tc := range cases {
		if got := channelMatches(tc.rule, tc.id, tc.name); got != tc.want {
			t.Errorf("channelMatches(%q,%q,%q)=%v want %v", tc.rule, tc.id, tc.name, got, tc.want)
		}
	}
}

// --- stripMention ---

func TestStripMention(t *testing.T) {
	cases := []struct{ in, want string }{
		{"<@USERID> hello", "hello"},
		{"<@U1> <@U2> do something", "do something"},
		{"no mention here", "no mention here"},
	}
	for _, tc := range cases {
		if got := stripMention(tc.in); got != tc.want {
			t.Errorf("stripMention(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}
