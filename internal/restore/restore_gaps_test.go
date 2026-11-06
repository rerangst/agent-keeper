package restore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rerangst/agent-keeper/internal/config"
)

func TestShellQuoteEdgeCases(t *testing.T) {
	cases := map[string]string{
		"":               "''",
		"plain-word_1.2": "plain-word_1.2",
		"a b":            "'a b'",
		"it's":           `'it'\''s'`,
		"'":              `''\'''`,
		"$HOME":          "'$HOME'",
		`a"b`:            `'a"b'`,
		"`id`":           "'`id`'",
		"~/x":            "'~/x'",
		"line\nbreak":    "'line\nbreak'",
		"Zürich Ørsted":  "'Zürich Ørsted'",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMenuKeysMultipleGlyphsAndFarCursor(t *testing.T) {
	cases := []struct {
		name   string
		screen string
		keys   []string
		ok     bool
	}{
		// REPL input glyph below the menu (after blank rows) is outside the menu block.
		{"repl line outside the menu", "  ❯ A\n    B\n    Target\n\n\n❯ $ prompt", []string{"Down", "Down", "Enter"}, true},
		// A cursor separated from the target by blank rows is not this menu's cursor.
		{"cursor in another block is not used", "  ❯ top\n\n\n\n\n\n\n\n    Target", nil, false},
		{"cursor already on target", "    A\n  ❯ Target", []string{"Enter"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			keys, ok := menuKeys(strings.Split(c.screen, "\n"), "Target")
			if ok != c.ok || strings.Join(keys, " ") != strings.Join(c.keys, " ") {
				t.Errorf("menuKeys = %v,%v want %v,%v", keys, ok, c.keys, c.ok)
			}
		})
	}
}

func TestBackoffReusesLastValue(t *testing.T) {
	en := &Engine{Cfg: testCfgWithBackoff([]int{30, 120})}
	want := []time.Duration{30, 120, 120, 120}
	for i, w := range want {
		if got := en.backoff(i + 1); got != w*time.Second {
			t.Errorf("backoff(%d) = %v, want %v", i+1, got, w*time.Second)
		}
	}
	if got := (&Engine{Cfg: testCfgWithBackoff(nil)}).backoff(2); got != 0 {
		t.Errorf("empty backoff_s: got %v, want 0", got)
	}
}

func TestWaitCwdBootAndNonBoot(t *testing.T) {
	h := newHarness(t)
	h.en.Cfg.CwdTimeoutS = 10
	ctx := context.Background()

	// Boot: directory appears on a late mount after the first wait.
	late := filepath.Join(t.TempDir(), "late-mount")
	h.clock.onLong = func(time.Duration) { _ = os.Mkdir(late, 0o755) }
	if !h.en.waitCwd(ctx, late, true) {
		t.Error("boot: cwd that appears during wait reported missing")
	}
	h.clock.onLong = nil

	// Boot: never appears; gives up at cwd_timeout_s.
	gone := filepath.Join(t.TempDir(), "never")
	start := h.clock.Now()
	if h.en.waitCwd(ctx, gone, true) {
		t.Error("boot: missing cwd reported present")
	}
	if el := h.clock.Now().Sub(start); el < 10*time.Second {
		t.Errorf("boot: gave up after %v, want >= cwd_timeout_s", el)
	}

	// Not boot: no waiting at all.
	start = h.clock.Now()
	if h.en.waitCwd(ctx, gone, false) {
		t.Error("non-boot: missing cwd reported present")
	}
	if el := h.clock.Now().Sub(start); el != 0 {
		t.Errorf("non-boot: waited %v", el)
	}
}

func TestWaitNetworkTimeoutContinues(t *testing.T) {
	h := newHarness(t)
	h.en.Cfg.NetworkCheck = "203.0.113.1:443"
	h.en.Cfg.NetworkTimeoutS = 10
	h.en.Dial = func(context.Context, string) error { return context.DeadlineExceeded }

	start := h.clock.Now()
	h.en.waitNetwork(context.Background(), true)
	if el := h.clock.Now().Sub(start); el < 10*time.Second {
		t.Errorf("boot: returned after %v, want >= network_timeout_s", el)
	}

	start = h.clock.Now()
	h.en.waitNetwork(context.Background(), false)
	if el := h.clock.Now().Sub(start); el != 0 {
		t.Errorf("non-boot: waited %v", el)
	}
}

func TestDialogAnsweredAtMostThreeTimes(t *testing.T) {
	trust := pane(t, "trust-folder.txt")
	h := newHarness(t)
	h.put(t, h.entry(t, "alpha", "id-a", false))
	h.en.Cfg.MaxAttempts = 1
	h.en.Cfg.VerifyTimeoutS = 30
	// The fake pops one screen per Enter; five stacked copies keep the dialog up.
	h.tm.behave = func(string, int) claudeScript {
		return claudeScript{dialogs: []string{trust, trust, trust, trust, trust}}
	}
	rep := h.run(t, Options{})
	if r := byName(rep)["alpha"]; r.Result != ResultFailed {
		t.Fatalf("alpha = %+v, want failed", r)
	}
	if n := len(h.tm.keys); n != maxDialogAnswers {
		t.Errorf("trust dialog answered %d times, want %d (keys %v)", n, maxDialogAnswers, h.tm.keys)
	}
}

func TestResumeFailedWithoutBackupNeverFreshStarts(t *testing.T) {
	h := newHarness(t)
	h.put(t, h.entry(t, "alpha", "id-a", false)) // intact transcript: nothing repaired, no backup
	h.tm.behave = func(string, int) claudeScript {
		return claudeScript{failText: "No conversation found with session ID: id-a"}
	}
	rep := h.run(t, Options{})
	r := byName(rep)["alpha"]
	if r.Result != ResultFailed || r.Attempts != h.en.Cfg.MaxAttempts {
		t.Fatalf("alpha = %+v, want failed after %d attempts", r, h.en.Cfg.MaxAttempts)
	}
	if h.tm.launches["alpha"] != h.en.Cfg.MaxAttempts {
		t.Errorf("launches = %d", h.tm.launches["alpha"])
	}
	// The kept (last) session must still resume the same conversation.
	if p := h.tm.panes["alpha"]; p == nil || !strings.Contains(p.cmd, "--resume id-a") {
		t.Errorf("last attempt command lost --resume: %+v", p)
	}
}

func testCfgWithBackoff(b []int) config.Config {
	c := config.Defaults()
	c.BackoffS = b
	return c
}
