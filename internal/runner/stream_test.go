package runner

import (
	"bytes"
	"strings"
	"testing"
)

// helper: drive a list of writes through a streamWriter without color so
// assertions can match exact text without ANSI escapes.
func drive(t *testing.T, writes []string) (log, live string, sw *streamWriter) {
	t.Helper()
	var raw, logBuf, liveBuf bytes.Buffer
	sw = newStreamWriter(&raw, &logBuf, &liveBuf, false)
	for _, w := range writes {
		n, err := sw.Write([]byte(w))
		if err != nil {
			t.Fatalf("Write() error: %v", err)
		}
		if n != len(w) {
			t.Fatalf("Write() = %d, want %d", n, len(w))
		}
	}
	return logBuf.String(), liveBuf.String(), sw
}

func TestStreamWriter_TextDeltaStreaming(t *testing.T) {
	log, live, sw := drive(t, []string{
		`{"type":"stream_event","event":{"type":"content_block_start","content_block":{"type":"text"}}}` + "\n",
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello "}}}` + "\n",
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"world"}}}` + "\n",
		`{"type":"stream_event","event":{"type":"content_block_stop"}}` + "\n",
		`{"type":"result","result":"Hello world","session_id":"sess-123","subtype":"success","num_turns":1,"duration_ms":1234}` + "\n",
	})
	gotResult, gotSession := sw.getResult()
	if gotResult != "Hello world" || gotSession != "sess-123" {
		t.Errorf("result/session = %q/%q, want %q/%q", gotResult, gotSession, "Hello world", "sess-123")
	}
	// Live: streamed char-by-char with prefix and trailing newline.
	if !strings.Contains(live, "● Hello world\n") {
		t.Errorf("live missing streamed text:\n%s", live)
	}
	// Log: coalesced single line with timestamp.
	if !strings.Contains(log, "● Hello world\n") {
		t.Errorf("log missing coalesced text:\n%s", log)
	}
	// Result footer.
	if !strings.Contains(log, "claude: success") || !strings.Contains(log, "1 turns") || !strings.Contains(log, "1.2s") {
		t.Errorf("log missing result footer:\n%s", log)
	}
}

func TestStreamWriter_ToolCallFromAssistantMessage(t *testing.T) {
	log, live, _ := drive(t, []string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"main.go"}}]}}` + "\n",
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"line1\nline2\nline3\n"}]}}` + "\n",
	})
	for _, want := range []string{"● Read(main.go)", "⎿  Read 3 lines"} {
		if !strings.Contains(log, want) {
			t.Errorf("log missing %q:\n%s", want, log)
		}
		if !strings.Contains(live, want) {
			t.Errorf("live missing %q:\n%s", want, live)
		}
	}
}

func TestStreamWriter_BashToolWithDescription(t *testing.T) {
	log, _, _ := drive(t, []string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go test ./...","description":"Run tests"}}]}}` + "\n",
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"ok    ./pkg    0.123s\nok    ./pkg2   0.456s\n"}]}}` + "\n",
	})
	if !strings.Contains(log, "● Bash(Run tests)") {
		t.Errorf("expected description in tool call: %s", log)
	}
	if !strings.Contains(log, "ok    ./pkg    0.123s (+1 more lines)") {
		t.Errorf("expected first-line + count summary: %s", log)
	}
}

func TestStreamWriter_BashWithoutDescription(t *testing.T) {
	log, _, _ := drive(t, []string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls -la"}}]}}` + "\n",
	})
	if !strings.Contains(log, "● Bash(ls -la)") {
		t.Errorf("expected command fallback: %s", log)
	}
}

