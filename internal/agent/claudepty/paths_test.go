package claudepty

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
