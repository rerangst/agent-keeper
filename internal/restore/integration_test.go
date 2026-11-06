//go:build integration

// End-to-end crash recovery against real tmux and a real, logged-in Claude
// Code. Opt-in because it spends a few tiny prompts:
//
//	AGENT_KEEPER_IT=1 go test -tags integration -run TestCrashRecovery -v -timeout 20m ./internal/restore/
//
// Three throwaway sessions run in temp directories: A (Remote Control on,
// mid-task), B (RC off, idle), C (idle, then /clear and one more prompt).
// After a snapshot they are SIGKILLed and their tmux sessions killed, then
// restore must bring each back by its last session id. The registry and state
// live in temp dirs; only ~/.claude is shared with the user's real sessions.
package restore

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rerangst/agent-keeper/internal/claudesessions"
	"github.com/rerangst/agent-keeper/internal/config"
	"github.com/rerangst/agent-keeper/internal/registry"
	"github.com/rerangst/agent-keeper/internal/snapshot"
	"github.com/rerangst/agent-keeper/internal/tmux"
)

type itSession struct {
	name, cwd string
	rc        bool
}

func TestCrashRecovery(t *testing.T) {
	if os.Getenv("AGENT_KEEPER_IT") != "1" {
		t.Skip("set AGENT_KEEPER_IT=1 to run (starts real Claude sessions)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Minute)
	defer cancel()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	claudeHome := filepath.Join(home, ".claude")
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		claudeHome = v
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	tc := tmux.Client{Runner: tmux.ExecRunner{}}
	src := &claudesessions.Source{ClaudeHome: claudeHome, ProcRoot: "/proc", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	shell := UserShell()
	stamp := time.Now().Format("150405")
	sessions := []itSession{
		{name: "ak-it-a-" + stamp, rc: true},
		{name: "ak-it-b-" + stamp},
		{name: "ak-it-c-" + stamp},
	}
	work := t.TempDir()
	for i := range sessions {
		sessions[i].cwd = filepath.Join(work, sessions[i].name)
		if err := os.MkdirAll(sessions[i].cwd, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { cleanupIT(t, tc, src, claudeHome, sessions) })

	// Start the three agents.
	for _, s := range sessions {
		argv := []string{"claude", "--dangerously-skip-permissions", "-n", s.name}
		if s.rc {
			argv = append(argv, "--remote-control", s.name)
		}
		pane := []string{shell, "-ic", shellJoin(argv) + "; exec " + shellQuote(shell) + " -i"}
		if _, err := tc.NewSession(ctx, s.name, s.cwd, pane); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range sessions {
		waitClaude(ctx, t, tc, src, s.name, "")
	}
	prompt(ctx, t, tc, sessions[1].name, "Reply with the single word ok.")
	prompt(ctx, t, tc, sessions[2].name, "Reply with the single word ok.")
	waitStatus(ctx, t, src, sessions[1].name, "idle")
	waitStatus(ctx, t, src, sessions[2].name, "idle")
	before := waitClaude(ctx, t, tc, src, sessions[2].name, "")
	prompt(ctx, t, tc, sessions[2].name, "/clear")
	cleared := waitIDChange(ctx, t, src, sessions[2].name, before.SessionID)
	prompt(ctx, t, tc, sessions[2].name, "Reply with the single word ok.")
	waitStatus(ctx, t, src, sessions[2].name, "idle")
	prompt(ctx, t, tc, sessions[0].name, "Run this exact bash command in the foreground and nothing else: sleep 300")
	waitStatus(ctx, t, src, sessions[0].name, "busy", "shell")
	a := waitBridge(ctx, t, src, sessions[0].name)

	// Snapshot into an isolated registry holding only the test sessions.
	state := t.TempDir()
	store := registry.Store{Path: filepath.Join(state, "registry.json")}
	snap := &snapshot.Snapshotter{Source: claudesessions.Source{ClaudeHome: claudeHome, ProcRoot: "/proc"}, Store: store,
		Opts:   snapshot.Options{Debounce: 180 * time.Second, Allowlist: config.DefaultFlagAllowlist()},
		BootID: snapshot.CurrentBootID("/proc")}
	if _, err := snap.Once(); err != nil {
		t.Fatal(err)
	}
	keep := map[string]bool{}
	for _, s := range sessions {
		keep[s.name] = true
	}
	if err := store.Update(func(r *registry.Registry) error {
		for n := range r.Entries {
			if !keep[n] {
				delete(r.Entries, n)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reg, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	pre := map[string]registry.Entry{}
	for n, e := range reg.Entries {
		pre[n] = *e
	}
	if len(pre) != 3 || pre[sessions[2].name].SessionID != cleared {
		t.Fatalf("snapshot = %+v (want 3 entries, C at post-/clear id %s)", pre, cleared)
	}

	// Simulated power loss: SIGKILL Claude (no clean exit), then kill tmux.
	for _, s := range sessions {
		live := waitClaude(ctx, t, tc, src, s.name, "")
		_ = syscall.Kill(live.PID, syscall.SIGKILL)
		_ = tc.KillSession(ctx, s.name)
	}
	for _, s := range sessions {
		deadline := time.Now().Add(30 * time.Second)
		for _, ok := liveIn(src, s.name); ok && time.Now().Before(deadline); _, ok = liveIn(src, s.name) {
			time.Sleep(200 * time.Millisecond)
		}
	}

	en := &Engine{
		Cfg:    config.Defaults(),
		Paths:  config.Paths{StateDir: state, ClaudeHome: claudeHome, ProcRoot: "/proc"},
		Store:  store,
		Source: src,
		Tmux:   tmux.Client{Runner: tmux.ExecRunner{}, Scope: []string{"systemd-run", "--user", "--scope", "--quiet"}},
		Shell:  shell,
		Log:    log,
	}
	en.Cfg.StaggerS = 2
	rep, err := en.Run(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]EntryReport{}
	for _, r := range rep.Entries {
		got[r.TmuxSession] = r
		t.Logf("%s: %s attempts=%d nudged=%v bridge=%q %s", r.TmuxSession, r.Result, r.Attempts, r.Nudged, r.BridgeID, r.Detail)
	}

	// A: resumed by id, nudged, same RC bridge (also on the live session file).
	ra := got[sessions[0].name]
	if ra.Result != ResultRestored || !ra.Nudged || ra.BridgeID != a {
		t.Errorf("A = %+v, want restored, nudged, bridge %s", ra, a)
	}
	if live := waitClaude(ctx, t, tc, src, sessions[0].name, pre[sessions[0].name].SessionID); live.BridgeSessionID != "" && live.BridgeSessionID != a {
		t.Errorf("A live bridge = %s, want %s", live.BridgeSessionID, a)
	}
	// B: resumed, not nudged, no bridge.
	if rb := got[sessions[1].name]; rb.Result != ResultRestored || rb.Nudged || rb.BridgeID != "" {
		t.Errorf("B = %+v, want restored, not nudged, RC off", rb)
	}
	// C: resumed by the post-/clear id.
	if rc := got[sessions[2].name]; rc.Result != ResultRestored || rc.SessionID != cleared {
		t.Errorf("C = %+v, want restored at %s", rc, cleared)
	}
	for _, s := range sessions {
		live := waitClaude(ctx, t, tc, src, s.name, pre[s.name].SessionID)
		if live.Cwd != s.cwd {
			t.Errorf("%s cwd = %s, want %s", s.name, live.Cwd, s.cwd)
		}
	}
}

func prompt(ctx context.Context, t *testing.T, tc tmux.Client, name, text string) {
	t.Helper()
	if err := tc.SendText(ctx, name, text); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if err := tc.SendKeys(ctx, name, "Enter"); err != nil {
		t.Fatal(err)
	}
}

// waitClaude answers startup dialogs until the session's Claude is live (with
// wantID when non-empty) and returns it.
func waitClaude(ctx context.Context, t *testing.T, tc tmux.Client, src *claudesessions.Source, name, wantID string) claudesessions.Session {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if screen, err := tc.CapturePane(ctx, name); err == nil {
			if _, keys, ok := MatchDialog(screen); ok {
				_ = tc.SendKeys(ctx, name, keys...)
				time.Sleep(time.Second)
				continue
			}
		}
		if s, ok := liveIn(src, name); ok && (wantID == "" || s.SessionID == wantID) {
			return s
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("%s: Claude not live (want id %q)", name, wantID)
	return claudesessions.Session{}
}

func liveIn(src *claudesessions.Source, name string) (claudesessions.Session, bool) {
	live, _ := src.ListLive()
	for _, s := range live {
		if s.TmuxSession == name {
			return s, true
		}
	}
	return claudesessions.Session{}, false
}

func waitStatus(ctx context.Context, t *testing.T, src *claudesessions.Source, name string, statuses ...string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if s, ok := liveIn(src, name); ok {
			for _, st := range statuses {
				if s.Status == st {
					return
				}
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("%s: status never reached %v", name, statuses)
}

func waitIDChange(ctx context.Context, t *testing.T, src *claudesessions.Source, name, old string) string {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if s, ok := liveIn(src, name); ok && s.SessionID != old {
			return s.SessionID
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("%s: session id did not change after /clear", name)
	return ""
}

func waitBridge(ctx context.Context, t *testing.T, src *claudesessions.Source, name string) string {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if s, ok := liveIn(src, name); ok && s.BridgeSessionID != "" {
			return s.BridgeSessionID
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("%s: no Remote Control bridge", name)
	return ""
}

// cleanupIT exits the test agents cleanly (so Claude can unregister Remote
// Control), kills their tmux sessions and deletes their transcripts. It only
// touches sessions this test created.
func cleanupIT(t *testing.T, tc tmux.Client, src *claudesessions.Source, claudeHome string, sessions []itSession) {
	ctx := context.Background()
	for _, s := range sessions {
		if ok, _ := tc.HasSession(ctx, s.name); !ok {
			continue
		}
		// Errors are ignored here: cleanup must always reach the kill/delete below.
		_ = tc.SendKeys(ctx, s.name, "Escape")
		_ = tc.SendText(ctx, s.name, "/exit")
		time.Sleep(300 * time.Millisecond)
		_ = tc.SendKeys(ctx, s.name, "Enter")
	}
	time.Sleep(3 * time.Second)
	for _, s := range sessions {
		if ok, _ := tc.HasSession(ctx, s.name); ok {
			_ = tc.KillSession(ctx, s.name)
		}
		// Transcripts live in ~/.claude/projects/<cwd with / and . as ->.
		proj := filepath.Join(claudeHome, "projects", strings.NewReplacer("/", "-", ".", "-", "_", "-").Replace(s.cwd))
		if strings.Contains(proj, "ak-it-") {
			_ = os.RemoveAll(proj)
		}
	}
}
