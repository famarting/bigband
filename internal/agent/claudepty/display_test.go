package claudepty

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestSummarizeToolUse(t *testing.T) {
	tests := []struct {
		name      string
		block     contentBlock
		wantName  string
		wantInDet string // substring expected in detail
	}{
		{
			name:      "bash",
			block:     contentBlock{Name: "Bash", Input: json.RawMessage(`{"command":"go test ./...","run_in_background":false}`)},
			wantName:  "Bash",
			wantInDet: "$ go test ./...",
		},
		{
			name:      "bash background",
			block:     contentBlock{Name: "Bash", Input: json.RawMessage(`{"command":"sleep 9","run_in_background":true}`)},
			wantName:  "Bash",
			wantInDet: " &",
		},
		{
			name:      "read",
			block:     contentBlock{Name: "Read", Input: json.RawMessage(`{"file_path":"/tmp/foo.go"}`)},
			wantName:  "Read",
			wantInDet: "/tmp/foo.go",
		},
		{
			name:      "edit",
			block:     contentBlock{Name: "Edit", Input: json.RawMessage(`{"file_path":"/tmp/bar.go","replace_all":true}`)},
			wantName:  "Edit",
			wantInDet: "replace_all",
		},
		{
			name:      "grep",
			block:     contentBlock{Name: "Grep", Input: json.RawMessage(`{"pattern":"foo","path":"/x","output_mode":"files_with_matches"}`)},
			wantName:  "Grep",
			wantInDet: "files_with_matches",
		},
		{
			name:      "todowrite",
			block:     contentBlock{Name: "TodoWrite", Input: json.RawMessage(`{"todos":[{"content":"a","status":"pending"},{"content":"b","status":"in_progress"},{"content":"c","status":"completed"}]}`)},
			wantName:  "TodoWrite",
			wantInDet: "3 item(s) — 1 todo, 1 doing, 1 done",
		},
		{
			name:      "schedule wakeup",
			block:     contentBlock{Name: "ScheduleWakeup", Input: json.RawMessage(`{"delaySeconds":600,"reason":"poll CI"}`)},
			wantName:  "ScheduleWakeup",
			wantInDet: "+600s poll CI",
		},
		{
			name:      "unknown tool",
			block:     contentBlock{Name: "Mystery", Input: json.RawMessage(`{"x":1}`)},
			wantName:  "Mystery",
			wantInDet: `{"x":1}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, detail := summarizeToolUse(tt.block)
			if name != tt.wantName {
				t.Errorf("name = %q, want %q", name, tt.wantName)
			}
			if !strings.Contains(detail, tt.wantInDet) {
				t.Errorf("detail = %q, want substring %q", detail, tt.wantInDet)
			}
		})
	}
}

func TestSingleLineTruncate(t *testing.T) {
	tests := []struct {
		in    string
		limit int
		want  string
	}{
		{"", 10, ""},
		{"   ", 10, ""},
		{"short", 10, "short"},
		{"hello\nworld\there  too", 100, "hello world here too"},
		{"abcdefghij", 5, "abcde…"},
	}
	for _, tt := range tests {
		got := singleLineTruncate(tt.in, tt.limit)
		if got != tt.want {
			t.Errorf("singleLineTruncate(%q, %d) = %q, want %q", tt.in, tt.limit, got, tt.want)
		}
	}
}

func TestEmitToolResult(t *testing.T) {
	var log, live bytes.Buffer
	emitToolResult(&log, &live, contentBlock{Type: "tool_result", Content: json.RawMessage(`"all good"`)})
	if !strings.Contains(log.String(), "↵ all good") {
		t.Errorf("success log = %q", log.String())
	}
	if !strings.Contains(live.String(), "all good") || strings.Contains(live.String(), "✗") {
		t.Errorf("success live = %q", live.String())
	}

	log.Reset()
	live.Reset()
	emitToolResult(&log, &live, contentBlock{Type: "tool_result", IsError: true, Content: json.RawMessage(`"boom\n\n\nextra extra extra extra extra extra extra extra extra extra extra extra extra extra extra extra extra extra extra extra extra extra extra extra extra extra extra extra extra extra extra extra extra extra extra extra extra extra extra extra extra extra extra extra extra extra extra extra extra"`)})
	if !strings.Contains(log.String(), "✗") {
		t.Errorf("error log missing marker: %q", log.String())
	}
	if !strings.HasSuffix(strings.TrimRight(log.String(), "\n"), "…") {
		t.Errorf("expected truncation ellipsis, got %q", log.String())
	}
}

func TestEmitTodoDelta(t *testing.T) {
	var log, live bytes.Buffer
	prev := map[string]string{"a": "pending", "b": "in_progress"}
	next := map[string]string{"a": "completed", "b": "in_progress", "c": "pending"}
	emitTodoDelta(&log, &live, prev, next)
	out := log.String()
	if !strings.Contains(out, "[x] a (pending→completed)") {
		t.Errorf("missing status-change line: %q", out)
	}
	if !strings.Contains(out, "[ ] c (new)") {
		t.Errorf("missing new line: %q", out)
	}
	if strings.Contains(out, "b ") {
		// "b" did not change; should not appear (look for content followed by space).
		t.Errorf("unchanged item should not appear: %q", out)
	}

	// No-op case
	log.Reset()
	live.Reset()
	emitTodoDelta(&log, &live, next, next)
	if log.Len() != 0 || live.Len() != 0 {
		t.Errorf("identical maps should emit nothing: log=%q live=%q", log.String(), live.String())
	}
}

func TestHumanTokens(t *testing.T) {
	tests := []struct {
		in   int
		want string
	}{
		{500, "500"},
		{1234, "1.2k"},
		{9999, "10.0k"},
		{71270, "71k"},
		{1_500_000, "1.5M"},
	}
	for _, tt := range tests {
		if got := humanTokens(tt.in); got != tt.want {
			t.Errorf("humanTokens(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
