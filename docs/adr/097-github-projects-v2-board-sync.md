# ADR-097 — GitHub Projects v2 ↔ native board sync

- Status: accepted (2026-09-05)
- Serves: epic [#613](https://github.com/SocialGouv/iterion/issues/613) — plug a
  cloud team onto the [Iterion project board](https://github.com/orgs/SocialGouv/projects/203)
- Relates to: [AGENTS.md](../../AGENTS.md) (the GitHub board is the roadmap
  truth, the native board is the bots' operational surface), ADR-046 (trigger
  spine), ADR-094 (durable effect outbox), ADR-096 (board claim lease),
  [docs/native-tracker.md](../native-tracker.md), [docs/repo-scope.md](../repo-scope.md)

## Context

AGENTS.md makes the GitHub Projects v2 board the truth for ongoing work, and
keeps the iterion **native board** as the bots' operational surface (auto-triage,
dispatch, claim/lease). Today the two are joined by exactly one thread:
`iterion issue import` mirrors a repo's **issues** onto native cards, one way,
idempotently (`pkg/server/board_forge.go:syncForgeIssuesToBoard`).

That thread carries none of the board itself. A Projects v2 board is not repo
data: its `Status`, `Area`, `Mode` and `Priority` fields live in a project
object, reachable **only through the GraphQL API**, and no seam in iterion
speaks GraphQL to a forge at all (`pkg/forge/github` is REST-only; the only
mention of GraphQL in the tree is a comment in `ci.go` explaining what REST
cannot do).

The consequences are concrete and daily:

- A human moves #613 to *In progress* on the board; the dispatcher, which
  reads native columns, never learns it.
- A bot moves a native card to `done`; the board still shows *Planned*, so the
  roadmap view lies until someone drags a card by hand.
- The import lands every open issue in the same column, because the issue's
  `open`/`closed` state is all it can see. *Inbox* vs *Planned* vs *Blocked* —
  the distinction the methodology is built on — is invisible to it.

So the engine cannot serve the very contract AGENTS.md asks agents to follow.

## Decision

Add a provider-neutral **project-board capability** to `pkg/forge`, implement it
for GitHub Projects v2 over a new GraphQL client, and join it to the native
board through **one two-way field (Status) and nothing else**.

### 1. Content flows ONE way: GitHub issue → native card

Title, body, labels, assignees, open/closed keep flowing forge → board, through
the existing `syncForgeIssuesToBoard`. Nothing in this ADR ever writes an
issue's content back to GitHub.

*Rejected: two-way content sync.* It needs per-field conflict resolution on
free text, and the loser of a conflict is a human's paragraph. The board's job
is to route work, not to be a second authoring surface; ADR-074 already refused
a mutable per-bot projection for the same reason. Push-to-forge for a card
authored on the native side stays what it is today — an explicit, operator-
triggered gesture (`POST /api/v1/native/issues/{id}/push`), not a sync.

### 2. Status is TWO-way, over an injective map the operator can replace

The **default** vocabulary, which is what board 203 and the shipped native
board already agree on:

| Projects v2 `Status` | native state |
|---|---|
| `Inbox` | `inbox` |
| `Planned` | `ready` |
| `In progress` | `in_progress` |
| `Blocked` | `blocked` |
| `Done` | `done` |

A native transition into a mapped state writes the item's `Status` field; a
`Status` change on GitHub moves the card.

**Those five names are a default, not a fence.** A board whose columns read
*Todo* / *Doing* / *Shipped* binds by naming them — `iterion remote board bind
… --status-map "Todo=ready,Doing=in_progress,Shipped=done"`, the same field on
the binding API, `tracker.github.project.status_map` in a dispatcher config.
The effective map is stored on the binding and rendered by `board show`, so
what a deployment actually runs is always readable. One builder
(`forge.StatusMappingFromMap`) serves every entry point, because three copies
of a validation rule is how the third one drifts.

The map must stay **injective in both directions**, and a bind that maps two
columns onto one state is refused, naming the collision: the reverse direction
would otherwise be ambiguous — the reflect would pick one name and the next
import would read the other back and undo the transition.

Native states outside the map (`backlog`, `waiting_deps`, `awaiting_input`,
`review`) are **inert**: the reflect logs the skip once and writes nothing.

*Rejected: collapsing the unmapped states onto their nearest neighbour*
(`review` → *In progress*). It makes the round trip lossy — the next import
would read *In progress* back and drag a card out of `review` into
`in_progress`, undoing a bot's own state machine. An honest no-op leaves the
GitHub board showing the last true thing it was told.

Status option names are matched **case-insensitively on the trimmed name**, so a
board that writes *In Progress* binds the same as one that writes *In progress*.
A board missing one of the mapped columns is bound anyway: the missing rows are
dropped and named in the bind result, rather than refusing the binding.

### 3. Area / Mode / Priority are read into labels, written back never

Each single-select field the board carries beyond `Status` is imported onto the
card as a namespaced label — `area:<value>`, `mode:<value>`, `prio:<value>`,
slugified (lowercased, spaces → `-`). They are **read-only**: the import writes
them, nothing else does, and no iterion path ever sets those fields on GitHub.

They are declared **board-local** (`boardLocalLabelPrefixes` in
`pkg/server/board_forge.go`, alongside `triage:` / `needs:` / `cmd:` /
`source:`). Without that, the next plain `iterion issue import` — which mirrors
the *repo's* labels verbatim and keeps only board-local namespaces — would
silently strip every project-derived label off every card.

*Rejected: modelling them as native custom `Fields`.* Native fields are typed
and board-scoped; a bot's label matcher (`all_labels` on a board invocation,
the dispatcher's include/exclude lists) reads labels, not fields. Labels make
`area:cloud/ops` immediately usable as a trigger predicate with zero new engine
surface — which is the whole point of importing them.

### 4. The binding lives at the TEAM level

One document, `forge.BoardBinding`, keyed on the team (the resource tenant,
ADR-048):

```
TeamID, Provider, Owner, OwnerKind (org|user), ProjectNumber,
ConnectionID   (the forge.Connection supplying the token)
StatusMapping  []{Status, State}      the EFFECTIVE map (§2), stored so what
                                      a deployment runs is always readable
SyncEvery      the reconciliation interval (0 = off, default 10m — §10)
ProjectID, ProjectTitle, StatusFieldID, StatusOptions map[state]optionID,
LabelFields []BoundField{FieldID, Name, Prefix}
BoundAt, UpdatedAt
```

*Rejected: per-repo binding* (on `forge.RepoIntegration`). A Projects v2 board
spans repos by design — board 203 tracks engine, bots, cloud/ops, studio and
docs work that lands in several repos — so a per-repo binding would either
duplicate the same project N times or force one repo to be "the" board owner.
The team is the tenant every store already keys on, and one team ↔ one roadmap
board matches how the methodology is actually used.

*Rejected: binding by project URL string.* `owner + number` is what both the
GraphQL query and the human URL are built from, and it survives a project
rename; the URL does not.

### 5. Field and option ids are DISCOVERED by name at bind time, then cached

`PVTSSF_lADOAh0HH84BiOg8zhhHUgk` is not a name anyone can type, is not stable
across projects, and would be a hardcoded constant tying the engine to one
board — exactly the coupling the ENGINE-stays-bot-agnostic rule forbids. So
`BindBoard` resolves the project by `owner/number`, reads its fields, matches
`Status` / `Area` / `Mode` / `Priority` **by name**, and stores the ids it
found on the binding.

The cache is a cache, never an authority: any sync may re-run discovery, and a
`Status` write that fails because an option id is gone re-discovers once and
retries. A field renamed or deleted on GitHub surfaces as an explicit error
naming the field, never as a silent skip.

### 6. The project import HYDRATES cards; it never creates them

A project item carries a title, a URL and its field values — no body, no
labels, no assignees, and **no author**. A card built from one would be
degraded, and worse, would enter the board without passing the author-trust
gate that runs at issue ingest and decides whether a card may spend LLM budget
at all. So the project pass joins onto cards the *issue* import created, and
counts the items it could not join (`skipped_no_card`) instead of inventing
them.

Consequence for the operator: run the issue import for each repo the board
spans, then the project pass. `iterion issue import --project` does both in one
command for one repo.

**The skip is actionable, not a number.** The result carries `missing_repos` —
the distinct `owner/repo` of the skipped items with a count each, most-missing
first — on the CLI output and the API response. "12 skipped" tells an operator
nothing they can act on; "8 in SocialGouv/iterion, 4 in SocialGouv/infra" *is*
the next two commands.

### 7. A terminal card is never reopened by the import

Leaving a `Terminal: true` native column is a **reopen** — an operator surface
op with a dependents check and an audit trail, and the native board's guard
(`ValidateStateExit`) refuses it to every automated writer, deliberately: the
silent resurrection of a closed card was the failure that guard exists for.

The import does not carve an exception. A board Status that would drag a card
out of `done`/`blocked` is **refused, counted (`refused_terminal`) and
logged**; the two boards stay legitimately divergent until a human reopens the
card. Making automation the one writer allowed to resurrect work would trade
that invariant for a convenience.

### 8. Idempotency: the card id IS the key

The existing deterministic card id stays the only key:
`forgeCardID(provider, repo, number)` = `native:` + UUIDv5 over
`"<provider>:<repo>#<number>"`. A project item whose content is
`SocialGouv/iterion#613` therefore addresses exactly the card the issue import
already created or will create — the two importers converge on the same row
with no shared bookkeeping.

Per-card sync state hangs off `native.ExternalRef.Project`:

```
Owner, Number      the bound project this card is synced with
ItemID             the provider's project-item node id (skips a lookup)
Status, StatusAt   the Status option NAME last synchronized, and the
                   provider's own timestamp for that value
StateAt            when the native state last changed, per iterion
```

*Rejected: a separate mapping collection.* Two rows to keep consistent, and a
card deletion that leaks a row. `ExternalRef` is already the card's external
identity, already round-trips through both the FS store and the Mongo twin, and
already survives the import's patch path.

### 9. Conflict rule: newer wins, GitHub wins ties, always logged

Both directions carry a timestamp of the **state change** (not of the record):
GitHub's `ProjectV2ItemFieldSingleSelectValue.updatedAt`, and iterion's
`ExternalRef.Project.StateAt`.

1. **Value already equal ⇒ nothing happens.** This is the echo suppressor, and
   it is checked first, in both directions: a Status the reflect just wrote
   reads back as "already equal" at the next import, so a write can never
   ping-pong.
2. Both sides moved since the last sync ⇒ the **newer** timestamp wins.
3. Timestamps equal ⇒ **GitHub wins**, because it is the roadmap authority a
   human is looking at.
4. Every applied conflict resolution is logged at `Warn` with both timestamps,
   both values and the winner — a silent overwrite of somebody's decision is
   the one outcome that must never be invisible.

The native write is a **CAS** (`SetStateFrom(id, seen, want)`), so an operator
who moved the card between our read and our write wins over the stale fact we
were carrying, exactly as the issue import already behaves.

### 10. The reflect is an EFFECT, not a new worker

A card transition reaches GitHub through the seam both delivery paths already
share: `trigger.Evaluator.applyEffect`. The binding materializes an ordinary
board-kind `trigger.Subscription` (matcher `card.moved`), and the projection is
a third arm next to *promote a card* and *launch a bot*.

That single choice buys both halves at once:

- **Cloud** — board events do not ride the bus at all since ADR-094; they are
  materialized into the `trigger_effects` outbox inside `drainTenant`, under
  the per-tenant CAS cursor that elects the materializing replica, and executed
  by `trigger.EffectWorker` with an atomic leased claim, bounded exponential
  retry and a dead-letter row. A projection inherits every one of those
  guarantees for free. No in-process global is the authority; N replicas are
  correct.
- **Local** — the FS board's `Subscribe` seam publishes onto `InProcBus`, whose
  subscriber is the same `Evaluator`, which calls the same `applyEffect`. One
  implementation, two buses, two stores.

*Rejected: a standalone reflect worker subscribed to the bus.* It reads as the
smaller change and is not: in cloud it would receive **nothing** (the board no
longer publishes), so it would need either a re-publish — reintroducing a lossy
path that then owes its own reconciliation sweep — or a second poll-tail with a
second per-tenant cursor. Strictly more machinery, and one more place for the
two modes to drift.

Everything else follows the shipped doctrine:

- Every native write is a CAS; every GitHub write is idempotent by construction
  (setting a single-select to the value it already has is a no-op we skip
  anyway, per rule 9.1).
- The periodic import is the **reconciliation net** for the reflect path. Local
  delivery is best-effort by design (`BoardSource` drops on a full buffer) and
  the cloud tail can step over a gap loudly, so convergence may not be assumed
  from event delivery: a dropped event costs a delay, never a divergence,
  because the next import recomputes the truth from both timestamps.

  **That net has a named owner**, because a reconciliation net nobody runs is a
  comment. In cloud, a **project sync worker** ticks each bound team on the
  binding's own `sync_every` (default 10m; `0` = off), **elected per tenant**
  with the same leased-claim shape as the board tail — never an in-process
  global — and logs one Info line per pass carrying the counters (`hydrated`,
  `conflicts`, `refused_terminal`, `skipped_no_card`), so a board drifting
  quietly is visible in the logs rather than in someone's confusion. Locally
  the net is the operator's: `iterion issue import --project` by hand, or wired
  into `iterion schedule` — documented as such, not implied.
- Machine-caused events (`tracker.IsMachineReason` — watchdog, state/field
  rename or delete) are declined for *launches* because they must not spend
  budget. A projection is not a launch: a watchdog that files a card in
  `blocked` is precisely the movement the roadmap must show, so the projection
  arm is exempted from that gate, explicitly and in one place.

### 11. Permissions, stated up front

- **GitHub App**: `organization_projects: write` — an **organization**-level
  permission, so an existing installation does *not* acquire it silently; the
  org owner must approve the new grant. It is added to the App manifest as an
  opt-in profile (`ProjectsInstallationPermissions`), never folded into
  `RuntimeInstallationPermissions`: a token that can rewrite an org's roadmap
  is a broader privilege than one that can push a branch, and the
  `security_read` precedent (a separate profile, a separate purpose) is the
  pattern to imitate.
- **PAT**: classic PATs need the `project` scope (`read:project` suffices for a
  read-only binding); fine-grained PATs need organization permission
  *Projects: Read and write*.
- A binding whose credential lacks the grant fails at **bind time** with the
  missing permission named — not hours later, at the first status write.

## Consequences

- `pkg/forge` grows one optional capability (`BoardClient`, type-asserted like
  `IssueClient` / `PermissionClient` / `RepoCreator`) and one team-scoped store
  (`BoardBindingStore`, memory + Mongo twins under one conformance suite). No
  existing interface changes.
- `pkg/forge/github` grows a GraphQL transport shared by the PAT client and the
  App installation client. A response carrying `errors[]` is an **error**,
  including when `data` is partially populated — GitHub answers `200` with a
  populated `data` and a `NOT_FOUND` error for a missing project, and treating
  that as a nil result is exactly the silent-fallback failure mode the repo
  forbids.
- The dispatcher's GitHub tracker gains a **board mode**: candidates are the
  items whose `Status` is *Planned*, and `UpdateState` writes the field. The
  label claim stays the lease (Projects v2 has nothing to fence with), so
  `ClaimLeaser` remains unimplemented for it, as today.
- Any provider that later grows a project board (GitLab boards, Forgejo
  projects) implements `BoardClient` and inherits the import, the reflect and
  the dispatcher mode with no engine change.
- The engine still knows no specific bot and no specific board: the binding is
  data, the field names are configuration, and the five Status names are the
  only vocabulary written down — in one map, next to the states it maps to.

## Alternatives rejected wholesale

**Mirror the project INTO the native board as a second board.** Two boards to
keep consistent, two claim domains, and the dispatcher would have to pick one.
The native board stays the single operational surface; the project is a view
onto it, joined on one field.

**Use the GitHub board directly as a `Tracker`, dropping native.** The claim
lease (ADR-096), the fencing epoch, the label consume-atomicity and the
board-events spine all live on the native store. A Projects v2 item has no CAS
primitive to rebuild them on, so this would trade every concurrency guarantee
for one less hop.

**Poll the project instead of reacting to native events.** Polling alone makes
a bot's state change visible on the roadmap only at the next tick, which is the
lag the epic exists to remove. The reflect is the fast path; the poll stays as
its reconciliation net, per the doctrine that a lossy path always needs one.

**Shell out to `gh project`.** It is what the current GitHub *tracker* does for
issues, and it is why that adapter cannot run in a cloud pod with a
per-connection App token: `gh` authenticates itself, from its own config. The
board capability rides `forge.Connection` credentials like every other outbound
write in `pkg/forge`.
