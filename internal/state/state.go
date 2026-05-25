package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/famarting/bigband/internal/paths"
)

// RunStatus is the outcome of a job run.
type RunStatus string

const (
	StatusOK        RunStatus = "ok"
	StatusFailed    RunStatus = "failed"
	StatusTimeout   RunStatus = "timeout"
	StatusPreFailed RunStatus = "pre_failed"
	StatusSkipped   RunStatus = "skipped"
	StatusRunning   RunStatus = "running"
	StatusStopped   RunStatus = "stopped" // cancelled by daemon shutdown
	StatusUnknown   RunStatus = "unknown" // orphan exited without tracking
)

// JobState holds persisted info about a job.
type JobState struct {
	LastRun       *time.Time `json:"last_run,omitempty"`
	LastStatus    RunStatus  `json:"last_status,omitempty"`
	LastDuration  string     `json:"last_duration,omitempty"`
	LastLog       string     `json:"last_log,omitempty"`
	LastReplyFile string     `json:"last_reply_file,omitempty"`
	RunningPID    int        `json:"running_pid,omitempty"`
	WorktreePath  string     `json:"worktree_path,omitempty"`
	SessionID     string     `json:"session_id,omitempty"`
	// Folder is the directory the most recent run executed in (i.e. job.Folder
	// at runner start, before any worktree creation). Recorded so ephemeral
	// submissions — which never appear in config.yaml — remain followable.
	Folder string `json:"folder,omitempty"`
	// KeepWorktree mirrors the job's keep_worktree setting at run time.
	// Persisted so the retention prune can skip worktree-owning ephemerals that
	// an extension still relies on. Configured jobs
	// are never pruned regardless, so this only affects submitted ephemerals.
	KeepWorktree bool `json:"keep_worktree,omitempty"`

	// Last-run input parameters — captured at run start so a job can be
	// re-fired (`bigband rerun`) using exactly what it was last given. For
	// configured jobs this duplicates config.yaml; for ephemeral submits this
	// is the only durable record of the prompt once logs are pruned.
	Prompt   string   `json:"prompt,omitempty"`
	PreExec  []string `json:"pre_exec,omitempty"`
	PostExec []string `json:"post_exec,omitempty"`
	Worktree *bool    `json:"worktree,omitempty"`
	Timeout  string   `json:"timeout,omitempty"` // e.g. "2h"; empty inherits defaults
	Model    string   `json:"model,omitempty"`
	Effort   string   `json:"effort,omitempty"`
	Agent    string   `json:"agent,omitempty"`
}

// JobParams is the subset of a Job's run-time parameters we persist into
// state so the run is reproducible. State avoids importing config so the
// runner translates from *config.Job into this neutral struct.
type JobParams struct {
	Prompt   string
	PreExec  []string
	PostExec []string
	Worktree *bool
	Timeout  string
	Model    string
	Effort   string
	Agent    string
}

// State is the full state file.
type State struct {
	mu   sync.Mutex
	path string
	Jobs map[string]*JobState `json:"jobs"`
}

// Load reads or creates the state file.
func Load() (*State, error) {
	s := &State{path: paths.StateFile(), Jobs: map[string]*JobState{}}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, s); err != nil {
		return s, nil // corrupt state: start fresh rather than crash
	}
	return s, nil
}

func (s *State) get(name string) *JobState {
	js := s.Jobs[name]
	if js == nil {
		js = &JobState{}
		s.Jobs[name] = js
	}
	return js
}

// SetRunning marks a job as running with the given PID and records the
// folder the run is executing from. folder is the original job.Folder before
// any worktree resolution; passing "" leaves the existing value unchanged.
func (s *State) SetRunning(name string, pid int, folder string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	js := s.get(name)
	js.RunningPID = pid
	if folder != "" {
		js.Folder = folder
	}
	now := time.Now()
	js.LastRun = &now
	js.LastStatus = StatusRunning
	return s.save()
}

