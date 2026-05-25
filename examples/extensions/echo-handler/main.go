// echo-handler is a 60-line bigband extension example.
//
// It subscribes to job_run.completed and prints a one-line summary for each
// completed run. Demonstrates the full end-to-end shape of an extension:
//
//   - dial the daemon's Unix socket
//   - send an IPC subscribe Cmd
//   - read the OK reply
//   - decode incoming envelopes line by line
//
// It does NOT import any internal/* package — it speaks the wire format
// directly via stdlib JSON, exactly like an extension written in any other
// language would.
//
// Run:
//
//	go run examples/extensions/echo-handler
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

type cmd struct {
	Action    string         `json:"action"`
	Subscribe map[string]any `json:"subscribe,omitempty"`
}

type reply struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

type envelope struct {
	Type    string         `json:"type"`
	RunID   string         `json:"run_id"`
	JobName string         `json:"job_name"`
	Data    map[string]any `json:"data"`
}

func main() {
	sock := filepath.Join(os.Getenv("HOME"), ".bigband", "daemon.sock")
	conn, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dial:", err)
		os.Exit(1)
	}
	defer conn.Close()

	if err := json.NewEncoder(conn).Encode(cmd{
		Action: "subscribe",
		Subscribe: map[string]any{
			"name":  "echo-handler",
			"types": []string{"job_run.completed"},
		},
	}); err != nil {
		fmt.Fprintln(os.Stderr, "send:", err)
		os.Exit(1)
	}

	r := bufio.NewReader(conn)
	line, err := r.ReadBytes('\n')
	if err != nil {
		fmt.Fprintln(os.Stderr, "ack:", err)
		os.Exit(1)
	}
	var ack reply
	if err := json.Unmarshal(line, &ack); err != nil || !ack.OK {
		fmt.Fprintln(os.Stderr, "subscribe rejected:", ack.Error)
		os.Exit(1)
	}
	fmt.Println("echo-handler: subscribed; waiting for completions…")

	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			fmt.Fprintln(os.Stderr, "stream ended:", err)
			return
		}
		var env envelope
		if err := json.Unmarshal(line, &env); err != nil {
			continue
		}
		status, _ := env.Data["status"].(string)
		dur, _ := env.Data["duration_ms"].(float64)
		fmt.Printf("[%s] %s %s in %.0fms\n", env.RunID, env.JobName, status, dur)
	}
}
