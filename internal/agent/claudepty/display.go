package claudepty

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// resultPreviewLimit caps how many characters of a tool_result we surface in
// the log. Results can run tens of thousands of bytes; this is just the "did
// it work" preview, not the full output.
const resultPreviewLimit = 160

// thinkingPreviewLimit caps how many characters of a thinking block we
// surface. Thinking is internal monologue — useful as a hint, not a transcript.
const thinkingPreviewLimit = 160

// emitThinking renders a (heavily truncated) thinking block. Empty strings
// (the redacted/signed shape) are caller-filtered; this is a no-op for "".
func emitThinking(log, live io.Writer, text string) {
	preview := singleLineTruncate(text, thinkingPreviewLimit)
	if preview == "" {
		return
	}
	fmt.Fprintf(log, "[%s] · %s\n", time.Now().Format("15:04:05"), preview)
	fmt.Fprintf(live, "\x1b[2m· %s\x1b[0m\n", preview)
}

// emitToolUse renders a one-line summary of a tool_use block. Live output is
// cyan for the arrow + tool name and dim for the argument tail; the log file
// gets the same shape without ANSI codes.
func emitToolUse(log, live io.Writer, b contentBlock) {
	name, detail := summarizeToolUse(b)
	if detail == "" {
		fmt.Fprintf(log, "[%s] → %s\n", time.Now().Format("15:04:05"), name)
		fmt.Fprintf(live, "\x1b[36m→ %s\x1b[0m\n", name)
		return
	}
	fmt.Fprintf(log, "[%s] → %s %s\n", time.Now().Format("15:04:05"), name, detail)
	fmt.Fprintf(live, "\x1b[36m→ %s\x1b[0m \x1b[2m%s\x1b[0m\n", name, detail)
}

// emitToolResult renders a one-line summary of a tool_result block. Errors
// are marked with a red ✗; successes with a dim ↵. Content is always
// heavily truncated regardless of outcome.
func emitToolResult(log, live io.Writer, b contentBlock) {
	preview := singleLineTruncate(toolResultText(&b), resultPreviewLimit)
	if b.IsError {
		if preview == "" {
			preview = "(error)"
		}
		fmt.Fprintf(log, "[%s] ✗ %s\n", time.Now().Format("15:04:05"), preview)
		fmt.Fprintf(live, "\x1b[31m✗ %s\x1b[0m\n", preview)
		return
	}
	if preview == "" {
		preview = "(ok)"
	}
	fmt.Fprintf(log, "[%s] ↵ %s\n", time.Now().Format("15:04:05"), preview)
	fmt.Fprintf(live, "\x1b[2m↵ %s\x1b[0m\n", preview)
}

// summarizeToolUse returns (toolName, detail) for a tool_use block. detail is
// a single short string suitable for appending after the tool name. Unknown
// tool inputs fall back to a tiny JSON preview so the reader can still tell
// what fired.
func summarizeToolUse(b contentBlock) (string, string) {
	switch b.Name {
	case "Bash":
		var in struct {
			Command         string `json:"command"`
			Description     string `json:"description"`
			RunInBackground bool   `json:"run_in_background"`
		}
		_ = json.Unmarshal(b.Input, &in)
		suffix := ""
		if in.RunInBackground {
			suffix = " &"
		}
		return "Bash", "$ " + singleLineTruncate(in.Command, 140) + suffix
	case "Read":
		var in struct {
			FilePath string `json:"file_path"`
			Offset   int    `json:"offset"`
			Limit    int    `json:"limit"`
		}
		_ = json.Unmarshal(b.Input, &in)
		extra := ""
		if in.Offset > 0 || in.Limit > 0 {
			extra = fmt.Sprintf(" (offset=%d limit=%d)", in.Offset, in.Limit)
		}
		return "Read", shortPath(in.FilePath) + extra
	case "Edit", "Write", "NotebookEdit":
		var in struct {
			FilePath  string `json:"file_path"`
			ReplaceAll bool  `json:"replace_all"`
		}
		_ = json.Unmarshal(b.Input, &in)
		extra := ""
		if in.ReplaceAll {
			extra = " (replace_all)"
		}
		return b.Name, shortPath(in.FilePath) + extra
	case "Glob":
		var in struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
		}
		_ = json.Unmarshal(b.Input, &in)
		if in.Path != "" {
			return "Glob", in.Pattern + " in " + shortPath(in.Path)
		}
		return "Glob", in.Pattern
	case "Grep":
		var in struct {
			Pattern   string `json:"pattern"`
			Path      string `json:"path"`
			OutputMode string `json:"output_mode"`
		}
		_ = json.Unmarshal(b.Input, &in)
		parts := []string{in.Pattern}
		if in.Path != "" {
			parts = append(parts, "in "+shortPath(in.Path))
		}
		if in.OutputMode != "" {
			parts = append(parts, "("+in.OutputMode+")")
		}
		return "Grep", strings.Join(parts, " ")
	case "TodoWrite":
		return "TodoWrite", summarizeTodos(b.Input)
	case "Task":
		var in struct {
			SubagentType string `json:"subagent_type"`
			Description  string `json:"description"`
		}
		_ = json.Unmarshal(b.Input, &in)
		typ := in.SubagentType
		if typ == "" {
			typ = "general-purpose"
		}
		return "Task", "[" + typ + "] " + singleLineTruncate(in.Description, 100)
	case "WebFetch":
		var in struct {
			URL    string `json:"url"`
			Prompt string `json:"prompt"`
		}
		_ = json.Unmarshal(b.Input, &in)
		return "WebFetch", in.URL
	case "WebSearch":
		var in struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(b.Input, &in)
		return "WebSearch", in.Query
	case "ScheduleWakeup":
		var in struct {
			DelaySeconds int    `json:"delaySeconds"`
			Reason       string `json:"reason"`
		}
		_ = json.Unmarshal(b.Input, &in)
		return "ScheduleWakeup", fmt.Sprintf("+%ds %s", in.DelaySeconds, singleLineTruncate(in.Reason, 100))
	default:
		return b.Name, singleLineTruncate(string(b.Input), 100)
	}
}

