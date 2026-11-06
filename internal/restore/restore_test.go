package restore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rerangst/agent-keeper/internal/claudesessions"
	"github.com/rerangst/agent-keeper/internal/config"
	"github.com/rerangst/agent-keeper/internal/registry"
	"github.com/rerangst/agent-keeper/internal/tmux"
)

// claudeScript is how a fake Claude behaves after one launch.
type claudeScript struct {
	dialogs    []string // screens shown first; each cleared by an Enter
	noStart    bool     // never writes its session file
	failText   string   // screen when resume fails (Claude exits)
	bridge     string   // bridge id written at start ("" = RC off)
	bridgeOnRC string   // bridge id written when /remote-control is typed
	sessionID  string   // overrides the id taken from --resume
	cwd        string   // overrides the cwd Claude reports
	atShell    bool     // pane shows the shell (claude missing / exited)
	slow       bool     // with noStart: Claude process runs but never registers
	screen     string   // screen while running (default: a Claude banner)
}

type fakePane struct {
	id, cwd, cmd string
	script       claudeScript
	pid          int
	started      bool
}

// fakeTmux emulates the tmux CLI and the Claude processes it starts, writing
// session files and /proc entries the engine's Source reads.
type fakeTmux struct {
	t            *testing.T
	home, proc   string
	mu           sync.Mutex
	panes        map[string]*fakePane
	launches     map[string]int // per tmux name
	behave       func(name string, attempt int) claudeScript
	nextPID      int
	keys         [][]string
	texts        []string
	kills        []string
	scoped       int
	onNewSession func(name string) // runs before new-session creates anything
}

var resumeRE = regexp.MustCompile(`--resume ([^ ']+)`)

func (f *fakeTmux) Run(ctx context.Context, bin string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if bin == "systemd-run" {
		f.scoped++
		i := slices.Index(args, "tmux")
		args = args[i+1:]
	}
	target := func() string {
		t := args[slices.Index(args, "-t")+1]
		if strings.HasPrefix(t, "$") { // tmux session id
			for name, p := range f.panes {
				if p.id == t {
					return name
				}
			}
			return ""
		}
		return strings.TrimSuffix(strings.TrimPrefix(t, "="), ":")
	}
	unescape := func(v string) string { return strings.ReplaceAll(v, "##", "#") }
	switch args[0] {
	case "has-session":
		if _, ok := f.panes[target()]; ok {
			return "", nil
		}
		return "", &tmux.ExitError{Code: 1}
	case "new-session":
		name := unescape(args[slices.Index(args, "-s")+1])
		cwd := unescape(args[slices.Index(args, "-c")+1])
		if f.onNewSession != nil {
			f.onNewSession(name)
		}
		if _, ok := f.panes[name]; ok {
			return "", &tmux.ExitError{Code: 1, Stderr: "duplicate session: " + name}
		}
		f.launches[name]++
		f.nextPID++
		argv := args[slices.Index(args, "--")+1:]
		p := &fakePane{id: "$" + strconv.Itoa(f.nextPID), cwd: cwd, cmd: strings.Join(argv, " "), pid: f.nextPID,
			script: f.behave(name, f.launches[name])}
		f.panes[name] = p
		f.maybeStart(name, p)
		return p.id + "\n", nil
	case "capture-pane":
		p := f.panes[target()]
		switch {
		case p == nil:
			return "", &tmux.ExitError{Code: 1}
		case len(p.script.dialogs) > 0:
			return p.script.dialogs[0], nil
		case p.script.failText != "":
			return p.script.failText, nil
		case p.script.screen != "":
			return p.script.screen, nil
		case p.started:
			return "╭─ Claude Code ─╮\n> ", nil
		}
		return "", nil
	case "send-keys":
		p := f.panes[target()]
		rest := args[slices.Index(args, "-t")+2:]
		if len(rest) == 2 && rest[0] == "-l" {
			f.texts = append(f.texts, rest[1])
			if strings.HasPrefix(rest[1], "/remote-control") && p.script.bridgeOnRC != "" {
				p.script.bridge = p.script.bridgeOnRC
				f.writeSession(target(), p)
			}
			return "", nil
		}
		f.keys = append(f.keys, rest)
		if rest[len(rest)-1] == "Enter" && len(p.script.dialogs) > 0 {
			p.script.dialogs = p.script.dialogs[1:]
			f.maybeStart(target(), p)
		}
		return "", nil
	case "kill-session":
		name := target()
		if p := f.panes[name]; p != nil {
			os.RemoveAll(filepath.Join(f.proc, strconv.Itoa(p.pid)))
			os.Remove(filepath.Join(f.home, "sessions", strconv.Itoa(p.pid)+".json"))
		}
		delete(f.panes, name)
		f.kills = append(f.kills, name)
		return "", nil
	case "display-message":
		p := f.panes[target()]
		if p == nil {
			return "", &tmux.ExitError{Code: 1}
		}
		if p.script.failText != "" || p.script.atShell || p.script.noStart && len(p.script.dialogs) == 0 && !p.script.slow {
			return "sh\n", nil // Claude exited (or never ran): back at the shell
		}
		return "claude\n", nil
	}
	return "", fmt.Errorf("fake tmux: unsupported %v", args)
}

