# ADR-012: Dispatcher workspace teardown (incl. before_remove hook) runs on the worker goroutine

- **Status**: Accepted
- **Date**: 2026-06-02
- **Authors**: devthejo
- **Code context**: [`pkg/dispatcher/loop.go`](../../pkg/dispatcher/loop.go)
  (`runWorker` — teardown now invoked here before `postFinished`),
  [`pkg/dispatcher/commands.go`](../../pkg/dispatcher/commands.go)
  (`cleanupWorkspace` new signature + `before_remove` invocation; `finishRun`
  success branch no longer cleans up),
  [`pkg/dispatcher/hooks.go`](../../pkg/dispatcher/hooks.go) (`Hooks.BeforeRemove`,
  `Hook.Run`), [`pkg/cli/dispatch_defaults.go`](../../pkg/cli/dispatch_defaults.go)
  (the former destructive default hook is intentionally absent),
  [`pkg/dispatcher/workspace.go`](../../pkg/dispatcher/workspace.go)
  (run-generation paths and external ownership tombstones),
  [`pkg/dispatcher/config.go`](../../pkg/dispatcher/config.go)
  (`WorkspacePersistPolicy`). Tests:
  [`pkg/dispatcher/cleanup_workspace_test.go`](../../pkg/dispatcher/cleanup_workspace_test.go).
  Related: [ADR-011 retry attempt cap](015-dispatcher-retry-attempt-cap.md)
  (the `blocked` give-up path a leaked-worktree re-dispatch failure would otherwise hit).

## Context

The dispatcher has four workspace-lifecycle hooks (`after_create`,
`before_run`, `after_run`, `before_remove`). Three were invoked by `runWorker`;
`before_remove` was **declared, validated, path-expanded, wired by default, and
documented as load-bearing — but never called anywhere**. At the time of the
original decision, `BuildDefaultConfig` installed a hook running `git -C
$PROJECT_DIR worktree remove --force $ITERION_WORKSPACE`; meanwhile
`Workspaces.Remove` only recursively deleted the directory and did not update
Git registration.

Teardown lived in `finishRun`'s clean-success branch (`cleanupWorkspace`), which
called `Workspaces.Remove` → `os.RemoveAll` only. `finishRun` runs on the
dispatcher's single **actor goroutine**. So the obvious "just call
`before_remove` in `cleanupWorkspace`" fix would run a shell command — bounded
only by the hook's own timeout (default 60s) — on the actor, freezing all
polling, dispatch, retries, and snapshot serving for its duration.

The impact of the dead hook: under the default `workspace.persist: keep` the
hook is dormant dead code (the directory is never removed, so the git
registration stays valid). But the moment an operator enables the documented
`cleanup_on_done` / `cleanup_on_terminal` policy, every completed issue's
directory is deleted while its host-repo worktree registration leaks.
`git worktree list` fills with stale entries, and **re-dispatching a
previously-cleaned issue fails**: the workspace path is keyed by issue ID, so
`after_create`'s `git worktree add` (no `-f`) hits "already registered", the
run errors, retries, and the ticket lands in `blocked` (ADR-011) with a cause
invisible on the board — a silent dispatch failure in the exact board →
dispatcher → result loop.

## Decision

Perform workspace teardown in `runWorker` (the per-dispatch worker goroutine),
immediately after a clean `Runner.Dispatch` return and **before**
`postFinished`. `finishRun`'s success branch no longer cleans up.

The sequence shipped today (`cleanupWorkspace`, pkg/dispatcher/commands.go) is:

1. Skip cleanup entirely when `git status --porcelain` reports a dirty
   working tree — uncommitted work is never destroyed, the workspace is kept
   and the operator is told where it is.
2. Retire the external ownership marker for the exact issue/run generation.
   A later logical run has a different path; this generation can never become
   authoritative again. A failure to retire preserves the workspace.
3. Run the snapshotted `before_remove` hook. A failing hook is **logged and
   removal proceeds** — the hook is operator code, not a safety gate.
4. Remove the owned directory for that generation (`RemoveForRun`).
5. Deregister that exact linked worktree against the host repository's common
   gitdir (`git worktree remove --force`), but only once the path is confirmed
   absent — if a late writer recreated it, the registration is retained
   fail-closed.

