package snapshot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rerangst/agent-keeper/internal/claudesessions"
	"github.com/rerangst/agent-keeper/internal/config"
	"github.com/rerangst/agent-keeper/internal/registry"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

const (
	boot1 = "boot-1111"
	boot2 = "boot-2222"
)

func opts(log *slog.Logger) Options {
	return Options{Debounce: 180 * time.Second, Allowlist: config.DefaultFlagAllowlist(), Log: log}
}

func bufLog(b *bytes.Buffer) *slog.Logger { return slog.New(slog.NewTextHandler(b, nil)) }

func sess(tmux, id, status string, pid int) claudesessions.Session {
	return claudesessions.Session{
		PID: pid, SessionID: id, Cwd: "/w/" + tmux, Name: tmux + "-name", NameSource: "derived",
		Tmux: tmux + ":@1.%1", TmuxSession: tmux, Status: status, Version: "2.1.319",
		Argv: []string{"claude", "--dangerously-skip-permissions", "-n", "x"},
	}
}

func TestMergeNewAndUpdate(t *testing.T) {
	reg := registry.New()
	a := sess("alpha", "id-a", "busy", 10)
	a.BridgeSessionID = "session_A"
	Merge(reg, []claudesessions.Session{a, sess("beta", "id-b", "idle", 11)}, t0, boot1, nil, opts(nil))

	e := reg.Entries["alpha"]
	if e == nil || e.State != registry.StateActive || e.SessionID != "id-a" || !e.RemoteControl ||
		e.BridgeSessionID != "session_A" || e.LastStatus != "busy" || e.Cwd != "/w/alpha" || e.LastSeenBoot != boot1 ||
		!slices.Equal(e.Flags, []string{"--dangerously-skip-permissions"}) || !e.FirstSeen.Equal(t0) || !e.LastSeen.Equal(t0) {
		t.Fatalf("alpha entry = %+v", e)
	}
	if b := reg.Entries["beta"]; b == nil || b.RemoteControl || b.BridgeSessionID != "" {
		t.Fatalf("beta entry = %+v", b)
	}

	// /clear rewrites the session id; RC turned off; first_seen kept.
	a2 := sess("alpha", "id-a2", "idle", 10)
	Merge(reg, []claudesessions.Session{a2, sess("beta", "id-b", "idle", 11)}, t0.Add(10*time.Second), boot1, nil, opts(nil))
	e = reg.Entries["alpha"]
	if e.SessionID != "id-a2" || e.RemoteControl || e.BridgeSessionID != "" || !e.FirstSeen.Equal(t0) {
		t.Fatalf("after /clear: %+v", e)
	}
}

func TestMergeIgnoresSessionsOutsideTmux(t *testing.T) {
	reg := registry.New()
	s := sess("x", "id-x", "idle", 1)
	s.TmuxSession, s.Tmux = "", ""
	Merge(reg, []claudesessions.Session{s}, t0, boot1, nil, opts(nil))
	if len(reg.Entries) != 0 {
		t.Fatalf("entries = %v", reg.Entries)
	}
}

func TestMergeDebounce(t *testing.T) {
	cases := []struct {
		name       string
		seenBoot   string
		mergeBoot  string
		absentFor  time.Duration
		state      string
		inProgress bool
		want       string
	}{
		{"seen this boot, absent < debounce stays active", boot1, boot1, 179 * time.Second, registry.StateActive, false, registry.StateActive},
		{"seen this boot, absent exactly debounce stays active", boot1, boot1, 180 * time.Second, registry.StateActive, false, registry.StateActive},
		{"seen this boot, absent > debounce becomes stopped", boot1, boot1, 181 * time.Second, registry.StateActive, false, registry.StateStopped},
		{"seen in an earlier boot is left to restore", boot1, boot2, time.Hour, registry.StateActive, false, registry.StateActive},
		{"never seen (no boot recorded) is untouched", "", boot1, time.Hour, registry.StateActive, false, registry.StateActive},
		{"unknown current boot marks nothing", "", "", time.Hour, registry.StateActive, false, registry.StateActive},
		{"restore in progress is never stopped", boot1, boot1, time.Hour, registry.StateActive, true, registry.StateActive},
		{"excluded stays excluded", boot1, boot1, time.Hour, registry.StateExcluded, false, registry.StateExcluded},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reg := registry.New()
			reg.Entries["gone"] = &registry.Entry{TmuxSession: "gone", SessionID: "id-g", State: c.state,
				LastSeen: t0, LastSeenBoot: c.seenBoot, Restore: registry.Restore{InProgress: c.inProgress}}
			Merge(reg, nil, t0.Add(c.absentFor), c.mergeBoot, nil, opts(nil))
			if got := reg.Entries["gone"].State; got != c.want {
				t.Fatalf("state = %s, want %s", got, c.want)
			}
		})
	}
}

