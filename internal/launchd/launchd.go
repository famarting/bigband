// Package launchd generates and manages macOS LaunchAgent plists for bigband
// and its sidecars (e.g. bigband-slack).
//
// Service is the parameterised entry point: each service has its own label,
// argv, log path, and optional environment variables, but shares plist
// generation and bootstrap/unload logic. Pre-existing helpers
// (Install/Uninstall/Start/Stop) wrap the bigband-daemon Service for
// backwards compatibility.
package launchd

import (
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/famarting/bigband/internal/paths"
)

const bigbandLabel = "io.bigband.daemon"

// Service describes one LaunchAgent. Construct with NewBigbandService or
// build inline for sidecars.
type Service struct {
	// Label is the launchd reverse-DNS label, e.g. "io.bigband.daemon".
	Label string
	// Args are the program arguments (the binary path is prepended). The
	// first non-binary slot is typically the subcommand (e.g. "daemon").
	Args []string
	// LogPath is where stdout+stderr are redirected.
	LogPath string
	// Env adds EnvironmentVariables to the plist on top of HOME/PATH.
	Env map[string]string
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

var plistTemplate = template.Must(template.New("plist").Funcs(template.FuncMap{
	"xml": xmlEscape,
}).Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>{{xml .Label}}</string>
    <key>ProgramArguments</key>
    <array>
{{- range .Args}}
        <string>{{xml .}}</string>
{{- end}}
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>
    <key>StandardOutPath</key>
    <string>{{xml .LogPath}}</string>
    <key>StandardErrorPath</key>
    <string>{{xml .LogPath}}</string>
    <key>EnvironmentVariables</key>
    <dict>
{{- range $k, $v := .Env}}
        <key>{{xml $k}}</key>
        <string>{{xml $v}}</string>
{{- end}}
    </dict>
</dict>
</plist>
`))

type plistData struct {
	Label   string
	Args    []string
	LogPath string
	Env     map[string]string
}

// PlistPath is where this Service's plist will be written.
func (s *Service) PlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", s.Label+".plist")
}

// Install writes the plist and starts (or restarts) the agent.
func (s *Service) Install() error {
	binary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot determine binary path: %w", err)
	}
	home, _ := os.UserHomeDir()
	if err := paths.EnsureDirs(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.LogPath), 0700); err != nil {
		return fmt.Errorf("creating log dir: %w", err)
	}

	alreadyInstalled := s.PlistExists()

	args := append([]string{binary}, s.Args...)
	env := map[string]string{
		"HOME": home,
		"PATH": os.Getenv("PATH"),
	}
	// User-supplied env overrides HOME/PATH if explicitly set.
	for k, v := range s.Env {
		env[k] = v
	}
	data := plistData{
		Label:   s.Label,
		Args:    args,
		LogPath: s.LogPath,
		Env:     sortedEnv(env),
	}

	f, err := os.Create(s.PlistPath())
	if err != nil {
		return fmt.Errorf("creating plist: %w", err)
	}
	if err := plistTemplate.Execute(f, data); err != nil {
		f.Close()
		return fmt.Errorf("writing plist: %w", err)
	}
	f.Close()
	fmt.Printf("Wrote %s\n", s.PlistPath())

	// Unload before loading, rather than `launchctl kickstart -k`. kickstart
	// restarts the process launchd already has in memory; it does not re-read
	// the plist we just wrote. So with kickstart, neither a changed binary path
	// nor a changed PATH takes effect — and if the old binary has been deleted
	// (e.g. moving from ~/bin to $(go env GOPATH)/bin), launchd is left trying
	// to spawn a file that no longer exists and kickstart blocks waiting for it.
	// bootout+bootstrap is the only pair that reloads the definition.
	if alreadyInstalled {
		if err := s.bootstrapUnload(); err != nil {
			// Not fatal on its own: the service may not be loaded at all, which
			// is exactly the state we are trying to reach. Still loaded is a
			// different matter — the bootstrap below would then re-confirm the
			// old definition and we would report a restart that never happened.
			if s.loaded() {
				return fmt.Errorf("could not unload the running %s (%v) and it is still loaded, so %s cannot take effect",
					s.Label, err, s.PlistPath())
			}
			fmt.Printf("NOTE: could not unload the running agent (%v); continuing\n", err)
		}
	}

	if err := s.bootstrapLoad(binary); err != nil {
		fmt.Printf("WARNING: could not load agent automatically: %v\n", err)
		fmt.Printf("Run manually: launchctl bootout gui/%d/%s; launchctl bootstrap gui/%d %s\n",
			os.Getuid(), s.Label, os.Getuid(), s.PlistPath())
	} else if alreadyInstalled {
		fmt.Printf("%s restarted with %s\n", s.Label, binary)
	} else {
		fmt.Printf("%s installed and started.\n", s.Label)
	}
	return nil
}

// PlistExists reports whether this service's plist file is already on disk.
func (s *Service) PlistExists() bool {
	_, err := os.Stat(s.PlistPath())
	return err == nil
}

// Uninstall stops and removes the LaunchAgent plist.
func (s *Service) Uninstall() error {
	_ = s.Stop()
	_ = s.bootstrapUnload()
	p := s.PlistPath()
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fmt.Printf("Removed %s\n", p)
	return nil
}

// Start asks launchctl to start the agent.
func (s *Service) Start() error { return launchctl("start", s.Label) }

// Stop asks launchctl to stop the agent.
func (s *Service) Stop() error { return launchctl("stop", s.Label) }

// settleTimeout bounds how long we wait for launchd to finish a bootout or a
// bootstrap. A daemon shutdown has to stop its extension children first, so the
// teardown outlives the bootout call by roughly a second; ten is slack.
// Currently not user-configurable; a var only so tests can shrink it.
var settleTimeout = 10 * time.Second

// launchctlPrint dumps launchd's in-memory definition for a service. A var so
// tests can drive the load/unload logic without a real launchd.
var launchctlPrint = func(serviceTarget string) ([]byte, error) {
	return exec.Command("launchctl", "print", serviceTarget).CombinedOutput()
}

// serviceTarget is the launchd address of this service, e.g.
// gui/501/io.bigband.daemon.
func (s *Service) serviceTarget() string {
	return "gui/" + strconv.Itoa(os.Getuid()) + "/" + s.Label
}

// definition reports whether launchd currently has a definition for this
// service, and which program that definition runs ("" when the dump carries no
// program line — callers must read that as unknown, never as a mismatch).
//
// This is the only trustworthy signal that a load worked: `launchctl bootstrap`
// races a still-running teardown, and the legacy `launchctl load -w` shim exits
// 0 whether or not it loaded anything, so an unverified "success" is how the
// daemon ends up silently absent for hours.
func (s *Service) definition() (program string, loaded bool) {
	out, err := launchctlPrint(s.serviceTarget())
	if err != nil {
		return "", false
	}
	return parseProgram(out), true
}

// loaded reports whether launchd has any definition for this service, without
// regard to which one.
func (s *Service) loaded() bool {
	_, ok := s.definition()
	return ok
}

// parseProgram extracts the "program = /path/to/binary" line from launchctl
// print output. Returns "" when there is none.
func parseProgram(out []byte) string {
	for _, line := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "program = "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// waitFor polls want until it holds or settleTimeout elapses, reporting whether
// it held.
func waitFor(want func() bool) bool {
	deadline := time.Now().Add(settleTimeout)
	for {
		if want() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// bootstrapLoad loads the plist and verifies that launchd ended up running
// wantProgram from it. wantProgram may be "" to skip that half of the check.
func (s *Service) bootstrapLoad(wantProgram string) error {
	uid := strconv.Itoa(os.Getuid())
	var last []byte
	// Retry rather than fire once: bootstrap fails while the previous instance
	// is still booting out, and that window is exactly the one `bigband
	// install` runs in when it restarts a running daemon.
	deadline := time.Now().Add(settleTimeout)
	for {
		out, err := exec.Command("launchctl", "bootstrap", "gui/"+uid, s.PlistPath()).CombinedOutput()
		if err == nil {
			break
		}
		last = out
		if time.Now().After(deadline) {
			out2, err2 := exec.Command("launchctl", "load", "-w", s.PlistPath()).CombinedOutput()
			if err2 != nil {
				return fmt.Errorf("%s; legacy load: %s", last, out2)
			}
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	// "Something is loaded" is not enough: a bootout that never completed
	// leaves the *previous* definition in place under the same label and the
	// same plist path, so it answers yes while the plist we just wrote has had
	// no effect. Compare the program launchd is actually running.
	var program string
	if !waitFor(func() bool {
		p, ok := s.definition()
		program = p
		return ok
	}) {
		return fmt.Errorf("launchd reports no service %s after loading %s: %s", s.Label, s.PlistPath(), last)
	}
	return s.programMismatch(wantProgram, program)
}

// programMismatch reports the case where launchd has a definition loaded but it
// is not the one just written to the plist. Either side being unknown is not a
// mismatch — the check must never fail an install over a missing program line.
func (s *Service) programMismatch(want, got string) error {
	if want == "" || got == "" || want == got {
		return nil
	}
	return fmt.Errorf("launchd still runs %s for %s, not the %s just written to %s — the previous instance never booted out",
		got, s.Label, want, s.PlistPath())
}

func (s *Service) bootstrapUnload() error {
	uid := strconv.Itoa(os.Getuid())
	out, err := exec.Command("launchctl", "bootout", "gui/"+uid+"/"+s.Label).CombinedOutput()
	if err != nil {
		out2, err2 := exec.Command("launchctl", "unload", "-w", s.PlistPath()).CombinedOutput()
		if err2 != nil {
			return fmt.Errorf("%s; legacy unload: %s", out, out2)
		}
	}
	// bootout returns before the service is gone — the daemon still has to stop
	// its extension children. Loading again while that runs is what fails.
	if !waitFor(func() bool { return !s.loaded() }) {
		return fmt.Errorf("service %s still loaded %s after bootout", s.Label, settleTimeout)
	}
	return nil
}

func launchctl(args ...string) error {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl %v: %w\n%s", args, err, out)
	}
	return nil
}

// sortedEnv returns env's keys in deterministic order. Templates iterate the
// map directly via {{range}}, which Go orders by key for string maps; this
// helper keeps the contract explicit so stable plist output is preserved.
func sortedEnv(env map[string]string) map[string]string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]string, len(env))
	for _, k := range keys {
		out[k] = env[k]
	}
	return out
}

// --- Backwards-compatible wrappers for the bigband daemon ---

// BigbandDaemonService returns the Service describing the main bigband daemon.
func BigbandDaemonService() *Service {
	return &Service{
		Label:   bigbandLabel,
		Args:    []string{"daemon"},
		LogPath: paths.DaemonLog(),
	}
}

// Install installs the bigband daemon LaunchAgent. Preserved for callers that
// haven't been updated to the Service API.
func Install(start bool) error {
	_ = start // legacy parameter kept for signature compat; Install always starts.
	return BigbandDaemonService().Install()
}

// Uninstall removes the bigband daemon LaunchAgent.
func Uninstall() error { return BigbandDaemonService().Uninstall() }

// Start asks launchctl to start the bigband daemon.
func Start() error { return BigbandDaemonService().Start() }

// Stop asks launchctl to stop the bigband daemon.
func Stop() error { return BigbandDaemonService().Stop() }
