# Post-mortem shell

Open an interactive shell **inside a run's preserved worktree**, from
the studio, to inspect what a failed (or paused/cancelled) run left
behind: read files, `git log`/`git diff`, re-run a failing test —
without hunting for `<store-dir>/worktrees/<run-id>` by hand.

Local mode only (`server_info.run_shell_enabled`). Cloud mode never
offers it: spawning an interactive host shell from a multi-tenant API
is not a thing.

## Where it appears

On a run's header, the row **“worktree preserved → Open post-mortem
shell”** shows when all of these hold:

- the run is **at rest** — `failed`, `failed_resumable`, `cancelled`,
  `paused_*` or `finished` (a live run's worktree belongs to the
  engine; a concurrent interactive writer would race its git
  operations);
- the run recorded a working directory and it still exists on disk
  (the engine preserves worktrees on error precisely for this);
- the studio talks to the daemon that owns the store (cross-store
  runs are read-only).

The button and the endpoint share the same gate, so the affordance
never 409s.

## Semantics

- **One shell per connection.** Closing the panel ends the shell;
  reopening spawns a fresh one. The worktree is the state — the shell
  is a viewer, not a persistent tmux session.
- `$SHELL -l` (`/bin/bash` fallback), `TERM=xterm-256color`,
  `COLORTERM=truecolor`, cwd = the preserved worktree, `ITERION_RUN_ID`
  exported.
- **Idle timeout 30 min** — counting BOTH directions, so a running
  `htop` or a long build's output keeps the session alive without
  keystrokes. **Absolute cap 2 h.** Overridable via
  `ITERION_RUN_SHELL_IDLE_TIMEOUT` / `ITERION_RUN_SHELL_MAX_LIFETIME`
  (Go durations). On teardown the whole process group is killed — a
  `sleep 999 &` left behind does not outlive the socket.
- Unix only; the Windows desktop build answers 501.

## Wire protocol (for tooling)

`GET /api/ws/runs/{id}/shell?cols=&rows=` upgrades to a WebSocket:

| Direction | Frame | Payload |
|---|---|---|
| both | binary | raw PTY bytes (no envelope, no base64) |
| C→S | text | `{"type":"resize","cols":N,"rows":N}` · `{"type":"ping"}` |
| S→C | text | `{"type":"pong"}` · `{"type":"exit"}` · `{"type":"error"}` |

Refusals answer plain HTTP before the upgrade: 403 cloud mode, 404
unknown run, 409 live run / no workdir / cross-store, 410 worktree
gone or run deleted, 501 unsupported platform.

## Relation to other affordances

The **Files** panel (read/edit via Monaco) stays the right tool for
targeted file edits; the shell is for *investigation* — running the
repo's own commands in the exact state the run left. For a run that
needs code changes then a retry, combine: inspect in the shell, edit,
then `Resume`.
