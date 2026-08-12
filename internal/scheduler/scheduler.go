// Package scheduler wraps robfig/cron and provides diff-based hot-reload.
package scheduler

import (
	"crypto/sha256"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/famarting/bigband/internal/config"
	"github.com/robfig/cron/v3"
)

type entry struct {
	id   cron.EntryID
	hash string
}

// Scheduler wraps cron.Cron and live-reloads jobs on config change.
type Scheduler struct {
	mu          sync.Mutex
	c           *cron.Cron
	entries     map[string]entry // job name → cron entry (scheduled jobs only)
	oneOffFired map[string]bool  // one-off jobs fired this session
	handler     func(*config.Config, *config.Job)
	hasFired    func(string) bool // returns true if job has a persisted run record
}

// New returns a running Scheduler. handler is called for each fired job.
// hasFired is called for one-off jobs to check whether they already ran in a
// previous session (prevents re-firing after a daemon restart).
//
// Schedules are interpreted in UTC, not in the machine's local time, so the
// same schedule string fires at the same instant on every machine. Display is
// unaffected: NextRuns formats in local time.
func New(handler func(*config.Config, *config.Job), hasFired func(string) bool) *Scheduler {
	s := &Scheduler{
		entries:     make(map[string]entry),
		oneOffFired: make(map[string]bool),
		handler:     handler,
		hasFired:    hasFired,
		c:           cron.New(cron.WithLocation(time.UTC)),
	}
	s.c.Start()
	return s
}

// Reload applies a new config, adding/removing/updating cron entries as needed.
func (s *Scheduler) Reload(cfg *config.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := make(map[string]bool)
	for _, job := range cfg.Jobs {
		if !job.IsEnabled() {
			continue
		}
		next[job.Name] = true

		// One-off jobs are fired immediately on first Reload, not via cron.
		if job.IsOneOff() {
			if !s.oneOffFired[job.Name] && !s.hasFired(job.Name) {
				s.oneOffFired[job.Name] = true
				c, j := cfg, job
				log.Printf("bigband: firing one-off job %q immediately", job.Name)
				s.handler(c, j)
			}
			continue
		}

		h := jobHash(job)
		if existing, ok := s.entries[job.Name]; ok && existing.hash == h {
			continue // unchanged
		}
		// Remove old entry if it exists.
		if existing, ok := s.entries[job.Name]; ok {
			s.c.Remove(existing.id)
		}
		j := job // capture
		c := cfg // capture
		id, err := s.c.AddFunc(job.CronExpr(), func() {
			log.Printf("bigband: running scheduled job %q", j.Name)
			s.handler(c, j)
		})
		if err != nil {
			log.Printf("bigband: scheduling job %q failed: %v", job.Name, err)
			continue
		}
		s.entries[job.Name] = entry{id: id, hash: h}
		log.Printf("bigband: scheduled job %q (%s)", job.Name, job.CronExpr())
	}

	// Remove scheduled jobs that are no longer in config or are disabled.
	for name, e := range s.entries {
		if !next[name] {
			s.c.Remove(e.id)
			delete(s.entries, name)
			log.Printf("bigband: removed job %q", name)
		}
	}
}

// NextRuns returns a map of job name → next scheduled time string.
// One-off jobs are not included; their status is derived from state.
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

func jobHash(j *config.Job) string {
	h := sha256.Sum256(fmt.Appendf(nil, "%s|%s|%v", j.Name, j.CronExpr(), j.IsEnabled()))
	return fmt.Sprintf("%x", h[:8])
}
