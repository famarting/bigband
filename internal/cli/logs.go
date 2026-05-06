package cli

import (
	"bufio"
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

// tailFile streams new lines from f until EOF is stable (no new data).
// For task logs it stops at the "=== END" marker; for the daemon log it runs
// until the process is interrupted.
func tailFile(f *os.File) error {
	fmt.Println("\n--- following ---")
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			fmt.Print(line)
			if strings.Contains(line, "=== END") {
				return nil
			}
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

// waitAndFollowLog polls until a new log file appears for taskName, then
// streams it until the "=== END" marker. prevLog is the symlink target before
// the run was triggered; the function waits until the symlink points to a
// different (newer) file. Pass an empty string when the task has never run.
func waitAndFollowLog(taskName, prevLog string) error {
	latest := paths.TaskLogLatest(taskName)
	deadline := time.Now().Add(30 * time.Second)
	fmt.Printf("Waiting for task %q to start", taskName)
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
		Short:             "Show run logs for a task",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeTaskNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			logDir := paths.TaskLogDir(name)

			entries, err := os.ReadDir(logDir)
			if err != nil {
				if os.IsNotExist(err) {
					return fmt.Errorf("no logs found for task %q", name)
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
				return fmt.Errorf("no runs found for task %q", name)
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
			latest := paths.TaskLogLatest(name)
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
