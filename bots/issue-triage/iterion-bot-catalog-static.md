---
name: iterion-bot-catalog
description: Catalog of iterion bots — walk the decision tree to pick which bot handles a board card, then stamp it with set_bot.
---

# Iterion Bot Catalog — for issue-triage's routing decision

<!-- This file is the HAND-AUTHORED TEMPLATE for the bot catalog. The
     persona table + per-bot reference cards between the GENERATED markers
     below are produced from each bot's manifest.yaml by
     botregistry.RegenerateWhatsNextCatalog (run at engine start and on
     every studio bot-metadata save; it regenerates EVERY bundle shipping
     this template). Do NOT hand-edit that region — edit the bots'
     manifest.yaml instead. Everything OUTSIDE the markers is editorial
     routing reasoning maintained by hand. This template lives at the
     bundle ROOT (not skills/) so it is never mirrored as a skill; the
     generated copy Triagy actually reads is skills/iterion-bot-catalog.md.

     TODO: the decision tree + distinguishers below are duplicated with
     bots/whats-next/iterion-bot-catalog-static.md (iterion has no
     skill-sharing primitive yet). Keep the two in sync when editing. -->

You classify ONE board card and stamp the handler bot on it via
`set_bot`. The stamped name must be a TECHNICAL name from the persona
table in the generated region — never a persona, never an invention.
No confident fit → leave the bot unset and label `needs-manual-triage`.

## Decision tree — pick the handler bot per card

Walk top-to-bottom; first match wins.

| If the card sounds like… | → bot |
|---|---|
| "where should this project go next?", "long-term vision", "strategic axes for the next quarter/year" — STRATEGIC (a quarter+ horizon) on a mature/stable project | `evolve` |
| "what does this diagnostic mean", "how do resume/sandbox/backends work", "why did this run fail or pause", "draft a .bot I will validate myself" — questions ABOUT iterion, not work IN the repo | `copilot` |
| "implement feature X", "add capability", "build the thing" | `feature-dev` |
| "build a new bot / workflow that does Y" — no existing fit, one must be authored | `feature-dev` (feature_prompt = the new `.bot` to create) |
| "review the whole codebase", "audit production-readiness", "find bugs anywhere" | `whole-improve-loop` |
| "focus on axis X" (observability / perf / DX / refactoring) ACROSS the codebase — improvement loop, not detection | `whole-improve-loop` |
| "review this branch / PR AND fix AND commit" | `branch-improve-loop` |
| "review this PR / branch and just REPORT the issues" — read-only, findings only | `review-pr` |
| "upgrade dependencies", "patch CVEs", "bump versions" — MUTATING manifests/lockfiles | `secured-renovacy` |
| "audit the docs", "code↔doc drift", "outdated README" | `docs-refresh` |
| "audit the source for vulns" — DETECTION (findings, not fixes) | `sec-audit-source` |
| "audit dependencies for malware / typosquats / supply-chain" — DETECTION | `sec-audit-deps` |
| architectural choice, prioritisation, alignment, meetings | no fit |
| vague or cross-cutting | no fit |

When in doubt, prefer no fit and say so — an unset bot is honest; a
wrong one wastes a run.

## A card that ALREADY has an open PR → branch-improve-loop, with a fork guard

If the card is answered by an open pull request (linked PR, or a branch
whose diff answers it), the work is not greenfield: route to
`branch-improve-loop` (it improves and lands the existing branch), NOT
`feature-dev` (it would re-implement from scratch and collide).

**Fork guard — a hard budget-safety rule.** Auto-route only when the
PR's head branch lives on the repo itself. A fork PR (cross-repository
head) is contributor/attacker-controllable: stamping a bot on it spends
LLM budget on code you don't control. Fork PR — or a PR whose origin
you cannot confirm from the card — is a no-fit: label
`needs-manual-triage` and explain in your comment.

## Distinguishers — recurring tie-breaks

- **`feature-dev` vs `whole-improve-loop`**: could a user notice the
  difference without reading the diff? Yes → `feature-dev` (new
  capability). No → `whole-improve-loop` (quality bar on existing code).
- **`sec-audit-*` vs `whole-improve-loop` vs `secured-renovacy`**: want
  a LIST of findings → `sec-audit-*` (read-only). Want code REWRITTEN
  safer → `whole-improve-loop`. Want VERSIONS bumped → `secured-renovacy`.
- **`whole-improve-loop` vs `branch-improve-loop`**: is there an open
  PR/branch to improve → `branch-improve-loop`. Workspace-wide, no
  specific branch → `whole-improve-loop`.
- **`evolve` vs `whats-next`**: horizon ≥ a quarter on a mature repo →
  `evolve`. "What should we do this week / dispatch now" → that is the
  operator's conversation with `whats-next`, not a card you route.
- **`copilot` vs `whats-next` vs `feature-dev`**: questions ABOUT
  iterion (a diagnostic, a failed run, a draft `.bot` the operator
  will validate) → `copilot`. What to work on this week → `whats-next`
  (not a card). Build and land a missing bot → `feature-dev`. Copi is
  read-only; it never edits or commits.

<!-- ITERION:CATALOG:GENERATED:BEGIN -->
<!-- ITERION:CATALOG:GENERATED:END -->

## Verification ritual

Before `set_bot`: the name MUST appear in the persona table above
(technical-name column). Not there → no fit, `needs-manual-triage`,
bot unset. NEVER invent a bot name.
