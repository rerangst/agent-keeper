// Package restore recreates tracked tmux sessions after a reboot and resumes
// the exact Claude conversation in each, answering startup dialogs and
// verifying the result.
package restore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/rerangst/agent-keeper/internal/claudesessions"
	"github.com/rerangst/agent-keeper/internal/config"
	"github.com/rerangst/agent-keeper/internal/registry"
	"github.com/rerangst/agent-keeper/internal/tmux"
)

// Results recorded per entry.
const (
	ResultRestored       = "restored"        // resumed; RC state as before
	ResultFreshStart     = "fresh_start"     // transcript missing: new conversation in cwd
	ResultRCNewBridge    = "rc_new_bridge"   // resumed; RC re-enabled with a new bridge
	ResultRCFailed       = "rc_failed"       // resumed locally; RC could not be re-enabled
	ResultRCUnexpected   = "rc_unexpected"   // resumed; RC came up although it was off
	ResultFailed         = "failed"          // every attempt failed (last session left for inspection)
	ResultAborted        = "aborted"         // restore was stopped (e.g. shutdown)
	ResultCwdMissing     = "cwd_missing"     // working directory never appeared
	ResultConflict       = "conflict"        // tmux name taken by something that isn't Claude
	ResultAlreadyRunning = "already_running" // a live Claude already holds the session
	ResultDuplicateID    = "duplicate_id"    // another entry restores the same conversation
	ResultExcluded       = "excluded"        // named explicitly but excluded by the user
	ResultDryRun         = "dry_run"         // would be launched
	ResultOtherSession   = "other_session"   // Claude runs, but resumed a different conversation
)

// Succeeded reports whether result means the agent is running again.
func Succeeded(result string) bool {
	switch result {
	case ResultRestored, ResultFreshStart, ResultRCNewBridge, ResultRCFailed, ResultRCUnexpected, ResultOtherSession:
		return true
	}
	return false
}

// EntryReport is one line of last-restore.json.
type EntryReport struct {
	TmuxSession string  `json:"tmux_session"`
	SessionID   string  `json:"session_id"`
	Result      string  `json:"result"`
	Attempts    int     `json:"attempts"`
	DurationS   float64 `json:"duration_s"`
	Nudged      bool    `json:"nudged"`
	RC          bool    `json:"remote_control"`
	BridgeID    string  `json:"bridge_session_id,omitempty"`
	Detail      string  `json:"detail,omitempty"`
	Command     string  `json:"command,omitempty"` // dry-run only
	Pane        string  `json:"pane,omitempty"`    // last screen on failure
}

// Report is written to last-restore.json after each real run.
type Report struct {
	StartedAt  time.Time     `json:"started_at"`
	FinishedAt time.Time     `json:"finished_at"`
	Boot       bool          `json:"boot"`
	DryRun     bool          `json:"dry_run"`
	Entries    []EntryReport `json:"entries"`
}

// Options select what a run does.
type Options struct {
	Boot   bool   // started by systemd at boot: wait for network and cwd
	DryRun bool   // print commands only; launch and change nothing
	Only   string // restore just this tmux session name
}

// Engine restores sessions. Zero-valued hooks get real implementations.
type Engine struct {
	Cfg    config.Config
	Paths  config.Paths
	Store  registry.Store
	Source *claudesessions.Source
	Tmux   tmux.Client
	Shell  string
	Log    *slog.Logger
	Out    io.Writer // dry-run listing

	Now       func() time.Time
	Sleep     func(ctx context.Context, d time.Duration) error
	Dial      func(ctx context.Context, addr string) error
	PollEvery time.Duration // dialog/verify polling, default 1s
	RCWait    time.Duration // wait for a Remote Control bridge, default 30s

	cwdDeadline time.Time // one deadline for all working directories of a run
}

func (en *Engine) defaults() {
	if en.Now == nil {
		en.Now = time.Now
	}
	if en.Sleep == nil {
		en.Sleep = sleepCtx
	}
	if en.Dial == nil {
		en.Dial = dialTCP
	}
	if en.PollEvery == 0 {
		en.PollEvery = time.Second
	}
	if en.RCWait == 0 {
		en.RCWait = 30 * time.Second
	}
	if en.Log == nil {
		en.Log = slog.Default()
	}
	if en.Out == nil {
		en.Out = io.Discard
	}
	if en.Shell == "" {
		en.Shell = UserShell()
	}
}

// ErrBusy means another restore run holds the lock.
var ErrBusy = errors.New("another agent-keeper restore is running")

