package systemd

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRenderUnits(t *testing.T) {
	units, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	snap, restore := units[SnapshotUnit], units[RestoreUnit]
	for _, want := range []string{"Type=simple", "ExecStart=%h/.local/bin/agent-keeper daemon", "Restart=always", "RestartSec=5", "WantedBy=default.target"} {
		if !strings.Contains(snap, want) {
			t.Errorf("snapshot unit missing %q:\n%s", want, snap)
		}
	}
	for _, want := range []string{"Type=oneshot", "ExecStart=%h/.local/bin/agent-keeper restore --boot", "KillMode=process", "TimeoutStartSec=infinity", "WantedBy=default.target"} {
		if !strings.Contains(restore, want) {
			t.Errorf("restore unit missing %q:\n%s", want, restore)
		}
	}
	for name, u := range units {
		if strings.Contains(u, "PrivateTmp") || strings.Contains(u, "{{") {
			t.Errorf("%s: PrivateTmp would hide the tmux socket / unrendered template:\n%s", name, u)
		}
	}
}

func TestWriteAndRemoveUnits(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "systemd", "user")
	changed, err := WriteUnits(dir)
	if err != nil || !changed {
		t.Fatalf("first write: changed=%v err=%v", changed, err)
	}
	if changed, err = WriteUnits(dir); err != nil || changed {
		t.Fatalf("second write must be a no-op: changed=%v err=%v", changed, err)
	}
	if err := RemoveUnits(dir); err != nil {
		t.Fatal(err)
	}
	if err := RemoveUnits(dir); err != nil {
		t.Fatalf("remove is idempotent: %v", err)
	}
	left, _ := filepath.Glob(filepath.Join(dir, "agent-keeper*"))
	if len(left) != 0 {
		t.Fatalf("left: %v", left)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFindConflicts(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "systemd", "user")
	// Anonymized shape of a real keeper: a timer runs a script that creates a
	// tmux session and types `claude --continue` into it.
	writeFile(t, filepath.Join(home, ".local", "bin", "keeper-ensure.sh"), `#!/usr/bin/env bash
SESSION=build-demo
if ! /usr/bin/tmux has-session -t "=$SESSION"; then
  /usr/bin/systemd-run --user --scope --quiet /usr/bin/tmux new-session -d -s "$SESSION"
  /usr/bin/tmux send-keys -t "=$SESSION:" "claude --dangerously-skip-permissions --continue" Enter
fi
`)
	writeFile(t, filepath.Join(dir, "keeper-claude.service"), "[Service]\nType=oneshot\nExecStart=%h/.local/bin/keeper-ensure.sh\n")
	writeFile(t, filepath.Join(dir, "keeper-claude.timer"), "[Timer]\nOnBootSec=1min\n[Install]\nWantedBy=timers.target\n")
	writeFile(t, filepath.Join(dir, "inline.service"), "[Service]\nExecStart=-/usr/bin/tmux new-session -d -s x 'claude'\n")
	writeFile(t, filepath.Join(dir, "every-hour.timer"), "[Timer]\nUnit=inline.service\nOnCalendar=hourly\n")
	writeFile(t, filepath.Join(dir, "unrelated.service"), "[Service]\nExecStart=/usr/bin/syncthing serve\n")
	writeFile(t, filepath.Join(dir, "only-tmux.service"), "[Service]\nExecStart=/usr/bin/tmux new -d -s logs htop\n")
	writeFile(t, filepath.Join(dir, "agent-keeper-restore.service"), "[Service]\nExecStart=%h/.local/bin/agent-keeper restore --boot # tmux claude\n")
	writeFile(t, filepath.Join(dir, "binary-script.service"), "[Service]\nExecStart="+filepath.Join(home, "blob")+"\n")
	writeFile(t, filepath.Join(home, "blob"), "tmux\x00claude")

	got, err := FindConflicts(dir, home)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string][]string{}
	for _, c := range got {
		names[c.Service] = c.Timers
		if c.Reason == "" {
			t.Errorf("%s: empty reason", c.Service)
		}
	}
	if len(names) != 2 || !slices.Equal(names["keeper-claude.service"], []string{"keeper-claude.timer"}) ||
		!slices.Equal(names["inline.service"], []string{"every-hour.timer"}) {
		t.Fatalf("conflicts = %+v", got)
	}
}

