package main

import (
	"context"
	"fmt"
	"log"
	"os/signal"
	"syscall"
	"time"

	"github.com/famarting/bigband/internal/proc"
	"github.com/famarting/bigband/pkg/bigbandext"
	"github.com/fsnotify/fsnotify"
	"github.com/spf13/cobra"
)

func newDaemonCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "daemon",
		Short:  "Run the Slack integration in the foreground (normally invoked by launchd)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
			defer cancel()

			// Refuse to start if another bigband-slack daemon already owns the
			// state.json for this user. Two concurrent instances race on the
			// store and silently lose run-to-thread mappings (which causes
			// completions to drop their Slack reply).
			releaseLock, holder, err := proc.AcquireInstanceLock(InstanceLockPath())
			if err != nil {
				if holder > 0 {
					return fmt.Errorf("bigband-slack daemon is already running as pid=%d (lock %s) — stop it first", holder, InstanceLockPath())
				}
				return fmt.Errorf("acquire instance lock: %w", err)
			}
			defer releaseLock()
			log.Printf("bigband-slack: acquired instance lock %s pid=%d", InstanceLockPath(), syscall.Getpid())

			// Self-terminate if the bigband daemon that spawned us exits.
			// Without this, a bigband daemon crash leaves bigband-slack
			// reparented to launchd; the next bigband daemon spawns a fresh
			// instance (blocked on the lock above) and the orphan keeps
			// running until somebody notices.
			go func() {
				if proc.WatchParent(ctx, 0) {
					log.Printf("bigband-slack: parent process exited (reparented); shutting down")
					cancel()
				}
			}()

			cfg, err := LoadConfig()
			if err != nil {
				return err
			}
			log.Printf("bigband-slack: config loaded mirror_rules=%d trigger_channels=%d retention=%s", len(cfg.Mirror), len(cfg.TriggerChannels), cfg.RetentionDuration())
			store, err := LoadStore()
			if err != nil {
				return err
			}
			log.Printf("bigband-slack: store loaded runs=%d jobs=%d threads=%d", len(store.Runs), len(store.Jobs), len(store.Threads))

			bb, err := bigbandextClientFromEnv()
			if err != nil {
				return err
			}

			sc, sm, err := newSlackClient(cfg)
			if err != nil {
				return err
			}
			router := &Router{store: store, slack: sc, bb: bb}
			router.SetConfig(cfg)

			// Auto-reload on config file edits via fsnotify. Token / connection
			// changes still require a full restart — the socket-mode session
			// is bound at startup. Rule-only changes (mirror, trigger_channels,
			// threads) take effect immediately.
			go watchConfigFile(ctx, router)

			// Subscribe to the bigband event stream; reconnects with backoff
			// when the daemon restarts. Uses pkg/bigbandext.Client.Subscribe
			// directly — same API any external integration would use.
			go runSubscribeLoop(ctx, router)

			// Periodically prune stale mappings so the store doesn't grow
			// unbounded. Default 7 days; configurable via `retention:`.
			go runPruneLoop(ctx, store, cfg.RetentionDuration())

			log.Println("bigband-slack: starting socket-mode")
			return runSocketMode(ctx, cfg, sm, sc, router)
		},
	}
}

// bigbandextClientFromEnv mirrors NewClientFromEnv but logs the resolved
// socket path so first-run debugging is easy.
func bigbandextClientFromEnv() (*bigbandext.Client, error) {
	c, err := bigbandext.NewClientFromEnv()
	if err != nil {
		return nil, err
	}
	log.Printf("bigband-slack: using bigband daemon socket %s", c.SocketPath())
	return c, nil
}

