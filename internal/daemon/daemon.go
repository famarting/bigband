// Package daemon runs the bigband scheduling daemon.
package daemon

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"path/filepath"

	"github.com/famarting/bigband/internal/config"
	"github.com/famarting/bigband/internal/events"
	"github.com/famarting/bigband/internal/extensions"
	"github.com/famarting/bigband/internal/ipc"
	"github.com/famarting/bigband/internal/paths"
	"github.com/famarting/bigband/internal/proc"
	"github.com/famarting/bigband/internal/runner"
	"github.com/famarting/bigband/internal/scheduler"
	"github.com/famarting/bigband/internal/state"
	"github.com/famarting/bigband/internal/worktree"
)

// IPC accept-loop limits. A local client should never need more than a fraction
// of either: typical Cmds are <1 KiB and arrive immediately. The bounds exist
// to keep a stuck or hostile peer from pinning a goroutine or growing memory.
const (
	ipcInitialReadTimeout = 5 * time.Second
	ipcMaxCmdBytes        = 1 << 20 // 1 MiB
)

// Run is the daemon entrypoint.
func Run() error {
	if err := paths.EnsureDirs(); err != nil {
		return fmt.Errorf("creating directories: %w", err)
	}

	// Single-instance guard. Two daemons sharing ~/.bigband-tasks (e.g. a
	// leftover from a renamed launchd plist) silently corrupt state.json and
	// race on daemon.sock — see the bigband-slack/state.json race that drops
	// Slack replies. Refuse to start a duplicate and tell the operator which
	// PID is holding the lock.
	releaseLock, holder, err := proc.AcquireInstanceLock(paths.InstanceLock())
	if err != nil {
		if holder > 0 {
			return fmt.Errorf("bigband daemon is already running as pid=%d (lock %s) — stop it before starting another", holder, paths.InstanceLock())
		}
		return fmt.Errorf("acquire instance lock: %w", err)
	}
	defer releaseLock()

	if err := writePID(); err != nil {
		return err
	}
	defer os.Remove(paths.PidFile())

	// Daemon-wide shutdown context: cancelled on SIGINT/SIGTERM. All long-lived
	// goroutines started below derive from this so they exit cleanly when the
	// daemon stops.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	// Tee daemon logs to file and stdout (launchd redirects stdout to the log
	// file anyway, so this is always safe and makes `bigband daemon` watchable).
	lf, err := os.OpenFile(paths.DaemonLog(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("opening daemon log: %w", err)
	}
	defer lf.Close()
	// Only tee to stdout when it's a terminal. Under launchd, stdout is already
	// redirected to this same log file, so tee-ing would double every line.
	var logDest io.Writer = lf
	if fi, err := os.Stdout.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		logDest = io.MultiWriter(lf, os.Stdout)
	}
	log.SetOutput(logDest)
	log.SetFlags(log.LstdFlags)
	log.Println("bigband daemon starting")

	st, err := state.Load()
	if err != nil {
		log.Printf("WARNING: could not load state: %v", err)
		st = &state.State{Tasks: map[string]*state.TaskState{}}
	}

	initialCfg, err := config.Load(paths.Config())
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	// cfgPtr holds the live config. The fsnotify-driven Watch callback below
	// replaces it; readers (IPC handlers, prune loop) Load() to get a current
	// snapshot. This avoids a data race on the bare local variable.
	var cfgPtr atomic.Pointer[config.Config]
	cfgPtr.Store(initialCfg)

	reconcileOrphans(ctx, initialCfg, st)

	bus, err := events.NewBus(paths.EventsFile())
	if err != nil {
		return fmt.Errorf("opening events bus: %w", err)
	}
	defer bus.Close()

	// Mirror lifecycle events into the daemon log so operators can trace
	// what's happening without tailing each per-task log file. The events
	// JSONL file is still the structured ground truth; this is the human
	// summary. Goroutine exits when bus.Close (deferred above) closes the
	// subscriber channel.
	go logBusEvents(bus)

	// Extension supervisor: spawns and watches every extension declared by a
	// manifest under ~/.bigband-tasks/extensions/<name>/manifest.yaml. This
	// is what lets the user run only `bigband install` and have all
	// integrations come up automatically — no per-extension LaunchAgents.
	extDir := filepath.Join(paths.Root(), "extensions")
	sup := extensions.NewSupervisor(extDir, bus, log.Default())
	if err := extensions.Discover(extDir, sup, log.Default()); err != nil {
		log.Printf("bigband: extension discovery failed: %v", err)
	}
	stopWatchExt, err := extensions.Watch(extDir, sup, log.Default())
	if err != nil {
		log.Printf("bigband: extension watcher failed: %v", err)
	} else {
		defer stopWatchExt()
	}
	defer sup.Shutdown(30 * time.Second)

	// Periodically prune ephemeral one-off entries (state + logs) older
	// than the configured retention. Configured tasks are never pruned.
	go runPruneLoop(ctx, &cfgPtr, st)

	started := time.Now()

	var (
		wg        sync.WaitGroup
		cancelsMu sync.Mutex
		cancels   = map[string]context.CancelFunc{}
	)

	runTask := func(c *config.Config, t *config.Task) {
		ctx, cancel := context.WithCancel(context.Background())
		cancelsMu.Lock()
		cancels[t.Name] = cancel
		cancelsMu.Unlock()
		defer func() {
			cancelsMu.Lock()
			delete(cancels, t.Name)
			cancelsMu.Unlock()
			cancel()
		}()
		runner.Run(ctx, c, t, st, io.Discard, bus)
	}

	// launchTask spawns runTask in a goroutine tracked by the WaitGroup so that
	// graceful shutdown can wait for in-flight tasks to finish. The scheduler and
	// IPC "run" handler both call this instead of go+runTask directly.
	launchTask := func(c *config.Config, t *config.Task) {
		mode := "fresh"
		if t.ResumeSessionID != "" {
			mode = "resume:" + t.ResumeSessionID
		}
		log.Printf("bigband: launching task=%s folder=%s ephemeral=%v %s", t.Name, t.Folder, t.Ephemeral, mode)
		wg.Add(1)
		go func() {
			defer wg.Done()
			runTask(c, t)
		}()
	}

	stopTask := func(name string) bool {
		cancelsMu.Lock()
		cancel, ok := cancels[name]
		cancelsMu.Unlock()
		if ok {
			cancel()
		}
		return ok
	}

	sched := scheduler.New(launchTask, func(name string) bool {
		return st.Get(name).LastRun != nil
	})
	sched.Reload(cfgPtr.Load())

	stopWatch, err := config.Watch(paths.Config(), func(newCfg *config.Config) {
		log.Println("config reloaded")
		cfgPtr.Store(newCfg)
		sched.Reload(newCfg)
		bus.Publish(events.Envelope{
			Type:   events.TypeConfigReloaded,
			Source: events.SourceDaemon,
			Data:   events.MustData(configReloadedPayload(newCfg)),
		})
	})
	if err != nil {
		log.Printf("WARNING: cannot watch config: %v", err)
	} else {
		defer stopWatch()
	}

	stopIPC, err := ipc.Serve(func(conn net.Conn) {
		defer conn.Close()
		// Bound the initial command read: a misbehaving or malicious local
		// client must not be able to hold a connection open forever or stream
		// an unbounded payload. After the command is parsed, the deadline is
		// cleared because subscribe holds the connection open indefinitely
		// and the other actions only write replies from here on.
		if err := conn.SetReadDeadline(time.Now().Add(ipcInitialReadTimeout)); err != nil {
			return
		}
		var cmd ipc.Cmd
		if err := json.NewDecoder(io.LimitReader(conn, ipcMaxCmdBytes)).Decode(&cmd); err != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Time{})
		// Subscribe holds the connection open and streams envelopes.
		if cmd.Action == "subscribe" {
			handleSubscribe(conn, cmd, bus)
			return
		}
		// Subscribers introspection — needs the bus, so route here.
		if cmd.Action == "subscribers" {
			subs := bus.Subscribers()
			out := make([]ipc.SubscriberInfo, 0, len(subs))
			for _, s := range subs {
				types := make([]string, 0, len(s.Types))
				for _, t := range s.Types {
					types = append(types, string(t))
				}
				out = append(out, ipc.SubscriberInfo{
					Name:        s.Name,
					Types:       types,
					Tasks:       s.Tasks,
					ConnectedAt: s.ConnectedAt,
					LagDropped:  s.LagDropped,
				})
			}
			raw, err := json.Marshal(ipc.SubscribersReply{Subscribers: out})
			if err != nil {
				_ = json.NewEncoder(conn).Encode(ipc.Reply{OK: false, Error: "marshal subscribers: " + err.Error()})
				return
			}
			if err := json.NewEncoder(conn).Encode(ipc.Reply{OK: true, Payload: raw}); err != nil {
				log.Printf("bigband: ipc encode subscribers reply: %v", err)
			}
			return
		}
		reply := handleCmd(cmd, cfgPtr.Load(), sched, st, started, launchTask, stopTask, sup)
		if err := json.NewEncoder(conn).Encode(reply); err != nil {
			log.Printf("bigband: ipc encode reply action=%s: %v", cmd.Action, err)
		}
	})
	if err != nil {
		return fmt.Errorf("starting IPC: %w", err)
	}
	defer stopIPC()

	var scheduled, oneOff, disabled int
	for _, t := range cfgPtr.Load().Tasks {
		switch {
		case !t.IsEnabled():
			disabled++
		case t.IsOneOff():
			oneOff++
		default:
			scheduled++
		}
	}
	log.Printf("bigband daemon ready, %d scheduled, %d one-off, %d disabled", scheduled, oneOff, disabled)

	<-ctx.Done()
	log.Println("bigband daemon stopping")
	sched.Stop()

	// Cancel all in-flight tasks, then wait up to the grace period for them to
	// finish. killProcessGroupOnCancel sends SIGTERM to each subprocess group;
	// WaitDelay in the runner escalates to SIGKILL after 5 s, so tasks that
	// ignore SIGTERM are still collected well within the 30 s window.
	cancelsMu.Lock()
	for _, cancel := range cancels {
		cancel()
	}
	cancelsMu.Unlock()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	const grace = 30 * time.Second
	select {
	case <-done:
	case <-time.After(grace):
		log.Printf("bigband: grace period (%s) exceeded, some tasks may not have finished cleanly", grace)
	}
	return nil
}

