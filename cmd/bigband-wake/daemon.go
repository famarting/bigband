package main

import (
	"context"
	"log"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/famarting/bigband/internal/proc"
	"github.com/famarting/bigband/pkg/bigbandext"
	"github.com/fsnotify/fsnotify"
	"github.com/spf13/cobra"
)

// nextRunLayout is the format used by the daemon's IPC status reply
// (internal/daemon/daemon.go formats next_run with "2006-01-02 15:04:05" in
// the local zone). We parse against the same layout, in the local zone, so
// times round-trip exactly.
const nextRunLayout = "2006-01-02 15:04:05"

// reconcileDebounce is how long we wait after a triggering event before
// running reconcile, so a burst of task_run.completed events for several
// tasks coalesces into a single pmset reshuffle.
const reconcileDebounce = 500 * time.Millisecond

func newDaemonCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "daemon",
		Short:  "Run the wake scheduler in the foreground (normally invoked by the bigband supervisor)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
			defer cancel()

			// Exit if the bigband daemon that spawned us dies — see the
			// matching block in bigband-slack for the orphan story.
			go func() {
				if proc.WatchParent(ctx, 0) {
					log.Printf("bigband-wake: parent process exited (reparented); shutting down")
					cancel()
				}
			}()

			cfg, err := LoadConfig()
			if err != nil {
				return err
			}
			log.Printf("bigband-wake: config loaded enabled=%v lead=%s max_events=%d reconcile_every=%s",
				cfg.Enabled, cfg.LeadDuration(), cfg.MaxEventsValue(), cfg.ReconcileEvery())

			if runtime.GOOS != "darwin" {
				log.Printf("bigband-wake: %s is not supported (macOS only); idling so the supervisor doesn't restart-loop", runtime.GOOS)
				<-ctx.Done()
				return nil
			}

			if !cfg.Enabled {
				log.Printf("bigband-wake: enabled=false in %s; idling (no pmset calls will be made)", ConfigPath())
				<-ctx.Done()
				return nil
			}

			// One-shot sudo probe up front: if the sudoers stanza is missing
			// we want a single loud line at startup, not a stack of failures
			// on every reconcile.
			if err := pmsetReachable(ctx); err != nil {
				log.Printf("bigband-wake: WARNING pmset is not reachable via sudo -n: %v", err)
				log.Printf("bigband-wake: run `bigband-wake setup` and follow the printed instructions, then restart this extension")
			}

			bb, err := bigbandext.NewClientFromEnv()
			if err != nil {
				return err
			}
			log.Printf("bigband-wake: using bigband daemon socket %s", bb.SocketPath())

			st, err := LoadState()
			if err != nil {
				log.Printf("bigband-wake: WARNING could not load state (%v); starting from empty", err)
				st = &State{}
			} else {
				log.Printf("bigband-wake: state loaded events=%d", len(st.Events))
			}

			r := &reconciler{
				bb:           bb,
				state:        st,
				signal:       make(chan string, 8),
				failureCount: make(map[string]int),
			}
			r.cfg.Store(cfg)

			// Burst-coalescing loop. Anything pokes `r.signal` with a short
			// reason string; the reconciler debounces and runs once per quiet
			// period. A single goroutine owns reconcile so we never race two
			// pmset reshuffles against each other.
			var wg sync.WaitGroup
			wg.Go(func() { r.run(ctx) })

			// Trigger 1: extension start → immediate reconcile.
			r.kick("startup")

			// Trigger 2 + 3: subscribe to the event bus for task_run.completed
			// (next-fire-time for that task just rolled forward) and
			// config.reloaded (tasks may have been added / removed / disabled).
			wg.Go(func() { r.runSubscribeLoop(ctx) })

			// Trigger 4: safety-net periodic reconcile.
			wg.Go(func() { r.runTicker(ctx) })

			// Trigger 5: react to our own config.yaml edits (lead_seconds /
			// enabled toggles) without needing an extension restart.
			wg.Go(func() { r.watchConfigFile(ctx) })

			<-ctx.Done()
			log.Println("bigband-wake: stopping; cancelling owned pmset wakes")
			// Shutdown reconcile: cancel everything we own. Use a fresh
			// context with a short deadline so we don't hang shutdown if sudo
			// is wedged.
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			r.cancelAll(shutdownCtx)
			wg.Wait()
			return nil
		},
	}
}