func TestStreamWriter_TodoWriteRendersChecklist(t *testing.T) {
	log, _, _ := drive(t, []string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"TodoWrite","input":{"todos":[` +
			`{"content":"Plan","status":"completed","activeForm":"Planning"},` +
			`{"content":"Implement","status":"in_progress","activeForm":"Implementing"},` +
			`{"content":"Test","status":"pending","activeForm":"Testing"}` +
			`]}}]}}` + "\n",
	})
	for _, want := range []string{"● TodoWrite", "☒ Plan", "▶ Implement", "☐ Test"} {
		if !strings.Contains(log, want) {
			t.Errorf("log missing %q:\n%s", want, log)
		}
	}
}

func TestStreamWriter_ThinkingBlock(t *testing.T) {
	log, live, _ := drive(t, []string{
		`{"type":"stream_event","event":{"type":"content_block_start","content_block":{"type":"thinking"}}}` + "\n",
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"I should read the file"}}}` + "\n",
		`{"type":"stream_event","event":{"type":"content_block_stop"}}` + "\n",
	})
	if !strings.Contains(log, "✻ I should read the file\n") {
		t.Errorf("log missing thinking line:\n%s", log)
	}
	if !strings.Contains(live, "✻ I should read the file\n") {
		t.Errorf("live missing thinking line:\n%s", live)
	}
}

func TestStreamWriter_ToolErrorResult(t *testing.T) {
	log, _, _ := drive(t, []string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"missing.go"}}]}}` + "\n",
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":"file not found: missing.go"}]}}` + "\n",
	})
	if !strings.Contains(log, "⎿  Error: file not found") {
		t.Errorf("expected error-prefixed result: %s", log)
	}
}

func TestStreamWriter_ToolResultArrayContent(t *testing.T) {
	log, _, _ := drive(t, []string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"x.go"}}]}}` + "\n",
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"a\nb\n"}]}]}}` + "\n",
	})
	if !strings.Contains(log, "⎿  Read 2 lines") {
		t.Errorf("expected array content summary: %s", log)
	}
}

func TestStreamWriter_MCPToolNamespaceRender(t *testing.T) {
	log, _, _ := drive(t, []string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"mcp__claude_ai_Linear__list_issues","input":{"query":"active"}}]}}` + "\n",
	})
	if !strings.Contains(log, "● claude_ai_Linear: list_issues(query=active)") {
		t.Errorf("expected MCP namespace split: %s", log)
	}
}

func TestStreamWriter_ScheduleWakeupCaptured(t *testing.T) {
	_, _, sw := drive(t, []string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"ScheduleWakeup","input":{"delaySeconds":1200,"prompt":"<<autonomous-loop-dynamic>>","reason":"idle tick"}}]}}` + "\n",
	})
	w := sw.getWakeup()
	if w == nil || w.DelaySeconds != 1200 || w.Prompt != "<<autonomous-loop-dynamic>>" {
		t.Errorf("wakeup = %+v, want delay=1200 prompt=<<autonomous-loop-dynamic>>", w)
	}
}

func TestStreamWriter_ResultMetadataFooter(t *testing.T) {
	log, _, _ := drive(t, []string{
		`{"type":"result","subtype":"success","result":"ok","session_id":"s","num_turns":4,"duration_ms":12300,"total_cost_usd":0.0873,"usage":{"input_tokens":1000,"output_tokens":3000,"cache_read_input_tokens":23000}}` + "\n",
	})
	for _, want := range []string{"claude: success", "4 turns", "12.3s", "$0.0873", "24k in / 3k out"} {
		if !strings.Contains(log, want) {
			t.Errorf("log missing %q:\n%s", want, log)
		}
	}
}

func TestStreamWriter_ResultErrorSubtype(t *testing.T) {
	log, _, _ := drive(t, []string{
		`{"type":"result","subtype":"error_max_turns","result":"hit limit","session_id":"s","num_turns":50}` + "\n",
	})
	if !strings.Contains(log, "claude: error_max_turns") {
		t.Errorf("expected error subtype in footer: %s", log)
	}
}

func TestStreamWriter_APIRetryEvent(t *testing.T) {
	log, _, _ := drive(t, []string{
		`{"type":"system","subtype":"api_retry","attempt":1,"max_retries":5,"retry_delay_ms":3000,"error":"rate_limit"}` + "\n",
	})
	if !strings.Contains(log, "[retry] Attempt 1/5, retrying in 3000ms (rate_limit)") {
		t.Errorf("expected retry line: %s", log)
	}
}

func TestStreamWriter_SystemInit(t *testing.T) {
	log, _, sw := drive(t, []string{
		`{"type":"system","subtype":"init","session_id":"sess-init"}` + "\n",
	})
	if !strings.Contains(log, "Session started") {
		t.Errorf("expected session-started line: %s", log)
	}
	if _, sid := sw.getResult(); sid != "sess-init" {
		t.Errorf("session id from init not captured: %q", sid)
	}
}