func (f *fakeTmux) maybeStart(name string, p *fakePane) {
	if len(p.script.dialogs) > 0 || p.script.noStart || p.script.failText != "" || p.started {
		return
	}
	p.started = true
	f.writeSession(name, p)
}

func (f *fakeTmux) writeSession(name string, p *fakePane) {
	id := p.script.sessionID
	if m := resumeRE.FindStringSubmatch(p.cmd); id == "" && m != nil {
		id = m[1]
	}
	if id == "" {
		id = "fresh-0000-0000"
	}
	pid := strconv.Itoa(p.pid)
	cwd := p.cwd
	if p.script.cwd != "" {
		cwd = p.script.cwd
	}
	js, _ := json.Marshal(map[string]any{"pid": p.pid, "sessionId": id, "cwd": cwd, "procStart": pid,
		"kind": "interactive", "tmux": name + ":@1.%1", "status": "idle", "bridgeSessionId": p.script.bridge})
	dir := filepath.Join(f.proc, pid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	stat := pid + " (claude) S" + strings.Repeat(" 0", 18) + " " + pid + " 0\n"
	must(f.t, os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644))
	must(f.t, os.WriteFile(filepath.Join(dir, "cmdline"), []byte("claude\x00"), 0o644))
	must(f.t, os.MkdirAll(filepath.Join(f.home, "sessions"), 0o755))
	must(f.t, os.WriteFile(filepath.Join(f.home, "sessions", pid+".json"), js, 0o644))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
	onLong func(d time.Duration) // called for sleeps > 1s (stagger/backoff)
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d > time.Second && c.onLong != nil {
		c.onLong(d)
	}
	c.mu.Lock()
	c.now = c.now.Add(d)
	if d > time.Second {
		c.sleeps = append(c.sleeps, d)
	}
	c.mu.Unlock()
	return ctx.Err()
}

type harness struct {
	en    *Engine
	tm    *fakeTmux
	clock *fakeClock
	store registry.Store
	logs  *bytes.Buffer
	state string
	home  string
}

func newHarness(t *testing.T, entries ...*registry.Entry) *harness {
	t.Helper()
	home, proc, state := t.TempDir(), t.TempDir(), t.TempDir()
	must(t, os.MkdirAll(filepath.Join(home, "sessions"), 0o755))
	store := registry.Store{Path: filepath.Join(state, "registry.json")}
	must(t, store.Update(func(r *registry.Registry) error {
		for _, e := range entries {
			r.Entries[e.TmuxSession] = e
		}
		return nil
	}))
	tm := &fakeTmux{t: t, home: home, proc: proc, panes: map[string]*fakePane{}, launches: map[string]int{},
		behave: func(string, int) claudeScript { return claudeScript{} }, nextPID: 5000}
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	cfg := config.Defaults()
	en := &Engine{
		Cfg:    cfg,
		Paths:  config.Paths{StateDir: state, ClaudeHome: home, ProcRoot: proc},
		Store:  store,
		Source: &claudesessions.Source{ClaudeHome: home, ProcRoot: proc, Log: log},
		Tmux:   tmux.Client{Runner: tm, Scope: []string{"systemd-run", "--user", "--scope", "--quiet"}},
		Shell:  "/bin/sh",
		Log:    log,
		Now:    clock.Now,
		Sleep:  clock.Sleep,
		Dial:   func(context.Context, string) error { return nil },
	}
	return &harness{en: en, tm: tm, clock: clock, store: store, logs: logs, state: state, home: home}
}

