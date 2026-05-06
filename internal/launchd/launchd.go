// Package launchd generates and manages a macOS LaunchAgent plist for bigband.
package launchd

import (
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"

	"github.com/famarting/bigband/internal/paths"
)

const label = "io.bigband.daemon"

// xmlEscape escapes a string for safe insertion into an XML text node. The
// values rendered into the plist (HOME, PATH, BinaryPath, log path) come from
// the user's environment and may legitimately contain `&`, `<`, or `>`; without
// escaping, those characters would produce a malformed plist.
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
        <string>{{xml .BinaryPath}}</string>
        <string>daemon</string>
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
        <key>HOME</key>
        <string>{{xml .Home}}</string>
        <key>PATH</key>
        <string>{{xml .Path}}</string>
    </dict>
</dict>
</plist>
`))

type plistData struct {
	Label      string
	BinaryPath string
	LogPath    string
	Home       string
	Path       string
}

func plistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist")
}

// Install writes the plist and starts (or restarts) the agent.
// If the agent is already running it is kicked so the new binary takes effect.
func Install(start bool) error {
	binary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot determine binary path: %w", err)
	}
	home, _ := os.UserHomeDir()

	if err := paths.EnsureDirs(); err != nil {
		return err
	}

	alreadyInstalled := plistExists()

	data := plistData{
		Label:      label,
		BinaryPath: binary,
		LogPath:    paths.DaemonLog(),
		Home:       home,
		Path:       os.Getenv("PATH"),
	}

	f, err := os.Create(plistPath())
	if err != nil {
		return fmt.Errorf("creating plist: %w", err)
	}
	if err := plistTemplate.Execute(f, data); err != nil {
		f.Close()
		return fmt.Errorf("writing plist: %w", err)
	}
	f.Close()

	fmt.Printf("Wrote %s\n", plistPath())

	if alreadyInstalled {
		// Kick the running instance so the new binary takes effect immediately.
		uid := strconv.Itoa(os.Getuid())
		out, err := exec.Command("launchctl", "kickstart", "-k", "gui/"+uid+"/"+label).CombinedOutput()
		if err != nil {
			fmt.Printf("WARNING: kickstart failed: %s\n", out)
		} else {
			fmt.Println("Daemon restarted with new binary.")
		}
		return nil
	}

	if err := bootstrapLoad(); err != nil {
		fmt.Printf("WARNING: could not load agent automatically: %v\n", err)
		fmt.Printf("Run manually: launchctl load -w %s\n", plistPath())
	} else {
		fmt.Println("Daemon installed and started.")
	}
	return nil
}

func plistExists() bool {
	_, err := os.Stat(plistPath())
	return err == nil
}

// Uninstall stops and removes the LaunchAgent.
func Uninstall() error {
	_ = Stop()
	_ = bootstrapUnload()
	p := plistPath()
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Printf("Removed %s\n", p)
	return nil
}

// Start asks launchctl to start the agent.
func Start() error {
	return launchctl("start", label)
}

// Stop asks launchctl to stop the agent.
func Stop() error {
	return launchctl("stop", label)
}

func launchctl(args ...string) error {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl %v: %w\n%s", args, err, out)
	}
	return nil
}

func bootstrapLoad() error {
	uid := strconv.Itoa(os.Getuid())
	out, err := exec.Command("launchctl", "bootstrap", "gui/"+uid, plistPath()).CombinedOutput()
	if err != nil {
		// Fallback to legacy load.
		out2, err2 := exec.Command("launchctl", "load", "-w", plistPath()).CombinedOutput()
		if err2 != nil {
			return fmt.Errorf("%s; legacy load: %s", out, out2)
		}
	}
	return nil
}

func bootstrapUnload() error {
	uid := strconv.Itoa(os.Getuid())
	out, err := exec.Command("launchctl", "bootout", "gui/"+uid+"/"+label).CombinedOutput()
	if err != nil {
		out2, err2 := exec.Command("launchctl", "unload", "-w", plistPath()).CombinedOutput()
		if err2 != nil {
			return fmt.Errorf("%s; legacy unload: %s", out, out2)
		}
	}
	return nil
}
