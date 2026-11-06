package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestLoadMissingFileGivesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PollInterval() != 10*time.Second || cfg.RemovalDebounce() != 180*time.Second || cfg.StaggerS != 8 ||
		!slices.Equal(cfg.NudgeStatuses, []string{"busy", "shell"}) || !slices.Equal(cfg.BackoffS, []int{30, 120, 600}) ||
		cfg.MaxAttempts != 3 || cfg.ClaudeBin != "claude" || cfg.NetworkCheck != "api.anthropic.com:443" {
		t.Fatalf("defaults = %+v", cfg)
	}
}

func TestLoadOverridesAndMergesAllowlist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"poll_interval_s": 5, "nudge_statuses": ["busy"], "flag_allowlist": {"--verbose": "bool"}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PollIntervalS != 5 || !slices.Equal(cfg.NudgeStatuses, []string{"busy"}) || cfg.RemovalDebounceS != 180 {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.FlagAllowlist["--verbose"] != ArityBool || cfg.FlagAllowlist["--model"] != ArityValue {
		t.Fatalf("allowlist should extend defaults: %v", cfg.FlagAllowlist)
	}
}

func TestLoadRejectsInvalid(t *testing.T) {
	for name, body := range map[string]string{
		"syntax":    `{"poll_interval_s":`,
		"interval":  `{"poll_interval_s": 0}`,
		"attempts":  `{"max_attempts": 0}`,
		"backoff":   `{"backoff_s": [-1]}`,
		"arity":     `{"flag_allowlist": {"--x": "maybe"}}`,
		"bin":       `{"claude_bin": ""}`,
		"debounce":  `{"removal_debounce_s": -1}`,
		"stagger":   `{"stagger_s": -1}`,
		"verify":    `{"verify_timeout_s": 0}`,
		"network":   `{"network_timeout_s": -1}`,
		"cwd":       `{"cwd_timeout_s": -1}`,
		"nobackoff": `{"backoff_s": []}`,
	} {
		path := filepath.Join(t.TempDir(), name+".json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestDefaultPathsHonorsEnv(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	t.Setenv("XDG_CONFIG_HOME", "/cfg")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "/cl")
	p, err := DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	if p.ConfigFile != "/cfg/agent-keeper/config.json" || p.StateDir != "/home/u/.local/state/agent-keeper" ||
		p.ClaudeHome != "/cl" || p.ProcRoot != "/proc" || p.Registry() != "/home/u/.local/state/agent-keeper/registry.json" {
		t.Fatalf("paths = %+v", p)
	}
}

func TestLoadZeroDebounceAccepted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"removal_debounce_s": 0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil || cfg.RemovalDebounce() != 0 {
		t.Fatalf("debounce=%v err=%v; 0 must mean no grace period", cfg.RemovalDebounce(), err)
	}
}

func TestLoadUnreadablePathIsError(t *testing.T) {
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("reading a directory as config must fail, not fall back to defaults silently")
	}
}

func TestDefaultPathsHomeFallback(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	p, err := DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	if p.ConfigFile != "/home/u/.config/agent-keeper/config.json" || p.StateDir != "/home/u/.local/state/agent-keeper" ||
		p.ClaudeHome != "/home/u/.claude" {
		t.Fatalf("fallback paths = %+v", p)
	}
}

func TestLoadEmptyArityRemovesDefaultFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"flag_allowlist": {"--model": ""}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.FlagAllowlist["--model"]; ok {
		t.Fatal(`"--model": "" must remove the default`)
	}
	if cfg.FlagAllowlist["--dangerously-skip-permissions"] != ArityBool {
		t.Fatal("other defaults must stay")
	}
}