// entry returns an active entry whose cwd exists and whose transcript exists.
func (h *harness) entry(t *testing.T, name, id string, rc bool) *registry.Entry {
	t.Helper()
	cwd := t.TempDir()
	h.transcript(t, id, `{"type":"user"}`+"\n")
	e := &registry.Entry{TmuxSession: name, SessionID: id, Cwd: cwd, Name: name + " agent", State: registry.StateActive,
		LastStatus: "idle", Flags: []string{"--dangerously-skip-permissions"}, RemoteControl: rc}
	if rc {
		e.BridgeSessionID = "session_" + name
	}
	return e
}

func (h *harness) transcript(t *testing.T, id, content string) string {
	t.Helper()
	dir := filepath.Join(h.home, "projects", "-w")
	must(t, os.MkdirAll(dir, 0o755))
	p := filepath.Join(dir, id+".jsonl")
	must(t, os.WriteFile(p, []byte(content), 0o644))
	return p
}

func (h *harness) put(t *testing.T, entries ...*registry.Entry) {
	t.Helper()
	must(t, h.store.Update(func(r *registry.Registry) error {
		for _, e := range entries {
			r.Entries[e.TmuxSession] = e
		}
		return nil
	}))
}

func (h *harness) run(t *testing.T, opts Options) *Report {
	t.Helper()
	rep, err := h.en.Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func (h *harness) reg(t *testing.T) *registry.Registry {
	t.Helper()
	r, err := h.store.Load()
	must(t, err)
	return r
}

func byName(rep *Report) map[string]EntryReport {
	m := map[string]EntryReport{}
	for _, e := range rep.Entries {
		m[e.TmuxSession] = e
	}
	return m
}

func TestRestoreReattachesRCAndRecords(t *testing.T) {
	h := newHarness(t)
	a := h.entry(t, "alpha", "id-alpha", true)
	b := h.entry(t, "beta", "id-beta", false)
	b.LastStatus = "busy"
	h.put(t, a, b)
	h.tm.behave = func(name string, _ int) claudeScript {
		if name == "alpha" {
			return claudeScript{bridge: "session_alpha"}
		}
		return claudeScript{}
	}
	rep := byName(h.run(t, Options{Boot: true}))

	if r := rep["alpha"]; r.Result != ResultRestored || r.Attempts != 1 || r.BridgeID != "session_alpha" || r.Nudged {
		t.Errorf("alpha = %+v", r)
	}
	if r := rep["beta"]; r.Result != ResultRestored || !r.Nudged {
		t.Errorf("beta = %+v", r)
	}
	if h.tm.scoped != 2 {
		t.Errorf("new-session ran in a systemd scope %d times, want 2", h.tm.scoped)
	}
	if cmd := h.tm.panes["beta"].cmd; !strings.Contains(cmd, "--resume id-beta") || !strings.Contains(cmd, "auto-restored") {
		t.Errorf("beta command = %s", cmd)
	}
	if len(h.tm.kills) != 0 {
		t.Errorf("kills = %v", h.tm.kills)
	}
	if got := h.clock.sleeps; !slices.Equal(got, []time.Duration{8 * time.Second}) {
		t.Errorf("long sleeps = %v, want one 8s stagger", got)
	}
	reg := h.reg(t)
	for _, n := range []string{"alpha", "beta"} {
		r := reg.Entries[n].Restore
		if r.InProgress || r.Attempts != 1 || r.LastResult == nil || *r.LastResult != ResultRestored || r.LastAt == nil {
			t.Errorf("%s registry restore = %+v", n, r)
		}
	}
	data, err := os.ReadFile(filepath.Join(h.state, "last-restore.json"))
	must(t, err)
	var saved Report
	must(t, json.Unmarshal(data, &saved))
	if !saved.Boot || len(saved.Entries) != 2 || saved.Entries[0].TmuxSession != "alpha" {
		t.Errorf("last-restore.json = %+v", saved)
	}
}

func TestRestoreAnswersDialogs(t *testing.T) {
	h := newHarness(t)
	h.put(t, h.entry(t, "alpha", "id-a", false))
	h.tm.behave = func(string, int) claudeScript {
		return claudeScript{dialogs: []string{pane(t, "trust-folder.txt"), pane(t, "bypass-permissions.txt")}}
	}
	rep := byName(h.run(t, Options{}))
	if r := rep["alpha"]; r.Result != ResultRestored {
		t.Fatalf("alpha = %+v\nlogs: %s", r, h.logs)
	}
	want := [][]string{{"Down", "Enter"}, {"Down", "Enter"}}
	if !slices.EqualFunc(h.tm.keys, want, slices.Equal[[]string]) {
		t.Errorf("keys = %v, want %v", h.tm.keys, want)
	}
}

func TestRestoreRCFallbacks(t *testing.T) {
	cases := []struct {
		name   string
		rc     bool
		script claudeScript
		result string
		typed  bool
	}{
		{"rc on, no reattach, /remote-control works", true, claudeScript{bridgeOnRC: "session_new"}, ResultRCNewBridge, true},
		{"rc on, never attaches", true, claudeScript{}, ResultRCFailed, true},
		{"rc on, different bridge", true, claudeScript{bridge: "session_other"}, ResultRCNewBridge, false},
		{"rc off, bridge appears", false, claudeScript{bridge: "session_x"}, ResultRCUnexpected, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.put(t, h.entry(t, "alpha", "id-a", c.rc))
			h.tm.behave = func(string, int) claudeScript { return c.script }
			r := byName(h.run(t, Options{}))["alpha"]
			if r.Result != c.result {
				t.Fatalf("result = %s, want %s (%s)", r.Result, c.result, r.Detail)
			}
			typed := slices.Contains(h.tm.texts, "/remote-control alpha agent")
			if typed != c.typed {
				t.Errorf("/remote-control typed = %v, want %v (texts %q)", typed, c.typed, h.tm.texts)
			}
			if !Succeeded(r.Result) || len(h.tm.kills) != 0 {
				t.Errorf("RC outcomes keep the agent running: kills=%v", h.tm.kills)
			}
		})
	}
}

