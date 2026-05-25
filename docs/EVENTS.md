# Events reference (schema v3)

Every lifecycle event the bigband daemon emits is wrapped in the same envelope and written to `~/.bigband/events.jsonl` as well as fanned out to active `subscribe` IPC subscribers.

## Envelope

```json
{
  "schema_version": 3,
  "event_id": "8 bytes hex",
  "type": "<one of the types below>",
  "occurred_at": "RFC3339 UTC timestamp",
  "run_id": "<job_name>/<iso-timestamp>",
  "job_name": "<configured or oneoff-...>",
  "source": "scheduler | ipc | cli | daemon",
  "triggered_by": "<free-form label, e.g. slack:thread:1234>",
  "data": { /* type-specific payload */ }
}
```

Only `schema_version`, `event_id`, `type`, and `occurred_at` are guaranteed populated. All other fields use omitempty and may be absent for system-level events.

## Event types

| Type | Source struct (Go) | When |
|---|---|---|
| `job_run.started` | `bigbandext.JobRunStartedData` | After lock acquisition, before pre_exec |
| `job_run.worktree_ready` | `bigbandext.JobRunWorktreeReadyData` | After worktree creation/reuse succeeds |
| `claude.session_started` | `bigbandext.ClaudeSessionStartedData` | First time a Claude session id is captured for the run |
| `claude.turn_completed` | `bigbandext.ClaudeTurnCompletedData` | Each `claude` invocation returns (one per ScheduleWakeup loop iteration) |
| `claude.scheduled_wakeup` | `bigbandext.ClaudeWakeupData` | Claude requested a delayed resume; bigband is about to sleep |
| `job_run.completed` | `bigbandext.JobRunCompletedData` | End of `runner.Run`, after post_exec and worktree cleanup |
| `job_run.failed_pre_exec` | `bigbandext.JobRunPreFailedData` | Pre_exec command failed; run skips the claude block and goes to post_exec |
| `subscriber.lagged` | (no data) | A subscriber's buffer overflowed and missed events; resync from `events.jsonl` |
| `extension.started` | `bigbandext.ExtensionStartedData` | Supervisor spawned an extension's child process |
| `extension.exited` | `bigbandext.ExtensionExitedData` | An extension's child process exited (cleanly or otherwise) |
| `extension.failed` | `bigbandext.ExtensionFailedData` | Manifest invalid, or supervisor circuit-broke after consecutive failures |

Go structs are defined in [`pkg/bigbandext/payloads.go`](../pkg/bigbandext/payloads.go) and re-exported as type aliases by `internal/events`.

Adding a new type bumps `schema_version`. Removing or renaming requires a major version bump.

## Payloads

### `job_run.started`
```json
{
  "folder": "/path/to/run-dir",
  "schedule": "@daily",
  "one_off": false,
  "worktree": true,
  "resume": false,
  "resume_from": "",
  "ephemeral": false
}
```

### `job_run.worktree_ready`
```json
{
  "worktree_path": "/path/to/worktree",
  "run_dir": "/path/to/worktree/subdir"
}
```

### `claude.session_started`
```json
{ "session_id": "claude-session-abc-123" }
```

### `claude.turn_completed`
```json
{
  "subtype": "success",
  "num_turns": 12,
  "duration_ms": 184321,
  "cost_usd": 0.0421,
  "final_message": "I committed the change to ...",
  "session_id": "claude-session-abc-123"
}
```

### `claude.scheduled_wakeup`
```json
{ "delay_seconds": 1800, "prompt": "<<autonomous-loop-dynamic>>" }
```

### `job_run.completed`
```json
{
  "status": "ok",
  "final_message": "Done. PR is at https://...",
  "log_path": "/Users/me/.bigband/logs/morning-brief/2026-05-09T15-00-00Z.log",
  "reply_file": "/Users/me/.bigband/logs/morning-brief/2026-05-09T15-00-00Z.reply.txt",
  "session_id": "claude-session-abc-123",
  "folder": "/Users/me/work/projects/cloudgrid",
  "worktree_path": "/path/to/worktree",
  "duration_ms": 184321
}
```

`status` is one of `ok`, `failed`, `timeout`, `pre_failed`, `stopped`, `unknown`. `final_message` is empty when the run ended on a tool call (no closing assistant text). `folder` is the directory the run executed from before any worktree resolution (i.e. `job.Folder`); `worktree_path` is the live worktree if one was created and kept. Together they let an integration follow up on the run without re-querying state — see the slack reference integration in `cmd/bigband-slack/`.

### `job_run.failed_pre_exec`
```json
{ "command": "git pull", "error": "exit status 1" }
```

### `extension.started`
```json
{ "name": "bigband-slack", "pid": 12345, "command": ["bigband-slack", "daemon"] }
```

### `extension.exited`
```json
{
  "name": "bigband-slack",
  "pid": 12345,
  "exit_code": 0,
  "signal": "",
  "duration_ms": 8421,
  "will_restart": true
}
```

`signal` is non-empty when the process was killed by a signal (e.g. `terminated`). `will_restart` is true when the supervisor will respawn after backoff.

### `extension.failed`
```json
{ "name": "bigband-slack", "error": "circuit-broke after 5 consecutive failures" }
```

Emitted on supervisor circuit-break or invalid manifest. The extension stays `failed` until `bigband ext restart` or a manifest re-save.

## Stability

- The set of types listed here is **closed for v1**. New types in v2 will not silently appear in v1 streams.
- Field names within a payload are stable. New fields may be added with omitempty; consumers must ignore unknown fields.
- The wire format is always the envelope: do not consume from `data` without checking `type` first.
