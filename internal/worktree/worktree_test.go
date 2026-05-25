package worktree

import (
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