func TestRestoreRetriesWithBackoffAndKeepsLastAttempt(t *testing.T) {
	h := newHarness(t)
	h.put(t, h.entry(t, "alpha", "id-a", false))
	h.tm.behave = func(string, int) claudeScript { return claudeScript{noStart: true} }
	var inProgressDuringBackoff []bool
	h.clock.onLong = func(time.Duration) {
		inProgressDuringBackoff = append(inProgressDuringBackoff, h.reg(t).Entries["alpha"].Restore.InProgress)
	}
	r := byName(h.run(t, Options{}))["alpha"]
	if r.Result != ResultFailed || r.Attempts != 3 || r.Pane == "" && r.Detail == "" {
		t.Fatalf("alpha = %+v", r)
	}
	if !slices.Equal(h.tm.kills, []string{"alpha", "alpha"}) {
		t.Errorf("kills = %v, want two (between attempts only)", h.tm.kills)
	}
	if _, alive := h.tm.panes["alpha"]; !alive {
		t.Error("last failed attempt must be left alive for inspection")
	}
	if got := h.clock.sleeps; !slices.Equal(got, []time.Duration{30 * time.Second, 120 * time.Second}) {
		t.Errorf("backoff sleeps = %v", got)
	}
	if !slices.Equal(inProgressDuringBackoff, []bool{true, true}) {
		t.Errorf("in_progress during backoff = %v", inProgressDuringBackoff)
	}
	if e := h.reg(t).Entries["alpha"]; e.Restore.InProgress || *e.Restore.LastResult != ResultFailed || e.Restore.Attempts != 3 {
		t.Errorf("registry = %+v", e.Restore)
	}
}

