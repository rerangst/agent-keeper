// Package claudesessions is the only package that knows Claude Code
// internals: the per-process session registry (~/.claude/sessions/<pid>.json),
// the process command line, and transcript locations. When Claude Code changes
// these formats, this package is the one place to update.
package claudesessions

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Session is one live Claude Code process as described by its session file.
type Session struct {
	PID             int
	SessionID       string
	Cwd             string
	Name            string
	NameSource      string
	ProcStart       string
	Tmux            string // raw "<session>:@<window>.%<pane>"
	TmuxSession     string // part of Tmux before the first ':'
	Status          string // idle | busy | shell
	BridgeSessionID string // non-empty when Remote Control is on
	Version         string // Claude Code version that wrote the file
	Argv            []string
}

// RemoteControl reports whether the session has an active Remote Control bridge.
func (s Session) RemoteControl() bool { return s.BridgeSessionID != "" }

// sessionFile mirrors the subset of ~/.claude/sessions/<pid>.json we use.
// Unknown keys are ignored; missing keys stay empty.
type sessionFile struct {
	PID             int    `json:"pid"`
	SessionID       string `json:"sessionId"`
	Cwd             string `json:"cwd"`
	Name            string `json:"name"`
	NameSource      string `json:"nameSource"`
	ProcStart       string `json:"procStart"`
	Tmux            string `json:"tmux"`
	Status          string `json:"status"`
	BridgeSessionID string `json:"bridgeSessionId"`
	Version         string `json:"version"`
	Kind            string `json:"kind"` // interactive | bg | daemon | daemon-worker
}

// ErrNoSessionsDir means ~/.claude/sessions does not exist (Claude Code never
// ran an interactive session here, or the Claude home moved).
var ErrNoSessionsDir = errors.New("claude sessions directory not found")

// Source reads Claude session state from a Claude home and a proc root.
// Use it through a pointer (it holds a mutex): it remembers which warnings it
// already logged so a persistent problem is reported once, not on every poll.
// Safe for concurrent use.
type Source struct {
	ClaudeHome string // usually ~/.claude
	ProcRoot   string // usually /proc
	Log        *slog.Logger

	mu     sync.Mutex        // guards warned; Source may be shared by goroutines
	warned map[string]string // session file -> last warning logged for it
}

func (s *Source) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// warn logs msg for a session file unless the same warning was already logged
// for it, and records it in next (the set of current problems).
func (s *Source) warn(next map[string]string, path, msg string, args ...any) {
	if next != nil {
		next[path] = msg
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.warned[path] == msg {
		return
	}
	if s.warned == nil {
		s.warned = map[string]string{}
	}
	s.warned[path] = msg
	s.log().Warn(msg, append([]any{"file", path}, args...)...)
}

// ListLive returns every session whose process is alive (same PID and same
// process start time, so a reused PID after reboot is rejected), sorted by
// process start (oldest first). Sessions outside tmux are included with an
// empty TmuxSession; callers decide whether to track them. Unreadable or
// partial files are skipped with a warning. A missing sessions directory
// yields ErrNoSessionsDir and an unreadable one an error, never an empty
// list, so callers cannot mistake an I/O problem for "every session closed".
func (s *Source) ListLive() ([]Session, error) {
	dir := filepath.Join(s.ClaudeHome, "sessions")
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoSessionsDir
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	next := map[string]string{}
	var out []Session
	for _, ent := range ents {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		sess, ok := s.readLive(filepath.Join(dir, ent.Name()), next)
		if ok {
			out = append(out, sess)
		}
	}
	s.mu.Lock()
	s.warned = next // forget problems that went away, so they warn again if they return
	s.mu.Unlock()
	sort.SliceStable(out, func(i, j int) bool {
		a, b := procStartNum(out[i].ProcStart), procStartNum(out[j].ProcStart)
		if a != b {
			return a < b
		}
		return out[i].PID < out[j].PID
	})
	return out, nil
}

func (s *Source) readLive(path string, next map[string]string) (Session, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) { // vanished between listing and read is normal
			s.warn(next, path, "skip unreadable session file", "err", err)
		}
		return Session{}, false
	}
	var f sessionFile
	if err := json.Unmarshal(data, &f); err != nil {
		s.warn(next, path, "skip malformed session file", "err", err)
		return Session{}, false
	}
	if f.PID <= 0 || f.SessionID == "" {
		s.warn(next, path, "skip session file without pid/sessionId")
		return Session{}, false
	}
	if f.Kind != "" && f.Kind != "interactive" {
		// Background/daemon processes inherit TMUX from the pane they were
		// spawned in; only the interactive agent is the pane's conversation.
		return Session{}, false
	}
	start, alive := s.procStart(f.PID)
	if !alive {
		return Session{}, false // stale file of a dead process
	}
	if f.ProcStart == "" {
		// Without procStart a PID reused after reboot is indistinguishable
		// from the original Claude process; a false "live" would block restore.
		s.warn(next, path, "skip session file without procStart (unsupported Claude Code format?)", "pid", f.PID)
		return Session{}, false
	}
	if f.ProcStart != start {
		return Session{}, false // PID reused by another process
	}
	argv, err := s.cmdline(f.PID)
	if err != nil {
		s.warn(next, path, "cannot read cmdline; launch flags unknown", "pid", f.PID, "err", err)
	}
	return Session{
		PID:             f.PID,
		SessionID:       f.SessionID,
		Cwd:             f.Cwd,
		Name:            f.Name,
		NameSource:      f.NameSource,
		ProcStart:       f.ProcStart,
		Tmux:            f.Tmux,
		TmuxSession:     tmuxSessionName(f.Tmux),
		Status:          f.Status,
		BridgeSessionID: f.BridgeSessionID,
		Version:         f.Version,
		Argv:            argv,
	}, true
}

