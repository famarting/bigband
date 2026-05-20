package claudepty

import (
	"encoding/json"
	"strings"
)

// sessionRecord is the umbrella shape for one line in a Claude session JSONL.
// Per-field population varies by record type; the fields we don't care about
// are intentionally absent.
type sessionRecord struct {
	Type        string          `json:"type"`
	Subtype     string          `json:"subtype,omitempty"`
	IsSidechain bool            `json:"isSidechain,omitempty"`
	Message     json.RawMessage `json:"message,omitempty"`
}

// innerMessage is the role-tagged inner message embedded in user/assistant
// records. Content is polymorphic: a plain string for simple user messages,
// or an array of content blocks for everything else.
type innerMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// contentBlock covers the block shapes we extract text from. tool_use and
// tool_result blocks are recognised but their text is deliberately ignored;
// only assistant "text" blocks are surfaced to the runner.
type contentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Thinking string `json:"thinking,omitempty"`
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
