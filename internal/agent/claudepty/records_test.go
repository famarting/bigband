package claudepty

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseWakeup(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantDelay int
		wantStop  bool
		wantNil   bool
	}{
		{name: "valid", input: `{"delaySeconds":1200,"prompt":"keep going","reason":"poll"}`, wantDelay: 1200},
		{name: "missing delay", input: `{"prompt":"x"}`, wantNil: true},
		{name: "zero delay", input: `{"delaySeconds":0,"prompt":"x"}`, wantNil: true},
		{name: "negative delay", input: `{"delaySeconds":-5}`, wantNil: true},
		{name: "empty input", input: ``, wantNil: true},
		{name: "malformed", input: `{not-json`, wantNil: true},
		// The cancellation form carries no delay of its own, so it has to
		// survive the positive-delay check to reach the caller.
		{name: "stop", input: `{"stop":true}`, wantStop: true},
		{name: "stop with stale delay", input: `{"stop":true,"delaySeconds":600}`, wantDelay: 600, wantStop: true},
		{name: "stop false", input: `{"stop":false,"delaySeconds":60,"prompt":"x"}`, wantDelay: 60},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseWakeup(json.RawMessage(tt.input))
			if tt.wantNil {
				if got != nil {
					t.Errorf("parseWakeup(%q) = %+v, want nil", tt.input, got)
				}
				return
			}
			if got == nil || got.DelaySeconds != tt.wantDelay {
				t.Fatalf("parseWakeup(%q) = %+v, want delay=%d", tt.input, got, tt.wantDelay)
			}
			if got.Stop != tt.wantStop {
				t.Errorf("parseWakeup(%q).Stop = %v, want %v", tt.input, got.Stop, tt.wantStop)
			}
		})
	}
}

