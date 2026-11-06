package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rerangst/agent-keeper/internal/config"
	"github.com/rerangst/agent-keeper/internal/registry"
	"github.com/rerangst/agent-keeper/internal/systemd"
)

// fakeSystemd stands in for systemctl --user and loginctl. It keeps enable,
// active and linger state so install/uninstall round-trips are observable.
type fakeSystemd struct {
	calls       []string
	enabled     map[string]bool
	active      map[string]bool
	static      map[string]bool
	lingerOn    bool
	lingerFails bool
	unreachable bool
}

func newFakeSystemd() *fakeSystemd {
	return &fakeSystemd{enabled: map[string]bool{}, active: map[string]bool{}, static: map[string]bool{}}
}

func (f *fakeSystemd) Run(_ context.Context, name string, args ...string) (string, error) {
	f.calls = append(f.calls, strings.TrimSpace(name+" "+strings.Join(args, " ")))
	if name == "loginctl" {
		switch args[0] {
		case "show-user":
			if f.lingerOn {
				return "yes\n", nil
			}
			return "no\n", nil
		case "enable-linger":
			if f.lingerFails {
				return "", errors.New("access denied")
			}
			f.lingerOn = true
		}
		return "", nil
	}
	if name != "systemctl" || len(args) == 0 || args[0] != "--user" {
		return "", nil
	}
	args = args[1:]
	switch args[0] {
	case "show":
		if f.unreachable {
			return "", errors.New("no bus")
		}
		return "Version=255\n", nil
	case "is-enabled":
		switch {
		case f.static[args[1]]:
			return "static\n", nil
		case f.enabled[args[1]]:
			return "enabled\n", nil
		}
		return "disabled\n", nil
	case "is-active":
		if f.active[args[1]] {
			return "active\n", nil
		}
		return "inactive\n", nil
	case "enable":
		for _, u := range args[1:] {
			f.enabled[u] = true
		}
	case "disable":
		now := slices.Contains(args, "--now")
		for _, u := range args[1:] {
			if u == "--now" {
				continue
			}
			f.enabled[u] = false
			if now {
				f.active[u] = false
			}
		}
	case "start", "restart":
		f.active[args[1]] = true
	case "stop":
		f.active[args[1]] = false
	}
	return "", nil
}

