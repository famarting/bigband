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
	Tasks   map[string]TaskMapping   `json:"tasks"`
	Runs    map[string]RunMapping    `json:"runs"`
	Threads map[string]ThreadSnapshot `json:"threads"` // thread_ts → per-thread snapshot
}

// TaskMapping is a per-task staging area for pre-completion events
// (session_started, worktree_ready) and tracks the latest thread TS for
// routing completion events back to the right Slack thread.
type TaskMapping struct {
	ThreadTS     string `json:"thread_ts"`
	Channel      string `json:"channel"`
	SessionID    string `json:"session_id,omitempty"`
	Folder       string `json:"folder,omitempty"`
	Worktree     string `json:"worktree,omitempty"`
	LastSeenUnix int64  `json:"last_seen_unix,omitempty"`
}

// ThreadSnapshot captures the exact session and folder context at the moment
// a run posts its completion into a Slack thread. Keyed by thread_ts in the
// store so each thread carries independent state: a later run of the same
// task does not overwrite the session of an earlier thread.
type ThreadSnapshot struct {
	TaskName     string `json:"task_name"`
	SessionID    string `json:"session_id,omitempty"`
	Folder       string `json:"folder,omitempty"`
	Worktree     string `json:"worktree,omitempty"`
	Channel      string `json:"channel,omitempty"`
	AllowReplies bool   `json:"allow_replies,omitempty"`
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
		Threads: map[string]ThreadSnapshot{},
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
		s.Threads = map[string]ThreadSnapshot{}
	}
	return s, nil
}

// LinkRun records that a run posted into a thread. It:
//   - writes a run-keyed mapping so completion events for follow-up runs find the thread,
//   - updates the task-keyed staging entry for routing future completion events,
//   - writes a per-thread snapshot that freezes the session/folder/allowReplies at this
//     moment so replies to this thread always resume the correct session even after the
//     same task has run again and overwritten the task-level staging entry.
func (s *Store) LinkRun(runID, taskName, channel, threadTS, sessionID string, allowReplies bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()

	s.Runs[runID] = RunMapping{TaskName: taskName, ThreadTS: threadTS, Channel: channel, LastSeenUnix: now}

	// Update task-level staging (used to route completion events back to threads
	// and as a source for folder/worktree when promoting to per-thread snapshot).
	staged := s.Tasks[taskName]
	staged.ThreadTS = threadTS
	staged.Channel = channel
	if sessionID != "" {
		staged.SessionID = sessionID
	}
	staged.LastSeenUnix = now
	s.Tasks[taskName] = staged

	// Promote staged state to a per-thread snapshot. Existing snapshot fields
	// (e.g. folder set by an earlier LinkTaskMeta) are preserved if the new
	// values are empty, so partial updates don't wipe good data.
	snap := s.Threads[threadTS]
	snap.TaskName = taskName
	snap.Channel = channel
	snap.AllowReplies = allowReplies
	if sessionID != "" {
		snap.SessionID = sessionID
	} else if snap.SessionID == "" {
		snap.SessionID = staged.SessionID
	}
	if staged.Folder != "" {
		snap.Folder = staged.Folder
	}
	if staged.Worktree != "" {
		snap.Worktree = staged.Worktree
	}
	snap.LastSeenUnix = now
	s.Threads[threadTS] = snap

	return s.save()
}

// SetTaskSessionID records the latest Claude session id for a task. Used when
// the session-started event arrives independently of LinkRun (e.g. for runs
// that didn't originate from Slack). Also propagates the session id to the
// current thread snapshot so replies resume the right session mid-run.
func (s *Store) SetTaskSessionID(taskName, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()

	staged := s.Tasks[taskName]
	staged.SessionID = sessionID
	staged.LastSeenUnix = now
	s.Tasks[taskName] = staged

	// Propagate to the current thread snapshot so a reply that arrives before
	// the run completes still picks up the live session id.
	if staged.ThreadTS != "" {
		snap := s.Threads[staged.ThreadTS]
		snap.SessionID = sessionID
		snap.LastSeenUnix = now
		s.Threads[staged.ThreadTS] = snap
	}

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
	for ts, snap := range s.Threads {
		if snap.LastSeenUnix > 0 && snap.LastSeenUnix < cutoffUnix {
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

// LookupThread returns the per-thread snapshot for the given thread TS.
// The snapshot is independent of the task's current state, so replies to an
// older thread always resume the session that was active for that specific run.
func (s *Store) LookupThread(threadTS string) (taskName string, snap ThreadSnapshot, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, ok = s.Threads[threadTS]
	if !ok || snap.TaskName == "" {
		return "", ThreadSnapshot{}, false
	}
	return snap.TaskName, snap, true
}

const maxStoreBytes = 10 * 1024 * 1024 // 10 MiB

// pruneOldest drops entries older than cutoff. Must be called with s.mu held.
func (s *Store) pruneOldest(cutoff time.Time) {
	cutoffUnix := cutoff.Unix()
	for id, r := range s.Runs {
		if r.LastSeenUnix > 0 && r.LastSeenUnix < cutoffUnix {
			delete(s.Runs, id)
		}
	}
	for name, t := range s.Tasks {
		if t.LastSeenUnix > 0 && t.LastSeenUnix < cutoffUnix {
			delete(s.Tasks, name)
		}
	}
	for ts, snap := range s.Threads {
		if snap.LastSeenUnix > 0 && snap.LastSeenUnix < cutoffUnix {
			delete(s.Threads, ts)
		}
	}
}

func (s *Store) save() error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	// Guard against unbounded growth: prune entries older than 30 days and
	// re-marshal when the serialized size exceeds 10 MiB.
	if len(data) > maxStoreBytes {
		s.pruneOldest(time.Now().Add(-30 * 24 * time.Hour))
		if data, err = json.MarshalIndent(s, "", "  "); err != nil {
			return err
		}
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
