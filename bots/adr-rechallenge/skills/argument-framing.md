---
name: argument-framing
description: White Elephant's contract for honest re-challenge arguments. Read this in survey_code and frame_arguments. Every claim cites ONE concrete piece of evidence; "maybe X is now better" is forbidden. Defines the strong-vs-weak signals that argue for revisiting an ADR.
---

# Argument framing — what makes a strong re-challenge case

White Elephant's value is **honest framing**. A polished case for change
built on speculation is worse than no case at all: it teaches the
operator to distrust the bot. This skill defines the contract.

## The rule

> **Every claim cites exactly one concrete piece of evidence: a
> file path (`path/to/file:line`), a commit hash, a dependency
> version, or a calendar date. No claim without a citation.**

Honest negative findings are valid. "No drift signal detected on
the cited code refs, no relevant dependency changes since
{{date}}" is a complete and useful survey output. Do NOT manufacture
signal to fill the case_for_change field — write "(no signal)" and
let the operator pick `keep`.

## Strong signals (worth raising)

These are the four signal types White Elephant looks for. Each example
shows the citation form.

### 1. Code drift

The cited code has diverged from what the ADR describes.

- **Strong**: "ADR says replay uses `runtime.NodeExecutor`
  (path: `pkg/botreplay/verify.go:42`), but the cited file now
  also calls into `pkg/runview` at line 88 — the seam moved."
- **Weak**: "The code might no longer match the ADR."

### 2. Dependency drift

A dependency the decision depends on has been bumped, removed, or
materially changed.

- **Strong**: "ADR (dated 2026-05-29) chose `claw-code-go` v0.4.x
  for the executor seam; `go.mod` now pins v0.7.2, which added
  `WithClientFactory` — the very option the ADR rejected as
  'doesn't exist'. The Rejected Alternative 1 is now feasible."
- **Weak**: "Newer versions of dependencies are usually better."

### 3. New alternative matured

A technology, library, or pattern that was speculative or
unavailable at the ADR's date has shipped a stable version.

- **Strong**: "Codex Sessions SDK shipped a `--replay-input` flag
  on 2026-06-01 (release notes `https://...`); the ADR (2026-05-29)
  rejected this alternative because the flag didn't exist."
- **Weak**: "There are probably newer/better tools for this now."

### 4. Triggered consequence

A consequence the ADR predicted has fired, and the predicted
mitigation hasn't held.

- **Strong**: "The ADR's Consequences listed 'schema drift = loud
  failure'. `git log` shows three back-to-back `task test:goldens`
  failures in the last week (commits a1b2c3, d4e5f6, g7h8i9) — the
  loud-failure mitigation is now noise the operator is suppressing."
- **Weak**: "Some consequence is probably triggered by now."

## Weak signals (do NOT raise as a case for change)

- Personal preference ("I would have chosen X").
- Generic age ("the ADR is from a year ago, things probably changed").
- Hypothetical futures ("X might be better when feature Y ships").
- Re-litigating an explicitly-considered alternative without new
  evidence (the ADR already weighed it).

## Writing the three cases

In `frame_arguments`, produce three short cases (3-6 sentences each):

### case_for_keep

When in doubt, this is the most honest case. Tactics that work:

- Cite the absence of drift: "the survey found zero changed lines
  on the cited surface since {{adr.date}}".
- Cite the trade-offs from the ADR's own Trade-offs section if
  they still apply.
- Note that the decision's known consequences haven't been
  triggered.

### case_for_change

Only write substance here when at least ONE strong signal exists.
Otherwise: `"(no signal worth a change proposal)"`. When raising
substance:

- Lead with the strongest signal type (drift > triggered consequence
  > matured alternative > dependency drift, when prioritising).
- Cite the evidence (path / version / commit / date).
- Be specific about WHAT decision should be revisited: "swap the
  executor seam to runtime-driven replay" — not "rethink replay".

### case_for_addendum

The middle path matters when the decision is fundamentally sound
but the survey surfaced a fact the historical record should
reflect. Examples:

- "Confirm the decision; note that the Rejected Alternative 1
  is now feasible but we still prefer the chosen seam because
  reason Y."
- "Confirm the decision; note that the predicted mitigation Z
  was overridden by intentional change W (link the commit)."

The addendum is a 2-4 line note, not a new ADR. If the framing
case is longer than that, the human should probably pick `change`
and let a downstream pass propose a successor ADR.

## summary field

One or two sentences naming the strongest signal, or "no signal"
honestly. The studio shows this prominently — it's the hook for
the human's attention before they read the three cases.

## The human is the safety net

The framing skill is honest, the human gate is the second line of
defence. A weak case slips past the bot's discipline only into a
human inspection — the human reads the rationale, sees no
citation, and picks `keep`. Both layers must do their job.
