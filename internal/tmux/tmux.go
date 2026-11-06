// Package tmux is agent-keeper's launcher backend: typed helpers over the
// tmux CLI. Every target uses exact-match syntax ("=name") so a session named
// "api" never resolves to "api-v2". A different backend (e.g. native Claude
// background sessions) can replace this package behind the same operations.
package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Runner executes a program and returns its stdout. Tests inject a fake.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// ExecRunner runs real processes.
type ExecRunner struct{}

// ExitError is returned when the program ran but exited non-zero.
type ExitError struct {
	Code   int
	Stderr string
}

func (e *ExitError) Error() string {
	if e.Stderr != "" {
		return fmt.Sprintf("exit status %d: %s", e.Code, e.Stderr)
	}
	return fmt.Sprintf("exit status %d", e.Code)
}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return stdout.String(), &ExitError{Code: ee.ExitCode(), Stderr: strings.TrimSpace(stderr.String())}
	}
	return stdout.String(), err
}

// Client talks to the user's default tmux server.
type Client struct {
	Runner Runner
	Bin    string   // tmux binary, default "tmux"
	Scope  []string // prefix for new-session, e.g. systemd-run --user --scope --quiet
}

func (c Client) bin() string {
	if c.Bin != "" {
		return c.Bin
	}
	return "tmux"
}

// callTimeout bounds every tmux call, so a hung client cannot stall restore
// (which runs with no unit timeout while holding its lock).
const callTimeout = 30 * time.Second

func (c Client) run(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	return c.Runner.Run(ctx, c.bin(), args...)
}

func session(name string) string { return "=" + name }       // exact session target
func pane(name string) string    { return "=" + name + ":" } // its current pane

// HasSession reports whether a session with exactly this name exists. A
// missing tmux server counts as "no session".
func (c Client) HasSession(ctx context.Context, name string) (bool, error) {
	_, err := c.run(ctx, "has-session", "-t", session(name))
	var ee *ExitError
	if errors.As(err, &ee) && ee.Code == 1 {
		return false, nil
	}
	return err == nil, err
}

// ErrDuplicate means a session with the requested name already exists.
var ErrDuplicate = errors.New("tmux session already exists")

// escapeFormat doubles '#': tmux expands formats in -s and -c, so a literal
// "#P" in a directory name would otherwise change the path (or run a
// command via "#(...)").
func escapeFormat(s string) string { return strings.ReplaceAll(s, "#", "##") }

// NewSession creates a detached session running argv (executed directly, no
// shell in between) in cwd, and returns tmux's unique id for it ("$N"), which
// KillSessionID uses so only this exact session can ever be killed. When
// Scope is set the tmux client runs inside it, so a tmux server started now
// lives in its own scope instead of the caller's cgroup (a systemd oneshot
// would otherwise kill it on exit).
func (c Client) NewSession(ctx context.Context, name, cwd string, argv []string) (string, error) {
	args := append([]string{"new-session", "-d", "-P", "-F", "#{session_id}",
		"-s", escapeFormat(name), "-c", escapeFormat(cwd), "--"}, argv...)
	var out string
	var err error
	if len(c.Scope) > 0 {
		full := append(append(append([]string{}, c.Scope[1:]...), c.bin()), args...)
		sctx, cancel := context.WithTimeout(ctx, callTimeout)
		out, err = c.Runner.Run(sctx, c.Scope[0], full...)
		cancel()
	} else {
		out, err = c.run(ctx, args...)
	}
	if err != nil {
		var ee *ExitError
		if errors.As(err, &ee) && strings.Contains(ee.Stderr, "duplicate session") {
			return "", fmt.Errorf("tmux new-session %s: %w", name, ErrDuplicate)
		}
		return "", fmt.Errorf("tmux new-session %s: %w", name, err)
	}
	id := strings.TrimSpace(out)
	if !strings.HasPrefix(id, "$") {
		return "", fmt.Errorf("tmux new-session %s: unexpected id %q", name, id)
	}
	return id, nil
}

// SendKeys sends tmux key names (e.g. "Down", "Enter") to the session's pane.
func (c Client) SendKeys(ctx context.Context, name string, keys ...string) error {
	_, err := c.run(ctx, append([]string{"send-keys", "-t", pane(name)}, keys...)...)
	return err
}

// SendText types text literally (no key-name lookup) into the session's pane.
func (c Client) SendText(ctx context.Context, name, text string) error {
	_, err := c.run(ctx, "send-keys", "-t", pane(name), "-l", text)
	return err
}

// CapturePane returns the visible text of the session's pane.
func (c Client) CapturePane(ctx context.Context, name string) (string, error) {
	return c.run(ctx, "capture-pane", "-p", "-t", pane(name))
}

// PaneCommand returns the name of the foreground program in the session's pane.
func (c Client) PaneCommand(ctx context.Context, name string) (string, error) {
	out, err := c.run(ctx, "display-message", "-p", "-t", pane(name), "#{pane_current_command}")
	return strings.TrimSpace(out), err
}

// KillSession kills exactly the named session. Restore never uses it (it
// kills only what it created, by id); it serves tools and tests that own
// the name.
func (c Client) KillSession(ctx context.Context, name string) error {
	_, err := c.run(ctx, "kill-session", "-t", session(name))
	return err
}

// KillSessionID kills the session with tmux id ("$N") as returned by
// NewSession; ids are never reused while the server runs.
func (c Client) KillSessionID(ctx context.Context, id string) error {
	if !strings.HasPrefix(id, "$") {
		return fmt.Errorf("not a tmux session id: %q", id)
	}
	_, err := c.run(ctx, "kill-session", "-t", id)
	return err
}
