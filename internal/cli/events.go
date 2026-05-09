package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/famarting/bigband/internal/paths"
	"github.com/spf13/cobra"
)

// NewEventsCmd registers `bigband events` — tail the JSONL events file. This
// is the simplest way to inspect what the daemon is emitting and is also the
// recommended fallback when an extension wants durable / replayable access
// rather than a live IPC subscribe stream.
func NewEventsCmd() *cobra.Command {
	var (
		follow bool
		tail   int
	)
	cmd := &cobra.Command{
		Use:     "events",
		Short:   "Show or follow the lifecycle events log",
		GroupID: "run",
		RunE: func(cmd *cobra.Command, args []string) error {
			path := paths.EventsFile()
			f, err := os.Open(path)
			if err != nil {
				return fmt.Errorf("opening events file: %w", err)
			}
			defer f.Close()

			if tail > 0 {
				if err := printLastN(f, tail); err != nil {
					return err
				}
			} else {
				if _, err := io.Copy(os.Stdout, f); err != nil {
					return err
				}
			}
			if !follow {
				return nil
			}
			// Follow: poll for new bytes appended to the file.
			r := bufio.NewReader(f)
			for {
				line, err := r.ReadString('\n')
				if len(line) > 0 {
					fmt.Print(line)
				}
				if err == io.EOF {
					time.Sleep(200 * time.Millisecond)
					continue
				}
				if err != nil {
					return err
				}
			}
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "follow the events file (tail -F)")
	cmd.Flags().IntVarP(&tail, "tail", "n", 0, "show only the last N lines before optional --follow")
	return cmd
}

// printLastN reads the last n newline-terminated lines from f and prints them.
// Reads the entire file — fine for events.jsonl which the daemon trims via
// runtime config later if it grows too large.
func printLastN(f *os.File, n int) error {
	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	// Walk backwards to find n newlines.
	count := 0
	start := len(data)
	for i := len(data) - 1; i >= 0; i-- {
		if data[i] == '\n' {
			count++
			if count > n {
				start = i + 1
				break
			}
		}
	}
	if count <= n {
		start = 0
	}
	_, err = os.Stdout.Write(data[start:])
	return err
}
