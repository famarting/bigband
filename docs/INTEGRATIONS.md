# Build your first bigband integration

This is a 30-minute walkthrough that takes you from "empty Go file" to a working bigband integration. The result will be a small webhook poster: every time a bigband job finishes, your program receives the event and POSTs Claude's final assistant message to an HTTP endpoint of your choice.

The tutorial uses the [`pkg/bigbandext`](../pkg/bigbandext) public Go SDK. The same shape works in any language — the `examples/extensions/echo-handler/` example is stdlib-only Go, and the protocol (one JSON object per line over a Unix socket) is trivial to reach from Python, Node, or shell.

If you only need bullet-point reference, read [`EXTENDING.md`](../EXTENDING.md) and [`docs/EVENTS.md`](EVENTS.md). This document is the narrative version.

---

## Prerequisites

- Bigband installed and running (`bigband daemon-logs -f` shows a heartbeat).
- A Go 1.26+ toolchain (matches bigband's go.mod).
- One scheduled or one-off bigband job you can trigger to generate events. If you don't have one, run any quick job (`bigband submit --folder . --prompt "say hi"`).

## 0. New module

```sh
mkdir ~/projects/bigband-webhook && cd $_
go mod init example.com/bigband-webhook
go get github.com/famarting/bigband/pkg/bigbandext@latest
```

If `pkg/bigbandext` isn't published to a registry yet, point your replace at the local repo:

```sh
go mod edit -replace github.com/famarting/bigband=/path/to/bigband
go mod tidy
```

## 1. Connect

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/famarting/bigband/pkg/bigbandext"
)

func main() {
	client, err := bigbandext.NewClientFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	if err := client.Ping(); err != nil {
		log.Fatal("daemon not reachable: ", err)
	}
	fmt.Println("connected to", client.SocketPath())
}
```

`NewClientFromEnv` honours `$BIGBAND_HOME`; otherwise it falls back to `~/.bigband`. Run it:

```sh
go run .
# connected to /Users/you/.bigband/daemon.sock
```

If `Ping` fails, the daemon isn't running. `bigband install` (or `bigband daemon` foreground) and try again.

## 2. Subscribe

Replace `main` with a subscribe loop:

```go
ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
defer cancel()

events, errs := client.Subscribe(ctx, bigbandext.SubscribeRequest{
	Name:  "bigband-webhook",
	Types: []string{string(bigbandext.TypeJobRunCompleted)},
})

for {
	select {
	case env, ok := <-events:
		if !ok {
			return
		}
		fmt.Printf("[%s] %s — run %s\n", env.Type, env.JobName, env.RunID)
	case err := <-errs:
		log.Println("subscribe error:", err)
		return
	case <-ctx.Done():
		return
	}
}
```

(You'll need `os/signal`, `os`, `syscall` imports.)

Trigger a bigband job in another terminal — `bigband run <some-job>` — and watch the line print. **The contract is real**: you're now consuming the same stream the bundled `bigband-slack` does.

Confirm the daemon sees you:

```sh
bigband subscribers
# NAME              CONNECTED FOR  TYPES                 JOBS  LAG DROPPED
# bigband-webhook   12s            job_run.completed    *      0
```

## 3. Decode the payload

`Envelope.Data` is `json.RawMessage`. Decode into the typed payload for the type you matched:

```go
var d bigbandext.JobRunCompletedData
if err := json.Unmarshal(env.Data, &d); err != nil {
	log.Println("bad envelope:", err)
	continue
}
fmt.Printf("  status=%s msg=%q duration=%dms\n", d.Status, truncate(d.FinalMessage, 80), d.DurationMS)
```

`docs/EVENTS.md` has the full payload reference for every event type.

## 4. POST to a webhook

Add an HTTP client. Wrap the inner block:

```go
type post struct {
	Job     string `json:"job"`
	Status  string `json:"status"`
	Message string `json:"message"`
	RunID   string `json:"run_id"`
}

