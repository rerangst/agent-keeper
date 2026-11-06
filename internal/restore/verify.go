package restore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rerangst/agent-keeper/internal/claudesessions"
	"github.com/rerangst/agent-keeper/internal/registry"
	"github.com/rerangst/agent-keeper/internal/tmux"
)

// resumeFailedText is what Claude prints when --resume cannot load the
// conversation; it then exits back to the shell. The text alone is not
// enough (a resumed history may quote it), so the pane must also be back at
// the shell with no live Claude.
const resumeFailedText = "No conversation found with session ID"

// maxDialogAnswers caps how often one dialog is answered, so a dialog whose
// layout changed can't be answered wrongly forever.
const maxDialogAnswers = 3

// settlePolls is how many consecutive dialog-free polls are required after
// Claude's session file appears: Claude may register before a later startup
// dialog (e.g. bypass-permissions after trust-folder) is drawn.
const settlePolls = 3

// shellGrace is how long the pane may sit at the shell without a live Claude
// before the attempt fails fast (zshrc/nvm start-up takes a few seconds).
const shellGrace = 15 * time.Second

type outcome struct {
	result       string
	detail       string
	pane         string
	bridge       string
	tmuxID       string // session this attempt created ("" if none)
	resumeFailed bool
	noRetry      bool // retrying cannot help
	stopAgent    bool // the agent must not keep running (kill our session)
}

// staleRepeat is how many polls an unchanged screen must persist after an
// answer before the same dialog is answered again (a capture taken before
// the UI redrew must not get a second keypress).
const staleRepeat = 3

// screenWatch answers known dialogs on one pane and keeps the last capture.
type screenWatch struct {
	en           *Engine
	name         string
	answered     map[string]int
	lastAnswered string
	stale        int
	screen       string
}

// poll captures the pane and answers a visible dialog; it reports whether
// one was answered.
func (w *screenWatch) poll(ctx context.Context) bool {
	if s, err := w.en.Tmux.CapturePane(ctx, w.name); err == nil {
		w.screen = s
	}
	d, keys, ok := MatchDialog(w.screen)
	if !ok || w.answered[d.Name] >= maxDialogAnswers {
		return false
	}
	if w.screen == w.lastAnswered {
		if w.stale++; w.stale < staleRepeat {
			return false
		}
	}
	w.answered[d.Name]++
	w.lastAnswered, w.stale = w.screen, 0
	w.en.Log.Info("answering dialog", "tmux", w.name, "dialog", d.Name)
	if err := w.en.Tmux.SendKeys(ctx, w.name, keys...); err != nil {
		w.en.Log.Warn("send-keys failed", "tmux", w.name, "err", err)
	}
	return true
}

// launchAndVerify starts the tmux session and waits until Claude has written
// its session file and no startup dialog is pending (answering known dialogs
// on the way), checks it runs in the entry's cwd on the expected
// conversation, then checks Remote Control against the recorded state.
func (en *Engine) launchAndVerify(ctx context.Context, e *registry.Entry, l Launch) outcome {
	// Re-check right before launching: the name may have been taken since
	// candidates were selected (a login, a tmux plugin); never touch it.
	if exists, err := en.Tmux.HasSession(ctx, e.TmuxSession); err != nil || exists {
		return outcome{result: ResultConflict, detail: "tmux session appeared before launch", noRetry: true}
	}
	id, err := en.Tmux.NewSession(ctx, e.TmuxSession, e.Cwd, l.Pane)
	if errors.Is(err, tmux.ErrDuplicate) {
		return outcome{result: ResultConflict, detail: "tmux session appeared before launch", noRetry: true}
	}
	if err != nil {
		return outcome{result: ResultFailed, detail: err.Error()}
	}
	o := en.verify(ctx, e, l)
	o.tmuxID = id
	if o.stopAgent {
		// An agent with bypass permissions and a "continue" prompt must not
		// keep running in the wrong directory: stop the session we created.
		if err := en.Tmux.KillSessionID(context.WithoutCancel(ctx), id); err != nil {
			en.Log.Error("cannot stop agent started in the wrong directory", "tmux", e.TmuxSession, "err", err)
		}
		o.tmuxID = ""
	}
	return o
}

func (en *Engine) verify(ctx context.Context, e *registry.Entry, l Launch) outcome {
	w := &screenWatch{en: en, name: e.TmuxSession, answered: map[string]int{}}
	start := en.Now()
	deadline := start.Add(time.Duration(en.Cfg.VerifyTimeoutS) * time.Second)
	quiet := 0
	var atShell time.Time // since when the pane has been at the shell without Claude
	for {
		if w.poll(ctx) {
			quiet = 0
		} else if s, ok := en.findClaude(e.TmuxSession); ok {
			atShell = time.Time{}
			if quiet++; quiet >= settlePolls {
				return en.accept(ctx, e, l, s)
			}
		} else if en.paneAtShell(ctx, e.TmuxSession) {
			if atShell.IsZero() {
				atShell = en.Now()
			}
			switch {
			case strings.Contains(w.screen, resumeFailedText):
				return outcome{result: ResultFailed, detail: "claude could not resume the conversation", pane: tail(w.screen), resumeFailed: true}
			case claudeNotFound(w.screen, en.Cfg.ClaudeBin):
				return outcome{result: ResultFailed, detail: en.Cfg.ClaudeBin + " not found on the interactive shell PATH", pane: tail(w.screen), noRetry: true}
			case en.Now().Sub(atShell) >= shellGrace:
				return outcome{result: ResultFailed, detail: "claude exited or did not start", pane: tail(w.screen)}
			}
		}
		if !en.Now().Before(deadline) {
			return outcome{result: ResultFailed, detail: "claude did not start within verify_timeout_s", pane: tail(w.screen)}
		}
		if err := en.Sleep(ctx, en.PollEvery); err != nil {
			return outcome{result: ResultAborted, detail: "stopped", pane: tail(w.screen)}
		}
	}
}

