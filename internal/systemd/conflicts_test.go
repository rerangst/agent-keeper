package systemd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// claudeInTmuxScript is a minimal launcher that starts claude inside tmux.
const claudeInTmuxScript = "#!/bin/sh\ntmux new-session -d -s demo\ntmux send-keys -t demo claude Enter\n"

func TestFindConflictsExecStartPrefixes(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "systemd", "user")
	writeFile(t, filepath.Join(home, ".local", "bin", "ensure.sh"), claudeInTmuxScript)

	// systemd strips these leading characters from the executable path.
	prefixes := []string{"", "-", "@", ":", "+", "!", "-+", "!!", "@-"}
	for i, prefix := range prefixes {
		name := fmt.Sprintf("prefix%d.service", i)
		writeFile(t, filepath.Join(dir, name), "[Service]\nExecStart="+prefix+"%h/.local/bin/ensure.sh\n")
	}
	got, err := FindConflicts(dir, home)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(prefixes) {
		t.Fatalf("detected %d of %d prefixed units: %+v", len(got), len(prefixes), got)
	}
	for _, c := range got {
		if !strings.Contains(c.Reason, "ensure.sh") {
			t.Errorf("%s: reason %q does not name the script", c.Service, c.Reason)
		}
	}
}

func TestFindConflictsMissingDirIsNotAnError(t *testing.T) {
	got, err := FindConflicts(filepath.Join(t.TempDir(), "absent"), t.TempDir())
	if err != nil || len(got) != 0 {
		t.Fatalf("got %+v, err %v; want no conflicts and no error", got, err)
	}
}

func TestFindConflictsScriptSizeBoundary(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "systemd", "user")
	marker := "tmux claude\n"
	pad := func(n int) string { return marker + strings.Repeat("#", n-len(marker)) }

	writeFile(t, filepath.Join(home, "at-limit.sh"), pad(maxScriptSize))
	writeFile(t, filepath.Join(home, "over-limit.sh"), pad(maxScriptSize+1))
	writeFile(t, filepath.Join(dir, "at-limit.service"), "[Service]\nExecStart="+filepath.Join(home, "at-limit.sh")+"\n")
	writeFile(t, filepath.Join(dir, "over-limit.service"), "[Service]\nExecStart="+filepath.Join(home, "over-limit.sh")+"\n")

	got, err := FindConflicts(dir, home)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range got {
		names = append(names, c.Service)
	}
	if !slices.Equal(names, []string{"at-limit.service"}) {
		t.Fatalf("conflicts = %v; scripts over %d bytes must be skipped, at the limit inspected", names, maxScriptSize)
	}
}

func TestFindConflictsTimersAttachedSorted(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "systemd", "user")
	writeFile(t, filepath.Join(dir, "legacy.service"), "[Service]\nExecStart=/usr/bin/tmux new -d -s x 'claude'\n")
	// Two timers wire to the same service: one by Unit=, one by basename.
	writeFile(t, filepath.Join(dir, "zeta.timer"), "[Timer]\nUnit=legacy.service\n")
	writeFile(t, filepath.Join(dir, "legacy.timer"), "[Timer]\nOnBootSec=1min\n")
	// A timer for an unrelated service must not be attached.
	writeFile(t, filepath.Join(dir, "other.timer"), "[Timer]\nOnBootSec=1min\n")
	writeFile(t, filepath.Join(dir, "other.service"), "[Service]\nExecStart=/usr/bin/true\n")

	got, err := FindConflicts(dir, home)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Service != "legacy.service" {
		t.Fatalf("conflicts = %+v", got)
	}
	if want := []string{"legacy.timer", "zeta.timer"}; !slices.Equal(got[0].Timers, want) {
		t.Fatalf("timers = %v, want %v", got[0].Timers, want)
	}
}

func TestFindConflictsIgnoresExecStartPreAndNonClaudeTmux(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "systemd", "user")
	writeFile(t, filepath.Join(dir, "pre-only.service"),
		"[Service]\nExecStartPre=/usr/bin/tmux kill-server\nExecStart=/usr/bin/true\n")
	writeFile(t, filepath.Join(dir, "tmux-only.service"), "[Service]\nExecStart=/usr/bin/tmux new -d -s logs htop\n")
	got, err := FindConflicts(dir, home)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("unexpected conflicts: %+v", got)
	}
}

