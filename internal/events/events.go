// Package events is bigband's lifecycle event bus.
//
// It exposes a typed Envelope, a Bus with append-only JSONL persistence, and a
// fan-out Subscribe API. The runner publishes events as a job progresses; the
// daemon's IPC subscribe action streams them to long-running extensions; humans
// (and ad-hoc shell pipelines) can also tail the JSONL file directly.
//
// Schema: events are versioned via Envelope.SchemaVersion. Additions to the
// closed type list (see Type constants) bump the version. The on-disk JSONL is
// the durable contract — wire format and field names are stable.
package events

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/famarting/bigband/pkg/bigbandext"
)

// Public types and constants are sourced from pkg/bigbandext (the public Go
// SDK). Aliasing keeps internal callers unchanged while ensuring there is
// exactly one source of truth for the wire shape that external integrations
// also consume.

// SchemaVersion is the current envelope schema version.
const SchemaVersion = bigbandext.SchemaVersion

// Type aliases for the canonical pkg/bigbandext types.
type (
	Envelope           = bigbandext.Envelope
	Type               = bigbandext.Type
	Source             = bigbandext.Source
	Filter             = bigbandext.Filter
	Publisher          = bigbandext.Publisher
	NopPublisher       = bigbandext.NopPublisher
	ConfigReloadedData = bigbandext.ConfigReloadedData
)

// Re-export the closed Type constants.
const (
	TypeJobRunStarted        = bigbandext.TypeJobRunStarted
	TypeJobRunWorktreeReady  = bigbandext.TypeJobRunWorktreeReady
	TypeClaudeSessionStarted = bigbandext.TypeClaudeSessionStarted
	TypeClaudeTurnCompleted  = bigbandext.TypeClaudeTurnCompleted
	TypeClaudeWakeup         = bigbandext.TypeClaudeWakeup
	TypeJobRunCompleted      = bigbandext.TypeJobRunCompleted
	TypeJobRunPreFailed      = bigbandext.TypeJobRunPreFailed
	TypeSubscriberLagged     = bigbandext.TypeSubscriberLagged
	TypeExtensionStarted     = bigbandext.TypeExtensionStarted
	TypeExtensionExited      = bigbandext.TypeExtensionExited
	TypeExtensionFailed      = bigbandext.TypeExtensionFailed
	TypeConfigReloaded       = bigbandext.TypeConfigReloaded
)

// Re-export the Source constants.
const (
	SourceScheduler = bigbandext.SourceScheduler
	SourceIPC       = bigbandext.SourceIPC
	SourceCLI       = bigbandext.SourceCLI
	SourceDaemon    = bigbandext.SourceDaemon
)

// Bus is bigband's event bus. It appends every published envelope to a JSONL
// file (durable ground truth) and fans out to in-memory subscribers (for live
// streaming via IPC subscribe). Slow subscribers are dropped after their
// buffer fills, with a SubscriberLagged envelope sent so they can resync.
type Bus struct {
	mu          sync.Mutex
	file        *os.File
	subscribers []*subscription
}

type subscription struct {
	filter      Filter
	name        string
	ch          chan Envelope
	lagged      bool
	connectedAt time.Time
	lagDropped  int
}

// SubscriberInfo is a snapshot of one active subscription, exposed via
// Bus.Subscribers for introspection commands.
type SubscriberInfo struct {
	Name        string    `json:"name"`
	Types       []Type    `json:"types,omitempty"`
	Jobs        []string  `json:"jobs,omitempty"`
	ConnectedAt time.Time `json:"connected_at"`
	LagDropped  int       `json:"lag_dropped,omitempty"`
}

// Subscribers returns a snapshot of every currently-attached subscriber.
// Order is connection order (oldest first). Safe to call concurrently.
func (b *Bus) Subscribers() []SubscriberInfo {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]SubscriberInfo, 0, len(b.subscribers))
	for _, s := range b.subscribers {
		out = append(out, SubscriberInfo{
			Name:        s.name,
			Types:       s.filter.Types,
			Jobs:        s.filter.Jobs,
			ConnectedAt: s.connectedAt,
			LagDropped:  s.lagDropped,
		})
	}
	return out
}

const subscriberBuffer = 1024

// NewBus opens (creating if needed) the JSONL events file at path and returns
// a Bus. The caller is responsible for calling Close on shutdown.
func NewBus(path string) (*Bus, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	return &Bus{file: f}, nil
}

// Close releases the underlying file handle. Safe to call multiple times.
func (b *Bus) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.file == nil {
		return nil
	}
	err := b.file.Close()
	b.file = nil
	for _, s := range b.subscribers {
		close(s.ch)
	}
	b.subscribers = nil
	return err
}

// Publish appends env to the JSONL file and fans out to subscribers. Errors
// writing to the file are silently swallowed — events are best-effort and we
// never want to break the runner pipeline because logging hiccupped.
func (b *Bus) Publish(env Envelope) {
	if env.SchemaVersion == 0 {
		env.SchemaVersion = SchemaVersion
	}
	if env.EventID == "" {
		env.EventID = newEventID()
	}
	if env.OccurredAt.IsZero() {
		env.OccurredAt = time.Now().UTC()
	}
	line, err := json.Marshal(env)
	if err != nil {
		return
	}
	line = append(line, '\n')

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.file != nil {
		_, _ = b.file.Write(line)
	}
	for _, s := range b.subscribers {
		if !s.filter.Match(env) {
			continue
		}
		select {
		case s.ch <- env:
		default:
			// Subscriber is too slow — drop the event and mark lagged so we
			// emit a SubscriberLagged on the next opportunity.
			s.lagged = true
			s.lagDropped++
		}
	}
	// Best-effort lagged notification: re-fan over subscribers and try to
	// deliver a synthetic SubscriberLagged once each lagged subscriber's buffer
	// has space again. Cheap and bounded, runs only when something dropped.
	for _, s := range b.subscribers {
		if !s.lagged {
			continue
		}
		lag := Envelope{
			SchemaVersion: SchemaVersion,
			EventID:       newEventID(),
			Type:          TypeSubscriberLagged,
			OccurredAt:    time.Now().UTC(),
			Source:        SourceDaemon,
		}
		select {
		case s.ch <- lag:
			s.lagged = false
		default:
			// Still full — keep lagged set and try later.
		}
	}
}

// Subscribe registers a new subscriber and returns a receive-only channel of
// envelopes plus a cancel func. The channel is closed when cancel is called or
// the bus is Closed. Subscribers should drain promptly — buffer size is fixed.
func (b *Bus) Subscribe(filter Filter, name string) (<-chan Envelope, func()) {
	s := &subscription{
		filter:      filter,
		name:        name,
		ch:          make(chan Envelope, subscriberBuffer),
		connectedAt: time.Now().UTC(),
	}
	b.mu.Lock()
	b.subscribers = append(b.subscribers, s)
	b.mu.Unlock()

	cancel := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		for i, sub := range b.subscribers {
			if sub == s {
				b.subscribers = append(b.subscribers[:i], b.subscribers[i+1:]...)
				close(s.ch)
				return
			}
		}
	}
	return s.ch, cancel
}

// MustData re-exports bigbandext.MustData for ergonomic in-package use.
func MustData(v any) json.RawMessage { return bigbandext.MustData(v) }

// newEventID returns a short random identifier suitable for event correlation.
// 16 hex chars (~64 bits of entropy) is enough for human-scale event volumes.
func newEventID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// Fall back to a timestamp — not unique, but never panics.
		return time.Now().UTC().Format("20060102T150405.000000")
	}
	return hex.EncodeToString(buf[:])
}
