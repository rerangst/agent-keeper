// Command agent-keeper snapshots running Claude Code sessions in tmux and
// restores them after an unexpected shutdown.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"syscall"

	"github.com/rerangst/agent-keeper/internal/claudesessions"
	"github.com/rerangst/agent-keeper/internal/config"
	"github.com/rerangst/agent-keeper/internal/registry"
	"github.com/rerangst/agent-keeper/internal/restore"
	"github.com/rerangst/agent-keeper/internal/snapshot"
	"github.com/rerangst/agent-keeper/internal/systemd"
	"github.com/rerangst/agent-keeper/internal/tmux"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

const usage = `agent-keeper keeps Claude Code agents in tmux alive across reboots.

Usage:
  agent-keeper <command> [flags]

Commands:
  status            tracked sessions, live state, last restore, linger
  restore [--boot] [--dry-run] [name]
                    recreate tracked tmux sessions and resume their Claude
                    conversations (--boot: wait for network and cwd)
  exclude <name>    never restore this tmux session
  include <name>    undo exclude
  forget <name>     drop an entry from the registry
  install           install/refresh systemd user units, enable linger,
                    disable conflicting keepers (run by install.sh)
  uninstall [--purge]
                    remove units, re-enable what install disabled
                    (--purge also deletes registry and config)
  snapshot --once   record live tmux+Claude sessions once and exit
  snapshot, daemon  keep the registry current (run by systemd)
  version           print the version
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// env bundles what every command needs; tests build it on temp dirs.
type env struct {
	paths  config.Paths
	cfg    config.Config
	log    *slog.Logger
	stdout io.Writer
	stderr io.Writer
	runner tmux.Runner  // runs external programs (tmux, systemctl, shells)
	ctl    *systemd.Ctl // systemctl --user / loginctl
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(stdout, usage)
		return 0
	}
	if args[0] == "version" || args[0] == "--version" {
		fmt.Fprintln(stdout, version)
		return 0
	}
	paths, err := config.DefaultPaths()
	if err != nil {
		fmt.Fprintln(stderr, "agent-keeper:", err)
		return 1
	}
	return dispatch(args, paths, stdout, stderr)
}

func dispatch(args []string, paths config.Paths, stdout, stderr io.Writer) int {
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		fmt.Fprintln(stderr, "agent-keeper: config:", err)
		return 1
	}
	runner := tmux.ExecRunner{}
	e := &env{paths: paths, cfg: cfg, log: newLogger(stderr), stdout: stdout, stderr: stderr,
		runner: runner, ctl: &systemd.Ctl{Runner: runner}}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "snapshot":
		return e.cmdSnapshot(rest)
	case "daemon":
		return e.cmdDaemon(rest)
	case "restore":
		return e.cmdRestore(rest)
	case "status":
		return e.cmdStatus(rest)
	case "exclude", "include", "forget":
		return e.cmdSetState(cmd, rest)
	case "install":
		return e.cmdInstall(rest)
	case "uninstall":
		return e.cmdUninstall(rest)
	default:
		fmt.Fprintf(stderr, "agent-keeper: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
}

// newLogger logs to stderr, keeping stdout for command output (both reach
// journald under systemd). journald stamps time itself, so the time attribute
// is dropped when running under it.
func newLogger(w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{}
	if os.Getenv("JOURNAL_STREAM") != "" {
		opts.ReplaceAttr = func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		}
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

func (e *env) snapshotter() *snapshot.Snapshotter {
	return &snapshot.Snapshotter{
		Source: claudesessions.Source{ClaudeHome: e.paths.ClaudeHome, ProcRoot: e.paths.ProcRoot, Log: e.log},
		Store:  registry.Store{Path: e.paths.Registry()},
		Opts:   snapshot.OptionsFromConfig(e.cfg, e.log),
		BootID: snapshot.CurrentBootID(e.paths.ProcRoot),
	}
}

// parseFlags parses fs; ok=false means the command should exit with code
// (0 for -h/--help, 2 for a usage error).
func parseFlags(fs *flag.FlagSet, args []string) (code int, ok bool) {
	err := fs.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		return 0, false
	}
	if err != nil {
		return 2, false
	}
	return 0, true
}

func (e *env) cmdSnapshot(args []string) int {
	fs := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	once := fs.Bool("once", false, "record live sessions once and exit")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if !*once {
		return e.cmdDaemon(fs.Args())
	}
	reg, err := e.snapshotter().Once()
	if err != nil {
		fmt.Fprintln(e.stderr, "agent-keeper: snapshot:", err)
		return 1
	}
	names := make([]string, 0, len(reg.Entries))
	for n := range reg.Entries {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Fprintf(e.stdout, "%d entries in %s\n", len(names), e.paths.Registry())
	for _, n := range names {
		en := reg.Entries[n]
		fmt.Fprintf(e.stdout, "  %-24s %-8s %-6s rc=%-5t %s\n", n, en.State, en.LastStatus, en.RemoteControl, en.Cwd)
	}
	return 0
}

func (e *env) cmdDaemon(args []string) int {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	snapshot.RunDaemon(ctx, e.snapshotter(), e.cfg.PollInterval(), e.log)
	return 0
}

func (e *env) cmdRestore(args []string) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	boot := fs.Bool("boot", false, "running at boot: wait for network and working directories")
	dry := fs.Bool("dry-run", false, "print the commands; launch and change nothing")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(e.stderr, "agent-keeper: restore takes at most one session name")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	en := &restore.Engine{
		Cfg:    e.cfg,
		Paths:  e.paths,
		Store:  registry.Store{Path: e.paths.Registry()},
		Source: &claudesessions.Source{ClaudeHome: e.paths.ClaudeHome, ProcRoot: e.paths.ProcRoot, Log: e.log},
		Tmux:   tmux.Client{Runner: e.runner, Scope: userScope(ctx, e.runner, e.log)},
		Log:    e.log,
		Out:    e.stdout,
	}
	rep, err := en.Run(ctx, restore.Options{Boot: *boot, DryRun: *dry, Only: fs.Arg(0)})
	if err != nil {
		fmt.Fprintln(e.stderr, "agent-keeper: restore:", err)
		return 1
	}
	failed := 0
	for _, r := range rep.Entries {
		if !*dry {
			fmt.Fprintf(e.stdout, "%-24s %-16s attempts=%d %s\n", r.TmuxSession, r.Result, r.Attempts, r.Detail)
		}
		if r.Result == restore.ResultFailed || r.Result == restore.ResultCwdMissing || r.Result == restore.ResultConflict {
			failed++
		}
	}
	if len(rep.Entries) == 0 {
		fmt.Fprintln(e.stdout, "nothing to restore")
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// userScope returns the systemd-run prefix that puts a newly started tmux
// server in its own scope, or nil when no user systemd instance is reachable
// (tmux then starts directly; fine outside systemd units).
func userScope(ctx context.Context, r tmux.Runner, log *slog.Logger) []string {
	scope := []string{"systemd-run", "--user", "--scope", "--quiet"}
	if _, err := r.Run(ctx, scope[0], append(scope[1:], "true")...); err != nil {
		log.Warn("systemd-run --user unavailable; starting tmux without a scope (the restore unit keeps it alive with KillMode=process)", "err", err)
		return nil
	}
	return scope
}
