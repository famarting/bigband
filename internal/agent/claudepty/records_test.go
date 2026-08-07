package claudepty

import (
	"encoding/json"
	"testing"
)

func TestParseWakeup(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantDelay int
		wantNil   bool
	}{
		{"valid", `{"delaySeconds":1200,"prompt":"keep going","reason":"poll"}`, 1200, false},
		{"missing delay", `{"prompt":"x"}`, 0, true},
		{"zero delay", `{"delaySeconds":0,"prompt":"x"}`, 0, true},
		{"negative delay", `{"delaySeconds":-5}`, 0, true},
		{"empty input", ``, 0, true},
		{"malformed", `{not-json`, 0, true},
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
				t.Errorf("parseWakeup(%q) = %+v, want delay=%d", tt.input, got, tt.wantDelay)
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