// Run restores every candidate entry (or only opts.Only) and returns the report.
func (en *Engine) Run(ctx context.Context, opts Options) (*Report, error) {
	en.defaults()
	rep := &Report{StartedAt: en.Now().UTC(), Boot: opts.Boot, DryRun: opts.DryRun}
	if !opts.DryRun {
		unlock, err := lockFile(filepath.Join(en.Paths.StateDir, "restore.lock"))
		if err != nil {
			return nil, err
		}
		defer unlock()
	}
	reg, err := en.Store.Load()
	if err != nil {
		return nil, err
	}
	live, err := en.Source.ListLive()
	if err != nil && !errors.Is(err, claudesessions.ErrNoSessionsDir) {
		return nil, err
	}
	cands, skipped, err := SelectCandidates(reg, live, opts.Only)
	if err != nil {
		return nil, err
	}
	rep.Entries = append(rep.Entries, skipped...)

	// A tmux session with the entry's name: Claude in it means it is already
	// running; anything else is a conflict we must not touch.
	var todo []*registry.Entry
	for _, e := range cands {
		exists, err := en.Tmux.HasSession(ctx, e.TmuxSession)
		if err != nil {
			rep.Entries = append(rep.Entries, en.skipEntry(e, ResultFailed, "tmux has-session: "+err.Error(), opts.DryRun))
			continue
		}
		if exists {
			rep.Entries = append(rep.Entries, en.skipEntry(e, ResultConflict, "tmux session exists without Claude", opts.DryRun))
			continue
		}
		todo = append(todo, e)
	}

	if opts.DryRun {
		for i := range rep.Entries { // skipped entries: show what would run
			r := &rep.Entries[i]
			if e := reg.Entries[r.TmuxSession]; e != nil && r.Result != ResultExcluded {
				l := en.printDryRun(e, "skip: "+r.Result)
				r.Command, r.Nudged = l.Command, l.Nudged
			}
		}
		for _, e := range todo {
			l := en.printDryRun(e, "would launch")
			r := EntryReport{TmuxSession: e.TmuxSession, SessionID: e.SessionID, Result: ResultDryRun,
				RC: e.RemoteControl, Command: l.Command, Nudged: l.Nudged}
			if l.Fresh {
				r.Detail = "transcript missing: fresh start"
			}
			rep.Entries = append(rep.Entries, r)
		}
		rep.FinishedAt = en.Now().UTC()
		return rep, nil
	}

	// Hold in_progress for every queued entry (also while it waits for the
	// network or its stagger slot) and clear flags left by a run that died:
	// we hold restore.lock, so no other restore can own them.
	en.markInProgress(todo)
	if len(todo) > 0 {
		en.waitNetwork(ctx, opts.Boot)
	}
	en.cwdDeadline = en.Now().Add(time.Duration(en.Cfg.CwdTimeoutS) * time.Second)
	for i, e := range todo {
		if i > 0 {
			if err := en.Sleep(ctx, time.Duration(en.Cfg.StaggerS)*time.Second); err != nil {
				rep.Entries = append(rep.Entries, en.skipEntry(e, ResultAborted, "stopped before launch", false))
				continue
			}
		}
		rep.Entries = append(rep.Entries, en.restoreOne(ctx, e, opts.Boot))
	}
	rep.FinishedAt = en.Now().UTC()
	sort.SliceStable(rep.Entries, func(i, j int) bool { return rep.Entries[i].TmuxSession < rep.Entries[j].TmuxSession })
	if err := en.writeReport(rep); err != nil {
		en.Log.Error("cannot write last-restore.json", "err", err)
	}
	return rep, nil
}

// printDryRun writes the launch an entry would get and returns it.
func (en *Engine) printDryRun(e *registry.Entry, note string) Launch {
	fresh := en.Source.FindTranscript(e.SessionID) == ""
	l := BuildCommand(e, en.Cfg, en.Shell, fresh, en.Now())
	extra := ""
	if fresh {
		extra = ", fresh start (no transcript)"
	}
	if l.Nudged {
		extra += ", nudged"
	}
	fmt.Fprintf(en.Out, "%s  [%s%s]\n  cd %s\n  %s\n", e.TmuxSession, note, extra, shellQuote(e.Cwd), l.Command)
	return l
}

