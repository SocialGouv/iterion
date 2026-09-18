# 🗂️ Runbook — the GitHub board's epics

<!-- The "Two axes" heading below carries no emoji on purpose: it is linked by
     anchor, and a leading emoji slugs differently on GitHub than in VitePress. -->

**Read it when** you add a ticket to the [board](https://github.com/orgs/SocialGouv/projects/203),
create or retire an epic, wonder why a card is in the wrong column, or need to
know which view answers your question.

The board is the roadmap/chantier view; the iterion **native board** (studio
`/board`) stays the bots' operational surface. The working contract —
statuses, session phases, the claim rule — is in [AGENTS.md](../AGENTS.md).
This page is the mechanics.

## 🧭 Which view answers what

Nine views, split by audience. **The epic views carry `label:epic`; the ticket
views carry `-label:epic`** — an epic sitting in the Kanban's *In progress*
column is noise, and there are 26 of them.

*The strategy — the epics themselves:*

| view | question it answers |
|---|---|
| 🚦 **Chantier state** | *Where do we stand, and what needs attention first?* Board, columns = `State of play`. The one-screen read. |
| 📚 **Epic map** | *What just landed, and what is next, on each chantier?* Table: state of play, `Latest`, `Next`, priority, progress. |

*The work — tickets:*

| view | question it answers |
|---|---|
| 🎯 **Work by epic** | *What is live on each objective?* Board, columns = `Epic`, open work only. |
| 🗂 **Kanban** | the day-to-day board, by status |
| 🚀 **In flight** | who is on what — In progress + Blocked |
| 📥 **Triage** | the Inbox to empty at session start (Phase A) |
| 🧩 **Unassigned** | *Did anything escape?* `no:epic`. **It must read 0.** |

*Reference:* 📋 **All items** (the raw table — give it a `filterQuery` to slice
by epic) and 🗺 **Roadmap**.

## Two axes — `Status` and `State of play`

They answer different questions, and using one for the other is what made the
board unreadable the first time round.

- **`Status`** — *is someone on it?* Its vocabulary is a **claim lifecycle**,
  written for tickets: Inbox → Planned → In progress → Blocked → Done, where
  `Planned` means "triaged, ready to pick up" and `In progress` means "claimed
  by a session".
- **`State of play`** — *how far is it?* 🔴 blocked · 🟠 gap identified ·
  🟡 running, watch it · 🟢 humming · ⚪ design stage. It grades **evidence,
  not ambition**, and it is the axis that actually separates one chantier from
  another.

**For an epic, `Status` is almost always `In progress`.** A chantier with an
état d'avancement is not "ready to pick up", whatever its ticket flow looks
like — Credentials had 27 delivered tickets while sitting in `Planned`, which
is simply false. An epic is `Planned` only if genuinely nothing has been done,
and `Blocked` only when a **named decision** is pending (say whose it is in
the body).

So do not read `Status` to prioritise epics: it is near-uniform by design.
Read **`State of play`**, which is why 🚦 Chantier state exists. `Status` on
an epic still earns its keep for one thing: it keeps the epic out of the
day-to-day Triage and Kanban views.

For a **ticket**, `Status` keeps its ordinary claim meaning and
`State of play` is left empty.

**`Priority` is an epic-level field.** All 26 epics carry one; most open
tickets do not, and that is deliberate rather than neglected. Ranking 77 open
tickets against each other is fake precision — what actually decides the next
move is which *chantier* matters (its `Priority`) and what its `Next` says.
Give a ticket a priority when something makes it urgent on its own; leave it
empty otherwise. An empty column here is not a backlog to fill.

## 🔭 `Latest` and `Next` — the two lines that make it readable

Two **text** fields, on epics only, are what turn 📚 Epic map from an inventory
into a briefing:

- **`Latest`** — the most recent *proven* advance, dated: `2026-09-17 · #1164 —
  lot 3: import and the multi-file compilation unit`.
- **`Next`** — the very next step, one line, naming its ticket when there is
  one. When the next step is a **decision**, say so and say whose:
  `DECISION, operator's: …`.

They are the freshest thing on the board and therefore the first to rot. Two
habits keep them true, and they cost seconds:

- **Closing a ticket under an epic → update that epic's `Latest`.**
- **Picking up the next one → update its `Next`.**

A `Latest` older than the last release is itself a signal worth reading: either
the chantier is genuinely quiet, or nobody is maintaining its line. Don't
guess between the two — go and look.

## Why there is no "ambition" layer above epics

It was considered and deliberately not built. The board already carries `Area`
(engine · bots · cloud/ops · studio · docs) and `State of play`, and the
latter partitions all 26 epics into five scannable columns — the grouping need
is met. Adding a third classification to answer *"what landed, what's next"*
would be solving a **content** problem with a **taxonomy**, which is how this
board became unreadable the first time. `Latest` and `Next` answer it directly.

Revisit if and only if the epic list genuinely stops being scannable — the seam
goes in at the **second** variant, not the fifth: a real second grouping need,
named, not an anticipated one.

## 🧱 How membership is represented — and why twice

An item belongs to an epic in **two** places, deliberately:

- **Sub-issues are the authority.** Native, they survive off the board, they
  give each epic a progress rollup in the repo, and agents that read issues
  (Codex, pi, Revi) see them without opening the project.
- **The `Epic` single-select field is a projection.** The views need it: a
  board's columns accept a single-select but **never** "Parent issue", a filter
  can match a field but not a parent, and GitHub's Board+swimlane-by-parent
  rendering **drops cards silently**
  ([community #193324](https://github.com/orgs/community/discussions/193324),
  unresolved, correlated with sub-issue count). An overview built on that path
  would lie without saying so.

A view is a read model, never a second source of truth (philosophy #5). The
field is re-derived from the parent by:

```bash
scripts/board-epics-sync.sh            # report
scripts/board-epics-sync.sh --apply    # fix the projection — the parent wins
task board:epics:sync                  # same, through the Taskfile
```

It is idempotent: a second consecutive run reports `board is consistent` and
writes nothing. **That second run is the proof, not the first.**

The rule it enforces holds at any depth: **an item's `Epic` equals its
parent's `Epic`.** When the parent is an epic issue that is the epic itself;
when the parent is an ordinary ticket (a legitimate two-level chain — #1209 →
#1072 → the Connectors epic) it is whatever that ticket carries.

**Sub-issues are attached to OPEN work only.** Closed items carry the field
without a parent — reported as `field-only`, which is the expected state. Two
reasons: GitHub caps a parent at **100 sub-issues** and 8 levels of nesting,
and a progress bar that counts history means nothing. With open-only, the bar
means *what is left*.

## ➕ Adding a ticket

1. Open the issue as usual.
2. Add it to the board, set `Epic`, `Status`, `Area`.
3. Attach it as a sub-issue of its epic (`gh issue edit` in the UI, or
   `addSubIssue`).
4. `task board:epics:sync` — it must say `board is consistent`.
5. Closing it later? Update its epic's **`Latest`**. Picking up the next one?
   Update its **`Next`**.

Step 3 is the one that gets skipped, and the sync script is what catches it.
It already earned its keep on its first run: a ticket created by another
session *while the sweep was running* showed up as `NO EPIC`, and two items
mis-filed by the bulk classifier showed up as `DIVERGENT`.

## 🆕 Adding an epic

1. Create the issue titled `epic: <emoji> <objective>`, label **`epic`**, body
   using the template below.
2. Add an option to the project's `Epic` field.
   ⚠️ **`updateProjectV2Field` must re-send every existing option WITH its
   `id`** — omit an id and every item holding that option is silently
   cleared. The schema says so, and it is the one irreversible mistake here.
3. Put the epic on the board and give it the new option as its own `Epic`
   value — that is how `board-epics-sync.sh` learns the mapping, so no epic
   list lives in the script. Set its **`State of play`** to match the mark in
   its body, and its `Status` to `In progress` (or `Blocked`, with the
   decision named) — see [the two axes](#two-axes--status-and-state-of-play).
4. Add its row to [state-of-the-art.md](state-of-the-art.md#the-chantier-map),
   and link that page back from the epic body. **Both ends, same change.**

## ✍️ The shape of an epic body

`epic:` is an **objective container**. A multi-page specification is a
`design:` ticket that an epic consumes as a sub-issue (#1006, #1165) — two
very different objects sharing one word is part of what made the board hard to
read.

```markdown
## Objective            — one paragraph, in product terms
## Where we stand — <date>
**Status: 🟢 humming**   — 🟢 humming · 🟡 running, watch it
                          🟠 gap identified · 🔴 blocked · ⚪ design stage
- evidence, each linked to its proof (run, check, PR, schedule, doc)
- and, honestly, whether the state is *measured* or *derived from the tickets*
## What is settled     — the acquis, N closed tickets, the 3–5 that matter
## What is left        — GitHub renders the sub-issues
## What blocks         — named, with the pending arbitration if there is one
```

The marks grade **evidence, not ambition**; the vocabulary is shared with
[state-of-the-art.md](state-of-the-art.md#the-maturity-vocabulary).

## ⚠️ What the API cannot do

Everything above is scriptable — fields, options, item values, sub-issues,
view creation (`name`, `layout`, `filter`, `visibleFieldIds`) — with one
exception:

**`ProjectV2ViewConfigurationInput` exposes only `visibleFieldIds`.** A view's
**group-by / column field is not settable through the API**. After creating a
board view you must open it and set it by hand:

> view tab → **View options** → *Column field* (board) or *Group by* (table)
> → pick **Epic** → **Save**.

Two views depend on it: 🚦 **Chantier state** (column field = `State of play`)
and 🎯 **Work by epic** (column field = `Epic`). Keeping it to two is why
there is no third grouped view — the same read is one `filterQuery` away on
📋 All items.

> **Do not try to automate it with Playwright — measured, 2026-09-18.** The
> configuration sub-panels of the *View options* menu render
> `Uh oh! There was an error while loading`, reproducibly, while the
> management half (Rename / Move / Duplicate / Delete / Generate chart /
> Export) renders fine in the same menu. It is not the API (the schema has no
> group-by input at all), not auth (signed in, and the other half works), and
> not the network (no request fails — the only 404 is an unrelated Copilot
> entitlement probe). It is a client-side render failure in the automation
> browser. A normal browser is fine, and the setting is six clicks.

Verify by reading it back rather than trusting the UI:

```bash
gh api graphql -f query='query{organization(login:"SocialGouv"){projectV2(number:203){
  views(first:20){nodes{number name layout
    groupByFields(first:3){nodes{... on ProjectV2FieldCommon{name}}}
    verticalGroupByFields(first:3){nodes{... on ProjectV2FieldCommon{name}}}}}}}}'
```
