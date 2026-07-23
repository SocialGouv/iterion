# Native kanban tracker

Iterion ships with a first-class issue tracker — no Linear, no GitHub
account required. It is the default backing store for
[`iterion dispatch`](dispatcher.md), but is fully usable on its own
through the `iterion issue` CLI or the studio's Board view.

The native tracker is a deliberate design choice: iterion's autonomous
loop should not require the operator to lock themselves into a
proprietary issue tracker. External adapters (`github`, `gitlab`,
`forgejo`) are optional plug-ins, not the source of truth.

The studio's Board view (`/board`) is a drag-and-drop front end over
this store — cards carry labels, priorities, and per-card bot assignees:

![Studio kanban board with labelled issue cards](images/studio/board.png)

A label manager (`/board/labels`) keeps the vocabulary tight — bots read
this catalogue before emitting new issues, so renaming or merging a label
directly constrains future runs:

![Studio board label manager grouped by namespace](images/studio/board-labels.png)

## Storage layout

```
<store-dir>/dispatcher/
  board.json                  # column + custom-field schema
  issues/<encoded-id>.json    # one file per issue
  events.jsonl                # append-only audit log (monotonic Seq)
```

`<store-dir>` is the same directory the runtime store uses (resolved
via [`store.ResolveStoreDir`](../pkg/store/storedir.go)). Issue IDs
are `native:<uuid>` on the wire; the colon is illegal in NTFS
filenames, so the on-disk encoding swaps it for `__`.

Writes are serialized through a single mutex. Each mutation appends
one record to `events.jsonl`. The event types are:

`issue_created`, `issue_updated`, `issue_state_changed`,
`issue_deleted`, `issue_claimed`, `issue_released`,
`issue_last_run_updated`, `issue_comment_added`,
`issue_blockers_updated`, `issue_unblocked`, `label_rename`,
`label_merge`, `label_delete`, `board_updated`.

The dispatcher stamps every issue it processes with `last_run_id`
(the run that handled it) + `last_workdir` (the worktree path on
the host) when the run finishes — success, failure, or cancel.
The studio's Board view surfaces these as a "Last run" panel on the
issue modal with a link back to `/runs/<id>` and a
`vscode://file/<path>` shortcut to open the worktree.

## Board

`board.json` defines what columns exist on the kanban and what custom
fields the operator can attach to issues. Defaults:

```jsonc
{
  "states": [
    { "name": "inbox",          "display": "Inbox" },
    { "name": "backlog",        "display": "Backlog" },
    { "name": "ready",          "display": "Ready",       "eligible": true },
    { "name": "waiting_deps",   "display": "Waiting on deps" },
    { "name": "in_progress",    "display": "In progress", "eligible": true },
    { "name": "awaiting_input", "display": "Awaiting input" },
    { "name": "review",         "display": "Review" },
    { "name": "done",           "display": "Done",        "terminal": true },
    { "name": "blocked",        "display": "Blocked",     "terminal": true }
  ],
  "fields": [
    { "name": "bot_args", "display": "Bot args", "type": "text" }
  ]
}
```

`inbox` is the leftmost state. Bots with `board.create` capability
post their out-of-scope observations there (labeled `findings`) so
operators can triage on /board without a separate inbox surface —
drag inbox → backlog to promote, delete the card to dismiss.

