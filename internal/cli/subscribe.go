package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/famarting/bigband/internal/ipc"
	"github.com/famarting/bigband/internal/paths"
	"github.com/spf13/cobra"
)

// NewSubscribeCmd registers `bigband subscribe` — opens a long-lived IPC
// connection to the daemon and prints incoming event envelopes as NDJSON.
// Useful for debugging extensions: you can see exactly what they would receive.
func NewSubscribeCmd() *cobra.Command {
	var (
		types []string
		tasks []string
		name  string
		since string
	)
	cmd := &cobra.Command{
		Use:     "subscribe",
		Short:   "Stream lifecycle events from the daemon (long-lived)",
		GroupID: "run",
		RunE: func(cmd *cobra.Command, args []string) error {
			conn, err := net.DialTimeout("unix", paths.Socket(), 2*time.Second)
			if err != nil {
				return fmt.Errorf("cannot reach daemon: %w", err)
			}
			defer conn.Close()
			req := &ipc.SubscribeRequest{
				Name:  name,
				Types: types,
				Tasks: tasks,
				Since: since,
			}
			if err := json.NewEncoder(conn).Encode(ipc.Cmd{Action: "subscribe", Subscribe: req}); err != nil {
				return fmt.Errorf("sending subscribe: %w", err)
			}
			r := bufio.NewReader(conn)
			// First line is the OK reply.
			line, err := r.ReadBytes('\n')
			if err != nil {
				return fmt.Errorf("reading subscribe ack: %w", err)
			}
			var reply ipc.Reply
			if err := json.Unmarshal(line, &reply); err != nil {
				return fmt.Errorf("decoding ack: %w", err)
			}
			if !reply.OK {
				return fmt.Errorf("subscribe rejected: %s", reply.Error)
			}
			// Subsequent lines are envelopes — copy them to stdout as-is.
			for {
				line, err := r.ReadBytes('\n')
				if len(line) > 0 {
					_, _ = os.Stdout.Write(line)
				}
				if err != nil {
					return nil
				}
			}
		},
	}
	cmd.Flags().StringSliceVar(&types, "types", nil, "filter by event type (repeatable; empty = all)")
	cmd.Flags().StringSliceVar(&tasks, "tasks", nil, "filter by task name or '*' (repeatable; empty = all)")
	cmd.Flags().StringVar(&name, "name", "cli:subscribe", "subscriber name reported to the daemon")
	cmd.Flags().StringVar(&since, "since", "", "RFC3339 timestamp; replay events from events.jsonl from this point before going live")
	return cmd
}
