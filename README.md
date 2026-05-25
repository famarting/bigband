# bigband

**bigband** is a macOS daemon that runs [Claude Code](https://claude.ai/code) prompts on a cron schedule. Define jobs in a YAML config file — each job runs `claude` in a target folder at the specified time, with full log capture and optional git worktree isolation.

## How it works

bigband runs as a background daemon (via launchd or manually). On each scheduled tick it:

1. Runs optional `pre_exec` shell commands (e.g. `git pull`)
2. Creates an isolated git worktree for the job (optional)
3. Invokes `claude -p <prompt>` in the target folder
4. Streams and logs all output
5. Runs optional `post_exec` commands (e.g. commit, push, notify)
6. Cleans up the worktree (unless configured to keep/reuse it)

## Prerequisites

- macOS (auto-starts via `bigband install` → launchd) or Linux (use the systemd unit in [examples/systemd](examples/systemd))
- [Claude Code CLI](https://claude.ai/code) (`claude` in PATH, authenticated)
- Go 1.26+
- git

## Installation

```sh
go install github.com/famarting/bigband/cmd/bigband@latest
```

Or build from source:

```sh
git clone https://github.com/famarting/bigband
cd bigband
make install   # builds and copies to ~/bin/bigband
```

## Quick start

```sh
# Add your first job (interactive wizard)
bigband add

# Install as a launchd service (auto-starts on login;
# re-run after upgrading the binary to restart with the new version)
bigband install

# List configured jobs and schedules
bigband list

# Show recent execution history
bigband status

# Tail a running job
bigband logs <job> --follow
```

## Configuration

Config file: `~/.bigband/config.yaml`  
Override location: `BIGBAND_HOME` environment variable.

```yaml
defaults:
  shell: /bin/sh                # shell for pre/post_exec
  timeout: 45m                  # max Claude runtime per job
  retain_logs: 50               # log files to keep per job
  jitter: 15m                   # random delay applied when ~ is in schedule
  ephemeral_retention: 168h     # auto-prune ephemeral one-off state + logs
  model: claude-opus-4-7        # Claude model used by all jobs (optional)
  effort: high                  # thinking effort level: low, medium, high (optional)
  folder: /path/to/repo         # default folder used by `bigband add` wizard
  pre_exec:                     # default pre_exec used by `bigband add` wizard
    - git fetch --all

templates:
  - name: triage
    folder: /path/to/repo
    prompt: |
      Review open issues and PRs. Summarise activity and flag urgent items.
    pre_exec:
      - git pull origin main

jobs:
  - name: morning-triage
    schedule: "Weekdays at ~9:00"   # human-readable, see Schedules below
    folder: /path/to/repo
    prompt: |
      Review open GitHub issues and PRs. Summarise new activity,
      flag anything urgent, and draft replies for obvious wins.
    pre_exec:
      - git pull origin main
    post_exec:
      - git add -A && git commit -m "auto: morning triage" || true

  - name: daily-docs
    schedule: "0 22 * * *"          # standard 5-field cron
    folder: /path/to/repo/docs
    timeout: 30m
    model: claude-sonnet-4-6        # override model for this job
    prompt: Update the changelog based on commits since yesterday.
    extra_claude_flags: ["--allowedTools", "Read,Write,Bash"]
  - name: one-off-refactor
    # no schedule — fires once immediately, then stays in state
    folder: /path/to/repo
    reuse_worktree: true            # pick up where Claude left off
    prompt: Refactor the auth module to use the new middleware pattern.
```

### Job fields

| Field | Default | Description |
|---|---|---|
| `name` | required | Unique identifier (`[a-z0-9][a-z0-9-_]*`) |
| `schedule` | — | Cron schedule (omit for one-off) |
| `folder` | required | Absolute path where Claude runs |
| `prompt` | required | Prompt passed to `claude -p` |
| `enabled` | `true` | Set `false` to pause a job |
| `timeout` | `defaults.timeout` (45m) | Max Claude runtime |
| `jitter` | `defaults.jitter` | Random delay added at start (overrides default) |
| `model` | `defaults.model` | Claude model (e.g. `claude-opus-4-7`); overrides global default |
| `effort` | `defaults.effort` | Thinking effort level (`low`, `medium`, `high`); overrides global default |
| `pre_exec` | `[]` | Shell commands run before Claude |
| `post_exec` | `[]` | Shell commands run after Claude |
| `extra_claude_flags` | `[]` | Extra flags appended to the `claude` invocation |
| `worktree` | `true` | Master switch: when `false`, Claude runs directly in `folder` and `keep_worktree`/`reuse_worktree` are ignored. |
| `keep_worktree` | `true` | Keep worktree after the run for inspection; it is discarded at the start of the next run. Set `false` to remove it immediately after each run. |
| `reuse_worktree` | `false` | Reuse the existing worktree as-is across runs (skip discard + recreate at run start) |

### Defaults fields

| Field | Default | Description |
|---|---|---|
| `shell` | `/bin/sh` | Shell used to run `pre_exec` / `post_exec` |
| `timeout` | `45m` | Default per-job max runtime |
| `retain_logs` | `50` | Log files kept per job |
| `jitter` | `15m` | Default jitter window when `~` is in a schedule |
| `ephemeral_retention` | `168h` | How long IPC-submitted one-off state + logs are kept before auto-prune; configured jobs are never touched. Set to `0s` to disable. |
| `model` | — | Claude model applied to all jobs (e.g. `claude-opus-4-7`). Omit to let Claude use its own default. |
| `effort` | — | Thinking effort level applied to all jobs (`low`, `medium`, `high`). Omit to let Claude use its own default. |
| `folder` | — | Default folder seeded by `bigband add` |
| `pre_exec` | `[]` | Default pre-exec list seeded by `bigband add` |

### Templates

Templates are reusable job definitions stored under a `templates:` key. They
are not scheduled; instead, use `bigband add --from <template>` to seed a new
job with the template's prompt, folder, pre/post exec, and worktree settings.
Manage them with `bigband template …` (see commands below).

### Schedules

bigband accepts three formats:

```
# Standard 5-field cron
"0 20 * * 1-5"

# Robfig descriptors
"@daily"  "@hourly"  "@every 30m"

# Human DSL
"Weekdays at 9:00"
"Mondays at ~8am"          # ~ enables jitter (±defaults.jitter)
"Mon, wed, fri at 18:00"
"Daily at noon"
"Every 10 minutes"
```

Validate a schedule without touching the daemon:

```sh
bigband validate
```

### Post-exec environment

`post_exec` commands receive these variables:

| Variable | Value |
|---|---|
| `BIGBAND_STATUS` | `ok`, `failed`, `timeout`, `pre_failed`, `stopped`, `unknown` |
| `BIGBAND_JOB` | Job name |
| `BIGBAND_LOG` | Absolute path to this run's log file |
| `BIGBAND_WORKTREE` | Worktree path (empty if none) |
| `BIGBAND_REPLY_FILE` | Path to a sidecar file holding Claude's final assistant message (empty if the run produced no text — e.g. ended on a tool call) |
| `BIGBAND_SESSION_ID` | Claude session id captured during the run; pass to `claude --resume <id>` to continue |

## Commands

### Daemon

| Command | Description |
|---|---|
| `bigband install` | Install as a launchd LaunchAgent (auto-start on login). If already installed, restarts the agent so a freshly built binary takes effect. |
| `bigband uninstall` | Stop and remove the LaunchAgent |
| `bigband daemon-logs [-f] [-n N]` | Print or tail the daemon log |

### Jobs

| Command | Description |
|---|---|
| `bigband list` | List configured jobs with schedule, enabled flag, and next run |
| `bigband status [-n N] [-r]` | Show recent execution history (`-r` filters to active/orphaned runs) |
| `bigband get <name>` | Show full config and state for a single job |
| `bigband add [--from <name>]` | Add a job via interactive wizard, optionally seeded from an existing job or template |
| `bigband edit [name]` | Edit a single job in `$EDITOR`, or the whole config if no name given |
| `bigband rm <name>` | Remove a job and clean up its worktree |
| `bigband enable <name>` | Enable a disabled job |
| `bigband disable <name>` | Pause a job without removing it |
| `bigband worktree move <job> <dest>` | Move a job's worktree to a new location |
| `bigband worktree rm <job>` | Remove a job's tracked worktree |

### Templates

| Command | Description |
|---|---|
| `bigband template list` | List configured templates |
| `bigband template add [--from <name>]` | Add a template via interactive wizard |
| `bigband template edit <name>` | Edit a template's YAML in `$EDITOR` |
| `bigband template rm <name>` | Remove a template |
| `bigband template get <name>` | Print a template's full definition |
| `bigband template save <job> [--as <name>]` | Save an existing job as a template (drops `schedule` and `enabled`) |

### Running

| Command | Description |
|---|---|
| `bigband run <name>` | Fire a job immediately (bypasses schedule and jitter). Falls back to inline execution if the daemon is not running. |
| `bigband submit --folder <dir> --prompt "..."` | Submit a one-off ephemeral run via IPC (no `config.yaml` edit). Returns a run id immediately. |
| `bigband followup <job> "<prompt>"` | Resume the job's last Claude session with a new prompt (sugar around `submit --parent-session-id`). |
| `bigband stop <name>` | Stop a currently running job |
| `bigband logs <name>` | Print the latest run log |
| `bigband logs <name> -f` | Tail the latest run in real time |
| `bigband logs <name> -l [-n N]` | List the most recent N runs (default 10) |
| `bigband resume <name>` | Resume the Claude session in the job's worktree (interactive) |
| `bigband events [-f] [-n N]` | Print or tail `~/.bigband/events.jsonl` (the durable lifecycle event stream) |
| `bigband subscribe [--types ...] [--jobs ...] [--since <ts>]` | Open a long-lived stream of lifecycle events; `--since` replays from `events.jsonl` |
| `bigband subscribers` | List integrations currently attached to the daemon's event bus |

### Config

| Command | Description |
|---|---|
| `bigband validate [path]` | Parse and validate the config file (default: `~/.bigband/config.yaml`) |
| `bigband config path` | Print the config file path |
| `bigband config edit` | Open the config file in `$EDITOR` |
| `bigband prune [--older-than <dur>] [--keep-logs] [--dry-run]` | Drop ephemeral one-off state (and logs) older than the cutoff. Configured jobs are never touched. |

## Running on Linux

`bigband install` is macOS-only (it generates a launchd plist). On Linux, use the systemd user unit shipped under [examples/systemd/bigband.service](examples/systemd/bigband.service):

```sh
mkdir -p ~/.config/systemd/user
cp examples/systemd/bigband.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now bigband.service

# Tail the daemon log via journald:
journalctl --user -u bigband -f
# Or via bigband's own log file:
bigband daemon-logs -f
```

To survive logout, enable lingering: `loginctl enable-linger $USER`.

## Worktrees

When `folder` is inside a git repo, bigband creates a git worktree for each run on a dedicated branch (`bigband/<job-name>`). This isolates Claude's changes from the main working tree. By default the worktree is kept after the run so you can inspect it — it is discarded and recreated fresh at the start of the next run. Set `keep_worktree: false` to remove it immediately after each run.

With `reuse_worktree: true`, the same worktree persists across runs. Use `bigband resume <job>` to open an interactive Claude session in that worktree, picking up from the last recorded session ID.

## Logs

- Daemon log: `~/.bigband/daemon.log`
- Job logs: `~/.bigband/logs/<job>/<timestamp>.log`
- Reply sidecar: `~/.bigband/logs/<job>/<timestamp>.reply.txt` (Claude's final assistant message)
- Latest symlink: `~/.bigband/logs/<job>/latest.log`
- Events stream: `~/.bigband/events.jsonl` (durable, structured; one line per lifecycle event)

Log rotation keeps the `retain_logs` most recent files per job (default 50). Ephemeral one-off state and logs are auto-pruned per `defaults.ephemeral_retention` (default 7 days).

## Extending bigband

bigband is a focused orchestrator. Anything you'd build on top — Slack mirroring, GitHub comment triggers, webhook posts, custom dashboards — lives in **separate processes** that talk to bigband through documented contracts: IPC, events, on-disk state, and a manifest the daemon uses to spawn and supervise the process.

You install bigband once via `bigband install`. Each extension is a directory under `~/.bigband/extensions/<name>/` with a `manifest.yaml`; the daemon discovers and supervises it automatically (no per-extension LaunchAgent).

- **[`docs/MANIFESTS.md`](docs/MANIFESTS.md)** — manifest schema, the supervisor's restart policies, and the `bigband ext` CLI.
- **[`EXTENDING.md`](EXTENDING.md)** — reference for the IPC, events, and state/logs contracts.
- **[`docs/INTEGRATIONS.md`](docs/INTEGRATIONS.md)** — 30-minute walkthrough that builds a webhook integration in ~50 lines using the public Go SDK at [`pkg/bigbandext/`](pkg/bigbandext).
- **[`docs/EVENTS.md`](docs/EVENTS.md)** — every lifecycle event type and its payload schema.
- **[`cmd/bigband-slack/`](cmd/bigband-slack/README.md)** — production-grade reference integration: Slack socket-mode bot, supervised via manifest.
- **[`examples/extensions/notify-sh/`](examples/extensions/notify-sh/)** — ~80-line bash script that posts macOS notifications, also supervised via manifest.
- **[`examples/extensions/echo-handler/`](examples/extensions/echo-handler/)** — minimal stdlib-only Go subscriber.

## Trust model

bigband is designed for a **single trusted local user**. The daemon, all integrations, and your Claude Code runs all execute under your UID and trust each other implicitly. There is no authentication on the IPC socket, no permission boundary between extensions, and no redaction of model output anywhere on disk.

What this means in practice:

- **The IPC socket** (`~/.bigband/daemon.sock`) sits inside a `chmod 700` directory, so other users on the box can't reach it. Any process running as **you** can submit_run, subscribe to events, fire jobs, or read history. Don't run bigband on a multi-user dev box where you don't trust everyone with your UID.
- **`final_message` lives in plaintext on disk** in three places:
  - `~/.bigband/logs/<job>/<ts>.log` — the per-run log (chmod 0600)
  - `~/.bigband/logs/<job>/<ts>.reply.txt` — the captured Claude reply (chmod 0600)
  - `~/.bigband/events.jsonl` — the durable event stream (chmod 0600)
  
  All three sit under the chmod-700 root. If a job asks Claude to handle a secret (an API key, a credential, a code snippet from a private repo), that content ends up in those files. The `subscribe` IPC stream (and `subscribe --since` replay) re-emits the same content to anyone who can reach the socket. If your threat model includes other processes on your machine reading your home directory, treat these files like credentials.
- **Slack integration tokens**: bigband-slack reads tokens from the slack config (`file:/abs/path/token` references resolved from chmod-600 files are recommended) or from env vars interpolated by the manifest's `${env:NAME}` placeholders, which read from the daemon's process environment. Don't embed token literals in `manifest.yaml` itself — it's the same chmod-600 file but the manifest is meant to be reviewable / committable in dotfiles, the secrets file is not.

### How bigband runs your jobs

bigband runs commands and Claude Code with **your user's permissions**. Specifically:

- `pre_exec` and `post_exec` commands are run as shell commands under the configured `shell`. They inherit your environment.
- `claude` is invoked with a fixed set of required flags (`-p --output-format stream-json --verbose --include-partial-messages`) that bigband needs for output parsing, plus any `--model`, `--effort`, or `extra_claude_flags` you configure. bigband does **not** pass `--dangerously-skip-permissions` for you — if you want fully unattended runs, add it explicitly via `extra_claude_flags`, and only for jobs whose prompts you fully trust.
- Each scheduled job runs the prompt as written without further confirmation. Treat the YAML config like code: anyone who can edit it can run arbitrary shell on your machine on your schedule.

## License

MIT — see [LICENSE](LICENSE).