// configReloadedPayload builds the ConfigReloadedData payload from a parsed
// config snapshot. Counts mirror the daemon's startup summary so subscribers
// can log a meaningful line on every reload without re-parsing the file.
func configReloadedPayload(cfg *config.Config) events.ConfigReloadedData {
	var scheduled, oneOff, disabled int
	for _, t := range cfg.Tasks {
		switch {
		case !t.IsEnabled():
			disabled++
		case t.IsOneOff():
			oneOff++
		default:
			scheduled++
		}
	}
	return events.ConfigReloadedData{
		TaskCount:      len(cfg.Tasks),
		ScheduledCount: scheduled,
		OneOffCount:    oneOff,
		DisabledCount:  disabled,
		TemplatesCount: len(cfg.Templates),
	}
}

func handleCmd(cmd ipc.Cmd, cfg *config.Config, sched *scheduler.Scheduler, st *state.State, started time.Time, runTask func(*config.Config, *config.Task), stopTask func(string) bool, sup *extensions.Supervisor) ipc.Reply {
	switch cmd.Action {
	case "ping":
		return ipc.Reply{OK: true}

	case "status":
		nextRuns := sched.NextRuns()
		var tasks []ipc.TaskStatus
		seen := map[string]bool{}
		for _, t := range cfg.Tasks {
			seen[t.Name] = true
			ts := st.Get(t.Name)
			next := nextRuns[t.Name]
			switch {
			case !t.IsEnabled():
				next = "disabled"
			case t.IsOneOff():
				if ts.LastRun != nil {
					next = "done"
				} else {
					next = "pending"
				}
			}
			lastRun := ""
			if ts.LastRun != nil {
				lastRun = ts.LastRun.Local().Format("2006-01-02 15:04:05")
			}
			tasks = append(tasks, ipc.TaskStatus{
				Name:         t.Name,
				Schedule:     t.Schedule,
				Enabled:      t.IsEnabled(),
				NextRun:      next,
				LastRun:      lastRun,
				LastStatus:   string(ts.LastStatus),
				LastDuration: ts.LastDuration,
				LastLog:      ts.LastLog,
				WorktreePath: ts.WorktreePath,
				Folder:       t.Folder,
				WorktreeMode: t.WorktreeMode(),
				SessionID:    ts.SessionID,
				Prompt:       t.Prompt,
			})
		}
		// Surface ephemeral submissions (state-only — never made it into
		// config.yaml). Without this they're invisible to `bb list` and
		// `bb status`. Sorted for stable output.
		var ephemeralNames []string
		for name, ts := range st.Tasks {
			if seen[name] || ts == nil || ts.LastRun == nil {
				continue
			}
			ephemeralNames = append(ephemeralNames, name)
		}
		sort.Strings(ephemeralNames)
		for _, name := range ephemeralNames {
			ts := st.Tasks[name]
			lastRun := ts.LastRun.Local().Format("2006-01-02 15:04:05")
			next := "done"
			if ts.LastStatus == state.StatusRunning {
				next = "running"
			}
			tasks = append(tasks, ipc.TaskStatus{
				Name:         name,
				Schedule:     "one-off",
				Enabled:      true,
				NextRun:      next,
				LastRun:      lastRun,
				LastStatus:   string(ts.LastStatus),
				LastDuration: ts.LastDuration,
				LastLog:      ts.LastLog,
				WorktreePath: ts.WorktreePath,
				Folder:       ts.Folder,
				Ephemeral:    true,
				SessionID:    ts.SessionID,
			})
		}
		payload := ipc.StatusPayload{
			Uptime: time.Since(started).Round(time.Second).String(),
			Tasks:  tasks,
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return ipc.Reply{OK: false, Error: "marshal status: " + err.Error()}
		}
		return ipc.Reply{OK: true, Payload: raw}

	case "run":
		t := cfg.TaskByName(cmd.Task)
		if t == nil {
			log.Printf("bigband: ipc run task=%s rejected: unknown", cmd.Task)
			return ipc.Reply{OK: false, Error: "unknown task: " + cmd.Task}
		}
		log.Printf("bigband: ipc run task=%s", cmd.Task)
		t.ClearJitter() // Don't apply jitter to manual runs.
		runTask(cfg, t)
		return ipc.Reply{OK: true}

	case "stop":
		if !stopTask(cmd.Task) {
			log.Printf("bigband: ipc stop task=%s rejected: not running", cmd.Task)
			return ipc.Reply{OK: false, Error: "task " + cmd.Task + " is not running"}
		}
		log.Printf("bigband: ipc stop task=%s", cmd.Task)
		return ipc.Reply{OK: true}

	case "forget":
		if cmd.Task == "" {
			return ipc.Reply{OK: false, Error: "forget: task name required"}
		}
		ts := st.Get(cmd.Task)
		if ts.RunningPID != 0 {
			// Don't drop state for an in-flight run — the runner needs the
			// row to record completion. CLI rm refuses this case anyway.
			log.Printf("bigband: ipc forget task=%s rejected: running", cmd.Task)
			return ipc.Reply{OK: false, Error: "task " + cmd.Task + " has a running pid; stop it first"}
		}
		if err := st.RemoveTask(cmd.Task); err != nil {
			return ipc.Reply{OK: false, Error: err.Error()}
		}
		log.Printf("bigband: ipc forget task=%s", cmd.Task)
		return ipc.Reply{OK: true}

	case "submit":
		if cmd.Submit == nil {
			return ipc.Reply{OK: false, Error: "submit: missing payload"}
		}
		t, runID, err := buildSubmittedTask(cmd.Submit, cfg, st)
		if err != nil {
			log.Printf("bigband: ipc submit rejected: %v", err)
			return ipc.Reply{OK: false, Error: err.Error()}
		}
		log.Printf("bigband: ipc submit task=%s folder=%s parent_session=%q triggered_by=%q ephemeral=%v",
			t.Name, t.Folder, cmd.Submit.ParentSessionID, cmd.Submit.TriggeredBy, cmd.Submit.Ephemeral)
		logPath := filepath.Join(paths.TaskLogDir(t.Name), t.RunTimestamp+".log")
		runTask(cfg, t)
		raw, err := json.Marshal(ipc.SubmitRunReply{
			RunID:    runID,
			TaskName: t.Name,
			LogPath:  logPath,
		})
		if err != nil {
			return ipc.Reply{OK: false, Error: "marshal submit reply: " + err.Error()}
		}
		return ipc.Reply{OK: true, Payload: raw}

	case "ext_list":
		views := sup.List()
		out := make([]ipc.ExtensionInfo, 0, len(views))
		for _, v := range views {
			out = append(out, ipc.ExtensionInfo{
				Name:         v.Name,
				Status:       v.Status,
				PID:          v.PID,
				ManifestPath: v.ManifestPath,
				Restarts:     v.Restarts,
				StartedAt:    v.StartedAt,
				LastExitCode: v.LastExitCode,
				LastExitAt:   v.LastExitAt,
				LastError:    v.LastError,
				Enabled:      v.Enabled,
				Description:  v.Description,
				LogPath:      v.LogPath,
			})
		}
		raw, err := json.Marshal(ipc.ExtListReply{Extensions: out})
		if err != nil {
			return ipc.Reply{OK: false, Error: "marshal ext_list: " + err.Error()}
		}
		return ipc.Reply{OK: true, Payload: raw}

	case "ext_start":
		if cmd.Extension == "" {
			return ipc.Reply{OK: false, Error: "ext_start: extension name required"}
		}
		if err := sup.Start(cmd.Extension); err != nil {
			return ipc.Reply{OK: false, Error: err.Error()}
		}
		log.Printf("bigband: ipc ext_start %s", cmd.Extension)
		return ipc.Reply{OK: true}

	case "ext_stop":
		if cmd.Extension == "" {
			return ipc.Reply{OK: false, Error: "ext_stop: extension name required"}
		}
		if err := sup.Stop(cmd.Extension); err != nil {
			return ipc.Reply{OK: false, Error: err.Error()}
		}
		log.Printf("bigband: ipc ext_stop %s", cmd.Extension)
		return ipc.Reply{OK: true}

	case "ext_restart":
		if cmd.Extension == "" {
			return ipc.Reply{OK: false, Error: "ext_restart: extension name required"}
		}
		if err := sup.Restart(cmd.Extension); err != nil {
			return ipc.Reply{OK: false, Error: err.Error()}
		}
		log.Printf("bigband: ipc ext_restart %s", cmd.Extension)
		return ipc.Reply{OK: true}

	default:
		return ipc.Reply{OK: false, Error: "unknown action: " + cmd.Action}
	}
}

