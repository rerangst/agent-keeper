# agent-keeper architecture

## Data flow

```
 while the machine runs                              at boot (lingering, no login)

 ~/.claude/sessions/<pid>.json ──┐
 /proc/<pid>/{stat,cmdline} ─────┤ poll every 10 s
 /proc/sys/kernel/random/boot_id ┘
                                 ▼
 agent-keeper-snapshot.service ──► registry.json ──► agent-keeper-restore.service (oneshot)
 (agent-keeper daemon)             (state dir)        agent-keeper restore --boot
                                                      wait network + cwd → per entry, staggered:
                                                      systemd-run --user --scope tmux new-session …
                                                        $SHELL -ic 'claude <flags> --resume <id> -n <name> [nudge]; exec $SHELL -i'
                                                      answer dialogs → verify → Remote Control check
                                                      retry with backoff → last-restore.json + journal
```

## Packages

| Package | Role |
|---|---|
| `cmd/agent-keeper` | CLI (stdlib `flag`), wiring. |
| `internal/config` | Defaults, `config.json`, XDG / `CLAUDE_CONFIG_DIR` paths. |
| `internal/claudesessions` | **The only package that knows Claude Code internals**: session files, process cmdline, transcript location and repair. |
| `internal/registry` | Registry schema, atomic save (temp file, fsync, rename, directory fsync) under an `flock` on a sibling lock file. |
| `internal/snapshot` | Pure `Merge` of live sessions into the registry, and the daemon loop. |
| `internal/tmux` | Launcher backend: typed tmux helpers behind a `Runner` interface (tests use a fake). A different backend (e.g. native Claude background sessions) can replace it. |
| `internal/restore` | Candidate selection, launch command, dialog table, verification, retries, report. |
| `internal/systemd` | Unit templates (embedded), `systemctl --user` / `loginctl`, conflicting-unit handling. |

## Claude Code internals used

Claude Code writes `~/.claude/sessions/<pid>.json` for every running process:

| Key | Use |
|---|---|
| `pid`, `procStart` | Liveness. `procStart` is field 22 of `/proc/<pid>/stat`; a mismatch means the PID was reused (e.g. after reboot). Files without `procStart` are rejected. |
| `sessionId` | The conversation to resume. `/clear` rewrites it. |
| `cwd`, `name`, `nameSource` | Restore directory and `-n` name. |
| `tmux` | `"<session>:@<window>.%<pane>"`; the part before `:` keys the registry. |
| `status` | `idle`, `busy` or `shell`; decides the nudge. |
| `bridgeSessionId` | Present while Remote Control is on. |
| `kind` | Only `interactive` (or missing) is tracked; background/daemon processes inherit `TMUX`. |
| `version` | Recorded as `claude_version` (helps spot format changes). |

Facts the design relies on (observed with Claude Code 2.1.319):

- A clean exit deletes the session file; SIGKILL leaves it, and the next Claude
  start sweeps stale files. So agent-keeper keeps its own registry.
- `claude --resume <id>` without `--remote-control` reattaches the bridge
  recorded in the transcript (`{"type":"bridge-session",…}`), keeping the same
  Remote Control entry.
- `-n <name>` sets the name; a positional prompt after `--resume` is submitted
  at once (the nudge).
- Transcripts live in `~/.claude/projects/<project>/<session id>.jsonl`.
- The session file appears only after the trust-folder dialog is answered.

**When a Claude Code release breaks this**: symptoms are sessions missing from
`status`, `skip session file …` warnings in the journal, or a new
`claude_version`. Fix `internal/claudesessions` only (field names in
`sessionFile`, the `kind` filter, `ExtractFlags` for CLI changes) and the
dialog table in `internal/restore/dialogs.go` for UI text changes. If the
session files disappear entirely, the planned fallback is a
SessionStart/UserPromptSubmit hook that writes the same `Session` struct from
`$TMUX_PANE`, `session_id` and `cwd`.

## Registry (schema 1)

```json
{"schema": 1, "host": "<hostname>", "machine_id": "<machine-id>", "updated_at": "…",
 "entries": {"<tmux session>": {
   "tmux_session": "…", "session_id": "<uuid>", "cwd": "/home/<user>/…",
   "name": "…", "name_source": "user|derived", "remote_control": true,
   "bridge_session_id": "session_…",
   "flags": ["--dangerously-skip-permissions"], "last_status": "idle|busy|shell",
   "claude_version": "…", "state": "active|stopped|excluded",
   "first_seen": "…", "last_seen": "…", "last_seen_boot": "<boot_id>",
   "restore": {"attempts": 0, "last_result": null, "last_at": null, "in_progress": false}}}}
```