// SetDone records the final outcome of a run. replyFile is the path to the
// captured final assistant message (empty if none).
func (s *State) SetDone(name string, status RunStatus, dur time.Duration, logPath, replyFile string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	js := s.get(name)
	js.RunningPID = 0
	js.LastStatus = status
	js.LastDuration = dur.Round(time.Second).String()
	js.LastLog = logPath
	js.LastReplyFile = replyFile
	return s.save()
}

// SetJobParams records the input parameters the run was started with. Called
// from the runner once per run so the state row always reflects the latest
// invocation — enabling `bigband rerun` and self-describing `bigband get` for
// ephemeral submits whose log may have been pruned.
//
// Slices are copied so later mutations on the caller's job don't leak into
// state.
func (s *State) SetJobParams(name string, p JobParams) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	js := s.get(name)
	js.Prompt = p.Prompt
	js.PreExec = append([]string(nil), p.PreExec...)
	js.PostExec = append([]string(nil), p.PostExec...)
	js.Worktree = p.Worktree
	js.Timeout = p.Timeout
	js.Model = p.Model
	js.Effort = p.Effort
	js.Agent = p.Agent
	return s.save()
}

// SetSessionID records the Claude session ID produced by a run.
func (s *State) SetSessionID(name, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	js := s.get(name)
	js.SessionID = id
	return s.save()
}

// SetWorktreePath records the worktree path for a running job.
// Pass an empty string to clear it after cleanup.
func (s *State) SetWorktreePath(name, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	js := s.get(name)
	js.WorktreePath = path
	return s.save()
}

// SetWorktreeKept records the worktree path AND the keep_worktree flag in a
// single transaction. Used by the runner to mark a worktree as retained so the
// retention pruner won't delete it underneath a long-lived extension instance.
func (s *State) SetWorktreeKept(name, path string, keep bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	js := s.get(name)
	js.WorktreePath = path
	js.KeepWorktree = keep
	return s.save()
}

// Get returns a copy of the job state.
func (s *State) Get(name string) JobState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if js := s.Jobs[name]; js != nil {
		return *js
	}
	return JobState{}
}

// RemoveJob deletes the named entry from state. No-op when absent.
func (s *State) RemoveJob(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.Jobs[name]; !ok {
		return nil
	}
	delete(s.Jobs, name)
	return s.save()
}

// RemovedEphemeral describes one ephemeral entry dropped by
// RemoveStaleEphemerals. Folder + WorktreePath are returned so the caller can
// clean up the on-disk worktree (which lives outside the state file).
type RemovedEphemeral struct {
	Name         string
	Folder       string
	WorktreePath string
}

// RemoveStaleEphemerals drops state entries for jobs not in the configured
// set whose LastRun is before cutoff and which are not currently running.
// Returns the removed entries (caller is expected to clean up logs/worktrees).
//
// Ephemeral here means "submitted via IPC, never written to config.yaml" —
// configured jobs (whose names are in `configured`) are never touched, even
// when their last run is ancient.
func (s *State) RemoveStaleEphemerals(configured map[string]bool, cutoff time.Time) []RemovedEphemeral {
	s.mu.Lock()
	defer s.mu.Unlock()
	var removed []RemovedEphemeral
	for name, js := range s.Jobs {
		if configured[name] || js == nil {
			continue
		}
		if js.RunningPID != 0 {
			continue
		}
		// Worktrees retained at the owner's request must outlive
		// their last run. Their owners are
		// responsible for explicit cleanup via `bigband rm` / Forget.
		if js.KeepWorktree && js.WorktreePath != "" {
			continue
		}
		if js.LastRun == nil || js.LastRun.Before(cutoff) {
			removed = append(removed, RemovedEphemeral{
				Name:         name,
				Folder:       js.Folder,
				WorktreePath: js.WorktreePath,
			})
			delete(s.Jobs, name)
		}
	}
	if len(removed) > 0 {
		_ = s.save()
	}
	return removed
}

func (s *State) save() error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	// Write to a sibling tmp file and rename so a crash mid-write can't leave
	// a half-written state.json behind.
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, "state-*.json.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, s.path)
}
