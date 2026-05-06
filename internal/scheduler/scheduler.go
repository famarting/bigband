// Package scheduler wraps robfig/cron and provides diff-based hot-reload.
package scheduler

import (
	"crypto/sha256"
	"fmt"
	"log"
	"sync"

	"github.com/famarting/bigband/internal/config"
	"github.com/robfig/cron/v3"
)

type entry struct {
	id   cron.EntryID
	hash string
}

// Scheduler wraps cron.Cron and live-reloads tasks on config change.
type Scheduler struct {
	mu          sync.Mutex
	c           *cron.Cron
	entries     map[string]entry // task name → cron entry (scheduled tasks only)
	oneOffFired map[string]bool  // one-off tasks fired this session
	handler     func(*config.Config, *config.Task)
	hasFired    func(string) bool // returns true if task has a persisted run record
}

// New returns a running Scheduler. handler is called for each fired task.
// hasFired is called for one-off tasks to check whether they already ran in a
// previous session (prevents re-firing after a daemon restart).
func New(handler func(*config.Config, *config.Task), hasFired func(string) bool) *Scheduler {
	s := &Scheduler{
		entries:     make(map[string]entry),
		oneOffFired: make(map[string]bool),
		handler:     handler,
		hasFired:    hasFired,
		c:           cron.New(),
	}
	s.c.Start()
	return s
}

// Reload applies a new config, adding/removing/updating cron entries as needed.
func (s *Scheduler) Reload(cfg *config.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := make(map[string]bool)
	for _, task := range cfg.Tasks {
		if !task.IsEnabled() {
			continue
		}
		next[task.Name] = true

		// One-off tasks are fired immediately on first Reload, not via cron.
		if task.IsOneOff() {
			if !s.oneOffFired[task.Name] && !s.hasFired(task.Name) {
				s.oneOffFired[task.Name] = true
				c, t := cfg, task
				go s.handler(c, t)
				log.Printf("bigband: firing one-off task %q immediately", task.Name)
			}
			continue
		}

		h := taskHash(task)
		if existing, ok := s.entries[task.Name]; ok && existing.hash == h {
			continue // unchanged
		}
		// Remove old entry if it exists.
		if existing, ok := s.entries[task.Name]; ok {
			s.c.Remove(existing.id)
		}
		t := task // capture
		c := cfg  // capture
		id, err := s.c.AddFunc(task.CronExpr(), func() {
			go s.handler(c, t)
		})
		if err != nil {
			log.Printf("bigband: scheduling task %q failed: %v", task.Name, err)
			continue
		}
		s.entries[task.Name] = entry{id: id, hash: h}
		log.Printf("bigband: scheduled task %q (%s)", task.Name, task.CronExpr())
	}

	// Remove scheduled tasks that are no longer in config or are disabled.
	for name, e := range s.entries {
		if !next[name] {
			s.c.Remove(e.id)
			delete(s.entries, name)
			log.Printf("bigband: removed task %q", name)
		}
	}
}

// NextRuns returns a map of task name → next scheduled time string.
// One-off tasks are not included; their status is derived from state.
func (s *Scheduler) NextRuns() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := make(map[string]string, len(s.entries))
	for name, e := range s.entries {
		ce := s.c.Entry(e.id)
		if !ce.Next.IsZero() {
			m[name] = ce.Next.Local().Format("2006-01-02 15:04:05")
		}
	}
	return m
}

// Stop halts the underlying cron runner.
func (s *Scheduler) Stop() { s.c.Stop() }

func taskHash(t *config.Task) string {
	h := sha256.Sum256(fmt.Appendf(nil, "%s|%s|%v", t.Name, t.CronExpr(), t.IsEnabled()))
	return fmt.Sprintf("%x", h[:8])
}