`bridge_session_id` is omitted while Remote Control is off. `host` and
`machine_id` leave room for aggregating several machines later.

### Entry states

- `active`: restored at boot.
- `stopped`: closed by the user. An entry becomes `stopped` only if it was
  seen alive **during the current boot** (`last_seen_boot` equals the kernel
  boot id), has been absent longer than `removal_debounce_s`, and no restore
  is in progress for it. Entries from an earlier boot are left to restore, and
  a daemon restart keeps this knowledge. A stopped session that comes back
  alive is `active` again.
- `excluded`: set by the user; never restored and never changed by the
  snapshot daemon.

The daemon writes after every pass and does nothing more once it gets
SIGTERM, so Claude processes exiting during shutdown are never recorded as
user-closed.

## Restore algorithm

1. Take `restore.lock` (non-blocking; a second run fails fast).
2. Candidates: `active` entries whose session id and tmux name are not held by
   a live Claude. Two entries with one session id: the most recently seen
   wins (`duplicate_id`). An existing tmux session without Claude is a
   `conflict` and is never touched.
3. Set `restore.in_progress` on every queued entry (and clear stale flags), so
   the snapshot daemon never marks one `stopped` while it waits or backs off.
4. Wait for `network_check` (TCP) and, per entry, for its cwd, up to one
   shared deadline.
5. Per entry, `stagger_s` apart:
   - Missing transcript: fresh start in the cwd (no `--resume`, no nudge).
     Power-loss damage: the original is backed up, then a complete last record
     only missing its newline gets one, or a partial or zero-filled tail (up
     to 3 lines) is dropped.
   - Re-check that the tmux name is still free, then
     `tmux new-session -d -P -F '#{session_id}' -s <name> -c <cwd> -- $SHELL -ic '…'`
     inside `systemd-run --user --scope`. `#` in the name and cwd is escaped
     because tmux expands formats there, and the command is passed as argv.
   - Poll the pane once a second:
     - Answer known dialogs. A dialog is recognised only as a real menu: its
       title text plus a row that is exactly the option label, in the same
       block as the focused `❯` row.
     - Accept once Claude's session file for this tmux session exists and 3
       polls in a row show no dialog. The cwd must match the entry; a
       different session id gives `other_session`.
     - Fail fast when the pane is back at the shell: resume error, `claude`
       not found, or exit.
   - Remote Control: on → wait for a bridge id, else type
     `/remote-control <name>` once (`rc_new_bridge` / `rc_failed`); off → a
     bridge appearing is reported (`rc_unexpected`).
   - On failure, kill only the session this attempt created (by tmux id) and
     retry after `backoff_s`. The last failed attempt is left alive for
     inspection. A resume failure after a transcript repair restores the backup
     and falls back to a fresh start.
6. Record `restore.{attempts,last_result,last_at}`, clear `in_progress`, write
   `last-restore.json`, log one line per entry.

Invariant: no restore path kills a tmux session it did not create in this
run.

## systemd units

- `agent-keeper-snapshot.service`: `Type=simple`, `Restart=always`, runs
  `%h/.local/bin/agent-keeper daemon`.
- `agent-keeper-restore.service`: `Type=oneshot`, runs `restore --boot`,
  `KillMode=process`, `TimeoutStartSec=infinity`.
- Both `WantedBy=default.target`, with no `PrivateTmp` (the tmux socket lives in
  `/tmp/tmux-$UID`). User units cannot order after the system's
  `network-online.target`, hence restore's own network wait.

The tmux server started by restore lives in its own `run-*.scope`, not in
the restore unit's cgroup, so finishing or restarting a unit never kills
restored agents. `KillMode=process` is a second guard.

Conflicting keepers: `install` scans `~/.config/systemd/user/*.service` for
an `ExecStart` (or the script it points to) that mentions both `tmux` and
`claude`. Their timers are disabled and stopped, and the services only
disabled. Stopping a service could kill a tmux server it owns. What was
changed is recorded in `disabled-conflicts.json` (the first recorded state
wins on re-install), and `uninstall` restores it.

## Roadmap

- Watchdog: restart agents that exit while the machine stays up.
- Notifications (restore summary to phone/chat).
- Multi-machine view built on `host`/`machine_id`.
- Native Claude background-session backend behind the `internal/tmux`
  boundary.
