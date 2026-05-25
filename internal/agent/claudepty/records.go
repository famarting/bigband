package claudepty

import (
	"encoding/json"
	"strings"
)

// sessionRecord is the umbrella shape for one line in a Claude session JSONL.
// Per-field population varies by record type; the fields we don't care about
// are intentionally absent.
type sessionRecord struct {
	Type        string             `json:"type"`
	Subtype     string             `json:"subtype,omitempty"`
	IsSidechain bool               `json:"isSidechain,omitempty"`
	Message     json.RawMessage    `json:"message,omitempty"`
	Attachment  *attachmentPayload `json:"attachment,omitempty"`
	// DurationMs is populated on system/turn_duration records.
	DurationMs int64 `json:"durationMs,omitempty"`
}

// innerMessage is the role-tagged inner message embedded in user/assistant
// records. Content is polymorphic: a plain string for simple user messages,
// or an array of content blocks for everything else. Usage is only populated
// on assistant messages.
type innerMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Usage   *messageUsage   `json:"usage,omitempty"`
}

// messageUsage captures the token-accounting fields we surface in the
// turn-complete banner. The full claude usage object has many more fields
// (per-iteration breakdown, server-tool counts, cache tiers); we only keep
// what we render.
type messageUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// contentBlock covers the block shapes we extract text from. tool_use blocks
// are also parsed structurally so the provider can detect background work
// (run_in_background:true) and ScheduleWakeup calls. tool_result blocks
// (which only appear in user records) reuse this struct via the ToolUseID /
// IsError / Content fields.
type contentBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	Thinking string          `json:"thinking,omitempty"`
	ID       string          `json:"id,omitempty"`
	Name     string          `json:"name,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
	// tool_result fields
	ToolUseID string          `json:"tool_use_id,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}

// attachmentPayload covers the attachment record shapes we care about. The
// queued_command variant carries task-notification text when a background
// Bash task completes; we mine the prompt to clear the corresponding entry
// from the pending-bg set.
type attachmentPayload struct {
	Type        string `json:"type,omitempty"`
	CommandMode string `json:"commandMode,omitempty"`
	Prompt      string `json:"prompt,omitempty"`
}

// wakeupRequest holds the parameters Claude passed to ScheduleWakeup, as
// they appear on the wire. The provider converts this into agent.WakeupRequest
// (with time.Duration) for the runner.
type wakeupRequest struct {
	DelaySeconds int    `json:"delaySeconds"`
	Prompt       string `json:"prompt"`
}

// isTurnTerminal reports whether r marks the end of one Claude turn. Claude
// writes a system/turn_duration record once the assistant has finished
// responding; that's our cue to stop tailing.
func isTurnTerminal(r *sessionRecord) bool {
	return r.Type == "system" && r.Subtype == "turn_duration" && !r.IsSidechain
}

// assistantText returns the concatenated text blocks of an assistant record,
// or "" if the record isn't a non-sidechain assistant message or holds no
// text. Thinking blocks and tool_use blocks are dropped.
func assistantText(r *sessionRecord) string {
	if r.Type != "assistant" || r.IsSidechain || len(r.Message) == 0 {
		return ""
	}
	var msg innerMessage
	if err := json.Unmarshal(r.Message, &msg); err != nil {
		return ""
	}
	return extractText(msg.Content)
}

// assistantToolUses returns the tool_use content blocks of an assistant
// record, or nil if r is not a non-sidechain assistant message. Used to
// detect background-bash starts and ScheduleWakeup calls.
func assistantToolUses(r *sessionRecord) []contentBlock {
	if r.Type != "assistant" || r.IsSidechain || len(r.Message) == 0 {
		return nil
	}
	var msg innerMessage
	if err := json.Unmarshal(r.Message, &msg); err != nil {
		return nil
	}
	var blocks []contentBlock
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return nil
	}
	out := blocks[:0]
	for _, b := range blocks {
		if b.Type == "tool_use" && b.Name != "" {
			out = append(out, b)
		}
	}
	return out
}

