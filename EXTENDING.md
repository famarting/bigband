# Extending bigband

bigband core is a generic Claude Code session orchestrator. Anything beyond that — Slack mirroring, webhooks, reporting dashboards, custom UIs — lives in **separate processes** that talk to bigband through three documented contracts:

1. **IPC** — a Unix-socket JSON protocol for triggering and inspecting runs
2. **Events** — a JSONL append-only stream + live IPC subscribe channel for lifecycle notifications
3. **State / logs** — the on-disk layout of `~/.bigband-tasks/`

Together these form bigband's public surface. Everything an integration does goes through one of them. They are versioned and stable.

This document is the **reference**. For a step-by-step walkthrough, read [`docs/INTEGRATIONS.md`](docs/INTEGRATIONS.md) — it builds a working webhook integration in ~50 lines using the public Go SDK at [`pkg/bigbandext`](pkg/bigbandext).

The bundled `bigband-slack` binary (under `cmd/bigband-slack/`) is the production-grade reference.

---

## Quickstart: a 30-line subscriber

The shortest useful integration tails completed-run events and posts the final assistant message to a webhook:

```sh
bigband subscribe --types task_run.completed | while IFS= read -r env; do
  msg=$(echo "$env" | jq -r '.data.final_message')
  task=$(echo "$env" | jq -r '.task_name')
  curl -s -X POST https://example.com/webhook \
    -H 'content-type: application/json' \
    -d "$(jq -n --arg t "$task" --arg m "$msg" '{task:$t, message:$m}')"
done
```

That's the entire surface most integrations need.

For richer flows (responding back into a conversation, firing new runs, persisting state across daemon restarts) read on.

---

## Contract 1 — IPC

The daemon listens on `~/.bigband-tasks/daemon.sock` (Unix domain socket). The wire format is one JSON-encoded `Cmd` per connection, then one or more JSON-encoded `Reply` objects. Schema lives in [`internal/ipc/ipc.go`](internal/ipc/ipc.go).

### Action `submit` — fire a one-off run

Submit a task definition inline; the daemon runs it through the same pipeline as scheduled tasks. Set `ephemeral: true` so it never gets persisted into `config.yaml`.

```json
{
  "action": "submit",
  "submit": {
    "folder": "/Users/me/work/cloudgrid",
    "prompt": "summarise the diff on this branch",
    "ephemeral": true,
    "triggered_by": "slack:msg:CXXX/123.456"
  }
}
```

Reply:
```json
{ "ok": true, "payload": {"run_id": "oneoff-2026-05-09T15-00-00Z/...", "task_name": "oneoff-..."} }
```

To **resume** a previous Claude session with a new prompt — the path used for follow-up replies — set `parent_session_id`. Pass the same `folder` you originally ran in (or its worktree path with `worktree: false` if you want to land in the same workspace):

```json
{
  "action": "submit",
  "submit": {
    "folder": "/Users/me/.bigband-tasks/worktrees/cloudgrid-foo",
    "worktree": false,
    "prompt": "actually, switch the approach to use SSE",
    "parent_session_id": "claude-session-abc-123",
    "ephemeral": true
  }
}
```

CLI shortcuts: `bigband submit --folder X --prompt "..."` and `bigband followup <task> "..."`.

### Action `subscribe` — stream events

Open a long-lived connection. The daemon sends one OK reply, then NDJSON envelopes:

```json
{ "action": "subscribe", "subscribe": { "name": "my-extension", "types": ["task_run.completed"], "tasks": ["*"] } }
```

Empty `types` or `tasks` matches everything. `tasks: ["*"]` is also "all".

**Replay**: set `since` to an RFC3339 timestamp to get past events from `events.jsonl` before transitioning to live. Useful for surviving integration restarts without losing anything. Delivery is at-least-once; dedup by `event_id` if you can't tolerate duplicates.

The connection stays open until you close it or the daemon shuts down. Each subscriber gets a 1024-event in-memory buffer; if you fall behind, you'll get a synthetic `subscriber.lagged` event and you should resync from `~/.bigband-tasks/events.jsonl` (the durable copy).

### Action `subscribers` — introspect attached streams

```sh
bigband subscribers
```

Returns the list of currently-attached subscribers (name, connect time, filters, lag). First command to run when you're not sure whether an integration is actually receiving events.

### Other actions

| Action | Purpose |
|---|---|
| `ping` | health check |
| `status` | task list + next-run times (includes ephemeral one-offs from state) |
| `run` | trigger a configured task by name |
| `stop` | cancel an in-flight task |
| `forget` | drop a (non-running) ephemeral task's state from the daemon's in-memory map; used by `bigband rm` and `bigband prune` |
| `ext_list` | enumerate manifest-supervised extensions (status, pid, restarts, last exit) |
| `ext_start` | start a stopped extension (`extension` field names the target) |
| `ext_stop` | stop a running extension; it stays stopped until `ext_start` |
| `ext_restart` | bounce a running extension |

---

## Contract 2 — Events

Every lifecycle moment of a task run produces one envelope, written to `~/.bigband-tasks/events.jsonl` (one JSON object per line) and fanned out to every active `subscribe` connection. Schema:

```json
{
  "schema_version": 2,
  "event_id": "abc123def456",
  "type": "task_run.completed",
  "occurred_at": "2026-05-09T15:00:42.123Z",
  "run_id": "morning-brief/2026-05-09T15-00-00Z",
  "task_name": "morning-brief",
  "source": "scheduler",
  "triggered_by": "",
  "data": { ... }
}
```

