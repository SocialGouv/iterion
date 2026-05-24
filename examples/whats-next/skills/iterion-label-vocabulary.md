---
name: iterion-label-vocabulary
description: Canonical label namespaces for the iterion native kanban board. Bots query the existing label set via mcp__iterion_board__list_labels before emitting new issues so the vocabulary stays consistent across sessions and operators.
---

# Iterion Label Vocabulary — for every bot that writes labels

## Why this skill exists

Labels accumulate from many sources: whats-next runs, sec-audit findings, manual operator triage, ad-hoc imports. Without a vocabulary, each session invents its own scheme — we ended up with `source:battle-tested-plan-2026-05-24` (a 35-character session-specific string) sitting next to `source:whats-next` (a clean per-bot tag) on the same board. The result: filters break, the operator can't tell at a glance whether two labels mean the same thing, and the studio's label picker becomes useless.

Fix: a small set of canonical namespaces, a list_labels query before every emit, and a strong preference for **reusing an existing value** over inventing a new one.

## Canonical namespaces

Use `namespace:value` (colon-prefixed). Single-word tags exist but are rare.

| Namespace | Allowed values | Meaning |
|---|---|---|
| `source:` | bot name, import path, or `manual` | Where the issue came from. Prefer the bot identity (`source:whats-next`, `source:sec-audit-source`, `source:sec-audit-deps`, `source:doc-align`). For one-off operator imports, `source:manual` plus a date suffix only when chronology genuinely matters. |
| `horizon:` | `next-action`, `short-term`, `long-term`, `theme` | Time horizon. `theme` for strategic items the operator never expects to dispatch directly. |
| `epic:` | short kebab-case name | Long-running effort grouping multiple issues. Example: `epic:battle-tested`, `epic:cloud-readiness`. ONE epic label per issue is enough — multi-epic items dilute the signal. |
| `sprint:` | integer | Sprint window the issue is committed to. Example: `sprint:1`. Combine with `epic:` to scope. |
| `axis:` | area name from the repo | Subject area. Prefer names that match a top-level directory or `pkg/<x>/` package: `axis:runtime`, `axis:studio`, `axis:dispatcher`, `axis:dsl`, `axis:backend`, `axis:cloud`, `axis:sandbox`. For cross-cutting: `axis:testing`, `axis:observability`, `axis:reliability`, `axis:security`, `axis:docs`, `axis:bot`, `axis:performance`. |
| `priority:` | `low`, `medium`, `high`, `critical` | Visible flag complementing the numeric `priority` field. Use only when one of `medium/high/critical` is meaningful; do not label everything `priority:low`. |
| `status:` | soft state flag | `status:blocked-by-research`, `status:waiting-on-external`, `status:archived-by-bot`, `status:duplicate-of-<id>`. Operator-facing nuance the formal board state can't capture. |
| `needs:` | operator action | `needs:manual-triage` (assignee invalid), `needs:bot-assignment`, `needs:retest`. Signals the board state alone can't (issue is in backlog but needs human work before it's dispatchable). |

**Single-word labels** (no namespace) are reserved for unambiguous cross-cutting tags: `breaking-change`, `hotfix`, `experiment`. Use sparingly; namespaced labels scale better.

## The ritual — query before invent

Every bot that writes labels MUST call **`mcp__iterion_board__list_labels`** (or `GET /api/v1/native/labels` over HTTP) BEFORE choosing labels for new issues. The result is a JSON array:

```json
[
  {"label": "source:whats-next", "count": 50, "last_used_at": "2026-05-24T15:37:17Z"},
  {"label": "epic:battle-tested", "count": 12, "last_used_at": "2026-05-24T15:07:06Z"},
  ...
]
```

Use it to:

1. **Reuse existing values** when semantically equivalent. If `source:whats-next` already exists, don't invent `source:whats-next-2026-05-24`. If `epic:battle-tested` exists, don't write `epic:battle-tested-plan` — they refer to the same effort.

2. **Spot operator-created labels** (manual labels surface via list_labels too — the registry doesn't distinguish bot-written from operator-written). Honor them: if the operator made `priority:critical` mean something specific on this board, keep using that exact spelling.

3. **Detect drift early**. If you see two near-identical labels (`epic:cloud` and `epic:cloud-readiness`), surface the question in `rationale` rather than picking one silently.

## The minimum label set per emit_action issue

Three is the comfortable baseline; four is reasonable when an epic + sprint is in play. More than five labels per issue dilutes filtering.

- **`source:<bot>`** — always.
- **`horizon:<value>`** — when the issue is positioned on a time horizon (every whats-next-emitted issue).
- **`epic:<name>`** — when the issue belongs to a named epic. Optional otherwise.
- **`axis:<area>`** — when one axis dominates. Optional when truly cross-cutting.
- **`sprint:<N>`** — when committed to a specific sprint. Optional.

Avoid one-issue-only labels (`source:battle-tested-plan-2026-05-24` is the canonical anti-pattern: 12 issues, all created in one batch, distinguished from `source:whats-next` only by the date — the date belongs on `created_at`, not the label).

## Anti-patterns

- **Dated labels for non-recurring events** — the issue's `created_at` covers chronology. A label survives the session that created it; dates lock you in.
- **Synonyms for the same concept** — `area:runtime` AND `axis:runtime` AND `pkg:runtime` on the same board. Pick one; we use `axis:`.
- **Verb-form labels** — `to-do`, `fixing`, `reviewing`. Board state already encodes this. Use `status:` or `needs:` for nuance.
- **Personal labels** — `joe-needs-to-look-at-this`. Use the `assignee` field.
- **More than 5 labels on one issue** — filters become noise.

## When you introduce a new label

Only when no existing label fits AND the new value will recur. Document the new value in the run's audit markdown (whats-next's `docs/plans/whats-next-*.md`) so a future contributor reading the audit knows where the convention came from. If the value should become canonical, propose adding it to this skill via a `doc-align` follow-up item.

## Cheat-sheet

```
Most issues:  [ source:<bot>, horizon:<h>, axis:<a> ]
With epic:    [ source:<bot>, horizon:<h>, axis:<a>, epic:<name>, sprint:<N>? ]
Findings:     [ source:<bot>, axis:security|reliability|docs, needs:retest? ]
Manual:       [ source:manual, horizon:<h>, axis:<a>, status:<s>? ]
```

Reread the `list_labels` output if uncertain — the operator's existing vocabulary is the source of truth.