// handleSubscribe streams events to a connected client until the client
// disconnects or the bus closes. Sends one OK reply, then NDJSON envelopes.
//
// When SubscribeRequest.Since is set, the daemon does at-least-once replay
// from events.jsonl: it subscribes to the live bus first (buffering), tails
// the file from the cursor emitting matching envelopes, then drains the
// buffered live tail skipping any event_id already replayed.
//
// Failures writing to the connection silently end the subscription.
func handleSubscribe(conn net.Conn, cmd ipc.Cmd, bus *events.Bus) {
	var (
		name  string
		types []events.Type
		tasks []string
		since time.Time
	)
	if cmd.Subscribe != nil {
		name = cmd.Subscribe.Name
		for _, t := range cmd.Subscribe.Types {
			types = append(types, events.Type(t))
		}
		tasks = cmd.Subscribe.Tasks
		if cmd.Subscribe.Since != "" {
			t, err := time.Parse(time.RFC3339, cmd.Subscribe.Since)
			if err != nil {
				_ = json.NewEncoder(conn).Encode(ipc.Reply{OK: false, Error: "subscribe: invalid since timestamp: " + err.Error()})
				return
			}
			since = t.UTC()
		}
	}
	if name == "" {
		name = "anon"
	}
	if err := json.NewEncoder(conn).Encode(ipc.Reply{OK: true}); err != nil {
		return
	}
	filter := events.Filter{Types: types, Tasks: tasks}
	log.Printf("bigband: subscriber connected name=%q types=%v tasks=%v replay_since=%v", name, types, tasks, since)

	// Subscribe to live first so events that arrive during replay aren't lost.
	ch, cancel := bus.Subscribe(filter, name)
	defer cancel()
	enc := json.NewEncoder(conn)

	// Replay from events.jsonl when requested.
	replayed := map[string]struct{}{}
	if !since.IsZero() {
		if err := streamReplay(enc, filter, since, replayed); err != nil {
			log.Printf("bigband: subscriber name=%q replay error: %v", name, err)
			return
		}
		log.Printf("bigband: subscriber name=%q replayed %d event(s) from %s", name, len(replayed), since.Format(time.RFC3339))
	}

	// Drain live channel; skip anything we already replayed.
	for env := range ch {
		if _, ok := replayed[env.EventID]; ok {
			continue
		}
		if err := enc.Encode(env); err != nil {
			log.Printf("bigband: subscriber disconnected name=%q: %v", name, err)
			return
		}
	}
	log.Printf("bigband: subscriber stream closed name=%q", name)
}

