package claudepty

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// physicalPath resolves dir the way the kernel reports it once a process has
// chdir'd there: absolute, with every symlink on the way expanded. Claude keys
// both its project directory and its trust entry off its own process cwd, so
// anything we encode from the configured string instead points somewhere claude
// never writes whenever a job's folder is reached through a symlink (say
// ~/shortcut/repo -> /Users/me/src/repo). That cost three-hour jobs: the tail
// waited out the whole deadline on a file that could not appear.
//
// Falls back to the lexical absolute path when dir does not exist yet, which is
// what callers want for a worktree they are about to create.
func physicalPath(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("absolute path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		// Not existing yet is the expected case and stays lexical. Anything
		// else (a parent we may not traverse, a component that is not a
		// directory) has to be loud: a silent lexical fallback there is exactly
		// the wrong path this function exists to stop producing, and for the
		// trust stamp there is no later fallback to catch it.
		if errors.Is(err, os.ErrNotExist) {
			return abs, nil
		}
		return "", fmt.Errorf("resolve symlinks in %s: %w", abs, err)
	}
	return resolved, nil
}

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
	abs, err := physicalPath(cwd)
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

// sessionFileGrace bounds how long awaitSessionFile waits for the transcript to
// show up before it gives up on the run. Claude writes its first records within
// seconds of starting; five minutes is slack for a cold start behind slow MCP
// servers, and still far below the multi-hour deadlines these jobs carry.
const sessionFileGrace = 5 * time.Minute

// sessionScanInterval throttles the by-session-id search. Stat'ing the derived
// path is one syscall, but findSessionFile readdirs ~/.claude/projects and
// lstats every project directory in it — a few hundred on a machine that has
// been used for a while — so it runs on a human interval rather than the tail's
// poll interval.
const sessionScanInterval = time.Second

// findSessionFile looks for sessionID's transcript under any project directory
// in ~/.claude/projects. claudeProjectDir mirrors an encoding internal to the
// claude CLI, so it is a guess that can go stale; searching by session id is
// the ground truth, and keeps a wrong guess costing one banner line instead of
// the entire job.
func findSessionFile(sessionID string) (string, bool) {
	if err := validateSessionID(sessionID); err != nil {
		return "", false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	matches, err := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", sessionID+".jsonl"))
	if err != nil || len(matches) == 0 {
		return "", false
	}
	return matches[0], true
}

// awaitSessionFile blocks until the transcript for sessionID exists, returning
// the path to tail. It prefers want (the derived path) and falls back to
// wherever the session id actually landed, reporting the discrepancy through
// notify so the log says why.
//
// It gives up after grace with an error rather than returning want and letting
// the tail poll a file that will never exist: an unreachable transcript used to
// be indistinguishable from a long-running turn, so the job burned its whole
// deadline and reported a timeout for a run that had already finished.
// childDead short-circuits the wait — the tail reports that case better than we
// can here.
func awaitSessionFile(ctx context.Context, want, sessionID string, grace time.Duration, childDead func() bool, notify func(found string)) (string, error) {
	deadline := time.Now().Add(grace)
	var nextScan time.Time // zero: scan on the first pass
	for {
		if _, err := os.Stat(want); err == nil {
			return want, nil
		}
		if now := time.Now(); !now.Before(nextScan) {
			nextScan = now.Add(sessionScanInterval)
			if found, ok := findSessionFile(sessionID); ok {
				if notify != nil {
					notify(found)
				}
				return found, nil
			}
		}
		if childDead != nil && childDead() {
			return want, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("no transcript for session %s under ~/.claude/projects after %s (expected %s)", sessionID, grace, want)
		}
		select {
		case <-time.After(pollInterval):
		case <-ctx.Done():
			return want, nil
		}
	}
}
