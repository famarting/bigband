// Package daemon runs the bigband scheduling daemon.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/famarting/bigband/internal/config"
	"github.com/famarting/bigband/internal/ipc"
	"github.com/famarting/bigband/internal/paths"
	"github.com/famarting/bigband/internal/runner"
	"github.com/famarting/bigband/internal/scheduler"
	"github.com/famarting/bigband/internal/state"
)

// Run is the daemon entrypoint.
func Run() error {
	if err := paths.EnsureDirs(); err != nil {
		return fmt.Errorf("creating directories: %w", err)
	}

	if err := writePID(); err != nil {
		return err
	}
	defer os.Remove(paths.PidFile())

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

	cfg, err := config.Load(paths.Config())
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	reconcileOrphans(cfg, st)

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
		runner.Run(ctx, c, t, st, io.Discard)
	}

	// launchTask spawns runTask in a goroutine tracked by the WaitGroup so that
	// graceful shutdown can wait for in-flight tasks to finish. The scheduler and
	// IPC "run" handler both call this instead of go+runTask directly.
	launchTask := func(c *config.Config, t *config.Task) {
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
	sched.Reload(cfg)

	stopWatch, err := config.Watch(paths.Config(), func(newCfg *config.Config) {
		log.Println("config reloaded")
		cfg = newCfg
		sched.Reload(newCfg)
	})
	if err != nil {
		log.Printf("WARNING: cannot watch config: %v", err)
	} else {
		defer stopWatch()
	}

	stopIPC, err := ipc.Serve(func(conn net.Conn) {
		defer conn.Close()
		var cmd ipc.Cmd
		if err := json.NewDecoder(conn).Decode(&cmd); err != nil {
			return
		}
		reply := handleCmd(cmd, cfg, sched, st, started, launchTask, stopTask)
		json.NewEncoder(conn).Encode(reply) //nolint:errcheck
	})
	if err != nil {
		return fmt.Errorf("starting IPC: %w", err)
	}
	defer stopIPC()

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
	log.Printf("bigband daemon ready, %d scheduled, %d one-off, %d disabled", scheduled, oneOff, disabled)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
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

func handleCmd(cmd ipc.Cmd, cfg *config.Config, sched *scheduler.Scheduler, st *state.State, started time.Time, runTask func(*config.Config, *config.Task), stopTask func(string) bool) ipc.Reply {
	switch cmd.Action {
	case "ping":
		return ipc.Reply{OK: true}

	case "status":
		nextRuns := sched.NextRuns()
		var tasks []ipc.TaskStatus
		for _, t := range cfg.Tasks {
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
			})
		}
		payload := ipc.StatusPayload{
			Uptime: time.Since(started).Round(time.Second).String(),
			Tasks:  tasks,
		}
		raw, _ := json.Marshal(payload)
		return ipc.Reply{OK: true, Payload: raw}

	case "run":
		t := cfg.TaskByName(cmd.Task)
		if t == nil {
			return ipc.Reply{OK: false, Error: "unknown task: " + cmd.Task}
		}
		t.ClearJitter() // Don't apply jitter to manual runs.
		runTask(cfg, t)
		return ipc.Reply{OK: true}

	case "stop":
		t := cfg.TaskByName(cmd.Task)
		if t == nil {
			return ipc.Reply{OK: false, Error: "unknown task: " + cmd.Task}
		}
		if !stopTask(cmd.Task) {
			return ipc.Reply{OK: false, Error: "task " + cmd.Task + " is not running"}
		}
		return ipc.Reply{OK: true}

	default:
		return ipc.Reply{OK: false, Error: "unknown action: " + cmd.Action}
	}
}

func reconcileOrphans(cfg *config.Config, st *state.State) {
	for _, task := range cfg.Tasks {
		ts := st.Get(task.Name)
		if ts.RunningPID == 0 {
			continue
		}
		pid := ts.RunningPID
		if pidAlive(pid) {
			log.Printf("bigband: task %q has orphaned process %d — holding lock until it exits", task.Name, pid)
			release, acquired := state.Lock(task.Name)
			if !acquired {
				log.Printf("bigband: WARNING could not hold lock for orphan task %q", task.Name)
				continue
			}
			go func(name string, pid int, release func()) {
				defer release()
				waitPID(pid)
				log.Printf("bigband: orphaned process %d for task %q exited", pid, name)
				if err := st.SetDone(name, state.StatusUnknown, 0, ""); err != nil {
					log.Printf("bigband: state update failed for %q: %v", name, err)
				}
			}(task.Name, pid, release)
		} else {
			log.Printf("bigband: clearing stale running state for task %q (pid %d gone)", task.Name, pid)
			if err := st.SetDone(task.Name, state.StatusUnknown, 0, ""); err != nil {
				log.Printf("bigband: state update failed for %q: %v", task.Name, err)
			}
		}
	}
}

func pidAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

func waitPID(pid int) {
	for {
		time.Sleep(5 * time.Second)
		if !pidAlive(pid) {
			return
		}
	}
}

func writePID() error {
	return os.WriteFile(paths.PidFile(), []byte(strconv.Itoa(os.Getpid())), 0600)
}
