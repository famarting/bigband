package main

import (
	"fmt"
	"os/exec"
	"os/user"
	"strings"
)

// sudoersFile is the canonical path the user is asked to write the stanza
// to. Keeping it under /etc/sudoers.d/ means uninstalling is one `rm` and
// the syntax-check happens automatically via visudo -c.
const sudoersFile = "/etc/sudoers.d/bigband-wake"

// SudoersStanza returns a copy-pastable instruction block: the stanza body
// plus the recommended install / verify / uninstall commands. The stanza is
// scoped to the exact pmset verbs the daemon issues — schedule wake … and
// schedule cancel wake … — so granting it doesn't open up the rest of pmset
// (which can put the machine to sleep, force reboot, etc.).
func SudoersStanza() string {
	username := "<your-username>"
	if u, err := user.Current(); err == nil && u.Username != "" {
		username = u.Username
	}
	pmsetPath := "/usr/bin/pmset"
	if p, err := exec.LookPath("pmset"); err == nil {
		pmsetPath = p
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Copy the block below into %s using visudo so the syntax is checked:\n", sudoersFile)
	fmt.Fprintf(&b, "#\n")
	fmt.Fprintf(&b, "#     sudo visudo -f %s\n", sudoersFile)
	fmt.Fprintf(&b, "#\n")
	fmt.Fprintf(&b, "# The stanza grants %s passwordless access to ONLY:\n", username)
	fmt.Fprintf(&b, "#   - %s schedule wake \"MM/DD/YYYY HH:MM:SS\"\n", pmsetPath)
	fmt.Fprintf(&b, "#   - %s schedule cancel wake \"MM/DD/YYYY HH:MM:SS\"\n", pmsetPath)
	fmt.Fprintf(&b, "#   - %s -g sched         (read-only probe used at startup)\n", pmsetPath)
	fmt.Fprintf(&b, "# Nothing else: not sleepnow, not displaysleep, not battery settings.\n")
	fmt.Fprintln(&b, "#")
	fmt.Fprintln(&b, "# ----- begin stanza -----")
	fmt.Fprintf(&b, "Cmnd_Alias BIGBAND_WAKE_PMSET = %s schedule wake *, %s schedule cancel wake *, %s -g sched\n",
		pmsetPath, pmsetPath, pmsetPath)
	fmt.Fprintf(&b, "%s ALL=(root) NOPASSWD: BIGBAND_WAKE_PMSET\n", username)
	fmt.Fprintln(&b, "# ----- end stanza -----")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "Verify the stanza is active (should print pmset output, NOT a password prompt):")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "    sudo -n %s -g sched\n", pmsetPath)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "Smoke-test the full path:")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "    bigband-wake test --in 90s")
	fmt.Fprintln(&b, "    pmset -g sched     # the new wake event should be present")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "Uninstall (revoke the privilege):")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "    sudo rm %s\n", sudoersFile)
	fmt.Fprintln(&b)
	return b.String()
}
