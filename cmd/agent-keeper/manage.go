package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/rerangst/agent-keeper/internal/claudesessions"
	"github.com/rerangst/agent-keeper/internal/registry"
	"github.com/rerangst/agent-keeper/internal/restore"
	"github.com/rerangst/agent-keeper/internal/systemd"
)

func (e *env) store() registry.Store { return registry.Store{Path: e.paths.Registry()} }

func (e *env) disabledConflictsPath() string {
	return filepath.Join(e.paths.StateDir, "disabled-conflicts.json")
}

// cmdStatus prints the registry joined with live state.
func (e *env) cmdStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	reg, err := e.store().Load()
	if err != nil {
		fmt.Fprintln(e.stderr, "agent-keeper: status:", err)
		return 1
	}
	src := &claudesessions.Source{ClaudeHome: e.paths.ClaudeHome, ProcRoot: e.paths.ProcRoot, Log: e.log}
	live, _ := src.ListLive()
	liveByTmux := map[string]claudesessions.Session{}
	for _, s := range live {
		if s.TmuxSession != "" {
			if _, dup := liveByTmux[s.TmuxSession]; !dup {
				liveByTmux[s.TmuxSession] = s
			}
		}
	}

	ctx := context.Background()
	if e.ctl != nil {
		fmt.Fprintf(e.stdout, "snapshot daemon: %s   restore at boot: %s   linger: %s\n",
			e.ctl.Query(ctx, "is-active", systemd.SnapshotUnit),
			e.ctl.Query(ctx, "is-enabled", systemd.RestoreUnit),
			lingerWord(e.ctl.Linger(ctx, currentUser())))
	}
	fmt.Fprintf(e.stdout, "registry: %s (updated %s)\n", e.paths.Registry(), ago(reg.UpdatedAt))
	if rep := e.lastRestore(); rep != nil {
		fmt.Fprintf(e.stdout, "last restore: %s (%d entries)\n", ago(rep.FinishedAt), len(rep.Entries))
	}

	names := make([]string, 0, len(reg.Entries))
	for n := range reg.Entries {
		names = append(names, n)
	}
	sort.Strings(names)
	home, _ := os.UserHomeDir()
	tw := tabwriter.NewWriter(e.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TMUX\tSTATE\tLIVE\tSESSION\tRC\tSTATUS\tLAST RESTORE\tCWD")
	for _, n := range names {
		en := reg.Entries[n]
		liveMark := "-"
		if s, ok := liveByTmux[n]; ok {
			liveMark = "yes"
			if s.SessionID != en.SessionID {
				liveMark = "yes (other id)"
			}
		}
		rc := "off"
		if en.RemoteControl {
			rc = "on"
		}
		last := "-"
		if en.Restore.InProgress {
			last = "in progress"
		} else if en.Restore.LastResult != nil {
			last = *en.Restore.LastResult
			if en.Restore.LastAt != nil {
				last += " " + ago(*en.Restore.LastAt)
			}
		}
		cwd := en.Cwd
		if home != "" && strings.HasPrefix(cwd, home+"/") {
			cwd = "~" + strings.TrimPrefix(cwd, home)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", n, en.State, liveMark, short(en.SessionID), rc,
			orDash(en.LastStatus), last, cwd)
	}
	tw.Flush()
	if len(names) == 0 {
		fmt.Fprintln(e.stdout, "no tracked sessions yet (the snapshot daemon records Claude sessions running in tmux)")
	}
	return 0
}

func (e *env) lastRestore() *restore.Report {
	data, err := os.ReadFile(filepath.Join(e.paths.StateDir, "last-restore.json"))
	if err != nil {
		return nil
	}
	var rep restore.Report
	if json.Unmarshal(data, &rep) != nil {
		return nil
	}
	return &rep
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return orDash(id)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func lingerWord(v string) string {
	switch v {
	case "yes":
		return "on"
	case "no", "":
		return "off (sessions are restored only after you log in; run: sudo loginctl enable-linger " + currentUser() + ")"
	}
	return v
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return d.String() + " ago"
	case d < 48*time.Hour:
		return d.Truncate(time.Minute).String() + " ago"
	}
	return t.Local().Format("2006-01-02 15:04")
}

// currentUser is the account of the real uid ($USER can be stale under su).
func currentUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return os.Getenv("USER")
}

