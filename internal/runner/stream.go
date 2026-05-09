package runner

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// ANSI color escapes; emitted only when liveColor is true.
const (
	cReset  = "\x1b[0m"
	cBold   = "\x1b[1m"
	cDim    = "\x1b[2m"
	cItalic = "\x1b[3m"
	cRed    = "\x1b[31m"
	cGreen  = "\x1b[32m"
	cYellow = "\x1b[33m"
	cCyan   = "\x1b[36m"
	cGray   = "\x1b[90m"
)

// streamWriter parses Claude --output-format stream-json NDJSON output.
//
// It writes a plain-text rendering to log (intended for log files) and a
// possibly-colorized rendering to live (intended for the terminal). raw
// receives the original NDJSON bytes unchanged; pass io.Discard to drop them.
//
// Tool calls and tool results are taken from complete assistant/user messages
// to avoid parsing partial input_json fragments. Text and thinking are still
// streamed live via stream_event deltas (those are plain strings, not JSON).
type streamWriter struct {
	raw       io.Writer
	log       io.Writer
	live      io.Writer
	liveColor bool

	mu      sync.Mutex
	lineBuf []byte

	// State for live text/thinking streaming.
	inText         bool
	textBuf        strings.Builder
	textOnLive     bool
	inThinking     bool
	thinkingBuf    strings.Builder
	thinkingOnLive bool

	// tool_use_id → tool name. Populated from assistant messages so that
	// later user-message tool_result events can be rendered with the right
	// per-tool formatter.
	toolByID map[string]string

	// Top-level result fields.
	result    string
	sessionID string
	wakeup    *WakeupRequest

	// Final-message capture: lastTextBlock holds the most recently completed
	// text content block. On each result event it is promoted into
	// finalMessage and then reset, so finalMessage always reflects the last
	// non-empty assistant text from the most recent turn.
	lastTextBlock string
	finalMessage  string
}

// WakeupRequest holds the parameters Claude passed to ScheduleWakeup.
type WakeupRequest struct {
	DelaySeconds int    `json:"delaySeconds"`
	Prompt       string `json:"prompt"`
}

func newStreamWriter(raw, log, live io.Writer, liveColor bool) *streamWriter {
	if raw == nil {
		raw = io.Discard
	}
	if log == nil {
		log = io.Discard
	}
	if live == nil {
		live = io.Discard
	}
	return &streamWriter{
		raw:       raw,
		log:       log,
		live:      live,
		liveColor: liveColor,
		toolByID:  map[string]string{},
	}
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

// getFinalMessage returns the most recent non-empty assistant text block from
// the most recent turn (set on each result event). Empty when the run produced
// no assistant text — e.g. ended on a tool call.
func (sw *streamWriter) getFinalMessage() string {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.finalMessage
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

// topLevelEvent is the umbrella shape for one NDJSON line. Per-type fields are
// populated only when relevant; the rest stay zero.
type topLevelEvent struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype,omitempty"`
	Event     json.RawMessage `json:"event,omitempty"`
	Message   json.RawMessage `json:"message,omitempty"`
	Result    string          `json:"result,omitempty"`
	SessionID string          `json:"session_id,omitempty"`

	// system / api_retry
	Attempt    int    `json:"attempt,omitempty"`
	MaxRetries int    `json:"max_retries,omitempty"`
	RetryDelay int    `json:"retry_delay_ms,omitempty"`
	Error      string `json:"error,omitempty"`

	// result event
	DurationMs   int             `json:"duration_ms,omitempty"`
	NumTurns     int             `json:"num_turns,omitempty"`
	TotalCostUSD float64         `json:"total_cost_usd,omitempty"`
	Usage        json.RawMessage `json:"usage,omitempty"`
}

type apiStreamEvent struct {
	Type         string       `json:"type"`
	ContentBlock contentBlock `json:"content_block"`
	Delta        deltaPayload `json:"delta"`
}

type contentBlock struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
	ID   string `json:"id,omitempty"`
}

type deltaPayload struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
}

type assistantMessage struct {
	Content []assistantContentBlock `json:"content"`
}

type assistantContentBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	Thinking string          `json:"thinking,omitempty"`
	ID       string          `json:"id,omitempty"`
	Name     string          `json:"name,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
}

type userMessage struct {
	Content []userContentBlock `json:"content"`
}

type userContentBlock struct {
	Type      string          `json:"type"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