// SelectCandidates picks the entries to restore and reports the ones it
// skips. Pure: depends only on the registry and the live sessions.
func SelectCandidates(reg *registry.Registry, live []claudesessions.Session, only string) ([]*registry.Entry, []EntryReport, error) {
	liveIDs := map[string]bool{}
	liveTmux := map[string]bool{}
	for _, s := range live {
		liveIDs[s.SessionID] = true
		if s.TmuxSession != "" {
			liveTmux[s.TmuxSession] = true
		}
	}
	names := make([]string, 0, len(reg.Entries))
	for n, e := range reg.Entries {
		if e != nil {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	if only != "" {
		e := reg.Entries[only]
		if e == nil {
			return nil, nil, fmt.Errorf("no registry entry named %q", only)
		}
		names = []string{only}
	}

	var skipped []EntryReport
	skip := func(e *registry.Entry, result, detail string) {
		skipped = append(skipped, EntryReport{TmuxSession: e.TmuxSession, SessionID: e.SessionID, Result: result,
			RC: e.RemoteControl, Detail: detail})
	}
	// Newest sighting wins when two entries hold one conversation.
	owner := map[string]*registry.Entry{}
	var picked []*registry.Entry
	for _, n := range names {
		e := reg.Entries[n]
		switch {
		case e.State == registry.StateExcluded:
			if only != "" {
				skip(e, ResultExcluded, "run `agent-keeper include` first")
			}
			continue
		case e.State != registry.StateActive && only == "":
			continue // stopped: closed by the user (a named restore overrides)
		case liveIDs[e.SessionID] || liveTmux[e.TmuxSession]:
			skip(e, ResultAlreadyRunning, "")
			continue
		}
		if prev := owner[e.SessionID]; prev != nil && e.SessionID != "" {
			loser := e
			if e.LastSeen.After(prev.LastSeen) {
				loser = prev
				owner[e.SessionID] = e
				picked = slicesDelete(picked, prev)
				picked = append(picked, e)
			}
			skip(loser, ResultDuplicateID, "same session id as "+owner[e.SessionID].TmuxSession)
			continue
		}
		owner[e.SessionID] = e
		picked = append(picked, e)
	}
	return picked, skipped, nil
}

func slicesDelete(s []*registry.Entry, x *registry.Entry) []*registry.Entry {
	out := s[:0]
	for _, v := range s {
		if v != x {
			out = append(out, v)
		}
	}
	return out
}

// restoreOne runs all attempts for one entry. restore.in_progress (set by
// markInProgress) stays on through backoff and is cleared by record on every
// exit path.
func (en *Engine) restoreOne(ctx context.Context, e *registry.Entry, boot bool) EntryReport {
	start := en.Now()
	rep := EntryReport{TmuxSession: e.TmuxSession, SessionID: e.SessionID, RC: e.RemoteControl}
	defer func() {
		rep.DurationS = en.Now().Sub(start).Seconds()
		en.record(e.TmuxSession, rep)
		en.Log.Info("restore result", "tmux", rep.TmuxSession, "result", rep.Result, "attempts", rep.Attempts,
			"nudged", rep.Nudged, "rc", rep.RC, "detail", rep.Detail)
	}()

	if !en.waitCwd(ctx, e.Cwd, boot) {
		rep.Result, rep.Detail = ResultCwdMissing, e.Cwd
		if ctx.Err() != nil {
			rep.Result = ResultAborted
		}
		return rep
	}

	transcript := en.Source.FindTranscript(e.SessionID)
	fresh := transcript == ""
	backup := ""
	if !fresh {
		b, err := claudesessions.RepairTranscript(transcript, filepath.Join(en.Paths.StateDir, "backups"), en.Now())
		if err != nil {
			en.Log.Warn("transcript repair skipped", "tmux", e.TmuxSession, "err", err)
		} else if b != "" {
			backup = b
			en.Log.Warn("repaired truncated transcript", "tmux", e.TmuxSession, "backup", b)
		}
	}

	attempts := max(en.Cfg.MaxAttempts, 1)
	for attempt := 1; attempt <= attempts; attempt++ {
		rep.Attempts = attempt
		l := BuildCommand(e, en.Cfg, en.Shell, fresh, en.Now())
		rep.Nudged = l.Nudged
		out := en.launchAndVerify(ctx, e, l)
		rep.Result, rep.Detail, rep.Pane, rep.BridgeID = out.result, out.detail, out.pane, out.bridge
		if Succeeded(out.result) {
			if fresh { // the conversation is new whatever RC did; keep RC detail
				if out.result != ResultRestored {
					rep.Detail = strings.TrimSpace(out.result + " " + out.detail)
				}
				rep.Result = ResultFreshStart
			}
			rep.Pane = ""
			return rep
		}
		if ctx.Err() != nil {
			rep.Result = ResultAborted
			return rep
		}
		if out.noRetry {
			return rep // retrying cannot help (conflict, wrong cwd, claude missing)
		}
		if out.resumeFailed && backup != "" {
			// The repair may be what broke resume: put the original back and
			// start fresh so the agent at least runs in its directory.
			if err := claudesessions.RestoreBackup(backup, transcript); err != nil {
				en.Log.Error("cannot restore transcript backup", "tmux", e.TmuxSession, "err", err)
			}
			backup, fresh = "", true
		}
		if attempt == attempts {
			break // leave the last session alive for inspection
		}
		// Kill only the session this attempt created, by its tmux id: the
		// name may belong to someone else by now.
		if out.tmuxID != "" {
			if err := en.Tmux.KillSessionID(context.WithoutCancel(ctx), out.tmuxID); err != nil {
				en.Log.Warn("cannot kill failed attempt", "tmux", e.TmuxSession, "err", err)
			}
		}
		if err := en.Sleep(ctx, en.backoff(attempt)); err != nil {
			rep.Result = ResultAborted
			return rep
		}
	}
	rep.Result = ResultFailed
	return rep
}

func (en *Engine) backoff(attempt int) time.Duration {
	b := en.Cfg.BackoffS
	if len(b) == 0 {
		return 0
	}
	return time.Duration(b[min(attempt-1, len(b)-1)]) * time.Second
}

// skipEntry builds the report line for an entry that is not launched. Real
// runs record it in the registry so `status` shows why.
func (en *Engine) skipEntry(e *registry.Entry, result, detail string, dryRun bool) EntryReport {
	r := EntryReport{TmuxSession: e.TmuxSession, SessionID: e.SessionID, Result: result, RC: e.RemoteControl, Detail: detail}
	if !dryRun {
		en.Log.Info("restore result", "tmux", r.TmuxSession, "result", r.Result, "detail", r.Detail)
		en.record(e.TmuxSession, r)
	}
	return r
}

func (en *Engine) markInProgress(todo []*registry.Entry) {
	queued := map[string]bool{}
	for _, e := range todo {
		queued[e.TmuxSession] = true
	}
	err := en.Store.Update(func(reg *registry.Registry) error {
		for name, e := range reg.Entries {
			if e != nil {
				e.Restore.InProgress = queued[name]
			}
		}
		return nil
	})
	if err != nil {
		en.Log.Error("cannot update registry", "err", err)
	}
}

// record stores the outcome and clears in_progress.
func (en *Engine) record(name string, r EntryReport) {
	at := en.Now().UTC()
	result := r.Result
	err := en.Store.Update(func(reg *registry.Registry) error {
		if e := reg.Entries[name]; e != nil {
			e.Restore = registry.Restore{Attempts: r.Attempts, LastResult: &result, LastAt: &at, InProgress: false}
		}
		return nil
	})
	if err != nil {
		en.Log.Error("cannot record restore result", "tmux", name, "err", err)
	}
}

func (en *Engine) writeReport(rep *Report) error {
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return registry.WriteFileAtomic(filepath.Join(en.Paths.StateDir, "last-restore.json"), append(data, '\n'), 0o600)
}

// waitNetwork waits until the network check address accepts TCP, so Claude
// can reach its API and Remote Control at boot. On timeout it continues: an
// agent without network still restores and retries on its own.
func (en *Engine) waitNetwork(ctx context.Context, boot bool) {
	addr := en.Cfg.NetworkCheck
	if addr == "" {
		return
	}
	deadline := en.Now().Add(time.Duration(en.Cfg.NetworkTimeoutS) * time.Second)
	for {
		if err := en.Dial(ctx, addr); err == nil {
			return
		} else if !boot || !en.Now().Before(deadline) {
			en.Log.Warn("network check failed; restoring anyway", "addr", addr, "err", err)
			return
		}
		if en.Sleep(ctx, 2*time.Second) != nil {
			return
		}
	}
}

// waitCwd waits for the working directory (it may be on a late mount at
// boot), up to one deadline shared by the whole run so several missing
// directories don't add up.
func (en *Engine) waitCwd(ctx context.Context, cwd string, boot bool) bool {
	deadline := en.cwdDeadline
	if deadline.IsZero() { // called outside Run
		deadline = en.Now().Add(time.Duration(en.Cfg.CwdTimeoutS) * time.Second)
	}
	for {
		if info, err := os.Stat(cwd); err == nil && info.IsDir() {
			return true
		}
		if !boot || !en.Now().Before(deadline) {
			return false
		}
		if en.Sleep(ctx, 2*time.Second) != nil {
			return false
		}
	}
}

func dialTCP(ctx context.Context, addr string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return c.Close()
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// lockFile takes a non-blocking exclusive flock; a second restore fails fast.
func lockFile(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, err
	}
	return func() { f.Close() }, nil
}