// ByPID returns the live session for pid, if its session file exists.
func (s *Source) ByPID(pid int) (Session, bool) {
	return s.readLive(filepath.Join(s.ClaudeHome, "sessions", strconv.Itoa(pid)+".json"), nil)
}

// procStart returns field 22 (starttime, clock ticks since boot) of
// /proc/<pid>/stat, which is what Claude records as procStart.
func (s *Source) procStart(pid int) (string, bool) {
	data, err := os.ReadFile(filepath.Join(s.ProcRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return "", false
	}
	return parseStatStart(string(data))
}

// parseStatStart extracts starttime from a /proc/<pid>/stat line. The comm
// field (2) may contain spaces and parentheses, so fields are counted from the
// last ')'. A zombie or dead process (state Z/X, e.g. a just-killed Claude
// not yet reaped) is not alive.
func parseStatStart(stat string) (string, bool) {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return "", false
	}
	fields := strings.Fields(stat[i+1:]) // fields[0] is field 3 (state)
	const idx = 22 - 3
	if len(fields) <= idx {
		return "", false
	}
	if st := fields[0]; st == "Z" || st == "X" || st == "x" {
		return "", false
	}
	return fields[idx], true
}

func (s *Source) cmdline(pid int) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(s.ProcRoot, strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return nil, err
	}
	data = []byte(strings.TrimRight(string(data), "\x00"))
	if len(data) == 0 {
		return nil, fmt.Errorf("empty cmdline")
	}
	return strings.Split(string(data), "\x00"), nil
}

// tmuxSessionName returns the session part of "<session>:@<window>.%<pane>".
// tmux forbids ':' in session names, so the first ':' is the separator.
func tmuxSessionName(tmux string) string {
	name, _, _ := strings.Cut(tmux, ":")
	return name
}

func procStartNum(s string) uint64 {
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return ^uint64(0) // unknown start sorts last
	}
	return n
}

// FindTranscript returns the transcript path of sessionID
// (~/.claude/projects/<project>/<id>.jsonl), or "" when none exists.
func (s *Source) FindTranscript(sessionID string) string {
	if sessionID == "" || strings.ContainsAny(sessionID, `/\*?[`) {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(s.ClaudeHome, "projects", "*", sessionID+".jsonl"))
	if len(matches) == 0 {
		return ""
	}
	return matches[0]
}