// fakeSystemctl answers is-enabled/is-active from maps and records calls.
type fakeSystemctl struct {
	enabled, active map[string]string
	calls           []string
}

func (f *fakeSystemctl) Run(_ context.Context, name string, args ...string) (string, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	if name == "systemctl" && len(args) == 3 {
		switch args[1] {
		case "is-enabled":
			return f.enabled[args[2]] + "\n", nil
		case "is-active":
			return f.active[args[2]] + "\n", nil
		}
	}
	return "", nil
}

func TestDisableAndReenableConflicts(t *testing.T) {
	f := &fakeSystemctl{
		enabled: map[string]string{"k.timer": "enabled", "k.service": "static", "j.service": "enabled"},
		active:  map[string]string{"k.timer": "active"},
	}
	c := Ctl{Runner: f}
	done, err := c.DisableConflicts(context.Background(), []Conflict{
		{Service: "k.service", Timers: []string{"k.timer"}},
		{Service: "j.service"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []DisabledUnit{{Unit: "k.timer", WasEnabled: true, WasActive: true}, {Unit: "j.service", WasEnabled: true}}
	if !slices.Equal(done, want) {
		t.Fatalf("done = %+v", done)
	}
	for _, call := range f.calls {
		if strings.Contains(call, "stop") || (strings.Contains(call, "--now") && strings.Contains(call, ".service")) {
			t.Errorf("services must never be stopped (may own live tmux): %s", call)
		}
	}
	if !slices.Contains(f.calls, "systemctl --user disable --now k.timer") || !slices.Contains(f.calls, "systemctl --user disable j.service") {
		t.Errorf("calls = %q", f.calls)
	}

	f.calls = nil
	if err := c.ReenableUnits(context.Background(), done); err != nil {
		t.Fatal(err)
	}
	wantCalls := []string{"systemctl --user enable k.timer", "systemctl --user start k.timer", "systemctl --user enable j.service"}
	if !slices.Equal(f.calls, wantCalls) {
		t.Fatalf("reenable calls = %q", f.calls)
	}
}

func TestSaveDisabledMergesAndKeepsFirstState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disabled-conflicts.json")
	if err := SaveDisabled(path, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("nothing disabled: no file")
	}
	first := []DisabledUnit{{Unit: "k.timer", WasEnabled: true, WasActive: true}}
	if err := SaveDisabled(path, first); err != nil {
		t.Fatal(err)
	}
	// Re-install sees the timer already disabled: the original state must win.
	if err := SaveDisabled(path, []DisabledUnit{{Unit: "k.timer"}, {Unit: "x.service", WasEnabled: true}}); err != nil {
		t.Fatal(err)
	}
	got, err := LoadDisabled(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []DisabledUnit{{Unit: "k.timer", WasEnabled: true, WasActive: true}, {Unit: "x.service", WasEnabled: true}}
	if !slices.Equal(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestLingerAndQuery(t *testing.T) {
	f := &fakeSystemctl{enabled: map[string]string{"a.service": "disabled"}}
	c := Ctl{Runner: f}
	if got := c.Query(context.Background(), "is-enabled", "a.service"); got != "disabled" {
		t.Errorf("Query = %q", got)
	}
	if got := c.Query(context.Background(), "is-active", "missing.service"); got != "unknown" {
		t.Errorf("Query of empty answer = %q", got)
	}
	_ = c.EnableLinger(context.Background(), "u")
	_ = c.Linger(context.Background(), "u")
	if !slices.Contains(f.calls, "loginctl enable-linger u") || !slices.Contains(f.calls, "loginctl show-user u --property=Linger --value") {
		t.Errorf("calls = %q", f.calls)
	}
}

func TestFindConflictsScriptAsArgument(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "systemd", "user")
	writeFile(t, filepath.Join(home, "bin", "ensure.sh"), "tmux new -d -s x\ntmux send-keys -t x claude Enter\n")
	writeFile(t, filepath.Join(dir, "wrapped.service"), "[Service]\nExecStart=/usr/bin/bash %h/bin/ensure.sh --quiet\n")
	writeFile(t, filepath.Join(dir, "pre.service"), "[Service]\nExecStartPre=/usr/bin/tmux new -d -s c claude\nExecStart=/bin/true\n")
	got, err := FindConflicts(dir, home)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, c := range got {
		names = append(names, c.Service)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"pre.service", "wrapped.service"}) {
		t.Fatalf("conflicts = %v", names)
	}
}