func TestParseStopTaskID(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"task id", `{"task_id":"bve3b3lqo"}`, "bve3b3lqo"},
		{"agent id", `{"task_id":"aad7cd68064192ae8"}`, "aad7cd68064192ae8"},
		{"no id", `{"other":"x"}`, ""},
		{"empty input", ``, ""},
		{"malformed", `{nope`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseStopTaskID(json.RawMessage(tt.input)); got != tt.want {
				t.Errorf("parseStopTaskID(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestBackgroundTaskID pins the id extraction against the verbatim
// acknowledgements observed in production sessions — those strings are the
// only place a task id and its tool_use id appear together.
func TestBackgroundTaskID(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			"background bash",
			"Command running in background with ID: bve3b3lqo. Output is being written to: /private/tmp/claude-501/x/tasks/bve3b3lqo.output. You will be notified when it completes.",
			"bve3b3lqo",
		},
		{
			"monitor",
			"Monitor started (task b46h39aov, timeout 2400000ms). You will be notified on each event. Keep working — do not poll or sleep.",
			"b46h39aov",
		},
		{
			"async agent",
			"Async agent launched successfully. (This tool result is internal metadata — never quote it.)\nagentId: a08c63b3717812215\nThe agent is working in the background. You will be notified automatically when it completes.",
			"a08c63b3717812215",
		},
		{"no id", "Message delivered to agent a1.", ""},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := backgroundTaskID(tt.text); got != tt.want {
				t.Errorf("backgroundTaskID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsBackgroundToolInput(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"bash explicit true", `{"command":"sleep 9","run_in_background":true}`, true},
		{"bash explicit false", `{"command":"ls","run_in_background":false}`, false},
		{"bash missing", `{"command":"ls"}`, false},
		// A real Agent input never carries run_in_background (it is async by
		// default); the flag is honoured generically if present, but Agent
		// background detection actually keys off the tool_result — see
		// TestIsAsyncAgentLaunch.
		{"agent typical (no flag)", `{"description":"d","subagent_type":"general-purpose","prompt":"p"}`, false},
		{"empty", ``, false},
		{"malformed", `garbage`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isBackgroundToolInput(json.RawMessage(tt.input)); got != tt.want {
				t.Errorf("isBackgroundToolInput(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestIsAsyncAgentLaunch(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{"async launch ack", "Async agent launched successfully.\nagentId: a0abc (internal ID)", true},
		{"async launch mid-text", "note: Async agent launched successfully for the fetch", true},
		{"synchronous output", "Both queries complete. Here are the results.\n## TASK 1", false},
		{"empty", "", false},
		{"unrelated", "Command running in the background with ID bhq", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isAsyncAgentLaunch(tt.text); got != tt.want {
				t.Errorf("isAsyncAgentLaunch(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

func TestBgCompletionToolUseID(t *testing.T) {
	mkAttachment := func(typ, mode, prompt string) string {
		a, _ := json.Marshal(map[string]any{
			"type": "attachment",
			"attachment": map[string]any{
				"type":        typ,
				"commandMode": mode,
				"prompt":      prompt,
			},
		})
		return string(a)
	}
	notification := "<task-notification>\n<task-id>tid</task-id>\n<tool-use-id>toolu_abc</tool-use-id>\n<status>completed</status>\n</task-notification>"
	tests := []struct {
		name string
		line string
		want string
	}{
		{"completed notification", mkAttachment("queued_command", "task-notification", notification), "toolu_abc"},
		{"killed status still clears", mkAttachment("queued_command", "task-notification",
			"<tool-use-id>toolu_xyz</tool-use-id>\n<status>killed</status>"), "toolu_xyz"},
		{"wrong attachment type", mkAttachment("hook_success", "task-notification", notification), ""},
		{"wrong commandMode", mkAttachment("queued_command", "other", notification), ""},
		{"no tool-use-id tag", mkAttachment("queued_command", "task-notification", "<status>completed</status>"), ""},
		{"non-attachment record", `{"type":"assistant"}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var rec sessionRecord
			if err := json.Unmarshal([]byte(tt.line), &rec); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := bgCompletionToolUseID(&rec); got != tt.want {
				t.Errorf("bgCompletionToolUseID = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAssistantThinkingTexts(t *testing.T) {
	rec := &sessionRecord{
		Type: "assistant",
		Message: json.RawMessage(`{"role":"assistant","content":[
            {"type":"thinking","thinking":"first idea"},
            {"type":"thinking","thinking":""},
            {"type":"thinking","thinking":"second idea"},
            {"type":"text","text":"hi"}
        ]}`),
	}
	got := assistantThinkingTexts(rec)
	if len(got) != 2 || got[0] != "first idea" || got[1] != "second idea" {
		t.Errorf("assistantThinkingTexts = %#v", got)
	}

	side := *rec
	side.IsSidechain = true
	if got := assistantThinkingTexts(&side); got != nil {
		t.Errorf("sidechain should be filtered out, got %#v", got)
	}
}

func TestAssistantUsage(t *testing.T) {
	rec := &sessionRecord{
		Type: "assistant",
		Message: json.RawMessage(`{"role":"assistant","content":[],"usage":{
            "input_tokens":1,
            "output_tokens":42,
            "cache_read_input_tokens":71270,
            "cache_creation_input_tokens":2290
        }}`),
	}
	u := assistantUsage(rec)
	if u == nil {
		t.Fatal("expected usage, got nil")
	}
	if u.OutputTokens != 42 || u.CacheReadInputTokens != 71270 || u.CacheCreationInputTokens != 2290 {
		t.Errorf("assistantUsage = %+v", u)
	}

	user := &sessionRecord{Type: "user", Message: rec.Message}
	if u := assistantUsage(user); u != nil {
		t.Errorf("user record should yield nil, got %+v", u)
	}
}

func TestToolResults(t *testing.T) {
	rec := &sessionRecord{
		Type: "user",
		Message: json.RawMessage(`{"role":"user","content":[
            {"type":"tool_result","tool_use_id":"t1","content":"file output here"},
            {"type":"tool_result","tool_use_id":"t2","is_error":true,"content":"Exit code 1"},
            {"type":"tool_result","tool_use_id":"t3","content":[{"type":"text","text":"hi"},{"type":"text","text":"there"}]},
            {"type":"text","text":"not a tool result"}
        ]}`),
	}
	got := toolResults(rec)
	if len(got) != 3 {
		t.Fatalf("toolResults len = %d, want 3", len(got))
	}
	if s := toolResultText(&got[0]); s != "file output here" {
		t.Errorf("string content = %q", s)
	}
	if !got[1].IsError {
		t.Error("expected IsError on second result")
	}
	if s := toolResultText(&got[2]); s != "hi\nthere" {
		t.Errorf("array content = %q", s)
	}

	side := *rec
	side.IsSidechain = true
	if got := toolResults(&side); got != nil {
		t.Errorf("sidechain user should yield nil, got %+v", got)
	}
}

func TestAssistantToolUses(t *testing.T) {
	// Mixed content: thinking + text + tool_use blocks; sidechain assistants
	// are excluded entirely.
	rec := &sessionRecord{
		Type: "assistant",
		Message: json.RawMessage(`{"role":"assistant","content":[
            {"type":"thinking","thinking":"…"},
            {"type":"text","text":"hi"},
            {"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}},
            {"type":"tool_use","id":"t2","name":"ScheduleWakeup","input":{"delaySeconds":60}}
        ]}`),
	}
	got := assistantToolUses(rec)
	if len(got) != 2 || got[0].Name != "Bash" || got[1].Name != "ScheduleWakeup" {
		t.Errorf("assistantToolUses = %+v", got)
	}

	side := *rec
	side.IsSidechain = true
	if got := assistantToolUses(&side); got != nil {
		t.Errorf("sidechain should be filtered out, got %+v", got)
	}

	user := &sessionRecord{Type: "user", Message: rec.Message}
	if got := assistantToolUses(user); got != nil {
		t.Errorf("user record should yield nil, got %+v", got)
	}
}

func TestIsAsyncLaunchResult(t *testing.T) {
	// Verbatim acknowledgements from real session transcripts.
	const (
		monitorAck     = "Monitor started (task b138ll10i, timeout 3600000ms). You will be notified on each event. Keep working — do not poll or sleep. Events may arrive while you are waiting for the user — an event is not their reply."
		sendMessageAck = `{"success":true,"message":"Agent \"a0faeab183be5d8e5\" had no active task; resumed from transcript in the background with your message. You'll be notified when it finishes. Output: /private/tmp/x/tasks/a0faeab183be5d8e5.output","resumedAgentId":"a0faeab183be5d8e5"}`
		agentAck       = "Async agent launched successfully. (This tool result is internal metadata — never quote it.)\nagentId: a08c63b3717812215\nThe agent is working in the background. You will be notified automatically when it completes."
		bashAck        = "Command running in background with ID: b27ax1yy8. Output is being written to: /tmp/b27ax1yy8.output. You will be notified when it completes. To check interim output, use Read on that file path."
	)
	tests := []struct {
		name   string
		tool   string
		result string
		want   bool
	}{
		// The regression this function exists for: an armed Monitor is the
		// only signal that a CI-watching job is still waiting on something.
		{"monitor armed", "Monitor", monitorAck, true},
		{"sendmessage backgrounded", "SendMessage", sendMessageAck, true},
		{"async agent", "Agent", agentAck, true},
		// A hypothetical future async tool is picked up on the shared promise
		// without needing to be named here.
		{"unknown async tool", "Workflow", "Workflow started (run wf_abc123). You will be notified when it completes.", true},

		// Bash is exempt: its signal is run_in_background on the tool_use.
		{"background bash exempt", "Bash", bashAck, false},
		// Content that merely quotes the promise is not a dispatch.
		{"skill doc quoting promise", "Skill", "Launching skill: monitor-help\nMonitor runs in the background and you will be notified on each event.", false},
		{"read of doc quoting promise", "Read", "1\t// You will be notified when it completes.", false},
		{"mcp payload quoting promise", "mcp__claude_ai_Notion__notion-fetch", `{"text":"You will be notified when the run completes"}`, false},
		{"sync agent prose", "Agent", "Here are the results. The job will be notified downstream.", false},
		{"oversized payload", "Monitor", strings.Repeat("x", asyncPromiseMaxLen) + " You will be notified on each event.", false},
		{"unrelated result", "Monitor", "Monitor stopped.", false},
		{"empty result", "Monitor", "", false},
		// No tool_use seen for this id: fall back to the exact Agent marker
		// rather than the loose promise.
		{"unknown tool name with agent marker", "", agentAck, true},
		{"unknown tool name with promise only", "", monitorAck, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isAsyncLaunchResult(tt.tool, tt.result); got != tt.want {
				t.Errorf("isAsyncLaunchResult(%q, %.40q…) = %v, want %v", tt.tool, tt.result, got, tt.want)
			}
		})
	}
}
