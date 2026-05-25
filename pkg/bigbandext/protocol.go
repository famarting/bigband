package bigbandext

import (
	"encoding/json"
	"time"
)

// This file holds the IPC wire types shared between the bigband daemon, the
// `bigband` CLI, and external SDK consumers. The wire format is the public
// contract; field names and JSON tags are stable across SchemaVersion.
//
// Internal package internal/ipc aliases these types so daemon-side code can
// keep typing `ipc.Cmd`, `ipc.JobStatus`, etc. while there is exactly one
// source of truth.

// Cmd is the message sent from a client to the daemon over the IPC socket.
//
// Action values: ping | status | run | stop | submit | subscribe | forget |
// ext_list | ext_start | ext_stop | ext_restart.
type Cmd struct {
	Action string `json:"action"`
	Job    string `json:"job,omitempty"`
	// Submit is populated for action=="submit" with the inline job definition
	// to run. Pointer so the field is omitted when nil.
	Submit *SubmitRunRequest `json:"submit,omitempty"`
	// Subscribe is populated for action=="subscribe" with the inline filter.
	// nil means "subscribe to everything".
	Subscribe *SubscribeRequest `json:"subscribe,omitempty"`
	// Extension names the target extension for action=="ext_start"|"ext_stop"|
	// "ext_restart". Ignored for ext_list (which lists all).
	Extension string `json:"extension,omitempty"`
}

// Reply is the daemon's response to a Cmd. Payload holds the typed reply
// body when OK is true; callers unmarshal it into the per-action struct.
type Reply struct {
	OK      bool            `json:"ok"`
	Error   string          `json:"error,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// SubmitRunRequest is the inline job definition for action=="submit". It
// describes a single one-off run that may either start fresh or resume a
// previous Claude session via ParentSessionID. Ephemeral submissions are not
// persisted to config.yaml; their state still lands in state.json so logs and
// follow-ups remain addressable.
//
// Folder and Prompt are required. When ParentSessionID is set, the run uses
// `claude --resume <id>` on its first turn. Folder is the directory the run
// executes in: callers that want to continue inside an existing worktree
// should pass the worktree (or its subdir) as Folder and set Worktree=false.
type SubmitRunRequest struct {
	Name            string   `json:"name,omitempty"`
	Folder          string   `json:"folder"`
	Prompt          string   `json:"prompt"`
	PreExec         []string `json:"pre_exec,omitempty"`
	PostExec        []string `json:"post_exec,omitempty"`
	Worktree        *bool    `json:"worktree,omitempty"`
	KeepWorktree    *bool    `json:"keep_worktree,omitempty"`
	Timeout         string   `json:"timeout,omitempty"`
	Model           string   `json:"model,omitempty"`
	Effort          string   `json:"effort,omitempty"`
	Agent           string   `json:"agent,omitempty"`
	ParentSessionID string   `json:"parent_session_id,omitempty"`
	Ephemeral       bool     `json:"ephemeral,omitempty"`
	TriggeredBy     string   `json:"triggered_by,omitempty"`
}

// SubmitRunReply is the payload of a successful "submit" reply. LogPath is
// deterministic from job name + run timestamp — the runner will open it
// shortly after the reply is sent.
type SubmitRunReply struct {
	RunID   string `json:"run_id"`
	JobName string `json:"job_name"`
	LogPath string `json:"log_path,omitempty"`
}

// SubscribeRequest is the inline filter for action=="subscribe". Empty fields
// match everything. The connection is held open by the daemon and streams one
// JSON envelope per line until the client disconnects or the daemon stops.
//
// Since opts the subscription into replay from events.jsonl: the daemon
// streams matching events with OccurredAt >= Since first, then transitions
// to live. Format is RFC3339; the empty string disables replay (live-only).
// Replay delivery is at-least-once — dedup by Envelope.EventID if needed.
type SubscribeRequest struct {
	Name  string   `json:"name,omitempty"`
	Types []string `json:"types,omitempty"`
	Jobs  []string `json:"jobs,omitempty"`
	Since string   `json:"since,omitempty"`
}

// JobStatus is one row of a "status" reply, summarising a single job's
// configuration and most recent run.
type JobStatus struct {
	Name         string `json:"name"`
	Schedule     string `json:"schedule"`
	Enabled      bool   `json:"enabled"`
	NextRun      string `json:"next_run"`
	LastRun      string `json:"last_run,omitempty"`
	LastStatus   string `json:"last_status,omitempty"`
	LastDuration string `json:"last_duration,omitempty"`
	LastLog      string `json:"last_log,omitempty"`
	WorktreePath string `json:"worktree_path,omitempty"`
	Folder       string `json:"folder,omitempty"`
	// WorktreeMode summarises the configured worktree behaviour: "" when the
	// job runs in its folder directly, or one of "ephemeral", "fresh",
	// "reused" when it uses a worktree.
	WorktreeMode string `json:"worktree_mode,omitempty"`
	// Ephemeral is true when the job exists only in state.json — i.e. it was
	// fired via IPC submit and was never written to config.yaml.
	Ephemeral bool `json:"ephemeral,omitempty"`
	// SessionID is the Claude session id recorded by the most recent run.
	// Empty when the job has never run or didn't produce one.
	SessionID string `json:"session_id,omitempty"`
	// Prompt is the configured prompt body (populated for configured jobs
	// only — ephemerals don't persist their prompt). Used by extensions that
	// promote a configured job into a multi-step workflow.
	Prompt string `json:"prompt,omitempty"`
}

// StatusReply is the payload of a successful "status" reply.
type StatusReply struct {
	Uptime string      `json:"uptime"`
	Jobs   []JobStatus `json:"jobs"`
}

// StatusPayload is a legacy alias for StatusReply, kept so internal callers
// that historically used ipc.StatusPayload remain unchanged.
type StatusPayload = StatusReply

// SubscriberInfo is one row of a "subscribers" reply.
type SubscriberInfo struct {
	Name        string    `json:"name"`
	Types       []string  `json:"types,omitempty"`
	Jobs        []string  `json:"jobs,omitempty"`
	ConnectedAt time.Time `json:"connected_at"`
	LagDropped  int       `json:"lag_dropped,omitempty"`
}

// SubscribersReply is the payload of a successful "subscribers" reply.
type SubscribersReply struct {
	Subscribers []SubscriberInfo `json:"subscribers"`
}

// ExtensionInfo is one row of the "ext_list" reply. Mirrors the supervisor's
// process state on the wire so internal layout can change without affecting
// external clients.
type ExtensionInfo struct {
	Name         string    `json:"name"`
	Status       string    `json:"status"` // running | stopped | failed | starting | crashed
	PID          int       `json:"pid,omitempty"`
	ManifestPath string    `json:"manifest_path,omitempty"`
	Restarts     int       `json:"restarts,omitempty"`
	StartedAt    time.Time `json:"started_at,omitempty"`
	LastExitCode int       `json:"last_exit_code,omitempty"`
	LastExitAt   time.Time `json:"last_exit_at,omitempty"`
	LastError    string    `json:"last_error,omitempty"`
	Enabled      bool      `json:"enabled"`
	Description  string    `json:"description,omitempty"`
	LogPath      string    `json:"log_path,omitempty"`
}

// ExtListReply is the payload of a successful "ext_list" reply.
type ExtListReply struct {
	Extensions []ExtensionInfo `json:"extensions"`
}
