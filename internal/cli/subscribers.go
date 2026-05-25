package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/famarting/bigband/internal/ipc"
	"github.com/spf13/cobra"
)

// NewSubscribersCmd registers `bigband subscribers` — list integrations
// currently attached to the daemon's event bus. Useful to confirm whether
// bigband-slack (or any other extension) is actually connected.
func NewSubscribersCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "subscribers",
		Short:   "List active event-stream subscribers attached to the daemon",
		GroupID: "run",
		RunE: func(cmd *cobra.Command, args []string) error {
			reply, err := ipc.Send(ipc.Cmd{Action: "subscribers"})
			if err != nil {
				return fmt.Errorf("cannot reach daemon: %w", err)
			}
			if !reply.OK {
				return fmt.Errorf("daemon error: %s", reply.Error)
			}
			var payload ipc.SubscribersReply
			if err := json.Unmarshal(reply.Payload, &payload); err != nil {
				return fmt.Errorf("decoding reply: %w", err)
			}
			if len(payload.Subscribers) == 0 {
				fmt.Println("(no subscribers attached)")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tCONNECTED FOR\tTYPES\tJOBS\tLAG DROPPED")
			now := time.Now().UTC()
			for _, s := range payload.Subscribers {
				dur := now.Sub(s.ConnectedAt).Round(time.Second)
				types := strings.Join(s.Types, ",")
				if types == "" {
					types = "*"
				}
				jobs := strings.Join(s.Jobs, ",")
				if jobs == "" {
					jobs = "*"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\n", s.Name, dur, types, jobs, s.LagDropped)
			}
			return w.Flush()
		},
	}
}