// hostEnv builds an env whose HOME, XDG dirs and PATH all live in temp dirs.
// tools lists the stub executables present on PATH.
func hostEnv(t *testing.T, fake *fakeSystemd, tools ...string) (*env, string) {
	t.Helper()
	bin := t.TempDir()
	for _, tool := range tools {
		if err := os.WriteFile(filepath.Join(bin, tool), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	home := t.TempDir()
	t.Setenv("PATH", bin)
	t.Setenv("HOME", home)
	t.Setenv("USER", "tester") // ignored: the account comes from the real uid
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("SHELL", "/bin/sh")
	paths, err := config.DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	return &env{paths: paths, cfg: cfg, log: newLogger(&bytes.Buffer{}), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
		runner: fake, ctl: &systemd.Ctl{Runner: fake}}, home
}

var allTools = []string{"tmux", "systemd-run", "systemctl"}

// writeKeeperFixture creates a user unit plus the script it runs, shaped like
// the real legacy keeper: a timer starts a service that runs claude in tmux.
func writeKeeperFixture(t *testing.T, home string) string {
	t.Helper()
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(home, ".local", "bin", "legacy-ensure.sh")
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte(claudeTmuxScript), 0o755); err != nil {
		t.Fatal(err)
	}
	must := func(name, content string) {
		if err := os.WriteFile(filepath.Join(unitDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("legacy-keeper.service", "[Service]\nType=oneshot\nExecStart=%h/.local/bin/legacy-ensure.sh\n")
	must("legacy-keeper.timer", "[Timer]\nOnBootSec=1min\n[Install]\nWantedBy=timers.target\n")
	return unitDir
}

const claudeTmuxScript = "#!/bin/sh\ntmux new-session -d -s legacy\ntmux send-keys -t legacy 'claude --continue' Enter\n"

func TestInstallFlagParsing(t *testing.T) {
	fake := newFakeSystemd()
	e, _ := hostEnv(t, fake, allTools...)
	if code := e.cmdInstall([]string{"-h"}); code != 0 {
		t.Errorf("install -h: code=%d, want 0", code)
	}
	if code := e.cmdUninstall([]string{"-h"}); code != 0 {
		t.Errorf("uninstall -h: code=%d, want 0", code)
	}
	if code := e.cmdInstall([]string{"--bogus"}); code != 2 {
		t.Errorf("install --bogus: code=%d, want 2", code)
	}
	if code := e.cmdUninstall([]string{"--bogus"}); code != 2 {
		t.Errorf("uninstall --bogus: code=%d, want 2", code)
	}
	if len(fake.calls) != 0 {
		t.Errorf("flag errors must not touch systemd: %q", fake.calls)
	}
}

func TestInstallHappyPathEnablesLingerAndCopiesBinary(t *testing.T) {
	fake := newFakeSystemd()
	e, home := hostEnv(t, fake, allTools...)
	if code := e.cmdInstall(nil); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, e.stderr.(*bytes.Buffer).String())
	}
	for _, want := range []string{
		"systemctl --user daemon-reload",
		"systemctl --user enable agent-keeper-snapshot.service agent-keeper-restore.service",
		"systemctl --user restart agent-keeper-snapshot.service",
		"loginctl enable-linger " + currentUser(),
	} {
		if !slices.Contains(fake.calls, want) {
			t.Errorf("missing call %q in %q", want, fake.calls)
		}
	}
	if !fake.enabled[systemd.SnapshotUnit] || !fake.enabled[systemd.RestoreUnit] || !fake.active[systemd.SnapshotUnit] {
		t.Errorf("units not enabled/active: enabled=%v active=%v", fake.enabled, fake.active)
	}
	for _, u := range systemd.Units {
		if _, err := os.Stat(filepath.Join(home, ".config", "systemd", "user", u)); err != nil {
			t.Errorf("unit file %s: %v", u, err)
		}
	}
	info, err := os.Stat(filepath.Join(home, ".local", "bin", "agent-keeper"))
	if err != nil {
		t.Fatalf("binary not installed: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("binary mode = %o, want 755", info.Mode().Perm())
	}
	if out := e.stdout.(*bytes.Buffer).String(); !strings.Contains(out, "linger enabled") {
		t.Errorf("stdout = %q", out)
	}
}

func TestInstallLingerFailureMarksIncomplete(t *testing.T) {
	fake := newFakeSystemd()
	fake.lingerFails = true
	e, home := hostEnv(t, fake, allTools...)
	if code := e.cmdInstall(nil); code != 1 {
		t.Fatalf("code=%d, want 1", code)
	}
	stderr := e.stderr.(*bytes.Buffer).String()
	if !strings.Contains(stderr, "INSTALL INCOMPLETE") || !strings.Contains(stderr, "sudo loginctl enable-linger "+currentUser()) {
		t.Errorf("stderr lacks incomplete notice and fix command: %q", stderr)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "systemd", "user", systemd.SnapshotUnit)); err != nil {
		t.Errorf("units should still be written when linger fails: %v", err)
	}
}

func TestInstallFailsWithoutUserManagerOrTools(t *testing.T) {
	fake := newFakeSystemd()
	fake.unreachable = true
	e, home := hostEnv(t, fake, allTools...)
	if code := e.cmdInstall(nil); code != 1 {
		t.Fatalf("unreachable: code=%d, want 1", code)
	}
	if !strings.Contains(e.stderr.(*bytes.Buffer).String(), "no systemd user instance reachable") {
		t.Errorf("stderr = %q", e.stderr)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "systemd", "user")); !os.IsNotExist(err) {
		t.Errorf("units written although systemd is unreachable: %v", err)
	}

	fake2 := newFakeSystemd()
	e2, _ := hostEnv(t, fake2, "tmux", "systemctl") // systemd-run missing
	if code := e2.cmdInstall(nil); code != 1 {
		t.Fatalf("missing tool: code=%d, want 1", code)
	}
	if !strings.Contains(e2.stderr.(*bytes.Buffer).String(), "systemd-run not found on PATH") {
		t.Errorf("stderr = %q", e2.stderr)
	}
}

func TestInstallDisablesConflictAndUninstallRestores(t *testing.T) {
	fake := newFakeSystemd()
	fake.enabled["legacy-keeper.timer"] = true
	fake.active["legacy-keeper.timer"] = true
	fake.static["legacy-keeper.service"] = true
	fake.lingerOn = true
	e, home := hostEnv(t, fake, allTools...)
	unitDir := writeKeeperFixture(t, home)

	if code := e.cmdInstall(nil); code != 0 {
		t.Fatalf("install code=%d stderr=%q", code, e.stderr.(*bytes.Buffer).String())
	}
	if fake.enabled["legacy-keeper.timer"] || fake.active["legacy-keeper.timer"] {
		t.Fatal("conflicting timer must be disabled and stopped")
	}
	for _, c := range fake.calls {
		if strings.Contains(c, " stop ") || strings.Contains(c, "disable legacy-keeper.service") {
			t.Errorf("conflict handling touched the service: %q", c)
		}
	}
	if _, err := os.Stat(filepath.Join(unitDir, "legacy-keeper.timer")); err != nil {
		t.Errorf("conflict files must be kept: %v", err)
	}
	if out := e.stdout.(*bytes.Buffer).String(); !strings.Contains(out, "conflicting unit legacy-keeper.service") ||
		!strings.Contains(out, "disabled legacy-keeper.timer") {
		t.Errorf("stdout does not name the conflict: %q", out)
	}
	recorded, err := systemd.LoadDisabled(e.disabledConflictsPath())
	if err != nil || len(recorded) != 1 || !recorded[0].WasEnabled || !recorded[0].WasActive {
		t.Fatalf("bookkeeping = %+v, err %v", recorded, err)
	}

	// A second install sees the timer already disabled; the first state must survive.
	if code := e.cmdInstall(nil); code != 0 {
		t.Fatalf("re-install code=%d", code)
	}
	recorded, _ = systemd.LoadDisabled(e.disabledConflictsPath())
	if len(recorded) != 1 || !recorded[0].WasEnabled {
		t.Fatalf("re-install overwrote original state: %+v", recorded)
	}

	fake.calls = nil
	if code := e.cmdUninstall(nil); code != 0 {
		t.Fatalf("uninstall code=%d stderr=%q", code, e.stderr.(*bytes.Buffer).String())
	}
	for _, want := range []string{"systemctl --user enable legacy-keeper.timer", "systemctl --user start legacy-keeper.timer"} {
		if !slices.Contains(fake.calls, want) {
			t.Errorf("uninstall missing %q: %q", want, fake.calls)
		}
	}
	if !fake.enabled["legacy-keeper.timer"] || !fake.active["legacy-keeper.timer"] {
		t.Error("timer not restored to its prior enabled+active state")
	}
	if _, err := os.Stat(e.disabledConflictsPath()); !os.IsNotExist(err) {
		t.Errorf("bookkeeping must be removed after successful re-enable: %v", err)
	}
	for _, u := range systemd.Units {
		if _, err := os.Stat(filepath.Join(unitDir, u)); !os.IsNotExist(err) {
			t.Errorf("unit %s not removed: %v", u, err)
		}
	}
}

func TestUninstallPurgeAndKeep(t *testing.T) {
	fake := newFakeSystemd()
	e, _ := hostEnv(t, fake, allTools...)
	reg := registry.Store{Path: e.paths.Registry()}
	seed(t, reg, &registry.Entry{TmuxSession: "alpha", State: registry.StateActive})

	if code := e.cmdUninstall(nil); code != 0 {
		t.Fatalf("uninstall code=%d", code)
	}
	if _, err := os.Stat(e.paths.Registry()); err != nil {
		t.Errorf("registry must survive plain uninstall: %v", err)
	}
	if code := e.cmdUninstall([]string{"--purge"}); code != 0 {
		t.Fatalf("purge code=%d", code)
	}
	for _, dir := range []string{e.paths.StateDir, filepath.Dir(e.paths.ConfigFile)} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("--purge left %s", dir)
		}
	}
}

