package systemd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rerangst/agent-keeper/internal/registry"
)

// Conflict is another user unit that (re)launches Claude inside tmux. At
// boot it would race agent-keeper and could resume the wrong conversation.
type Conflict struct {
	Service string   `json:"service"`
	Timers  []string `json:"timers,omitempty"`
	Reason  string   `json:"reason"`
}

// DisabledUnit records a unit agent-keeper disabled, so uninstall can
// restore exactly the previous state.
type DisabledUnit struct {
	Unit       string `json:"unit"`
	WasEnabled bool   `json:"was_enabled"`
	WasActive  bool   `json:"was_active"`
}

// maxScriptSize bounds how much of an ExecStart script is inspected.
const maxScriptSize = 1 << 20

// FindConflicts scans user unit files in dir for services whose ExecStart
// line, or the script it runs, mentions both tmux and claude. Timers that
// trigger such a service are attached. home expands the %h specifier.
func FindConflicts(dir, home string) ([]Conflict, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.service"))
	if err != nil {
		return nil, err
	}
	timerFor := timersByService(dir)
	var out []Conflict
	for _, path := range files {
		name := filepath.Base(path)
		if strings.HasPrefix(name, "agent-keeper") {
			continue
		}
		reason := conflictReason(path, home)
		if reason == "" {
			continue
		}
		out = append(out, Conflict{Service: name, Timers: timerFor[name], Reason: reason})
	}
	return out, nil
}

func conflictReason(unitPath, home string) string {
	for _, exec := range execStarts(unitPath) {
		line := strings.ReplaceAll(exec, "%h", home)
		if mentionsBoth(line) {
			return "ExecStart runs claude in tmux"
		}
		// Any argument may be the script (`bash %h/bin/ensure.sh`).
		for i, f := range strings.Fields(line) {
			if i == 0 {
				f = strings.TrimLeft(f, "-@:+!")
			}
			if !strings.HasPrefix(f, "/") {
				continue
			}
			if info, err := os.Stat(f); err == nil && info.Mode().IsRegular() && info.Size() <= maxScriptSize {
				if data, err := os.ReadFile(f); err == nil && isText(data) && mentionsBoth(string(data)) {
					return "ExecStart script " + f + " runs claude in tmux"
				}
			}
		}
	}
	return ""
}

func mentionsBoth(s string) bool {
	return strings.Contains(s, "tmux") && strings.Contains(s, "claude")
}

func isText(b []byte) bool { return !strings.ContainsRune(string(b[:min(len(b), 4096)]), 0) }

// execStarts returns the commands of ExecStart=/ExecStartPre=/ExecStartPost= lines.
func execStarts(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		for _, key := range []string{"ExecStart=", "ExecStartPre=", "ExecStartPost="} {
			if v, ok := strings.CutPrefix(line, key); ok && v != "" {
				out = append(out, v)
			}
		}
	}
	return out
}

// timersByService maps service name → timers that activate it (Unit= or the
// same basename, systemd's default).
func timersByService(dir string) map[string][]string {
	out := map[string][]string{}
	timers, _ := filepath.Glob(filepath.Join(dir, "*.timer"))
	for _, path := range timers {
		name := filepath.Base(path)
		target := strings.TrimSuffix(name, ".timer") + ".service"
		if data, err := os.ReadFile(path); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if v, ok := strings.CutPrefix(strings.TrimSpace(line), "Unit="); ok && v != "" {
					target = strings.TrimSpace(v)
				}
			}
		}
		out[target] = append(out[target], name)
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

// DisableConflicts stops and disables the conflicts' timers and disables
// their services. Services are not stopped: one may own a running tmux
// server, and stopping it would kill live agents. It returns what it changed.
func (c Ctl) DisableConflicts(ctx context.Context, conflicts []Conflict) ([]DisabledUnit, error) {
	var done []DisabledUnit
	var errs []error
	for _, cf := range conflicts {
		for _, t := range cf.Timers {
			d := DisabledUnit{Unit: t, WasEnabled: c.Query(ctx, "is-enabled", t) == "enabled",
				WasActive: c.Query(ctx, "is-active", t) == "active"}
			if _, err := c.Systemctl(ctx, "disable", "--now", t); err != nil {
				errs = append(errs, err)
				continue
			}
			done = append(done, d)
		}
		d := DisabledUnit{Unit: cf.Service, WasEnabled: c.Query(ctx, "is-enabled", cf.Service) == "enabled"}
		if d.WasEnabled {
			if _, err := c.Systemctl(ctx, "disable", cf.Service); err != nil {
				errs = append(errs, err)
				continue
			}
			done = append(done, d)
		}
	}
	return done, errors.Join(errs...)
}

// ReenableUnits restores units recorded by DisableConflicts.
func (c Ctl) ReenableUnits(ctx context.Context, units []DisabledUnit) error {
	var errs []error
	for _, u := range units {
		if u.WasEnabled {
			if _, err := c.Systemctl(ctx, "enable", u.Unit); err != nil {
				errs = append(errs, err)
			}
		}
		if u.WasActive {
			if _, err := c.Systemctl(ctx, "start", u.Unit); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// LoadDisabled reads the bookkeeping file; a missing file means none.
func LoadDisabled(path string) ([]DisabledUnit, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var units []DisabledUnit
	if err := json.Unmarshal(data, &units); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return units, nil
}

// SaveDisabled merges units into the bookkeeping file (a re-install must not
// forget what an earlier install disabled).
func SaveDisabled(path string, units []DisabledUnit) error {
	prev, err := LoadDisabled(path)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, u := range prev {
		seen[u.Unit] = true
	}
	for _, u := range units {
		if !seen[u.Unit] {
			prev = append(prev, u)
		}
	}
	if len(prev) == 0 {
		return nil
	}
	data, err := json.MarshalIndent(prev, "", "  ")
	if err != nil {
		return err
	}
	return registry.WriteFileAtomic(path, append(data, '\n'), 0o600)
}
