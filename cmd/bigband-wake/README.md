# bigband-wake

Wake-from-sleep scheduler for bigband on macOS.

It is a **separate process** from the bigband daemon and only talks to bigband through the [public IPC and events contracts](../../EXTENDING.md). Same monorepo, separate binary, supervised by the bigband daemon. Everything wake/pmset-specific lives here; bigband core knows nothing about wake scheduling.

## The problem

A bigband job scheduled for `Weekdays at ~6:00` only fires if the laptop is awake at 06:00. macOS suspends launchd-managed processes during system sleep, and `robfig/cron` (the scheduler bigband uses) does **not** replay missed firings when the system wakes back up — the cron tick simply doesn't happen.

So if you sleep your MacBook overnight and the 06:00 job should have fired, you wake up at 09:00 and notice nothing ran. That's not a bigband bug; that's macOS power management doing its job.

## What it does

- Subscribes to `job_run.completed` and `config.reloaded` events from bigband.
- Reads each enabled scheduled job's `next_run` via the `status` IPC.
- Calls `sudo -n pmset schedule wake "MM/DD/YYYY HH:MM:SS"` to register a one-shot system wake `lead_seconds` before each upcoming firing.
- Tracks the wake events it owns in `state.json` so cancel-on-shutdown / cancel-on-reconcile only touches **its own** entries — user-added pmset events (Calendar, DoNotDisturb, etc.) are left alone.

End result: a closed-lid MacBook on AC wakes itself ~60 s before each scheduled job, runs it, and goes back to sleep.

Not on macOS, not on AC, or `enabled: false` → bigband-wake is a logging no-op. It cannot make the daemon fail.

## Install

```sh
make install-wake                  # builds + copies to ~/bin/bigband-wake
bigband-wake init                  # writes config.yaml + manifest.yaml under ~/.bigband/extensions/bigband-wake/
bigband-wake setup                 # prints the sudoers stanza (does NOT run sudo)
```

The bigband daemon discovers the manifest via fsnotify and supervises bigband-wake as a child process. You only need `bigband install` once for the whole system — there's no per-extension LaunchAgent.

```sh
bigband ext list                   # confirm bigband-wake is RUNNING
bigband ext logs bigband-wake -f   # tail its stdout/stderr
```

## Granting pmset access

`pmset schedule wake` requires root. To keep the daemon running as your user, the recommended path is a narrowly-scoped sudoers stanza — granted to one user, for **three** exact pmset verbs, nothing else.

```sh
bigband-wake setup                 # copy the printed stanza
sudo visudo -f /etc/sudoers.d/bigband-wake   # paste, save (visudo syntax-checks)
sudo -n /usr/bin/pmset -g sched    # verify: should print events, NOT prompt
```

The stanza grants:

```
Cmnd_Alias BIGBAND_WAKE_PMSET = /usr/bin/pmset schedule wake *, /usr/bin/pmset schedule cancel wake *, /usr/bin/pmset -g sched
<your-user> ALL=(root) NOPASSWD: BIGBAND_WAKE_PMSET
```

Notably **not** granted: `pmset sleepnow`, `pmset displaysleepnow`, `pmset -a` (settings changes), or anything else. If you ever want to revoke the privilege: `sudo rm /etc/sudoers.d/bigband-wake`.

## Activating

After init + sudoers, flip the switch:

```sh
$EDITOR ~/.bigband/extensions/bigband-wake/config.yaml
# set: enabled: true

bigband ext restart bigband-wake   # or wait for the manifest watcher; either way
bigband-wake status                # show owned wakes + the full pmset schedule
```

## Verifying it actually works

```sh
bigband-wake test --in 90s         # schedules a wake 90 s from now
pmset -g sched                     # the new event should be visible
# close the lid / unplug-replug magsafe and wait — the Mac should wake itself
```

If you want to confirm the daemon-driven path:

1. Trigger a config reload: `touch ~/.bigband/config.yaml`
2. `bigband-wake status` — owned events should match the soonest scheduled jobs.
3. `bigband ext logs bigband-wake -f` — you should see `reconcile … add=N` lines.