func TestRestoreSucceedsOnRetry(t *testing.T) {
	h := newHarness(t)
	h.put(t, h.entry(t, "alpha", "id-a", false))
	h.tm.behave = func(_ string, attempt int) claudeScript { return claudeScript{noStart: attempt == 1} }
	r := byName(h.run(t, Options{}))["alpha"]
	if r.Result != ResultRestored || r.Attempts != 2 || r.Pane != "" {
		t.Fatalf("alpha = %+v", r)
	}
}

func TestRestoreFreshStartWhenTranscriptMissing(t *testing.T) {
	h := newHarness(t)
	e := h.entry(t, "alpha", "id-a", false)
	e.SessionID = "id-without-transcript"
	e.LastStatus = "busy"
	h.put(t, e)
	r := byName(h.run(t, Options{}))["alpha"]
	if r.Result != ResultFreshStart || r.Nudged {
		t.Fatalf("alpha = %+v", r)
	}
	if cmd := h.tm.panes["alpha"].cmd; strings.Contains(cmd, "--resume") || !strings.Contains(cmd, `-n 'alpha agent'`) {
		t.Errorf("fresh command = %s", cmd)
	}
}

func TestRestoreSkipsAndConflicts(t *testing.T) {
	h := newHarness(t)
	conflict := h.entry(t, "busytmux", "id-c", false)
	stopped := h.entry(t, "stopped", "id-s", false)
	stopped.State = registry.StateStopped
	excluded := h.entry(t, "excl", "id-x", false)
	excluded.State = registry.StateExcluded
	missing := h.entry(t, "nocwd", "id-m", false)
	missing.Cwd = filepath.Join(t.TempDir(), "gone")
	h.put(t, conflict, stopped, excluded, missing)
	h.tm.panes["busytmux"] = &fakePane{pid: 1} // someone else's tmux session, no Claude
	rep := byName(h.run(t, Options{}))
	if rep["busytmux"].Result != ResultConflict || rep["nocwd"].Result != ResultCwdMissing {
		t.Fatalf("report = %+v", rep)
	}
	if _, ok := rep["stopped"]; ok {
		t.Error("stopped entries are not restored")
	}
	if _, ok := rep["excl"]; ok {
		t.Error("excluded entries are not restored")
	}
	if len(h.tm.kills) != 0 || h.tm.launches["busytmux"] != 0 {
		t.Errorf("conflicting session touched: kills=%v launches=%v", h.tm.kills, h.tm.launches)
	}
	if r := h.reg(t).Entries["busytmux"].Restore; r.LastResult == nil || *r.LastResult != ResultConflict {
		t.Errorf("conflict not recorded: %+v", r)
	}
}

func TestRestoreNamedEntry(t *testing.T) {
	h := newHarness(t)
	a := h.entry(t, "alpha", "id-a", false)
	s := h.entry(t, "stopped", "id-s", false)
	s.State = registry.StateStopped
	x := h.entry(t, "excl", "id-x", false)
	x.State = registry.StateExcluded
	h.put(t, a, s, x)
	rep := byName(h.run(t, Options{Only: "stopped"}))
	if len(rep) != 1 || rep["stopped"].Result != ResultRestored {
		t.Fatalf("named restore of a stopped entry = %+v", rep)
	}
	if rep := byName(h.run(t, Options{Only: "excl"})); rep["excl"].Result != ResultExcluded {
		t.Fatalf("named excluded = %+v", rep)
	}
	if _, err := h.en.Run(context.Background(), Options{Only: "nope"}); err == nil {
		t.Fatal("unknown name must error")
	}
}

