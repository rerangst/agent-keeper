# agent-keeper

Keeps long-lived [Claude Code](https://code.claude.com) agents running in tmux
alive across unexpected shutdowns. When a power cut or crash kills the machine,
agent-keeper brings every agent back at boot, without anyone logging in:
same tmux session name, same working directory, **same Claude conversation**,
Remote Control state preserved. Agents that were in the middle of a task get a
short prompt asking them to check what was interrupted and carry on.

- One static binary (Linux amd64/arm64), no runtime dependencies.
- Runs as two systemd **user** units; lingering makes them start at boot.
- Discovers sessions on its own: every Claude Code session running inside tmux
  is tracked. Opt out per session with `agent-keeper exclude`.

## How it works

1. **Snapshot** (while the machine runs): a daemon polls Claude Code's own
   per-process session files every 10 s and keeps a durable registry of
   `tmux session → Claude session id, cwd, name, launch flags, Remote Control,
   last status`. `/clear` (new session id) is picked up on the next poll.
2. **Restore** (at boot): a oneshot unit waits for the network and each
   working directory, recreates the tmux session and runs
   `claude <flags> --resume <id> -n <name>` in it, answers the known startup
   dialogs, verifies the conversation came back, and retries with backoff.

A session you close yourself (`/exit`, `tmux kill-session`) is marked
`stopped` once it has been gone for 3 minutes and is not restored. See
[docs/architecture.md](docs/architecture.md) for the details.

## Requirements

- Linux with a systemd user instance (`systemctl --user` works), `tmux`, and
  `systemd-run`.
- Claude Code installed and logged in, reachable as `claude` from your
  interactive shell (`$SHELL -ic 'command -v claude'`). Supported: Claude Code
  2.1.x, tested with 2.1.319. agent-keeper reads Claude Code's internal
  session files, so a later release may need an agent-keeper update (see
  Limitations).
- An sh-compatible login shell (sh, bash, zsh, dash, ksh).
- Lingering for your user (the installer enables it when allowed).

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/rerangst/agent-keeper/main/install.sh | sh
```

The installer downloads the release binary for your architecture, verifies it
against `checksums.txt`, installs it to `~/.local/bin/agent-keeper`, then runs
`agent-keeper install`, which:

- writes `agent-keeper-snapshot.service` and `agent-keeper-restore.service` to
  `~/.config/systemd/user/`, enables both and (re)starts the snapshot daemon;
- enables lingering (`loginctl enable-linger $USER`). If that is not permitted
  it prints the `sudo` command to run and exits non-zero;
- disables other user units that launch `claude` inside `tmux` (they would
  race agent-keeper at boot). Their timers are stopped and disabled, their
  services only disabled, never stopped. Unit files are kept, and
  `agent-keeper uninstall` re-enables them.

Pin a version, update, or install a local build:

```sh
curl -fsSL https://raw.githubusercontent.com/rerangst/agent-keeper/main/install.sh | AGENT_KEEPER_VERSION=v0.1.0 sh
curl -fsSL https://raw.githubusercontent.com/rerangst/agent-keeper/main/install.sh | sh   # re-run = update
sh install.sh --local ./dist/agent-keeper                                                # from a checkout
```

Uninstall (tmux sessions are never touched; lingering is left on because
other user units may rely on it, and uninstall prints how to turn it off if
install enabled it):

```sh
agent-keeper uninstall            # remove units, re-enable what install disabled; keep the registry
agent-keeper uninstall --purge    # also delete registry, reports and config
rm ~/.local/bin/agent-keeper
```

## Commands

| Command | What it does |
|---|---|
| `agent-keeper status` | Tracked sessions with live state, Remote Control, last status and last restore result; daemon, restore unit and linger state. |
| `agent-keeper restore [--dry-run] [name]` | Restore now (all active entries, or one by tmux name, including a `stopped` one). `--dry-run` prints the commands and changes nothing. The boot unit runs `restore --boot`, which also waits for network and working directories. |
| `agent-keeper exclude <name>` / `include <name>` | Never restore a session / undo that. |
| `agent-keeper forget <name>` | Drop an entry (a live session is tracked again on the next poll; use `exclude` to opt out). |
| `agent-keeper install` / `uninstall [--purge]` | Set up / remove the systemd units (see above). |
| `agent-keeper snapshot --once` | Record live sessions once (the daemon does this every poll). |
| `agent-keeper daemon` | The snapshot daemon (run by systemd); `snapshot` without `--once` is the same. |
| `agent-keeper version` | Print the version. |

## Configuration

Optional `~/.config/agent-keeper/config.json` (respects `XDG_CONFIG_HOME`);
every key falls back to its default.

| Key | Default | Meaning |
|---|---|---|
| `poll_interval_s` | `10` | Snapshot poll interval. |
| `removal_debounce_s` | `180` | How long a session must be gone before it counts as closed by you. |
| `stagger_s` | `8` | Pause between restoring two sessions. |
| `nudge_statuses` | `["busy","shell"]` | Last statuses that get the continue prompt after restore. |
| `nudge_prompt` | see below | The continue prompt; `{time}` is replaced with the restore time. |
| `max_attempts` | `3` | Launch attempts per session. |
| `backoff_s` | `[30,120,600]` | Wait before attempt 2, 3, …; the last value repeats. |
| `network_check` | `"api.anthropic.com:443"` | TCP address that must answer before restoring (`""` skips). |
| `network_timeout_s` | `300` | Give up waiting for the network (restore continues anyway). |
| `cwd_timeout_s` | `300` | Wait for working directories (late mounts), shared by the whole run. |
| `verify_timeout_s` | `90` | Time for Claude to come up in a restored pane. |
| `claude_bin` | `"claude"` | Command used to start Claude. |
| `flag_allowlist` | see below | Extra launch flags to carry over, as `{"--flag": "bool" \| "value" \| "variadic"}`. Entries extend the defaults; `""` removes a default. |

Launch flags carried over by default: `--dangerously-skip-permissions`,
`--permission-mode`, `--model`, `--effort`, `--add-dir`, `--agent`,
`--mcp-config`, `--settings`, `--plugin-dir`. Session-selecting options
(`--continue`, `--resume`, `--session-id`, `--fork-session`, `--name`,
`--remote-control`) and prompts are never carried over.

Default nudge prompt, sent only to sessions whose last status was `busy` or
`shell`:

> [agent-keeper] The machine shut down unexpectedly while you were working;
> this session was auto-restored at {time}. Before continuing: (1) determine
> whether your last command/tool call actually completed, (2) check git status
> and any half-edited files, (3) check the processes, dev servers, and services
> you had running and restart them if needed. Then continue the interrupted
> work.

## Files

| Path | Content |
|---|---|
| `~/.local/state/agent-keeper/registry.json` | Tracked sessions (respects `XDG_STATE_HOME`). |
| `~/.local/state/agent-keeper/last-restore.json` | Per-session result of the last restore run. |
| `~/.local/state/agent-keeper/backups/` | Transcript copies made before repairing a power-loss-truncated transcript. |
| `~/.local/state/agent-keeper/disabled-conflicts.json` | Units `install` disabled, re-enabled by `uninstall`. |
| `~/.local/state/agent-keeper/linger-enabled-by-install` | Present when `install` turned lingering on. |
| `~/.local/state/agent-keeper/*.lock` | `flock` files for the registry and for restore runs. |
| `~/.config/systemd/user/agent-keeper-*.service` | The two units. |

Logs go to the journal:

```sh
journalctl --user -u agent-keeper-restore -b      # what the last boot restored
journalctl --user -u agent-keeper-snapshot -f     # tracking changes
```

## Restore results

`status`, `last-restore.json` and the journal use these results:
`restored`, `fresh_start` (transcript missing: new conversation in the same
directory), `rc_new_bridge` (Remote Control re-enabled with a new entry in the
app), `rc_failed` (agent runs locally, Remote Control could not be enabled),
`rc_unexpected`, `other_session` (Claude came up on a different conversation),
`failed` (all attempts failed, the last session is left for inspection),
`cwd_missing`, `conflict` (the tmux name is taken by something else, left
untouched), `already_running`, `duplicate_id`, `excluded` (named in
`restore <name>` but excluded), `aborted`, and `dry_run` in `--dry-run`
reports.

## Limitations

- One Claude agent per tmux session; extra windows and panes are not
  recreated, and tmux sessions without Claude are not managed.
- Restore happens at boot only. An agent that exits while the machine stays up
  is not restarted.
- A session you `/exit` less than 3 minutes before a crash is restored anyway
  (it cannot be told apart from shutdown); `exclude` it if needed.
- If the original Remote Control bridge does not reattach, the fallback
  creates a new entry in the Claude app with the session name as title.
- agent-keeper reads Claude Code's internal session files. A Claude Code
  release that changes them can break discovery until agent-keeper is
  updated; `status` and the journal show it (sessions missing, format
  warnings).
- Linux only.

## Development

```sh
go test ./...                       # unit tests
go vet ./... && gofmt -l .
CGO_ENABLED=0 go build -o dist/agent-keeper ./cmd/agent-keeper
AGENT_KEEPER_IT=1 go test -tags integration -run TestCrashRecovery -v -timeout 20m ./internal/restore/
```

The integration test starts three throwaway Claude sessions in temp
directories (a few tiny prompts), kills them like a power cut, restores them
and checks ids, Remote Control and the nudge. It needs a logged-in Claude Code
and changes that Claude config the way real use does: it runs the agents with
`--dangerously-skip-permissions` (accepting that warning once) and trusts the
temp directories in `~/.claude.json`. One agent has Remote Control on, so it
briefly shows up in the Claude app.

## License

MIT
