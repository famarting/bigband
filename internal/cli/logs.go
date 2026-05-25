package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/famarting/bigband/internal/paths"
	"github.com/spf13/cobra"
)

// tailLines streams new lines from f as they appear, polling on EOF. When
// stop is non-nil, the function returns once it returns true for any printed
// line (used to terminate job-log follows at the "=== END" marker). When
// stop is nil the loop runs until f returns a non-EOF error.
func tailLines(f *os.File, stop func(string) bool) error {
	fmt.Println("\n--- following ---")
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			fmt.Print(line)
			if stop != nil && stop(line) {
				return nil
			}
		}
		if errors.Is(err, io.EOF) {
			time.Sleep(300 * time.Millisecond)
			continue
		}
		if err != nil {
			return err
		}
	}
}

// tailFile follows a job log until the runner's "=== END" marker.
func tailFile(f *os.File) error {
	return tailLines(f, func(line string) bool { return strings.Contains(line, "=== END") })
}

// waitAndFollowLog polls until a new log file appears for jobName, then
// streams it until the "=== END" marker. prevLog is the symlink target before
// the run was triggered; the function waits until the symlink points to a
// different (newer) file. Pass an empty string when the job has never run.
func waitAndFollowLog(jobName, prevLog string) error {
	latest := paths.JobLogLatest(jobName)
	deadline := time.Now().Add(30 * time.Second)
	fmt.Printf("Waiting for job %q to start", jobName)
	for time.Now().Before(deadline) {
		if target, err := os.Readlink(latest); err == nil && target != prevLog {
			break
		}
		fmt.Print(".")
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Println()

	f, err := os.Open(latest)
	if err != nil {
		return fmt.Errorf("log file never appeared (is the daemon running?): %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(os.Stdout, f); err != nil {
		return err
	}
	return tailFile(f)
}

func NewLogsCmd() *cobra.Command {
	var follow bool
	var list bool
	var num int

	cmd := &cobra.Command{
		Use:               "logs <name>",
		Short:             "Show run logs for a job",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeJobNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			logDir := paths.JobLogDir(name)

			entries, err := os.ReadDir(logDir)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("no logs found for job %q", name)
				}
				return err
			}

			var logs []string
			for _, e := range entries {
				if !e.IsDir() && strings.HasSuffix(e.Name(), ".log") && e.Name() != "latest.log" {
					logs = append(logs, e.Name())
				}
			}
			sort.Strings(logs)

			if len(logs) == 0 {
				return fmt.Errorf("no runs found for job %q", name)
			}

			if list {
				// Print a table of runs.
				if num > 0 && len(logs) > num {
					logs = logs[len(logs)-num:]
				}
				for i, l := range logs {
					info, _ := os.Stat(filepath.Join(logDir, l))
					size := ""
					if info != nil {
						size = fmt.Sprintf("%dKB", info.Size()/1024)
					}
					marker := " "
					if i == len(logs)-1 {
						marker = "→"
					}
					fmt.Printf("%s %s  %s\n", marker, strings.TrimSuffix(l, ".log"), size)
				}
				fmt.Printf("\nTip: bigband logs %s --follow  to tail the latest run\n", name)
				return nil
			}

			// Open the latest log.
			latest := paths.JobLogLatest(name)
			f, err := os.Open(latest)
			if err != nil {
				return fmt.Errorf("opening latest log: %w", err)
			}
			defer f.Close()

			// Print existing content.
			if _, err := io.Copy(os.Stdout, f); err != nil {
				return err
			}

			if !follow {
				return nil
			}

			return tailFile(f)
		},
	}

	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "tail the latest run in real time")
	cmd.Flags().BoolVarP(&list, "list", "l", false, "list all runs with sizes")
	cmd.Flags().IntVarP(&num, "num", "n", 10, "number of runs to show (with --list)")
	return cmd
}