func TestStreamWriter_PartialLineBuffering(t *testing.T) {
	_, _, sw := drive(t, []string{
		`{"type":"result","resu`,
		`lt":"buffered","session_id":"sess-buf"}` + "\n",
	})
	gotResult, gotSession := sw.getResult()
	if gotResult != "buffered" || gotSession != "sess-buf" {
		t.Errorf("result/session = %q/%q, want buffered/sess-buf", gotResult, gotSession)
	}
}

func TestStreamWriter_MalformedJSONIgnored(t *testing.T) {
	_, _, sw := drive(t, []string{
		"not json at all\n",
		`{"type":"result","result":"valid","session_id":"sess-ok"}` + "\n",
	})
	if r, _ := sw.getResult(); r != "valid" {
		t.Errorf("result = %q, want valid", r)
	}
}

func TestStreamWriter_NilWritersSafe(t *testing.T) {
	sw := newStreamWriter(nil, nil, nil, false)
	input := `{"type":"result","result":"test","session_id":"s1"}` + "\n"
	if _, err := sw.Write([]byte(input)); err != nil {
		t.Fatalf("Write() error: %v", err)
	}
	if r, sid := sw.getResult(); r != "test" || sid != "s1" {
		t.Errorf("result/sid = %q/%q, want test/s1", r, sid)
	}
}

func TestStreamWriter_ColorEscapesOnlyOnLive(t *testing.T) {
	var raw, logBuf, liveBuf bytes.Buffer
	sw := newStreamWriter(&raw, &logBuf, &liveBuf, true)
	input := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"x.go"}}]}}` + "\n"
	if _, err := sw.Write([]byte(input)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logBuf.String(), "\x1b[") {
		t.Errorf("log must not contain ANSI escapes: %q", logBuf.String())
	}
	if !strings.Contains(liveBuf.String(), "\x1b[") {
		t.Errorf("live must contain ANSI escapes when liveColor=true: %q", liveBuf.String())
	}
}

func TestFormatToolCall(t *testing.T) {
	cases := []struct {
		name  string
		tool  string
		input string
		want  string
	}{
		{"read with offset", "Read", `{"file_path":"a.go","offset":10,"limit":20}`, "Read(a.go, lines 10-29)"},
		{"read no offset", "Read", `{"file_path":"a.go"}`, "Read(a.go)"},
		{"edit", "Edit", `{"file_path":"x.go","old_string":"a","new_string":"b"}`, "Edit(x.go)"},
		{"grep with path", "Grep", `{"pattern":"foo","path":"src/"}`, "Grep(foo in src/)"},
		{"grep no path", "Grep", `{"pattern":"foo"}`, "Grep(foo)"},
		{"glob", "Glob", `{"pattern":"**/*.go"}`, "Glob(**/*.go)"},
		{"task with desc", "Task", `{"description":"audit deps","subagent_type":"general-purpose"}`, "Task(audit deps)"},
		{"webfetch", "WebFetch", `{"url":"https://x.com"}`, "WebFetch(https://x.com)"},
		{"websearch", "WebSearch", `{"query":"go generics"}`, "WebSearch(go generics)"},
		{"schedulewakeup", "ScheduleWakeup", `{"delaySeconds":600,"reason":"poll build"}`, "ScheduleWakeup(600s: poll build)"},
		{"unknown tool", "FooBar", `{"x":"a","y":1}`, "FooBar(x=a, y=1)"},
		{"unknown empty", "FooBar", `{}`, "FooBar()"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatToolCall(tc.tool, []byte(tc.input))
			if got != tc.want {
				t.Errorf("formatToolCall(%s, %s) = %q, want %q", tc.tool, tc.input, got, tc.want)
			}
		})
	}
}

func TestHumanTokens(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{500, "500"},
		{1000, "1k"},
		{24500, "24k"},
		{1_500_000, "1.5M"},
	}
	for _, tc := range cases {
		if got := humanTokens(tc.n); got != tc.want {
			t.Errorf("humanTokens(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}
