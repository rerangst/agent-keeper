package restore

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func pane(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "panes", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestMatchDialogFixtures(t *testing.T) {
	cases := []struct {
		file string
		name string
		keys []string
	}{
		{"trust-folder.txt", "trust_folder", []string{"Down", "Enter"}},
		{"bypass-permissions.txt", "bypass_permissions", []string{"Down", "Enter"}},
		{"large-session-resume.txt", "large_session_resume", []string{"Down", "Enter"}},
		{"shell-prompt-then-trust.txt", "trust_folder", []string{"Down", "Enter"}},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			d, keys, ok := MatchDialog(pane(t, c.file))
			if !ok || d.Name != c.name {
				t.Fatalf("MatchDialog = %+v, %v; want %s", d, ok, c.name)
			}
			if !slices.Equal(keys, c.keys) {
				t.Fatalf("keys = %v, want %v", keys, c.keys)
			}
		})
	}
}

func TestMenuKeysStructure(t *testing.T) {
	const label = "Target"
	cases := []struct {
		name   string
		screen string
		keys   []string // nil = not recognised as a menu
	}{
		{"cursor on target", "    A\n  ❯ Target", []string{"Enter"}},
		{"cursor below", "    Target\n  ❯ Other", []string{"Up", "Enter"}},
		{"two rows down", "  ❯ A\n    B\n    Target", []string{"Down", "Down", "Enter"}},
		{"ascii cursor", "  > A\n    Target", []string{"Down", "Enter"}},
		{"boxed rows", "│ ❯ A        │\n│   Target   │", []string{"Down", "Enter"}},
		{"REPL cursor after a border is ignored", "  ❯ A\n    B\n    Target\n─────\n❯ ", []string{"Down", "Down", "Enter"}},
		{"REPL cursor after a blank row is ignored", "    A\n    Target\n\n❯ typing", nil},
		{"no cursor at all", "    A\n    Target", nil},
		{"label only inside a sentence", "  ❯ please pick Target now", nil},
		{"history line quoting the label", "● I chose Target earlier\n❯ ", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			keys, ok := menuKeys(strings.Split(c.screen, "\n"), label)
			if c.keys == nil {
				if ok {
					t.Fatalf("recognised as menu: keys %v", keys)
				}
				return
			}
			if !ok || !slices.Equal(keys, c.keys) {
				t.Fatalf("keys = %v ok=%v, want %v", keys, ok, c.keys)
			}
		})
	}
}

func TestMatchDialogNeedsDetectText(t *testing.T) {
	// The bypass option label alone (without the warning title) is not enough.
	if d, _, ok := MatchDialog("  ❯ No, exit\n    Yes, I accept\n"); ok {
		t.Fatalf("matched %+v without its title", d)
	}
	if d, _, ok := MatchDialog("╭─ Claude Code ─╮\n> \n"); ok {
		t.Fatalf("unexpected dialog %+v", d)
	}
}

func TestClaudeNotFound(t *testing.T) {
	cases := map[string]bool{
		"zsh: command not found: claude\n":        true,
		"bash: claude: command not found\n":       true,
		"sh: 1: claude: not found\n":              true,
		"zsh: command not found: pyenv\n$ ":       false, // noise from shell rc files
		"bash: claudette: command not found\n":    false,
		"Claude Code v2 ready\n> how is it found": false,
	}
	for screen, want := range cases {
		if got := claudeNotFound(screen, "claude"); got != want {
			t.Errorf("claudeNotFound(%q) = %v, want %v", screen, got, want)
		}
	}
	if !claudeNotFound("zsh: command not found: claude-beta\n", "/opt/bin/claude-beta") {
		t.Error("custom claude_bin path: basename must be matched")
	}
}
