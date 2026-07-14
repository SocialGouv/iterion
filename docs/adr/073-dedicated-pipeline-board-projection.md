# ADR-073 — A dedicated pipeline board projection

Status: **proposed** (2026-07-14). Refines ADR-071 and implements the core
product direction described in [issue #125](https://github.com/SocialGouv/iterion/issues/125)
without replacing the native backlog board.

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

### D1 — Keep both products

Keep the native board and its `/board` route unchanged. Add a second Studio
surface:

- `/pipelines` lists the pipeline boards available from the bot registry;
- `/pipelines/{bot}` opens the board whose identity is that bot;
- `/api/v1/pipeline-boards/{bot}` builds the corresponding server-side read
  model.

For this first slice the board identity is deterministic (`bot:<bot-id>`). It
does not need a new persistent configuration store: creating or removing a bot
creates or removes the corresponding projection. A future need for aliases,
custom access rules or several boards for one bot would justify persistent
board definitions without changing the projection contract.

### D2 — Derive columns from the workflow and runtime

The server compiles the selected bot and returns, in stable graph order:

1. `Todo`;
2. `Running`;
3. one named column per statically declared human-capable interaction;
4. named columns discovered from currently paused child workflows;
5. `Other input` for a dynamic pause that cannot be matched safely;
6. `Needs attention`;
7. `Done`.

`Running` and `Needs attention` are necessary completeness states: omitting
them would make active and failed pipeline instances disappear between `Todo`
and `Done`. The interaction columns remain the domain-specific centre of the
view.

Columns are not drag targets. A card's column is calculated from its current
run status and checkpoint. `paused_waiting_human` is placed using
`(workflow_name, checkpoint.node_id)`; answering delegates to the existing
structured run `Resume` contract. `queued`/`running` map to `Running`, terminal
success to `Done`, and failed/cancelled/operator-paused runs to
`Needs attention`.

### D3 — Project tasks, attempts and descendants

The native issue remains the ingestion record for this slice. Only issues
explicitly pinned to the board bot (using the registry's canonical name
matching) belong to it. `Issue.LastRunID` is the current completed-attempt
pointer; while the dispatcher is still running, the newer run's persisted
source-issue edge takes precedence. `Issue.Runs` remains the attempt history.
Descendant runs are walked recursively through the run store and rendered as
separate, nested cards, so parallel children may wait in different interaction
columns at the same time.

Top-level runs genuinely associated with the bot but not referenced by a
native issue are also projected. This ensures manual, API and scheduled runs do
not disappear merely because they bypassed the dispatcher backlog.

The canonical board-scoped ingestion endpoint is:

```http
POST /api/v1/pipeline-boards/{bot}/tasks
```

The bot comes from the path and cannot be overridden by the request body. The
endpoint creates a native issue in the first column, or in the first eligible
state when the caller explicitly requests immediate start. Existing native
REST/MCP/forge ingestion remains valid and appears in the projection whenever
it pins the same bot.

### D4 — Keep projection and execution tenant-scoped

In local mode the projection reads the configured filesystem board. In cloud
mode it resolves the active team's board per request. Run listing, lookup and
descendant traversal retain the authenticated request context so the run store
applies the same tenant boundary. The aggregated endpoint never accepts a
tenant ID from its body or query.

## Consequences

- Existing native board data and dispatcher behaviour require no migration.
- A pipeline board is useful before its first run because its interaction
  columns come from the bot source.
- Answering a root or child interaction uses the exact paused run ID; no
  synthetic tracker transition is needed.
- Polling costs one aggregate request instead of an issue request plus an N+1
  run-tree walk from the browser.
- The first slice intentionally reuses native issues as task ingestion records
  instead of introducing a second task store.

## Known limits and follow-ups

- Historical runs with neither bot metadata nor a reliable source-issue link
  cannot be associated safely and are not guessed into a board.
- The projection follows the current bot source. Persisting a topology snapshot
  per workflow hash would make old attempts exactly reproducible after source
  edits.
- Run parentage is only as complete as persisted `ParentRunID` links. This
  change fixes new fork/subbot paths where practical; legacy missing links stay
  as independent roots rather than being heuristically attached.
- Native issue state and the run lifecycle can briefly disagree while a
  dispatcher claims or reconciles a card. Run state wins once a current run is
  known.
- A future first-class `PipelineInstance` store may add idempotency keys,
  explicit attempt correlation and durable board configuration. It should
  preserve the API principle established here: board identity chooses the
  pipeline, while columns remain a derived execution projection.
