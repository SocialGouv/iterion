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
SyncEvery      the reconciliation interval (0 = off, default 2m — §10)
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
`native.Issue.StateAt`.

`Issue.StateAt` is stamped **by the store** at every state write, on both twins
(the FS store derives it in `writeIssueLocked` from the state differing from
the indexed record; the Mongo store in `stateSetAt` and in the state-naming
`replace`). It has to be the store's, not this sync's: a card moves from the
studio, the dispatcher, a board MCP tool and the trigger spine, and a stamp
only this package wrote would have under-dated every one of them and lost them
all. It is not `UpdatedAt` either — that bumps on any edit, so a retitle would
win a status conflict. A card whose last transition predates the stamp falls
back to `ExternalRef.Project.StateAt` (when iterion last wrote its state for
this board), which is what the rule read before.

1. **Value already equal ⇒ nothing happens.** This is the echo suppressor, and
   it is checked first, in both directions: a Status the reflect just wrote
   reads back as "already equal" at the next import, so a write can never
   ping-pong.
2. **Only one side moved ⇒ no conflict**, and no timestamp is consulted. The
   native side is unmoved while the card still sits in the state the RECORDED
   status maps to — that mapped state is iterion's own last write, so a card
   still there has not moved. The oracle is that fact, not a timestamp
   comparison: `Issue.StateAt` is bumped by any move, including a move away
   and back, so "the card's transition is newer than the board's" does not
   mean the card is anywhere other than where iterion last put it. A one-sided
   board move is therefore a plain apply and a one-sided native move a plain
   reflect. A recorded status the mapping does not cover leaves the question
   undecidable (iterion never derived a state from it) and is treated as
   moved — a phantom conflict costs one `Warn`, the reverse silently overwrites
   somebody's decision.
3. Both sides moved since the last sync ⇒ the **newer** timestamp wins.
4. Timestamps equal ⇒ **GitHub wins**, because it is the roadmap authority a
   human is looking at.
5. Every applied conflict resolution is logged at `Warn` with both timestamps,
   both values and the winner — a silent overwrite of somebody's decision is
   the one outcome that must never be invisible.

Rule 2 is what keeps `Conflicts` meaning what §9.2 says it means. Without it a
human's drag — the ordinary gesture on a project board — counted as "both sides
moved", and whenever the card's transition happened to be the newer of the two,
the phantom conflict resolved in the native side's favour and the reflect pushed
the card's OLD column back over the drag.

The native write is a **CAS** (`SetStateFrom(id, seen, want)`), so an operator
who moved the card between our read and our write wins over the stale fact we
were carrying, exactly as the issue import already behaves.

### 10. The reflect is the second direction of ONE reconciliation pass

Both directions run in the same pass, on the same board read, elected per
tenant. For each item the pass asks one question — **does the board still say
what iterion last recorded?** — and that single comparison is simultaneously
the who-moved oracle and the echo suppressor:

- **the board's status differs from the recorded one** ⇒ the board moved ⇒ the
  import arm applies §9's conflict rule. When the board wins, the card follows
  it and nothing is pushed. When **iterion wins** (its state change is newer),
  the pass pushes instead — that is the case the conflict rule exists for, and
  the recorded status is deliberately left stale until the push overwrites it
  with what was actually written;
- **the board matches the record, but the card's column maps to a different
  status** ⇒ nobody but iterion put that status there, so the divergence can
  only be a native move ⇒ write the `Status` field;
- **first sight** (nothing recorded) ⇒ import only: the board is the authority
  on the join, and pushing would overwrite a column nobody has reconciled yet;
- **unmapped native state** ⇒ inert (§2).

A native-wins conflict is the one case that does not advance the recorded
status inline: the reflect writes it with what it actually pushed. When the
reflect pushes NOTHING — the two sides already landed on the same column, the
native state is unmapped, the bound board has no column for it, the pass is
read-only — the import records what it OBSERVED instead, so a divergence
nothing can resolve is derived once rather than warned and counted on every
tick. A *failed* write is deliberately excluded: the stale record is what makes
the next pass retry it.

Because the recorded status advances on every write, a pass with nothing moving
writes nothing — the property that keeps the loop from re-pushing forever and
stamping a fresh `updatedAt` that would then win every conflict against the
operator.

**Owner and election.** `BoardSyncWorker` ticks every 30s, takes each binding
whose own `sync_every` is due, and **CAS-advances that binding's watermark
while taking a lease on it** (`sync_lease_until`, TTL
`forge.BoardSyncLeaseTTL` = 5 min); only the winner runs the pass. No
in-process global is the authority and N replicas are correct.