## Config

`~/.bigband/extensions/bigband-wake/config.yaml`. Auto-reloaded on every save via `fsnotify`; no restart needed for `enabled` / `lead_seconds` / `max_events` changes.

```yaml
# Master switch. With enabled:false the daemon idles — no pmset calls.
enabled: true

# How many seconds before each scheduled fire time to wake. 60 s gives the
# daemon time to thaw before cron triggers.
lead_seconds: 60

# Cap on simultaneously-owned pmset entries. macOS allows ~64 total; 16
# leaves room for user-added events. Hard-capped at 32.
max_events: 16

# Safety-net cadence: even if every event-bus nudge is missed, reconcile
# from scratch this often. Minimum 1 minute.
reconcile_interval: 1h
```

## Operator commands

```sh
bigband-wake init                  # scaffold config + manifest
bigband-wake setup                 # print sudoers stanza
bigband-wake status                # owned wakes + full `pmset -g sched`
bigband-wake clear                 # cancel every wake bigband-wake owns
bigband-wake test --in 90s         # one-off wake to smoke-test sudo+pmset
```

Service-side:

```sh
bigband ext list                   # supervisor view
bigband ext logs bigband-wake -f   # tail stdout/stderr
bigband ext restart bigband-wake   # bounce
```

(There is also a hidden `bigband-wake daemon` subcommand — that's the supervisor entry point. You don't run it yourself.)

## How reconcile works

Single-owner reconciler with five triggers:

1. **Extension startup** — initial reconcile against current `bigband status`.
2. **`job_run.completed`** event — that job's next-fire just rolled forward.
3. **`config.reloaded`** event (core) — jobs may have been added / removed / disabled.
4. **Hourly safety-net tick** — recovers from any missed event.
5. **fsnotify on our own `config.yaml`** — `enabled` / `lead_seconds` edits.
6. **On SIGTERM** — cancel every owned wake before exiting.

Each trigger writes a short reason string into a buffered channel. The reconcile goroutine debounces 500 ms so a burst of job completions coalesces into a single pmset reshuffle. Reconcile diffs `(job, wake_at)` tuples against `state.json` — cancel-removed first, then add-new — and persists the new set atomically.

`state.json` lives at `~/.bigband/extensions/bigband-wake/state.json`. Cancellation is by **exact local time** because that's how pmset matches; we never `cancelall`, so a hand-added `pmset schedule wake "..."` survives untouched.

## Caveats

- **macOS only.** Non-darwin builds are a logging no-op. The manifest's `supervisor` happily spawns them; they just idle.
- **AC only.** macOS only honors scheduled wakes on AC power by default. On battery, the wake is queued but won't actually fire until the laptop is plugged in.
- **Lid open vs. closed.** Closed-lid wakes are allowed when on AC and an external display is connected, OR when "Prevent automatic sleep" is set in Energy preferences for a power source. Otherwise the system wakes briefly to fire the cron, then goes back to sleep — which is fine.
- **DST transitions.** pmset uses local time. Reconcile computes wake times from each job's `next_run`, which the daemon already formats in local time, so DST round-trips correctly. The hourly tick catches any edge case the same day.
- **No catch-up for missed runs.** If a wake fails to fire (e.g. unplugged), the cron firing is still lost. Adding "fire missed jobs on wake" is a separate feature; bigband-wake is preventative, not corrective.

## Ethos

This integration deliberately:
- Is a **separate binary** so a sudo / pmset bug can't take down the bigband daemon.
- Owns **its own config + state** under `extensions/bigband-wake/` — bigband core stays wake-agnostic.
- Confines all privilege escalation to this binary. The bigband daemon never invokes sudo.
- Talks to bigband only through the public `pkg/bigbandext` SDK — what any external integration would have access to.
- Cancels only its own pmset entries, identified by exact local time + state-file tracking. User-added wakes are untouched.
- Survives daemon restarts: state is durable, and the subscribe stream uses `Since=lastSeen` replay with EventID-based dedup.
- Falls back to a logging no-op on any unsupported platform / missing sudoers / disabled config, so dropping the manifest in is always reversible.