// streamReplay reads events.jsonl line by line, decodes each envelope, and
// emits those at-or-after since matching the filter. Records emitted EventIDs
// in seen so the live loop can skip duplicates.
func streamReplay(enc *json.Encoder, filter events.Filter, since time.Time, seen map[string]struct{}) error {
	f, err := os.Open(paths.EventsFile())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // no events yet — nothing to replay
		}
		return fmt.Errorf("open events file: %w", err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var env events.Envelope
		if err := json.Unmarshal(line, &env); err != nil {
			continue // skip malformed
		}
		if env.OccurredAt.Before(since) {
			continue
		}
		if !filter.Match(env) {
			continue
		}
		if err := enc.Encode(env); err != nil {
			return err
		}
		seen[env.EventID] = struct{}{}
	}
	return scanner.Err()
}

// runPruneLoop runs pruneOnce at startup and then once per hour until ctx is
// cancelled. cfgPtr is loaded each tick so config hot-reload is picked up.
func runPruneLoop(ctx context.Context, cfgPtr *atomic.Pointer[config.Config], st *state.State) {
	pruneOnce(cfgPtr.Load(), st)
	t := time.NewTicker(1 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pruneOnce(cfgPtr.Load(), st)
		}
	}
}

// pruneOnce removes ephemeral state entries (and their log dirs + worktrees)
// older than the configured retention. No-op when retention is zero.
func pruneOnce(cfg *config.Config, st *state.State) {
	if cfg == nil {
		return
	}
	retention := cfg.Defaults.EphemeralRetention.Duration
	if retention <= 0 {
		return
	}
	cutoff := time.Now().Add(-retention)
	configured := configuredNames(cfg)
	removed := st.RemoveStaleEphemerals(configured, cutoff)
	for _, r := range removed {
		dir := paths.TaskLogDir(r.Name)
		if err := os.RemoveAll(dir); err != nil {
			log.Printf("bigband: prune logs %s: %v", dir, err)
		}
		if r.WorktreePath != "" {
			pruneEphemeralWorktree(r)
		}
		log.Printf("bigband: pruned ephemeral task=%s (last_run before %s)", r.Name, cutoff.Format(time.RFC3339))
	}
}

