#!/usr/bin/env bash
# bigband-notify (bash edition) — post macOS notifications for bigband job
# completions. ~50 lines, no compile step, no replay-on-restart: missed events
# while this script wasn't running stay missed (which is what you want for
# desktop banners).
#
# Requires: bigband, jq, and either terminal-notifier (preferred, supports
# click-to-open) or the macOS built-in osascript (fallback).
#
# Usage:
#   ./notify.sh                       # notify on every completed job
#   NOTIFY_JOBS=morning-brief,report-* ./notify.sh
#   NOTIFY_FAILURES_ONLY=1 ./notify.sh
#
# Install as a LaunchAgent: see README.md next to this file.

set -euo pipefail

JOBS="${NOTIFY_JOBS:-*}"
TITLE_PREFIX="${NOTIFY_TITLE_PREFIX:-bigband}"
SOUND="${NOTIFY_SOUND:-default}"
FAILURE_SOUND="${NOTIFY_FAILURE_SOUND:-Basso}"
FAILURES_ONLY="${NOTIFY_FAILURES_ONLY:-0}"
GROUP_PREFIX="${NOTIFY_GROUP_PREFIX:-io.bigband}"

have_tn() { command -v terminal-notifier >/dev/null 2>&1; }

# matches returns 0 if $1 matches one of the comma-separated patterns in JOBS.
# Patterns support a single trailing/leading "*" wildcard, like the Slack
# sidecar's matchPattern helper.
matches() {
  local job="$1"
  [[ "$JOBS" == "*" ]] && return 0
  local IFS=','
  for pat in $JOBS; do
    # shellcheck disable=SC2254  # glob match is intentional (report-* etc.)
    case "$job" in
      $pat) return 0 ;;
    esac
  done
  return 1
}

# post sends one notification. Args: job status message log_path.
post() {
  local job="$1" status="$2" message="$3" log="$4"
  local sound="$SOUND"
  [[ "$status" != "ok" && -n "$FAILURE_SOUND" ]] && sound="$FAILURE_SOUND"
  local title="$TITLE_PREFIX — $job"
  local subtitle="$status"

  # Collapse newlines; macOS notifications render single-line anyway.
  message="${message//$'\n'/ }"
  message="${message//$'\r'/ }"
  # Truncate to keep the banner short.
  if [[ ${#message} -gt 240 ]]; then
    message="${message:0:237}..."
  fi

  if have_tn; then
    local args=(-title "$title" -subtitle "$subtitle" -message "$message"
      -group "$GROUP_PREFIX.$job")
    [[ -n "$sound" ]] && args+=(-sound "$sound")
    [[ -n "$log"   ]] && args+=(-execute "open \"$log\"")
    terminal-notifier "${args[@]}" >/dev/null
    return
  fi

  # osascript fallback. Escape backslashes first, then double quotes — order
  # matters or the doubled backslashes get re-escaped.
  local m="${message//\\/\\\\}"; m="${m//\"/\\\"}"
  local s="${subtitle//\\/\\\\}"; s="${s//\"/\\\"}"
  local t="${title//\\/\\\\}";    t="${t//\"/\\\"}"
  local snd=""
  [[ -n "$sound" ]] && snd=" sound name \"$sound\""
  osascript -e "display notification \"$m\" with title \"$t\" subtitle \"$s\"$snd"
}

# Reconnect loop: bigband subscribe exits when the daemon restarts. Sleep a
# bit and reattach. No --since means we don't replay past events — that's the
# whole point of preferring bash here.
while true; do
  bigband subscribe \
    --types job_run.completed,job_run.failed_pre_exec \
    --name bigband-notify-sh \
  | while IFS= read -r line; do
      [[ -z "$line" ]] && continue
      job=$(jq -r '.job_name // ""'         <<<"$line")
      [[ -z "$job" ]] && continue
      matches "$job" || continue

      type=$(jq -r '.type // ""'              <<<"$line")
      status=$(jq -r '.data.status // ""'     <<<"$line")
      [[ "$type" == "job_run.failed_pre_exec" ]] && status="pre-exec failed"

      [[ "$FAILURES_ONLY" == "1" && "$status" == "ok" ]] && continue

      msg=$(jq -r '.data.final_message // .data.error // ""' <<<"$line")
      log=$(jq -r '.data.log_path // ""'      <<<"$line")
      post "$job" "$status" "$msg" "$log"
    done
  echo "bigband-notify: subscribe ended; reconnecting in 2s" >&2
  sleep 2
done
