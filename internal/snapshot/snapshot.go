// Package snapshot keeps the registry in step with the live Claude sessions
// while the machine runs.
package snapshot

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rerangst/agent-keeper/internal/claudesessions"
	"github.com/rerangst/agent-keeper/internal/config"
	"github.com/rerangst/agent-keeper/internal/registry"
)

// Options control a merge.
type Options struct {
	Debounce  time.Duration     // absence needed before an entry is marked stopped
	Allowlist map[string]string // launch flags to carry over
	Log       *slog.Logger
}

// Noise remembers which notices were already logged, so a persistent
// condition (e.g. two Claude processes in one tmux session) is reported once
// instead of on every poll. A nil *Noise logs every time.
type Noise struct {
	logged, current map[string]bool
}

func (n *Noise) begin() {
	if n != nil {
		n.current = map[string]bool{}
	}
}

// end forgets conditions that went away, so they are reported again if they return.
func (n *Noise) end() {
	if n != nil {
		n.logged = n.current
	}
}

func (n *Noise) first(key string) bool {
	if n == nil {
		return true
	}
	n.current[key] = true
	return !n.logged[key]
}

// Merge folds live sessions into reg. bootID identifies the running boot:
// an entry is marked stopped only if it was seen alive during this boot
// (last_seen_boot == bootID), has been absent longer than the debounce, and
// no restore is in progress for it. Entries from an earlier boot belong to
// restore and are left untouched. Live sessions outside tmux are ignored.
// When two live sessions claim the same tmux name or session id, the first
// (oldest process) wins and the conflict is logged.
func Merge(reg *registry.Registry, live []claudesessions.Session, now time.Time, bootID string, noise *Noise, opts Options) {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	noise.begin()
	defer noise.end()

	byName := map[string]claudesessions.Session{}
	byID := map[string]string{} // session id -> tmux name
	for _, s := range live {
		if s.TmuxSession == "" {
			continue
		}
		if prev, dup := byName[s.TmuxSession]; dup {
			if noise.first("tmux\x00" + s.TmuxSession + "\x00" + s.SessionID) {
				log.Warn("two live sessions in one tmux session; keeping the first",
					"tmux", s.TmuxSession, "kept_pid", prev.PID, "ignored_pid", s.PID)
			}
			continue
		}
		if prevName, dup := byID[s.SessionID]; dup {
			if noise.first("id\x00" + s.SessionID + "\x00" + s.TmuxSession) {
				log.Warn("one session id in two tmux sessions; keeping the first",
					"session_id", s.SessionID, "kept_tmux", prevName, "ignored_tmux", s.TmuxSession)
			}
			continue
		}
		byName[s.TmuxSession] = s
		byID[s.SessionID] = s.TmuxSession
	}

	for name, s := range byName {
		e := reg.Entries[name]
		if e == nil {
			e = &registry.Entry{TmuxSession: name, State: registry.StateActive, FirstSeen: now}
			reg.Entries[name] = e
			log.Info("tracking new session", "tmux", name, "session_id", s.SessionID, "cwd", s.Cwd)
		}
		if e.SessionID != "" && e.SessionID != s.SessionID {
			log.Info("session id changed", "tmux", name, "old", e.SessionID, "new", s.SessionID)
		}
		if e.State == registry.StateStopped {
			e.State = registry.StateActive
			log.Info("stopped session is live again", "tmux", name)
		}
		e.SessionID = s.SessionID
		// A key missing from one read keeps the known value: restore needs cwd.
		setIfNonEmpty(&e.Cwd, s.Cwd)
		setIfNonEmpty(&e.Name, s.Name)
		setIfNonEmpty(&e.NameSource, s.NameSource)
		setIfNonEmpty(&e.LastStatus, s.Status)
		setIfNonEmpty(&e.ClaudeVersion, s.Version)
		e.RemoteControl = s.RemoteControl() // absent bridge id means RC is off
		e.BridgeSessionID = s.BridgeSessionID
		e.LastSeen = now
		e.LastSeenBoot = bootID
		if s.Argv != nil { // keep previous flags when the cmdline was unreadable
			kept, unknown := claudesessions.ExtractFlags(s.Argv, opts.Allowlist)
			if kept == nil {
				kept = []string{}
			}
			if len(unknown) > 0 && noise.first("flags\x00"+name+"\x00"+strings.Join(unknown, "\x00")) {
				log.Info("launch flags not carried over", "tmux", name, "flags", unknown)
			}
			e.Flags = kept
		}
		if e.Flags == nil {
			e.Flags = []string{}
		}
	}

	if bootID == "" {
		return // cannot tell which entries were seen this boot: mark nothing
	}
	for name, e := range reg.Entries {
		if e == nil || e.State != registry.StateActive || e.LastSeenBoot != bootID || e.Restore.InProgress {
			continue
		}
		if _, live := byName[name]; live {
			continue
		}
		if now.Sub(e.LastSeen) > opts.Debounce {
			e.State = registry.StateStopped
			log.Info("session closed; marked stopped", "tmux", name, "last_seen", e.LastSeen)
		}
	}
}

