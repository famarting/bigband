// Package bigbandext is the public Go SDK for building integrations on top of
// the bigband daemon. It exposes the typed event envelope and payload schemas
// (the same wire format that `events.jsonl` and the IPC `subscribe` stream
// use), plus a Client that wraps the Unix-socket protocol with high-level
// methods (Submit, Followup, Subscribe, Status, ...).
//
// Stability: this package follows the SchemaVersion of the underlying event
// stream. New fields may be added to existing payload structs (omitempty);
// new event types bump SchemaVersion. Wire field names are stable.
//
// Versus internal/events: this is the public surface for *external*
// integrations. internal/events aliases these types for the daemon's own use,
// so there is exactly one source of truth.
package bigbandext

import (
	"encoding/json"
	"time"
)

// SchemaVersion is the current envelope schema version. New event types bump
// this; additive payload changes do not.
const SchemaVersion = 2

// Type is the closed enumeration of lifecycle event types. Authoritative list
// is in docs/EVENTS.md.
type Type string

const (
	TypeTaskRunStarted       Type = "task_run.started"
	TypeTaskRunWorktreeReady Type = "task_run.worktree_ready"
	TypeClaudeSessionStarted Type = "claude.session_started"
	TypeClaudeTurnCompleted  Type = "claude.turn_completed"
	TypeClaudeWakeup         Type = "claude.scheduled_wakeup"
	TypeTaskRunCompleted     Type = "task_run.completed"
	TypeTaskRunPreFailed     Type = "task_run.failed_pre_exec"
	TypeSubscriberLagged     Type = "subscriber.lagged"

	// Extension lifecycle events. The daemon emits these when supervising
	// processes declared by an extension manifest.
	TypeExtensionStarted Type = "extension.started"
	TypeExtensionExited  Type = "extension.exited"
	TypeExtensionFailed  Type = "extension.failed"

	// TypeConfigReloaded is emitted by the daemon every time
	// ~/.bigband-tasks/config.yaml is parsed successfully after an fsnotify
	// change. Extensions that maintain derived state (e.g. wake schedules,
	// routing tables) subscribe to it to reconcile without polling. Payload:
	// ConfigReloadedData.
	TypeConfigReloaded Type = "config.reloaded"
)

// Source labels who triggered the run, for traceability.
type Source string

const (
	SourceScheduler Source = "scheduler"
	SourceIPC       Source = "ipc"
	SourceCLI       Source = "cli"
	SourceDaemon    Source = "daemon"
)

// Envelope is the wire shape of every lifecycle event. SchemaVersion + Type
// define the public contract; consumers must ignore unknown fields and unknown
// types so additive schema changes don't break them.
type Envelope struct {
	SchemaVersion int             `json:"schema_version"`
	EventID       string          `json:"event_id"`
	Type          Type            `json:"type"`
	OccurredAt    time.Time       `json:"occurred_at"`
	RunID         string          `json:"run_id,omitempty"`
	TaskName      string          `json:"task_name,omitempty"`
	Source        Source          `json:"source,omitempty"`
	TriggeredBy   string          `json:"triggered_by,omitempty"`
	Data          json.RawMessage `json:"data,omitempty"`
}

// Publisher is the minimal interface for code that emits events. Useful for
// tests; production wires the daemon's *events.Bus.
type Publisher interface {
	Publish(env Envelope)
}

// NopPublisher swallows all events. Useful for tests and the `bigband run`
// daemon-less fallback.
type NopPublisher struct{}

// Publish satisfies Publisher.
func (NopPublisher) Publish(Envelope) {}

// Filter narrows a subscription. Empty fields match everything; "*" in Tasks
// is also "all".
type Filter struct {
	Types []Type   `json:"types,omitempty"`
	Tasks []string `json:"tasks,omitempty"`
}

// Match reports whether env passes the filter.
func (f Filter) Match(env Envelope) bool {
	if len(f.Types) > 0 {
		ok := false
		for _, t := range f.Types {
			if t == env.Type {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if len(f.Tasks) > 0 {
		ok := false
		for _, n := range f.Tasks {
			if n == "*" || n == env.TaskName {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// MustData marshals v to a json.RawMessage. Panics on error — only call with
// values guaranteed JSON-encodable (typed payload structs are safe).
func MustData(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic("bigbandext: cannot marshal data: " + err.Error())
	}
	return raw
}
