# ADR-073 — A dedicated pipeline board projection

Status: **proposed** (2026-07-14, revised same day). Refines ADR-071 and
implements the core product direction described in
[issue #125](https://github.com/SocialGouv/iterion/issues/125) without replacing
the native backlog board.

> **Revision note.** An earlier draft of this ADR proposed one board *per bot*
> (`/pipelines/{bot}`) with workflow-derived interaction columns. Dogfooding
> that shape showed the operator's real need is a **single control center**:
> "show me every running pipeline and let me answer the human reviews they are
> blocked on, across all bots, in one place." This revision therefore replaces
> the per-bot boards with **one global board** of four fixed lanes and folds a
> pipeline's children into its root card, and it adds a **local concurrency
> gate** so the Todo lane means "waiting for a run slot." The per-bot decisions
> below are superseded by D1–D5 as written here.

## Context

ADR-071 deliberately kept the first increment small: the native board gained
a bot filter, run history, an awaiting-input state and an answer form. Those
seams are useful, but a filtered backlog still answers a backlog question:
"which cards currently select this bot?" It does not answer the pipeline
question from #125: "where are the instances of this pipeline, including its
children, and which interaction needs me?"

The distinction is structural:

- `/board` is an editable tracker. Its columns are dispatcher lifecycle states
  and moving a card changes tracker state.
- a pipeline board is a read model over execution. Its columns come from the
  workflow and run statuses; moving a card by hand would make the view lie.
- the backlog can contain cards for many bots. A pipeline board has one bot as
  part of its identity, rather than carrying a temporary client-side filter.
- a paused child run must be answerable in its own interaction column even when
  the root run is still active.

Creating another mutable `board.json` per bot would duplicate dispatcher state
and re-open the concurrency and migration problems identified by ADR-071.
Conversely, implementing the projection entirely in the browser would require
an N+1 walk over issues, runs, checkpoints and descendants, and could not
derive columns before the first run exists.

## Decision

### D1 — One global pipeline board (a control center), not one board per bot

Keep the native board and its `/board` route unchanged. Add a **single** Studio
surface:

- `/pipelines` renders one global board of every root pipeline, across all bots;
- `GET /api/v1/pipeline-board` builds the server-side read model.

There is no per-bot board and no `/pipelines/{bot}` route. The purpose is
operational: let one operator watch and unblock many pipelines at once. Because
the view is a projection of persisted run state, cards are **not** drag targets.

### D2 — Four fixed lanes; children fold into the root card

The board has exactly four lanes: **Todo**, **In progress**, **Done**,
**Attention**. Columns are no longer derived from a workflow's graph.

A card is one **root** pipeline — a run with no parent (`ParentRunID == ""`,
with a `ForkedFrom` compatibility fallback). Every descendant run is **folded
into its root's card** rather than shown as a separate card, so the operator
sees pipelines, not a forest of sub-runs. The root card aggregates:

- **progress** — node-weighted `Σ executed / Σ total` over the root and all
  descendants, where `executed` is the count of distinct `node_started` events
  and `total = len(workflow.Nodes)` (compiled per file path, memoized). A
  finished run clamps to 100%; a queued run reports `0/total`.
- **pending reviews** — every `paused_waiting_human` run anywhere in the tree,
  each carrying its exact `run_id` + `node_id` + questions. The card presents
  them **one at a time**; answering delegates to the existing structured run
  `Resume` contract keyed by that descendant's `run_id`.

Lane placement is computed from the root's status, with one override: **a tree
blocked on any human review is `In progress`** (the operator's turn) regardless
of the root's transient status. Otherwise: `queued → Todo`; `running` /
`paused_waiting_human → In progress`; `finished → Done`;
`failed` / `failed_resumable` / `cancelled` / `paused_operator → Attention`.
`Attention` is the deliberate fourth lane (the three-lane ideal would hide
failures); it carries a Resume affordance.

### D3 — Todo means "waiting for a slot"; Done shows the output

**Todo** holds pipelines that are not yet executing:

- runs `queued` by the local concurrency gate (D5), annotated with their FIFO
  position, and
- native tasks pinned to a bot that have not launched yet (their `bot_args` are
  the entry input). The native issue remains the ingestion record; there is no
  second task store in this slice.

**Done** cards surface the pipeline's **output**: the terminal `final_answer`
artifact field (the pinned `CallbackAnswerNode` first, else any artifact node),
falling back to a compact rendering of the latest-written artifact when no
`final_answer` exists.

Task ingestion is now global, so the bot moves from the URL into the body:

```http
POST /api/v1/pipeline-board/tasks   { "bot": "...", "title": "...", ... }
```

The handler validates the bot against the registry and creates a native issue
in the first column (or the first eligible state on `{start:true}`). Existing
native REST/MCP/forge ingestion still appears in the projection.

### D4 — Keep projection and execution tenant-scoped

Unchanged from the earlier draft. In local mode the projection reads the
configured filesystem board; in cloud mode it resolves the active team's board
per request. Run listing, lookup and descendant traversal retain the
authenticated request context so the run store applies the same tenant
boundary. The endpoint never accepts a tenant ID from its body or query.

### D5 — A local concurrency gate makes Todo real

Local launches previously started immediately; only `max_parallel_branches`
limited work *inside* a run, never the number of *pipelines*. This slice adds a
per-machine cap on concurrent **root** pipelines
(`--max-concurrent-pipelines`, default 3; `ITERION_MAX_CONCURRENT_PIPELINES`;
`runview.WithMaxConcurrentPipelines`). It lives in `runview.Service` as a
nil-safe guard (`pipeline_queue.go`, modelled on `runtime.DailyCapGuard`):

- **Admission** happens in `Service.Launch`, in the in-process branch, for root
  launches only (children never consume a slot). Under one mutex the guard
  either admits (records the run running and starts it) or, over the limit,
  persists a `queued` run doc and appends the launch to an in-memory FIFO,
  returning the run id + queue position immediately.
- **Dequeue** is driven by a scheduler goroutine woken (non-blocking signal)
  when a root's goroutine frees its slot, with a lazy poll tick as a backstop.
  A queued root starts via the engine's existing `queued → running` pickup —
  no engine change.
- **A paused pipeline frees its slot.** When a root parks on a human review its
  goroutine exits, so its slot is released and a queued pipeline can start while
  the operator thinks — exactly what a control center wants (paused reviews must
  not starve the queue). `Resume` deliberately bypasses the gate so answering a
  review proceeds immediately. The cap is therefore a soft cap on *launches*,
  not a hard ceiling on total concurrency: actively resuming several paused
  pipelines can briefly exceed it, bounded by deliberate operator actions.
- **Restart** recovers `queued` root docs into the FIFO (minimal spec
  reconstructed from the doc); non-persisted launch overrides are not recovered
  (a documented V1 limit). Drain leaves queued docs untouched, so they are
  never stranded.

The cap is in-process/local only: the cloud publisher path bypasses it (cloud
admission is the NATS queue + org/team gates).

## Consequences

- Existing native board data and dispatcher behaviour require no migration.
- One aggregate request drives the whole board; a paused review is answered by
  the exact paused run id with no synthetic tracker transition.
- Folding children into roots keeps the board readable as *pipelines*; progress
  and blocking reviews are still visible without a forest of sub-cards.
- The concurrency cap protects the host from an operator launching more
  pipelines than the machine can run; excess work waits visibly in Todo.
- Progress and event-scan costs are bounded to non-finished runs (finished
  clamps to 100% with no scan), and the active set is itself bounded by the cap.

## Known limits and follow-ups

- Progress compiles the *current* bot source, not the exact IR a historical run
  executed (a workflow-hash snapshot would make old attempts reproducible).
- `Done` output is empty for bots that emit neither a `final_answer` nor any
  artifact.
- Queue restart fidelity is partial: `file_path` + inputs survive, but
  backend/merge/branch/compress/permission overrides on a queued launch do not.
  Persisting the full launch spec (sidecar or run fields) is a follow-up.
- The unit of concurrency is one root run. Trigger-chained pipeline *stages*
  set no `ParentRunID`, so each stage counts as its own slot; a pipeline-lineage
  id would be needed to count a chain as one.
- Detached (`ITERION_RUNS_DETACHED`) and cloud launches do not honor the local
  gate in this slice (in-process only).
- A future first-class `PipelineInstance` store may add idempotency keys,
  durable launch specs and explicit attempt correlation while preserving this
  API's principle: the board is a derived execution projection, never a second
  mutable store.
