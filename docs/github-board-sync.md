# GitHub project board ↔ native board sync

Making a GitHub **Projects v2** board (the roadmap humans read) and iterion's
**native board** (the surface bots dispatch from) the same tickets.

Decision record: [ADR-097](adr/097-github-projects-v2-board-sync.md).
Methodology this serves: [AGENTS.md](../AGENTS.md).

---

## What syncs, and which way

| | direction | who writes it |
|---|---|---|
| Title, body, labels, assignees, open/closed | GitHub issue → card | `iterion issue import` |
| **`Status`** | **both ways** | the project pass |
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
   named**, so you know which issue imports to run.
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
minutes; see [Reconciliation](#reconciliation)). Cards still have to exist:
enable issue sync on each repository integration, or run the import above once
per repo.

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
  labelled=3 conflicts=0 refused_terminal=0 reflect_failed=0 \
  skipped_no_card=4 skipped=11 took=812ms
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

---

## Troubleshooting

**`skipped_no_card` is large / a card never appears.**
The project pass hydrates, it does not create. Read the repositories it names
(CLI output, `missing_repos` in the API response) and run the issue import for
each one.

**A card moved on GitHub but not in iterion.**
Check the column is in the map (`iterion remote board show` — a `!` marks a
mapped column the board lacks). Then check it is not a terminal card:
`refused_terminal > 0` means automation declined to resurrect a closed card,
which is by design — reopen it in iterion.

**A card moved in iterion but not on GitHub.**
Three causes, in order of likelihood: the state is unmapped (`review`,
`waiting_deps`, `awaiting_input`, `backlog` are inert); the binding has
`sync_every: 0`; or the credential lacks the write grant —
`reflect_failed > 0` with a `403 Resource not accessible by integration` in the
log means the App's *Projects: Read and write* is not approved.

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
