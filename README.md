# bigband

**bigband** is a macOS daemon that runs [Claude Code](https://claude.ai/code) prompts on a cron schedule. Define tasks in a YAML config file — each task runs `claude` in a target folder at the specified time, with full log capture and optional git worktree isolation.

## How it works

bigband runs as a background daemon (via launchd or manually). On each scheduled tick it:

1. Runs optional `pre_exec` shell commands (e.g. `git pull`)
2. Creates an isolated git worktree for the task (optional)
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
# Add your first task (interactive wizard)
bigband add

# Install as a launchd service (auto-starts on login;
# re-run after upgrading the binary to restart with the new version)
bigband install

# List configured tasks and schedules
bigband list

# Show recent execution history
bigband status

# Tail a running task
bigband logs <task> --follow
```

## Configuration

Config file: `~/.bigband-tasks/config.yaml`  
Override location: `BIGBAND_HOME` environment variable.

```yaml
defaults:
  shell: /bin/sh          # shell for pre/post_exec
  timeout: 45m            # max Claude runtime per task
  retain_logs: 50         # log files to keep per task
  jitter: 15m             # random delay applied when ~ is in schedule
  model: claude-opus-4-7  # Claude model used by all tasks (optional)
  effort: high            # thinking effort level: low, medium, high (optional)
  folder: /path/to/repo   # default folder used by `bigband add` wizard
  pre_exec:               # default pre_exec used by `bigband add` wizard
    - git fetch --all

templates:
  - name: triage
    folder: /path/to/repo
    prompt: |
      Review open issues and PRs. Summarise activity and flag urgent items.
    pre_exec:
      - git pull origin main

tasks:
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
    model: claude-sonnet-4-6        # override model for this task
    prompt: Update the changelog based on commits since yesterday.
    extra_claude_flags: ["--allowedTools", "Read,Write,Bash"]
    keep_worktree: false

  - name: one-off-refactor
    # no schedule — fires once immediately, then stays in state
    folder: /path/to/repo
    keep_worktree: true             # keep worktree so you can resume
    reuse_worktree: true            # pick up where Claude left off
    prompt: Refactor the auth module to use the new middleware pattern.
```

### Task fields

| Field | Default | Description |
|---|---|---|
| `name` | required | Unique identifier (`[a-z0-9][a-z0-9-_]*`) |
| `schedule` | — | Cron schedule (omit for one-off) |
| `folder` | required | Absolute path where Claude runs |
| `prompt` | required | Prompt passed to `claude -p` |
| `enabled` | `true` | Set `false` to pause a task |
| `timeout` | `defaults.timeout` (45m) | Max Claude runtime |
| `jitter` | `defaults.jitter` | Random delay added at start (overrides default) |
| `model` | `defaults.model` | Claude model (e.g. `claude-opus-4-7`); overrides global default |
| `effort` | `defaults.effort` | Thinking effort level (`low`, `medium`, `high`); overrides global default |
| `pre_exec` | `[]` | Shell commands run before Claude |
| `post_exec` | `[]` | Shell commands run after Claude |
| `extra_claude_flags` | `[]` | Extra flags appended to the `claude` invocation |
| `keep_worktree` | `false` (`true` for one-off / reuse) | Preserve worktree after the run |
| `reuse_worktree` | `false` | Reuse existing worktree across runs (implies keep) |

### Defaults fields

| Field | Default | Description |
|---|---|---|
| `shell` | `/bin/sh` | Shell used to run `pre_exec` / `post_exec` |
| `timeout` | `45m` | Default per-task max runtime |
| `retain_logs` | `50` | Log files kept per task |
| `jitter` | `15m` | Default jitter window when `~` is in a schedule |
| `model` | — | Claude model applied to all tasks (e.g. `claude-opus-4-7`). Omit to let Claude use its own default. |
| `effort` | — | Thinking effort level applied to all tasks (`low`, `medium`, `high`). Omit to let Claude use its own default. |
| `folder` | — | Default folder seeded by `bigband add` |
| `pre_exec` | `[]` | Default pre-exec list seeded by `bigband add` |

### Templates

Templates are reusable task definitions stored under a `templates:` key. They
are not scheduled; instead, use `bigband add --from <template>` to seed a new
task with the template's prompt, folder, pre/post exec, and worktree settings.
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
| `BIGBAND_STATUS` | `ok`, `failed`, `timeout`, `pre_failed` |
| `BIGBAND_TASK` | Task name |
| `BIGBAND_LOG` | Absolute path to this run's log file |
| `BIGBAND_WORKTREE` | Worktree path (empty if none) |

## Commands

### Daemon

| Command | Description |
|---|---|
| `bigband install` | Install as a launchd LaunchAgent (auto-start on login). If already installed, restarts the agent so a freshly built binary takes effect. |
| `bigband uninstall` | Stop and remove the LaunchAgent |
| `bigband daemon-logs [-f] [-n N]` | Print or tail the daemon log |

### Tasks

| Command | Description |
|---|---|
| `bigband list` | List configured tasks with schedule, enabled flag, and next run |
| `bigband status [-n N] [-r]` | Show recent execution history (`-r` filters to active/orphaned runs) |
| `bigband get <name>` | Show full config and state for a single task |
| `bigband add [--from <name>]` | Add a task via interactive wizard, optionally seeded from an existing task or template |
| `bigband edit [name]` | Edit a single task in `$EDITOR`, or the whole config if no name given |
| `bigband rm <name>` | Remove a task and clean up its worktree |
| `bigband enable <name>` | Enable a disabled task |
| `bigband disable <name>` | Pause a task without removing it |
| `bigband worktree move <task> <dest>` | Move a task's worktree to a new location |
| `bigband worktree rm <task>` | Remove a task's tracked worktree |

### Templates

| Command | Description |
|---|---|
| `bigband template list` | List configured templates |
| `bigband template add [--from <name>]` | Add a template via interactive wizard |
| `bigband template edit <name>` | Edit a template's YAML in `$EDITOR` |
| `bigband template rm <name>` | Remove a template |
| `bigband template show <name>` | Print a template's full definition |
| `bigband template save <task> [--as <name>]` | Save an existing task as a template (drops `schedule` and `enabled`) |

### Running

| Command | Description |
|---|---|
| `bigband run <name>` | Fire a task immediately (bypasses schedule and jitter). Falls back to inline execution if the daemon is not running. |
| `bigband stop <name>` | Stop a currently running task |
| `bigband logs <name>` | Print the latest run log |
| `bigband logs <name> -f` | Tail the latest run in real time |
| `bigband logs <name> -l [-n N]` | List the most recent N runs (default 10) |
| `bigband resume <name>` | Resume the Claude session in the task's worktree (interactive) |

### Config

| Command | Description |
|---|---|
| `bigband validate [path]` | Parse and validate the config file (default: `~/.bigband-tasks/config.yaml`) |
| `bigband config path` | Print the config file path |
| `bigband config edit` | Open the config file in `$EDITOR` |

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

When `folder` is inside a git repo, bigband creates a git worktree for each run on a dedicated branch (`bigband/<task-name>`). This isolates Claude's changes from the main working tree. After the run the worktree is removed (unless `keep_worktree: true`).

With `reuse_worktree: true`, the same worktree persists across runs. Use `bigband resume <task>` to open an interactive Claude session in that worktree, picking up from the last recorded session ID.

## Logs

- Daemon log: `~/.bigband-tasks/daemon.log`
- Task logs: `~/.bigband-tasks/logs/<task>/<timestamp>.log`
- Latest symlink: `~/.bigband-tasks/logs/<task>/latest.log`

Log rotation keeps the `retain_logs` most recent files (default 50).

## Security note

bigband runs commands and Claude Code with **your user's permissions**. Specifically:

- `pre_exec` and `post_exec` commands are run as shell commands under the configured `shell`. They inherit your environment.
- `claude` is invoked with a fixed set of required flags (`-p --output-format stream-json --verbose --include-partial-messages`) that bigband needs for output parsing, plus any `--model`, `--effort`, or `extra_claude_flags` you configure. bigband does **not** pass `--dangerously-skip-permissions` for you — if you want fully unattended runs, add it explicitly via `extra_claude_flags`, and only for tasks whose prompts you fully trust.
- Each scheduled task runs the prompt as written without further confirmation. Treat the YAML config like code: anyone who can edit it can run arbitrary shell on your machine on your schedule.

## License

MIT — see [LICENSE](LICENSE).
