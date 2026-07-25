---
name: roadmap-synthesis
description: Compose a roadmap study's synthesis — 6-9 named chantiers tiered now/next/later, quick-wins, an argued top-3, explicit blind spots — and the framed-ticket template (Context / Done criteria / Verify) every dispatchable ticket uses.
---

# Roadmap Synthesis — chantiers, tiering, framed tickets

This skill governs two artifacts of a roadmap study: the SHAPE of the
synthesis you write in `reply`, and the BODY of every ticket you create
on the board afterwards. There is no output schema — the synthesis is
markdown to the operator; the tickets are the durable output.

Start from evidence, not opinion: every chantier traces to an audit
finding (see `repo-survey`), a memory thread, or a file you re-read —
never to training-data priors ("repos like this usually need…").

## The chantier — the atomic unit

- Named: `C<n>: <short imperative>` — so the operator can point at one
  ("swap C7 for C5") without re-describing it.
- One paragraph: what it solves, why now (or why not now), the scope
  boundary, with compact evidence (`file:line`, sha, ADR number).
- Optionally: which existing tickets/PRs/findings feed it.

## Tiering — now / next / later

- `now` — start this week. 1-2 chantiers, no more.
- `next` — this month. 2-4.
- `later` — this quarter or "when the now-tier lands". 2-4.

6-9 chantiers total. Fewer usually means the audit was thin; more is
noise — merge or drop. A chantier list padded to "look strategic"
beyond what the evidence supports is a façade (see the playbook's
anti-patterns).

## Quick-wins — a distinct tier

Small scope, tight loop, obvious value, low coupling to open chantiers.
NOT a synonym of `now`: a quick-win is dispatchable today and done in
one run; a `now` chantier can be a multi-week effort that merely starts
today. List each in one line. At arbitrage the operator picks: dispatch
them to worker bots, keep them for manual work, or defer. You never
code them yourself — you are read-only outside the board.

## The argued top-3

Name the 3 chantiers you would launch first, and argue each pick with
its tie-breaker, in order of precedence:

1. **Operator's stated priority** — the most direct hit wins.
2. **Risk reduction** — broken CI, security, data-loss beat new
   capability.
3. **Smaller blast radius** — one-package upgrade beats "upgrade
   everything".
4. **Reversibility** — read-only or easily-reverted work beats deep
   mutation.

The top-3 is a recommendation, not a decision: the operator's arbitrage
answer is the last word.

## Blind spots

Close the synthesis with what the study did NOT cover — walk the
checklist in `repo-survey` (adoption, public docs, security posture,
GDPR/privacy, backups/DR, cost, release/versioning, ecosystem risk) and
name every axis no audit touched. If you have an opinion anyway, say
so; if not, offer the choice at arbitrage (targeted audit / ticket for
later / out of scope).

## Reply shape

```
<1-2 sentences: evidence density, the main tension>
## Chantiers            — grouped by tier, each named + one paragraph
## Quick-wins           — one line each
## Top 3 à lancer       — the argument
## Angles morts         — bullets
<pointer: the grouped arbitrage questions come next — see operator-arbitrage>
```

Mirror the operator's language throughout. Cite compactly; never paste
audit reports or raw JSON.

## The framed ticket — Context / Done criteria / Verify

Every dispatchable ticket you create (`create_issue`) uses this body —
non-negotiable. It is the anti-façade contract: the `## Verify` section
is what the closer (a future you, or the operator) executes before
saying "delivered".

```
## Context
<2-4 lines: why this ticket exists, what surfaced it — audit finding
with file:line/sha, operator priority, ADR/PR reference. No vibes:
every claim grounded.>

## Done criteria
<3-6 bullets, ALL testable — file exists / test passes / grep returns
0 / PR merged / label removed. At least one in negative space: "X does
NOT exist any more", "no callers of Y remain". Each checkable by a
fresh reader in ≤5 minutes.>

## Verify
<2-4 bullets: the concrete commands/greps/test invocations that prove
each done-criterion.>
```

Labels per `iterion-label-vocabulary` — call `list_labels` FIRST, then:
`source:whats-next` + `horizon:<now|next|later>` + `axis:<area>` when
one dominates + `epic:<slug>` when the batch belongs to a named effort.
Priority via the `priority` field, blockers via `blockers[]` when a
ticket depends on another landing first.

Bot routing (`bot` at create time, or `set_bot` after) and `bot_args`
carry the dispatch payload — e.g.
`{"feature_prompt": "Add CSV export to the reports page"}`.

## Assignee discipline — you own the routing decision

A wrong bot silently runs the wrong workflow and burns budget. The
catalogue is finite (`iterion-bot-catalog`): walk its decision tree and
the Distinguishers pairs before committing. The ladder, in order:

1. **Confident match → set it.** One catalogue row matches, the body
   fits one bot's "use when" line, and you can name the var override
   without paraphrasing the operator.
2. **Closest match → set it with a caveat** in the ticket body's last
   line ("closer to `feature-dev` than `whole-improve-loop` because
   the change is a new capability, not an axis-wide improvement").
3. **Ambiguous → leave the bot unset and ask at arbitrage** — one line
   per ambiguous ticket, as a decision block (`operator-arbitrage`).
4. **No fit → propose a new bot**: a separate ticket for `feature-dev`
   whose `feature_prompt` describes the bot to build; mark the original
   work blocked on it.

**Never silently default to `feature-dev`** — that was the historical
failure mode. The unmatched catch-all is "unset", not a guess. Never
invent a bot name: validate against the catalogue.

## In-session work vs dispatched tickets — don't double-bill

Board-state operations the operator could ask you to do in the next
chat turn (triage, close stale, re-label, promote) are IN-SESSION work
— do them in conversation, never as tickets. Tickets are for work that
needs a long-running bot on a branch (code, docs, tests, upgrades).
When in doubt: "could a one-line chat instruction do this?" → yes:
in-session; no: ticket.

## Revision discipline

When the operator rejects or refocuses the synthesis:

1. Their feedback is a hard constraint ("drop X" → X is gone).
2. Re-emit the WHOLE chantier list — unchanged parts verbatim, changes
   clearly visible — plus 1-2 lines on what changed and why.
3. Read ambiguous feedback charitably and state your reading inline so
   one round can correct it.

## What the synthesis never does

- Add scope the operator didn't ask for, or pad tiers for optics.
- Create tickets BEFORE arbitrage (the batch is the operator's call —
  see the playbook's guardrails on bulk actions).
- Bias with adjectives — conviction comes from precision, not prose.
