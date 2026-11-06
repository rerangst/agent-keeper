package restore

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rerangst/agent-keeper/internal/config"
	"github.com/rerangst/agent-keeper/internal/registry"
)

func TestShellQuoteRoundTripsThroughSh(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	for _, s := range []string{
		"plain", "", "with space", "it's", "''", `"double"`, "$HOME `id` $(id)", "a\nb", "tab\there",
		"Ünïcødé — naïve café", "back\\slash", "*?[glob]", "-n", "semi;colon&amp|pipe",
	} {
		out, err := exec.Command(sh, "-c", "printf '%s' "+shellQuote(s)).Output()
		if err != nil {
			t.Fatalf("sh failed for %q: %v", s, err)
		}
		if string(out) != s {
			t.Errorf("round trip %q -> %q (quoted %s)", s, out, shellQuote(s))
		}
	}
}

func TestBuildCommand(t *testing.T) {
	cfg := config.Defaults()
	now := time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC)
	base := registry.Entry{TmuxSession: "alpha", SessionID: "11111111-1111-4111-8111-111111111111",
		Name: "Ünïcødé 'alpha'", Flags: []string{"--dangerously-skip-permissions", "--add-dir", "/w/a b"}}

	idle := base
	idle.LastStatus = "idle"
	l := BuildCommand(&idle, cfg, "/usr/bin/zsh", false, now)
	want := []string{"claude", "--dangerously-skip-permissions", "--add-dir", "/w/a b",
		"--resume", "11111111-1111-4111-8111-111111111111", "-n", "Ünïcødé 'alpha'"}
	if !slices.Equal(l.Argv, want) || l.Nudged || l.Fresh {
		t.Fatalf("idle argv = %q nudged=%v", l.Argv, l.Nudged)
	}
	if !strings.HasPrefix(l.Command, "/usr/bin/zsh -ic '") || !strings.HasSuffix(l.Command, "exec /usr/bin/zsh -i'") {
		t.Errorf("command = %s", l.Command)
	}
	if strings.Contains(l.Command, "--remote-control") {
		t.Error("RC must reattach from the transcript, not be requested at launch")
	}

	for _, st := range []string{"busy", "shell"} {
		e := base
		e.LastStatus = st
		l := BuildCommand(&e, cfg, "/bin/sh", false, now)
		if !l.Nudged || !strings.Contains(l.Argv[len(l.Argv)-1], "restored at 2026-01-02 03:04 UTC") {
			t.Errorf("%s: not nudged or {time} not filled: %q", st, l.Argv)
		}
	}

	busy := base
	busy.LastStatus = "busy"
	fresh := BuildCommand(&busy, cfg, "/bin/sh", true, now)
	if slices.Contains(fresh.Argv, "--resume") || fresh.Nudged || !fresh.Fresh || !slices.Contains(fresh.Argv, "-n") {
		t.Errorf("fresh start argv = %q nudged=%v", fresh.Argv, fresh.Nudged)
	}

	noName := base
	noName.Name = ""
	if l := BuildCommand(&noName, cfg, "/bin/sh", false, now); slices.Contains(l.Argv, "-n") {
		t.Errorf("empty name must not pass -n: %q", l.Argv)
	}
}

// The pane command is run by tmux through a shell; make sure the outer
// layer decodes to exactly `<shell> -ic <inner>`.
func TestBuildCommandOuterQuoting(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	e := registry.Entry{SessionID: "id", Name: `it's "x"`, LastStatus: "busy"}
	l := BuildCommand(&e, config.Defaults(), "/bin/sh", false, time.Now())
	// Replace the shell by printf to see the argv the shell would receive.
	probe := "printf '[%s]'" + strings.TrimPrefix(l.Command, "/bin/sh")
	out, err := exec.Command(sh, "-c", probe).Output()
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.HasPrefix(got, "[-ic][") || !strings.Contains(got, `-n 'it'\''s "x"'`) || !strings.HasSuffix(got, "; exec /bin/sh -i]") {
		t.Fatalf("decoded = %s", got)
	}
}

func TestPasswdShell(t *testing.T) {
	passwd := "root:x:0:0:root:/root:/bin/bash\nu:x:1000:1000::/home/u:/usr/bin/zsh\nbad line\n"
	if got := passwdShell(passwd, 1000); got != "/usr/bin/zsh" {
		t.Errorf("uid 1000 shell = %q", got)
	}
	if got := passwdShell(passwd, 42); got != "" {
		t.Errorf("unknown uid shell = %q", got)
	}
}