`run_id` is `<task_name>/<iso-timestamp>` and is stable for the lifetime of one run — use it to correlate `task_run.started` with `task_run.completed`.

The closed v1 type list and per-type payload shapes are in [`docs/EVENTS.md`](docs/EVENTS.md). Adding new types bumps `schema_version`.

### Two ways to consume

- **Tail the file** — durable, replayable, debuggable. Use `bigband events -f` or `tail -F ~/.bigband-tasks/events.jsonl`.
- **Subscribe via IPC** — live (~ms latency), filtered server-side, no file polling. Use `bigband subscribe --types ...` or open a Unix socket directly.

The file is the **ground truth**. The subscribe stream is the same data delivered live. Pick whichever fits your integration's needs (most need both: subscribe for live, file for replay-on-restart).

---

## Contract 3 — State and logs

| Path | Shape | Notes |
|---|---|---|
| `~/.bigband-tasks/config.yaml` | YAML | Tasks/templates/defaults. Hot-reloaded on change. **Never write here from an extension** — submit ephemeral runs via IPC instead. |
| `~/.bigband-tasks/state.json` | JSON | Per-task: `last_run`, `last_status`, `last_duration`, `last_log`, `last_reply_file`, `worktree_path`, `session_id`, `folder`. |
| `~/.bigband-tasks/logs/<task>/<ts>.log` | text | Per-run log: pre_exec output, stream-json render, post_exec output. |
| `~/.bigband-tasks/logs/<task>/<ts>.reply.txt` | text | Claude's final assistant message for that run. Empty/missing if the run ended on a tool call. |
| `~/.bigband-tasks/events.jsonl` | NDJSON | Lifecycle events. Append-only. |
| `~/.bigband-tasks/extensions/<name>/` | any | Reserved for extension-private config and state. Bigband itself ignores it. |

Existing `post_exec` shell commands receive these env vars (cheap fallback when full event subscribing is overkill):

| Env | Meaning |
|---|---|
| `BIGBAND_STATUS` | one of `ok`, `failed`, `timeout`, `pre_failed`, `stopped`, `unknown` |
| `BIGBAND_LOG` | path to this run's log file |
| `BIGBAND_TASK` | task name |
| `BIGBAND_WORKTREE` | worktree path (empty when no worktree) |
| `BIGBAND_REPLY_FILE` | path to `.reply.txt` (empty when no final message) |
| `BIGBAND_SESSION_ID` | Claude session id captured during the run |

A complete Slack-out integration can be done with only `post_exec` and `BIGBAND_REPLY_FILE` — no event bus required. Use the events bus when you need cross-task routing or stateful behaviour like Slack thread continuity.

---

## Where extensions live

Each extension claims a directory under `~/.bigband-tasks/extensions/<name>/`:

- `manifest.yaml` — declares how the bigband daemon should spawn and supervise this extension. See [`docs/MANIFESTS.md`](docs/MANIFESTS.md).
- `config.yaml` (or any other filename) — the extension's own config, which it reads from its `working_dir` at startup.
- State files (mappings, caches, secrets) sit alongside.

**Extension config must never mix into bigband's `config.yaml`.** That keeps bigband core fully extension-agnostic and lets each extension version its own schema.

When a manifest is present, the daemon supervises the extension's process directly — there's no per-extension LaunchAgent. Drop a directory with a `manifest.yaml`, and `bigband ext list` reports it within ~300ms (fsnotify debounce).

The bundled `bigband-slack` follows this convention — see [`cmd/bigband-slack/README.md`](cmd/bigband-slack/README.md) for the full reference. The bash example at [`examples/extensions/notify-sh/`](examples/extensions/notify-sh/) shows the same pattern in ~80 lines of shell.

---

## Design principles

If you're building an extension, these principles will keep you on the path that bigband core supports:

1. **Opt-in by default.** Empty config = your extension does nothing. Never act on a task unless the user has explicitly listed it.
2. **Per-task configurability.** Match by exact name and/or simple glob. Allow opt-out flags. Provide CLI sugar (`enable`/`disable`) so users don't hand-edit YAML.
3. **Talk to bigband only via the three contracts.** Don't read `config.yaml` to discover tasks — call `status` over IPC. Don't tail logs to detect completion — subscribe to events.
4. **Stay a separate process.** A Slack outage shouldn't take the bigband daemon down. Crash and restart freely.
5. **Persist your own state.** Bigband stores nothing about your extension. Use `extensions/<name>/state.*` and reconstruct from `events.jsonl` if your state file is missing.
6. **Use replay across restarts.** Subscribe with `since=<RFC3339>` after your last-seen event so the daemon replays anything you missed before transitioning to live. Delivery is at-least-once; dedup by `event_id` if duplicates would matter. The bundled `bigband-slack` does exactly this — see `cmd/bigband-slack/start.go` for the pattern.

---

## What's not yet in v1

- **Per-tool-call progress events** — `claude.assistant_text` / `claude.tool_call` envelopes for live streaming. Deferred to v2.
- **Skills directory** — extensions cannot install Claude Code skills. (Claude Code already loads `.claude/skills/` from worktrees; bigband stays out of it.)
- **Capability/permission enforcement on extensions** — extensions are trusted local processes in v1. The `capabilities:` field in manifests, when added, will be advisory.

When any of these grows up, the existing contracts won't change — only new fields and types will be added, and `schema_version` bumped.
