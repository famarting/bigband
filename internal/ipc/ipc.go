// Package ipc provides a simple JSON-over-Unix-socket protocol used between
// the bigband CLI and the daemon.
package ipc

import (
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/famarting/bigband/internal/paths"
)

const dialTimeout = 2 * time.Second

// Cmd is the message sent from CLI to daemon.
type Cmd struct {
	Action string `json:"action"` // ping | status | run | tail
	Task   string `json:"task,omitempty"`
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
	// Remove stale socket.
	_ = paths.EnsureDirs()
	ln, err := net.Listen("unix", paths.Socket())
	if err != nil {
		return nil, fmt.Errorf("listening on socket: %w", err)
	}
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
			go handler(conn)
		}
	}()
	return func() { close(done); ln.Close() }, nil
}