func TestRestoreDryRunChangesNothing(t *testing.T) {
	h := newHarness(t)
	a := h.entry(t, "alpha", "id-a", true)
	a.LastStatus = "shell"
	h.put(t, a)
	var out bytes.Buffer
	h.en.Out = &out
	before, _ := os.ReadFile(h.store.Path)
	rep := byName(h.run(t, Options{DryRun: true}))
	if r := rep["alpha"]; r.Result != ResultDryRun || !r.Nudged || !strings.Contains(r.Command, "--resume id-a") {
		t.Fatalf("dry run = %+v", r)
	}
	after, _ := os.ReadFile(h.store.Path)
	if !bytes.Equal(before, after) || len(h.tm.launches) != 0 {
		t.Fatal("dry run changed the registry or launched something")
	}
	if _, err := os.Stat(filepath.Join(h.state, "last-restore.json")); err == nil {
		t.Error("dry run must not write last-restore.json")
	}
	if !strings.Contains(out.String(), "alpha  [would launch, nudged]") {
		t.Errorf("dry-run output = %s", out.String())
	}
}

func TestRestoreLockBusy(t *testing.T) {
	h := newHarness(t)
	unlock, err := lockFile(filepath.Join(h.state, "restore.lock"))
	must(t, err)
	defer unlock()
	if _, err := h.en.Run(context.Background(), Options{}); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
}

func TestRestoreAbortDuringBackoffClearsInProgress(t *testing.T) {
	h := newHarness(t)
	h.put(t, h.entry(t, "alpha", "id-a", false))
	h.tm.behave = func(string, int) claudeScript { return claudeScript{noStart: true} }
	ctx, cancel := context.WithCancel(context.Background())
	h.clock.onLong = func(time.Duration) { cancel() }
	rep, err := h.en.Run(ctx, Options{})
	must(t, err)
	if r := byName(rep)["alpha"]; r.Result != ResultAborted {
		t.Fatalf("alpha = %+v", r)
	}
	if e := h.reg(t).Entries["alpha"]; e.Restore.InProgress {
		t.Fatal("in_progress left set after abort")
	}
}

func TestRestoreRepairedTranscriptFallsBackToFresh(t *testing.T) {
	h := newHarness(t)
	e := h.entry(t, "alpha", "id-a", false)
	path := h.transcript(t, "id-a", `{"type":"user"}`+"\n"+`{"type":"assist`) // power-loss tail
	h.put(t, e)
	h.tm.behave = func(_ string, attempt int) claudeScript {
		if attempt == 1 {
			return claudeScript{failText: "No conversation found with session ID: id-a"}
		}
		return claudeScript{}
	}
	r := byName(h.run(t, Options{}))["alpha"]
	if r.Result != ResultFreshStart || r.Attempts != 2 {
		t.Fatalf("alpha = %+v", r)
	}
	if data, _ := os.ReadFile(path); !strings.HasSuffix(string(data), `{"type":"assist`) {
		t.Errorf("original transcript not restored from backup: %q", data)
	}
	if cmd := h.tm.panes["alpha"].cmd; strings.Contains(cmd, "--resume") {
		t.Errorf("second attempt should start fresh: %s", cmd)
	}
	backups, _ := filepath.Glob(filepath.Join(h.state, "backups", "id-a-*.jsonl"))
	if len(backups) != 1 {
		t.Errorf("backups = %v", backups)
	}
}

