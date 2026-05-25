# bigband-slack

The reference Slack integration for bigband.

It is a **separate process** from the bigband daemon and only talks to bigband through the [public IPC and events contracts](../../EXTENDING.md). Same monorepo, separate binary, separate launchd plist. Everything Slack-specific lives here; bigband core knows nothing about Slack.

## What it does

- **Outbound mirror**: subscribes to `job_run.completed` events and posts the final assistant message into Slack channels you opted in to.
- **Inbound triggers**: in channels you list, a message fires a one-off bigband run (`submit_run` IPC). The reply lands in a thread so you can follow up.
- **Thread continuity**: replying in a thread runs `submit_run` with `parent_session_id`, resuming the same Claude session.

Empty config = does nothing. Mirror rules and trigger channels are explicitly opt-in.

## Install

```sh
make install-slack                 # builds + copies to ~/bin/bigband-slack
bigband-slack init                 # writes config.yaml + manifest.yaml under ~/.bigband/extensions/bigband-slack/
```

The bigband daemon discovers the manifest via fsnotify and supervises bigband-slack as a child process. You only need `bigband install` once for the whole system — there's no per-extension LaunchAgent any more.

```sh
bigband ext list                   # confirm bigband-slack is RUNNING
bigband ext logs bigband-slack -f  # tail its stdout/stderr
```

Edit the config to match your workspace. At minimum you need:
- A Slack app with **Socket Mode** enabled (see scopes below).
- An app-level token with `connections:write` (`xapp-...`).
- A bot OAuth token (`xoxb-...`) with the scopes below.
- The bot invited into every channel you mirror to or trigger from (`/invite @<your-app>`).

### Slack app scopes

Bot scopes:
- `chat:write` — post messages
- `app_mentions:read` — receive `@app` mentions
- `channels:history`, `channels:read` — read public channels
- `groups:history`, `groups:read` — read private channels (only if you use any)
- `users:read` — resolve user names

Event subscriptions:
- `app_mention`
- `message.channels`
- `message.groups` (private channels)

App-level token scope: `connections:write`.

## Tokens

Tokens reach the slack daemon through the manifest's `env` block. The manifest written by `bigband-slack init` uses `${env:SLACK_APP_TOKEN}` / `${env:SLACK_BOT_TOKEN}` placeholders, which the bigband daemon resolves from its own environment when spawning the slack child.

So: export the tokens in the environment the bigband daemon was started in. With launchd that means using `launchctl setenv` *before* `bigband install`, or restarting the daemon after exporting:

```sh
launchctl setenv SLACK_APP_TOKEN xapp-...
launchctl setenv SLACK_BOT_TOKEN xoxb-...
bigband install      # the daemon inherits launchctl's env
```

Higher-security alternative: use `file:/abs/path` token references in the slack config and skip the env vars entirely.

```sh
mkdir -p ~/.config/bigband-slack && chmod 700 ~/.config/bigband-slack
printf '%s' "xapp-..." > ~/.config/bigband-slack/app_token && chmod 600 ~/.config/bigband-slack/app_token
printf '%s' "xoxb-..." > ~/.config/bigband-slack/bot_token && chmod 600 ~/.config/bigband-slack/bot_token
```

In `~/.bigband/extensions/bigband-slack/config.yaml`:

```yaml
slack:
  app_token: file:/Users/me/.config/bigband-slack/app_token
  bot_token: file:/Users/me/.config/bigband-slack/bot_token
```

Then drop the `${env:...}` lines from the manifest's `env:` block — the slack daemon reads the tokens directly from disk.

```sh
bigband ext logs bigband-slack -f  # tail the supervisor-managed stdout/stderr
bigband ext restart bigband-slack  # bounce after editing config or manifest
```

(There is also a hidden `bigband-slack daemon` subcommand — that's the supervisor entry point. You don't run it yourself.)

## Config

`~/.bigband/extensions/bigband-slack/config.yaml`. Auto-reloaded on every save via `fsnotify`. Token / connection changes still require a full restart — the socket-mode session is bound at startup.

