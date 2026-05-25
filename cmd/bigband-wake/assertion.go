package main

import (
	"log"
	"sync"
	"time"
)

// assertionMgr owns at most one IOPMAssertion at a time. The assertion is the
// programmatic equivalent of `caffeinate -i`: while held, macOS will not let
// the system go to idle-sleep, even on battery with the lid closed.
//
// Why a singleton: multiple overlapping assertions would still keep the
// system awake, but releasing them in the right order on shutdown gets
// fiddly. One assertion + an "extend until later" deadline gives the same
// effect with simpler bookkeeping.
//
// Lifecycle:
//
//	holdFor(d) → create assertion (if none) + arm a timer for d; the timer
//	             releases the assertion when it fires. A later holdFor with a
//	             larger deadline extends the timer; a smaller one is a no-op.
//	releaseNow() → forcibly release, stop the timer. Used on shutdown.
//
// Concurrency: holdFor and releaseNow are safe for concurrent callers; the
// internal timer callback also takes the lock.
type assertionMgr struct {
	mu     sync.Mutex
	id     uint32     // 0 means "nothing held"
	until  time.Time  // wall-clock deadline of the current assertion
	timer  *time.Timer
	reason string // reason text; only used for log lines
}

func newAssertionMgr(reason string) *assertionMgr {
	return &assertionMgr{reason: reason}
}

// holdFor ensures a sleep assertion is held for at least d into the future.
// Returns true if a new assertion was created or the existing one extended.
// Logs both create and extend so the operator can see what happened.
func (a *assertionMgr) holdFor(d time.Duration) bool {
	if d <= 0 {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	newUntil := time.Now().Add(d)

	if a.id != 0 {
		if !newUntil.After(a.until) {
			return false
		}
		// Extend: stop the old timer, re-arm with the new deadline. The
		// already-held assertion stays valid; no new IOPM call needed.
		if a.timer != nil {
			a.timer.Stop()
		}
		a.until = newUntil
		a.timer = time.AfterFunc(d, a.releaseFromTimer)
		log.Printf("bigband-wake: extended sleep assertion id=%d until=%s",
			a.id, a.until.Format(time.RFC3339))
		return true
	}

	id, err := createPMAssertion(a.reason)
	if err != nil {
		log.Printf("bigband-wake: failed to create sleep assertion: %v", err)
		return false
	}
	a.id = id
	a.until = newUntil
	a.timer = time.AfterFunc(d, a.releaseFromTimer)
	log.Printf("bigband-wake: created sleep assertion id=%d duration=%s until=%s",
		a.id, d, a.until.Format(time.RFC3339))
	return true
}

// releaseFromTimer is the timer-driven release path. Mirrors releaseNow but
// logs differently so the daemon log distinguishes natural-expiry from
// shutdown.
func (a *assertionMgr) releaseFromTimer() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.id == 0 {
		return
	}
	id := a.id
	if err := releasePMAssertion(id); err != nil {
		log.Printf("bigband-wake: failed to release sleep assertion id=%d: %v", id, err)
	} else {
		log.Printf("bigband-wake: released sleep assertion id=%d (expired)", id)
	}
	a.id = 0
	a.until = time.Time{}
	a.timer = nil
}

// releaseNow forcibly drops the assertion if one is held. Safe to call when
// nothing is held. Intended for clean shutdown.
func (a *assertionMgr) releaseNow() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.id == 0 {
		return
	}
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}
	id := a.id
	if err := releasePMAssertion(id); err != nil {
		log.Printf("bigband-wake: failed to release sleep assertion id=%d on shutdown: %v", id, err)
	} else {
		log.Printf("bigband-wake: released sleep assertion id=%d (shutdown)", id)
	}
	a.id = 0
	a.until = time.Time{}
}