// accept checks a live, settled Claude: right directory, expected
// conversation, then Remote Control. No dialogs are answered from here on:
// keys typed now would land in the live conversation.
func (en *Engine) accept(ctx context.Context, e *registry.Entry, l Launch, s claudesessions.Session) outcome {
	if !samePath(s.Cwd, e.Cwd) {
		return outcome{result: ResultFailed, detail: fmt.Sprintf("wrong cwd: claude runs in %s, expected %s", s.Cwd, e.Cwd),
			noRetry: true, stopAgent: true}
	}
	o := en.verifyRC(ctx, e, s)
	if !l.Fresh && s.SessionID != e.SessionID {
		// Claude runs, but on another conversation: report it and keep it
		// running (retrying would not change the outcome).
		o.detail = strings.TrimSpace(fmt.Sprintf("resumed %s instead of %s; %s %s", s.SessionID, e.SessionID, o.result, o.detail))
		o.result = ResultOtherSession
	}
	return o
}

// claudeNotFound reports a shell's "command not found" for the claude binary
// itself (zsh: "command not found: claude", bash: "claude: command not
// found", dash: "claude: not found"), not for other commands in shell rc files.
func claudeNotFound(screen, bin string) bool {
	name := filepath.Base(bin)
	for _, line := range strings.Split(screen, "\n") {
		if !strings.Contains(line, "not found") {
			continue
		}
		if strings.Contains(line, "not found: "+name) || strings.Contains(line, name+": command not found") ||
			strings.Contains(line, name+": not found") {
			return true
		}
	}
	return false
}

func samePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA == nil && errB == nil && ra == rb {
		return true
	}
	ia, errA := os.Stat(a)
	ib, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(ia, ib)
}

// paneAtShell reports whether the pane's foreground program is a shell,
// i.e. Claude is not running in it.
func (en *Engine) paneAtShell(ctx context.Context, name string) bool {
	cmd, err := en.Tmux.PaneCommand(ctx, name)
	if err != nil {
		return false
	}
	switch cmd {
	case filepath.Base(en.Shell), "sh", "bash", "zsh", "dash", "fish", "ksh":
		return true
	}
	return false
}

// findClaude returns the live interactive Claude whose session file names
// this tmux session.
func (en *Engine) findClaude(name string) (claudesessions.Session, bool) {
	live, err := en.Source.ListLive()
	if err != nil {
		return claudesessions.Session{}, false
	}
	for _, s := range live {
		if s.TmuxSession == name {
			return s, true
		}
	}
	return claudesessions.Session{}, false
}

// verifyRC checks Remote Control against the recorded state. RC on: the
// transcript's bridge record should reattach on its own; if no bridge shows
// up, `/remote-control <name>` is sent once (this creates a new bridge, so
// the phone shows a new entry; while the nudge turn runs it is queued and
// may finish after the wait). RC off: a bridge appearing is reported only.
func (en *Engine) verifyRC(ctx context.Context, e *registry.Entry, s claudesessions.Session) outcome {
	if !e.RemoteControl {
		if bridge := en.waitBridge(ctx, s.PID, min(en.RCWait, 5*time.Second)); bridge != "" {
			return outcome{result: ResultRCUnexpected, bridge: bridge, detail: "remote control attached although it was off"}
		}
		return outcome{result: ResultRestored}
	}
	if bridge := en.waitBridge(ctx, s.PID, en.RCWait); bridge != "" {
		if e.BridgeSessionID != "" && bridge != e.BridgeSessionID {
			return outcome{result: ResultRCNewBridge, bridge: bridge, detail: "reattached to a different bridge"}
		}
		return outcome{result: ResultRestored, bridge: bridge}
	}
	cmd := "/remote-control"
	if e.Name != "" {
		cmd += " " + e.Name
	}
	en.Log.Info("remote control did not reattach; enabling it", "tmux", e.TmuxSession)
	if err := en.Tmux.SendText(ctx, e.TmuxSession, cmd); err == nil {
		_ = en.Tmux.SendKeys(ctx, e.TmuxSession, "Enter")
	}
	if bridge := en.waitBridge(ctx, s.PID, en.RCWait); bridge != "" {
		return outcome{result: ResultRCNewBridge, bridge: bridge, detail: "enabled with /remote-control"}
	}
	return outcome{result: ResultRCFailed, detail: "agent runs locally; remote control not attached yet (a queued /remote-control may still apply)"}
}

// waitBridge polls the Claude process's session file for a bridge id.
func (en *Engine) waitBridge(ctx context.Context, pid int, wait time.Duration) string {
	deadline := en.Now().Add(wait)
	for {
		if s, ok := en.Source.ByPID(pid); ok && s.BridgeSessionID != "" {
			return s.BridgeSessionID
		}
		if !en.Now().Before(deadline) || en.Sleep(ctx, en.PollEvery) != nil {
			return ""
		}
	}
}

// tail keeps the last lines of a screen capture for the report.
func tail(screen string) string {
	const keep = 30
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	if len(lines) > keep {
		lines = lines[len(lines)-keep:]
	}
	return strings.Join(lines, "\n")
}
