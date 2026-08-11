package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSplitForSlack_UnderLimitIsUntouched(t *testing.T) {
	text := "*Good morning, Fabian.*\n\n*Do these today*\n1. merge #9930"
	got := splitForSlack(text, 3900)
	if len(got) != 1 {
		t.Fatalf("len(chunks) = %d, want 1", len(got))
	}
	if got[0] != text {
		t.Errorf("chunk mutated:\n got %q\nwant %q", got[0], text)
	}
}

func TestSplitForSlack_PrefersBlankLineBoundary(t *testing.T) {
	// Two sections, each 40 bytes of body, separated by a blank line.
	secA := "*Section A*\n" + strings.Repeat("a", 40)
	secB := "*Section B*\n" + strings.Repeat("b", 40)
	text := secA + "\n\n" + secB

	// Budget has to fit the first section once the continuation reserve is off.
	got := splitForSlack(text, len(secA)+continuationReserve+2)
	if len(got) != 2 {
		t.Fatalf("len(chunks) = %d, want 2 (chunks: %q)", len(got), got)
	}
	if got[0] != secA {
		t.Errorf("chunk 1 = %q, want the whole first section %q", got[0], secA)
	}
	if !strings.HasSuffix(got[1], secB) {
		t.Errorf("chunk 2 = %q, want it to end with the second section", got[1])
	}
	if !strings.HasPrefix(got[1], "_…continued (2/2)_") {
		t.Errorf("chunk 2 missing continuation marker: %q", got[1])
	}
}

func TestSplitForSlack_FallsBackToLineBoundary(t *testing.T) {
	// No blank lines anywhere, so the cut has to land on a single newline.
	var b strings.Builder
	for i := range 60 {
		b.WriteString("• bullet line number ")
		b.WriteByte(byte('0' + i%10))
		b.WriteString("\n")
	}
	text := strings.TrimRight(b.String(), "\n")

	got := splitForSlack(text, 400)
	if len(got) < 2 {
		t.Fatalf("len(chunks) = %d, want >= 2", len(got))
	}
	for i, chunk := range got {
		if len(chunk) > 400 {
			t.Errorf("chunk %d is %d bytes, over the 400 limit", i+1, len(chunk))
		}
		// No chunk may start mid-bullet.
		if body := stripMarker(chunk); !strings.HasPrefix(body, "• ") {
			t.Errorf("chunk %d starts mid-line: %q", i+1, firstLine(body))
		}
	}
	if joinBodies(got) != text {
		t.Errorf("round-trip lost content")
	}
}

func TestSplitForSlack_HardCutStaysOnRuneBoundary(t *testing.T) {
	// One unbroken run of multi-byte runes: no newline to cut on anywhere.
	text := strings.Repeat("—", 400) // 3 bytes each = 1200 bytes
	got := splitForSlack(text, 100)
	if len(got) < 2 {
		t.Fatalf("len(chunks) = %d, want >= 2", len(got))
	}
	for i, chunk := range got {
		if !utf8.ValidString(chunk) {
			t.Errorf("chunk %d is not valid utf-8: %q", i+1, chunk)
		}
		if len(chunk) > 100 {
			t.Errorf("chunk %d is %d bytes, over the 100 limit", i+1, len(chunk))
		}
	}
}

func TestSplitForSlack_RealisticBriefingSplitsOnce(t *testing.T) {
	// A briefing the size of the Aug 11 one (~7.4k) must come back as chunks
	// that each fit, with the first section intact in chunk 1.
	section := "*Waiting on you*\n" + strings.Repeat("• some item that needs attention\n", 20)
	text := "*Good morning, Fabian.*\n\n" + strings.Repeat(section+"\n", 12)

	got := splitForSlack(text, 3900)
	if len(got) < 2 {
		t.Fatalf("len(chunks) = %d, want >= 2 for a %d byte briefing", len(got), len(text))
	}
	for i, chunk := range got {
		if len(chunk) > 3900 {
			t.Errorf("chunk %d is %d bytes, over the 3900 limit", i+1, len(chunk))
		}
	}
	if !strings.HasPrefix(got[0], "*Good morning, Fabian.*") {
		t.Errorf("chunk 1 lost the greeting: %q", firstLine(got[0]))
	}
}

func TestSplitForSlack_ZeroLimitUsesDefault(t *testing.T) {
	text := strings.Repeat("x", slackMaxMessageChars+500)
	got := splitForSlack(text, 0)
	if len(got) < 2 {
		t.Fatalf("len(chunks) = %d, want >= 2", len(got))
	}
	if len(got[0]) > slackMaxMessageChars {
		t.Errorf("chunk 1 is %d bytes, over the default limit", len(got[0]))
	}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// stripMarker removes a leading continuation marker, if present.
func stripMarker(chunk string) string {
	if !strings.HasPrefix(chunk, "_…continued (") {
		return chunk
	}
	_, body, found := strings.Cut(chunk, "_\n\n")
	if !found {
		return chunk
	}
	return body
}

// joinBodies strips continuation markers and rejoins, to check no content was
// dropped across the split.
func joinBodies(chunks []string) string {
	out := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		out = append(out, stripMarker(chunk))
	}
	return strings.Join(out, "\n")
}