// cmdSetState implements exclude, include and forget.
func (e *env) cmdSetState(cmd string, args []string) int {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(e.stderr, "usage: agent-keeper %s <tmux-session>\n", cmd)
		return 2
	}
	name := fs.Arg(0)
	msg := ""
	err := e.store().Update(func(reg *registry.Registry) error {
		en := reg.Entries[name]
		if en == nil {
			return fmt.Errorf("no tracked session named %q (see `agent-keeper status`)", name)
		}
		switch cmd {
		case "exclude":
			en.State = registry.StateExcluded
			msg = name + ": excluded (never restored; `agent-keeper include " + name + "` undoes)"
		case "include":
			if en.State != registry.StateExcluded {
				return fmt.Errorf("%s is %s, not excluded", name, en.State)
			}
			en.State = registry.StateActive
			msg = name + ": included again"
		case "forget":
			delete(reg.Entries, name)
			msg = name + ": forgotten (a live session is tracked again on the next poll; use exclude to opt out)"
		}
		return nil
	})
	if err != nil {
		fmt.Fprintln(e.stderr, "agent-keeper:", err)
		return 1
	}
	fmt.Fprintln(e.stdout, msg)
	return 0
}

// cmdInstall sets up the user units, linger and conflict handling. It is
// idempotent: re-running it after a binary update restarts the daemon.
func (e *env) cmdInstall(args []string) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(e.stderr, "agent-keeper: install takes no arguments\n")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	home, err := os.UserHomeDir()
	if err != nil {
		return e.fail("install", err)
	}
	for _, bin := range []string{"tmux", "systemd-run", "systemctl"} {
		if _, err := exec.LookPath(bin); err != nil {
			return e.fail("install", fmt.Errorf("%s not found on PATH", bin))
		}
	}
	usr := currentUser()
	if err := e.ctl.Reachable(ctx); err != nil {
		// No user manager yet (e.g. a root shell without a login session):
		// enabling linger starts one.
		if e.ctl.Linger(ctx, usr) != "yes" && e.ctl.EnableLinger(ctx, usr) == nil {
			err = e.waitReachable(ctx)
		}
		if err != nil {
			return e.fail("install", fmt.Errorf("no systemd user instance reachable (%v); log in via a real session or run: sudo loginctl enable-linger %s", err, usr))
		}
	}
	e.checkClaude(ctx)

	exe, err := e.ensureBinary(home)
	if err != nil {
		return e.fail("install", err)
	}
	unitDir := systemd.UnitDir(home)
	if _, err := systemd.WriteUnits(unitDir); err != nil {
		return e.fail("install", err)
	}
	steps := [][]string{
		{"daemon-reload"},
		{"enable", systemd.SnapshotUnit, systemd.RestoreUnit},
		{"restart", systemd.SnapshotUnit},
	}
	for _, s := range steps {
		if _, err := e.ctl.Systemctl(ctx, s...); err != nil {
			return e.fail("install", err)
		}
	}
	fmt.Fprintf(e.stdout, "units installed in %s; snapshot daemon (re)started using %s\n", unitDir, exe)

	incomplete := false
	if err := e.handleConflicts(ctx, unitDir, home); err != nil {
		fmt.Fprintf(e.stderr, "agent-keeper: INSTALL INCOMPLETE: could not disable a conflicting unit: %v\n"+
			"  it may race agent-keeper at boot; disable it by hand (systemctl --user disable --now <unit>)\n", err)
		incomplete = true
	}

	if e.ctl.Linger(ctx, usr) != "yes" {
		if err := e.ctl.EnableLinger(ctx, usr); err == nil && e.ctl.Linger(ctx, usr) == "yes" {
			fmt.Fprintln(e.stdout, "linger enabled: user units now start at boot without a login")
			if err := registry.WriteFileAtomic(e.lingerMarkPath(), []byte(usr+"\n"), 0o600); err != nil {
				e.log.Warn("cannot record that linger was enabled", "err", err)
			}
		} else {
			fmt.Fprintf(e.stderr, "agent-keeper: INSTALL INCOMPLETE: linger is off, so nothing is restored until %s logs in.\n"+
				"  fix: sudo loginctl enable-linger %s\n", usr, usr)
			incomplete = true
		}
	}
	if incomplete {
		return 1
	}
	fmt.Fprintln(e.stdout, "agent-keeper installed. Check with: agent-keeper status")
	return 0
}

// lingerMarkPath records that install (not the user) turned linger on.
func (e *env) lingerMarkPath() string {
	return filepath.Join(e.paths.StateDir, "linger-enabled-by-install")
}