func TestSelectCandidates(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reg := registry.New()
	add := func(name, id, state string, seen time.Time) {
		reg.Entries[name] = &registry.Entry{TmuxSession: name, SessionID: id, State: state, LastSeen: seen}
	}
	add("a", "id-1", registry.StateActive, t0)
	add("b", "id-1", registry.StateActive, t0.Add(time.Minute)) // newer owner of id-1
	add("c", "id-live", registry.StateActive, t0)
	add("d", "id-d", registry.StateActive, t0)
	add("e", "id-e", registry.StateStopped, t0)
	add("f", "id-f", registry.StateActive, t0)
	live := []claudesessions.Session{{SessionID: "id-live", TmuxSession: "zzz"}, {SessionID: "other", TmuxSession: "f"}}
	cands, skipped, err := SelectCandidates(reg, live, "")
	must(t, err)
	var names []string
	for _, e := range cands {
		names = append(names, e.TmuxSession)
	}
	if !slices.Equal(names, []string{"b", "d"}) {
		t.Fatalf("candidates = %v", names)
	}
	got := map[string]string{}
	for _, s := range skipped {
		got[s.TmuxSession] = s.Result
	}
	want := map[string]string{"a": ResultDuplicateID, "c": ResultAlreadyRunning, "f": ResultAlreadyRunning}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("skipped = %v, want %v", got, want)
	}
}

func TestQueuedEntriesInProgressDuringStaggerAndStaleFlagsCleared(t *testing.T) {
	h := newHarness(t)
	a, b := h.entry(t, "alpha", "id-a", false), h.entry(t, "beta", "id-b", false)
	stale := h.entry(t, "stale", "id-s", false)
	stale.State = registry.StateStopped
	stale.Restore.InProgress = true // left by a restore that died
	h.put(t, a, b, stale)
	var betaDuringStagger []bool
	h.clock.onLong = func(d time.Duration) {
		if d == 8*time.Second {
			betaDuringStagger = append(betaDuringStagger, h.reg(t).Entries["beta"].Restore.InProgress)
		}
	}
	h.run(t, Options{})
	if !slices.Equal(betaDuringStagger, []bool{true}) {
		t.Fatalf("beta in_progress while waiting its stagger slot = %v, want [true]", betaDuringStagger)
	}
	reg := h.reg(t)
	for _, n := range []string{"alpha", "beta", "stale"} {
		if reg.Entries[n].Restore.InProgress {
			t.Errorf("%s: in_progress still set", n)
		}
	}
}

func TestFreshStartKeepsResultWhenRCFallbackRuns(t *testing.T) {
	h := newHarness(t)
	e := h.entry(t, "alpha", "id-a", true)
	e.SessionID = "no-transcript"
	h.put(t, e)
	h.tm.behave = func(string, int) claudeScript { return claudeScript{bridgeOnRC: "session_new"} }
	r := byName(h.run(t, Options{}))["alpha"]
	if r.Result != ResultFreshStart || !strings.Contains(r.Detail, ResultRCNewBridge) {
		t.Fatalf("alpha = %+v", r)
	}
}

func TestResumedOtherSessionIsReportedNotRetried(t *testing.T) {
	h := newHarness(t)
	h.put(t, h.entry(t, "alpha", "id-a", false))
	h.tm.behave = func(string, int) claudeScript { return claudeScript{sessionID: "id-other"} }
	r := byName(h.run(t, Options{}))["alpha"]
	if r.Result != ResultOtherSession || r.Attempts != 1 || !strings.Contains(r.Detail, "id-other") || len(h.tm.kills) != 0 {
		t.Fatalf("alpha = %+v kills=%v", r, h.tm.kills)
	}
}

func TestResumeFailedTextInLiveHistoryIsNotAFailure(t *testing.T) {
	h := newHarness(t)
	h.put(t, h.entry(t, "alpha", "id-a", false))
	// A resumed conversation that itself quotes the error text: Claude is
	// running, so this must not count as a failed resume.
	h.tm.behave = func(string, int) claudeScript {
		return claudeScript{dialogs: []string{"user: what does \"No conversation found with session ID\" mean?\n" + pane(t, "trust-folder.txt")}}
	}
	if r := byName(h.run(t, Options{}))["alpha"]; r.Result != ResultRestored {
		t.Fatalf("alpha = %+v", r)
	}
}

