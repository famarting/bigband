package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/famarting/bigband/internal/daemon"
	"github.com/famarting/bigband/internal/paths"
	"github.com/spf13/cobra"
)

func NewDaemonCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "daemon",
		Short:  "Run the scheduling daemon in the foreground (normally invoked by launchd)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return daemon.Run()
		},
	}
}

func NewDaemonLogsCmd() *cobra.Command {
	var follow bool
	var tailLines int

	cmd := &cobra.Command{
		Use:   "daemon-logs",
		Short: "Show the bigband daemon log (~/.bigband-tasks/daemon.log)",
		RunE: func(cmd *cobra.Command, args []string) error {
			path := paths.DaemonLog()
			f, err := os.Open(path)
			if err != nil {
				if os.IsNotExist(err) {
					return fmt.Errorf("no daemon log found at %s (has the daemon ever run?)", path)
				}
				return err
			}
			defer f.Close()

			if tailLines > 0 {
				if err := printLastLines(f, tailLines); err != nil {
					return err
				}
			} else if _, err := io.Copy(os.Stdout, f); err != nil {
				return err
			}

			if !follow {
				return nil
			}
			return tailUnbounded(f)
		},
	}

	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "tail the log in real time")
	cmd.Flags().IntVarP(&tailLines, "tail", "n", 0, "show only the last N lines (0 = whole file)")
	return cmd
}

// tailUnbounded streams new lines until the process is interrupted. Unlike
// tailFile (used for task logs), it has no "=== END" termination marker.
func tailUnbounded(f *os.File) error {
	fmt.Println("\n--- following ---")
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			fmt.Print(line)
		}
		if err == io.EOF {
			time.Sleep(300 * time.Millisecond)
			continue
		}
		if err != nil {
			return err
		}
	}
}

// printLastLines prints the last n newline-delimited lines of f and leaves the
// read offset at EOF so a follow-up tail picks up from there.
func printLastLines(f *os.File, n int) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	size := info.Size()

	const chunk int64 = 8192
	var buf []byte
	offset := size
	newlines := 0
	for offset > 0 && newlines <= n {
		read := min(chunk, offset)
		offset -= read
		tmp := make([]byte, read)
		if _, err := f.ReadAt(tmp, offset); err != nil {
			return err
		}
		buf = append(tmp, buf...)
		newlines = 0
		for _, b := range buf {
			if b == '\n' {
				newlines++
			}
		}
	}

	// Trim leading lines past n.
	for newlines > n {
		i := 0
		for ; i < len(buf); i++ {
			if buf[i] == '\n' {
				i++
				break
			}
		}
		buf = buf[i:]
		newlines--
	}

	if _, err := os.Stdout.Write(buf); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	return nil
}