// summarizeTodos renders the items in a TodoWrite input as a compact
// status-prefixed list. Used by both the emit path and (indirectly) by the
// delta path in runState.
func summarizeTodos(input json.RawMessage) string {
	var in struct {
		Todos []struct {
			Content string `json:"content"`
			Status  string `json:"status"`
		} `json:"todos"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return ""
	}
	if len(in.Todos) == 0 {
		return "(empty)"
	}
	var counts [3]int // pending, in_progress, completed
	for _, t := range in.Todos {
		switch t.Status {
		case "pending":
			counts[0]++
		case "in_progress":
			counts[1]++
		case "completed":
			counts[2]++
		}
	}
	return fmt.Sprintf("%d item(s) — %d todo, %d doing, %d done", len(in.Todos), counts[0], counts[1], counts[2])
}

// todoStates returns a stable content→status map for delta detection.
func todoStates(input json.RawMessage) map[string]string {
	var in struct {
		Todos []struct {
			Content string `json:"content"`
			Status  string `json:"status"`
		} `json:"todos"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil
	}
	out := make(map[string]string, len(in.Todos))
	for _, t := range in.Todos {
		if t.Content == "" {
			continue
		}
		out[t.Content] = t.Status
	}
	return out
}

// emitTodoDelta writes the change set (new items + status transitions) between
// prev and next. Nothing is emitted when the two are identical; that keeps
// repeated TodoWrites with the same payload quiet.
func emitTodoDelta(log, live io.Writer, prev, next map[string]string) {
	type change struct {
		content string
		from    string
		to      string
	}
	var changes []change
	for content, status := range next {
		if old, ok := prev[content]; !ok {
			changes = append(changes, change{content, "", status})
		} else if old != status {
			changes = append(changes, change{content, old, status})
		}
	}
	if len(changes) == 0 {
		return
	}
	for _, c := range changes {
		icon := todoIcon(c.to)
		label := singleLineTruncate(c.content, 120)
		if c.from == "" {
			fmt.Fprintf(log, "[%s]   %s %s (new)\n", time.Now().Format("15:04:05"), icon, label)
			fmt.Fprintf(live, "\x1b[2m  %s %s (new)\x1b[0m\n", icon, label)
		} else {
			fmt.Fprintf(log, "[%s]   %s %s (%s→%s)\n", time.Now().Format("15:04:05"), icon, label, c.from, c.to)
			fmt.Fprintf(live, "\x1b[2m  %s %s (%s→%s)\x1b[0m\n", icon, label, c.from, c.to)
		}
	}
}

func todoIcon(status string) string {
	switch status {
	case "completed":
		return "[x]"
	case "in_progress":
		return "[~]"
	default:
		return "[ ]"
	}
}

// singleLineTruncate collapses internal whitespace runs to a single space and
// caps the result at limit characters (with an ellipsis when truncated).
func singleLineTruncate(s string, limit int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Collapse newlines/tabs/multiple spaces into single spaces so the
	// result fits on one line.
	var sb strings.Builder
	sb.Grow(len(s))
	prevSpace := false
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' || r == ' ' {
			if !prevSpace {
				sb.WriteRune(' ')
				prevSpace = true
			}
			continue
		}
		sb.WriteRune(r)
		prevSpace = false
	}
	out := sb.String()
	if limit > 0 && len(out) > limit {
		out = out[:limit] + "…"
	}
	return out
}

// shortPath returns a $HOME-prefixed path turned into ~/… form when possible,
// or the input unchanged otherwise. Keeps the per-tool detail readable when
// claude is operating in a deep absolute path.
func shortPath(p string) string {
	if p == "" {
		return ""
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
			return "~/" + rel
		}
	}
	return p
}
