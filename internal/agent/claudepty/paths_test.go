package claudepty

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClaudeProjectDir_EncodesSlashDotUnderscore(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home dir: %v", err)
	}
	tests := []struct {
		name string
		cwd  string
		want string
	}{
		{
			"plain path",
			"/home/user/work/projects/example",
			filepath.Join(home, ".claude", "projects", "-home-user-work-projects-example"),
		},
		{
			"underscore in folder name",
			"/home/user/projects/feature-worktrees/example-docs__sub-section-restructure",
			filepath.Join(home, ".claude", "projects", "-home-user-projects-feature-worktrees-example-docs--sub-section-restructure"),
		},
		{
			"dot in folder name",
			"/home/user/.example-tool",
			filepath.Join(home, ".claude", "projects", "-home-user--example-tool"),
		},
		{
			"mixed dot and underscore",
			"/home/user/.config/my_app",
			filepath.Join(home, ".claude", "projects", "-home-user--config-my-app"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := claudeProjectDir(tt.cwd)
			if err != nil {
				t.Fatalf("claudeProjectDir: %v", err)
			}
			if got != tt.want {
				t.Errorf("claudeProjectDir(%q)\n got=  %q\n want= %q", tt.cwd, got, tt.want)
			}
			if !strings.ContainsRune(filepath.Base(got), '-') {
				t.Errorf("encoded dir should contain '-', got %q", got)
			}
		})
	}
}

func TestClaudeProjectDir_ResolvesSymlinkedWorkdir(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home dir: %v", err)
	}
	// t.TempDir() itself sits behind a symlink on macOS (/var -> /private/var),
	// so the expectation has to come from the resolved real path, not the
	// string we were handed.
	real, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	workDir := filepath.Join(real, "repo")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(real, "shortcut")
	if err := os.Symlink(workDir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	viaLink, err := claudeProjectDir(link)
	if err != nil {
		t.Fatalf("claudeProjectDir(link): %v", err)
	}
	direct, err := claudeProjectDir(workDir)
	if err != nil {
		t.Fatalf("claudeProjectDir(workDir): %v", err)
	}
	if viaLink != direct {
		t.Errorf("symlinked workdir must encode to the path claude writes to\n via link= %q\n direct=  %q", viaLink, direct)
	}
	if !strings.HasPrefix(viaLink, filepath.Join(home, ".claude", "projects")) {
		t.Errorf("unexpected project dir %q", viaLink)
	}
}

func TestClaudeProjectDir_MissingDirStaysLexical(t *testing.T) {
	// A worktree bigband is about to create does not exist yet; EvalSymlinks
	// fails on it and we must still return the encoded absolute path.
	got, err := claudeProjectDir("/no/such/dir/anywhere")
	if err != nil {
		t.Fatalf("claudeProjectDir: %v", err)
	}
	if !strings.HasSuffix(got, "-no-such-dir-anywhere") {
		t.Errorf("got %q, want it to end in -no-such-dir-anywhere", got)
	}
}

func TestFindSessionFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	const id = "56197767-a21d-4d82-931b-f35947413d49"
	dir := filepath.Join(home, ".claude", "projects", "-somewhere-else")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	want := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(want, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, ok := findSessionFile(id)
	if !ok || got != want {
		t.Errorf("findSessionFile(%q) = (%q, %v), want (%q, true)", id, got, ok, want)
	}
	if _, ok := findSessionFile("2ba7b810-9dad-11d1-80b4-00c04fd430c8"); ok {
		t.Error("findSessionFile found a session that was never written")
	}
	if _, ok := findSessionFile("../../escape"); ok {
		t.Error("findSessionFile accepted a non-UUID id")
	}
}

func TestAwaitSessionFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	const id = "73f53232-c02d-4e06-be53-ed77c8ea937f"
	alive := func() bool { return false }

	t.Run("derived path wins", func(t *testing.T) {
		want := filepath.Join(home, id+".jsonl")
		if err := os.WriteFile(want, []byte("{}\n"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		t.Cleanup(func() { os.Remove(want) })
		got, err := awaitSessionFile(t.Context(), want, id, time.Second, alive, func(string) {
			t.Error("notify called although the derived path existed")
		})
		if err != nil || got != want {
			t.Errorf("awaitSessionFile = (%q, %v), want (%q, nil)", got, err, want)
		}
	})

	t.Run("falls back to where the session landed", func(t *testing.T) {
		dir := filepath.Join(home, ".claude", "projects", "-elsewhere")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		actual := filepath.Join(dir, id+".jsonl")
		if err := os.WriteFile(actual, []byte("{}\n"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		t.Cleanup(func() { os.Remove(actual) })

		var notified string
		derived := filepath.Join(home, ".claude", "projects", "-wrong-guess", id+".jsonl")
		got, err := awaitSessionFile(t.Context(), derived, id, time.Second, alive, func(f string) { notified = f })
		if err != nil || got != actual {
			t.Errorf("awaitSessionFile = (%q, %v), want (%q, nil)", got, err, actual)
		}
		if notified != actual {
			t.Errorf("notify got %q, want %q", notified, actual)
		}
	})

	t.Run("gives up instead of polling to the deadline", func(t *testing.T) {
		derived := filepath.Join(home, ".claude", "projects", "-nothing-here", id+".jsonl")
		start := time.Now()
		if _, err := awaitSessionFile(t.Context(), derived, id, 150*time.Millisecond, alive, nil); err == nil {
			t.Fatal("expected an error when no transcript ever appears")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("waited %s, grace was 150ms", elapsed)
		}
	})
}

func TestPhysicalPath_UnresolvableIsAnError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root traverses unreadable directories")
	}
	// A parent we cannot traverse is not "does not exist": returning the lexical
	// path here would hand claudeProjectDir and the trust stamp the very
	// mis-spelled path this resolution exists to prevent, with nothing left to
	// catch it.
	parent := filepath.Join(t.TempDir(), "closed")
	if err := os.Mkdir(parent, 0o000); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })

	if got, err := physicalPath(filepath.Join(parent, "repo")); err == nil {
		t.Errorf("physicalPath = (%q, nil), want an error for an untraversable parent", got)
	}
}
