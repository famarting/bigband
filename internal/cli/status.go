package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/famarting/bigband/internal/ipc"
	"github.com/famarting/bigband/internal/paths"
	"github.com/famarting/bigband/internal/state"
	"github.com/spf13/cobra"
)

func NewStatusCmd() *cobra.Command {
	var num int
	var running bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show recent task execution history",
		RunE: func(cmd *cobra.Command, args []string) error {
			return printHistory(num, running)
		},
	}
	cmd.Flags().IntVarP(&num, "num", "n", 20, "number of runs to show")
	cmd.Flags().BoolVarP(&running, "running", "r", false, "show only active and orphaned runs")
	return cmd
}

type runEntry struct {
	Task         string
	Started      time.Time
	Status       string
	Duration     string
	Active       bool
	WorktreePath string
}

func printHistory(limit int, onlyRunning bool) error {
	if reply, err := ipc.Send(ipc.Cmd{Action: "status"}); err == nil && reply.OK {
		var payload ipc.StatusPayload
		if json.Unmarshal(reply.Payload, &payload) == nil {
			fmt.Printf("daemon: running  uptime: %s\n", payload.Uptime)
		} else {
			fmt.Println("daemon: running")
		}
	} else {
		fmt.Println("daemon: not running")
	}
	fmt.Println()

	st, _ := state.Load()
	entries := collectRuns(st)

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Started.After(entries[j].Started)
	})

	if onlyRunning {
		filtered := entries[:0]
		for _, e := range entries {
			if e.Active || e.Status == "crashed" {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	} else if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}

	if len(entries) == 0 {
		fmt.Println("no task runs found")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  TASK\tSTARTED\tSTATUS\tDURATION\tWORKTREE")
	for _, e := range entries {
		marker := " "
		if e.Active {
			marker = "*"
		}
		started := e.Started.Local().Format("2006-01-02 15:04:05")
		dur := e.Duration
		if dur == "" {
			if e.Active {
				dur = time.Since(e.Started).Round(time.Second).String() + " elapsed"
			} else {
				dur = "?"
			}
		}
		wt := e.WorktreePath
		if wt == "" {
			wt = "-"
		}
		fmt.Fprintf(w, "%s %s\t%s\t%s\t%s\t%s\n", marker, e.Task, started, e.Status, dur, wt)
	}
	return w.Flush()
}

func collectRuns(st *state.State) []runEntry {
	logsDir := paths.LogsDir()
	taskDirs, err := os.ReadDir(logsDir)
	if err != nil {
		return nil
	}

	var entries []runEntry
	for _, td := range taskDirs {
		if !td.IsDir() {
			continue
		}
		taskName := td.Name()
		taskLogDir := paths.TaskLogDir(taskName)
		files, err := os.ReadDir(taskLogDir)
		if err != nil {
			continue
		}

		// Collect log filenames sorted ascending (ReadDir returns them lexicographically,
		// and the timestamp format sorts correctly that way).
		var logFiles []string
		for _, f := range files {
			if !f.IsDir() && strings.HasSuffix(f.Name(), ".log") && f.Name() != "latest.log" {
				logFiles = append(logFiles, f.Name())
			}
		}
		if len(logFiles) == 0 {
			continue
		}
		sort.Strings(logFiles)

		ts := st.Get(taskName)
		for i, fname := range logFiles {
			started, err := parseLogStartTime(fname)
			if err != nil {
				continue
			}
			isLatest := i == len(logFiles)-1
			logPath := filepath.Join(taskLogDir, fname)
			status, duration, hasEnd := parseLogEnd(logPath)

			active := false
			if !hasEnd {
				if isLatest && pidAlive(ts.RunningPID) {
					active = true
					status = "running"
				} else {
					status = "crashed"
				}
			}
			wt := ""
			if isLatest {
				wt = ts.WorktreePath
			}
			entries = append(entries, runEntry{
				Task:         taskName,
				Started:      started,
				Status:       status,
				Duration:     duration,
				Active:       active,
				WorktreePath: wt,
			})
		}
	}
	return entries
}

// parseLogStartTime parses the log filename "2006-01-02T15-04-05Z.log" into a UTC time.
func parseLogStartTime(filename string) (time.Time, error) {
	name := strings.TrimSuffix(filename, ".log")
	return time.Parse("2006-01-02T15-04-05Z", name)
}

// parseLogEnd reads the tail of a log file looking for the "=== END" line.
func parseLogEnd(path string) (status, duration string, found bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", false
	}
	defer f.Close()

	const tailSize = 2048
	if fi, err := f.Stat(); err == nil {
		if offset := fi.Size() - tailSize; offset > 0 {
			_, _ = f.Seek(offset, io.SeekStart)
		}
	}

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, "=== END") {
			continue
		}
		for field := range strings.FieldsSeq(line) {
			if v, ok := strings.CutPrefix(field, "status="); ok {
				status = v
			}
			if v, ok := strings.CutPrefix(field, "duration="); ok {
				duration = v
			}
		}
		return status, duration, true
	}
	return "", "", false
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

