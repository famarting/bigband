// Package ipc provides a simple JSON-over-Unix-socket protocol used between
// the bigband CLI and the daemon.
//
// The wire types (Cmd, Reply, SubmitRunRequest, JobStatus, ...) are aliases
// for the canonical definitions in pkg/bigbandext. This package owns only the
// connection plumbing — Send for clients, Serve for the daemon — so the
// external SDK and the daemon agree on the wire shape by construction.
package ipc

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"time"

	"github.com/famarting/bigband/internal/paths"
	"github.com/famarting/bigband/pkg/bigbandext"
)

// maxConcurrentConns caps the number of IPC connections handled at once. A
// local CLI should never need more than a handful; this bound prevents a
// runaway caller from pinning unlimited goroutines.
const maxConcurrentConns = 32

const dialTimeout = 2 * time.Second

// Wire types are sourced from pkg/bigbandext so external integrations and
// the daemon share exactly one source of truth.
type (
	Cmd              = bigbandext.Cmd
	Reply            = bigbandext.Reply
	SubmitRunRequest = bigbandext.SubmitRunRequest
	SubmitRunReply   = bigbandext.SubmitRunReply
	SubscribeRequest = bigbandext.SubscribeRequest
	JobStatus        = bigbandext.JobStatus
	StatusPayload    = bigbandext.StatusReply
	SubscriberInfo   = bigbandext.SubscriberInfo
	SubscribersReply = bigbandext.SubscribersReply
	ExtensionInfo    = bigbandext.ExtensionInfo
	ExtListReply     = bigbandext.ExtListReply
)

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