func (sw *streamWriter) parseLine(line []byte) {
	var evt topLevelEvent
	if err := json.Unmarshal(line, &evt); err != nil {
		return
	}
	switch evt.Type {
	case "stream_event":
		sw.handleStreamEvent(evt.Event)
	case "assistant":
		sw.handleAssistantMessage(evt.Message)
	case "user":
		sw.handleUserMessage(evt.Message)
	case "result":
		sw.handleResult(&evt)
	case "system":
		sw.handleSystem(&evt)
	}
}

// handleStreamEvent only drives live text/thinking streaming and the per-block
// log-coalescing. Tool use is intentionally not handled here — the complete
// assistant message is used instead, which avoids any partial-JSON parsing.
func (sw *streamWriter) handleStreamEvent(raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var evt apiStreamEvent
	if err := json.Unmarshal(raw, &evt); err != nil {
		return
	}
	switch evt.Type {
	case "content_block_start":
		switch evt.ContentBlock.Type {
		case "text":
			sw.inText = true
			sw.textBuf.Reset()
			sw.textOnLive = false
		case "thinking":
			sw.inThinking = true
			sw.thinkingBuf.Reset()
			sw.thinkingOnLive = false
		}
	case "content_block_delta":
		switch evt.Delta.Type {
		case "text_delta":
			if !sw.inText || evt.Delta.Text == "" {
				return
			}
			sw.textBuf.WriteString(evt.Delta.Text)
			if !sw.textOnLive {
				sw.writeLive(sw.color("● ", cBold))
				sw.textOnLive = true
			}
			sw.writeLive(evt.Delta.Text)
		case "thinking_delta":
			if !sw.inThinking || evt.Delta.Thinking == "" {
				return
			}
			sw.thinkingBuf.WriteString(evt.Delta.Thinking)
			if !sw.thinkingOnLive {
				sw.writeLive(sw.color("✻ ", cDim+cItalic))
				sw.thinkingOnLive = true
			}
			sw.writeLive(sw.color(evt.Delta.Thinking, cDim+cItalic))
		}
	case "content_block_stop":
		switch {
		case sw.inText:
			text := strings.TrimRight(sw.textBuf.String(), "\n")
			if text != "" {
				sw.writeLog(stamp() + "● " + text + "\n")
				sw.lastTextBlock = text
			}
			if sw.textOnLive {
				sw.writeLive("\n")
			}
			sw.inText = false
			sw.textOnLive = false
			sw.textBuf.Reset()
		case sw.inThinking:
			text := strings.TrimRight(sw.thinkingBuf.String(), "\n")
			if text != "" {
				sw.writeLog(stamp() + "✻ " + text + "\n")
			}
			if sw.thinkingOnLive {
				sw.writeLive("\n")
			}
			sw.inThinking = false
			sw.thinkingOnLive = false
			sw.thinkingBuf.Reset()
		}
	}
}

func (sw *streamWriter) handleAssistantMessage(raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var msg assistantMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return
	}
	for _, b := range msg.Content {
		if b.Type != "tool_use" && b.Type != "server_tool_use" {
			continue
		}
		if b.ID != "" {
			sw.toolByID[b.ID] = b.Name
		}
		sw.renderToolCall(b.Name, b.Input)
		if b.Name == "ScheduleWakeup" {
			sw.captureWakeup(b.Input)
		}
	}
}

func (sw *streamWriter) renderToolCall(name string, input json.RawMessage) {
	if name == "TodoWrite" {
		sw.renderTodoWrite(input)
		return
	}
	body := formatToolCall(name, input)
	logLine := stamp() + "● " + body + "\n"
	liveLine := sw.color("● "+body, cCyan) + "\n"
	sw.writeLog(logLine)
	sw.writeLive(liveLine)
}

func (sw *streamWriter) renderTodoWrite(input json.RawMessage) {
	header := "● TodoWrite"
	sw.writeLog(stamp() + header + "\n")
	sw.writeLive(sw.color(header, cCyan) + "\n")

	var data struct {
		Todos []struct {
			Content    string `json:"content"`
			Subject    string `json:"subject"`
			Status     string `json:"status"`
			ActiveForm string `json:"activeForm"`
		} `json:"todos"`
	}
	if err := json.Unmarshal(input, &data); err != nil {
		return
	}
	for _, t := range data.Todos {
		marker := "☐"
		col := cDim
		switch t.Status {
		case "completed":
			marker = "☒"
			col = cGreen
		case "in_progress":
			marker = "▶"
			col = cYellow
		}
		text := t.Subject
		if text == "" {
			text = t.Content
		}
		bullet := fmt.Sprintf("    %s %s", marker, text)
		sw.writeLog(bullet + "\n")
		sw.writeLive(sw.color(bullet, col) + "\n")
	}
}

