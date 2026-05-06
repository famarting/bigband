package runner

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

// streamWriter wraps an io.Writer and parses Claude --output-format stream-json
// NDJSON lines, writing human-readable activity to a separate writer while
// forwarding raw bytes to the underlying writer.
type streamWriter struct {
	raw      io.Writer
	activity io.Writer

	mu         sync.Mutex
	lineBuf    []byte
	result     string
	sessionID  string
	inTool     bool
	toolName   string
	inputBuf   string
	wakeup     *WakeupRequest
	inThinking bool
	needsNL    bool
}

func newStreamWriter(raw, activity io.Writer) *streamWriter {
	return &streamWriter{raw: raw, activity: activity}
}

func (sw *streamWriter) Write(p []byte) (int, error) {
	n, err := sw.raw.Write(p)
	if err != nil {
		return n, err
	}
	sw.mu.Lock()
	defer sw.mu.Unlock()
	sw.lineBuf = append(sw.lineBuf, p[:n]...)
	sw.processLines()
	return n, nil
}

func (sw *streamWriter) getResult() (string, string) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.result, sw.sessionID
}

func (sw *streamWriter) getWakeup() *WakeupRequest {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.wakeup
}

func (sw *streamWriter) processLines() {
	for {
		idx := -1
		for i, b := range sw.lineBuf {
			if b == '\n' {
				idx = i
				break
			}
		}
		if idx < 0 {
			break
		}
		line := sw.lineBuf[:idx]
		sw.lineBuf = sw.lineBuf[idx+1:]
		if len(line) > 0 {
			sw.parseLine(line)
		}
	}
}

type streamEvent struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype,omitempty"`
	Event     json.RawMessage `json:"event"`
	Result    string          `json:"result,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	Attempt    int            `json:"attempt,omitempty"`
	MaxRetries int            `json:"max_retries,omitempty"`
	RetryDelay int            `json:"retry_delay_ms,omitempty"`
	Error      string         `json:"error,omitempty"`
}

type apiStreamEvent struct {
	Type         string       `json:"type"`
	ContentBlock contentBlock `json:"content_block"`
	Delta        deltaPayload `json:"delta"`
}

type contentBlock struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

type deltaPayload struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
}

// WakeupRequest holds the parameters Claude passed to ScheduleWakeup.
type WakeupRequest struct {
	DelaySeconds int    `json:"delaySeconds"`
	Prompt       string `json:"prompt"`
}

func (sw *streamWriter) parseLine(line []byte) {
	var evt streamEvent
	if err := json.Unmarshal(line, &evt); err != nil {
		return
	}
	switch evt.Type {
	case "stream_event":
		sw.handleStreamEvent(evt.Event)
	case "result":
		sw.result = evt.Result
		sw.sessionID = evt.SessionID
	case "system":
		sw.handleSystemEvent(evt)
	}
}

func (sw *streamWriter) handleStreamEvent(raw json.RawMessage) {
	if raw == nil {
		return
	}
	var evt apiStreamEvent
	if err := json.Unmarshal(raw, &evt); err != nil {
		return
	}
	switch evt.Type {
	case "content_block_start":
		switch evt.ContentBlock.Type {
		case "tool_use", "server_tool_use":
			if evt.ContentBlock.Name != "" {
				sw.inTool = true
				sw.toolName = evt.ContentBlock.Name
				sw.inputBuf = ""
				sw.writeActivity(fmt.Sprintf("Using tool: %s...", evt.ContentBlock.Name))
			}
		case "thinking":
			sw.inThinking = true
			sw.writeActivity("Thinking...")
		}
	case "content_block_delta":
		switch evt.Delta.Type {
		case "text_delta":
			if evt.Delta.Text != "" && !sw.inTool {
				sw.writeActivityRaw(evt.Delta.Text)
			}
		case "thinking_delta":
			if evt.Delta.Thinking != "" {
				sw.writeActivityRaw(evt.Delta.Thinking)
			}
		case "input_json_delta":
			if sw.inTool && sw.toolName == "ScheduleWakeup" {
				sw.inputBuf += evt.Delta.PartialJSON
			}
		}
	case "content_block_stop":
		switch {
		case sw.inTool:
			if sw.toolName == "ScheduleWakeup" && sw.inputBuf != "" {
				var req WakeupRequest
				if err := json.Unmarshal([]byte(sw.inputBuf), &req); err == nil && req.DelaySeconds > 0 {
					sw.wakeup = &req
				}
				sw.inputBuf = ""
			}
			sw.writeActivity(fmt.Sprintf("%s done", sw.toolName))
			sw.inTool = false
			sw.toolName = ""
		case sw.inThinking:
			sw.writeActivity("Thinking done")
			sw.inThinking = false
		}
	}
}

func (sw *streamWriter) handleSystemEvent(evt streamEvent) {
	switch evt.Subtype {
	case "init":
		if evt.SessionID != "" {
			sw.sessionID = evt.SessionID
		}
		sw.writeActivity("Session started")
	case "api_retry":
		sw.writeActivity(fmt.Sprintf("[retry] Attempt %d/%d, retrying in %dms (%s)",
			evt.Attempt, evt.MaxRetries, evt.RetryDelay, evt.Error))
	}
}

func (sw *streamWriter) writeActivity(msg string) {
	if sw.activity == nil {
		return
	}
	ts := time.Now().Format("15:04:05")
	fmt.Fprintf(sw.activity, "\n[%s] %s", ts, msg)
	sw.needsNL = true
}

func (sw *streamWriter) writeActivityRaw(text string) {
	if sw.activity == nil {
		return
	}
	if sw.needsNL {
		fmt.Fprint(sw.activity, "\n")
		sw.needsNL = false
	}
	fmt.Fprint(sw.activity, text)
}
