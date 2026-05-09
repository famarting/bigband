package bigbandext

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// DefaultDialTimeout is the default timeout for dialing the daemon socket.
// Subscribe streams hold connections open for arbitrarily long after the
// initial dial, but the *connect* must complete within this window.
const DefaultDialTimeout = 2 * time.Second

// Client speaks the bigband daemon's IPC protocol over its Unix socket.
// Construct with NewClient (or NewClientFromEnv to honour BIGBAND_HOME).
//
// Each method opens a fresh connection except Subscribe, which holds the
// connection open for the lifetime of the returned channel. Concurrent calls
// from multiple goroutines are safe.
type Client struct {
	socketPath  string
	dialTimeout time.Duration
}

// NewClient returns a Client that talks to the daemon at socketPath.
// dialTimeout controls connect time; pass 0 for the default.
func NewClient(socketPath string, dialTimeout time.Duration) *Client {
	if dialTimeout <= 0 {
		dialTimeout = DefaultDialTimeout
	}
	return &Client{socketPath: socketPath, dialTimeout: dialTimeout}
}

// NewClientFromEnv resolves the socket path the same way bigband itself does:
// $BIGBAND_HOME/daemon.sock, falling back to $HOME/.bigband-tasks/daemon.sock.
func NewClientFromEnv() (*Client, error) {
	root := os.Getenv("BIGBAND_HOME")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("home dir: %w", err)
		}
		root = filepath.Join(home, ".bigband-tasks")
	}
	return NewClient(filepath.Join(root, "daemon.sock"), 0), nil
}

// SocketPath returns the daemon socket path this client is configured for.
// Useful in error messages.
func (c *Client) SocketPath() string { return c.socketPath }

// --- Wire types (private) ---

// These shadow the daemon's internal/ipc types intentionally: the wire format
// is the public contract, but we don't import internal/ipc from here.
// SchemaVersion changes will surface as JSON unmarshal failures or unknown
// field handling — both detectable in tests.

type cmd struct {
	Action    string            `json:"action"`
	Task      string            `json:"task,omitempty"`
	Submit    *SubmitRunRequest `json:"submit,omitempty"`
	Subscribe *SubscribeRequest `json:"subscribe,omitempty"`
}