func (sw *streamWriter) captureWakeup(input json.RawMessage) {
	if len(input) == 0 {
		return
	}
	var req WakeupRequest
	if err := json.Unmarshal(input, &req); err == nil && req.DelaySeconds > 0 {
		sw.wakeup = &req
	}
}

func (sw *streamWriter) handleUserMessage(raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var msg userMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return
	}
	for _, b := range msg.Content {
		if b.Type != "tool_result" || b.ToolUseID == "" {
			continue
		}
		toolName := sw.toolByID[b.ToolUseID]
		content := decodeToolResultContent(b.Content)
		summary := formatToolResult(toolName, content, b.IsError)
		line := "  ⎿  " + summary
		sw.writeLog(line + "\n")
		col := cGray
		if b.IsError {
			col = cRed
		}
		sw.writeLive(sw.color(line, col) + "\n")
	}
}

// decodeToolResultContent handles the two shapes Claude emits for tool_result
// content: a JSON string, or an array of content blocks (the array form is
// used when results contain images alongside text — we keep the text only).
func decodeToolResultContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var b strings.Builder
		for _, blk := range blocks {
			if blk.Type == "text" {
				b.WriteString(blk.Text)
			}
		}
		return b.String()
	}
	return ""
}

func (sw *streamWriter) handleResult(evt *topLevelEvent) {
	sw.result = evt.Result
	if evt.SessionID != "" {
		sw.sessionID = evt.SessionID
	}
	if sw.lastTextBlock != "" {
		sw.finalMessage = sw.lastTextBlock
		sw.lastTextBlock = ""
	}

	subtype := evt.Subtype
	if subtype == "" {
		subtype = "done"
	}
	parts := []string{"claude: " + subtype}
	if evt.NumTurns > 0 {
		parts = append(parts, fmt.Sprintf("%d turns", evt.NumTurns))
	}
	if evt.DurationMs > 0 {
		parts = append(parts, fmt.Sprintf("%.1fs", float64(evt.DurationMs)/1000))
	}
	if evt.TotalCostUSD > 0 {
		parts = append(parts, fmt.Sprintf("$%.4f", evt.TotalCostUSD))
	}
	if usage := formatUsage(evt.Usage); usage != "" {
		parts = append(parts, usage)
	}
	line := "=== " + strings.Join(parts, " | ") + " ==="
	sw.writeLog(line + "\n")
	col := cCyan
	if evt.Subtype != "" && evt.Subtype != "success" {
		col = cYellow
	}
	sw.writeLive(sw.color(line, col) + "\n")
}

func formatUsage(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var u struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	}
	if err := json.Unmarshal(raw, &u); err != nil {
		return ""
	}
	in := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	if in == 0 && u.OutputTokens == 0 {
		return ""
	}
	return fmt.Sprintf("%s in / %s out", humanTokens(in), humanTokens(u.OutputTokens))
}

func humanTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1000:
		return fmt.Sprintf("%dk", n/1000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func (sw *streamWriter) handleSystem(evt *topLevelEvent) {
	switch evt.Subtype {
	case "init":
		if evt.SessionID != "" {
			sw.sessionID = evt.SessionID
		}
		line := "Session started"
		sw.writeLog(stamp() + line + "\n")
		sw.writeLive(sw.color(line, cDim) + "\n")
	case "api_retry":
		line := fmt.Sprintf("[retry] Attempt %d/%d, retrying in %dms (%s)",
			evt.Attempt, evt.MaxRetries, evt.RetryDelay, evt.Error)
		sw.writeLog(stamp() + line + "\n")
		sw.writeLive(sw.color(line, cYellow) + "\n")
	}
}

func (sw *streamWriter) writeLog(s string) { fmt.Fprint(sw.log, s) }
func (sw *streamWriter) writeLive(s string) { fmt.Fprint(sw.live, s) }

func (sw *streamWriter) color(s, code string) string {
	if !sw.liveColor || code == "" {
		return s
	}
	return code + s + cReset
}

func stamp() string { return "[" + time.Now().Format("15:04:05") + "] " }

// formatToolCall returns a one-line representation of a tool call, like
// "Read(file.go)" or "Bash(go test ./...)". Per-tool dispatch picks the most
// useful argument; unknown tools fall back to a generic key=value summary.
func formatToolCall(name string, input json.RawMessage) string {
	var args map[string]any
	if len(input) > 0 {
		_ = json.Unmarshal(input, &args)
	}

	if rest, ok := strings.CutPrefix(name, "mcp__"); ok {
		parts := strings.SplitN(rest, "__", 2)
		if len(parts) == 2 {
			label := parts[0] + ": " + parts[1]
			if brief := briefArgs(args); brief != "" {
				return label + "(" + brief + ")"
			}
			return label + "()"
		}
	}

	switch name {
	case "Read":
		fp := strField(args, "file_path")
		if fp == "" {
			break
		}
		off := intField(args, "offset")
		lim := intField(args, "limit")
		if off > 0 && lim > 0 {
			return fmt.Sprintf("Read(%s, lines %d-%d)", fp, off, off+lim-1)
		}
		return fmt.Sprintf("Read(%s)", fp)
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		fp := strField(args, "file_path")
		if fp == "" {
			fp = strField(args, "notebook_path")
		}
		if fp != "" {
			return fmt.Sprintf("%s(%s)", name, fp)
		}
	case "Bash":
		if desc := strField(args, "description"); desc != "" {
			return fmt.Sprintf("Bash(%s)", desc)
		}
		return fmt.Sprintf("Bash(%s)", oneline(strField(args, "command"), 80))
	case "Grep":
		pat := strField(args, "pattern")
		if path := strField(args, "path"); path != "" {
			return fmt.Sprintf("Grep(%s in %s)", pat, path)
		}
		return fmt.Sprintf("Grep(%s)", pat)
	case "Glob":
		return fmt.Sprintf("Glob(%s)", strField(args, "pattern"))
	case "Task", "Agent":
		desc := strField(args, "description")
		if desc == "" {
			desc = strField(args, "subagent_type")
		}
		return fmt.Sprintf("%s(%s)", name, desc)
	case "WebFetch":
		return fmt.Sprintf("WebFetch(%s)", strField(args, "url"))
	case "WebSearch":
		return fmt.Sprintf("WebSearch(%s)", oneline(strField(args, "query"), 80))
	case "ScheduleWakeup":
		delay := intField(args, "delaySeconds")
		if reason := oneline(strField(args, "reason"), 60); reason != "" {
			return fmt.Sprintf("ScheduleWakeup(%ds: %s)", delay, reason)
		}
		return fmt.Sprintf("ScheduleWakeup(%ds)", delay)
	case "ToolSearch":
		return fmt.Sprintf("ToolSearch(%s)", oneline(strField(args, "query"), 60))
	}

	if brief := briefArgs(args); brief != "" {
		return name + "(" + brief + ")"
	}
	return name + "()"
}

func strField(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func intField(m map[string]any, k string) int {
	if v, ok := m[k].(float64); ok {
		return int(v)
	}
	return 0
}

// briefArgs renders up to two scalar args as key=value pairs in alpha order.
func briefArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		switch v := args[k].(type) {
		case string:
			parts = append(parts, fmt.Sprintf("%s=%s", k, oneline(v, 40)))
		case float64:
			parts = append(parts, fmt.Sprintf("%s=%v", k, v))
		case bool:
			parts = append(parts, fmt.Sprintf("%s=%v", k, v))
		}
		if len(parts) >= 2 {
			break
		}
	}
	return strings.Join(parts, ", ")
}

func oneline(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if max > 0 && len(s) > max {
		return s[:max-1] + "…"
	}
	return s
}

// formatToolResult returns a one-line summary appropriate to the tool that
// produced it. toolName may be empty when the tool_use was not seen.
func formatToolResult(toolName, content string, isError bool) string {
	if isError {
		return "Error: " + oneline(firstNonEmptyLine(content), 200)
	}
	switch toolName {
	case "Read":
		lines := strings.Count(content, "\n")
		if lines == 0 && content != "" {
			lines = 1
		}
		return fmt.Sprintf("Read %d lines", lines)
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		return oneline(firstNonEmptyLine(content), 120)
	case "Bash":
		lines := splitNonEmptyLines(content)
		if len(lines) == 0 {
			return "(no output)"
		}
		head := oneline(lines[0], 100)
		if len(lines) > 1 {
			return fmt.Sprintf("%s (+%d more lines)", head, len(lines)-1)
		}
		return head
	case "Grep", "Glob":
		return oneline(firstNonEmptyLine(content), 120)
	case "TodoWrite":
		return "Todos updated"
	case "Task", "Agent":
		return oneline(content, 200)
	case "WebFetch", "WebSearch":
		return oneline(firstNonEmptyLine(content), 120)
	}
	return oneline(firstNonEmptyLine(content), 120)
}

func firstNonEmptyLine(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

func splitNonEmptyLines(s string) []string {
	var out []string
	for line := range strings.SplitSeq(s, "\n") {
		if t := strings.TrimRight(line, " \t\r"); t != "" {
			out = append(out, t)
		}
	}
	return out
}