func TestMergeExcludedNeverRevived(t *testing.T) {
	reg := registry.New()
	reg.Entries["alpha"] = &registry.Entry{TmuxSession: "alpha", SessionID: "old", State: registry.StateExcluded}
	Merge(reg, []claudesessions.Session{sess("alpha", "new", "idle", 1)}, t0, boot1, nil, opts(nil))
	e := reg.Entries["alpha"]
	if e.State != registry.StateExcluded || e.SessionID != "new" {
		t.Fatalf("excluded entry = %+v (state must stay excluded, metadata refreshed)", e)
	}
}

func TestMergeStoppedRevivedWhenLiveAgain(t *testing.T) {
	reg := registry.New()
	reg.Entries["alpha"] = &registry.Entry{TmuxSession: "alpha", SessionID: "old", State: registry.StateStopped}
	Merge(reg, []claudesessions.Session{sess("alpha", "new", "idle", 1)}, t0, boot1, nil, opts(nil))
	if e := reg.Entries["alpha"]; e.State != registry.StateActive {
		t.Fatalf("state = %s", e.State)
	}
}

func TestMergeConflictsKeepFirstAndLogOnce(t *testing.T) {
	var logs bytes.Buffer
	reg := registry.New()
	live := []claudesessions.Session{
		sess("alpha", "id-1", "idle", 1),
		sess("alpha", "id-2", "busy", 2), // same tmux name
		sess("beta", "id-1", "idle", 3),  // same session id
	}
	var noise Noise
	for i := 0; i < 5; i++ {
		Merge(reg, live, t0, boot1, &noise, opts(bufLog(&logs)))
	}
	if e := reg.Entries["alpha"]; e.SessionID != "id-1" {
		t.Fatalf("alpha kept %s, want id-1", e.SessionID)
	}
	if _, ok := reg.Entries["beta"]; ok {
		t.Fatal("beta must not be tracked: its session id belongs to alpha")
	}
	if n := strings.Count(logs.String(), "keeping the first"); n != 2 {
		t.Fatalf("conflict warnings over 5 passes = %d, want 2: %s", n, logs.String())
	}
	// The conflict clears, then returns: reported again.
	Merge(reg, live[:1], t0, boot1, &noise, opts(bufLog(&logs)))
	Merge(reg, live, t0, boot1, &noise, opts(bufLog(&logs)))
	if n := strings.Count(logs.String(), "keeping the first"); n != 4 {
		t.Fatalf("recurring conflict warnings = %d, want 4", n)
	}
}

func TestMergeUnknownFlagsNoticeOnce(t *testing.T) {
	var logs bytes.Buffer
	s := sess("alpha", "id", "idle", 1)
	s.Argv = []string{"claude", "--verbose"}
	reg := registry.New()
	var noise Noise
	for i := 0; i < 3; i++ {
		Merge(reg, []claudesessions.Session{s}, t0, boot1, &noise, opts(bufLog(&logs)))
	}
	if n := strings.Count(logs.String(), "not carried over"); n != 1 {
		t.Fatalf("unknown-flag notices = %d, want 1", n)
	}
}

func TestMergeKeepsFlagsWhenCmdlineUnreadable(t *testing.T) {
	reg := registry.New()
	reg.Entries["alpha"] = &registry.Entry{TmuxSession: "alpha", State: registry.StateActive, Flags: []string{"--model", "m"}}
	s := sess("alpha", "id", "idle", 1)
	s.Argv = nil
	Merge(reg, []claudesessions.Session{s}, t0, boot1, nil, opts(nil))
	if f := reg.Entries["alpha"].Flags; !slices.Equal(f, []string{"--model", "m"}) {
		t.Fatalf("flags = %q", f)
	}
}

func TestMergeSkipsNilEntries(t *testing.T) {
	reg := registry.New()
	reg.Entries["x"] = nil
	Merge(reg, nil, t0.Add(time.Hour), boot1, nil, opts(nil)) // must not panic
}

