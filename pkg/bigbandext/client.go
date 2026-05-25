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
// $BIGBAND_HOME/daemon.sock, falling back to $HOME/.bigband/daemon.sock.
func NewClientFromEnv() (*Client, error) {
	root := os.Getenv("BIGBAND_HOME")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("home dir: %w", err)
		}
		root = filepath.Join(home, ".bigband")
	}
	return NewClient(filepath.Join(root, "daemon.sock"), 0), nil
}

// SocketPath returns the daemon socket path this client is configured for.
// Useful in error messages.
func (c *Client) SocketPath() string { return c.socketPath }

// --- Public methods ---

// Ping returns nil when the daemon is reachable.
func (c *Client) Ping() error {
	return c.do(Cmd{Action: "ping"}, nil)
}

// Status returns the daemon's job list snapshot.
func (c *Client) Status() (*StatusReply, error) {
	var out StatusReply
	if err := c.do(Cmd{Action: "status"}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Run triggers a configured job by name.
func (c *Client) Run(jobName string) error {
	return c.do(Cmd{Action: "run", Job: jobName}, nil)
}

// Stop cancels an in-flight job.
func (c *Client) Stop(jobName string) error {
	return c.do(Cmd{Action: "stop", Job: jobName}, nil)
}

// Forget drops an ephemeral job's state from the daemon's in-memory map.
// Refused for jobs with a live RunningPID. CLI `bigband prune` and
// `bigband rm` both use this.
func (c *Client) Forget(jobName string) error {
	return c.do(Cmd{Action: "forget", Job: jobName}, nil)
}

// Submit fires a one-off run with an inline job definition. Returns the
// generated run_id and job name (auto-generated when req.Name is blank).
func (c *Client) Submit(req SubmitRunRequest) (*SubmitRunReply, error) {
	var out SubmitRunReply
	if err := c.do(Cmd{Action: "submit", Submit: &req}, &out); err != nil {
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

		if err := json.NewEncoder(conn).Encode(Cmd{Action: "subscribe", Subscribe: &req}); err != nil {
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
		var ack Reply
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
func (c *Client) do(req Cmd, payload any) error {
	conn, err := net.DialTimeout("unix", c.socketPath, c.dialTimeout)
	if err != nil {
		return fmt.Errorf("dial %s: %w", c.socketPath, err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return fmt.Errorf("send: %w", err)
	}
	var rep Reply
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
