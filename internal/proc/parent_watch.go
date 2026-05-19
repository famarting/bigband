package proc

import (
	"context"
	"os"
	"time"
)

// DefaultParentPollInterval is how often WatchParent samples getppid(). 5s is
// short enough to clear orphans before they do much damage (the bigband-slack
// state-race we care about plays out over minutes, not milliseconds) and long
// enough to be negligible overhead.
const DefaultParentPollInterval = 5 * time.Second

// WatchParent polls os.Getppid() and returns true when it changes from the
// value observed at entry, meaning our original parent has exited and the
// kernel has reparented us (to launchd on macOS, to init/systemd on Linux —
// always PID 1 in practice).
//
// Returns false if ctx is cancelled first, or if the initial PPID is already 1
// (we were started directly under launchd/init, so there is no supervisor
// parent to watch — orphan detection is meaningless).
//
// Intended use from an extension's `daemon` subcommand:
//
//	go func() {
//	    if proc.WatchParent(ctx, 0) {
//	        log.Printf("parent bigband daemon exited; shutting down")
//	        cancel() // the context that drives graceful shutdown
//	    }
//	}()
//
// interval == 0 picks DefaultParentPollInterval.
func WatchParent(ctx context.Context, interval time.Duration) bool {
	initial := os.Getppid()
	if initial <= 1 {
		return false
	}
	if interval <= 0 {
		interval = DefaultParentPollInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			if os.Getppid() != initial {
				return true
			}
		}
	}
}