// reconciler owns the wake state and is the single goroutine that calls
// pmset. Everyone else nudges it through `signal`.
//
// cfg is an atomic.Pointer so the fsnotify-driven watcher can swap in a new
// snapshot without coordinating with the reconcile goroutine. Readers call
// cfg.Load() — never dereference the field directly — so a torn read can't
// observe a half-mutated *Config. State writes still live on the single
// reconcile goroutine so they don't need synchronisation.
// maxCancelFailures is the number of consecutive pmset cancel failures for a
// single entry before we give up and evict it from state to avoid an infinite
// retry loop. A CRITICAL log is emitted so the operator can investigate.
const maxCancelFailures = 3

type reconciler struct {
	cfg          atomic.Pointer[Config]
	bb           *bigbandext.Client
	state        *State
	signal       chan string
	failureCount map[string]int // wakeKey → consecutive cancel failure count
}

// kick requests a reconcile, tagged with a short reason for log lines.
// Non-blocking: if the buffer is full, another reconcile is already pending
// and another nudge adds no information.
func (r *reconciler) kick(reason string) {
	select {
	case r.signal <- reason:
	default:
	}
}

// run is the single-owner reconcile loop. Reads signals, debounces them, and
// calls reconcileOnce per quiet period.
func (r *reconciler) run(ctx context.Context) {
	var pending *time.Timer
	pendingReason := ""
	fire := make(chan struct{}, 1)
	for {
		select {
		case <-ctx.Done():
			if pending != nil {
				pending.Stop()
			}
			return
		case reason := <-r.signal:
			if pendingReason == "" {
				pendingReason = reason
			} else if !strings.Contains(pendingReason, reason) {
				pendingReason = pendingReason + "+" + reason
			}
			if pending != nil {
				pending.Stop()
			}
			pending = time.AfterFunc(reconcileDebounce, func() {
				select {
				case fire <- struct{}{}:
				default:
				}
			})
		case <-fire:
			reason := pendingReason
			pendingReason = ""
			r.reconcileOnce(ctx, reason)
		}
	}
}

// runTicker fires a `tick` reconcile every cfg.ReconcileEvery() so even an
// extension that has missed events (subscribe stream dropped, clock skewed
// after wake-from-sleep) eventually self-heals. The cadence is sampled once
// at startup; a config edit that changes reconcile_interval takes effect on
// the next extension restart (rare; the other triggers cover most cases).
func (r *reconciler) runTicker(ctx context.Context) {
	t := time.NewTicker(r.cfg.Load().ReconcileEvery())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.kick("tick")
		}
	}
}

// runSubscribeLoop maintains a long-lived subscribe to the bigband event
// stream, kicking the reconciler whenever a relevant envelope arrives.
// Backs off on reconnect like bigband-slack does.
func (r *reconciler) runSubscribeLoop(ctx context.Context) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	var lastSeen time.Time
	seen := make(map[string]struct{})
	for {
		err := r.streamOnce(ctx, &lastSeen, seen)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("bigband-wake: subscribe ended: %v (reconnecting in %s)", err, backoff)
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

func (r *reconciler) streamOnce(ctx context.Context, lastSeen *time.Time, seen map[string]struct{}) error {
	req := bigbandext.SubscribeRequest{
		Name: "bigband-wake",
		Types: []string{
			string(bigbandext.TypeTaskRunCompleted),
			string(bigbandext.TypeConfigReloaded),
		},
	}
	if !lastSeen.IsZero() {
		req.Since = lastSeen.UTC().Format(time.RFC3339)
		log.Printf("bigband-wake: subscribing with replay since=%s types=%v", req.Since, req.Types)
	} else {
		log.Printf("bigband-wake: subscribing live types=%v", req.Types)
	}
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	envCh, errCh := r.bb.Subscribe(subCtx, req)
	for {
		select {
		case env, ok := <-envCh:
			if !ok {
				if err, drained := <-errCh; drained && err != nil {
					return err
				}
				return nil
			}
			if _, dup := seen[env.EventID]; dup {
				continue
			}
			seen[env.EventID] = struct{}{}
			if env.OccurredAt.After(*lastSeen) {
				*lastSeen = env.OccurredAt
			}
			r.kick(string(env.Type))
		case err := <-errCh:
			if err != nil {
				return err
			}
			return nil
		}
	}
}

// watchConfigFile re-reads our config.yaml on every write so toggling
// `enabled` or tweaking `lead_seconds` takes effect without restarting the
// extension. The pattern matches bigband-slack's watcher.
func (r *reconciler) watchConfigFile(ctx context.Context) {
	path := ConfigPath()
	w, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("bigband-wake: cannot watch config file: %v", err)
		return
	}
	defer w.Close()
	if err := w.Add(path); err != nil {
		log.Printf("bigband-wake: fsnotify add %s: %v", path, err)
		return
	}
	log.Printf("bigband-wake: watching %s for changes", path)
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
			if ev.Op&(fsnotify.Rename|fsnotify.Remove) != 0 {
				_ = w.Remove(path)
				_ = w.Add(path)
			}
			if pending != nil {
				pending.Stop()
			}
			pending = time.AfterFunc(debounce, func() {
				newCfg, err := LoadConfig()
				if err != nil {
					log.Printf("bigband-wake: config reload failed: %v", err)
					return
				}
				r.cfg.Store(newCfg)
				log.Printf("bigband-wake: config reloaded enabled=%v lead=%s max_events=%d",
					newCfg.Enabled, newCfg.LeadDuration(), newCfg.MaxEventsValue())
				r.kick("config-edit")
			})
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			log.Printf("bigband-wake: fsnotify error: %v", err)
		}
	}
}