// scriptedRunner records calls and fails the ones listed in fail.
type scriptedRunner struct {
	calls   []string
	fail    map[string]error
	enabled map[string]string
	active  map[string]string
}

func (s *scriptedRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	call := name + " " + strings.Join(args, " ")
	s.calls = append(s.calls, call)
	if err := s.fail[call]; err != nil {
		return "", err
	}
	if len(args) == 3 {
		switch args[1] {
		case "is-enabled":
			return s.enabled[args[2]] + "\n", nil
		case "is-active":
			return s.active[args[2]] + "\n", nil
		}
	}
	return "", nil
}

func TestDisableConflictsErrorsAreCollected(t *testing.T) {
	run := &scriptedRunner{
		enabled: map[string]string{"k.timer": "enabled", "k.service": "enabled", "j.service": "enabled"},
		active:  map[string]string{"k.timer": "active"},
		fail: map[string]error{
			"systemctl --user disable --now k.timer": errors.New("bus down"),
			"systemctl --user disable j.service":     errors.New("denied"),
		},
	}
	done, err := Ctl{Runner: run}.DisableConflicts(context.Background(), []Conflict{
		{Service: "k.service", Timers: []string{"k.timer"}},
		{Service: "j.service"},
	})
	if err == nil || !strings.Contains(err.Error(), "bus down") || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("err = %v, want both failures joined", err)
	}
	// The failed timer is not recorded, so uninstall never re-enables what was not disabled.
	for _, d := range done {
		if d.Unit == "k.timer" || d.Unit == "j.service" {
			t.Errorf("failed unit recorded as disabled: %+v", d)
		}
	}
	// Failure on the timer must not stop the service from being processed.
	if !slices.Contains(run.calls, "systemctl --user disable k.service") {
		t.Errorf("service skipped after timer failure: calls=%q", run.calls)
	}
}

func TestDisableConflictsSkipsStaticServices(t *testing.T) {
	run := &scriptedRunner{enabled: map[string]string{"s.service": "static"}}
	done, err := Ctl{Runner: run}.DisableConflicts(context.Background(), []Conflict{{Service: "s.service"}})
	if err != nil || len(done) != 0 {
		t.Fatalf("done=%+v err=%v; static unit needs no change", done, err)
	}
	for _, c := range run.calls {
		if strings.Contains(c, " disable ") || strings.Contains(c, " stop ") {
			t.Errorf("unexpected call %q", c)
		}
	}
}

func TestReenableContinuesAfterError(t *testing.T) {
	run := &scriptedRunner{fail: map[string]error{"systemctl --user enable a.timer": errors.New("nope")}}
	err := Ctl{Runner: run}.ReenableUnits(context.Background(), []DisabledUnit{
		{Unit: "a.timer", WasEnabled: true, WasActive: true},
		{Unit: "b.service", WasEnabled: true},
	})
	if err == nil {
		t.Fatal("enable failure must be reported")
	}
	if !slices.Contains(run.calls, "systemctl --user start a.timer") || !slices.Contains(run.calls, "systemctl --user enable b.service") {
		t.Fatalf("later units skipped after an error: %q", run.calls)
	}
}

func TestLoadDisabledMissingAndCorrupt(t *testing.T) {
	dir := t.TempDir()
	got, err := LoadDisabled(filepath.Join(dir, "absent.json"))
	if err != nil || got != nil {
		t.Fatalf("missing file: got %+v err %v", got, err)
	}
	path := filepath.Join(dir, "disabled-conflicts.json")
	writeFile(t, path, "{not json")
	if _, err := LoadDisabled(path); err == nil {
		t.Fatal("corrupt bookkeeping must be an error")
	}
	if err := SaveDisabled(path, []DisabledUnit{{Unit: "x.timer"}}); err == nil {
		t.Fatal("SaveDisabled must not overwrite a corrupt file")
	}
	if data, _ := os.ReadFile(path); string(data) != "{not json" {
		t.Fatalf("corrupt file was rewritten: %q", data)
	}
}

func TestSaveDisabledPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disabled-conflicts.json")
	if err := SaveDisabled(path, []DisabledUnit{{Unit: "x.timer", WasEnabled: true}}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("mode = %o, want 600", mode)
	}
}
