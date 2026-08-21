// Package worktree manages git worktrees created for bigband job runs.
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

// OriginPath returns the absolute path of the *primary* working tree containing
// folder — i.e. the original repository, even if folder is inside a linked
// worktree. For non-git folders it returns the absolute, symlink-resolved path
// of folder itself. The returned path is suitable for matching against an
// allowlist of source-of-truth roots.
//
// Implementation: `git rev-parse --git-common-dir` reports the .git directory
// of the *primary* worktree (linked worktrees point back at it via the
// commondir file). The parent of that directory is the primary working tree.
// If the path doesn't resolve as a git repo, fall back to the resolved folder.
func OriginPath(folder string) (string, error) {
	abs, err := filepath.Abs(folder)
	if err != nil {
		return "", err
	}
	// EvalSymlinks fails if the path doesn't exist; in that case we still want
	// a useful answer for the allowlist check at submit time, so fall back to
	// the lexically-cleaned absolute path.
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		resolved = abs
	}
	out, err := exec.Command("git", "-C", resolved, "rev-parse", "--git-common-dir").Output()
	if err != nil {
		return resolved, nil
	}
	commonDir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(resolved, commonDir)
	}
	primary := filepath.Dir(commonDir)
	if primaryReal, err := filepath.EvalSymlinks(primary); err == nil {
		primary = primaryReal
	}
	return primary, nil
}

// DefaultPath returns the conventional worktree path for a job:
// a sibling of the repo root named <repo>-bb-<job>.
func DefaultPath(repoRoot, jobName string) string {
	parent := filepath.Dir(repoRoot)
	base := filepath.Base(repoRoot)
	return filepath.Join(parent, base+"-bb-"+jobName)
}

// SubDir returns the path inside wtPath that corresponds to jobFolder.
// If jobFolder is the repo root itself, wtPath is returned unchanged.
//
// Both ends are resolved through symlinks first. repoRoot comes from `git
// rev-parse --show-toplevel`, which is always physical, while jobFolder is
// whatever string the job was configured with — so a folder reached through a
// symlink (~/shortcut/repo) produced a relative path climbing out of the repo
// and back down the other spelling. That path resolves to the original
// checkout, which exists, so the caller's existence check passed and the job
// silently ran in the user's own working tree instead of its worktree.
func SubDir(repoRoot, wtPath, jobFolder string) string {
	root := resolved(repoRoot)
	folder := resolved(jobFolder)
	rel, err := filepath.Rel(root, folder)
	if err != nil || rel == "." {
		return wtPath
	}
	// Anything still escaping the repo root is not a subdirectory of it, and
	// the only sane run dir left is the worktree root — never a path outside
	// the worktree.
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return wtPath
	}
	return filepath.Join(wtPath, rel)
}

// resolved expands symlinks in path, falling back to path itself when it does
// not exist yet.
func resolved(path string) string {
	if p, err := filepath.EvalSymlinks(path); err == nil {
		return p
	}
	return path
}

// BranchName returns the git branch name bigband uses for a job worktree.
func BranchName(jobName string) string {
	return "bigband/" + jobName
}

// CreateOrReplace creates a worktree at wtPath on a dedicated branch
// bigband/<jobName> reset to HEAD. Any existing worktree or branch at that
// name is removed first (stale from a crashed run).
func CreateOrReplace(repoRoot, wtPath, jobName string, w io.Writer) error {
	// Clean up any stale worktree at this path.
	if _, err := os.Stat(wtPath); err == nil {
		_ = Remove(repoRoot, wtPath)
	}
	// Delete stale branch if it exists.
	branch := BranchName(jobName)
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
//
// The fallback `os.RemoveAll(wtPath)` is guarded: wtPath must be under
// repoRoot's parent (where DefaultPath lives) and must look like a bigband
// worktree (contains "-bb-" in its base name). A corrupted state.json or a
// hand-edit cannot weaponise this path into deleting an unrelated directory.
func Remove(repoRoot, wtPath string) error {
	// Resolve the branch name before the worktree disappears.
	branchOut, _ := exec.Command("git", "-C", wtPath, "rev-parse", "--abbrev-ref", "HEAD").Output()
	branch := strings.TrimSpace(string(branchOut))

	cmd := exec.Command("git", "-C", repoRoot, "worktree", "remove", "--force", wtPath)
	if err := cmd.Run(); err != nil {
		if guardErr := guardWorktreePath(repoRoot, wtPath); guardErr != nil {
			return fmt.Errorf("refusing to recursively delete %s: %w", wtPath, guardErr)
		}
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

// guardWorktreePath returns nil only when wtPath is safe to recursively delete:
// it must be a sibling of the repo root (where DefaultPath places it) and its
// basename must follow the bigband naming convention (<repo>-bb-<job>). This
// prevents a corrupted or hand-edited state.json from steering os.RemoveAll
// at an unrelated directory.
func guardWorktreePath(repoRoot, wtPath string) error {
	abs, err := filepath.Abs(wtPath)
	if err != nil {
		return err
	}
	rootAbs, err := filepath.Abs(repoRoot)
	if err != nil {
		return err
	}
	expectedParent := filepath.Dir(rootAbs)
	if filepath.Dir(abs) != expectedParent {
		return fmt.Errorf("path is not a sibling of the repo root (expected parent %s, got %s)", expectedParent, filepath.Dir(abs))
	}
	base := filepath.Base(abs)
	repoBase := filepath.Base(rootAbs)
	if !strings.HasPrefix(base, repoBase+"-bb-") {
		return fmt.Errorf("path basename %q does not match bigband worktree convention (%s-bb-<job>)", base, repoBase)
	}
	if abs == "/" || abs == rootAbs || abs == expectedParent {
		return fmt.Errorf("path %s is the root, the repo, or its parent — refusing", abs)
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