// reconcileOnce derives the desired wake set from the daemon's current task
// status, diffs against the persisted state, and applies the diff via pmset.
// All pmset side-effects funnel through here.
func (r *reconciler) reconcileOnce(ctx context.Context, reason string) {
	cfg := r.cfg.Load()
	if !cfg.Enabled {
		// User toggled enabled→false at some point. Clean up anything we own.
		if len(r.state.Events) > 0 {
			log.Printf("bigband-wake: enabled=false; cancelling %d owned wakes (reason=%s)", len(r.state.Events), reason)
			r.cancelAll(ctx)
		}
		return
	}

	desired, err := r.desiredWakeSet(ctx)
	if err != nil {
		log.Printf("bigband-wake: reconcile (reason=%s) failed to read daemon status: %v", reason, err)
		return
	}

	// Diff current vs desired by exact (task, wake_at) tuple. The exact time
	// is what pmset matches on, so any drift means "different event".
	curByKey := make(map[string]WakeEvent, len(r.state.Events))
	for _, e := range r.state.Events {
		curByKey[wakeKey(e.Task, e.WakeAt)] = e
	}
	wantByKey := make(map[string]WakeEvent, len(desired))
	for _, e := range desired {
		wantByKey[wakeKey(e.Task, e.WakeAt)] = e
	}

	var toCancel, toAdd []WakeEvent
	for k, e := range curByKey {
		if _, keep := wantByKey[k]; !keep {
			toCancel = append(toCancel, e)
		}
	}
	for k, e := range wantByKey {
		if _, have := curByKey[k]; !have {
			toAdd = append(toAdd, e)
		}
	}

	if len(toCancel) == 0 && len(toAdd) == 0 {
		log.Printf("bigband-wake: reconcile (reason=%s) no-op; %d wakes stable", reason, len(r.state.Events))
		return
	}
	log.Printf("bigband-wake: reconcile (reason=%s) cancel=%d add=%d desired=%d",
		reason, len(toCancel), len(toAdd), len(desired))

	// Sort for stable log output and ordered application.
	sort.Slice(toCancel, func(i, j int) bool { return toCancel[i].WakeAt.Before(toCancel[j].WakeAt) })
	sort.Slice(toAdd, func(i, j int) bool { return toAdd[i].WakeAt.Before(toAdd[j].WakeAt) })

	// Apply: cancel first so we never push above max_events transiently.
	keep := make([]WakeEvent, 0, len(desired))
	canceledKeys := make(map[string]struct{}, len(toCancel))
	for _, e := range toCancel {
		k := wakeKey(e.Task, e.WakeAt)
		if err := cancelPmsetWake(ctx, e.WakeAt); err != nil {
			r.failureCount[k]++
			if r.failureCount[k] >= maxCancelFailures {
				log.Printf("bigband-wake: CRITICAL cancel %s @ %s failed %d times; evicting from owned set",
					e.Task, e.WakeAt.Local().Format(time.RFC3339), r.failureCount[k])
				canceledKeys[k] = struct{}{} // treat as removed so state is cleaned up
				delete(r.failureCount, k)
			} else {
				log.Printf("bigband-wake: cancel %s @ %s failed (attempt %d/%d): %v",
					e.Task, e.WakeAt.Local().Format(time.RFC3339), r.failureCount[k], maxCancelFailures, err)
				keep = append(keep, e)
			}
			continue
		}
		delete(r.failureCount, k)
		canceledKeys[k] = struct{}{}
	}
	for _, e := range r.state.Events {
		if _, dropped := canceledKeys[wakeKey(e.Task, e.WakeAt)]; dropped {
			continue
		}
		// Already kept above? skip — we only re-add cancel-failed entries.
		if contains(keep, e) {
			continue
		}
		// Was already-current AND wanted? keep it.
		if _, wanted := wantByKey[wakeKey(e.Task, e.WakeAt)]; wanted {
			keep = append(keep, e)
		}
	}
	for _, e := range toAdd {
		if err := schedulePmsetWake(ctx, e.WakeAt); err != nil {
			log.Printf("bigband-wake: schedule %s @ %s failed: %v",
				e.Task, e.WakeAt.Local().Format(time.RFC3339), err)
			continue
		}
		e.ScheduledAt = time.Now().Local()
		keep = append(keep, e)
	}

	sort.Slice(keep, func(i, j int) bool { return keep[i].WakeAt.Before(keep[j].WakeAt) })
	r.state.Events = keep
	if err := r.state.Save(); err != nil {
		log.Printf("bigband-wake: CRITICAL failed to persist wake state after pmset changes: %v", err)
	}
}

