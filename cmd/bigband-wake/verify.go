package main

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
)

func newVerifyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "verify",
		Short: "Verify that bigband-wake can schedule and cancel pmset wakes",
		Long: `Runs a quick self-test to confirm that the sudoers stanza installed by
'bigband-wake setup' grants the required pmset access:

  1. Reads the current pmset schedule (sudo -n pmset -g sched)
  2. Schedules a test wake 2 hours in the future
  3. Immediately cancels that test wake

Prints PASS / FAIL for each step with an actionable error message on failure.
All checks must pass before bigband-wake can manage wake events reliably.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			var anyFailed bool

			check := func(label string, fn func() error) bool {
				if err := fn(); err != nil {
					fmt.Fprintf(os.Stderr, "FAIL  %s\n      %v\n", label, err)
					anyFailed = true
					return false
				}
				fmt.Printf("PASS  %s\n", label)
				return true
			}

			check("sudo -n pmset -g sched (verify sudoers allows read access)", func() error {
				return pmsetReachable(ctx)
			})

			testTime := time.Now().Add(2 * time.Hour).Truncate(time.Minute)
			scheduled := check(fmt.Sprintf("schedule test wake at %s", testTime.Local().Format("15:04 MST")), func() error {
				return schedulePmsetWake(ctx, testTime)
			})
			if scheduled {
				check("cancel test wake (verify sudoers allows cancel)", func() error {
					return cancelPmsetWake(ctx, testTime)
				})
			}

			if anyFailed {
				fmt.Fprintln(os.Stderr, "\nOne or more checks failed.")
				fmt.Fprintln(os.Stderr, "Run `bigband-wake setup` and follow the printed instructions, then retry.")
				return fmt.Errorf("verify failed")
			}
			fmt.Println("\nAll checks passed — bigband-wake is ready to manage wake events.")
			return nil
		},
	}
}
