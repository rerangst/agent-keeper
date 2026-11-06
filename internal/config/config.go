// Package config holds agent-keeper's tunables (with built-in defaults) and
// resolves the XDG-style paths it reads and writes.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Flag arities understood by the cmdline flag extractor. They mirror how the
// Claude CLI parses its own options.
const (
	ArityBool     = "bool"     // --flag
	ArityValue    = "value"    // --flag X  or  --flag=X
	ArityVariadic = "variadic" // --flag A B C (until the next dash-prefixed arg)
)

// DefaultNudgePrompt is sent to sessions that were mid-task when the machine
// went down. {time} is replaced with the restore time.
const DefaultNudgePrompt = "[agent-keeper] The machine shut down unexpectedly while you were working; " +
	"this session was auto-restored at {time}. Before continuing: " +
	"(1) determine whether your last command/tool call actually completed, " +
	"(2) check git status and any half-edited files, " +
	"(3) check the processes, dev servers, and services you had running and restart them if needed. " +
	"Then continue the interrupted work."

// Config is the user-tunable configuration. Every field is optional in the
// JSON file; missing fields keep their defaults.
type Config struct {
	PollIntervalS    int               `json:"poll_interval_s"`
	RemovalDebounceS int               `json:"removal_debounce_s"`
	StaggerS         int               `json:"stagger_s"`
	NudgeStatuses    []string          `json:"nudge_statuses"`
	NudgePrompt      string            `json:"nudge_prompt"`
	MaxAttempts      int               `json:"max_attempts"`
	BackoffS         []int             `json:"backoff_s"`
	NetworkCheck     string            `json:"network_check"`
	NetworkTimeoutS  int               `json:"network_timeout_s"`
	CwdTimeoutS      int               `json:"cwd_timeout_s"`
	VerifyTimeoutS   int               `json:"verify_timeout_s"`
	ClaudeBin        string            `json:"claude_bin"`
	FlagAllowlist    map[string]string `json:"flag_allowlist"`
}

// Defaults returns the built-in configuration.
func Defaults() Config {
	return Config{
		PollIntervalS:    10,
		RemovalDebounceS: 180,
		StaggerS:         8,
		NudgeStatuses:    []string{"busy", "shell"},
		NudgePrompt:      DefaultNudgePrompt,
		MaxAttempts:      3,
		BackoffS:         []int{30, 120, 600},
		NetworkCheck:     "api.anthropic.com:443",
		NetworkTimeoutS:  300,
		CwdTimeoutS:      300,
		VerifyTimeoutS:   90,
		ClaudeBin:        "claude",
		FlagAllowlist:    DefaultFlagAllowlist(),
	}
}

// DefaultFlagAllowlist lists the launch flags carried over to a restored
// session. Session-selecting and prompt arguments are never carried over.
func DefaultFlagAllowlist() map[string]string {
	return map[string]string{
		"--dangerously-skip-permissions": ArityBool,
		"--permission-mode":              ArityValue,
		"--model":                        ArityValue,
		"--effort":                       ArityValue,
		"--add-dir":                      ArityVariadic,
		"--agent":                        ArityValue,
		"--mcp-config":                   ArityVariadic,
		"--settings":                     ArityValue,
		"--plugin-dir":                   ArityValue,
	}
}

// Load reads path over the defaults. A missing file yields the defaults.
func Load(path string) (Config, error) {
	cfg := Defaults()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	// flag_allowlist entries extend the defaults; an empty arity removes one.
	for flag, arity := range cfg.FlagAllowlist {
		if arity == "" {
			delete(cfg.FlagAllowlist, flag)
		}
	}
	return cfg, cfg.Validate()
}

// Validate rejects values that would make the daemon or restore misbehave.
func (c Config) Validate() error {
	switch {
	case c.PollIntervalS <= 0:
		return errors.New("poll_interval_s must be > 0")
	case c.RemovalDebounceS < 0:
		return errors.New("removal_debounce_s must be >= 0")
	case c.StaggerS < 0:
		return errors.New("stagger_s must be >= 0")
	case c.MaxAttempts <= 0:
		return errors.New("max_attempts must be > 0")
	case c.NetworkTimeoutS < 0:
		return errors.New("network_timeout_s must be >= 0")
	case c.CwdTimeoutS < 0:
		return errors.New("cwd_timeout_s must be >= 0")
	case c.VerifyTimeoutS <= 0:
		return errors.New("verify_timeout_s must be > 0")
	case c.ClaudeBin == "":
		return errors.New("claude_bin must not be empty")
	case strings.HasPrefix(strings.TrimSpace(c.NudgePrompt), "-"):
		return errors.New("nudge_prompt must not start with '-' (claude would read it as a flag)")
	}
	if c.MaxAttempts > 1 && len(c.BackoffS) == 0 {
		return errors.New("backoff_s needs at least one value when max_attempts > 1")
	}
	for _, b := range c.BackoffS {
		if b < 0 {
			return errors.New("backoff_s values must be >= 0")
		}
	}
	for flag, arity := range c.FlagAllowlist {
		if arity != ArityBool && arity != ArityValue && arity != ArityVariadic {
			return fmt.Errorf("flag_allowlist[%q]: arity must be bool, value or variadic", flag)
		}
	}
	return nil
}

// PollInterval is PollIntervalS as a duration.
func (c Config) PollInterval() time.Duration { return seconds(c.PollIntervalS) }

// RemovalDebounce is RemovalDebounceS as a duration.
func (c Config) RemovalDebounce() time.Duration { return seconds(c.RemovalDebounceS) }

func seconds(n int) time.Duration { return time.Duration(n) * time.Second }

// Paths are the filesystem locations agent-keeper uses. Tests override them.
type Paths struct {
	ConfigFile string // ~/.config/agent-keeper/config.json
	StateDir   string // ~/.local/state/agent-keeper
	ClaudeHome string // ~/.claude (or $CLAUDE_CONFIG_DIR)
	ProcRoot   string // /proc
}

// Registry is the registry file inside StateDir.
func (p Paths) Registry() string { return filepath.Join(p.StateDir, "registry.json") }

// DefaultPaths resolves paths from the environment (XDG_CONFIG_HOME,
// XDG_STATE_HOME, CLAUDE_CONFIG_DIR) with the usual HOME-relative fallbacks.
func DefaultPaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, err
	}
	configHome := envOr("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	stateHome := envOr("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	return Paths{
		ConfigFile: filepath.Join(configHome, "agent-keeper", "config.json"),
		StateDir:   filepath.Join(stateHome, "agent-keeper"),
		ClaudeHome: envOr("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude")),
		ProcRoot:   "/proc",
	}, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
