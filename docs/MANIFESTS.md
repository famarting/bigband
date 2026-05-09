# Extension manifests

A **manifest** is bigband's contract for "spawn me and route my lifecycle." It lives at:

```
~/.bigband-tasks/extensions/<name>/manifest.yaml
```

When the bigband daemon starts (or a manifest appears via fsnotify), it reads each manifest and runs the declared command as a long-lived child process. Crashes are restarted with exponential backoff. The user only ever installs **one** LaunchAgent — the bigband daemon itself.

The manifest is deliberately *generic*: it has no Slack-shaped or notify-shaped fields. Each extension keeps its own config under `extensions/<name>/` (any filename, any format) and reads it from its working_dir at startup.

## Schema

```yaml
# Required
name: bigband-slack                    # must equal the parent directory name; [a-z][a-z0-9-]*
command: [bigband-slack, daemon]       # argv; first element is binary (PATH-resolved or absolute)

# Optional
enabled: true                          # default true; set false to opt out without removing files
description: "Slack mirror integration"

working_dir: ""                        # default: extensions/<name>/
log_path: ""                           # default: <working_dir>/daemon.log

env:
  PATH: "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"
  SLACK_APP_TOKEN: "${env:SLACK_APP_TOKEN}"   # ${env:NAME} reads NAME from the daemon's env at spawn
  SLACK_BOT_TOKEN: "${env:SLACK_BOT_TOKEN}"

restart:
  policy: on_failure                   # always | on_failure | never  (default: on_failure)
  initial_backoff: 1s
  max_backoff: 30s
  max_consecutive_failures: 5          # circuit-break after N rapid failures (0 = unlimited)

subscribes: [task_run.completed]       # advisory in v1; reserved for future capability gating
```

### Field-by-field

- **`name`** — must match the *parent directory name* and `[a-z][a-z0-9-]*`. The daemon refuses to start a manifest where these don't agree.
- **`command`** — argv. The first element is resolved against the manifest's effective `env.PATH` (or you can pass an absolute path).
- **`enabled`** — when `false`, the daemon stops the process if running and never spawns it. Toggle by editing the file directly or via `bigband ext enable/disable`.
- **`working_dir`** — the cwd of the spawned process. Defaults to the manifest's directory, which is also where the extension's own config typically lives.
- **`log_path`** — appended to (not truncated) on each spawn. Each spawn boundary writes a one-line marker so `bigband ext logs` is readable.
- **`env`** — environment variables exported to the child. `${env:NAME}` placeholders interpolate from the daemon's own env at spawn time. Unset names resolve to empty and are logged once per spawn. `${other:thing}` patterns pass through untouched.
- **`restart.policy`** — `always` (restart on any exit), `on_failure` (default; restart only on non-zero exit), `never`.
- **`restart.initial_backoff` / `max_backoff`** — exponential backoff: `initial * 2^(failures-1)`, clamped to `max`.
- **`restart.max_consecutive_failures`** — the circuit breaker. After this many *rapid* failures (no run lived longer than 30s in between), the supervisor gives up and marks the extension `failed`. Use `bigband ext restart` to re-arm.
- **`subscribes`** — purely advisory in v1. Reviewers can see at a glance what events the extension consumes; the daemon does not enforce filtering.

Unknown top-level fields are rejected at parse time so typos surface immediately.

## Lifecycle

The daemon publishes three event types as it supervises children. They flow through the same event bus and JSONL file as task events:

| Type | Payload (selected fields) |
|---|---|
| `extension.started` | `name`, `pid`, `command` |
| `extension.exited`  | `name`, `pid`, `exit_code`, `signal`, `duration_ms`, `will_restart` |
| `extension.failed`  | `name`, `error` (manifest invalid or circuit-broken) |

Subscribe just like any other event:

```sh
bigband subscribe --types extension.started,extension.exited,extension.failed
```

## CLI

```
bigband ext list                # NAME / STATUS / PID / RESTARTS / LAST EXIT / MANIFEST
bigband ext start <name>        # start a stopped extension
bigband ext stop <name>         # stop a running extension (stays stopped until ext start)
bigband ext restart <name>      # bounce
bigband ext logs <name> [-f]    # tail stdout/stderr of the supervised process
bigband ext validate <path>     # lint a manifest without contacting the daemon
bigband ext enable <name>       # set enabled: true in the manifest
bigband ext disable <name>      # set enabled: false in the manifest
```

`bigband ext stop` pins the extension stopped until `bigband ext start` is called. `bigband ext disable` flips the manifest's `enabled` field instead, which survives daemon restarts.

## Examples

### Slack sidecar

```yaml
name: bigband-slack
command: [bigband-slack, daemon]
description: "Slack mirror — opt-in per task in extensions/bigband-slack/config.yaml"
env:
  PATH: "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"
  SLACK_APP_TOKEN: "${env:SLACK_APP_TOKEN}"
  SLACK_BOT_TOKEN: "${env:SLACK_BOT_TOKEN}"
subscribes:
  - claude.session_started
  - task_run.worktree_ready
  - task_run.completed
```

### macOS Notification Center (bash)

```yaml
name: notify-sh
command: [/bin/bash, /Users/me/work/bigband/examples/extensions/notify-sh/notify.sh]
env:
  PATH: "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"
  NOTIFY_TASKS: "*"
restart:
  policy: always       # script's reconnect loop is internal, but restart on any exit anyway
subscribes: [task_run.completed]
```

## How this differs from a per-extension LaunchAgent

Before manifests, every extension shipped its own `bigband-slack install` / `install.sh`. The user ended up with N LaunchAgents. The daemon was unaware that the extensions even existed.

With manifests:

- Only `bigband install` runs as a LaunchAgent.
- The daemon supervises every extension declared by a manifest.
- `bigband ext list` is the single place to see what's running.
- Extensions get lifecycle events for *each other* — a future supervisor dashboard can subscribe to `extension.*` and render them all.

The IPC and events contracts are unchanged. Existing extensions only need a `manifest.yaml` to participate.
