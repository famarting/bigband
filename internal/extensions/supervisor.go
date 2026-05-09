package extensions

import (
	"context"
	"errors"
	"fmt"
	"log"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/famarting/bigband/internal/events"
)

// Status enumerates the supervisor states a single extension can be in.
type Status string

const (
	StatusStopped  Status = "stopped"  // not running and not trying to start
	StatusStarting Status = "starting" // a spawn is in flight
	StatusRunning  Status = "running"  // child has been alive long enough to be considered healthy
	StatusBackoff  Status = "backoff"  // child exited; waiting before next attempt
	StatusFailed   Status = "failed"   // circuit-broken: too many consecutive failures
	StatusInvalid  Status = "invalid"  // manifest could not be loaded/validated
)

// healthyThreshold is how long a child must run before consecutive-failure
// count resets. Tunable; values in the high single-digit seconds work well in
// practice. Tests can shorten this via SetHealthyThreshold.
var healthyThreshold = 30 * time.Second

// SetHealthyThreshold overrides healthyThreshold. For tests only.
func SetHealthyThreshold(d time.Duration) { healthyThreshold = d }

// Supervisor runs and watches the set of extensions declared by manifests
// under extDir. One goroutine per extension drives its spawn → wait →
// restart loop. All public methods are safe for concurrent use.
type Supervisor struct {
	extDir string
	bus    events.Publisher
	logger *log.Logger

	mu     sync.Mutex
	states map[string]*procState
}

// procState is the supervisor's view of one extension. Mutated only with
// Supervisor.mu held.
type procState struct {
	name     string
	manifest *Manifest

	status   Status
	pid      int
	restarts int

	consecutiveFailures int

	startedAt    time.Time
	lastExitCode int
	lastExitAt   time.Time
	lastError    string

	// Lifecycle channels.
	stopReq chan struct{}      // closed to ask the supervise loop to exit
	cancel  context.CancelFunc // cancels the in-flight spawn (nil when not running)
	doneCh  chan struct{}      // closed when the supervise loop exits

	// User intent: when true, ignore the manifest's enabled flag and stay
	// stopped until ext_start clears it. Mirrors `bigband ext stop`.
	manuallyStopped bool
}

// NewSupervisor builds a Supervisor. Call Apply for each manifest discovered
// at startup, then continue calling Apply / Remove as the manifest dir
// changes. extDir is informational (used in log messages); discovery is
// driven by the caller.
func NewSupervisor(extDir string, bus events.Publisher, logger *log.Logger) *Supervisor {
	if logger == nil {
		logger = log.Default()
	}
	return &Supervisor{
		extDir: extDir,
		bus:    bus,
		logger: logger,
		states: map[string]*procState{},
	}
}

// Apply installs a manifest. If no extension by that name exists, a supervise
// goroutine is launched. If one exists and the manifest's spawn-relevant
// fields changed, the existing process is restarted. enabled flips are
// honoured even when no other field changed.
func (s *Supervisor) Apply(m *Manifest) {
	s.mu.Lock()
	st, exists := s.states[m.Name]
	if !exists {
		st = &procState{name: m.Name, status: StatusStopped}
		s.states[m.Name] = st
	}
	prev := st.manifest
	st.manifest = m
	st.lastError = ""
	wantRunning := m.IsEnabled() && !st.manuallyStopped
	needRestart := exists && prev != nil && !specEqual(prev, m)
	// "currently running" here means "the supervise loop is alive." A
	// stopped/failed/invalid extension has no loop. Without this we miss the
	// disabled→enabled transition: specEqual returns true (only `enabled:`
	// changed) so needRestart is false, yet the loop has already exited.
	currentlyAlive := st.status == StatusRunning || st.status == StatusStarting || st.status == StatusBackoff
	s.mu.Unlock()

	if !exists {
		s.startLoop(st)
		return
	}
	switch {
	case !wantRunning:
		s.requestStop(st, "disabled")
	case needRestart:
		s.requestStop(st, "manifest changed")
		s.startLoop(st)
	case wantRunning && !currentlyAlive:
		// Disabled → enabled, or transition out of failed.
		s.startLoop(st)
	}
}

