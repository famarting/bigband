package worktree

import (
	"path/filepath"
	"testing"
)

func TestDefaultPath(t *testing.T) {
	got := DefaultPath("/repos/myrepo", "my-task")
	want := "/repos/myrepo-bb-my-task"
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

func TestLooksLikeBigbandWorktree_Valid(t *testing.T) {
	cases := []string{
		"/parent/myrepo-bb-my-task",
		"/parent/myrepo-bb-oneoff-abc123",
		"relative-bb-task", // relative paths also pass via Abs
	}
	for _, c := range cases {
		if !LooksLikeBigbandWorktree(c) {
			t.Errorf("LooksLikeBigbandWorktree(%q) = false, want true", c)
		}
	}
}

func TestLooksLikeBigbandWorktree_Invalid(t *testing.T) {
	cases := []string{
		"/parent/myrepo-notbb-task",
		"/parent/myrepo",
		"/",
		"",
		"/parent/something-else",
	}
	for _, c := range cases {
		if LooksLikeBigbandWorktree(c) {
			t.Errorf("LooksLikeBigbandWorktree(%q) = true, want false", c)
		}
	}
}

func TestGuardWorktreePath_Valid(t *testing.T) {
	repoRoot := "/parent/myrepo"
	wtPath := "/parent/myrepo-bb-my-task"
	if err := guardWorktreePath(repoRoot, wtPath); err != nil {
		t.Errorf("guardWorktreePath(%q, %q) unexpected error: %v", repoRoot, wtPath, err)
	}
}

func TestGuardWorktreePath_NotSibling(t *testing.T) {
	repoRoot := "/parent/myrepo"
	wtPath := "/other/myrepo-bb-my-task"
	if err := guardWorktreePath(repoRoot, wtPath); err == nil {
		t.Error("expected error for non-sibling path, got nil")
	}
}

func TestGuardWorktreePath_WrongPrefix(t *testing.T) {
	repoRoot := "/parent/myrepo"
	wtPath := "/parent/other-bb-task" // not prefixed with repoBase
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
	wtPath := "/repos/myrepo-bb-task"
	got := SubDir(repoRoot, wtPath, repoRoot)
	if got != wtPath {
		t.Errorf("SubDir(%q, %q, %q) = %q, want %q", repoRoot, wtPath, repoRoot, got, wtPath)
	}
}

func TestSubDir_SubDirectory(t *testing.T) {
	repoRoot := "/repos/myrepo"
	wtPath := "/repos/myrepo-bb-task"
	taskFolder := "/repos/myrepo/services/api"
	got := SubDir(repoRoot, wtPath, taskFolder)
	want := "/repos/myrepo-bb-task/services/api"
	if got != want {
		t.Errorf("SubDir = %q, want %q", got, want)
	}
}
