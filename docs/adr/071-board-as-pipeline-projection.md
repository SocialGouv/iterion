# ADR-071 — Board as a projection of a pipeline (additive)

Status: **proposed** (2026-07-12). Records the target direction from
[issue #125](https://github.com/SocialGouv/iterion/issues/125) and the
decisions taken on its load-bearing forks. Frames the shift **additively**:
the pipeline view is a projection layered over the existing shared board, not
a replacement of it. The first concrete increment (a short-term slice on
existing seams) ships alongside this ADR; the deeper inversion (a ticket that
*is* a pipeline instance with a child/shards tree) stays an open tension.

**Refined by [ADR-074](074-dedicated-pipeline-board-projection.md):** its
dedicated bot-bound surface and named interaction columns supersede D1–D2 here.

## Context

Today the native kanban board is a **generic shared backlog**: a queue of
tickets the dispatcher consumes to launch runs. The proposal in #125 is to
evolve it toward a **live projection of a pipeline** — one board = one
pipeline (+ its children), columns = the human interactions the pipeline
needs, and cards in a "human feedback" column answerable directly from the
board.

The current model, verified against the source (2026-07-12):

- **`Board{States,Fields,Views,UpdatedAt}` has no identity** — no id/name/
  pipeline/bot. Identity *is* the storage path; one board per store (local) /
  per tenant (cloud) ([board.go](../../pkg/dispatcher/native/board.go)).
- **The ticket carries the bot** (`Issue.Bot`/`BotArgs`/`Assignee`), not the
  board. The dispatcher routes *many* bots from one board, per-ticket, never
  per-column ([config.go](../../pkg/dispatcher/config.go)).
- **Ticket→run is a single overwritten `LastRunID` pointer** — no history, no
  parent/child (only `Blockers`). The parent/child/shards tree lives only in
  `pkg/queue`, invisible to the board.
- **`State` carries only `Eligible`/`Terminal`** — no "awaiting human"
  semantic; it is only *implied* by a non-eligible non-terminal column.
- **The human answers off the board.** A run that pauses goes
  `paused_waiting_human` and is answered only via the run console /
  `iterion resume`; a *dispatched* run that pauses is parked in-progress and
  the operator is sent to the run console
  ([commands.go](../../pkg/dispatcher/commands.go)). The board's only human
  channel (comments / slash-commands) *launches a new run*.

Two facts about the seams matter for the design:

1. **`QueueMessage` cannot unblock a paused human node** — it only queues a
   cooperative message drained into the system prompt at the next pause. Only
   **`Resume`** records structured answers and re-enters the graph. So
   answering-from-board must target `Resume`.
2. **A pause was signal-less on the trigger bus** — the run-lifecycle kinds
   were only `run.finished/failed/cancelled`, and `emitRunCompletion`
   *mislabeled* a pause as `run.failed`. A board projection that marks a card
   "awaiting input" needs a real pause signal.

## Decision

### D1 — Additive projection, not replacement
Keep the shared backlog board as the dispatcher's substrate. Add the pipeline
view as a **new projection** (a filtered lens over the same store), not a new
per-pipeline board file. The two answer different questions — "what's queued,
all bots" vs "where is THIS pipeline and what does it need from me" — and the
dispatcher's per-state concurrency / eligibility / one-tracker-per-config
model, which rests on a single shared board, stays untouched.

### D2 — A single generic "awaiting input" column
Human interactions are **dynamic** (0, 1, or N per run, schema-driven) and do
not map cleanly onto a fixed set of columns. Rather than statically extracting
per-interaction columns from the IR graph (fragile under loops / conditionals
/ dynamic `ask_user`), a **single "awaiting input" column** holds every paused
card, and each card carries its own pause state (node + questions) read from
the run checkpoint.

### D3 — Answer from the board via `Resume` + a real pause signal
The answer affordance reuses the run-console interaction contract verbatim
(`PauseForm` → `POST /api/runs/{id}/resume {answers}`), keyed on
`Issue.LastRunID → GET /api/runs/{id}` and gated on `paused_waiting_human`.
A new `run.paused` trigger kind (`KindRunPaused`) replaces the mislabeled
`run.failed`, so a future live projection can subscribe to "this card now
needs input" the same way `board_source` bridges board→run.

## Tensions — decided or open

| # | Tension | Resolution |
|---|---------|-----------|
| T1 | Shared board → board-per-pipeline breaks dispatcher concurrency/eligibility semantics | **Decided (D1):** unchanged. The shared board stays the substrate; the pipeline view is a filter, so the limits keep meaning. |
| T2 | Where does a board's identity live + how does it bind to a pipeline? | **Decided (D1):** a projection is scoped by a *filter* over the shared store (labels / bot), not a new board identity. Inverting the `kind:board` bot→board subscription is not required for the projection. |
| T3 | Dynamic per-run interactions → fixed columns | **Decided (D2):** one "awaiting input" column, per-card pause state. |
| T5 | Which seam to answer from? | **Decided (D3):** `Resume` (not `QueueMessage`) + a `run.paused` signal. |
| T4 | Project the parent/child/shards tree onto cards | **Open.** Needs a ticket↔runs **1:N** model; today it is a single `LastRunID` pointer and the tree lives only in `pkg/queue`. A follow-on. |
| T6 | Is the board the source of truth for forge ingestion (self-hosted), or a mirror? | **Decided: mirror.** Generalize the cloud forge→board sync to self-hosted (`syncForgeIssuesToBoard`); the forge stays authoritative. |

## First increment (shipped with this ADR)

A short-term slice on existing seams, independent of the open tensions:

- **`run.paused` trigger signal** — `KindRunPaused` + fix the
  `emitRunCompletion` mislabel so a pause emits `run.paused` (carrying
  `node_id` + `interaction_id`), not `run.failed`
  ([trigger_emit.go](../../pkg/runview/trigger_emit.go)).
- **Answer-from-board affordance** — the paused-run answer form mounted on the
  board card's Last-run panel, reusing `PauseForm` and resolving to `Resume`
  with **no source** (the server falls back to the run's persisted FilePath;
  passing the operator's editor buffer would resume an unrelated workflow)
  ([LastRunSection.tsx](../../studio/src/views/Board/issueModal/LastRunSection.tsx)).
- **`board.create` MCP parity** — the `create_issue` boardop now accepts
  `bot`/`bot_args`, matching REST `POST /issues`
  ([ops.go](../../pkg/dispatcher/native/boardops/ops.go)), so a board-fed
  pipeline can be pinned at create time on all three transports.
- **Unified "feed the first column"** — the canonical ingest contract
  (`POST /issues` with no `state` → first column, PAT `iap_`) is documented,
  and the forge→board sync core is extracted store-agnostic
  (`syncForgeIssuesToBoard`) so a self-hosted import can reuse it.

## Consequences

- No migration: the change is additive; existing boards, dispatchers, and
  bots are unaffected.
- The `run.paused` fix removes a latent bug (a pause polluting the run-failed
  chain), independent of the board work.
- **Deferred** (open tensions, follow-ons): the ticket↔runs 1:N history and
  parent/child/shards tree projection (T4); the self-hosted forge-import entry
  point (a forge client + repo→board mapping without a cloud integration
  store) that wires `syncForgeIssuesToBoard` (T6); a board-wide per-card
  "awaiting input" badge (needs a denormalized `awaiting_input` signal on the
  issue to avoid an N+1 run fetch on every board render); and moving a parked
  dispatched run's card into the "awaiting input" column on pause.

## Addendum (2026-07-13) — T6 self-hosted forge-import entry point shipped

The T6 follow-on deferred above (a forge client + repo→board mapping without a
cloud integration store) has landed as **`iterion issue import`**
([issue.go](../../pkg/cli/issue.go)) backed by the exported
`server.ImportForgeIssues` ([board_forge.go](../../pkg/server/board_forge.go)).

- **Reuses the core verbatim.** `ImportForgeIssues` is a thin
  construct-then-delegate shim: it builds the `forge.IssueClient` via the same
  provider switch as `forgeAdminForToken` (kept DRY in one place), then calls
  the unchanged `syncForgeIssuesToBoard`. No sync logic is forked; the cloud
  path is untouched.
- **No integration store.** The self-hosted caller has no persisted
  `forge.Connection`, so the connection id is empty — the deterministic card id
  keys on `provider:repo#number` alone, and the high-water mark (`--since`) is
  the operator's concern, passed per-invocation (the ADR's "the high-water mark
  is the caller's concern" is what let the pure core stay reusable here).
- **Secrets discipline.** The forge token is read only from the env var named by
  `--token-env`, never a flag value.

This closes T6's self-hosted arm; the deeper T4 tree projection remains open.

## Addendum (2026-07-15) — the #125 campaign shipped the deferred follow-ons

The "Deferred" list above is now history — the board-as-pipeline campaign
(GitHub epic #125, delivered 2026-07-12→14 by the Featurly/Billy factory)
landed all four:

- **T4 ticket↔runs 1:N history** — native cards keep an append-only run
  history (`RunRefs`, newest-last; see `TestSetLastRunAppendsRunHistory`),
  rendered as the per-card run history (#154).
- **T4b parent/child/shards tree** — the run store carries the lineage
  (`ParentRunID` + `ShardIndex`/`ShardCount`/`ShardLabel` on `store.Run`,
  reverse query `ListChildRuns` — [pkg/store/iface.go](../../pkg/store/iface.go)),
  projected as the run tree in the console and on the card (#162, #169).
- **Per-card "awaiting input" badge** — denormalized signal, no N+1 run
  fetch (#161).
- **"Awaiting input" column** — a paused dispatched run's card moves into
  the generic `awaiting_input` state and back on resume (#173), with the
  awaiting-input lifecycle hardened for cloud parity (#179, #181).

The 2026-07-13 addendum above (T6 `iterion issue import`) plus this one
close every deferred item; the ADR is fully implemented.