if d.Status != "ok" {
	continue // skip failures, or include them — your call
}
body, _ := json.Marshal(post{
	Job:     env.JobName,
	Status:  d.Status,
	Message: d.FinalMessage,
	RunID:   env.RunID,
})
resp, err := http.Post(webhookURL, "application/json", bytes.NewReader(body))
if err != nil {
	log.Println("webhook error:", err)
	continue
}
resp.Body.Close()
```

That's a working integration in ~50 lines. Trigger another job and the webhook fires.

## 5. Survive restarts (replay)

If your program is down when an event fires, you miss it. Fix this in two lines:

```go
// Persist last-seen timestamp to ~/.bigband/extensions/bigband-webhook/last_seen
since := readLastSeen() // your impl; RFC3339 string from disk

events, errs := client.Subscribe(ctx, bigbandext.SubscribeRequest{
	Name:  "bigband-webhook",
	Types: []string{string(bigbandext.TypeJobRunCompleted)},
	Since: since,
})
```

When `Since` is set, the daemon replays matching events from `events.jsonl` before transitioning to live. Delivery is at-least-once; if you can't tolerate duplicates, dedup by `Envelope.EventID` in a small in-memory set (the bundled `bigband-slack` does exactly this — see [`cmd/bigband-slack/start.go`](../cmd/bigband-slack/start.go) for the pattern).

Persist `env.OccurredAt` after every successful POST and you'll never lose an event again.

## 6. Trigger bigband from your integration

Subscribing is half the story. The other half is firing jobs based on what your integration sees:

```go
// One-off run
reply, err := client.Submit(bigbandext.SubmitRunRequest{
	Folder:    "/Users/me/work/myrepo",
	Prompt:    "say hi",
	Ephemeral: true,
})
// → reply.RunID, reply.JobName

// Follow-up: resume a previous Claude session in its worktree
reply, err = client.Followup(prevSessionID, prevWorktreePath, "now do X")
```

`Followup` is sugar for `Submit` with `parent_session_id` set and `worktree: false` (so the runner doesn't try to create a fresh worktree on resume). The Slack integration uses this exact call to turn thread replies into resumed Claude sessions.

Other operations: `client.Run("job")`, `client.Stop("job")`, `client.Status()`, `client.Forget("oneoff-...")`.

## Where to live

By convention, integrations keep their config and state under `~/.bigband/extensions/<name>/`. Bigband core ignores it — the directory is yours.

```
~/.bigband/extensions/bigband-webhook/
├── config.yaml          # your config
├── last_seen            # for replay
└── ...
```

## Run it as a daemon (macOS)

When you're happy with the foreground version, install it as a LaunchAgent. Roll your own plist or copy the pattern from [`cmd/bigband-slack/service.go`](../cmd/bigband-slack/service.go) — `internal/launchd.Service` exposes `Install/Uninstall/Start/Stop` and accepts a custom label and env map.

## What to read next

- [`EXTENDING.md`](../EXTENDING.md) — the three contracts (IPC, events, state) in reference form.
- [`docs/EVENTS.md`](EVENTS.md) — every event type and its payload schema.
- [`cmd/bigband-slack/`](../cmd/bigband-slack/) — the reference integration. Same pattern as this tutorial, scaled up to a real product (config, store, hot reload, retention, status command, launchd).
- [`examples/extensions/echo-handler/`](../examples/extensions/echo-handler/) — stdlib-only Go subscriber, ~60 lines. No `pkg/bigbandext`, just raw protocol.

## User-shaped ecosystem

Bigband core has no idea your webhook integration exists. There is no plugin runtime, no manifest format, no permission model — just a documented IPC + events contract you're free to use however you want. Your integration crashes, the daemon doesn't notice. The daemon restarts, your `Since`-based replay catches you up. That's intentional minimalism, and it's why your integration can be ~50 lines.