// Remove tears down the supervisor for name. Used when a manifest is deleted.
// Returns once the supervise loop has exited (bounded by graceful kill in the
// runner — typically <5s).
func (s *Supervisor) Remove(name string) {
	s.mu.Lock()
	st, ok := s.states[name]
	if !ok {
		s.mu.Unlock()
		return
	}
	delete(s.states, name)
	s.mu.Unlock()
	s.requestStop(st, "manifest removed")
	<-st.doneCh
}

// Start, Stop, and Restart implement the operator IPC actions. Manual stop
// pins the extension stopped until Start is called or the manifest is
// re-saved (Apply clears manuallyStopped).
func (s *Supervisor) Start(name string) error {
	s.mu.Lock()
	st, ok := s.states[name]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("unknown extension: %s", name)
	}
	if st.manifest == nil {
		s.mu.Unlock()
		return fmt.Errorf("extension %s has no valid manifest", name)
	}
	st.manuallyStopped = false
	st.consecutiveFailures = 0
	st.lastError = ""
	already := st.doneCh != nil && st.status != StatusStopped && st.status != StatusFailed
	s.mu.Unlock()
	if already {
		return nil
	}
	s.startLoop(st)
	return nil
}

// Stop sets manual-stop and asks the loop to exit.
func (s *Supervisor) Stop(name string) error {
	s.mu.Lock()
	st, ok := s.states[name]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("unknown extension: %s", name)
	}
	st.manuallyStopped = true
	s.mu.Unlock()
	s.requestStop(st, "manual stop")
	return nil
}

// Restart bounces the extension. Equivalent to Stop followed by Start, but in
// one call so the IPC action is atomic from the caller's perspective.
func (s *Supervisor) Restart(name string) error {
	s.mu.Lock()
	st, ok := s.states[name]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("unknown extension: %s", name)
	}
	st.consecutiveFailures = 0
	st.lastError = ""
	s.mu.Unlock()
	s.requestStop(st, "manual restart")
	s.startLoop(st)
	return nil
}

// List returns a snapshot of every known extension. Order: alphabetical.
func (s *Supervisor) List() []ExtensionView {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ExtensionView, 0, len(s.states))
	names := make([]string, 0, len(s.states))
	for n := range s.states {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		st := s.states[name]
		ev := ExtensionView{
			Name:         st.name,
			Status:       string(st.status),
			PID:          st.pid,
			Restarts:     st.restarts,
			LastExitCode: st.lastExitCode,
			LastExitAt:   st.lastExitAt,
			LastError:    st.lastError,
			StartedAt:    st.startedAt,
		}
		if st.manifest != nil {
			ev.ManifestPath = st.manifest.Path()
			ev.Description = st.manifest.Description
			ev.Enabled = st.manifest.IsEnabled() && !st.manuallyStopped
			ev.LogPath = st.manifest.EffectiveLogPath()
		}
		out = append(out, ev)
	}
	return out
}

// Shutdown asks every supervise loop to exit and waits up to grace for them.
// After grace, returns even if some loops are still draining their child's
// SIGTERM → SIGKILL escalation.
func (s *Supervisor) Shutdown(grace time.Duration) {
	s.mu.Lock()
	loops := make([]*procState, 0, len(s.states))
	for _, st := range s.states {
		loops = append(loops, st)
	}
	s.mu.Unlock()

	for _, st := range loops {
		s.requestStop(st, "daemon shutdown")
	}
	deadline := time.NewTimer(grace)
	defer deadline.Stop()
	for _, st := range loops {
		select {
		case <-st.doneCh:
		case <-deadline.C:
			s.logger.Printf("bigband: extension %q did not stop within grace period", st.name)
			return
		}
	}
}

