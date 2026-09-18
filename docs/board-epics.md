# 🗂️ Runbook — the GitHub board's epics

**Read it when** you add a ticket to the [board](https://github.com/orgs/SocialGouv/projects/203),
create or retire an epic, wonder why a card is in the wrong column, or need to
know which view answers your question.

The board is the roadmap/chantier view; the iterion **native board** (studio
`/board`) stays the bots' operational surface. The working contract —
statuses, session phases, the claim rule — is in [AGENTS.md](../AGENTS.md).
This page is the mechanics.

## 🧭 Which view answers what

| view | question it answers |
|---|---|
| 🎯 **Epics** | *What is live on each objective?* Board, columns = `Epic`, `-status:Done`. |
| 📚 **Epic map** | *How is each chantier doing?* The 21 epics alone, with priority and progress. |
| 🧭 **Tally by epic** | *What have we actually done?* Every item, grouped by `Epic` — the acquis, counted. |
| 🧩 **Unassigned** | *Did anything escape?* `no:epic`. **It must read 0.** |
| 🗂 Kanban · 🚀 In flight · 📥 Triage · 🗺 Roadmap · 📋 All items | the day-to-day views, unchanged |

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
   list lives in the script.
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

Two views depend on it: 🎯 **Epics** (column field = `Epic`) and 🧭 **Tally by
epic** (group by = `Epic`).

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
