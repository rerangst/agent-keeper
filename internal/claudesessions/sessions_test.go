package claudesessions

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeProc writes /proc/<pid>/{stat,cmdline} under root. The comm field
// deliberately contains spaces and parentheses.
func fakeProc(t *testing.T, root string, pid int, start string, argv ...string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fields := []string{"S"} // field 3
	for i := 4; i <= 21; i++ {
		fields = append(fields, "0")
	}
	fields = append(fields, start, "123456", "789") // field 22 = starttime
	stat := strconv.Itoa(pid) + " (cla ude) (x)) " + strings.Join(fields, " ") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
	if len(argv) > 0 {
		cmd := strings.Join(argv, "\x00") + "\x00"
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(cmd), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// fixtureHome copies testdata/sessions into a temp Claude home.
func fixtureHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	dst := filepath.Join(home, "sessions")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join("..", "..", "testdata", "sessions")
	files, err := filepath.Glob(filepath.Join(src, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures in %s: %v", src, err)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, filepath.Base(f)), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func TestListLive(t *testing.T) {
	home := fixtureHome(t)
	proc := t.TempDir()
	fakeProc(t, proc, 1001, "5000", "claude", "--dangerously-skip-permissions")
	fakeProc(t, proc, 1002, "6000", "claude", "--model", "opus")
	fakeProc(t, proc, 1003, "7000", "claude")
	fakeProc(t, proc, 1004, "1", "claude")
	fakeProc(t, proc, 1005, "99999", "bash") // PID reused by another process
	// 1006 has a session file but no process: stale file of a dead PID.
	stale := `{"pid":1006,"sessionId":"66666666-6666-4666-8666-666666666666","procStart":"1","tmux":"dead:@6.%6"}`
	if err := os.WriteFile(filepath.Join(home, "sessions", "1006.json"), []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	src := &Source{ClaudeHome: home, ProcRoot: proc, Log: slog.New(slog.NewTextHandler(&logs, nil))}
	live, err := src.ListLive()
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for _, s := range live {
		pids = append(pids, s.PID)
	}
	if want := []int{1001, 1002, 1003}; !slices.Equal(pids, want) {
		t.Fatalf("live pids = %v, want %v (oldest procStart first; reused, dead, malformed skipped)", pids, want)
	}
	if !strings.Contains(logs.String(), "malformed") {
		t.Errorf("expected a malformed-file warning, logs: %s", logs.String())
	}

	a := live[0]
	if a.TmuxSession != "alpha" || a.Tmux != "alpha:@1.%1" || a.SessionID != "11111111-1111-4111-8111-111111111111" ||
		a.Cwd != "/home/user/prj/alpha" || a.Name != "Alpha Service" || a.NameSource != "user" ||
		a.Status != "idle" || a.BridgeSessionID != "session_TESTBRIDGEALPHA" || !a.RemoteControl() || a.Version != "2.1.319" {
		t.Errorf("alpha parsed wrong: %+v", a)
	}
	if got := strings.Join(a.Argv, " "); got != "claude --dangerously-skip-permissions" {
		t.Errorf("alpha argv = %q", got)
	}
	b := live[1]
	if b.RemoteControl() || b.Status != "busy" || b.TmuxSession != "beta" {
		t.Errorf("beta parsed wrong: %+v", b)
	}
	if live[2].TmuxSession != "" {
		t.Errorf("session outside tmux should have empty TmuxSession, got %q", live[2].TmuxSession)
	}
}

func TestListLiveMissingSessionsDir(t *testing.T) {
	live, err := (&Source{ClaudeHome: t.TempDir(), ProcRoot: t.TempDir()}).ListLive()
	if !errors.Is(err, ErrNoSessionsDir) || live != nil {
		t.Fatalf("missing sessions dir: live=%v err=%v, want ErrNoSessionsDir", live, err)
	}
}

func TestListLiveUnreadableSessionsDirIsError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	home := fixtureHome(t)
	dir := filepath.Join(home, "sessions")
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	live, err := (&Source{ClaudeHome: home, ProcRoot: t.TempDir()}).ListLive()
	if err == nil || errors.Is(err, ErrNoSessionsDir) {
		t.Fatalf("unreadable sessions dir: live=%v err=%v, want a read error (never an empty list)", live, err)
	}
}

func TestListLiveRejectsFileWithoutProcStart(t *testing.T) {
	home := t.TempDir()
	proc := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := `{"pid":2001,"sessionId":"77777777-7777-4777-8777-777777777777","tmux":"old:@1.%1"}`
	if err := os.WriteFile(filepath.Join(home, "sessions", "2001.json"), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	fakeProc(t, proc, 2001, "42", "claude")
	var logs bytes.Buffer
	live, err := (&Source{ClaudeHome: home, ProcRoot: proc, Log: slog.New(slog.NewTextHandler(&logs, nil))}).ListLive()
	if err != nil || len(live) != 0 {
		t.Fatalf("file without procStart must be rejected (PID reuse undetectable): live=%+v err=%v", live, err)
	}
	if !strings.Contains(logs.String(), "without procStart") {
		t.Errorf("expected a format warning, logs: %s", logs.String())
	}
}

func TestListLiveWarnsOncePerProblem(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "3001.json")
	if err := os.WriteFile(bad, []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	src := &Source{ClaudeHome: home, ProcRoot: t.TempDir(), Log: slog.New(slog.NewTextHandler(&logs, nil))}
	for i := 0; i < 3; i++ {
		if _, err := src.ListLive(); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(logs.String(), "malformed"); n != 1 {
		t.Fatalf("persistent problem logged %d times, want 1: %s", n, logs.String())
	}
	// The problem goes away, then comes back: it is reported again.
	if err := os.Remove(bad); err != nil {
		t.Fatal(err)
	}
	if _, err := src.ListLive(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := src.ListLive(); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(logs.String(), "malformed"); n != 2 {
		t.Fatalf("recurring problem logged %d times, want 2", n)
	}
}

func TestByPID(t *testing.T) {
	home := fixtureHome(t)
	proc := t.TempDir()
	fakeProc(t, proc, 1002, "6000", "claude")
	src := &Source{ClaudeHome: home, ProcRoot: proc}
	if s, ok := src.ByPID(1002); !ok || s.SessionID != "22222222-2222-4222-8222-222222222222" {
		t.Fatalf("ByPID(1002) = %+v, %v", s, ok)
	}
	if _, ok := src.ByPID(1001); ok {
		t.Fatal("ByPID of a dead process must be false")
	}
}

func TestParseStatStart(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"1 (init) S 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 4242 1 2", "4242", true},
		{"7 (a) b) (c) S 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 99 1", "99", true},
		{"7 (short) S 0 0", "", false},
		{"8 (claude) Z 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 4242 1", "", false}, // zombie: killed, not reaped
		{"9 (claude) X 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 4242 1", "", false},
		{"garbage", "", false},
	}
	for _, c := range cases {
		got, ok := parseStatStart(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("parseStatStart(%q) = %q,%v want %q,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestFindTranscript(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "projects", "-home-user-prj-alpha")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	id := "11111111-1111-4111-8111-111111111111"
	path := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := &Source{ClaudeHome: home}
	if got := src.FindTranscript(id); got != path {
		t.Errorf("FindTranscript = %q, want %q", got, path)
	}
	for _, bad := range []string{"", "*", "../x", "22222222-2222-4222-8222-222222222222"} {
		if got := src.FindTranscript(bad); got != "" {
			t.Errorf("FindTranscript(%q) = %q, want empty", bad, got)
		}
	}
}

func TestListLiveOrdersByProcStartNotPID(t *testing.T) {
	home := t.TempDir()
	proc := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Higher PID started first: oldest process must sort first.
	files := map[int]string{
		3001: `{"pid":3001,"sessionId":"aaaaaaaa-0000-4000-8000-000000000001","procStart":"9000","tmux":"late:@1.%1"}`,
		3002: `{"pid":3002,"sessionId":"aaaaaaaa-0000-4000-8000-000000000002","procStart":"100","tmux":"early:@1.%1"}`,
	}
	for pid, js := range files {
		if err := os.WriteFile(filepath.Join(home, "sessions", strconv.Itoa(pid)+".json"), []byte(js), 0o644); err != nil {
			t.Fatal(err)
		}
		fakeProc(t, proc, pid, map[int]string{3001: "9000", 3002: "100"}[pid], "claude")
	}
	live, err := (&Source{ClaudeHome: home, ProcRoot: proc}).ListLive()
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 2 || live[0].PID != 3002 || live[1].PID != 3001 {
		t.Fatalf("order = %+v, want pid 3002 (procStart 100) before 3001 (procStart 9000)", live)
	}
}

func TestListLiveSkipsInvalidSessionFiles(t *testing.T) {
	cases := map[string]string{
		"empty file":         ``,
		"truncated json":     `{"pid":4001,"sessionId":"x`,
		"json null":          `null`,
		"json array":         `[1,2,3]`,
		"pid zero":           `{"pid":0,"sessionId":"aaaaaaaa-0000-4000-8000-000000000003"}`,
		"missing session id": `{"pid":4002,"procStart":"1","tmux":"noid:@1.%1"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			proc := t.TempDir()
			if err := os.MkdirAll(filepath.Join(home, "sessions"), 0o755); err != nil {
				t.Fatal(err)
			}
			fakeProc(t, proc, 4001, "1", "claude")
			fakeProc(t, proc, 4002, "1", "claude")
			if err := os.WriteFile(filepath.Join(home, "sessions", "4001.json"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			live, err := (&Source{ClaudeHome: home, ProcRoot: proc}).ListLive()
			if err != nil || len(live) != 0 {
				t.Fatalf("live=%+v err=%v; invalid file must be skipped", live, err)
			}
		})
	}
}

func TestListLiveSkipsNonInteractiveKinds(t *testing.T) {
	home := t.TempDir()
	proc := t.TempDir()
	dir := filepath.Join(home, "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	kinds := map[int]string{4001: "interactive", 4002: "bg", 4003: "daemon", 4004: "daemon-worker", 4005: ""}
	for pid, kind := range kinds {
		js := `{"pid":` + strconv.Itoa(pid) + `,"sessionId":"id-` + strconv.Itoa(pid) + `","procStart":"` + strconv.Itoa(pid) +
			`","kind":"` + kind + `","tmux":"t` + strconv.Itoa(pid) + `:@1.%1"}`
		if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(pid)+".json"), []byte(js), 0o644); err != nil {
			t.Fatal(err)
		}
		fakeProc(t, proc, pid, strconv.Itoa(pid), "claude")
	}
	live, err := (&Source{ClaudeHome: home, ProcRoot: proc}).ListLive()
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for _, s := range live {
		pids = append(pids, s.PID)
	}
	// Missing kind (older Claude) counts as interactive.
	if want := []int{4001, 4005}; !slices.Equal(pids, want) {
		t.Fatalf("pids = %v, want %v (bg/daemon processes inherit TMUX and must be skipped)", pids, want)
	}
}

func TestSourceConcurrentUse(t *testing.T) {
	home := fixtureHome(t)
	proc := t.TempDir()
	fakeProc(t, proc, 1002, "6000", "claude")
	src := &Source{ClaudeHome: home, ProcRoot: proc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); src.ListLive() }()
		go func() { defer wg.Done(); src.ByPID(1004) }() // malformed file: warns
	}
	wg.Wait()
}
