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
// records and handing each to visit. It returns when:
//
//   - visit returns true (the caller indicating the turn is complete)
//   - ctx is cancelled
//   - childDead reports the producer process exited before completing
//   - the file produces malformed JSON or a single line exceeds maxLineLen
//
// path may not exist yet — claude creates it lazily once the session begins.
// In that case we wait (respecting ctx) for it to appear. childDead may be nil
// when the caller does not need exit detection.
func tailSession(ctx context.Context, path string, startOffset int64, childDead func() bool, visit func(*sessionRecord) bool) error {
	var (
		offset = startOffset
		buf    bytes.Buffer
	)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		size, exists, err := statSize(path)
		if err != nil {
			return fmt.Errorf("stat session file: %w", err)
		}
		if !exists || size <= offset {
			// If the producer has exited, one last read attempt above already
			// captured any final bytes; now bail rather than poll forever.
			if childDead != nil && childDead() {
				return errors.New("claude exited before turn completed")
			}
			select {
			case <-time.After(pollInterval):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		n, err := readRange(path, offset, size-offset, &buf)
		if err != nil {
			return fmt.Errorf("read session file: %w", err)
		}
		offset += n
		if buf.Len() > maxLineLen {
			return fmt.Errorf("session line exceeds %d bytes", maxLineLen)
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
				return nil
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
