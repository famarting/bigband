package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// State persists the set of pmset wake events this extension currently owns.
// We track them so reconcile can cancel only our own entries — never touching
// pmset events the user added by hand.
//
// On-disk layout (state.json):
//
//	{
//	  "events": [
//	    {"job": "my-buddy-kickoff", "wake_at": "2026-05-11T05:59:00+02:00",
//	     "fire_at":  "2026-05-11T06:00:00+02:00",
//	     "scheduled_at": "2026-05-11T03:00:00+02:00"}
//	  ]
//	}
//
// All times round-trip in their original location so pmset cancel-by-exact-time
// matches what we sent at scheduled-at time, even across DST transitions.
type State struct {
	Events []WakeEvent `json:"events"`
}

// WakeEvent is one pmset wake entry this extension owns.
type WakeEvent struct {
	Job         string    `json:"job"`
	WakeAt      time.Time `json:"wake_at"`
	FireAt      time.Time `json:"fire_at"`
	ScheduledAt time.Time `json:"scheduled_at"`
}

// LoadState reads state.json, returning an empty State if the file does not
// exist yet. A parse error is reported back so a corrupt file doesn't get
// silently overwritten — the operator can move it aside.
func LoadState() (*State, error) {
	path := StatePath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &State{}, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return &State{}, nil
	}
	s := &State{}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, err
	}
	return s, nil
}

// Save writes state.json atomically (write-then-rename) so a crash mid-write
// can never produce a half-parsed file.
func (s *State) Save() error {
	path := StatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Sorted returns the events ordered by WakeAt ascending so output and
// diffing are deterministic.
func (s *State) Sorted() []WakeEvent {
	out := make([]WakeEvent, len(s.Events))
	copy(out, s.Events)
	sort.Slice(out, func(i, j int) bool { return out[i].WakeAt.Before(out[j].WakeAt) })
	return out
}
