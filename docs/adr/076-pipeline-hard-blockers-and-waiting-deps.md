# ADR-076 — Pipeline hard blockers, waiting_deps, and unified launch admission

Status: **accepted** (2026-07-18). Follow-up to [ADR-074](074-dedicated-pipeline-board-projection.md)
and the multi-pipeline “design-first” product brief (Town production bus).

## Context

The native tracker already stored `Issue.Blockers []string` and the dispatcher
adapter skipped candidates whose blockers were not all *terminal*. Two gaps
blocked multi-pipeline production graphs (mesh → humanoid → feature):

1. **Satisfaction was any-terminal.** `StateBlocked` is terminal (“won't do”),
   so a cancelled/abandoned dep would *satisfy* dependents and make them
   eligible — wrong for hard asset deps that must finish successfully.
2. **The studio `/pipelines` launch loop ignored blockers entirely.** A ticket
   in `StateReady` with open blockers could start from the admission loop while
   `iterion dispatch` would skip it.
3. **No non-terminal hold state for open deps.** Operators had only
   `blocked` (terminal) or “stay Ready and hope”, neither of which models
   “written ticket waiting on upstream tickets.”

## Decision

### D1 — Ready only when hard deps are satisfied; else `waiting_deps`

`POST …/tasks/{id}/ready` with `ready:true`:

- if every blocker is `StateDone` → `StateReady`;
- else if the board has `waiting_deps` → park there;
- else → `409` with `open_blockers` in the JSON body.

Create-with-`start:true` uses the same rule. Unstage (`ready:false`) goes to
`backlog` (fallback `inbox`).

### D2 — Blocker satisfied ⇔ `state == done` only

Shared helpers in [`pkg/dispatcher/native/blockers.go`](../../pkg/dispatcher/native/blockers.go):

- `BlockerSatisfied(iss)` — only `StateDone`;
- `BlockersSatisfied` / `ResolveBlockers` — missing IDs fail closed (open);
- `CanLaunch` / `LaunchBlockedReason` — bot + `StateReady` + blockers all done.

Used by:

1. `Adapter.ListCandidates` (dispatcher);
2. `admitReadyPipelines` (studio launch loop);
3. projection `launch_blocked_reason` / `open_blocker_count`.

### D3 — Auto-promote when unblocked

When an issue transitions to `done`, dependents in `waiting_deps` whose blockers
are now all satisfied move to:

- **`backlog`** by default (human decides Ready);
- **`ready`** when `bot_args.auto_ready` is a truthy string (`true`/`1`/`yes`).

Emits `issue_unblocked` (+ `issue_state_changed` with `reason: unblocked`).

### D4 — Reverse index on read (V1)

`blocking: [{id, title}]` is computed over the full issue list at projection
time. No stored reverse index.

### D5 — Cycle rejection at write

`Create` / `Update` reject blocker lists that would create a cycle
(including self-ref). Missing blocker IDs remain allowed (fail closed at launch).

### Default board + migration

New state:

| name           | display           | eligible | terminal |
|----------------|-------------------|----------|----------|
| `waiting_deps` | Waiting on deps   | false    | false    |

`UpgradeBoardSchema` inserts it after `ready` (else after `backlog`) on
existing boards, same additive pattern as `awaiting_input`.

`StateBlocked` stays terminal “won't do” and does **not** satisfy hard deps.

## Projection / API surface

On each pipeline-board card (ticket-backed):

```json
{
  "blockers": [{"id":"…","title":"…","state":"…","bot":"…","satisfied":true}],
  "open_blocker_count": 0,
  "launch_blocked_reason": "open_blockers|waiting_deps|…",
  "blocking": [{"id":"…","title":"…"}]
}
```

`create` / `PATCH` accept `blockers: string[]`. Events:
`issue_blockers_updated`, `issue_unblocked`.

## Consequences

- Multi-pipeline DAGs (feature blocked on meshes) work on both dispatch and
  `/pipelines` without GitHub Issues as the bus.
- Custom boards without `waiting_deps` still gate launch via
  `open_blocker_count` / `CanLaunch`; only Ready staging is stricter (409).
- Soft deps, artefact acceptance, and upsert-by-`input_path` stay later waves
  (V2/V3 of the product brief).

## Follow-ups shipped in the same wave (V2–V3)

- **Ticket contract** (`bot_args` vocabulary + drawer strip) —
  `ticket_contract.go`.
- **Upsert** by `(bot, bot_args.input_path)` on `POST …/tasks?upsert`.
- **require_blocker_labels** optional gate on hard blockers (artefact
  acceptance without a second state machine).
- **Bulk ready** + **recompute-deps** endpoints; **dependency-graph** GET.
- **UI filters**: pipeline_kind, family_id, waiting-on-deps only.

## Non-goals (still deferred)

- Soft-deps / scoring, path-based artefact file checks, board-per-bot, proxy
  Godot fields (`placement_ref`), GitHub as pipeline source of truth.
