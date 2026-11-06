package restore

import (
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rerangst/agent-keeper/internal/config"
	"github.com/rerangst/agent-keeper/internal/registry"
)

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shellQuote quotes s for POSIX shells (sh, bash, zsh) with single quotes.
// An embedded single quote closes the quoted run, adds an escaped quote and
// reopens it. Plain words are left bare for readability.
func shellQuote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func shellJoin(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = shellQuote(a)
	}
	return strings.Join(q, " ")
}

// Launch describes how one entry is started.
type Launch struct {
	Argv    []string // claude argv
	Nudged  bool     // a continue prompt is appended
	Fresh   bool     // no transcript: new conversation in cwd
	Pane    []string // argv tmux runs in the pane: shell -ic <claude…; exec shell -i>
	Command string   // Pane as one shell-quoted string (dry-run, reports)
}

// BuildCommand builds the Claude invocation for e:
//
//	claude <flags> --resume <id> -n <name> [<nudge prompt>]
//
// and wraps it as `$SHELL -ic '<claude…>; exec $SHELL -i'` so the user's
// interactive shell setup (PATH, nvm) resolves claude and the pane survives
// when claude exits. tmux runs Pane as argv (no extra shell layer); the
// inner string is POSIX-quoted, so $SHELL must be sh-compatible (sh, bash,
// zsh, dash, ksh). Remote Control is not requested: resume reattaches the
// bridge recorded in the transcript. A fresh start (no transcript) drops
// --resume and the nudge, which would have no conversation to refer to.
func BuildCommand(e *registry.Entry, cfg config.Config, shell string, fresh bool, now time.Time) Launch {
	argv := append([]string{cfg.ClaudeBin}, e.Flags...)
	if !fresh {
		argv = append(argv, "--resume", e.SessionID)
	}
	if e.Name != "" {
		argv = append(argv, "-n", e.Name)
	}
	nudged := !fresh && slices.Contains(cfg.NudgeStatuses, e.LastStatus)
	if nudged {
		argv = append(argv, nudgePrompt(cfg.NudgePrompt, now))
	}
	inner := shellJoin(argv) + "; exec " + shellQuote(shell) + " -i"
	pane := []string{shell, "-ic", inner}
	return Launch{
		Argv:    argv,
		Nudged:  nudged,
		Fresh:   fresh,
		Pane:    pane,
		Command: shellJoin(pane),
	}
}

func nudgePrompt(tmpl string, now time.Time) string {
	return strings.ReplaceAll(tmpl, "{time}", now.Format("2006-01-02 15:04 MST"))
}

// UserShell returns the user's login shell: $SHELL, else the passwd entry
// for the current uid, else /bin/sh.
func UserShell() string {
	if s := os.Getenv("SHELL"); s != "" {
		return s
	}
	if data, err := os.ReadFile("/etc/passwd"); err == nil {
		if s := passwdShell(string(data), os.Getuid()); s != "" {
			return s
		}
	}
	return "/bin/sh"
}

func passwdShell(passwd string, uid int) string {
	want := strconv.Itoa(uid)
	for _, line := range strings.Split(passwd, "\n") {
		f := strings.Split(line, ":")
		if len(f) >= 7 && f[2] == want {
			return f[6]
		}
	}
	return ""
}
