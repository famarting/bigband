package state

import (
	"encoding/json"
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
	StatusUnknown   RunStatus = "unknown" // orphan exited without tracking
)

// TaskState holds persisted info about a task.
type TaskState struct {
	LastRun      *time.Time `json:"last_run,omitempty"`
	LastStatus   RunStatus  `json:"last_status,omitempty"`
	LastDuration string     `json:"last_duration,omitempty"`
	LastLog      string     `json:"last_log,omitempty"`
	RunningPID   int        `json:"running_pid,omitempty"`
	WorktreePath string     `json:"worktree_path,omitempty"`
	SessionID    string     `json:"session_id,omitempty"`
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
	if os.IsNotExist(err) {
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

// SetRunning marks a task as running with the given PID.
func (s *State) SetRunning(name string, pid int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts := s.get(name)
	ts.RunningPID = pid
	now := time.Now()
	ts.LastRun = &now
	ts.LastStatus = StatusRunning
	return s.save()
}

// SetDone records the final outcome of a run.
func (s *State) SetDone(name string, status RunStatus, dur time.Duration, logPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts := s.get(name)
	ts.RunningPID = 0
	ts.LastStatus = status
	ts.LastDuration = dur.Round(time.Second).String()
	ts.LastLog = logPath
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

// Get returns a copy of the task state.
func (s *State) Get(name string) TaskState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ts := s.Tasks[name]; ts != nil {
		return *ts
	}
	return TaskState{}
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