// pruneEphemeralWorktree removes the worktree that an ephemeral task owned.
// Configured tasks never reach this code path — RemoveStaleEphemerals filters
// them out — so the only worktrees touched here are <repo>-bb-<oneoff-...>
// dirs created by the ephemeral that's being pruned. worktree.Remove enforces
// its own guardrails (sibling-of-repo-root + "<repo>-bb-" basename prefix), so
// a corrupted state.json cannot weaponise this path.
func pruneEphemeralWorktree(r state.RemovedEphemeral) {
	if r.Folder == "" {
		log.Printf("bigband: prune worktree %s for task=%s skipped: no recorded folder, cannot resolve repo root", r.WorktreePath, r.Name)
		return
	}
	repoRoot, err := worktree.RepoRoot(r.Folder)
	if err != nil {
		log.Printf("bigband: prune worktree %s for task=%s skipped: %v", r.WorktreePath, r.Name, err)
		return
	}
	if _, err := os.Stat(r.WorktreePath); err != nil {
		return
	}
	if err := worktree.Remove(repoRoot, r.WorktreePath); err != nil {
		log.Printf("bigband: prune worktree %s for task=%s: %v", r.WorktreePath, r.Name, err)
		return
	}
	log.Printf("bigband: pruned worktree %s for task=%s", r.WorktreePath, r.Name)
}