func setIfNonEmpty(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}

// seenThisBoot reports whether any entry was seen alive during bootID.
func seenThisBoot(reg *registry.Registry, bootID string) bool {
	for _, e := range reg.Entries {
		if e != nil && bootID != "" && e.LastSeenBoot == bootID {
			return true
		}
	}
	return false
}

// CurrentBootID returns the kernel's random boot id, or "" if unreadable.
func CurrentBootID(procRoot string) string {
	data, err := os.ReadFile(filepath.Join(procRoot, "sys", "kernel", "random", "boot_id"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// Snapshotter runs one merge pass against the real session source and store.
type Snapshotter struct {
	Source claudesessions.Source
	Store  registry.Store
	Opts   Options
	BootID string
	Noise  Noise
	Now    func() time.Time
}

// Once lists live sessions and merges them into the registry under lock.
func (s *Snapshotter) Once() (*registry.Registry, error) {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	live, listErr := s.Source.ListLive()
	if listErr != nil && !errors.Is(listErr, claudesessions.ErrNoSessionsDir) {
		return nil, listErr
	}
	var out *registry.Registry
	err := s.Store.Update(func(reg *registry.Registry) error {
		if listErr != nil && seenThisBoot(reg, s.BootID) {
			// Sessions were seen this boot, so a vanished directory is an
			// anomaly, not "every session closed": skip the pass.
			return listErr
		}
		// Otherwise a missing directory just means Claude never ran here yet.
		reg.StampHost()
		Merge(reg, live, now().UTC(), s.BootID, &s.Noise, s.Opts)
		out = reg
		return nil
	})
	return out, err
}

// RunDaemon polls every interval until ctx is cancelled. Each pass is
// persisted immediately, so cancellation (SIGTERM at shutdown) just returns:
// it never runs another pass, because Claude processes exiting during
// shutdown must not be recorded as user-closed.
func RunDaemon(ctx context.Context, s *Snapshotter, interval time.Duration, log *slog.Logger) {
	log.Info("snapshot daemon started", "poll_interval", interval, "debounce", s.Opts.Debounce, "boot_id", s.BootID)
	t := time.NewTicker(interval)
	defer t.Stop()
	lastErr := ""
	for ctx.Err() == nil { // checked first: a tick and a cancel can race in select
		_, err := s.Once()
		switch {
		case err != nil && errKey(err) != lastErr:
			log.Error("snapshot pass failed; registry unchanged", "err", err)
			lastErr = errKey(err)
		case err == nil && lastErr != "":
			log.Info("snapshot pass recovered")
			lastErr = ""
		}
		select {
		case <-ctx.Done():
		case <-t.C:
		}
	}
	log.Info("snapshot daemon stopping; registry left as last written")
}

// errKey identifies an error for log de-duplication, ignoring file paths
// (atomic writes use random temp names that differ on every pass).
func errKey(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Op + ": " + pe.Err.Error()
	}
	return err.Error()
}

// OptionsFromConfig builds merge options from the configuration.
func OptionsFromConfig(cfg config.Config, log *slog.Logger) Options {
	return Options{Debounce: cfg.RemovalDebounce(), Allowlist: cfg.FlagAllowlist, Log: log}
}