// MarkInvalid records a manifest parse error so List can show it. No
// supervise loop is started for an invalid entry.
func (s *Supervisor) MarkInvalid(name, manifestPath, errMsg string) {
	s.mu.Lock()
	st, ok := s.states[name]
	if !ok {
		st = &procState{name: name, status: StatusInvalid}
		s.states[name] = st
	} else {
		st.status = StatusInvalid
	}
	st.lastError = errMsg
	if st.manifest == nil {
		st.manifest = &Manifest{Name: name, path: manifestPath}
	}
	s.mu.Unlock()
	s.bus.Publish(events.Envelope{
		Type:   events.TypeExtensionFailed,
		Source: events.SourceDaemon,
		Data: events.MustData(events.ExtensionFailedData{
			Name:  name,
			Error: errMsg,
		}),
	})
}

// ExtensionView is the snapshot type returned by List. Mirrors ipc.ExtensionInfo
// but lives in the extensions package so internal callers don't depend on ipc.
type ExtensionView struct {
	Name         string
	Status       string
	PID          int
	ManifestPath string
	Restarts     int
	StartedAt    time.Time
	LastExitCode int
	LastExitAt   time.Time
	LastError    string
	Enabled      bool
	Description  string
	LogPath      string
}

// requestStop signals an in-flight supervise loop to exit gracefully. Idempotent.
func (s *Supervisor) requestStop(st *procState, reason string) {
	s.mu.Lock()
	if st.stopReq != nil {
		select {
		case <-st.stopReq:
			// already closed
		default:
			close(st.stopReq)
		}
	}
	if st.cancel != nil {
		s.logger.Printf("bigband: stopping extension %q (%s)", st.name, reason)
		st.cancel()
	}
	s.mu.Unlock()
}

// startLoop launches a supervise goroutine for st. Does nothing if a loop is
// already running for st.
func (s *Supervisor) startLoop(st *procState) {
	s.mu.Lock()
	// If the previous loop hasn't fully exited yet, wait for it before
	// starting a new one. Holding the supervisor lock during the wait is OK
	// because requestStop already triggered the cancel.
	prevDone := st.doneCh
	if prevDone != nil {
		s.mu.Unlock()
		<-prevDone
		s.mu.Lock()
	}
	st.stopReq = make(chan struct{})
	st.doneCh = make(chan struct{})
	st.status = StatusStarting
	st.consecutiveFailures = 0
	stopReq := st.stopReq
	doneCh := st.doneCh
	s.mu.Unlock()

	go s.run(st, stopReq, doneCh)
}

