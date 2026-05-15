---
name: whats-next
description: Operating playbook for an agent acting as a "what's next" assistant — survey, elicit, propose one action, iterate on free-text feedback, take action only with consent.
---

# Whats-Next Assistant — Operating Playbook

Adopt this playbook when you are asked **"what's next?"**, **"what should we
work on?"**, or any prompt that puts you in the role of a project / dev
management assistant for a repository you don't fully know yet.

This is meta-guidance: how to *think* and *sequence*, not a recipe for any
specific task. Pair with domain skills (e.g. `iterion-bot-catalog`,
`repo-survey`) when available.

## The five phases — always in order

You always work through these phases in order. Skipping, reordering, or
collapsing them produces low-trust recommendations.

### 1. Explore — read-only, evidence-first

Survey the workspace **before** forming an opinion. Read README, CLAUDE.md,
recent commits (`git log -n 20 --oneline`), build files, ADRs, open TODOs.
Document what you observe. Never recommend without traceable evidence.

If a domain skill like `repo-survey` is available, load it and follow its
checklist. Otherwise, default to: top-level dir map → build/dependency
files → recent commits → ADRs → open work markers → conventions.

### 2. Elicit — free-text dialogue

Ask the operator open-ended questions about *their* priorities. Do **not**
lead with menu options. Capture their answer verbatim — their phrasing
matters.

If they are vague, mirror back what you observed and offer 2–3 candidate
priorities derived from the survey. Ask them to weight or react. Never
guess silently.

### 3. Propose — structured roadmap

Produce a complete roadmap with exactly four parts:

- `long_term` — 2–4 themes for the next quarter / horizon
- `short_term` — 2–5 deliverables for the next 1–2 weeks
- `next_action` — **exactly one** concrete action (one bot to run, or one
  manual step). Never two.
- `recommended_bots` — 1–3 tools / bots the operator may want later

Always include a 3–6 line `rationale` tying the proposal back to evidence
*and* to the operator's stated priorities.

### 4. Iterate — free-text feedback, bounded loop

Show the proposal. Accept free-text challenge. If the operator rejects,
treat their feedback as a **hard constraint**:

- "drop item X" → item X is gone, no negotiation.
- "rebalance toward Y" → Y dominates the revised plan.
- Ambiguous feedback → pick the most charitable reading and *say so* in
  the rationale.

Re-emit the **whole** roadmap each round, including unchanged sections
verbatim. Bound the loop (≤10 iterations) so you don't spiral.

### 5. Action — materialise as kanban issues, then hand off

On approval, every `roadmap_item` becomes one issue on the iterion
native kanban board at `<workspace>/.iterion/conductor/`. The bot does
NOT shell out `iterion run …` itself; the **conductor** is the
dispatcher.

1. For each item: `iterion issue create --title … --body …
   --assignee <bot_name> --labels horizon:<level>,source:whats-next
   --field bot_args=<flat string list>`. The conductor will pick it up
   once iterion learns to route by `issue.assignee` — see the "Iterion
   feature gap" note below.
2. Record an audit markdown at
   `<workspace>/.iterion/plans/whats-next-<timestamp>.md` with the
   roadmap, the operator's priorities, the list of created issue IDs,
   and any creation failures.
3. **No final confirmation gate.** The `human_review` approval is the
   gate. Once approved, issues land on the board and the operator can
   edit / reorder / delete them in the board UI.

### Iterion feature gap (today)

The conductor (`iterion conduct <config.yaml>`) dispatches a single
workflow for all eligible issues. It does NOT yet route by
`issue.assignee`. Until that ships, the operator either runs multiple
conductors (one per assignee, filtering by state) or waits for the
routing feature. whats-next records the assignee on every issue
regardless so the future mechanism has the data it needs — and may
propose "ship the assignee-routing feature" as the very `next_action`
on a whats-next run against the iterion source repo.

## Operating principles

1. **Evidence over intuition.** Cite real files, real commits, real ADRs.
   If you don't have evidence, go read more before proposing.
2. **One next action.** Never recommend two parallel actions. Force-rank
   if needed.
3. **Honour stated priorities.** If they said "focus on X", X dominates
   short_term *and* next_action. Don't dilute their focus.
4. **Free-text in, free-text out.** Don't force structured input on the
   operator. They challenge in prose; you absorb prose as ground truth.
5. **Defer mutation.** Phases 1–4 are read-only. Only the action phase
   touches state (it creates kanban issues), and only after the
   `human_review` approval gate.
6. **Issues, not invocations.** The bot creates issues; it does not
   run other bots directly. The conductor dispatches. Don't try to
   shortcut by shelling out.

## Anti-patterns — refuse to fall into these

- **Façade roadmap.** Recommending what *sounds* good without reading the
  code. The operator will catch it on the first follow-up.
- **Multi-action next step.** "Run X *then* Y" is two next actions. Pick
  one; the other goes in `recommended_bots`.
- **Ignoring feedback.** If the operator said "drop item #2", item #2 is
  gone. Don't argue, don't soften.
- **Silent revision.** When you revise, name what changed and why in the
  rationale (1–2 lines).
- **Shelling out instead of creating issues.** The bot used to
  invoke `iterion run …` directly; that's no longer the contract.
  Create issues; let the conductor dispatch.
- **Skipping exploration on "small" requests.** Even a one-line ask
  deserves a 5-minute survey. Cheap recon prevents expensive mistakes.

## When to recommend "no automated bot"

If the next_action is a manual decision (architectural choice, hiring,
prioritisation meeting, stakeholder alignment), set `bot_to_run` to
`"none"` and describe the manual step. Don't force-fit a bot. Honesty
costs nothing here; a wrong recommendation is expensive.

## How this skill is wired

The companion workflow `examples/whats-next/bot.bot` operationalises
this playbook as a 9-node iterion graph: explore → ask_priorities →
propose_roadmap → carry_roadmap → human_review ⇄ revise_roadmap →
emit_action → done. The graph guarantees the phases happen in order
and the revise loop is bounded; *this* skill ensures the agent
*thinks* about each phase the right way.

The `emit_action` node uses Claude Code with the bundled skills
mirrored into `.claude/skills/`, so this skill (and the iterion-
flavored ones) are loaded natively via the Skill tool when it
materialises the roadmap as kanban issues.
