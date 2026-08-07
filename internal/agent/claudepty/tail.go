package claudepty

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// pollInterval bounds how often the tail loop re-stats the JSONL file when it
// has no new bytes. Claude writes records in bursts; 50ms keeps perceived
// latency low without busy-spinning when the agent is thinking.
const pollInterval = 50 * time.Millisecond

// maxLineLen guards against malformed input that never produces a newline.
// Real records run a few KB at most; 16 MiB is a comfortable ceiling.
const maxLineLen = 16 * 1024 * 1024

// tailSession follows path from startOffset, parsing newline-delimited JSON
// records and handing each to visit. It returns the byte offset reached when
// it stops, along with an error if one occurred. Stop conditions:
//
//   - visit returns true (the caller indicating the turn is complete) — err nil
//   - ctx is cancelled — err is ctx.Err()
//   - childDead reports the producer process exited before completing
//   - the file produces malformed JSON or a single line exceeds maxLineLen
//
// path may not exist yet — claude creates it lazily once the session begins.
// In that case we wait (respecting ctx) for it to appear. childDead may be nil
// when the caller does not need exit detection.
func tailSession(ctx context.Context, path string, startOffset int64, childDead func() bool, visit func(*sessionRecord) bool) (int64, error) {
	var (
		offset = startOffset
		buf    bytes.Buffer
	)
	// resumable is the offset to report on any stop where the caller may tail
	// again from where we left off (visit returned true, ctx cancel, child
	// death). offset counts every byte read from disk, but buf may still hold
	// bytes we haven't handed to visit: complete lines that arrived in the same
	// read burst after the stopping line, and/or a partial, not-yet-newline-
	// terminated trailing line. Rewinding by buf.Len() points at the first such
	// unconsumed byte so the next call re-reads it, rather than resuming past it
	// (which would drop whole records, or corrupt a partial line into
	// unparseable JSON that is then silently skipped).
	resumable := func() int64 { return offset - int64(buf.Len()) }
	for {
		if err := ctx.Err(); err != nil {
			return resumable(), err
		}
		size, exists, err := statSize(path)
		if err != nil {
			return offset, fmt.Errorf("stat session file: %w", err)
		}
		if !exists || size <= offset {
			// If the producer has exited, one last read attempt above already
			// captured any final bytes; now bail rather than poll forever.
			if childDead != nil && childDead() {
				return resumable(), errors.New("claude exited before turn completed")
			}
			select {
			case <-time.After(pollInterval):
				continue
			case <-ctx.Done():
				return resumable(), ctx.Err()
			}
		}
		n, err := readRange(path, offset, size-offset, &buf)
		if err != nil {
			return offset, fmt.Errorf("read session file: %w", err)
		}
		offset += n
		if buf.Len() > maxLineLen {
			return offset, fmt.Errorf("session line exceeds %d bytes", maxLineLen)
		}
		for {
			line, ok := takeLine(&buf)
			if !ok {
				break
			}
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var rec sessionRecord
			if err := json.Unmarshal(line, &rec); err != nil {
				// One bad line shouldn't kill the whole turn. Skip it; the
				// caller can still detect completion via turn_duration.
				continue
			}
			if visit(&rec) {
				return resumable(), nil
			}
		}
	}
}

// statSize returns (size, exists, err). A missing file is not an error; the
// caller will poll until it appears.
func statSize(path string) (int64, bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return fi.Size(), true, nil
}

// readRange appends up to length bytes starting at offset onto buf, returning
// how many bytes were actually read. Short reads are fine — the outer loop
// will catch up on the next pass.
func readRange(path string, offset, length int64, buf *bytes.Buffer) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, err
	}
	n, err := io.CopyN(buf, f, length)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, err
	}
	return n, nil
}

// takeLine pulls the next newline-terminated line out of buf, returning
// (line, true) when one is available. The trailing newline is stripped.
func takeLine(buf *bytes.Buffer) ([]byte, bool) {
	data := buf.Bytes()
	idx := bytes.IndexByte(data, '\n')
	if idx < 0 {
		return nil, false
	}
	line := make([]byte, idx)
	copy(line, data[:idx])
	buf.Next(idx + 1)
	return line, true
}