// watchConfigFile watches the slack config for writes and triggers a reload
// on each. Mirrors the daemon's fsnotify behaviour for ~/.bigband/config.yaml.
//
// Editors often rename-or-replace the file rather than writing in place, so
// we react to Write, Create, and Rename events on the parent directory rather
// than just a single Watch on the file path. After a Rename/Create cycle the
// inode changes, and a re-add keeps subsequent events flowing.
func watchConfigFile(ctx context.Context, router *Router) {
	path := ConfigPath()
	w, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("bigband-slack: cannot watch config file: %v", err)
		return
	}
	defer w.Close()
	if err := w.Add(path); err != nil {
		log.Printf("bigband-slack: fsnotify add %s: %v", path, err)
		return
	}
	log.Printf("bigband-slack: watching %s for changes", path)

	// Coalesce burst events (editors fire several events per save) with a
	// short debounce; reload once per quiet period.
	var pending *time.Timer
	const debounce = 200 * time.Millisecond
	for {
		select {
		case <-ctx.Done():
			if pending != nil {
				pending.Stop()
			}
			return
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) == 0 {
				continue
			}
			// On Rename/Remove the original inode is gone; re-add the path
			// so we keep getting events on the replacement file.
			if ev.Op&(fsnotify.Rename|fsnotify.Remove) != 0 {
				_ = w.Remove(path)
				_ = w.Add(path)
			}
			if pending != nil {
				pending.Stop()
			}
			pending = time.AfterFunc(debounce, func() {
				reloadConfig(router, "fsnotify="+ev.Op.String())
			})
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			log.Printf("bigband-slack: fsnotify error: %v", err)
		}
	}
}

// reloadConfig is the shared reload path invoked by the fsnotify watcher.
// Logs the trigger source, the parsed counts, and any error.
func reloadConfig(router *Router, source string) {
	log.Printf("bigband-slack: reloading config from %s (%s)", ConfigPath(), source)
	newCfg, err := LoadConfig()
	if err != nil {
		log.Printf("bigband-slack: reload failed: %v", err)
		return
	}
	router.SetConfig(newCfg)
	log.Printf("bigband-slack: config reloaded mirror_rules=%d trigger_channels=%d retention=%s threads_enabled=%v",
		len(newCfg.Mirror), len(newCfg.TriggerChannels), newCfg.RetentionDuration(), newCfg.Threads.Enabled)
}

// runPruneLoop drops stale store entries at startup and once per hour until
// ctx is cancelled. Disabled when retention <= 0.
func runPruneLoop(ctx context.Context, store *Store, retention time.Duration) {
	if retention <= 0 {
		return
	}
	prune := func() {
		runs, jobs, threads := store.Prune(time.Now().Add(-retention))
		if runs+jobs+threads > 0 {
			log.Printf("bigband-slack: pruned runs=%d jobs=%d threads=%d (retention %s)", runs, jobs, threads, retention)
		}
	}
	prune()
	t := time.NewTicker(1 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			prune()
		}
	}
}

// runSubscribeLoop maintains a long-lived subscribe stream via
// pkg/bigbandext.Client.Subscribe. Reconnects with backoff when the daemon
// restarts; on each reconnect, asks the daemon to replay events from
// `lastSeen` so nothing is missed across restarts. Dedups by EventID across
// the process lifetime. Returns when ctx is cancelled.
func runSubscribeLoop(ctx context.Context, router *Router) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	state := &subscribeState{seen: make(map[string]struct{})}
	for {
		err := streamOnce(ctx, router, state)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("bigband-slack: subscribe ended: %v (reconnecting in %s)", err, backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

type subscribeState struct {
	lastSeen time.Time
	seen     map[string]struct{}
}

func streamOnce(ctx context.Context, router *Router, st *subscribeState) error {
	req := bigbandext.SubscribeRequest{
		Name: "bigband-slack",
		Types: []string{
			string(bigbandext.TypeClaudeSessionStarted),
			string(bigbandext.TypeJobRunWorktreeReady),
			string(bigbandext.TypeJobRunCompleted),
		},
	}
	if !st.lastSeen.IsZero() {
		req.Since = st.lastSeen.UTC().Format(time.RFC3339)
		log.Printf("bigband-slack: subscribing with replay since=%s types=%v", req.Since, req.Types)
	} else {
		log.Printf("bigband-slack: subscribing live types=%v", req.Types)
	}
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	envCh, errCh := router.bb.Subscribe(subCtx, req)
	log.Printf("bigband-slack: subscribed; awaiting events")
	for {
		select {
		case env, ok := <-envCh:
			if !ok {
				if err, ok := <-errCh; ok && err != nil {
					return err
				}
				return nil
			}
			if _, dup := st.seen[env.EventID]; dup {
				continue
			}
			st.seen[env.EventID] = struct{}{}
			if env.OccurredAt.After(st.lastSeen) {
				st.lastSeen = env.OccurredAt
			}
			router.HandleEvent(env)
		case err := <-errCh:
			if err != nil {
				return err
			}
			return nil
		}
	}
}