// waitReachable waits briefly for a just-started user manager.
func (e *env) waitReachable(ctx context.Context) error {
	var err error
	for i := 0; i < 20; i++ {
		if err = e.ctl.Reachable(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return err
}

func (e *env) fail(cmd string, err error) int {
	fmt.Fprintf(e.stderr, "agent-keeper: %s: %v\n", cmd, err)
	return 1
}

// checkClaude warns when claude isn't on the interactive-shell PATH that
// restored panes use ($SHELL -ic).
func (e *env) checkClaude(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	shell := restore.UserShell()
	out, err := e.runner.Run(ctx, shell, "-ic", "command -v "+e.cfg.ClaudeBin)
	if err != nil || strings.TrimSpace(out) == "" {
		fmt.Fprintf(e.stderr, "agent-keeper: warning: %q not found via `%s -ic`; restored panes could not start Claude\n", e.cfg.ClaudeBin, shell)
	}
}

// ensureBinary puts this executable at ~/.local/bin/agent-keeper (the path
// the units run) unless it already is that file.
func (e *env) ensureBinary(home string) (string, error) {
	target := filepath.Join(home, ".local", "bin", "agent-keeper")
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if t, err := filepath.EvalSymlinks(target); err == nil && t == exe {
		return target, nil
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		return "", err
	}
	if err := registry.WriteFileAtomic(target, data, 0o755); err != nil {
		return "", fmt.Errorf("copy binary to %s: %w", target, err)
	}
	return target, nil
}

func (e *env) handleConflicts(ctx context.Context, unitDir, home string) error {
	conflicts, err := systemd.FindConflicts(unitDir, home)
	if err != nil || len(conflicts) == 0 {
		return err
	}
	for _, c := range conflicts {
		fmt.Fprintf(e.stdout, "conflicting unit %s (%s); timers: %s\n", c.Service, c.Reason, orDash(strings.Join(c.Timers, ", ")))
	}
	done, err := e.ctl.DisableConflicts(ctx, conflicts)
	if saveErr := systemd.SaveDisabled(e.disabledConflictsPath(), done); saveErr != nil {
		err = errors.Join(err, saveErr)
	}
	for _, d := range done {
		fmt.Fprintf(e.stdout, "  disabled %s (files kept; `agent-keeper uninstall` re-enables it)\n", d.Unit)
	}
	return err
}

// cmdUninstall removes the units and re-enables what install disabled.
// It never touches tmux sessions.
func (e *env) cmdUninstall(args []string) int {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	purge := fs.Bool("purge", false, "also delete the registry, reports and config")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(e.stderr, "agent-keeper: uninstall takes no arguments\n")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	home, err := os.UserHomeDir()
	if err != nil {
		return e.fail("uninstall", err)
	}
	var errs []error
	unitDir := systemd.UnitDir(home)
	for _, u := range systemd.Units {
		if _, err := os.Stat(filepath.Join(unitDir, u)); err != nil {
			continue // already removed: uninstall is idempotent
		}
		if _, err := e.ctl.Systemctl(ctx, "disable", "--now", u); err != nil {
			errs = append(errs, err)
		}
	}
	if err := systemd.RemoveUnits(unitDir); err != nil {
		errs = append(errs, err)
	}
	if _, err := e.ctl.Systemctl(ctx, "daemon-reload"); err != nil {
		errs = append(errs, err)
	}
	keepRecord := true // the record is the only note of what to re-enable
	units, err := systemd.LoadDisabled(e.disabledConflictsPath())
	if err != nil {
		errs = append(errs, fmt.Errorf("%w (kept; re-enable those units by hand)", err))
	} else if err := e.ctl.ReenableUnits(ctx, units); err != nil {
		errs = append(errs, fmt.Errorf("%w (%s kept)", err, e.disabledConflictsPath()))
	} else {
		for _, u := range units {
			fmt.Fprintf(e.stdout, "re-enabled %s\n", u.Unit)
		}
		os.Remove(e.disabledConflictsPath())
		keepRecord = false
	}
	if usr, err := os.ReadFile(e.lingerMarkPath()); err == nil {
		fmt.Fprintf(e.stdout, "linger was enabled by agent-keeper install and is left on (other user units may rely on it);\n"+
			"  turn it off with: loginctl disable-linger %s\n", strings.TrimSpace(string(usr)))
		os.Remove(e.lingerMarkPath())
	}
	if *purge {
		if err := purgeDir(e.paths.StateDir, keepRecord, e.disabledConflictsPath()); err != nil {
			errs = append(errs, err)
		}
		if err := os.RemoveAll(filepath.Dir(e.paths.ConfigFile)); err != nil {
			errs = append(errs, err)
		}
		fmt.Fprintln(e.stdout, "registry, reports and config deleted")
	} else {
		fmt.Fprintf(e.stdout, "registry kept in %s (use --purge to delete)\n", e.paths.StateDir)
	}
	fmt.Fprintf(e.stdout, "units removed; tmux sessions untouched. Remove the binary with: rm %s\n",
		filepath.Join(home, ".local", "bin", "agent-keeper"))
	if err := errors.Join(errs...); err != nil {
		fmt.Fprintln(e.stderr, "agent-keeper: uninstall:", err)
		return 1
	}
	return 0
}

// purgeDir deletes dir, or everything in it but keep when keepIt is set.
func purgeDir(dir string, keepIt bool, keep string) error {
	if !keepIt {
		return os.RemoveAll(dir)
	}
	ents, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var errs []error
	for _, ent := range ents {
		if p := filepath.Join(dir, ent.Name()); p != keep {
			errs = append(errs, os.RemoveAll(p))
		}
	}
	return errors.Join(errs...)
}