func TestUninstallCorruptBookkeepingIsKept(t *testing.T) {
	fake := newFakeSystemd()
	e, _ := hostEnv(t, fake, allTools...)
	if err := os.MkdirAll(e.paths.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.disabledConflictsPath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := e.cmdUninstall(nil); code != 1 {
		t.Errorf("code=%d, want 1 for unreadable bookkeeping", code)
	}
	// Reading failed, so the record of what was disabled must not be deleted.
	if _, err := os.Stat(e.disabledConflictsPath()); err != nil {
		t.Errorf("unreadable bookkeeping was deleted; disabled units can no longer be re-enabled: %v", err)
	}
}

func TestStatusReportsDaemonAndLinger(t *testing.T) {
	fake := newFakeSystemd()
	fake.active[systemd.SnapshotUnit] = true
	fake.enabled[systemd.RestoreUnit] = true
	e, _ := hostEnv(t, fake, allTools...)
	if code := e.cmdStatus(nil); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := e.stdout.(*bytes.Buffer).String()
	if !strings.Contains(out, "snapshot daemon: active   restore at boot: enabled   linger: off (sessions are restored only after you log in; run: sudo loginctl enable-linger "+currentUser()+")") {
		t.Errorf("status line = %q", out)
	}

	fake.lingerOn = true
	e.stdout = &bytes.Buffer{}
	if code := e.cmdStatus(nil); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if out := e.stdout.(*bytes.Buffer).String(); !strings.Contains(out, "linger: on") {
		t.Errorf("status line = %q", out)
	}
}

func TestStatusCorruptRegistryExitsOne(t *testing.T) {
	fake := newFakeSystemd()
	e, _ := hostEnv(t, fake, allTools...)
	if err := os.MkdirAll(e.paths.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.paths.Registry(), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := e.cmdStatus(nil); code != 1 {
		t.Fatalf("code=%d, want 1", code)
	}
	if !strings.Contains(e.stderr.(*bytes.Buffer).String(), "agent-keeper: status:") {
		t.Errorf("stderr = %q", e.stderr)
	}
}

func TestSetStateExitCodesAndMessages(t *testing.T) {
	paths := testPaths(t)
	store := registry.Store{Path: paths.Registry()}
	seed(t, store, &registry.Entry{TmuxSession: "alpha", State: registry.StateActive},
		&registry.Entry{TmuxSession: "beta", State: registry.StateActive})
	steps := []struct {
		args   []string
		code   int
		stdout string
		stderr string
	}{
		{[]string{"exclude", "alpha"}, 0, "alpha: excluded (never restored", ""},
		{[]string{"include", "alpha"}, 0, "alpha: included again", ""},
		{[]string{"include", "alpha"}, 1, "", "alpha is active, not excluded"},
		{[]string{"exclude", "ghost"}, 1, "", `no tracked session named "ghost"`},
		{[]string{"forget", "beta"}, 0, "beta: forgotten", ""},
		{[]string{"forget", "beta"}, 1, "", `no tracked session named "beta"`},
		{[]string{"exclude"}, 2, "", "usage: agent-keeper exclude <tmux-session>"},
		{[]string{"exclude", "a", "b"}, 2, "", "usage: agent-keeper exclude"},
		{[]string{"include", "--bogus", "alpha"}, 2, "", "flag provided but not defined"},
		{[]string{"forget", "-h"}, 0, "", ""},
	}
	for _, s := range steps {
		var out, errOut bytes.Buffer
		if code := dispatch(s.args, paths, &out, &errOut); code != s.code {
			t.Errorf("%v: code=%d want %d (stderr %q)", s.args, code, s.code, errOut.String())
		}
		if s.stdout != "" && !strings.Contains(out.String(), s.stdout) {
			t.Errorf("%v: stdout %q lacks %q", s.args, out.String(), s.stdout)
		}
		if s.stderr != "" && !strings.Contains(errOut.String(), s.stderr) {
			t.Errorf("%v: stderr %q lacks %q", s.args, errOut.String(), s.stderr)
		}
	}
}