// configuredNames returns the set of task and template names from cfg, used
// to protect them from ephemeral pruning.
func configuredNames(cfg *config.Config) map[string]bool {
	out := make(map[string]bool, len(cfg.Tasks)+len(cfg.Templates))
	for _, t := range cfg.Tasks {
		out[t.Name] = true
	}
	for _, t := range cfg.Templates {
		out[t.Name] = true
	}
	return out
}

// logBusEvents subscribes to the full event firehose and writes a one-line
// human summary to the daemon log per envelope. Runs until the bus is closed.
func logBusEvents(bus *events.Bus) {
	ch, cancel := bus.Subscribe(events.Filter{}, "daemon-log")
	defer cancel()
	for env := range ch {
		summary := summarizeEvent(env)
		if env.TriggeredBy != "" {
			summary += " triggered_by=" + env.TriggeredBy
		}
		log.Printf("event task=%s run=%s %s %s", env.TaskName, env.RunID, env.Type, summary)
	}
}

// summarizeEvent returns a compact key=value summary of an envelope's payload.
// Each event type only surfaces the fields that are useful for tracing.
func summarizeEvent(env events.Envelope) string {
	switch env.Type {
	case events.TypeTaskRunStarted:
		var d events.TaskRunStartedData
		_ = json.Unmarshal(env.Data, &d)
		parts := []string{"folder=" + quote(d.Folder)}
		if d.Schedule != "" {
			parts = append(parts, "schedule="+quote(d.Schedule))
		}
		if d.OneOff {
			parts = append(parts, "one_off=true")
		}
		if d.Resume {
			parts = append(parts, "resume_from="+d.ResumeFrom)
		}
		if d.Ephemeral {
			parts = append(parts, "ephemeral=true")
		}
		return strings.Join(parts, " ")
	case events.TypeTaskRunWorktreeReady:
		var d events.TaskRunWorktreeReadyData
		_ = json.Unmarshal(env.Data, &d)
		return "worktree=" + quote(d.WorktreePath)
	case events.TypeClaudeSessionStarted:
		var d events.ClaudeSessionStartedData
		_ = json.Unmarshal(env.Data, &d)
		return "session_id=" + d.SessionID
	case events.TypeClaudeTurnCompleted:
		var d events.ClaudeTurnCompletedData
		_ = json.Unmarshal(env.Data, &d)
		parts := []string{}
		if d.Subtype != "" {
			parts = append(parts, "subtype="+d.Subtype)
		}
		if d.NumTurns > 0 {
			parts = append(parts, fmt.Sprintf("turns=%d", d.NumTurns))
		}
		if d.DurationMS > 0 {
			parts = append(parts, fmt.Sprintf("duration=%dms", d.DurationMS))
		}
		if d.CostUSD > 0 {
			parts = append(parts, fmt.Sprintf("cost=$%.4f", d.CostUSD))
		}
		if len(d.FinalMessage) > 0 {
			parts = append(parts, fmt.Sprintf("msg_len=%d", len(d.FinalMessage)))
		}
		return strings.Join(parts, " ")
	case events.TypeClaudeWakeup:
		var d events.ClaudeWakeupData
		_ = json.Unmarshal(env.Data, &d)
		return fmt.Sprintf("delay=%ds", d.DelaySeconds)
	case events.TypeTaskRunCompleted:
		var d events.TaskRunCompletedData
		_ = json.Unmarshal(env.Data, &d)
		parts := []string{"status=" + d.Status, fmt.Sprintf("duration=%dms", d.DurationMS)}
		if d.SessionID != "" {
			parts = append(parts, "session_id="+d.SessionID)
		}
		if d.WorktreePath != "" {
			parts = append(parts, "worktree="+quote(d.WorktreePath))
		}
		if len(d.FinalMessage) > 0 {
			parts = append(parts, fmt.Sprintf("msg_len=%d", len(d.FinalMessage)))
		}
		return strings.Join(parts, " ")
	case events.TypeTaskRunPreFailed:
		var d events.TaskRunPreFailedData
		_ = json.Unmarshal(env.Data, &d)
		return fmt.Sprintf("command=%s error=%s", quote(d.Command), quote(d.Error))
	case events.TypeSubscriberLagged:
		return "(events dropped — subscriber too slow)"
	case events.TypeExtensionStarted:
		var d events.ExtensionStartedData
		_ = json.Unmarshal(env.Data, &d)
		return fmt.Sprintf("name=%s pid=%d", d.Name, d.PID)
	case events.TypeExtensionExited:
		var d events.ExtensionExitedData
		_ = json.Unmarshal(env.Data, &d)
		parts := []string{
			"name=" + d.Name,
			fmt.Sprintf("exit=%d", d.ExitCode),
			fmt.Sprintf("duration=%dms", d.DurationMS),
			fmt.Sprintf("will_restart=%v", d.WillRestart),
		}
		if d.Signal != "" {
			parts = append(parts, "signal="+d.Signal)
		}
		return strings.Join(parts, " ")
	case events.TypeExtensionFailed:
		var d events.ExtensionFailedData
		_ = json.Unmarshal(env.Data, &d)
		return "name=" + d.Name + " error=" + quote(d.Error)
	case events.TypeConfigReloaded:
		var d events.ConfigReloadedData
		_ = json.Unmarshal(env.Data, &d)
		return fmt.Sprintf("tasks=%d scheduled=%d one_off=%d disabled=%d templates=%d",
			d.TaskCount, d.ScheduledCount, d.OneOffCount, d.DisabledCount, d.TemplatesCount)
	}
	return ""
}

