package worktree

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultPath(t *testing.T) {
	got := DefaultPath("/repos/myrepo", "my-job")
	want := "/repos/myrepo-bb-my-job"
	if got != want {
		t.Errorf("DefaultPath = %q, want %q", got, want)
	}
}

func TestDefaultPath_NestedRepo(t *testing.T) {
	got := DefaultPath("/home/user/work/myrepo", "deploy")
	want := "/home/user/work/myrepo-bb-deploy"
	if got != want {
		t.Errorf("DefaultPath = %q, want %q", got, want)
	}
}

func TestGuardWorktreePath_Valid(t *testing.T) {
	repoRoot := "/parent/myrepo"
	wtPath := "/parent/myrepo-bb-my-job"
	if err := guardWorktreePath(repoRoot, wtPath); err != nil {
		t.Errorf("guardWorktreePath(%q, %q) unexpected error: %v", repoRoot, wtPath, err)
	}
}

func TestGuardWorktreePath_NotSibling(t *testing.T) {
	repoRoot := "/parent/myrepo"
	wtPath := "/other/myrepo-bb-my-job"
	if err := guardWorktreePath(repoRoot, wtPath); err == nil {
		t.Error("expected error for non-sibling path, got nil")
	}
}

func TestGuardWorktreePath_WrongPrefix(t *testing.T) {
	repoRoot := "/parent/myrepo"
	wtPath := "/parent/other-bb-job" // not prefixed with repoBase
	if err := guardWorktreePath(repoRoot, wtPath); err == nil {
		t.Error("expected error for wrong prefix, got nil")
	}
}

func TestGuardWorktreePath_RejectsRepoRoot(t *testing.T) {
	repoRoot := "/parent/myrepo"
	if err := guardWorktreePath(repoRoot, repoRoot); err == nil {
		t.Error("expected error for repo root itself, got nil")
	}
}

func TestGuardWorktreePath_RejectsParent(t *testing.T) {
	repoRoot := "/parent/myrepo"
	if err := guardWorktreePath(repoRoot, filepath.Dir(repoRoot)); err == nil {
		t.Error("expected error for parent directory, got nil")
	}
}

func TestSubDir_RepoRoot(t *testing.T) {
	repoRoot := "/repos/myrepo"
	wtPath := "/repos/myrepo-bb-job"
	got := SubDir(repoRoot, wtPath, repoRoot)
	if got != wtPath {
		t.Errorf("SubDir(%q, %q, %q) = %q, want %q", repoRoot, wtPath, repoRoot, got, wtPath)
	}
}

func TestSubDir_SubDirectory(t *testing.T) {
	repoRoot := "/repos/myrepo"
	wtPath := "/repos/myrepo-bb-job"
	jobFolder := "/repos/myrepo/services/api"
	got := SubDir(repoRoot, wtPath, jobFolder)
	want := "/repos/myrepo-bb-job/services/api"
	if got != want {
		t.Errorf("SubDir = %q, want %q", got, want)
	}
}

func TestSubDir_SymlinkedJobFolder(t *testing.T) {
	// A job configured with a symlinked spelling of the repo root must still
	// run at the worktree root. Before both ends were resolved, the relative
	// path climbed out of the worktree and landed back in the original
	// checkout — which exists, so nothing caught it.
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	repoRoot := filepath.Join(tmp, "src", "repo")
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(tmp, "shortcut")
	if err := os.Symlink(repoRoot, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	wtPath := filepath.Join(tmp, "src", "repo-bb-job")

	if got := SubDir(repoRoot, wtPath, link); got != wtPath {
		t.Errorf("SubDir with symlinked folder = %q, want %q", got, wtPath)
	}
}

func TestSubDir_SymlinkedSubdirectory(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	repoRoot := filepath.Join(tmp, "src", "repo")
	if err := os.MkdirAll(filepath.Join(repoRoot, "services", "api"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(tmp, "shortcut")
	if err := os.Symlink(repoRoot, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	wtPath := filepath.Join(tmp, "src", "repo-bb-job")

	want := filepath.Join(wtPath, "services", "api")
	if got := SubDir(repoRoot, wtPath, filepath.Join(link, "services", "api")); got != want {
		t.Errorf("SubDir = %q, want %q", got, want)
	}
}