type reply struct {
	OK      bool            `json:"ok"`
	Error   string          `json:"error,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// SubmitRunRequest is the payload for Submit / Followup. See cmd/bigband for
// per-field semantics; the most common forms are:
//
//	// Fire-and-forget one-off:
//	{Folder: "/repo", Prompt: "...", Ephemeral: true}
//
//	// Follow-up resuming a previous Claude session:
//	{Folder: worktreePath, Worktree: ptrFalse, Prompt: "...",
//	 ParentSessionID: prevSessionID, Ephemeral: true}
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

// SubmitRunReply is the payload of a successful Submit reply.
type SubmitRunReply struct {
	RunID    string `json:"run_id"`
	TaskName string `json:"task_name"`
	LogPath  string `json:"log_path,omitempty"`
}

// SubscribeRequest customises a Subscribe call. Empty fields match everything.
// Since (RFC3339 timestamp or empty) opts into replay from events.jsonl
// before transitioning to live; "" disables replay.
type SubscribeRequest struct {
	Name  string   `json:"name,omitempty"`
	Types []string `json:"types,omitempty"`
	Tasks []string `json:"tasks,omitempty"`
	Since string   `json:"since,omitempty"`
}

// TaskStatus is one row of a Status reply.
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
	WorktreeMode string `json:"worktree_mode,omitempty"`
	Ephemeral    bool   `json:"ephemeral,omitempty"`
}

// StatusReply is the payload of a successful Status reply.
type StatusReply struct {
	Uptime string       `json:"uptime"`
	Tasks  []TaskStatus `json:"tasks"`
}

// --- Public methods ---

// Ping returns nil when the daemon is reachable.
func (c *Client) Ping() error {
	return c.do(cmd{Action: "ping"}, nil)
}

// Status returns the daemon's task list snapshot.
func (c *Client) Status() (*StatusReply, error) {
	var out StatusReply
	if err := c.do(cmd{Action: "status"}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Run triggers a configured task by name.
func (c *Client) Run(taskName string) error {
	return c.do(cmd{Action: "run", Task: taskName}, nil)
}

// Stop cancels an in-flight task.
func (c *Client) Stop(taskName string) error {
	return c.do(cmd{Action: "stop", Task: taskName}, nil)
}

// Forget drops an ephemeral task's state from the daemon's in-memory map.
// Refused for tasks with a live RunningPID. CLI `bigband prune` and
// `bigband rm` both use this.
func (c *Client) Forget(taskName string) error {
	return c.do(cmd{Action: "forget", Task: taskName}, nil)
}

// Submit fires a one-off run with an inline task definition. Returns the
// generated run_id and task name (auto-generated when req.Name is blank).
func (c *Client) Submit(req SubmitRunRequest) (*SubmitRunReply, error) {
	var out SubmitRunReply
	if err := c.do(cmd{Action: "submit", Submit: &req}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Followup is sugar for Submit with ParentSessionID set. The caller supplies
// folder (typically the previous run's worktree path) and prompt; the daemon
// resumes the named Claude session in that folder.
func (c *Client) Followup(parentSessionID, folder, prompt string) (*SubmitRunReply, error) {
	noWorktree := false
	return c.Submit(SubmitRunRequest{
		Folder:          folder,
		Prompt:          prompt,
		ParentSessionID: parentSessionID,
		Worktree:        &noWorktree,
		Ephemeral:       true,
	})
}

// Subscribe opens a long-lived stream of envelopes matching req. The returned
// channel is closed when ctx is cancelled, the daemon shuts down, or the
// connection drops. Errors during the stream are sent to the errs channel
// (also closed on termination).
//
// Replay: set req.Since to an RFC3339 timestamp to get past events from
// events.jsonl before transitioning to live. Delivery is at-least-once;
// dedup by Envelope.EventID if you can't tolerate duplicates.
func (c *Client) Subscribe(ctx context.Context, req SubscribeRequest) (<-chan Envelope, <-chan error) {
	envCh := make(chan Envelope, 32)
	errCh := make(chan error, 1)

	go func() {
		defer close(envCh)
		defer close(errCh)

		conn, err := net.DialTimeout("unix", c.socketPath, c.dialTimeout)
		if err != nil {
			errCh <- fmt.Errorf("dial %s: %w", c.socketPath, err)
			return
		}
		defer conn.Close()

		// Cancel via ctx by closing the conn; the bufio.ReadBytes will then
		// return an error and exit the loop.
		go func() {
			<-ctx.Done()
			_ = conn.Close()
		}()

		if err := json.NewEncoder(conn).Encode(cmd{Action: "subscribe", Subscribe: &req}); err != nil {
			errCh <- fmt.Errorf("send subscribe: %w", err)
			return
		}

		r := bufio.NewReader(conn)
		// First line is the OK reply.
		line, err := r.ReadBytes('\n')
		if err != nil {
			errCh <- fmt.Errorf("read ack: %w", err)
			return
		}
		var ack reply
		if err := json.Unmarshal(line, &ack); err != nil {
			errCh <- fmt.Errorf("decode ack: %w", err)
			return
		}
		if !ack.OK {
			errCh <- errors.New("subscribe rejected: " + ack.Error)
			return
		}

		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				if ctx.Err() != nil {
					return // graceful shutdown
				}
				errCh <- err
				return
			}
			var env Envelope
			if err := json.Unmarshal(line, &env); err != nil {
				continue // skip malformed line; daemon should never produce one
			}
			select {
			case envCh <- env:
			case <-ctx.Done():
				return
			}
		}
	}()

	return envCh, errCh
}

// do sends one request and decodes one reply. payload, when non-nil, receives
// reply.Payload unmarshaled into the given struct.
func (c *Client) do(req cmd, payload any) error {
	conn, err := net.DialTimeout("unix", c.socketPath, c.dialTimeout)
	if err != nil {
		return fmt.Errorf("dial %s: %w", c.socketPath, err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return fmt.Errorf("send: %w", err)
	}
	var rep reply
	if err := json.NewDecoder(conn).Decode(&rep); err != nil {
		return fmt.Errorf("read reply: %w", err)
	}
	if !rep.OK {
		return errors.New(rep.Error)
	}
	if payload != nil && len(rep.Payload) > 0 {
		if err := json.Unmarshal(rep.Payload, payload); err != nil {
			return fmt.Errorf("decode payload: %w", err)
		}
	}
	return nil
}
