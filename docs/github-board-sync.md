# GitHub project board ↔ native board sync

Making a GitHub **Projects v2** board (the roadmap humans read) and iterion's
**native board** (the surface bots dispatch from) the same tickets.

Decision record: [ADR-097](adr/097-github-projects-v2-board-sync.md).
Methodology this serves: [AGENTS.md](../AGENTS.md).

---

## What syncs, and which way

| | direction | who writes it |
|---|---|---|
| Title, body, labels, assignees, open/closed | GitHub issue → card | the **issue sync**: `iterion remote forge integrations sync <id>` on cloud, `iterion issue import` on a local store |
| **`Status`** | **both ways** | the project pass |

| Title, body, labels, assignees, open/closed | GitHub issue → card | `iterion issue import` |
| **`Status`** | **both ways** | the project pass, plus the per-move [projection effect](#how-fast-a-native-move-reaches-the-board) on the native → board side |
| `Area` / `Mode` / `Priority` | board → card labels | the project pass |
| everything else on the board | — | nothing |

The default `Status` map — replaceable, see [Boards with different
columns](#boards-with-different-columns):

| board column | native state |
|---|---|
| `Inbox` | `inbox` |
| `Planned` | `ready` |
| `In progress` | `in_progress` |
| `Blocked` | `blocked` |
| `Done` | `done` |

`Area: cloud/ops` becomes the card label `area:cloud-ops`; `Priority: P1`
becomes `prio:p1`. Those labels are usable straight away as trigger predicates
(a board invocation's `all_labels`, the dispatcher's include/exclude lists).

**Three things it deliberately does not do**, each of which will look like a
bug until you know it is a decision:

1. **It never creates a card.** A project item has no body, no labels and no
   author — and creating from one would bypass the author-trust gate that runs
   at issue ingest. Items with no card are counted and their **repositories are
   named**, so you know which issue syncs to run — the cloud one on a cloud
   instance, see [Troubleshooting](#troubleshooting).
2. **It never reopens a closed card.** Leaving a terminal column (`done`,
   `blocked`) is a *reopen* — an operator gesture with a dependents check and
   an audit trail. Dragging a card out of *Done* on GitHub is reported
   (`refused_terminal`) and the two boards stay divergent until a human reopens
   the card in iterion.
3. **Unmapped states are inert.** A card in `review` or `waiting_deps` leaves
   the board showing the last true thing it was told, rather than being
   collapsed onto *In progress* — which the next pass would read back and undo.

---

## Permissions

| credential | what it needs |
|---|---|
| **GitHub App** | organization permission **Projects: Read and write** (`organization_projects`). It is **org-level**, so an existing installation does not acquire it silently — an org owner must approve the new grant. Request it at App creation: tick *"Allow iterion to sync this org's project boards"* in the connect wizard (`allow_project_board` on `POST /api/teams/{id}/forge/oauth-apps/github-manifest`). At run time board calls ride a dedicated token, cached separately from and never served to the runtime one, so the token bots push with stays minimal. |
| **Fine-grained PAT** | organization permission **Projects: Read and write**. |
| **Classic PAT** | the `project` scope (`read:project` alone gives a read-only binding). |

A **GitHub App** installation without the grant fails at **bind time** naming
`organization_projects`, not hours later on the first status write: the bind
reads the installation's approved permissions before it reads the board.

A **PAT** cannot be probed that way — its scopes are not readable from the API
— so a PAT missing the grant surfaces as `project not found` on the board read.
That is GitHub's own answer for a project the token cannot see; if the board
address is right, the scope is what is missing.

---

## Setup

### Local / self-hosted — one repo, by hand

```bash
GH_TOKEN=… iterion issue import \
  --forge github --repo SocialGouv/iterion \
  --project SocialGouv/203 --token-env GH_TOKEN
```

Two passes in one command: the repo's issues become cards, then the board
hydrates them. Idempotent — re-run it as often as you like.

Repeat the command **per repository** the board spans, then re-run any one of
them to hydrate the rest. To keep it current, wire it into
[`iterion schedule`](scheduling.md):

```bash
iterion schedule add board-sync --cron "*/5 * * * *" \
  --bot … --workdir "$PWD"     # or a plain crontab line on the command above
```

### Cloud — bind the team once

```bash
iterion remote forge connections            # find the connection id
iterion remote board bind --project SocialGouv/203 --connection conn_123
iterion remote board show                   # the effective map + coverage
```

From then on the server reconciles the board on its own (default every 2
minutes; see [Reconciliation](#reconciliation)). Cards still have to exist, and
`iterion issue import` cannot make them — it writes to a LOCAL store, not the
instance's. Turn on issue sync per repository integration and let the
5-minute worker fill the board, or force one pass now:

```bash
iterion remote forge repo-bots               # the integrations, with their ids
iterion remote forge integrations update <integration-id> \
  --data '{"sync_issues_enabled":true}'      # the 5-minute worker takes over
iterion remote forge integrations sync <integration-id>   # one pass, right now
```

`iterion remote board unbind` stops it.

---

## Boards with different columns

The five shipped names are a **default, not a fence**:

```bash
iterion remote board bind --project acme/7 --connection conn_123 \
  --status-map "Todo=ready,Doing=in_progress,Shipped=done"
```

Rules the map must satisfy:

- **Injective.** Two columns mapping to one state is refused, naming the
  collision — the reverse direction would be ambiguous, so the reflect would
  pick one name and the next pass would read the other back and undo it.
- **At least one column must exist on the board.** A map matching nothing is a
  typo, not a partial board, and is refused with the board's actual columns
  listed.
- **Partial coverage is fine.** Columns in the map that the board lacks are
  reported (`missing_statuses`, and a `!` in `board show`); the covered half
  works.

Column names match **case-insensitively, trimmed** — *In Progress* and
*In progress* are the same column.

The same override exists for the dispatcher
(`tracker.github.project.status_map`, see
[dispatcher.md](dispatcher.md#board-mode--states-from-a-projects-v2-board-adr-097)).

---

## Editing a bound board's columns

Binding caches two things per column: the **option id** (what every write uses)
and the **name** (what both directions compare). Editing a column on GitHub
invalidates one or the other, so every reconciliation pass re-resolves both
against the board's live schema — at no extra API cost, since the pass already
reads it. What that repairs, and what it cannot:

| you did on GitHub | what survives | what the pass does |
|---|---|---|
| **renamed** a column | its id | adopts the new name; both directions keep working, nothing is rewritten |
| **deleted and re-added** a column under the same name | its name | adopts the new id |
| **added** a column the map named and the board lacked | — | adopts it and drops it from `missing_statuses` |
| **deleted** a column for good | nothing | marks the binding **degraded**, naming the column |

A degraded binding is a **partial** outage, not a stop: every column it can
still resolve keeps syncing, and only the cards whose state has no column are
left alone (counted as `reflect_no_column` in the pass line). The reason is on
the binding — `iterion remote board show`, or
`GET /api/teams/{id}/board-binding` — and is logged **once**, when it starts,
not on every pass.

It stands for exactly as long as the column is missing: each pass re-asks the
question, so nothing else happening on the board can clear it. It clears by
itself the moment that column exists again. Re-binding the board (`iterion
remote board bind …`) also clears it, and is the way out when the column is
gone for good: bind with a `--status-map` matching what the board actually
carries.

Partial coverage is **not** a degradation. A column your map names and the
board never had is reported as `missing_statuses` (a `!` in `board show`) and
its cards count `reflect_no_column`, but the binding reads healthy — binding a
three-column board with the five-column default map is a choice, not a break.
The bind records that choice (`unresolved_at_bind`), which is how a later pass
tells "never there" from "broke". **That exemption discharges the moment the
column works**: if you later add the column, the pass adopts it and it starts
taking cards — deleting it after that is a break like any other, and the
binding degrades. Only something that worked can break.

A binding created before `unresolved_at_bind` existed has it reconstructed on
its first pass **from its own cached option ids** — a mapped column the binding
holds no id for is one the bind never resolved — and not from what the board
happens to carry that day, which would fold a column that broke in the upgrade
window into the accepted set. One that is already degraded keeps its
degradation through the upgrade rather than being cleared by the gap in its own
record.

One population that upgrade cannot recover: a binding the previous release
degraded, whose flag an *unrelated* repair then cleared before the upgrade. It
arrives reading healthy with no cached id for the missing column — from the
outside, indistinguishable from partial coverage accepted at bind — so it
reconstructs as exempt and keeps reading healthy. The visible signal is
`reflect_no_column` climbing on a binding with no `degraded_reason`; the
recovery is a re-bind, which re-reads the board and records the accepted set
afresh. For the same reason, a binding degraded by the previous release with
*both* a broken column and a genuinely-accepted one will name both after the
upgrade: the record cannot tell them apart, and over-reporting is the side to
fail on. A re-bind is the reset there too.

---

## How fast a native move reaches the board

Two paths push a native card's column onto the bound board, and they run the
**same reflect** — the fast one just gets there first.

| path | latency | what drives it |
|---|---|---|
| **projection effect** | seconds | the card move itself, through the durable effect outbox |
| **reconciliation pass** | up to `sync_every` (default 2m) | the periodic board read |

A `card.moved` on a bound team writes a `projection` row into the same outbox
the board triggers use, and a worker executes it on the next drain — so the
roadmap follows a bot within seconds instead of trailing it by up to two
minutes. The row inherits the outbox's guarantees for free: a leased claim (two
replicas cannot both push), bounded retries with backoff, and a **visible
dead-letter** carrying the forge's refusal when the write keeps failing.

Nothing about it is operator-configurable, and nothing needs to be: the row
exists iff the team has a binding, and it is not a subscription — it carries no
bot, appears in no `/api/v1/triggers` listing, and cannot be deleted.

Two cases where the fast path deliberately does nothing and lets the pass
settle it:

- **The board had already moved.** The fast path issues no board read, so it
  cannot arbitrate "who moved last". It infers the board's state from the
  column the card *left*: when that column no longer matches what iterion
  recorded, somebody dragged the card on GitHub since the last pass, and
  pushing would silently overwrite them. It defers; the pass reads both
  timestamps and applies the [conflict rule](#conflicts).
- **The card is not joined to the board yet**, or was last synced against a
  different one. The import owns the join (it hydrates cards, it never creates
  them from items), so the reflect waits for it.

## What is projected — people and run verdicts, never the machinery

The roadmap follows two kinds of native move and ignores a third:

| the card's column was written by | reflected? |
|---|---|
| a **person** (studio, CLI, a bot's `board.move` tool) | yes |
| a **run's verdict** — the dispatcher filing `in_progress` at launch, `done` after a finished run, `blocked` after a failed one, `awaiting_input` on a pause | yes |
| **iterion on its own authority** — the claim watchdog parking a card, a column rename/delete, the dispatcher giving back a card it could not launch | **no** |

The third kind is *machine provenance*: the store stamps it on the card
(`state_reason`, the same value the card's state event carries — `watchdog`,
`state_rename`, `unlaunchable`, …) at every transition, on both the filesystem
and the Mongo board, and both reflect paths read it off the card. Such a
column says nothing about the card's work, so pushing it would show on the
operator's board as if someone had decided it. The pass counts what it left
alone as `reflect_machine`; the fast path materializes no projection row for
a machine-caused move at all. The trigger spine applies the same
`tracker.IsMachineReason` set to *launches*, for the same reason.

The card and the board therefore stay legitimately divergent after a machine
park — a watchdog-filed `blocked` while GitHub still says *Planned* — until a
person or a run moves the card again. Reopening the card in iterion, or
dragging it on GitHub, both clear the divergence on the next pass.

> Setting `--sync-every 0` leaves the fast path running with **no net under
> it**: a move the outbox dead-letters, or one made while the binding was
> misconfigured, then stays diverged until something moves that card again.
> Keep the pass on unless you have a reason not to.

---

## Reconciliation

The board pass IS the convergence: it recomputes the truth from the board and
the cards every run, so a missed transition costs at most one interval, never a
permanent divergence.

- **Interval**: `--sync-every` on the binding. Default **2m**, floor **1m**,
  `0`/`off` disables it. Below the floor is *refused*, not clamped.
- **Election**: each replica CAS-advances the binding's watermark *and takes a
  5-minute lease on it*; only the winner runs the pass, so N replicas cost one
  pass, not N — including when a pass outlives the interval, where the
  watermark alone would let a second replica join it. The lease is handed back
  at pass end — by the pass that took it, never by an older one that overran
  (that release is declined and logged) — so it only ever expires for a
  replica that died mid-pass.
- **Cost**: one project read per bound team per interval. GitHub prices a
  Projects v2 page at a handful of points against a 5000/hour budget.
- **Logs**: one line per pass.

```
board sync: team=t_123 board=SocialGouv/203 items=214 moved=2 reflected=1 \
  labelled=3 conflicts=0 refused_terminal=0 reflect_failed=0 reflect_no_column=0 \
  reflect_machine=0 skipped_no_card=4 skipped_archived=3 skipped=11 took=812ms
```

A failed pass logs `Warn` and **does not block the next tick**; one team's
revoked token skips that team, not the sweep.

---

## Archived items

Archiving is how a board gets cleared. GitHub removes the item from every view
but **keeps its field values**, so an item archived in "Planned" reads as
Planned forever.

- The **sync pass skips it entirely**, counted as `skipped_archived`. Neither
  direction runs: importing would drive a card from a column nobody can see,
  reflecting would write into a row nobody can read.
- The **dispatcher never dispatches it**. An archived item in a candidate
  column would otherwise launch a bot, and spend LLM budget, on work the
  operator visibly removed.
- A run **already in flight is not cancelled** by archiving its card. The
  dispatcher's liveness read still reports an archived item's state, because
  omitting it means "the issue disappeared" and reaps the run — and tidying a
  board is not a kill switch. Move the card out of its column, or cancel the
  run, to stop it.

Un-archive the item to put it back under sync.

---

## Conflicts

When both sides moved since the last pass:

1. **Value already equal ⇒ nothing happens.** This is checked first and is what
   makes the loop terminate: a status iterion itself wrote reads back as equal.
2. **Only one side moved ⇒ no conflict.** The native side is unmoved while the
   card still sits in the column the recorded status maps to — iterion's own
   last write. A one-sided board move is a plain apply, a one-sided native move
   a plain reflect, and neither touches the counter.
3. Otherwise the **newer** state change wins — the card's own transition time
   (stamped by the board store at every move, wherever the move came from)
   against the board column's `updatedAt`.
4. A tie goes to **GitHub** — it is the roadmap a human is looking at.
5. Every resolution is logged at `Warn` with both timestamps, both values and
   the winner.

So `conflicts=N` counts people and bots actually fighting over the board. A
board whose humans and bots take turns reads `conflicts=0`.

The native write is a CAS, so an operator who moved the card between our read
and our write wins over the stale fact we were carrying.

---

## Dispatching from the board

The dispatcher can take its workflow state from the board instead of labels —
`tracker.github.project` in the config. Full reference:
[dispatcher.md](dispatcher.md#board-mode--states-from-a-projects-v2-board-adr-097).

**The default map makes every *Planned* ticket a `ready` card**, and `ready`
is the column the cloud board dispatcher dispatches from. What keeps a bound
roadmap from being launched wholesale is the **bot**: the dispatcher only
claims a `ready` card that names one (there is no default bot on cloud), so a
roadmap ticket the sync moved to `ready` is roadmap content until something —
a triage bot via `set_bot`, an operator, a board trigger — stamps a bot on
it. A card the dispatcher claimed but could not launch after all (its bot was
cleared or cannot be resolved in between) is given back to its column, never
parked `blocked`; see
[dispatcher.md](dispatcher.md#claim-selection-on-the-cloud-board--what-is-never-claimed).

---

## Troubleshooting

**`skipped_no_card` is large / a card never appears.**
The project pass hydrates, it does not create. Read the repositories it names
(CLI output, `missing_repos` in the API response) and run the **issue sync**
for each one. On a cloud instance that is the server-side pass, not
`iterion issue import` — the import writes to a local store the instance never
reads:

```bash
iterion remote forge repo-bots                            # integration ids
iterion remote forge integrations sync <integration-id>   # → {"synced":N,…}
```

Enable `sync_issues_enabled` on the integration (see [Cloud — bind the team
once](#cloud--bind-the-team-once)) and the 5-minute worker keeps it filled.

**The sync answers `does not implement forge.IssueClient`.**
The connection's credential shape resolves to a client that cannot read the
issue API. The message names the connection kind and the concrete client, which
is what to act on. GitHub App, PAT and OAuth connections all serve it; a
provider that genuinely does not is the remaining case, and re-connecting the
repository under a supported one is the fix.

**A card moved on GitHub but not in iterion.**
Check the column is in the map (`iterion remote board show` — a `!` marks a
mapped column the board lacks). Then check it is not a terminal card:
`refused_terminal > 0` means automation declined to resurrect a closed card,
which is by design — reopen it in iterion.

**A card moved in iterion but not on GitHub.**
Five causes, in order of likelihood: the state is unmapped (`review`,
`waiting_deps`, `awaiting_input`, `backlog` are inert); the move was
**iterion's own** — a watchdog park, a column rename, a card given back after
a failed launch — which the roadmap never follows (`reflect_machine > 0`; the
card's `state_reason` names it, see [What is
projected](#what-is-projected--people-and-run-verdicts-never-the-machinery));
the binding has `sync_every: 0`; the board has no column for that state —
`reflect_no_column > 0`, and `iterion remote board show` says which (a `!` on
a mapped column, or a `degraded` reason when a column was deleted after the
bind, see [Editing a bound board's columns](#editing-a-bound-boards-columns));
or the credential lacks the write grant — `reflect_failed > 0` with a `403
Resource not accessible by integration` in the log means the App's *Projects:
Read and write* is not approved.

**A card moved in iterion and reached GitHub, but only minutes later.**
The [projection effect](#how-fast-a-native-move-reaches-the-board) declined and
the pass did the work. The two reasons it declines are both deliberate: the
board had already moved (the pass arbitrates that with real timestamps), or the
card is not joined to this board yet. A third possibility is that the fast path
*failed*: look for the row parked in the effect outbox with the forge's refusal
on it — the same `403` / permission causes as `reflect_failed` above.

**A card stopped following, and the item is not on the board any more.**
It is archived. `skipped_archived > 0` in the pass line; un-archive it to put
it back under sync (see [Archived items](#archived-items)).

**Bind fails with "none of the mapped columns exist".**
The board's real column names are listed in the error. Either rename them on
GitHub or pass `--status-map`.

**Bind fails with "could not resolve to a ProjectV2".**
Wrong owner, wrong number, or the credential cannot see the project. Check the
number against the board's URL (`/orgs/<owner>/projects/<n>`), and
`--owner-kind user` for a user-owned board.

**Labels vanish after an issue import.**
They should not: `area:` / `mode:` / `prio:` are declared board-local, so the
issue import preserves them. If they do vanish, the prefix in use is not one of
the three — check `iterion remote board show` under *Label fields*.