The lease is the half the watermark cannot give. A pass slower than the
binding's interval (floor 1m) makes the board due again *while it is still
running*, and the next tick presents exactly the watermark that pass wrote — so
the CAS matches, and two replicas reconcile the same board at once, issuing
duplicate `SetSingleSelect` calls and duplicate `External` writes on the same
cards. With the lease, the second one loses without advancing the watermark.

The release is a **CAS on the pass's own owner token** (`sync_lease_owner`,
fresh per pass). Without it, a pass that overran the TTL would clear the lease
of the successor that legitimately took the board, re-admitting exactly the
concurrent pass the lease refuses; with it, the late release is declined and
reported (`ErrBoardSyncLeaseLost` → one Warn naming the overrun), which is the
only moment an overrun is knowable at all.

The TTL is a backstop, not the normal release: `runPass` hands the board back
when it ends, whatever the outcome, so the TTL only ever fires for a replica
that **died** mid-pass — bounding that death to one TTL of staleness rather
than a board nobody may claim again. Deliberately not heartbeated (unlike
ADR-096's per-card claim lease): a reconciliation net is not a run, and a lease
it must refresh is machinery a five-minute ceiling buys nothing over. Each pass logs exactly one line: an `Info` with the
counters (`items`, `moved`, `reflected`, `labelled`, `conflicts`,
`refused_terminal`, `reflect_failed`, `skipped_no_card`) and its duration, or a
`Warn` naming the failure — which never blocks the next tick, since a
persistently failing board must not pin the sweep. One tenant's revoked token
skips that tenant, not the sweep.

**Cost and cadence.** `sync_every` defaults to **2 minutes** (floor 1 minute,
`0` = off, refused below the floor rather than clamped). Ten minutes was
rejected as the default: a roadmap lagging a bot by ten minutes reads as broken
to the human watching it. The price is one project read per bound team per
interval — GitHub prices a Projects v2 page at a handful of points against a
5000/hour budget, so a few-hundred-item board costs well under 1% of it.

Locally the same pass is the operator's to run: `iterion issue import
--project` by hand, or wired into `iterion schedule`.

*Rejected: a third arm of `trigger.Evaluator.applyEffect`* — the shape this ADR
first proposed, before the code was read. Three facts killed it:

1. `EffectRow` carries **no effect-kind**, and `MaterializeEffects` only
   creates rows for *matched subscriptions*. A projection reaches the cloud
   outbox only by BEING a subscription, or by an expand/contract migration of a
   durable schema.
2. Being a subscription means a new `bundle.ExecutionMode` — a bot-manifest
   vocabulary — for a row with no bot, which `/api/v1/triggers` would list and
   an operator could delete, silently killing the reflect.
3. `matchingSubscriptions` declines machine-caused events **in the shared
   prelude, before the mode switch**. A projection must NOT be declined (a
   watchdog filing a card in `blocked` is exactly what the roadmap must show),
   so it would require editing the admission path that protects the fleet from
   mass launches — the riskiest line in that file.

The pass form needs none of that, and it *is* the reconciliation net rather
than a second mechanism beside one.

*Rejected: a standalone reflect worker subscribed to the bus.* In cloud it
would receive **nothing** (board events left the bus at ADR-094), so it would
need either a re-publish — reintroducing a lossy path that then owes its own
sweep — or a second poll-tail with a second per-tenant cursor.

**Named follow-up.** If sub-minute reflect latency is ever wanted, the clean
path is an **effect-kind on `EffectRow`** (expand/contract) with the projection
as a first-class effect — not a pseudo-subscription. The pass stays the net
underneath it either way.

Everything else follows the shipped doctrine:

- Every native write is a CAS; every GitHub write is idempotent by construction
  (setting a single-select to the value it already has is a no-op we skip
  anyway, per rule 9.1).
- The pass IS the convergence, not a hope about delivery. It recomputes the
  truth from the board and the cards on every run, so a missed transition costs
  a delay of at most one interval, never a permanent divergence. That is why it
  has a named owner (§10) rather than a sentence in a doc.
- **A machine-caused move still reaches the roadmap.** The trigger spine
  declines `tracker.IsMachineReason` events (watchdog, state/field rename) for
  *launches*, because they must not spend budget. The pass form needs no
  exemption for that gate: it reads the card's CURRENT column, so a watchdog
  filing a card in `blocked` is reflected like any other move — which is
  precisely what the roadmap must show.

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
