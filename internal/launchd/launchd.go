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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/template"

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

	if alreadyInstalled {
		uid := strconv.Itoa(os.Getuid())
		out, err := exec.Command("launchctl", "kickstart", "-k", "gui/"+uid+"/"+s.Label).CombinedOutput()
		if err != nil {
			fmt.Printf("WARNING: kickstart failed: %s\n", out)
		} else {
			fmt.Printf("%s restarted with new binary.\n", s.Label)
		}
		return nil
	}

	if err := s.bootstrapLoad(); err != nil {
		fmt.Printf("WARNING: could not load agent automatically: %v\n", err)
		fmt.Printf("Run manually: launchctl load -w %s\n", s.PlistPath())
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
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Printf("Removed %s\n", p)
	return nil
}

// Start asks launchctl to start the agent.
func (s *Service) Start() error { return launchctl("start", s.Label) }

// Stop asks launchctl to stop the agent.
func (s *Service) Stop() error { return launchctl("stop", s.Label) }

func (s *Service) bootstrapLoad() error {
	uid := strconv.Itoa(os.Getuid())
	out, err := exec.Command("launchctl", "bootstrap", "gui/"+uid, s.PlistPath()).CombinedOutput()
	if err != nil {
		out2, err2 := exec.Command("launchctl", "load", "-w", s.PlistPath()).CombinedOutput()
		if err2 != nil {
			return fmt.Errorf("%s; legacy load: %s", out, out2)
		}
	}
	return nil
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