```yaml
slack:
  app_token: env:SLACK_APP_TOKEN          # or file:/abs/path
  bot_token: env:SLACK_BOT_TOKEN
  default_channel: "#bigband"             # fallback when a rule omits one

# OUTBOUND. Empty list = nothing is mirrored. First match wins.
mirror:
  - job: morning-brief
    channel: "#daily"
    open_thread: true                     # post final message as a new thread
    include_status: true                  # prefix "ok in 4m12s"
    on_failure: false                     # also post on non-success runs
  - jobs: ["report-*", "alert-*"]         # simple glob: prefix-* or *-suffix
    channel: "#reports"

# INBOUND. Empty list = no Slack message ever fires a run.
trigger_channels:
  - channel: "#bigband-control"           # name or channel ID
    folder: /Users/me/work/cloudgrid
    require_mention: true                 # only act on @app messages
    allow_freeform_prompt: true           # plain message → ephemeral submit_run
    commands:
      - match: "^run (?P<job>\\S+)$"
        action: run                       # bigband run <job>
      - match: "^job (?P<name>\\S+):\\s*(?P<prompt>.+)"
        action: submit

threads:
  enabled: true
  resume_with_session: true               # use ParentSessionID; otherwise fresh ephemeral

retention: 168h                           # drop store entries last touched > this ago
```

### Operator commands

Mirror rules (outbound — job completion → Slack post):

```sh
bigband-slack rules list                       # show mirror rules
bigband-slack enable <job> --channel "#x" --thread
bigband-slack disable <job>
```

Trigger channels (inbound — Slack message → bigband run):

```sh
bigband-slack trigger list
bigband-slack trigger add --channel "#bigband-control" --folder /path/to/repo
bigband-slack trigger rm "#bigband-control"
```

Diagnostic / debug:

```sh
bigband ext list                               # supervisor view: status, pid, restarts
bigband ext logs bigband-slack -f              # tail stdout/stderr
bigband ext restart bigband-slack              # bounce
bigband-slack mirror <job> [--dry-run]         # re-post the latest completion of a job (testing)
```

Setup:

```sh
bigband-slack init                             # scaffold config + manifest under extensions/bigband-slack/
```

The bigband daemon discovers the manifest and supervises the binary; there's no per-extension LaunchAgent.

## Troubleshooting

- **`channel_not_found`** — bot isn't in the channel. Run `/invite @<your-app>` in the channel. Channel name must match the workspace where the bot token lives. Channel IDs (e.g. `C0B2QBTFLUU`) also work in `channel:` fields.
- **Inbound message ignored** — verify the channel is listed under `trigger_channels`. The `bigband-slack: ignored message in #foo (Cxxx)` log line tells you the channel the bot saw. If `require_mention: true`, the message must include `@<your-app>`.
- **Thread reply doesn't follow up** — the thread mapping is built only after the bot first posts in that thread. Replies to threads that pre-date the integration are ignored.
- **Daemon log says "not in channel"** — same as `channel_not_found`; bot must be invited.
- **Token issues** — check `bigband ext logs bigband-slack` for "token" / "auth" lines, or look for `unresolved env placeholders` in `bigband daemon-logs` (manifest `${env:NAME}` interpolation failed). The bot must be in the same workspace as the channel.

## Ethos

This integration deliberately:
- Is a **separate binary** so a Slack outage / lib bug can't take down the bigband daemon.
- Owns **its own config + state** under `extensions/bigband-slack/` — bigband core stays Slack-agnostic.
- Auto-reloads rules on file save via `fsnotify`.
- Talks to bigband only through the public `pkg/bigbandext` SDK — what any external integration would have access to. Lifts no internal-only abstractions.
- Survives daemon restarts via `Subscribe(Since=lastSeen)` replay, with EventID-based dedup so handlers run at-most-once per event.
- Falls back gracefully (ID-based channel match if name resolution fails, default rule when posting Slack-originated runs, etc.) so a partial misconfiguration still does *something*.
- Persists nothing in bigband core except the documented contracts.

If you want to write your own integration (Discord, GitHub comments, an HTTP webhook, ...), this binary is the reference: read the [tutorial](../../docs/INTEGRATIONS.md) for a walkthrough, the [extension docs](../../EXTENDING.md) for the contracts, the [event reference](../../docs/EVENTS.md) for payload schemas, and the source files in this directory for a worked example.
