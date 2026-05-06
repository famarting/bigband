package runner

import (
	"bytes"
	"testing"
)

func TestStreamWriter(t *testing.T) {
	tests := []struct {
		name             string
		writes           []string
		wantResult       string
		wantSessionID    string
		wantActivitySubs []string
	}{
		{
			name: "text delta streaming",
			writes: []string{
				`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello "}}}` + "\n",
				`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"world"}}}` + "\n",
				`{"type":"result","result":"Hello world","session_id":"sess-123"}` + "\n",
			},
			wantResult:       "Hello world",
			wantSessionID:    "sess-123",
			wantActivitySubs: []string{"Hello ", "world"},
		},
		{
			name: "tool use with text",
			writes: []string{
				`{"type":"stream_event","event":{"type":"content_block_start","content_block":{"type":"tool_use","name":"Read"}}}` + "\n" +
					`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{\"file\":\"main.go\"}"}}}` + "\n" +
					`{"type":"stream_event","event":{"type":"content_block_stop"}}` + "\n" +
					`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Done reading."}}}` + "\n" +
					`{"type":"result","result":"Done reading.","session_id":"sess-456"}` + "\n",
			},
			wantResult:       "Done reading.",
			wantSessionID:    "sess-456",
			wantActivitySubs: []string{"Using tool: Read...", "Read done", "Done reading."},
		},
		{
			name: "api retry event",
			writes: []string{
				`{"type":"system","subtype":"api_retry","attempt":1,"max_retries":5,"retry_delay_ms":3000,"error":"rate_limit"}` + "\n",
				`{"type":"result","result":"ok","session_id":"sess-789"}` + "\n",
			},
			wantResult:       "ok",
			wantSessionID:    "sess-789",
			wantActivitySubs: []string{"[retry] Attempt 1/5, retrying in 3000ms (rate_limit)"},
		},
		{
			name: "partial line buffering across writes",
			writes: []string{
				`{"type":"result","resu`,
				`lt":"buffered","session_id":"sess-buf"}` + "\n",
			},
			wantResult:    "buffered",
			wantSessionID: "sess-buf",
		},
		{
			name: "malformed JSON lines are skipped",
			writes: []string{
				"not json at all\n",
				`{"type":"result","result":"valid","session_id":"sess-ok"}` + "\n",
			},
			wantResult:    "valid",
			wantSessionID: "sess-ok",
		},
		{
			name:          "no result event returns empty",
			writes:        []string{`{"type":"stream_event","event":{"type":"message_start"}}` + "\n"},
			wantResult:    "",
			wantSessionID: "",
		},
		{
			name: "newline between tool done and text",
			writes: []string{
				`{"type":"stream_event","event":{"type":"content_block_start","content_block":{"type":"tool_use","name":"Read"}}}` + "\n" +
					`{"type":"stream_event","event":{"type":"content_block_stop"}}` + "\n" +
					`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Let me check"}}}` + "\n" +
					`{"type":"result","result":"ok","session_id":"s"}` + "\n",
			},
			wantResult:       "ok",
			wantSessionID:    "s",
			wantActivitySubs: []string{"Read done\nLet me check"},
		},
		{
			name: "thinking block",
			writes: []string{
				`{"type":"stream_event","event":{"type":"content_block_start","content_block":{"type":"thinking"}}}` + "\n" +
					`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"I need to review"}}}` + "\n" +
					`{"type":"stream_event","event":{"type":"content_block_stop"}}` + "\n" +
					`{"type":"result","result":"done","session_id":"sess-think"}` + "\n",
			},
			wantResult:       "done",
			wantSessionID:    "sess-think",
			wantActivitySubs: []string{"Thinking...", "I need to review", "Thinking done"},
		},
		{
			name: "server_tool_use",
			writes: []string{
				`{"type":"stream_event","event":{"type":"content_block_start","content_block":{"type":"server_tool_use","name":"WebSearch"}}}` + "\n" +
					`{"type":"stream_event","event":{"type":"content_block_stop"}}` + "\n" +
					`{"type":"result","result":"searched","session_id":"sess-srv"}` + "\n",
			},
			wantResult:       "searched",
			wantSessionID:    "sess-srv",
			wantActivitySubs: []string{"Using tool: WebSearch...", "WebSearch done"},
		},
		{
			name: "system init event",
			writes: []string{
				`{"type":"system","subtype":"init","session_id":"sess-init"}` + "\n" +
					`{"type":"result","result":"ok","session_id":"sess-init"}` + "\n",
			},
			wantResult:       "ok",
			wantSessionID:    "sess-init",
			wantActivitySubs: []string{"Session started"},
		},
		{
			name: "text delta suppressed during tool use",
			writes: []string{
				`{"type":"stream_event","event":{"type":"content_block_start","content_block":{"type":"tool_use","name":"Bash"}}}` + "\n" +
					`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"should not appear"}}}` + "\n" +
					`{"type":"stream_event","event":{"type":"content_block_stop"}}` + "\n" +
					`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"visible"}}}` + "\n" +
					`{"type":"result","result":"done","session_id":"sess-tool"}` + "\n",
			},
			wantResult:       "done",
			wantSessionID:    "sess-tool",
			wantActivitySubs: []string{"Using tool: Bash...", "Bash done", "visible"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var raw, activity bytes.Buffer
			sw := newStreamWriter(&raw, &activity)

			for _, w := range tt.writes {
				n, err := sw.Write([]byte(w))
				if err != nil {
					t.Fatalf("Write() error: %v", err)
				}
				if n != len(w) {
					t.Fatalf("Write() = %d, want %d", n, len(w))
				}
			}

			gotResult, gotSessionID := sw.getResult()
			if gotResult != tt.wantResult {
				t.Errorf("result = %q, want %q", gotResult, tt.wantResult)
			}
			if gotSessionID != tt.wantSessionID {
				t.Errorf("sessionID = %q, want %q", gotSessionID, tt.wantSessionID)
			}

			activityStr := activity.String()
			for _, sub := range tt.wantActivitySubs {
				if !bytes.Contains([]byte(activityStr), []byte(sub)) {
					t.Errorf("activity missing %q\ngot: %q", sub, activityStr)
				}
			}

			if tt.name == "text delta suppressed during tool use" {
				if bytes.Contains([]byte(activityStr), []byte("should not appear")) {
					t.Error("activity should not contain text delta emitted during tool use")
				}
			}
		})
	}
}

func TestStreamWriterNilActivity(t *testing.T) {
	var raw bytes.Buffer
	sw := newStreamWriter(&raw, nil)

	input := `{"type":"result","result":"test","session_id":"s1"}` + "\n"
	n, err := sw.Write([]byte(input))
	if err != nil {
		t.Fatalf("Write() error: %v", err)
	}
	if n != len(input) {
		t.Fatalf("Write() = %d, want %d", n, len(input))
	}

	gotResult, gotSessionID := sw.getResult()
	if gotResult != "test" {
		t.Errorf("result = %q, want %q", gotResult, "test")
	}
	if gotSessionID != "s1" {
		t.Errorf("sessionID = %q, want %q", gotSessionID, "s1")
	}
}