// desiredWakeSet asks the bigband daemon for current task status and derives
// the set of wake events we'd like to own. Only enabled scheduled tasks with
// a future next_run produce entries; one-off / disabled / done tasks are
// skipped.
func (r *reconciler) desiredWakeSet(ctx context.Context) ([]WakeEvent, error) {
	_ = ctx // bigbandext.Client uses its own dial timeout; ctx reserved for future use.
	st, err := r.bb.Status()
	if err != nil {
		return nil, err
	}
	cfg := r.cfg.Load()
	now := time.Now()
	lead := cfg.LeadDuration()
	out := make([]WakeEvent, 0, len(st.Tasks))
	for _, t := range st.Tasks {
		if !t.Enabled || t.NextRun == "" {
			continue
		}
		// Filter pseudo-states emitted for one-off / disabled tasks.
		switch t.NextRun {
		case "disabled", "pending", "done", "running":
			continue
		}
		fire, err := time.ParseInLocation(nextRunLayout, t.NextRun, time.Local)
		if err != nil {
			log.Printf("bigband-wake: ignoring task %q with unparseable next_run %q: %v", t.Name, t.NextRun, err)
			continue
		}
		wakeAt := fire.Add(-lead)
		if !wakeAt.After(now) {
			// Too late — the wake would be in the past or right now. Skip;
			// macOS won't honor a past time anyway.
			continue
		}
		out = append(out, WakeEvent{
			Task:   t.Name,
			WakeAt: wakeAt,
			FireAt: fire,
		})
	}
	// Sort and cap to max_events, keeping the soonest firings.
	sort.Slice(out, func(i, j int) bool { return out[i].WakeAt.Before(out[j].WakeAt) })
	if maxEvents := cfg.MaxEventsValue(); len(out) > maxEvents {
		out = out[:maxEvents]
	}
	return out, nil
}

// cancelAll cancels every wake currently in state. Tolerates per-entry
// failures so a single stuck cancel doesn't trap the others. Non-darwin
// builds never schedule wakes in the first place, so state.Events is empty
// and this is a no-op.
func (r *reconciler) cancelAll(ctx context.Context) {
	var remaining []WakeEvent
	for _, e := range r.state.Events {
		if err := cancelPmsetWake(ctx, e.WakeAt); err != nil {
			log.Printf("bigband-wake: cancelAll: %s @ %s failed: %v",
				e.Task, e.WakeAt.Local().Format(time.RFC3339), err)
			remaining = append(remaining, e)
		}
	}
	r.state.Events = remaining
	if err := r.state.Save(); err != nil {
		log.Printf("bigband-wake: WARNING failed to persist state in cancelAll: %v", err)
	}
}

// wakeKey identifies a wake entry for diffing. The (task, wake_at) tuple is
// what we own; pmset itself only knows the time, but we track the task too
// so re-add after cancel-failed retries doesn't double up.
func wakeKey(task string, wakeAt time.Time) string {
	return task + "|" + wakeAt.UTC().Format(time.RFC3339Nano)
}

func contains(events []WakeEvent, target WakeEvent) bool {
	for _, e := range events {
		if e.Task == target.Task && e.WakeAt.Equal(target.WakeAt) {
			return true
		}
	}
	return false
}