func TestMergeKeepsKnownValuesWhenKeysMissing(t *testing.T) {
	reg := registry.New()
	reg.Entries["alpha"] = &registry.Entry{TmuxSession: "alpha", State: registry.StateActive,
		Cwd: "/w/alpha", Name: "Alpha", NameSource: "user", LastStatus: "busy", ClaudeVersion: "2.1.319",
		RemoteControl: true, BridgeSessionID: "session_A"}
	s := claudesessions.Session{PID: 1, SessionID: "id", TmuxSession: "alpha"} // keys missing
	Merge(reg, []claudesessions.Session{s}, t0, boot1, nil, opts(nil))
	e := reg.Entries["alpha"]
	if e.Cwd != "/w/alpha" || e.Name != "Alpha" || e.NameSource != "user" || e.LastStatus != "busy" || e.ClaudeVersion != "2.1.319" {
		t.Fatalf("known values lost: %+v", e)
	}
	if e.RemoteControl || e.BridgeSessionID != "" {
		t.Fatalf("absent bridge id must mean RC off: %+v", e)
	}
}

// writeLive creates a Claude home + proc root with one live session.
func writeLive(t *testing.T, home, proc string, pid int, tmux, id string) {
	t.Helper()
	sdir := filepath.Join(home, "sessions")
	pdir := filepath.Join(proc, strconv.Itoa(pid))
	for _, d := range []string{sdir, pdir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	js := `{"pid":` + strconv.Itoa(pid) + `,"sessionId":"` + id + `","cwd":"/w","procStart":"77","kind":"interactive","tmux":"` + tmux + `:@1.%1","status":"idle"}`
	stat := strconv.Itoa(pid) + " (claude) S" + strings.Repeat(" 0", 18) + " 77 0\n"
	files := map[string]string{
		filepath.Join(sdir, strconv.Itoa(pid)+".json"): js,
		filepath.Join(pdir, "stat"):                    stat,
		filepath.Join(pdir, "cmdline"):                 "claude\x00--model\x00m\x00",
	}
	for p, c := range files {
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func newSnap(home, proc, regPath, boot string, now *time.Time) *Snapshotter {
	s := &Snapshotter{
		Source: claudesessions.Source{ClaudeHome: home, ProcRoot: proc, Log: bufLog(&bytes.Buffer{})},
		Store:  registry.Store{Path: regPath},
		Opts:   opts(bufLog(&bytes.Buffer{})),
		BootID: boot,
	}
	if now != nil {
		s.Now = func() time.Time { return *now }
	}
	return s
}

func TestCancelledDaemonDoesNotMark(t *testing.T) {
	home, proc := t.TempDir(), t.TempDir()
	regPath := filepath.Join(t.TempDir(), "registry.json")
	writeLive(t, home, proc, 500, "alpha", "id-a")
	now := t0
	s := newSnap(home, proc, regPath, boot1, &now)
	reg, err := s.Once()
	if err != nil {
		t.Fatal(err)
	}
	if e := reg.Entries["alpha"]; e == nil || !slices.Equal(e.Flags, []string{"--model", "m"}) || e.LastSeenBoot != boot1 {
		t.Fatalf("entry = %+v", e)
	}

	// The process exits (as at shutdown) and the daemon is already cancelled:
	// it must return without a pass, although alpha is absent > debounce.
	if err := os.RemoveAll(filepath.Join(proc, "500")); err != nil {
		t.Fatal(err)
	}
	now = t0.Add(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	RunDaemon(ctx, s, time.Hour, bufLog(&bytes.Buffer{}))
	if reg, _ = s.Store.Load(); reg.Entries["alpha"].State != registry.StateActive {
		t.Fatalf("state after cancelled daemon = %s, want active", reg.Entries["alpha"].State)
	}
}

func TestDaemonRestartKeepsBootKnowledge(t *testing.T) {
	home, proc := t.TempDir(), t.TempDir()
	regPath := filepath.Join(t.TempDir(), "registry.json")
	writeLive(t, home, proc, 510, "alpha", "id-a")
	now := t0
	if _, err := newSnap(home, proc, regPath, boot1, &now).Once(); err != nil {
		t.Fatal(err)
	}
	// User closes the session; the daemon restarts (update/crash) before the
	// debounce elapsed. A fresh daemon in the same boot still marks it stopped.
	if err := os.RemoveAll(filepath.Join(proc, "510")); err != nil {
		t.Fatal(err)
	}
	now = t0.Add(4 * time.Minute)
	reg, err := newSnap(home, proc, regPath, boot1, &now).Once()
	if err != nil {
		t.Fatal(err)
	}
	if st := reg.Entries["alpha"].State; st != registry.StateStopped {
		t.Fatalf("same boot, new daemon: state = %s, want stopped", st)
	}
}

func TestNextBootLeavesEntriesToRestore(t *testing.T) {
	home, proc := t.TempDir(), t.TempDir()
	regPath := filepath.Join(t.TempDir(), "registry.json")
	writeLive(t, home, proc, 520, "alpha", "id-a")
	now := t0
	if _, err := newSnap(home, proc, regPath, boot1, &now).Once(); err != nil {
		t.Fatal(err)
	}
	// Power loss: after reboot the process is gone and the boot id differs.
	if err := os.RemoveAll(filepath.Join(proc, "520")); err != nil {
		t.Fatal(err)
	}
	now = t0.Add(time.Hour)
	reg, err := newSnap(home, proc, regPath, boot2, &now).Once()
	if err != nil {
		t.Fatal(err)
	}
	if st := reg.Entries["alpha"].State; st != registry.StateActive {
		t.Fatalf("after reboot: state = %s, want active", st)
	}
}

func TestSnapshotSurvivesMalformedSessionFile(t *testing.T) {
	home, proc := t.TempDir(), t.TempDir()
	writeLive(t, home, proc, 600, "alpha", "id-a")
	if err := os.WriteFile(filepath.Join(home, "sessions", "601.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := newSnap(home, proc, filepath.Join(t.TempDir(), "registry.json"), boot1, nil).Once()
	if err != nil || len(reg.Entries) != 1 {
		t.Fatalf("reg=%v err=%v", reg, err)
	}
}

func TestRegistryNullEntryDoesNotCrashPass(t *testing.T) {
	home, proc := t.TempDir(), t.TempDir()
	writeLive(t, home, proc, 700, "alpha", "id-a")
	regPath := filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(regPath, []byte(`{"schema": 1, "entries": {"x": null}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := newSnap(home, proc, regPath, boot1, nil).Once()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Entries["x"]; ok || reg.Entries["alpha"] == nil {
		t.Fatalf("entries = %v", reg.Entries)
	}
}

func TestOnceSessionsDirMissing(t *testing.T) {
	home, proc := t.TempDir(), t.TempDir()
	regPath := filepath.Join(t.TempDir(), "registry.json")

	// Fresh host (nothing seen this boot): not an error.
	if _, err := newSnap(home, proc, regPath, boot1, nil).Once(); err != nil {
		t.Fatalf("fresh host: %v", err)
	}

	// Sessions were seen this boot, then the directory vanished: the pass
	// fails and the registry is not touched, so nothing gets marked stopped.
	writeLive(t, home, proc, 800, "alpha", "id-a")
	now := t0
	s := newSnap(home, proc, regPath, boot1, &now)
	if _, err := s.Once(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(home, "sessions")); err != nil {
		t.Fatal(err)
	}
	now = t0.Add(time.Hour)
	if _, err := s.Once(); !errors.Is(err, claudesessions.ErrNoSessionsDir) {
		t.Fatalf("vanished dir: err = %v, want ErrNoSessionsDir", err)
	}
	if reg, _ := s.Store.Load(); reg.Entries["alpha"].State != registry.StateActive {
		t.Fatalf("state = %s, want active", reg.Entries["alpha"].State)
	}
}

func TestRunDaemonLogsRepeatedErrorOnce(t *testing.T) {
	home, proc := t.TempDir(), t.TempDir()
	writeLive(t, home, proc, 900, "alpha", "id-a")
	regPath := filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(regPath, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs syncBuffer
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	RunDaemon(ctx, newSnap(home, proc, regPath, boot1, nil), 5*time.Millisecond, slog.New(slog.NewTextHandler(&logs, nil)))
	if n := strings.Count(logs.String(), "snapshot pass failed"); n != 1 {
		t.Fatalf("repeated error logged %d times, want 1", n)
	}
}

func TestErrKeyIgnoresPaths(t *testing.T) {
	a := fmt.Errorf("save: %w", &fs.PathError{Op: "rename", Path: "/s/.registry.json.tmp-111", Err: errors.New("no space left on device")})
	b := fmt.Errorf("save: %w", &fs.PathError{Op: "rename", Path: "/s/.registry.json.tmp-222", Err: errors.New("no space left on device")})
	if errKey(a) != errKey(b) {
		t.Fatalf("keys differ: %q vs %q", errKey(a), errKey(b))
	}
	if errKey(errors.New("x")) != "x" {
		t.Fatal("plain errors use their message")
	}
}

func TestCurrentBootID(t *testing.T) {
	proc := t.TempDir()
	if got := CurrentBootID(proc); got != "" {
		t.Fatalf("missing file: %q", got)
	}
	dir := filepath.Join(proc, "sys", "kernel", "random")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "boot_id"), []byte("abc-123\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := CurrentBootID(proc); got != "abc-123" {
		t.Fatalf("boot id = %q", got)
	}
}

// syncBuffer is a bytes.Buffer safe for the daemon's logger.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *syncBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *syncBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}