func TestNameTakenAtLaunchIsConflictAndUntouched(t *testing.T) {
	h := newHarness(t)
	h.put(t, h.entry(t, "alpha", "id-a", false))
	foreign := &fakePane{id: "$999", pid: 1}
	// Someone else creates "alpha" between our re-check and new-session.
	h.tm.onNewSession = func(name string) {
		if name == "alpha" && h.tm.panes["alpha"] == nil {
			h.tm.panes["alpha"] = foreign
		}
	}
	r := byName(h.run(t, Options{}))["alpha"]
	if r.Result != ResultConflict || r.Attempts != 1 {
		t.Fatalf("alpha = %+v", r)
	}
	if len(h.tm.kills) != 0 || h.tm.panes["alpha"] != foreign {
		t.Fatalf("foreign session touched: kills=%v", h.tm.kills)
	}
}

func TestNameTakenDuringStaggerIsConflict(t *testing.T) {
	h := newHarness(t)
	h.put(t, h.entry(t, "alpha", "id-a", false), h.entry(t, "beta", "id-b", false))
	foreign := &fakePane{id: "$998", pid: 2}
	h.clock.onLong = func(time.Duration) {
		if h.tm.panes["beta"] == nil {
			h.tm.panes["beta"] = foreign // e.g. the user logged in and created it
		}
	}
	rep := byName(h.run(t, Options{}))
	if rep["alpha"].Result != ResultRestored || rep["beta"].Result != ResultConflict {
		t.Fatalf("report = %+v", rep)
	}
	if h.tm.launches["beta"] != 0 || len(h.tm.kills) != 0 || h.tm.panes["beta"] != foreign {
		t.Fatalf("foreign beta touched: launches=%v kills=%v", h.tm.launches, h.tm.kills)
	}
}

func TestCwdWithHashIsPassedEscapedAndVerified(t *testing.T) {
	h := newHarness(t)
	e := h.entry(t, "alpha", "id-a", false)
	e.Cwd = filepath.Join(t.TempDir(), "C#Px")
	must(t, os.MkdirAll(e.Cwd, 0o755))
	h.put(t, e)
	if r := byName(h.run(t, Options{}))["alpha"]; r.Result != ResultRestored {
		t.Fatalf("alpha = %+v", r)
	}
	if got := h.tm.panes["alpha"].cwd; got != e.Cwd {
		t.Fatalf("pane cwd = %q, want %q", got, e.Cwd)
	}
}

func TestWrongCwdStopsAgentWithoutRetry(t *testing.T) {
	h := newHarness(t)
	h.put(t, h.entry(t, "alpha", "id-a", false))
	h.tm.behave = func(string, int) claudeScript { return claudeScript{cwd: "/home/elsewhere"} }
	r := byName(h.run(t, Options{}))["alpha"]
	if r.Result != ResultFailed || r.Attempts != 1 || !strings.Contains(r.Detail, "wrong cwd") {
		t.Fatalf("alpha = %+v", r)
	}
	if _, alive := h.tm.panes["alpha"]; alive || !slices.Equal(h.tm.kills, []string{"alpha"}) {
		t.Fatalf("agent in the wrong cwd must be stopped: kills=%v", h.tm.kills)
	}
}

func TestClaudeMissingFailsFastWithoutRetry(t *testing.T) {
	h := newHarness(t)
	h.put(t, h.entry(t, "alpha", "id-a", false))
	h.tm.behave = func(string, int) claudeScript {
		return claudeScript{noStart: true, atShell: true, screen: "zsh: command not found: claude\n$ "}
	}
	r := byName(h.run(t, Options{}))["alpha"]
	if r.Result != ResultFailed || r.Attempts != 1 || !strings.Contains(r.Detail, "not found") || r.DurationS > 5 {
		t.Fatalf("alpha = %+v", r)
	}
}

func TestClaudeExitingFailsFastPerAttempt(t *testing.T) {
	h := newHarness(t)
	h.put(t, h.entry(t, "alpha", "id-a", false))
	h.en.Cfg.MaxAttempts = 1
	h.tm.behave = func(string, int) claudeScript { return claudeScript{noStart: true, atShell: true} }
	r := byName(h.run(t, Options{}))["alpha"]
	if r.Result != ResultFailed || r.DurationS > 20 {
		t.Fatalf("alpha = %+v (want a fast failure, not the 90s verify timeout)", r)
	}
}
