package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Store persists the bigband-slack ↔ Slack thread mapping. Plain JSON file
// for v1 — the data set is small (one entry per active thread). Atomic
// rename on save so we never leave a half-written file.
type Store struct {
	mu      sync.Mutex
	path    string
	Tasks   map[string]TaskMapping `json:"tasks"`
	Runs    map[string]RunMapping  `json:"runs"`
	Threads map[string]string      `json:"threads"` // thread_ts → task_name
}

// TaskMapping holds the most recent thread/session for a task name. Used to
// route Slack thread replies back to the same Claude session. LastSeenUnix
// is refreshed every time the entry is touched and drives retention pruning.
type TaskMapping struct {
	ThreadTS     string `json:"thread_ts"`
	Channel      string `json:"channel"`
	SessionID    string `json:"session_id,omitempty"`
	Folder       string `json:"folder,omitempty"`
	Worktree     string `json:"worktree,omitempty"`
	LastSeenUnix int64  `json:"last_seen_unix,omitempty"`
}

// RunMapping is keyed by run_id (task/timestamp). One row per individual run,
// so a follow-up's completion event can find the right thread. LastSeenUnix
// is set on creation and used for retention pruning.
type RunMapping struct {
	TaskName     string `json:"task_name"`
	ThreadTS     string `json:"thread_ts"`
	Channel      string `json:"channel"`
	LastSeenUnix int64  `json:"last_seen_unix,omitempty"`
}

// LoadStore reads (or creates) the state file.
func LoadStore() (*Store, error) {
	path := StatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	s := &Store{
		path:    path,
		Tasks:   map[string]TaskMapping{},
		Runs:    map[string]RunMapping{},
		Threads: map[string]string{},
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, s); err != nil {
		return s, nil // corrupt: start fresh
	}
	if s.Tasks == nil {
		s.Tasks = map[string]TaskMapping{}
	}
	if s.Runs == nil {
		s.Runs = map[string]RunMapping{}
	}
	if s.Threads == nil {
		s.Threads = map[string]string{}
	}
	return s, nil
}

// LinkRun records that a run posted into a thread. Updates both run-keyed
// and task-keyed maps so completion events for follow-up runs can find the
// thread, and so future thread replies can resume the right session.
func (s *Store) LinkRun(runID, taskName, channel, threadTS, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	s.Runs[runID] = RunMapping{TaskName: taskName, ThreadTS: threadTS, Channel: channel, LastSeenUnix: now}
	prev := s.Tasks[taskName]
	prev.ThreadTS = threadTS
	prev.Channel = channel
	if sessionID != "" {
		prev.SessionID = sessionID
	}
	prev.LastSeenUnix = now
	s.Tasks[taskName] = prev
	s.Threads[threadTS] = taskName
	return s.save()
}

// SetTaskSessionID records the latest Claude session id for a task. Used when
// the session-started event arrives independently of LinkRun (e.g. for runs
// that didn't originate from Slack).
func (s *Store) SetTaskSessionID(taskName, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.Tasks[taskName]
	prev.SessionID = sessionID
	prev.LastSeenUnix = time.Now().Unix()
	s.Tasks[taskName] = prev
	return s.save()
}

// LinkTaskMeta records the task's folder/worktree alongside the thread, so
// follow-ups know where to run.
func (s *Store) LinkTaskMeta(taskName, folder, worktree string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.Tasks[taskName]
	if folder != "" {
		prev.Folder = folder
	}
	if worktree != "" {
		prev.Worktree = worktree
	}
	prev.LastSeenUnix = time.Now().Unix()
	s.Tasks[taskName] = prev
	return s.save()
}

// Prune drops Runs and Tasks last touched before cutoff, plus Threads that
// no longer point at a known task. Returns counts for logging.
func (s *Store) Prune(cutoff time.Time) (runs, tasks, threads int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoffUnix := cutoff.Unix()
	for id, r := range s.Runs {
		if r.LastSeenUnix > 0 && r.LastSeenUnix < cutoffUnix {
			delete(s.Runs, id)
			runs++
		}
	}
	for name, t := range s.Tasks {
		if t.LastSeenUnix > 0 && t.LastSeenUnix < cutoffUnix {
			delete(s.Tasks, name)
			tasks++
		}
	}
	for ts, name := range s.Threads {
		if _, ok := s.Tasks[name]; !ok {
			delete(s.Threads, ts)
			threads++
		}
	}
	if runs+tasks+threads > 0 {
		_ = s.save()
	}
	return runs, tasks, threads
}

// LookupRun returns the thread mapping for a run id, or zero if absent.
func (s *Store) LookupRun(runID string) RunMapping {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Runs[runID]
}

// LookupThread returns the task whose latest run is in this thread.
func (s *Store) LookupThread(threadTS string) (string, TaskMapping, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name, ok := s.Threads[threadTS]
	if !ok {
		return "", TaskMapping{}, false
	}
	return name, s.Tasks[name], true
}

func (s *Store) save() error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), "state-*.json.tmp")
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