`waiting_deps` holds a ticket whose **hard blockers** are not yet
`done` (see [ADR-076](adr/076-pipeline-hard-blockers-and-waiting-deps.md)).
Non-eligible and non-terminal: neither the dispatcher nor the
`/pipelines` launch loop will start it. Distinct from `blocked`, which
is terminal “won't do” and must **not** be used as a temporary hold
for open deps (a ticket in `blocked` does **not** satisfy anyone
else's blockers).

`awaiting_input` holds a dispatched card whose run paused for input
(a `human` node, or an operator soft-pause): the dispatcher parks the
card there — non-eligible, claim retained — and a per-tick sweep moves
it on once the run reaches a terminal status (see the "Paused runs"
section in [docs/dispatcher.md](dispatcher.md)). Boards persisted by an
older iterion are **schema-upgraded automatically** on store open
(filesystem) or on read (Mongo): missing `inbox` is prepended, missing
`waiting_deps` is inserted after `ready` (else after `backlog`),
missing `awaiting_input` is inserted right after `in_progress`.
Fully-custom boards without those anchors are left untouched for the
optional inserts.

| Property            | Meaning                                                            |
|---------------------|--------------------------------------------------------------------|
| `eligible: true`    | Dispatcher will dispatch issues sitting in this state (subject to hard-blocker gate). |
| `terminal: true`    | Dispatcher treats this state as a stop signal for *lifecycle*. Hard blocker satisfaction is **only** `done` (not every terminal state). |

A state can be both `eligible` and `terminal` — for example a
`completed` column that triggers a final wrap-up workflow before
issues leave the board.

### Custom fields

A board may carry typed custom fields. Schema is enforced on every
issue write — unknown fields and bad types are rejected.

```jsonc
{
  "states": [...],
  "fields": [
    { "name": "severity",  "type": "enum",   "enum_values": ["low", "medium", "high"], "required": true },
    { "name": "due_date",  "type": "date" },
    { "name": "story_pts", "type": "number" },
    { "name": "external",  "type": "bool" }
  ]
}
```

| `type`    | Value shape                                       |
|-----------|---------------------------------------------------|
| `text`    | string                                            |
| `number`  | int or float                                      |
| `bool`    | true / false                                      |
| `date`    | RFC3339 string                                    |
| `enum`    | string ∈ `enum_values`                            |

Field values are rendered into workflow inputs via
`{{issue.fields.<name>}}` in the dispatcher's `dispatch.vars` block.

### Comments

Each issue carries an append-only `comments` thread (`Issue.Comments`,
persisted with the issue). A bot writes to it through the
`board.comment` capability's `comment_issue` tool; every append emits
an `issue_comment_added` event. The thread is a lightweight discussion
record alongside the structured fields — the `events.jsonl` log remains
the full audit trail.

## CLI — `iterion issue`

The full CLI works against `<store-dir>/dispatcher/` directly; it does
**not** need the dispatcher daemon to be running.

```
iterion issue create   --title T [--body B] [--state S] [--label L]+
                       [--priority N] [--assignee A] [--blocker ID]+
                       [--field key=value]+

iterion issue list     [--state S]+ [--label L]+ [--assignee A]
                       [--claimed] [--unclaimed]

iterion issue show     <id-or-prefix>
iterion issue move     <id-or-prefix>  --to <state>
iterion issue update   <id-or-prefix>  [--title T] [--body B] [--labels L1,L2]
                                       [--priority N] [--assignee A]
                                       [--field k=v]+ [--clear-field K]+
iterion issue close    <id-or-prefix>          # → first terminal state

iterion issue board show
iterion issue board init [--from <board.json>]
```

`<id-or-prefix>` accepts the full `native:<uuid>` form, the bare
UUID, or any uniquely-matching prefix (e.g. the first 8 characters
shown in `list`).

`--field key=value` infers the type from the value: `true`/`false` →
bool, integers / floats → number, everything else → string. Use
`--clear-field key` to unset a value.

`iterion issue list` accepts `--json` (the global flag) to emit
machine-readable output:

```bash
iterion issue list --state ready --json | jq '.[].id'
```

### Per-ticket `bot` / `bot_args`

The `native.Issue` record carries dedicated typed routing fields:
`Bot` (string) and `BotArgs` (`map[string]string`). At **create**
time the CLI exposes them directly:

```bash
iterion issue create --title "Ship X" --state ready \
  --bot feature-dev --bot-arg feature_prompt="Ship X exactly as specced"
```

(`--field key=value` is different: it lands in the freeform `Fields`
map, NOT in `BotArgs`.) `iterion issue update` does not expose the
two flags yet — to change routing on an EXISTING card use one of:

- the REST API (`PATCH /api/v1/native/issues/{id}` with
  `{ "bot": "feature_dev", "bot_args": { "feature_prompt": "…" } }`),
- the board MCP / claw tools (`create_issue` and `set_bot` both accept
  `bot` / `bot_args`, so a bot with the `board.create` / `board.assign`
  capability can pin routing at create time),
- the studio issue modal,
- or rely on the dispatcher-side `assignee_workflows:` /
  `assignee_dispatch:` mappings keyed on `--assignee` (see
  [docs/dispatcher.md](dispatcher.md)).

## REST surface

When iterion runs an HTTP server (`iterion studio` or `iterion
dispatch`'s embedded HTTP), the native tracker is exposed under
`/api/v1/native/`. Auth follows the surrounding server: the studio's
local mode is unauthenticated; cloud mode gates the routes through the
same JWT middleware as `/api/runs/*`.

| Endpoint                                     | Method | Body                                |
|----------------------------------------------|--------|-------------------------------------|
| `/api/v1/native/issues`                      | GET    | (query: `state`, `label`, `assignee`)|
| `/api/v1/native/issues`                      | POST   | `{title, body?, state?, labels?, priority?, assignee?, blockers?, fields?, bot?, bot_args?}` |
| `/api/v1/native/issues/{id}`                 | GET    | —                                   |
| `/api/v1/native/issues/{id}`                 | PATCH  | partial `{title?, body?, labels?, priority?, assignee?, blockers?, fields?, bot?, bot_args?}` |
| `/api/v1/native/issues/{id}`                 | DELETE | —                                   |
| `/api/v1/native/issues/{id}/transition`      | POST   | `{to: <state>}`                     |
| `/api/v1/native/board`                       | GET    | —                                   |
| `/api/v1/native/board`                       | PUT    | full `Board`                        |

`{id}` accepts the same prefix resolution as the CLI.

`bot` (string) and `bot_args` (`map[string,string]`) are
dedicated typed columns on the native `Issue` record; they are not
part of the freeform `fields` map. `bot_args` is merged on top of
the dispatcher's rendered `dispatch.vars` key-by-key at launch time,
with `bot_args` winning on shared keys. `bot` is resolved into the
dispatch request for custom runners/future routing, but the current
stock `EngineRunner` is precompiled for one workflow and does not use
the per-ticket `bot` field to override workflow selection. Use
`assignee_workflows:` in the dispatcher config for current stock
workflow routing. See [docs/dispatcher.md §Per-ticket bot + args
fields](dispatcher.md) for the current handoff.

The SPA's Board view (`/board` in the studio) consumes exactly these
endpoints — it's a thin React shell on top of the REST surface.

## Pipeline board (the second board)

The Studio also exposes a single global `/pipelines` board. This is a
different product surface — a **control center** for watching and unblocking
many pipelines at once — not a saved filter and not a replacement for `/board`:

- `/board` remains the editable, shared dispatcher backlog;
- `/pipelines` is one global projection of every **root** pipeline, across all
  bots;
- it has three fixed lanes — `Opened`, `In progress`, `Closed` (IDs `opened`,
  `in_progress`, `closed`; the IDs are the wire contract);
- each card is one root pipeline; its descendant runs are **folded into the
  root card** (aggregate node progress + a list of pending human reviews), not
  shown as separate cards;
- a tree blocked on `paused_waiting_human` shows a **Blocked — human review**
  tag on the card; the structured answer form lives in the card's details
  sidebar (click the card) and resumes the exact paused run — when several
  reviews are pending across the tree the sidebar steps through them one at a
  time.

Lane semantics — the board is **task-centric**:
- `Opened` = every not-yet-running ticket (pairs with `Closed`). A per-card
  **Ready** badge marks the ones cleared to leave Opened for In progress (a
  staged task, or a run already queued for a slot); tickets with open hard
  deps show **Blocked by N** / **Waiting on deps**; the rest show **Not
  ready**. The card's **Mark ready** / **Unmark ready** buttons flip that
  state; Mark ready with open blockers parks the ticket in `waiting_deps`
  (or 409 on boards without that state). A not-ready ticket also offers
  **Delete** (confirmed, and refused server-side while any run in the tree is
  active). The launch loop starts ready tickets **highest priority first**
  **and only when every hard blocker is `done`** — same `native.CanLaunch`
  rule as the dispatcher. A header control filters the lane by **All /
  Ready / Not ready**;
- `In progress` = running or awaiting a human review (progress bar + Blocked
  tag). A running card offers **Pause** (soft operator pause — the engine
  checkpoints at the next safe boundary; **Resume** appears while paused); a
  ticket-backed card offers **Reset** (cancels the run tree, then restages the
  ticket to Ready for a fresh start); a ticket-less card offers **Stop** (plain
  cancel → the run lands in Closed as failed);
- `Closed` = every finished pipeline, success or failure. A per-card
  **Success** / **Failed** badge distinguishes the outcome; a successful card's
  output shows in the details sidebar, a failed one shows the **error as the
  reason** and (ticket-backed) offers **Retry** (restages to Ready) + Edit. A
  header control filters the lane by **All / Success / Failed**.

**Hard dependencies (blockers).** Ticket-to-ticket DAG (roots only — not
sub-bot runs). Satisfied only when the blocker issue is in state `done`
(not merely terminal). Optional extra gate: set
`bot_args.require_blocker_labels` (e.g. `accepted`) so a done blocker still
blocks until it carries those labels (artefact acceptance without a second
state machine). Projection fields on each card: `blockers` (enriched
`{id,title,state,bot,satisfied,missing_labels?}`), `open_blocker_count`,
`launch_blocked_reason`, `blocking` (reverse index, computed on read).
When the last blocker reaches `done` (and labels if required), dependents
in `waiting_deps` auto-promote to `backlog` (or `ready` if
`bot_args.auto_ready` is truthy) and emit `issue_unblocked`. Full contract:
[ADR-076](adr/076-pipeline-hard-blockers-and-waiting-deps.md).

### Ticket contract (`bot_args`)

Iterion stays game-agnostic. Cross-bot multi-pipeline tickets share a small
**well-known key vocabulary** (constants in
`pkg/dispatcher/native/ticket_contract.go`). Bots own the immutable request
JSON on disk; the board only routes execution.

| Key | Role |
|-----|------|
| `input_path` | Path to the immutable request file (primary). Upsert key with `bot`. |
| `revision_id` / `request_hash` | Immutability / cache identity |
| `asset_id` / `feature_id` / `family_id` | Correlation / bulk filters |
| `pipeline_kind` | `mesh` \| `humanoid` \| `feature` \| custom — UI filter |
| `produces` / `consumes` | Serialized artefact / dependency lists (JSON strings) |
| `doc_refs` | Optional design refs |
| `auto_ready` | Truthy → auto Ready when unblocked (else backlog) |
| `require_blocker_labels` | Comma-separated labels required on every hard blocker |
| `spawned_from` | Planner ticket id that published this card (synced with `Issue.ParentID`) |
| `role` | Optional `planner` \| `producer` hint for UI grouping |

**Planner provenance.** Distinct from hard `blockers` (scheduling): a planner
ticket can spawn child tickets via `board.create` / `POST …/tasks`. When the
creating run is sourced from a ticket, `parent_id` / `spawned_from` is
auto-stamped. The `/pipelines` projection exposes `parent_issue_id`,
`children[]`, and `children_summary` so Inventory can group children under a
collapsible plan card.

The `/pipelines` drawer surfaces these under **Ticket contract** before the
generic inputs list.

**Upsert.** Planners re-run without flooding the board: `POST …/tasks` with
`upsert: true` and `bot_args.input_path` updates the existing card matching
`(bot, input_path)` (title / blockers / bot_args). Does **not** reset state
when the ticket is already `in_progress` / `done` / `awaiting_input`.

**Multi-engine access.** Board mutations do not require Claude MCP tools.
Canonical surfaces for every backend (Claude / Codex / Kimi / scripts):

1. **REST** — `/api/v1/pipeline-board/*` and `/api/v1/native/issues/*` (this page);
2. **CLI** — `iterion issue create|list|show|update|move … --blocker … --bot … --bot-arg key=value`;
3. **HTTP MCP** — `/api/v1/mcp/board` (ephemeral run token) for sandboxed agents.

Ticket movement is **button-driven** — there is no drag & drop. The studio's
launch loop starts ready tickets when a concurrency slot frees (no
`iterion dispatch` needed). Run cards (In progress / Closed without a
ticket) are positioned by run state and cannot be moved. Per-lane filters are
client-side and compose on top of the global search / bot / label /
`pipeline_kind` / `family_id` / “Waiting on deps” bar.

The aggregate read API is intentionally server-side, so the browser does not
perform an N+1 traversal over issues, checkpoints and child runs:

| Endpoint | Method | Purpose |
|----------|--------|---------|
| `/api/v1/pipeline-board` | GET | Global projection: 3 fixed lanes + one folded card per root pipeline (progress, pending reviews, deps, contract args, output, concurrency). Optional `?since=<duration\|RFC3339>` prunes CLOSED cards last changed before the cutoff (live pipelines are never pruned) so a long-lived store escapes the ≤500-card truncation banner; the prune is reported via `hidden_closed_count` / `hidden_closed_before` |
| `/api/v1/pipeline-board/tasks` | POST | Create a ticket; `bot` required; optional `blockers`, `upsert`; `{start:true}` → ready when deps OK else `waiting_deps` |
| `/api/v1/pipeline-board/tasks/{id}/ready` | POST | `{ready}` stages Ready when hard deps are done, else parks in `waiting_deps` (or 409); unstage → backlog |
| `/api/v1/pipeline-board/tasks/{id}` | PATCH | Edit a not-yet-run ticket (title, body, labels, priority, bot, bot_args, blockers) |
| `/api/v1/pipeline-board/tasks/{id}` | DELETE | Delete a ticket (issue only, never a run); 409 while any run in its tree is active |
| `/api/v1/pipeline-board/tasks/{id}/reset` | POST | Cancel every active run in the ticket's tree, then restage it to Ready |
| `/api/v1/pipeline-board/tasks/{id}/dependency-graph` | GET | Limited-depth hard-dep graph (also `GET /api/v1/native/issues/{id}/dependency-graph`) |
| `/api/v1/pipeline-board/bulk/ready` | POST | `{ids?\|family_id?\|pipeline_kind?}` stage many tickets Ready (skip open blockers by default) |
| `/api/v1/pipeline-board/bulk/recompute-deps` | POST | Re-promote `waiting_deps` tickets whose blockers are now satisfied |

**Local concurrency cap.** `iterion studio` caps concurrent **root** pipelines
at `--max-concurrent-pipelines` (default 3; also
`ITERION_MAX_CONCURRENT_PIPELINES`). Ready tickets and direct launches beyond
the cap wait until a slot frees; a paused-for-review pipeline frees its slot so
reviews never starve the queue. `0` disables the cap. Local (in-process) only;
cloud admission stays on the NATS queue + org/team gates. The launch loop
stands down while an operator-started dispatcher is running (it would otherwise
race the same tickets).

Task ingestion reuses native issues in this slice. A task appears before launch
only when its `Issue.Bot` names a bot; once a run exists, manual/API/scheduled
runs associated with that bot also appear even if no native issue references
them. See [ADR-074](adr/074-dedicated-pipeline-board-projection.md) for the
boundary, trade-offs and follow-ups.

## Use cases beyond the dispatcher

Even without `iterion dispatch` running, the native tracker is useful
as a local kanban for:

- **Pre-flight backlog grooming.** Curate issues before flipping the
  switch on the dispatcher.
- **Per-project task lists.** The store lives under the same
  `<store-dir>` as your runs, so issues travel with the project.
- **Lightweight personal queue.** Replace a sticky-note `TODO.md`
  with something that survives reflows, accepts custom fields, and
  speaks JSON.

## Feeding the first column (the canonical ingest contract)

The board's leftmost column is the single, canonical entry point for new
work — whatever puts a card there (CI, an API caller, another pipeline, or
an operator clicking "add") uses the **same** contract:

- **`POST /api/v1/native/issues` with no `state`.** `Store.Create` defaults
  an empty `state` to the board's first column
  ([store.go](../pkg/dispatcher/native/store.go) — `in.State = States[0].Name`),
  so an ingester never needs to know the column's name. The body may carry
  `bot` / `bot_args` to pin the pipeline that will run the card (parity across
  REST, the MCP `create_issue` boardop, and claw's `mcp.iterion_board.create`).
  Authenticate a machine caller with a `iap_` PAT (see *Programmatic access*).
- **CI / external API (6a).** A CI job POSTs a finding straight onto the
  first column — no dispatcher required.
- **A pipeline feeding the board (6b).** A run-completion `run.finished`
  trigger event chains a downstream launch (`serviceLauncher` /
  `NativeBoardEffect`) that can create the next card.
- **Forge import (6a, self-hosted).** `syncForgeIssuesToBoard`
  ([board_forge.go](../pkg/server/board_forge.go)) is the store-agnostic core
  that mirrors a repo's forge issues into a native board — one-way, forge is
  the source of truth, cards land in the first column on create and refresh in
  place on update (see ADR-071). The self-hosted entry point is
  **`iterion issue import`**, which builds the forge client for you and drives
  that same core against a local board:

  ```sh
  # github.com (base URL defaults to the github.com API):
  GH_TOKEN=ghp_… iterion issue import \
    --forge github --repo owner/name --token-env GH_TOKEN

  # a self-hosted forgejo/gitlab (base URL is required):
  FORGE_TOKEN=… iterion issue import \
    --forge forgejo --repo owner/name \
    --base-url https://forge.example.com --token-env FORGE_TOKEN \
    --since 2026-07-01T00:00:00Z   # optional; empty = full re-sync
  ```

  The token is read **only** from the named env var (`--token-env`), never a
  flag value. Pull requests are skipped; open issues land in the first column,
  closed ones in the terminal column. The import is **idempotent** — re-running
  upserts existing cards (keyed by a deterministic `native:<uuid>` derived from
  `provider:repo#number`) instead of duplicating them, so it doubles as an
  incremental `--since` sync. The command prints `created` / `updated` counts
  (`--json` for machine output). The exported Go wrapper is
  `server.ImportForgeIssues`, which keeps the forge-client provider switch in
  one place.

## Programmatic access

The Go package is exported:

```go
import "github.com/SocialGouv/iterion/pkg/dispatcher/native"

s, err := native.NewStore(storeDir + "/dispatcher")
if err != nil { return err }

iss, err := s.Create(native.Issue{Title: "do a thing", State: "ready"})
list, err := s.List(native.ListFilter{States: []string{"ready"}})
_, err = s.SetState(iss.ID, "in_progress")
err = s.Claim(iss.ID, "worker-1")
err = s.Release(iss.ID, "worker-1")
```

To plug it into the dispatcher's `Tracker` interface:

```go
adapter := native.NewAdapter(s) // satisfies tracker.Tracker
```

## Limitations (v1)

- **No bi-directional sync with GitHub / Forgejo.** A single
  dispatcher instance picks one tracker. Mirroring is on the v2
  roadmap.
- **No persistent retry queue.** Restart loses in-flight backoff
  timers; the next tick re-discovers candidates via the tracker.
- **Migration on board.json changes is manual.** Renaming a state
  leaves existing issues in the old name, surfaced as "Unmapped" in
  the SPA. v2 will support an explicit migration step.
- **Field validation is closed-world.** Add a new `Field` to the
  board before writing any issue that references it.
