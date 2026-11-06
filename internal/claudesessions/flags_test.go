package claudesessions

import (
	"slices"
	"testing"

	"github.com/rerangst/agent-keeper/internal/config"
)

func TestExtractFlags(t *testing.T) {
	allow := config.DefaultFlagAllowlist()
	cases := []struct {
		name    string
		argv    []string
		kept    []string
		unknown []string
	}{
		{"bare", []string{"claude"}, nil, nil},
		{"bool flag", []string{"claude", "--dangerously-skip-permissions"},
			[]string{"--dangerously-skip-permissions"}, nil},
		{"value flags both spellings", []string{"claude", "--model", "opus", "--effort=high", "--permission-mode", "plan"},
			[]string{"--model", "opus", "--effort=high", "--permission-mode", "plan"}, nil},
		{"variadic stops at next flag", []string{"claude", "--add-dir", "/a", "/b", "--model", "x"},
			[]string{"--add-dir", "/a", "/b", "--model", "x"}, nil},
		{"session args dropped", []string{"claude", "-c", "--resume", "abc", "--session-id", "u", "--fork-session",
			"-n", "my name", "--name=x", "--remote-control", "rc title", "--dangerously-skip-permissions"},
			[]string{"--dangerously-skip-permissions"}, nil},
		{"optional-value resume without value", []string{"claude", "-r", "--model", "m"},
			[]string{"--model", "m"}, nil},
		{"remote-control inline value", []string{"claude", "--remote-control=title", "--agent", "rev"},
			[]string{"--agent", "rev"}, nil},
		{"positional prompt dropped", []string{"claude", "--dangerously-skip-permissions", "fix the bug"},
			[]string{"--dangerously-skip-permissions"}, nil},
		{"unknown flags reported", []string{"claude", "--verbose", "--debug", "--settings", "s.json"},
			[]string{"--settings", "s.json"}, []string{"--verbose", "--debug"}},
		{"double dash ends options", []string{"claude", "--model", "m", "--", "--dangerously-skip-permissions"},
			[]string{"--model", "m"}, nil},
		{"npm install via node", []string{"/usr/bin/node", "--no-warnings", "/opt/claude/cli.js", "--model", "m"},
			[]string{"--model", "m"}, []string(nil)},
		{"value flag at end without value", []string{"claude", "--model"}, nil, nil},
		{"empty argv", nil, nil, nil},
		{"inline session args dropped", []string{"claude", "--session-id=u", "--resume=r", "--name=n", "--agent", "a"},
			[]string{"--agent", "a"}, nil},
		{"resume with explicit id consumes it", []string{"claude", "-r", "abc", "--model", "m"},
			[]string{"--model", "m"}, nil},
		{"absolute binary path", []string{"/opt/tools/bin/claude", "--model", "m"},
			[]string{"--model", "m"}, nil},
		{"bun runtime", []string{"bun", "/opt/claude/cli.js", "--model", "m"},
			[]string{"--model", "m"}, nil},
		{"node options before script", []string{"node", "--title", "x", "/opt/claude/cli.js", "--model", "m"},
			[]string{"--model", "m"}, nil},
		{"node without script", []string{"node"}, nil, nil},
		{"lone dash is positional", []string{"claude", "-", "--model", "m"},
			[]string{"--model", "m"}, nil},
		{"short unknown flag reported", []string{"claude", "-p", "--model", "m", "-c"},
			[]string{"--model", "m"}, []string{"-p"}},
		{"inline unknown flag reported", []string{"claude", "--verbose=1", "--model", "m"},
			[]string{"--model", "m"}, []string{"--verbose=1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kept, unknown := ExtractFlags(c.argv, allow)
			if !slices.Equal(kept, c.kept) {
				t.Errorf("kept = %q, want %q", kept, c.kept)
			}
			if !slices.Equal(unknown, c.unknown) {
				t.Errorf("unknown = %q, want %q", unknown, c.unknown)
			}
		})
	}
}

func TestExtractFlagsCustomAllowlist(t *testing.T) {
	allow := map[string]string{"--verbose": config.ArityBool}
	kept, unknown := ExtractFlags([]string{"claude", "--verbose", "--dangerously-skip-permissions"}, allow)
	if !slices.Equal(kept, []string{"--verbose"}) || !slices.Equal(unknown, []string{"--dangerously-skip-permissions"}) {
		t.Fatalf("kept=%q unknown=%q", kept, unknown)
	}
}
