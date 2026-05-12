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

// RunStatus is the outcome of a task run.
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

// TaskState holds persisted info about a task.
type TaskState struct {
	LastRun       *time.Time `json:"last_run,omitempty"`
	LastStatus    RunStatus  `json:"last_status,omitempty"`
	LastDuration  string     `json:"last_duration,omitempty"`
	LastLog       string     `json:"last_log,omitempty"`
	LastReplyFile string     `json:"last_reply_file,omitempty"`
	RunningPID    int        `json:"running_pid,omitempty"`
	WorktreePath  string     `json:"worktree_path,omitempty"`
	SessionID     string     `json:"session_id,omitempty"`
	// Folder is the directory the most recent run executed in (i.e. task.Folder
	// at runner start, before any worktree creation). Recorded so ephemeral
	// submissions — which never appear in config.yaml — remain followable.
	Folder string `json:"folder,omitempty"`
	// KeepWorktree mirrors the task's keep_worktree setting at run time.
	// Persisted so the retention prune can skip worktree-owning ephemerals that
	// an extension (e.g. bigband-workflows) still relies on. Configured tasks
	// are never pruned regardless, so this only affects submitted ephemerals.
	KeepWorktree bool `json:"keep_worktree,omitempty"`
}

// State is the full state file.
type State struct {
	mu    sync.Mutex
	path  string
	Tasks map[string]*TaskState `json:"tasks"`
}

// Load reads or creates the state file.
func Load() (*State, error) {
	s := &State{path: paths.StateFile(), Tasks: map[string]*TaskState{}}
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

func (s *State) get(name string) *TaskState {
	ts := s.Tasks[name]
	if ts == nil {
		ts = &TaskState{}
		s.Tasks[name] = ts
	}
	return ts
}

// SetRunning marks a task as running with the given PID and records the
// folder the run is executing from. folder is the original task.Folder before
// any worktree resolution; passing "" leaves the existing value unchanged.
func (s *State) SetRunning(name string, pid int, folder string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts := s.get(name)
	ts.RunningPID = pid
	if folder != "" {
		ts.Folder = folder
	}
	now := time.Now()
	ts.LastRun = &now
	ts.LastStatus = StatusRunning
	return s.save()
}

// SetDone records the final outcome of a run. replyFile is the path to the
// captured final assistant message (empty if none).
func (s *State) SetDone(name string, status RunStatus, dur time.Duration, logPath, replyFile string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts := s.get(name)
	ts.RunningPID = 0
	ts.LastStatus = status
	ts.LastDuration = dur.Round(time.Second).String()
	ts.LastLog = logPath
	ts.LastReplyFile = replyFile
	return s.save()
}

// SetSessionID records the Claude session ID produced by a run.
func (s *State) SetSessionID(name, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts := s.get(name)
	ts.SessionID = id
	return s.save()
}

// SetWorktreePath records the worktree path for a running task.
// Pass an empty string to clear it after cleanup.
func (s *State) SetWorktreePath(name, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts := s.get(name)
	ts.WorktreePath = path
	return s.save()
}

// SetWorktreeKept records the worktree path AND the keep_worktree flag in a
// single transaction. Used by the runner to mark a worktree as retained so the
// retention pruner won't delete it underneath a long-lived extension instance.
func (s *State) SetWorktreeKept(name, path string, keep bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts := s.get(name)
	ts.WorktreePath = path
	ts.KeepWorktree = keep
	return s.save()
}

// Get returns a copy of the task state.
func (s *State) Get(name string) TaskState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ts := s.Tasks[name]; ts != nil {
		return *ts
	}
	return TaskState{}
}

// RemoveTask deletes the named entry from state. No-op when absent.
func (s *State) RemoveTask(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.Tasks[name]; !ok {
		return nil
	}
	delete(s.Tasks, name)
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

// RemoveStaleEphemerals drops state entries for tasks not in the configured
// set whose LastRun is before cutoff and which are not currently running.
// Returns the removed entries (caller is expected to clean up logs/worktrees).
//
// Ephemeral here means "submitted via IPC, never written to config.yaml" —
// configured tasks (whose names are in `configured`) are never touched, even
// when their last run is ancient.
func (s *State) RemoveStaleEphemerals(configured map[string]bool, cutoff time.Time) []RemovedEphemeral {
	s.mu.Lock()
	defer s.mu.Unlock()
	var removed []RemovedEphemeral
	for name, ts := range s.Tasks {
		if configured[name] || ts == nil {
			continue
		}
		if ts.RunningPID != 0 {
			continue
		}
		// Worktrees retained at the owner's request (extensions like
		// bigband-workflows) must outlive their last run. Their owners are
		// responsible for explicit cleanup via `bigband rm` / Forget.
		if ts.KeepWorktree && ts.WorktreePath != "" {
			continue
		}
		if ts.LastRun == nil || ts.LastRun.Before(cutoff) {
			removed = append(removed, RemovedEphemeral{
				Name:         name,
				Folder:       ts.Folder,
				WorktreePath: ts.WorktreePath,
			})
			delete(s.Tasks, name)
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
