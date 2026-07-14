# Native kanban tracker

Iterion ships with a first-class issue tracker — no Linear, no GitHub
account required. It is the default backing store for
[`iterion dispatch`](dispatcher.md), but is fully usable on its own
through the `iterion issue` CLI or the studio's Board view.

The native tracker is a deliberate design choice: iterion's autonomous
loop should not require the operator to lock themselves into a
proprietary issue tracker. External adapters (`github`, `forgejo`) are
optional plug-ins, not the source of truth.

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
`issue_last_run_updated`, `board_updated`.

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

`awaiting_input` holds a dispatched card whose run paused for input
(a `human` node, or an operator soft-pause): the dispatcher parks the
card there — non-eligible, claim retained — and a per-tick sweep moves
it on once the run reaches a terminal status (see the "Paused runs"
section in [docs/dispatcher.md](dispatcher.md)). Boards persisted by an
older iterion are **schema-upgraded automatically** on store open
(filesystem) or on read (Mongo): missing `inbox` is prepended, missing
`awaiting_input` is inserted right after `in_progress`. Fully-custom
boards without an `in_progress` state are left untouched.

| Property            | Meaning                                                            |
|---------------------|--------------------------------------------------------------------|
| `eligible: true`    | Dispatcher will dispatch issues sitting in this state.              |
| `terminal: true`    | Dispatcher treats this state as a stop signal; blocker dependencies | 
|                     | resolve.                                                           |

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

## Pipeline boards (the second board)

The Studio also exposes `/pipelines` and `/pipelines/{bot}`. This is a
different product surface, not a saved filter and not a replacement for
`/board`:

- `/board` remains the editable, shared dispatcher backlog;
- one pipeline board is identified by one discovered bot (`bot:<bot-id>`);
- its columns are derived from the workflow graph and live run statuses;
- root runs and recursively-linked child runs are separate nested cards;
- a card paused on `paused_waiting_human` embeds the existing structured
  answer form and resumes the exact root or child run.

The aggregate read API is intentionally server-side, so the browser does not
perform an N+1 traversal over issues, checkpoints and child runs:

| Endpoint | Method | Purpose |
|----------|--------|---------|
| `/api/v1/pipeline-boards` | GET | List deterministic bot-bound board identities |
| `/api/v1/pipeline-boards/{bot}` | GET | Return columns plus the flat, depth-annotated task/run tree |
| `/api/v1/pipeline-boards/{bot}/tasks` | POST | Create a task pinned to the path bot; `{start:true}` admits it directly to the first eligible native state |

Runtime-derived columns cannot be drag-and-dropped: changing a card's visual
position without changing its run would make the projection false. The board
always includes `Todo`, `Running`, `Needs attention` and `Done`; declared human
interactions are named columns between them, with dynamic child interactions
and an `Other input` fallback for runtime-only pauses.

Task ingestion reuses native issues in this first slice. Consequently a task
appears before launch only when its `Issue.Bot` explicitly matches the board.
Once a run exists, manual/API runs genuinely associated with that bot also
appear even if no native issue references them. See
[ADR-073](adr/073-dedicated-pipeline-board-projection.md) for the boundary,
trade-offs and follow-ups.

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

- **No comments.** Events.jsonl is the audit trail; user-facing
  comments are a v2 ergonomic.
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