func quote(s string) string {
	if strings.ContainsAny(s, " \t") {
		return strconv.Quote(s)
	}
	return s
}

// buildSubmittedTask converts an IPC SubmitRunRequest into an in-memory Task
// suitable for runner.Run. Validates the folder exists, fills in a default
// name when blank, and parses the optional timeout.
//
// The returned task is never persisted to config.yaml — Ephemeral=true marks
// it so callers (e.g. config.Save) can skip it. State entries created during
// the run still land in state.json so logs and follow-ups remain addressable.
func buildSubmittedTask(req *ipc.SubmitRunRequest, cfg *config.Config, st *state.State) (*config.Task, string, error) {
	if req.Folder == "" {
		return nil, "", fmt.Errorf("submit: folder is required")
	}
	info, err := os.Stat(req.Folder)
	if err != nil {
		return nil, "", fmt.Errorf("submit: folder %q: %w", req.Folder, err)
	}
	if !info.IsDir() {
		return nil, "", fmt.Errorf("submit: folder %q: not a directory", req.Folder)
	}
	if err := cfg.CheckFolderAllowed(req.Folder); err != nil {
		return nil, "", fmt.Errorf("submit: %w", err)
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, "", fmt.Errorf("submit: prompt is required")
	}

	name := req.Name
	if name == "" {
		// Random hex suffix avoids the uppercase T/Z that timestamp formats
		// would produce (config.IsValidName rejects them) and keeps auto-names
		// short and unique.
		var buf [4]byte
		if _, err := rand.Read(buf[:]); err != nil {
			return nil, "", fmt.Errorf("submit: cannot generate name: %w", err)
		}
		name = "oneoff-" + hex.EncodeToString(buf[:])
	}
	if !config.IsValidName(name) {
		return nil, "", fmt.Errorf("submit: invalid name %q", name)
	}
	// Reject collisions with any task currently in config.yaml — we never want
	// a submitted run to be confused with a configured task.
	if cfg.TaskByName(name) != nil {
		return nil, "", fmt.Errorf("submit: name %q collides with an existing configured task", name)
	}
	// Also reject if an ephemeral with this name is already running — two
	// concurrent runs sharing a name would clobber each other's state slot.
	if st.Get(name).RunningPID != 0 {
		return nil, "", fmt.Errorf("submit: name %q collides with a currently running task", name)
	}

	// Pin the run timestamp now so the synchronously-returned run id matches
	// the one the runner emits on lifecycle events (it'll use this same ts as
	// the log filename).
	ts := time.Now().UTC().Format("2006-01-02T15-04-05Z")

	t := &config.Task{
		Name:            name,
		Folder:          req.Folder,
		Prompt:          req.Prompt,
		PreExec:         req.PreExec,
		PostExec:        req.PostExec,
		Worktree:        req.Worktree,
		KeepWorktree:    req.KeepWorktree,
		Model:           req.Model,
		Effort:          req.Effort,
		ResumeSessionID: req.ParentSessionID,
		Ephemeral:       req.Ephemeral,
		TriggeredBy:     req.TriggeredBy,
		RunTimestamp:    ts,
	}
	if req.Timeout != "" {
		dur, err := time.ParseDuration(req.Timeout)
		if err != nil {
			return nil, "", fmt.Errorf("submit: invalid timeout %q: %w", req.Timeout, err)
		}
		t.Timeout = &config.Duration{Duration: dur}
	}
	runID := name + "/" + ts
	return t, runID, nil
}

