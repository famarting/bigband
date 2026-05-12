//go:build darwin

package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// pmsetTimeLayout is what `pmset schedule` accepts and what `pmset -g sched`
// emits in its "wake at MM/dd/yyyy HH:mm:ss" listings. We always emit local
// time — pmset's clock is the system clock — and never UTC.
const pmsetTimeLayout = "01/02/2006 15:04:05"

// pmsetCancelTimeLayout is the format pmset accepts on the `schedule cancel`
// path. macOS is stricter here: it wants `MM/dd/yyyy HH:mm:ss` with the same
// shape we used to add. We reuse the same constant for clarity.
var pmsetCancelTimeLayout = pmsetTimeLayout

// schedulePmsetWake registers a one-shot wake at t via `sudo -n pmset
// schedule wake "..."`. The -n flag fails fast (without prompting) when the
// sudoers stanza is missing; we propagate the error so the daemon can log it
// loudly and back off.
func schedulePmsetWake(ctx context.Context, t time.Time) error {
	when := t.Local().Format(pmsetTimeLayout)
	cmd := exec.CommandContext(ctx, "sudo", "-n", "pmset", "schedule", "wake", when)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return pmsetError("schedule wake", when, out, err)
	}
	return nil
}

// cancelPmsetWake removes a one-shot wake at t. macOS matches by exact local
// time string; if the entry isn't found pmset still returns 0, so this is
// idempotent.
func cancelPmsetWake(ctx context.Context, t time.Time) error {
	when := t.Local().Format(pmsetCancelTimeLayout)
	cmd := exec.CommandContext(ctx, "sudo", "-n", "pmset", "schedule", "cancel", "wake", when)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return pmsetError("schedule cancel wake", when, out, err)
	}
	return nil
}

// pmsetReachable probes that `sudo -n pmset -g sched` runs without prompting.
// The daemon uses this on startup as a self-check so it can warn the operator
// instead of silently failing on every reconcile.
func pmsetReachable(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "sudo", "-n", "pmset", "-g", "sched")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return pmsetError("probe -g sched", "", out, err)
	}
	return nil
}

// dumpPmsetSched returns the raw `pmset -g sched` text. Used by the `status`
// CLI so the operator can compare our owned entries against the full list.
// Does not require sudo.
func dumpPmsetSched(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "pmset", "-g", "sched")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), err
	}
	return string(out), nil
}

// pmsetError wraps an exec failure with the captured stderr/stdout snippet so
// log lines explain WHY sudo failed (e.g. "a password is required" when the
// sudoers stanza is missing).
func pmsetError(action, when string, out []byte, err error) error {
	snippet := strings.TrimSpace(string(out))
	if snippet == "" {
		snippet = "(no output)"
	}
	if when != "" {
		return fmt.Errorf("pmset %s at %q: %w (%s)", action, when, err, snippet)
	}
	return fmt.Errorf("pmset %s: %w (%s)", action, err, snippet)
}
