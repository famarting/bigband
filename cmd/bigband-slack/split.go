package main

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// slackMaxMessageChars is the per-message budget we split on. chat.postMessage
// accepts far more, but Slack starts breaking text over ~4000 characters into
// follow-up messages on its own and it cuts wherever it lands — mid-list,
// mid-sentence. Splitting deliberately just under that keeps the break on a
// section boundary instead.
const slackMaxMessageChars = 3900

// continuationFormat marks chunks 2..n so a split message still reads in order.
// continuationReserve is its worst-case rendered length, held back from the
// budget so a marked chunk can't tip over the limit.
const (
	continuationFormat  = "_…continued (%d/%d)_\n\n"
	continuationReserve = 24
)

// splitForSlack breaks text into messages of at most limit characters,
// preferring section boundaries (blank lines), then line breaks, then a hard
// cut on a rune boundary. Text already within the limit comes back as a single
// chunk with no continuation marker. limit <= 0 uses slackMaxMessageChars.
func splitForSlack(text string, limit int) []string {
	if limit <= 0 {
		limit = slackMaxMessageChars
	}
	if len(text) <= limit {
		return []string{text}
	}

	budget := limit - continuationReserve
	if budget < 1 {
		budget = limit
	}

	var chunks []string
	rest := text
	for len(rest) > budget {
		cut := splitPoint(rest, budget)
		chunk := strings.TrimRight(rest[:cut], "\n")
		if chunk != "" {
			chunks = append(chunks, chunk)
		}
		rest = strings.TrimLeft(rest[cut:], "\n")
	}
	if rest != "" {
		chunks = append(chunks, rest)
	}
	if len(chunks) <= 1 {
		return chunks
	}

	out := make([]string, 0, len(chunks))
	for i, chunk := range chunks {
		if i == 0 {
			out = append(out, chunk)
			continue
		}
		out = append(out, fmt.Sprintf(continuationFormat, i+1, len(chunks))+chunk)
	}
	return out
}

// splitPoint returns the byte offset to cut s at, never past budget and never
// zero (a zero cut would spin the caller's loop forever). It prefers the last
// blank line in the window, then the last newline, then budget itself backed
// off to a rune boundary.
func splitPoint(s string, budget int) int {
	window := s[:budget]
	if i := strings.LastIndex(window, "\n\n"); i > 0 {
		return i
	}
	if i := strings.LastIndex(window, "\n"); i > 0 {
		return i
	}
	cut := budget
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if cut == 0 {
		return budget // pathological input; take the broken rune over a hang
	}
	return cut
}
