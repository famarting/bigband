package claudepty

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// trustField is the boolean key claude writes into
// ~/.claude.json -> projects[<absCwd>] once the user accepts its interactive
// workspace-trust dialog. Pre-stamping it as true lets non-interactive callers
// (like claude-pty, which discards the PTY's TUI output) skip the dialog
// entirely; without it, claude blocks invisibly on the dialog until the job
// deadline.
const trustField = "hasTrustDialogAccepted"

// claudeGlobalConfigPath returns ~/.claude.json. Split out so tests can stub
// the home dir via $HOME.
func claudeGlobalConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, ".claude.json"), nil
}

// ensureProjectTrusted records workDir as a trusted project in ~/.claude.json
// so claude doesn't show its workspace-trust dialog on first launch there.
//
// Fast path: if the file already has hasTrustDialogAccepted=true for the
// resolved absolute workDir, returns without writing — so the common case
// (worktree already seen) does no I/O beyond the read, and there's no race
// with claude's own background writes to the same file.
//
// Slow path: take an advisory flock on a sidecar lock file (to serialise with
// concurrent bigband runs marking different worktrees), read+modify+write
// ~/.claude.json via temp file + rename. Unknown keys at every level are
// preserved.
func ensureProjectTrusted(workDir string) error {
	abs, err := filepath.Abs(workDir)
	if err != nil {
		return fmt.Errorf("resolve workdir: %w", err)
	}
	cfgPath, err := claudeGlobalConfigPath()
	if err != nil {
		return err
	}

	if already, err := isTrusted(cfgPath, abs); err != nil {
		return err
	} else if already {
		return nil
	}

	release, err := lockTrustFile(cfgPath)
	if err != nil {
		return err
	}
	defer release()

	// Re-check under the lock: another job may have written it while we were
	// blocked acquiring the lock.
	if already, err := isTrusted(cfgPath, abs); err != nil {
		return err
	} else if already {
		return nil
	}

	cfg, err := readClaudeConfig(cfgPath)
	if err != nil {
		return err
	}
	setProjectTrusted(cfg, abs)
	return writeClaudeConfigAtomic(cfgPath, cfg)
}

// isTrusted returns true when ~/.claude.json already records absWorkDir with
// hasTrustDialogAccepted=true. A missing or unreadable file is treated as
// "not trusted" without erroring — the caller will create/repair it.
func isTrusted(cfgPath, absWorkDir string) (bool, error) {
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("read %s: %w", cfgPath, err)
	}
	if len(data) == 0 {
		return false, nil
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		return false, nil // malformed file → we'll overwrite under the lock
	}
	projects, _ := cfg["projects"].(map[string]any)
	entry, _ := projects[absWorkDir].(map[string]any)
	trusted, _ := entry[trustField].(bool)
	return trusted, nil
}

// readClaudeConfig parses ~/.claude.json into a generic map so unknown keys
// (the file has dozens of dynamic fields claude rewrites) are preserved
// across the round-trip. A missing or empty file yields an empty map.
func readClaudeConfig(cfgPath string) (map[string]any, error) {
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", cfgPath, err)
	}
	if len(data) == 0 {
		return map[string]any{}, nil
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", cfgPath, err)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return cfg, nil
}

// setProjectTrusted ensures cfg.projects[absWorkDir].hasTrustDialogAccepted is
// true, creating any missing nested maps. Other fields on the same project
// entry are left intact.
func setProjectTrusted(cfg map[string]any, absWorkDir string) {
	projects, _ := cfg["projects"].(map[string]any)
	if projects == nil {
		projects = map[string]any{}
		cfg["projects"] = projects
	}
	entry, _ := projects[absWorkDir].(map[string]any)
	if entry == nil {
		entry = map[string]any{}
		projects[absWorkDir] = entry
	}
	entry[trustField] = true
}

// writeClaudeConfigAtomic serialises cfg with 2-space indent (matching the
// format claude itself uses) and replaces ~/.claude.json via temp file +
// rename so a crashed write never leaves a half-written config behind.
func writeClaudeConfigAtomic(cfgPath string, cfg map[string]any) error {
	dir := filepath.Dir(cfgPath)
	tmp, err := os.CreateTemp(dir, ".claude.json.bigband.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(cfg); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpPath, cfgPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename to %s: %w", cfgPath, err)
	}
	return nil
}

// lockTrustFile acquires an exclusive flock on a sidecar lock file next to
// ~/.claude.json so concurrent bigband jobs serialise their read-modify-write
// of the config. claude itself does not take this lock, so this only protects
// us from ourselves — but that's enough, because once trust is recorded the
// fast path skips the write entirely on subsequent runs.
func lockTrustFile(cfgPath string) (func(), error) {
	lockPath := cfgPath + ".bigband.lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("flock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
