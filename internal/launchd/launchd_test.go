package launchd

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// printDump is a trimmed but otherwise verbatim `launchctl print` dump.
const printDump = `gui/501/io.bigband.daemon = {
	active count = 1
	path = /Users/me/Library/LaunchAgents/io.bigband.daemon.plist
	type = LaunchAgent
	state = running

	program = /Users/me/go/bin/bigband
	arguments = {
		/Users/me/go/bin/bigband
		daemon
	}

	pid = 80630
}`

func TestParseProgram(t *testing.T) {
	if got := parseProgram([]byte(printDump)); got != "/Users/me/go/bin/bigband" {
		t.Errorf("parseProgram = %q, want the program line", got)
	}
	// A dump without a program line must read as unknown, not as a mismatch:
	// erroring there would break installs over a formatting difference.
	if got := parseProgram([]byte("gui/501/io.bigband.daemon = {\n\tstate = running\n}")); got != "" {
		t.Errorf("parseProgram = %q, want empty for a dump with no program", got)
	}
}

func TestDefinition(t *testing.T) {
	s := &Service{Label: "io.bigband.test"}

	var asked string
	launchctlPrint = func(target string) ([]byte, error) {
		asked = target
		return []byte(printDump), nil
	}
	t.Cleanup(func() {
		launchctlPrint = func(target string) ([]byte, error) { return nil, errors.New("not stubbed") }
	})

	program, loaded := s.definition()
	if !loaded || program != "/Users/me/go/bin/bigband" {
		t.Errorf("definition = (%q, %v), want the loaded program", program, loaded)
	}
	if want := s.serviceTarget(); asked != want {
		t.Errorf("printed %q, want %q", asked, want)
	}

	// launchctl exits non-zero when nothing is loaded under that target.
	launchctlPrint = func(string) ([]byte, error) { return []byte("Could not find service"), errors.New("exit 113") }
	if program, loaded = s.definition(); loaded {
		t.Errorf("definition = (%q, true), want not loaded", program)
	}
	if s.loaded() {
		t.Error("loaded reported true with no definition")
	}
}

func TestWaitForGivesUp(t *testing.T) {
	settleTimeout = 150 * time.Millisecond
	t.Cleanup(func() { settleTimeout = 10 * time.Second })

	start := time.Now()
	if waitFor(func() bool { return false }) {
		t.Error("waitFor reported success for a condition that never holds")
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Errorf("waitFor returned after %s, expected it to poll for %s", elapsed, settleTimeout)
	}

	calls := 0
	if !waitFor(func() bool { calls++; return calls > 2 }) {
		t.Error("waitFor gave up on a condition that became true")
	}
}

func TestProgramMismatch(t *testing.T) {
	s := &Service{Label: "io.bigband.test"}

	// The bug this guards: a bootout that never finished leaves the previous
	// definition loaded under the same label and plist path, so "is something
	// loaded?" says yes and the install reports a restart that never happened.
	err := s.programMismatch("/new/bin/bigband", "/old/bin/bigband")
	if err == nil {
		t.Fatal("programMismatch accepted a stale definition")
	}
	for _, want := range []string{"/old/bin/bigband", "/new/bin/bigband", s.Label} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}

	for _, tc := range []struct{ name, want, got string }{
		{"same program", "/bin/bigband", "/bin/bigband"},
		{"program unknown", "/bin/bigband", ""},
		{"nothing expected", "", "/bin/bigband"},
	} {
		if err := s.programMismatch(tc.want, tc.got); err != nil {
			t.Errorf("%s: programMismatch = %v, want nil", tc.name, err)
		}
	}
}
