// Package worktree manages git worktrees created for bigband task runs.
package worktree

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// RepoRoot returns the absolute path of the git repository that contains dir.
func RepoRoot(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("%q is not in a git repository", dir)
	}
	return strings.TrimSpace(string(out)), nil
}

// DefaultPath returns the conventional worktree path for a task:
// a sibling of the repo root named <repo>-bb-<task>.
func DefaultPath(repoRoot, taskName string) string {
	parent := filepath.Dir(repoRoot)
	base := filepath.Base(repoRoot)
	return filepath.Join(parent, base+"-bb-"+taskName)
}

// SubDir returns the path inside wtPath that corresponds to taskFolder.
// If taskFolder is the repo root itself, wtPath is returned unchanged.
func SubDir(repoRoot, wtPath, taskFolder string) string {
	rel, err := filepath.Rel(repoRoot, taskFolder)
	if err != nil || rel == "." {
		return wtPath
	}
	return filepath.Join(wtPath, rel)
}

// BranchName returns the git branch name bigband uses for a task worktree.
func BranchName(taskName string) string {
	return "bigband/" + taskName
}

// CreateOrReplace creates a worktree at wtPath on a dedicated branch
// bigband/<taskName> reset to HEAD. Any existing worktree or branch at that
// name is removed first (stale from a crashed run).
func CreateOrReplace(repoRoot, wtPath, taskName string, w io.Writer) error {
	// Clean up any stale worktree at this path.
	if _, err := os.Stat(wtPath); err == nil {
		_ = Remove(repoRoot, wtPath)
	}
	// Delete stale branch if it exists.
	branch := BranchName(taskName)
	_ = exec.Command("git", "-C", repoRoot, "branch", "-D", branch).Run()

	cmd := exec.Command("git", "-C", repoRoot, "worktree", "add", "-b", branch, wtPath, "HEAD")
	cmd.Stdout = w
	cmd.Stderr = w
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git worktree add: %w", err)
	}
	return nil
}

// Remove removes the worktree at wtPath and its associated bigband branch.
// It tries `git worktree remove --force` first; on failure it falls back to
// os.RemoveAll + `git worktree prune`.
func Remove(repoRoot, wtPath string) error {
	// Resolve the branch name before the worktree disappears.
	branchOut, _ := exec.Command("git", "-C", wtPath, "rev-parse", "--abbrev-ref", "HEAD").Output()
	branch := strings.TrimSpace(string(branchOut))

	cmd := exec.Command("git", "-C", repoRoot, "worktree", "remove", "--force", wtPath)
	if err := cmd.Run(); err != nil {
		if rmErr := os.RemoveAll(wtPath); rmErr != nil {
			return fmt.Errorf("rm -rf %s: %w", wtPath, rmErr)
		}
		_ = exec.Command("git", "-C", repoRoot, "worktree", "prune").Run()
	}

	// Delete the branch if it's a bigband-managed one.
	if strings.HasPrefix(branch, "bigband/") {
		_ = exec.Command("git", "-C", repoRoot, "branch", "-D", branch).Run()
	}
	return nil
}

// Move relocates the worktree via `git worktree move`.
func Move(repoRoot, from, to string) error {
	out, err := exec.Command("git", "-C", repoRoot, "worktree", "move", from, to).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git worktree move: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