func reconcileOrphans(ctx context.Context, cfg *config.Config, st *state.State) {
	configured := map[string]bool{}
	for _, task := range cfg.Tasks {
		configured[task.Name] = true
		reconcileOrphan(ctx, task.Name, st)
	}
	// Also sweep ephemeral state entries that were running when the daemon last
	// stopped. Without this, a crashed ephemeral task keeps RunningPID set
	// forever, making it appear "running" and blocking forget/rm.
	for name := range st.Tasks {
		if !configured[name] {
			reconcileOrphan(ctx, name, st)
		}
	}
}

func reconcileOrphan(ctx context.Context, name string, st *state.State) {
	ts := st.Get(name)
	if ts.RunningPID == 0 {
		return
	}
	pid := ts.RunningPID
	if proc.Alive(pid) {
		log.Printf("bigband: task %q has orphaned process %d — holding lock until it exits", name, pid)
		release, acquired := state.Lock(name)
		if !acquired {
			log.Printf("bigband: WARNING could not hold lock for orphan task %q", name)
			return
		}
		go func(name string, pid int, release func()) {
			defer release()
			waitPID(ctx, pid)
			if ctx.Err() != nil {
				return
			}
			log.Printf("bigband: orphaned process %d for task %q exited", pid, name)
			if err := st.SetDone(name, state.StatusStopped, 0, "", ""); err != nil {
				log.Printf("bigband: state update failed for %q: %v", name, err)
			}
		}(name, pid, release)
	} else {
		log.Printf("bigband: clearing stale running state for task %q (pid %d gone)", name, pid)
		if err := st.SetDone(name, state.StatusUnknown, 0, "", ""); err != nil {
			log.Printf("bigband: state update failed for %q: %v", name, err)
		}
	}
}

func waitPID(ctx context.Context, pid int) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !proc.Alive(pid) {
				return
			}
		}
	}
}

func writePID() error {
	return os.WriteFile(paths.PidFile(), []byte(strconv.Itoa(os.Getpid())), 0600)
}
