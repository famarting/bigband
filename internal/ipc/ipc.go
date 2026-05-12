// Package ipc provides a simple JSON-over-Unix-socket protocol used between
// the bigband CLI and the daemon.
package ipc

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"time"

	"github.com/famarting/bigband/internal/paths"
)

// maxConcurrentConns caps the number of IPC connections handled at once. A
// local CLI should never need more than a handful; this bound prevents a
// runaway caller from pinning unlimited goroutines.
const maxConcurrentConns = 32

const dialTimeout = 2 * time.Second

// Cmd is the message sent from CLI to daemon.
type Cmd struct {
	Action string `json:"action"` // ping | status | run | stop | submit | subscribe | forget | ext_list | ext_start | ext_stop | ext_restart
	Task   string `json:"task,omitempty"`
	// Submit is populated for action=="submit" with the inline task definition
	// to run. Kept as a pointer so wire-compatibility with older callers is
	// preserved (the field is omitted when nil).
	Submit *SubmitRunRequest `json:"submit,omitempty"`
	// Subscribe is populated for action=="subscribe" with the inline filter.
	// nil means "subscribe to everything".
	Subscribe *SubscribeRequest `json:"subscribe,omitempty"`
	// Extension names the target extension for action=="ext_start"|"ext_stop"|"ext_restart".
	// Ignored for ext_list (which lists all).
	Extension string `json:"extension,omitempty"`
}

// SubmitRunRequest is the inline task definition for action=="submit". It
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
	ParentSessionID string   `json:"parent_session_id,omitempty"`
	Ephemeral       bool     `json:"ephemeral,omitempty"`
	TriggeredBy     string   `json:"triggered_by,omitempty"`
}

// SubmitRunReply is the payload for a successful submit reply.
type SubmitRunReply struct {
	RunID    string `json:"run_id"`
	TaskName string `json:"task_name"`
	LogPath  string `json:"log_path"` // path of the log file the runner will open for this run; deterministic from task name + run timestamp
}

// SubscriberInfo is one row of a "subscribers" reply. Mirrors
// events.SubscriberInfo to keep the wire shape stable independently of
// internal package layout.
type SubscriberInfo struct {
	Name        string    `json:"name"`
	Types       []string  `json:"types,omitempty"`
	Tasks       []string  `json:"tasks,omitempty"`
	ConnectedAt time.Time `json:"connected_at"`
	LagDropped  int       `json:"lag_dropped,omitempty"`
}

// SubscribersReply is the payload of a successful "subscribers" reply.
type SubscribersReply struct {
	Subscribers []SubscriberInfo `json:"subscribers"`
}

// ExtensionInfo is one row of the ext_list reply. Mirrors the supervisor's
// procState but on the wire — internal layout can change without affecting
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
	Tasks []string `json:"tasks,omitempty"`
	Since string   `json:"since,omitempty"`
}

// Reply is the daemon's response to a Cmd.
type Reply struct {
	OK      bool            `json:"ok"`
	Error   string          `json:"error,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// StatusPayload is the payload for an "ok" status reply.
type StatusPayload struct {
	Uptime string       `json:"uptime"`
	Tasks  []TaskStatus `json:"tasks"`
}

// TaskStatus is the per-task status summary.
type TaskStatus struct {
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
	// task runs in its folder directly, or one of "ephemeral", "fresh",
	// "reused" when it uses a worktree.
	WorktreeMode string `json:"worktree_mode,omitempty"`
	// Ephemeral is true when the task exists only in state.json — i.e. it was
	// fired via IPC submit and was never written to config.yaml.
	Ephemeral bool `json:"ephemeral,omitempty"`
	// SessionID is the Claude session id recorded by the most recent run.
	// Empty when the task has never run or didn't produce one.
	SessionID string `json:"session_id,omitempty"`
	// Prompt is the configured prompt body (populated for configured tasks
	// only — ephemerals don't persist their prompt). Used by extensions that
	// promote a configured task into a multi-step workflow.
	Prompt string `json:"prompt,omitempty"`
}

// Send opens a connection to the daemon and sends cmd, returning the reply.
func Send(cmd Cmd) (*Reply, error) {
	conn, err := net.DialTimeout("unix", paths.Socket(), dialTimeout)
	if err != nil {
		return nil, fmt.Errorf("cannot reach daemon (is it running?): %w", err)
	}
	defer conn.Close()

	if err := json.NewEncoder(conn).Encode(cmd); err != nil {
		return nil, fmt.Errorf("sending command: %w", err)
	}
	var reply Reply
	if err := json.NewDecoder(conn).Decode(&reply); err != nil {
		return nil, fmt.Errorf("reading reply: %w", err)
	}
	return &reply, nil
}

// Serve starts a listener on the daemon socket and calls handler for each
// connection. Returns a stop function. The handler receives conn already
// accepted; it is called in its own goroutine.
func Serve(handler func(net.Conn)) (stop func(), err error) {
	_ = paths.EnsureDirs()
	// Remove stale socket left from a prior unclean shutdown so Listen does
	// not fail with "address already in use".
	_ = os.Remove(paths.Socket())
	ln, err := net.Listen("unix", paths.Socket())
	if err != nil {
		return nil, fmt.Errorf("listening on socket: %w", err)
	}
	// Tighten perms to 0600 so even a misconfigured parent dir doesn't expose
	// the socket to other local users. The parent dir is already 0700.
	if err := os.Chmod(paths.Socket(), 0600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("chmod socket: %w", err)
	}
	sem := make(chan struct{}, maxConcurrentConns)
	done := make(chan struct{})
	go func() {
		defer ln.Close()
		for {
			conn, err := ln.Accept()
			if err != nil {
				select {
				case <-done:
					return
				default:
				}
				continue
			}
			select {
			case sem <- struct{}{}:
				go func() {
					defer func() { <-sem }()
					handler(conn)
				}()
			default:
				log.Printf("ipc: max concurrent connections (%d) reached; dropping connection", maxConcurrentConns)
				conn.Close()
			}
		}
	}()
	return func() { close(done); ln.Close() }, nil
}
