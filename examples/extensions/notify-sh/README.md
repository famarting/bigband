# bigband-notify (bash edition)

A ~80-line bash script that posts macOS Notification Center banners when
bigband tasks finish. Demonstrates that a useful integration doesn't need
its own binary — the `bigband subscribe` CLI plus `jq` plus `osascript` are
enough.

```
~/.bigband-tasks/extensions/notify-sh/
└── notify.sh        (this script)
```

## Why bash here, Go for Slack?

| Need                       | Slack sidecar | This script |
| -------------------------- | ------------- | ----------- |
| Replay missed events       | yes (`Since`) | no          |
| Per-task config file       | yes           | env vars    |
| Hot-reload rules           | yes           | restart     |
| Click-to-open / actions    | yes           | partial     |
| Lines of code              | ~1,500        | ~80         |

For desktop banners, "missed while my laptop was off" doesn't matter — by
the time you read the banner the run is hours old anyway. So the script
deliberately skips `--since` and stays simple.

## Install

```sh
brew install jq                       # required
brew install terminal-notifier        # optional; enables click-to-open + sound
chmod +x notify.sh
```

Run in the foreground to verify:

```sh
NOTIFY_TASKS=morning-brief ./notify.sh
# (in another terminal) bigband run morning-brief
```

You should see a Notification Center banner when the run completes.

## Tunables (env vars)

| Var                     | Default          | Effect                                                                |
| ----------------------- | ---------------- | --------------------------------------------------------------------- |
| `NOTIFY_TASKS`          | `*`              | Comma-separated task patterns. `*` wildcard supported (`report-*`).   |
| `NOTIFY_FAILURES_ONLY`  | `0`              | When `1`, only notify on non-ok runs.                                 |
| `NOTIFY_TITLE_PREFIX`   | `bigband`        | Prefix for the notification title.                                    |
| `NOTIFY_SOUND`          | `default`        | macOS sound name for successful runs. Empty disables.                 |
| `NOTIFY_FAILURE_SOUND`  | `Basso`          | Sound for non-ok runs.                                                |
| `NOTIFY_GROUP_PREFIX`   | `io.bigband`     | terminal-notifier group prefix; new banners replace older same-group. |

## Run under the bigband daemon (manifest-supervised)

Drop the directory under `~/.bigband-tasks/extensions/notify-sh/` and the
running bigband daemon picks it up via fsnotify within ~300ms. No separate
LaunchAgent — the daemon is the only thing under launchd.

```sh
mkdir -p ~/.bigband-tasks/extensions
cp -R . ~/.bigband-tasks/extensions/notify-sh
# OR symlink for in-place editing:
ln -s "$(pwd)" ~/.bigband-tasks/extensions/notify-sh
```

Edit `command` in `manifest.yaml` to the absolute path of `notify.sh` if you
copied (relative paths are resolved against `working_dir`, which defaults to
the manifest's parent — symlinks just work).

Verify and tail:

```sh
bigband ext list                # NAME / STATUS / PID / RESTARTS / LAST EXIT
bigband ext logs notify-sh -f
bigband ext restart notify-sh
```

Configure tasks via `env.NOTIFY_*` in the manifest (edits are picked up live
on save). The full env-var table is the same one the script reads at runtime
— see the section above.

### Migrating from the old install.sh / uninstall.sh

Earlier versions of this example shipped a `install.sh` that wrote a
per-extension LaunchAgent. If you ran it, remove that LaunchAgent before
installing the manifest so you don't end up with two notify-sh subscribers:

```sh
launchctl bootout "gui/$(id -u)/io.bigband.notify-sh" 2>/dev/null || true
rm -f ~/Library/LaunchAgents/io.bigband.notify-sh.plist
```

Then drop the manifest as above. `bigband subscribers` should show exactly
one `bigband-notify-sh` row.

## Limits, by design

- No replay across restarts — see table above.
- No per-task config file. If you want per-task channels/sounds, port this
  to Go using `cmd/bigband-slack/` as the reference.
- No click-to-open under the osascript fallback. macOS doesn't support it
  natively; install `terminal-notifier` to get it.