// assistantThinkingTexts returns the non-empty thinking-block texts of an
// assistant record. Thinking blocks are often empty in production sessions
// (signed/redacted by the model); we only return ones with plain text.
func assistantThinkingTexts(r *sessionRecord) []string {
	if r.Type != "assistant" || r.IsSidechain || len(r.Message) == 0 {
		return nil
	}
	var msg innerMessage
	if err := json.Unmarshal(r.Message, &msg); err != nil {
		return nil
	}
	var blocks []contentBlock
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return nil
	}
	var out []string
	for _, b := range blocks {
		if b.Type == "thinking" && b.Thinking != "" {
			out = append(out, b.Thinking)
		}
	}
	return out
}

// assistantUsage returns the per-message token usage attached to an assistant
// record, or nil if the record is not a non-sidechain assistant message or
// has no usage field.
func assistantUsage(r *sessionRecord) *messageUsage {
	if r.Type != "assistant" || r.IsSidechain || len(r.Message) == 0 {
		return nil
	}
	var msg innerMessage
	if err := json.Unmarshal(r.Message, &msg); err != nil {
		return nil
	}
	return msg.Usage
}

// toolResults returns the tool_result content blocks of a user record, or
// nil if r is not a non-sidechain user message or contains none. Tool
// results are how Claude observes the output of its tool calls; surfacing
// them in the log is the main way readers see what each tool produced.
func toolResults(r *sessionRecord) []contentBlock {
	if r.Type != "user" || r.IsSidechain || len(r.Message) == 0 {
		return nil
	}
	var msg innerMessage
	if err := json.Unmarshal(r.Message, &msg); err != nil {
		return nil
	}
	var blocks []contentBlock
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return nil
	}
	out := blocks[:0]
	for _, b := range blocks {
		if b.Type == "tool_result" {
			out = append(out, b)
		}
	}
	return out
}

// toolResultText flattens the polymorphic content of a tool_result block
// into a single string. content may be a JSON string or an array of
// {type:"text", text:"…"} blocks; non-text array entries are ignored.
func toolResultText(b *contentBlock) string {
	if len(b.Content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(b.Content, &s); err == nil {
		return s
	}
	var blocks []contentBlock
	if err := json.Unmarshal(b.Content, &blocks); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, blk := range blocks {
		if blk.Type == "text" && blk.Text != "" {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(blk.Text)
		}
	}
	return sb.String()
}

// isBackgroundBashInput reports whether a Bash tool_use input requests
// run_in_background:true. Other booleans in the input are ignored.
func isBackgroundBashInput(input json.RawMessage) bool {
	if len(input) == 0 {
		return false
	}
	var in struct {
		RunInBackground bool `json:"run_in_background"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return false
	}
	return in.RunInBackground
}

// parseWakeup extracts a wakeupRequest from a ScheduleWakeup tool_use input,
// or returns nil if the input is empty / malformed / has no positive delay.
func parseWakeup(input json.RawMessage) *wakeupRequest {
	if len(input) == 0 {
		return nil
	}
	var req wakeupRequest
	if err := json.Unmarshal(input, &req); err != nil {
		return nil
	}
	if req.DelaySeconds <= 0 {
		return nil
	}
	return &req
}

// bgCompletionToolUseID extracts the tool-use id from a task-notification
// attachment, or "" if r isn't one. Status is intentionally not checked:
// any terminal notification (completed, killed, failed) means the bg task
// is no longer running, which is all we need for the pending set.
func bgCompletionToolUseID(r *sessionRecord) string {
	if r.Type != "attachment" || r.Attachment == nil {
		return ""
	}
	a := r.Attachment
	if a.Type != "queued_command" || a.CommandMode != "task-notification" {
		return ""
	}
	_, after, ok := strings.Cut(a.Prompt, "<tool-use-id>")
	if !ok {
		return ""
	}
	id, _, ok := strings.Cut(after, "</tool-use-id>")
	if !ok {
		return ""
	}
	return strings.TrimSpace(id)
}

// extractText pulls a flat string out of an inner-message content field which
// may be either a JSON string (legacy/simple user messages) or an array of
// content blocks. Non-text block types are skipped.
func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == "text" && blk.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}
