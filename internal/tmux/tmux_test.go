package tmux

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

type recorder struct {
	calls [][]string
	out   string
	err   error
}

func (r *recorder) Run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	return r.out, r.err
}

func TestClientTargetsAreExact(t *testing.T) {
	r := &recorder{}
	c := Client{Runner: r}
	ctx := context.Background()
	if _, err := c.HasSession(ctx, "api"); err != nil {
		t.Fatal(err)
	}
	_ = c.SendKeys(ctx, "api", "Down", "Enter")
	_ = c.SendText(ctx, "api", "/remote-control x")
	_, _ = c.CapturePane(ctx, "api")
	_, _ = c.PaneCommand(ctx, "api")
	_ = c.KillSession(ctx, "api")
	_ = c.KillSessionID(ctx, "$7")
	if err := c.KillSessionID(ctx, "api"); err == nil {
		t.Error("KillSessionID must refuse a name")
	}
	want := [][]string{
		{"tmux", "has-session", "-t", "=api"},
		{"tmux", "send-keys", "-t", "=api:", "Down", "Enter"},
		{"tmux", "send-keys", "-t", "=api:", "-l", "/remote-control x"},
		{"tmux", "capture-pane", "-p", "-t", "=api:"},
		{"tmux", "display-message", "-p", "-t", "=api:", "#{pane_current_command}"},
		{"tmux", "kill-session", "-t", "=api"},
		{"tmux", "kill-session", "-t", "$7"},
	}
	if !slices.EqualFunc(r.calls, want, slices.Equal[[]string]) {
		t.Fatalf("calls =\n%q\nwant\n%q", r.calls, want)
	}
}

func TestNewSessionScopeEscapingAndID(t *testing.T) {
	r := &recorder{out: "$12\n"}
	c := Client{Runner: r, Bin: "/usr/bin/tmux", Scope: []string{"systemd-run", "--user", "--scope", "--quiet"}}
	id, err := c.NewSession(context.Background(), "s#1", "/w/C#Px", []string{"/bin/zsh", "-ic", "claude -n 'a#b'"})
	if err != nil || id != "$12" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	want := []string{"systemd-run", "--user", "--scope", "--quiet", "/usr/bin/tmux", "new-session", "-d", "-P", "-F", "#{session_id}",
		"-s", "s##1", "-c", "/w/C##Px", "--", "/bin/zsh", "-ic", "claude -n 'a#b'"}
	if !slices.Equal(r.calls[0], want) {
		t.Fatalf("call = %q", r.calls[0])
	}
	r.calls, c.Scope = nil, nil
	if _, err := c.NewSession(context.Background(), "s", "/w", []string{"cmd"}); err != nil {
		t.Fatal(err)
	}
	if r.calls[0][0] != "/usr/bin/tmux" {
		t.Fatalf("unscoped call = %q", r.calls[0])
	}
}

func TestNewSessionDuplicateAndBadID(t *testing.T) {
	c := Client{Runner: &recorder{err: &ExitError{Code: 1, Stderr: "duplicate session: s"}}}
	if _, err := c.NewSession(context.Background(), "s", "/w", []string{"x"}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("err = %v, want ErrDuplicate", err)
	}
	c = Client{Runner: &recorder{out: "garbage"}}
	if _, err := c.NewSession(context.Background(), "s", "/w", []string{"x"}); err == nil {
		t.Fatal("unexpected id output must be an error")
	}
}

func TestHasSessionExitCodes(t *testing.T) {
	c := Client{Runner: &recorder{err: &ExitError{Code: 1, Stderr: "no server running"}}}
	if ok, err := c.HasSession(context.Background(), "x"); ok || err != nil {
		t.Fatalf("exit 1 = %v, %v; want false, nil", ok, err)
	}
	boom := errors.New("exec: tmux not found")
	c = Client{Runner: &recorder{err: boom}}
	if ok, err := c.HasSession(context.Background(), "x"); ok || !errors.Is(err, boom) {
		t.Fatalf("exec error = %v, %v", ok, err)
	}
}

func TestExecRunnerExitError(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	out, err := ExecRunner{}.Run(context.Background(), sh, "-c", "echo out; echo bad >&2; exit 3")
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 3 || ee.Stderr != "bad" || strings.TrimSpace(out) != "out" {
		t.Fatalf("out=%q err=%v", out, err)
	}
}
