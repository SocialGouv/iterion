---
name: backlog-handoff
description: >
  How Evoly hands proposed evolutions to Nexie and the operator — one
  deep artifact per evolution in the shared findings/ memory inbox, plus
  a dispatch-ready backlog kanban ticket (set_bot + self-contained body)
  the human can launch by dragging to ready.
disable-model-invocation: true
---

# Backlog handoff — two channels, both picked up automatically

Every proposed evolution lands on **two channels** so neither Nexie nor
the operator has to go looking:

- **Channel A — `findings/` memory inbox** (the deep artifact: plan,
  technical decisions, rationale).
- **Channel B — a `backlog` kanban ticket** (the actionable, dispatch-
  ready card).

Both are read by Nexie's next survey with zero changes on Nexie's side.

## Channel A — the findings artifact

For each evolution, `memory_write` to the shared findings scope
(`memory: { scope: "findings" }` → `projects/<key>/memory/findings/`) a
file named `<YYYY-MM-DD>-<slug>.md` with this frontmatter:

```
---
title: "<one-line summary>"
description: "<one sentence>"
kind: "evolution"
source_bot: "evolve"
tags: ["axis:<x>", "horizon:<now|next|later>", "severity:<low|med|high>"]
---

# <title>

## Why
<the rationale, tied to the vision axis it advances>

## Plan
<the technical approach — enough for feature-dev / Nexie to act>

## Technical decisions
<the decisions the operator confirmed that shape this>

## Acceptance
<what "done" looks like>
```

This is the durable, deep record. Nexie's `emit_action` auto-hygiene
later archives it when a resolving commit lands — you do not manage its
lifecycle. Set the evolution_item's `finding_file` to this path so the
ticket body can point at it.

## Channel B — the dispatch-ready backlog ticket

Create one kanban issue per evolution (see `iterion-board.md` for the
tools):

Call `create_issue` once per proposal with:

- title = the evolution title; **state = `backlog`** (do NOT promote to
  `ready` — that's the operator's or Nexie's call); body = a
  **self-contained spec**;
- labels = `source:evolve`, `kind:evolution`, `horizon:<now|next|later>`,
  `axis:<x>`;
- `bot` and typed `bot_args` when a catalog bot clearly fits (for example,
  `feature-dev` plus its `feature_prompt`). When no bot clearly fits, omit
  both and add `needs-manual-triage`.

`set_bot` / `add_labels` / `remove_labels` (or the absolute `set_labels`
for a full rewrite) remain available for correcting a ticket after
creation, but the create call accepts those typed fields directly.

### The body IS the dispatch prompt

When the operator drags the ticket to `ready`, the dispatcher routes to
the bot named by `set_bot` and renders **{{issue.title}} + {{issue.body}}**
into that bot's prompt via its `dispatch_vars`. So the body must stand on
its own — a feature-dev ticket's body must be a complete feature spec,
not "see the finding". Include a one-line pointer to `finding_file` for
the deep context, but make the body self-sufficient.

`create_issue.bot_args` is the typed per-ticket `--var` map the dispatcher
applies at launch; it is distinct from freeform `fields`. Keep the body
self-contained even when args are present, so the ticket remains useful to a
human and to bots whose `dispatch_vars` consume title + body.

## What Evoly does NOT do at handoff

- Never move a ticket to `ready` — the operator launches it by dragging,
  or Nexie promotes it.
- Never set a hard deadline.
- Never invent a `suggested_bot` you're not confident about — an empty
  bot + `needs-manual-triage` is honest; a wrong bot wastes a dispatch.
- Never assign more than ~10 evolutions in one run.
