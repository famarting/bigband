package claudepty

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// claudeProjectDir returns the Claude session storage directory for cwd.
// Claude writes durable session JSONL files to
//
//	~/.claude/projects/<encoded-cwd>/<session-uuid>.jsonl
//
// where the encoding replaces "/", "." and "_" in the absolute path with
// "-". This mirrors what the CLI itself does; it's how we know where to
// tail. Missing the "_" case caused empty job logs whenever a workspace
// path contained an underscore (e.g. feature-worktrees/foo__bar).
func claudeProjectDir(cwd string) (string, error) {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("resolve cwd: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	encoded := strings.NewReplacer("/", "-", ".", "-", "_", "-").Replace(abs)
	return filepath.Join(home, ".claude", "projects", encoded), nil
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-5][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)

// validateSessionID rejects values that are not valid v1–v5 UUIDs or that
// contain path separators. Callers join the result into a filesystem path, so
// any escape here turns into directory traversal.
func validateSessionID(id string) error {
	if strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		return errors.New("session id must not contain path separators")
	}
	if !uuidRe.MatchString(id) {
		return errors.New("session id must be a UUID")
	}
	return nil
}

// sessionFilePath returns the durable JSONL path for sessionID in cwd, after
// validating that the id is a UUID and that the joined path stays inside the
// project directory.
func sessionFilePath(cwd, sessionID string) (string, error) {
	if err := validateSessionID(sessionID); err != nil {
		return "", err
	}
	dir, err := claudeProjectDir(cwd)
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, sessionID+".jsonl")
	rel, err := filepath.Rel(dir, p)
	if err != nil || rel == "" || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return "", errors.New("resolved session path escapes project directory")
	}
	return p, nil
}

// fileSize returns the current size of path, or 0 if it doesn't exist.
// Any other error is returned. Used to snapshot the tail-from offset before
// spawning claude so we only see records produced by this turn.
func fileSize(path string) (int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	return fi.Size(), nil
}
