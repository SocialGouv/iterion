---
name: interview-playbook
description: Adaptive interview method for converging a decision-quality application spec (SPEC.md) with the operator before development starts. Read this before conducting an app-spec interview.
---

# interview-playbook

## Why an interview

A greenfield brief is almost always under-specified: "an app for X" hides
a dozen decisions (who uses it, what is OUT of scope, where the data
lives, what "done" means). The development campaign that follows you
spends its budget far better against a precise, committed SPEC.md than
against a one-liner — every ambiguity you resolve here is a wrong slice
it won't build. You are the interviewer, **not the developer**: capture
the decisions the operator owns; leave the *how* (file layout, libraries
beyond the stack choice, implementation order) to the campaign.

## The coverage grid — adaptive, never a questionnaire

Converge these nine areas, in whatever order the conversation makes
natural. Skip an area the brief already answers; dig where answers are
vague. ONE question per turn (the one that would change a decision),
always with your recommended answer when you have one.

1. **Usage** — what does the app DO, concretely? The 2–3 core user
   journeys, stated as verbs ("search a directory, open a detail page").
2. **Users** — who, roughly how many, and with what roles/permissions?
   Public? Internal? Admin back-office?
3. **Scope in / OUT** — the OUT list is the highest-signal answer you
   can capture. Propose candidates to exclude ("no accounts in v1?",
   "no mobile app?") — operators find exclusions easier to confirm than
   to volunteer.
4. **Stack** — framework/language + the one-line rationale. If the
   operator named it in the brief, confirm and move on; if not,
   recommend one that fits the journeys and their team.
5. **Data** — the main entities, their relationships, where they live
   (file, SQLite, Postgres, an external API?), and whether data is
   seeded, imported, or user-created.
6. **Auth & sessions** — none / shared secret / accounts / SSO. For a
   first draft, "none, add later" is often the right call — say so.
7. **Hosting & deployment target** — local only, a PaaS, a container?
   This mostly shapes the README and config, not the code.
8. **Non-goals** — what this app is deliberately NOT (a CMS, a
   general-purpose tool…). One or two lines that keep the campaign from
   gold-plating.
9. **Definition of done** — what a shippable FIRST DRAFT must
   demonstrate, as observable checks ("searching 'préfecture' returns
   results", "the page passes an accessibility smoke").

## Convergence heuristic

- Typical interviews converge in **6–15 turns**; you are bounded at 30.
- STOP asking when another question would no longer change any decision
  in SPEC.md — or when the operator says "go / on y va / that's enough".
- When 1–2 areas stay genuinely undecided, record them in SPEC.md as
  explicit open questions with your default ("Auth: none in v1 —
  revisit if usage grows") rather than stalling the interview.
- Each turn: restate the spec-so-far in a few lines FIRST (the operator
  corrects drift early), then ask the next question. Offer 2–4
  `quick_replies` when the plausible answers are enumerable.

## Anti-patterns

- **The questionnaire.** Never send a numbered list of 8 questions.
  One decisive question per turn, recommendation attached.
- **Over-asking.** If you can infer an answer from a previous one with
  high confidence, state the inference and move on ("public directory →
  no auth in v1, I'll assume that unless you object").
- **Solutioning.** Component libraries, folder structures, ORM choice —
  not yours. The spec records WHAT and WHY; the campaign owns HOW.
- **Silent convergence.** Never set `spec_ready: true` in the same turn
  you introduced new material the operator hasn't seen. The last turn
  before convergence should be a spec recap the operator approves
  (explicitly, or via an "on y va" quick reply).

## SPEC.md format

Write at the workspace root, 2–3 pages max, every claim testable at the
draft review:

```markdown
# <App name> — specification

## Objective          (2–4 lines: what and for whom)
## Users              (roles, rough volume)
## Scope
### In                (the v1 journeys, bulleted)
### Out               (explicit exclusions)
## Stack              (choice + one-line rationale)
## Data model         (entities + relationships sketch; storage)
## Auth & hosting     (decisions or explicit "none in v1" defaults)
## Non-goals
## Definition of done (observable checks for the first draft)
## Open questions     (only if any remain — each with your default)
```

## Commit protocol

The spec hands off as a FILE, not as chat history — the campaign (and
every later pass, and any resumed run) re-reads it from the tree:

```sh
cd <workspace> && (git init -b main 2>/dev/null || true) \
  && git add -A \
  && (git diff --cached --quiet || git commit -m "docs(spec): SPEC.md from operator interview")
```

Idempotent on purpose (safe on a bare dir, safe to re-run). Only after
this commit exists may you report `spec_ready: true`. On a RE-interview
(the workspace already has an app and a SPEC.md), amend the spec file,
commit as `docs(spec): evolve SPEC.md — <topic>`, and keep the existing
decisions you are not changing.