// run drives one extension's spawn → wait → backoff → respawn loop.
func (s *Supervisor) run(st *procState, stopReq <-chan struct{}, doneCh chan struct{}) {
	defer close(doneCh)

	for {
		select {
		case <-stopReq:
			s.transition(st, StatusStopped, 0, "")
			return
		default:
		}

		s.mu.Lock()
		m := st.manifest
		if m == nil || !m.IsEnabled() || st.manuallyStopped {
			st.status = StatusStopped
			st.pid = 0
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()

		env, unresolved := m.ResolvedEnv(nil)
		if len(unresolved) > 0 {
			s.logger.Printf("bigband: extension %q has unresolved env placeholders: %s",
				st.name, strings.Join(unresolved, ","))
		}

		ctx, cancel := context.WithCancel(context.Background())
		s.mu.Lock()
		st.cancel = cancel
		st.status = StatusStarting
		s.mu.Unlock()

		// Translate stopReq closure into ctx cancellation.
		go func() {
			select {
			case <-stopReq:
				cancel()
			case <-ctx.Done():
			}
		}()

		pidCh := make(chan int, 1)
		// Notify on PID arrival so the extension.started event carries it.
		go func() {
			pid, ok := <-pidCh
			if !ok {
				return
			}
			s.mu.Lock()
			st.pid = pid
			st.status = StatusRunning
			st.startedAt = time.Now().UTC()
			s.mu.Unlock()
			s.bus.Publish(events.Envelope{
				Type:   events.TypeExtensionStarted,
				Source: events.SourceDaemon,
				Data: events.MustData(events.ExtensionStartedData{
					Name:    m.Name,
					PID:     pid,
					Command: m.Command,
				}),
			})
		}()

		res := spawnAndWait(ctx, m, env, m.EffectiveLogPath(), pidCh)
		close(pidCh)
		cancel()

		s.mu.Lock()
		st.cancel = nil
		st.pid = 0
		st.lastExitCode = res.exitCode
		st.lastExitAt = res.exitedAt
		if res.err != nil {
			st.lastError = res.err.Error()
		}
		s.mu.Unlock()

		// Determine whether to restart.
		stopRequested := false
		select {
		case <-stopReq:
			stopRequested = true
		default:
		}

		if stopRequested {
			s.transition(st, StatusStopped, res.exitCode, "")
			s.publishExited(m, res, false)
			return
		}

		policy := m.EffectiveRestart()
		shouldRestart := policy.Policy != "never" && (policy.Policy == "always" || res.exitCode != 0)

		// If the child ran longer than healthyThreshold, treat it as a real
		// run and reset the consecutive-failures counter — only rapid crashes
		// trip the circuit breaker.
		s.mu.Lock()
		if res.duration() >= healthyThreshold {
			st.consecutiveFailures = 0
		} else if shouldRestart {
			st.consecutiveFailures++
		}
		failures := st.consecutiveFailures
		s.mu.Unlock()

		if shouldRestart && policy.MaxConsecutiveFailures > 0 && failures >= policy.MaxConsecutiveFailures {
			s.transition(st, StatusFailed, res.exitCode,
				fmt.Sprintf("circuit-broke after %d consecutive failures", failures))
			s.publishExited(m, res, false)
			return
		}

		s.publishExited(m, res, shouldRestart)
		if !shouldRestart {
			s.transition(st, StatusStopped, res.exitCode, "")
			return
		}

		// Backoff before the next attempt. Exponential: initial * 2^(failures-1) capped at max.
		wait := computeBackoff(policy, failures)
		s.mu.Lock()
		st.status = StatusBackoff
		st.restarts++
		s.mu.Unlock()

		s.logger.Printf("bigband: extension %q exited code=%d signal=%q; restarting in %s (failures=%d)",
			st.name, res.exitCode, res.signal, wait, failures)

		select {
		case <-time.After(wait):
		case <-stopReq:
			s.transition(st, StatusStopped, res.exitCode, "")
			return
		}
	}
}

// transition is a tiny helper for status + lastError updates under the lock.
func (s *Supervisor) transition(st *procState, status Status, exitCode int, msg string) {
	s.mu.Lock()
	st.status = status
	st.pid = 0
	if msg != "" {
		st.lastError = msg
	}
	if exitCode != 0 {
		st.lastExitCode = exitCode
	}
	s.mu.Unlock()
}

// publishExited emits the extension.exited envelope for a single child exit.
func (s *Supervisor) publishExited(m *Manifest, res runResult, willRestart bool) {
	s.bus.Publish(events.Envelope{
		Type:   events.TypeExtensionExited,
		Source: events.SourceDaemon,
		Data: events.MustData(events.ExtensionExitedData{
			Name:        m.Name,
			PID:         res.pid,
			ExitCode:    res.exitCode,
			Signal:      res.signal,
			DurationMS:  res.duration().Milliseconds(),
			WillRestart: willRestart,
		}),
	})
}

// computeBackoff returns the wait duration for the failures-th consecutive
// failure under policy. Failures is 1-based.
func computeBackoff(policy RestartPolicy, failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	wait := policy.InitialBackoff.Duration
	for i := 1; i < failures; i++ {
		wait *= 2
		if wait >= policy.MaxBackoff.Duration {
			return policy.MaxBackoff.Duration
		}
	}
	return wait
}

// specEqual reports whether the spawn-relevant fields of two manifests match.
// Used to skip needless restarts on cosmetic edits (description, subscribes).
func specEqual(a, b *Manifest) bool {
	if a == nil || b == nil {
		return a == b
	}
	if !reflect.DeepEqual(a.Command, b.Command) {
		return false
	}
	if a.WorkingDir != b.WorkingDir {
		return false
	}
	if a.LogPath != b.LogPath {
		return false
	}
	if !reflect.DeepEqual(a.Env, b.Env) {
		return false
	}
	if !reflect.DeepEqual(a.Restart, b.Restart) {
		return false
	}
	return true
}

// ErrUnknown is returned by IPC handlers for unknown extension names.
var ErrUnknown = errors.New("unknown extension")
