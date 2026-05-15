---
name: iterion-bot-catalog
description: Catalog of iterion example bots — pick a bot name for each roadmap_item.assignee. The conductor will (eventually) route by assignee.
---

# Iterion Bot Catalog — for whats-next.bot's `propose_roadmap`, `revise_roadmap`, and `emit_action`

Consumed by three phases:

1. **`propose_roadmap` / `revise_roadmap`** — pick the right
   bot name for each `roadmap_item.assignee`. Leave it `""`
   when no existing bot fits.
2. **`emit_action`** — validate every assignee against the
   catalog before creating issues. Unrecognised assignees get
   stripped to `""` and the issue is labelled
   `needs-manual-triage`.

**Trust check first**: this catalog enumerates bots that exist
in the iterion source tree. If the workspace is NOT iterion,
none of these will resolve — all assignees should be `""` and
all issues will be `needs-manual-triage`.

## The pivot: kanban-driven, not shell-driven

whats-next.bot no longer shells out `iterion run <bot>`. Instead
every roadmap item becomes a kanban issue on the native board at
`<workspace>/.iterion/conductor/`, and a **conductor** dispatches
them. The conductor is wired via `iterion conduct <config.yaml>`.

**Important feature gap (today)**: the conductor dispatches a
SINGLE workflow for all eligible issues — it does NOT yet route
by `issue.assignee`. Until that ships, the operator has two
choices:
1. Run multiple conductors, one per assignee, each filtering by
   state or label.
2. Wait for the routing feature (which whats-next may have
   proposed as its `next_action` on this run).

In either case, whats-next records the assignee on the issue so
the future routing has the data it needs.

## Decision tree — pick `assignee` per roadmap item

Walk top-to-bottom; first match wins.

| If the work sounds like… | → `assignee` |
|---|---|
| "implement feature X", "add capability", "build the thing" | `vibe_feature_dev` |
| "review for production", "audit correctness", "find bugs" | `vibe_review_alternating` |
| "upgrade dependencies", "patch CVEs", "bump versions" | `secured-renovacy` |
| architectural choice, hiring, prioritisation meeting, alignment | `""` |
| operator is vague or it's cross-cutting | `""` |
| long-term theme (a quarter+ horizon) | usually `""` |

When in doubt, prefer `""` and let the operator triage manually
in the board UI. An empty assignee is honest; a wrong one
wastes a bot run.

## Bot reference

### `vibe_feature_dev`

- **Path**: `examples/bots/vibe_feature_dev.bot`
- **Required var**: `feature_prompt` (one feature + acceptance
  criteria).
- **Pipeline**: plan → act → simplify → alternating Claude/GPT
  review/fix → commit.
- **Budget**: 1 branch, 4h, $120.
- **Worktree**: `auto`. **Sandbox**: `auto`.
- **Use when**: an item can be phrased as one feature with a
  clear "done" state.

Example `args` payload for a roadmap_item:
```json
{"feature_prompt": "Add a CSV-export button to the reports page that POSTs to /api/export and saves to ~/Downloads. Include a Playwright test."}
```

### `vibe_review_alternating`

- **Path**: `examples/bots/vibe_review_alternating.bot`
- **Vars**: `workspace_dir` (default), `scope_notes: string=""`
  — free-text steering ("focus on auth and persistence",
  "ignore the editor").
- **Pipeline**: alternating Claude/GPT review → fix loop until
  two consecutive cross-family approvals (max 15 iterations).
- **Budget**: 1 branch, 2h, $60.
- **Use when**: existing code, operator wants rigorous
  production-readiness, doesn't yet know what's wrong.

### `secured-renovacy`

- **Path**: `examples/secured-renovacy/bot.bot` (or packed
  `examples/secured-renovacy.botz`).
- **Vars**: `scope: "patch"|"minor"|"patch,minor,major"`,
  `max_packages_per_run`, `major_policy:
  "skip"|"gate"|"attempt"`, `update_scope`. **Ask before
  running with `major_policy: "attempt"`**.
- **Budget**: 4 branches, 12h, $100, 500 iter, 5M tokens.
- **Use when**: dependency risk is the priority; CVE alerts;
  stale lockfiles.

## Issue-creation mapping (consumed by `emit_action`)

Each `roadmap_item` → one `iterion issue create` invocation:

```
roadmap_item.title    → --title
roadmap_item.body     → --body
roadmap_item.assignee → --assignee
roadmap_item.args     → --field bot_args=<flat string list>

horizon=next_action  → --labels horizon:next-action,source:whats-next
horizon=short_term   → --labels horizon:short-term,source:whats-next
horizon=long_term    → --labels horizon:long-term,source:whats-next
```

`bot_args` is a comma-joined flat string list. For
`args={"feature_prompt":"Add CSV export"}`, the field value is
`--var,feature_prompt=Add CSV export`. The eventual conductor
router will split on `,` and emit `--var` flags to the
dispatched bot.

## Verification ritual (emit_action)

Before creating each issue:

1. If `assignee != ""`, look it up in the table above. If it's
   not one of the three known bots, AND it doesn't correspond
   to a `.bot` file the explorer surfaced — strip to `""` and
   add label `needs-manual-triage`. NEVER invent.
2. Empty assignee is FINE. The issue lands without an assignee
   and the operator triages.

## What you do NOT do

- You do NOT shell out `iterion run …` directly. The bot used
  to do that; it doesn't anymore.
- You do NOT enumerate bots from the user's free-text alone.
  Walk the decision tree against the explore summary.
- You do NOT recommend an `assignee` whose `.bot` file the
  explorer did not surface.
- You do NOT recommend more than one `next_action`.