A stronger teardown proof — exact-HEAD/durable-ref ownership verification,
atomic quarantine to a recovery path with a sidecar manifest, and a
live-process quiescence census before non-forced Git cleanup — is **not**
implemented here. It is follow-up work; nothing in this ADR should be read as
describing behaviour that exists.

The hook receives the same `ITERION_*` environment and the same
config-snapshotted `Hooks` value the other hooks use, so a mid-flight reload
cannot swap the callback body. The old default `--force` hook was removed:
custom hooks remain explicit operator code, but stock teardown never delegates
its safety decision to a shell snippet.

Teardown is confined to the clean-finish path. Cancelled/failed dispatches keep
the workspace (retry resumes from it, the operator inspects it) — unchanged.
This is a faithful relocation: `cleanupWorkspace` was only ever reachable from
`finishRun`'s `err == nil` arm, which is only entered when the worker posts
`cmdRunFinished` with a nil error (`refreshRunningStates` and `reconcileStalled`
call `finishRun` with `context.Canceled`, hitting the cancel branch).

### Alternatives rejected

1. **Call `before_remove` synchronously inside `cleanupWorkspace` on the actor.**
   Rejected: a shell hook (≤ its timeout, 60s default) on the single actor
   goroutine stalls polling/dispatch/retries/snapshot serving. The whole reason
   the hook was a finding rather than a one-line fix is that the naive call site
   is on the wrong goroutine.
2. **Keep teardown in the actor's `finishRun` but offload hook+remove to a new
   goroutine tracked by `workersWG`.** Rejected for two reasons. (a) It opens a
   `Create`/`Remove` race: `finishRun` releases the tracker claim and (when
   `completed_state` is disabled or equals the running state) leaves the issue
   eligible, so the next tick can re-dispatch and `Workspaces.Create` the same
   per-issue path while the detached cleanup goroutine is mid-`RemoveAll`.
   (b) It adds a `WaitGroup`-reuse hazard (an `Add` racing the shutdown `Wait`)
   that has to be reasoned about. Doing teardown *before* `postFinished` sidesteps
   both: the directory is gone before the claim is released, and it reuses the
   worker's existing `workersWG` slot.
3. **Leave `before_remove` unused and document it as not-yet-wired.** Rejected:
   it ships in the default config and is advertised as the mechanism that keeps
   `git worktree list` clean. Shipping a validated, default-installed hook that
   silently never fires is the defect.

The non-obvious trade-off is **where** teardown runs. Moving it off the actor
and ahead of the claim release costs a small structural change (the success-path
cleanup no longer lives beside the other success-branch bookkeeping in
`finishRun`) but buys three properties at once: the actor never blocks on a
shell command, there is no re-dispatch/Create-Remove race, and shutdown still
drains cleanup via the worker's existing `workersWG` membership.

## Consequences

- The default `git worktree` workflow is now correct under `cleanup_on_done` /
  `cleanup_on_terminal`: the removed checkout is deregistered from the host
  repository instead of being left behind as a stale `git worktree list` entry.
  A workspace with uncommitted changes is kept, not cleaned.
- Workspace paths include the logical run generation. Re-dispatching a ticket
  cannot collide with an old retired path, even if an old absolute-path writer
  wakes after the new run starts.
- **Behaviour change:** workspace removal (and any `before_remove` hook) now
  happens on the worker goroutine just before the run is reported finished,
  rather than on the actor just after. The directory is gone slightly earlier
  in the lifecycle (before the claim release / completed-state transition);
  nothing in `finishRun` depends on the workspace still existing (`stampLastRun`
  reads `run.json`, not the workspace tree).
- `cleanupWorkspace`'s signature changed to take the snapshotted `*Hook` and
  env; its only caller is `runWorker`.
- Follow-up safety hardening (2026-07): the destructive `before_remove` default
  hook was removed. Teardown now retires an ownership marker, skips cleanup
  entirely when the tree is dirty, removes the owned generation directory, then
  deregisters that exact linked worktree against the host repository's common
  gitdir. A failing `before_remove` hook is still only logged — removal
  proceeds. The runtime-side exact-HEAD/durable-ref proof, atomic quarantine
  and live-process census are **not** implemented on this branch; they are
  tracked as follow-up work, so the direct-writer and clean-commit loss windows
  remain open.
- Default `workspace.persist: keep` is unaffected — teardown (and therefore the
  hook) remains a no-op, asserted by `TestCleanupWorkspace_SkippedUnderKeepPolicy`.
