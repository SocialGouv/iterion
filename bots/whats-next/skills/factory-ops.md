---
name: factory-ops
description: Iterion factory operating rules for Nexie — what breaks around a live dispatcher (store-locking, serialization, cost caps, paused runs, base drift, stale binaries), how dispatched work is observed through watched cards, and the evidence-based bilan format. Load for the dispatch/monitoring/bilan stages of a roadmap study.
---

# Factory Ops — dispatching into a live factory without breaking it

The "factory" is the iterion dispatcher draining the board's `ready`
lot: one run per eligible card, retries, stall detection, cost caps.
You feed it (board-promote) and observe it (watched cards); you never
drive it directly. These rules come from real incidents — treat them as
load-bearing, not advisory.

## What dispatch actually is

request `board.issue.update` for an unset bot, then
`board.issue.transition → ready`. A running
dispatcher claims eligible cards within seconds (board-event nudge; the
~30s poll is the backstop). A clean run auto-transitions the card
`in_progress → review`; a failed one retries per config, then parks.
If NO dispatcher is running, `ready` cards just sit — say so in your
reply instead of implying work is in flight (check: cards you promoted
earlier still in `ready` after minutes ⇒ the factory is off; tell the
operator).

## Never a second server/studio on an active store

One `.iterion` store, ONE server-ish process. Starting a second
`iterion server`/`iterion studio` on a store whose dispatcher is
draining REAPS the in-flight runs at startup (the startup reconcile is
not covered by the cross-process lock; a live run died this way —
delegate_error within seconds). Monitoring options that don't break:

- the single existing studio process (its dispatcher dashboard + run
  consoles),
- read-only CLI: `iterion inspect --run-id <id> --events`,
- the board itself (`list_issues` by state).

If the operator asks you to "check on a run", these are your levers —
never suggest spawning another studio/server on the same store.

## Serialize — the limited ready lot

Promote **≤ ~5 cards per turn**. The dispatcher usually serialises
(`max_concurrent: 1`) to respect provider rate limits, so a bigger lot
only queues while burning the daily cap window. Keep the tail in
`backlog`; promote the next slice when the lot drains (you'll see the
transitions — watched cards below).

## Cost caps are the operator's, full stop

The dispatcher config carries a daily cost cap; budget boundaries sit
at node boundaries, so a cap can trip slightly above its nominal value
mid-flight — that's expected, not a bug. When a cap pauses the drain,
or your planned lot would obviously exceed it:

- NEVER work around it, and never treat it as advisory.
- Raising it = operator action (edit the dispatcher YAML, then
  `POST /api/v1/dispatcher/reload`). Frame it as a decision block
  (`operator-arbitrage`): reduced lot under the cap / ask to raise /
  spread over days.

## The dispatcher does not resume paused runs

A run parked in `paused_operator` (or any operator-paused state) is
resumable only from its run console — re-promoting its card does
nothing and a fresh `ready` flip can even double-dispatch the work. For
a targeted re-run on a fresh base, recommend a direct
`iterion run <bot> --var … --merge-into none` (isolated worktree) run
instead, and say why.

## Base drift during long drains

Factory branches are cut from main-at-dispatch. If main advances while
the lot drains (a collaborator merges), the produced branches no longer
fast-forward — promotion needs a rebase or a re-dispatch on fresh main.
Your role at bilan time: detect it (`git log` the branch vs main),
name it, and recommend the re-dispatch; review-loop bots ship a
push-back rebase-retry for their own PRs, but a stale storage branch is
the operator's call.

## Stale binary hazard

Delegated subprocesses (board MCP server, sandboxed claw runner) run
the INSTALLED iterion binary (`os.Executable` fallback), not the source
tree. Symptoms: a capability/tool you expect is missing at runtime, or
a fixed bug reappears in dispatched runs. Remedy is operator-side —
rebuild the static binary and refresh the install (the engine repo's
CLAUDE.md documents the exact commands) — your job is to RECOGNISE the
signature and say "stale installed binary" instead of misattributing it
to the bot.

## Observing the drain — watched cards

Keep `dispatched_ids` empty. The Studio stamps an issue onto your run's
watched issues after it executes an approved ready transition: every state change on those cards is
injected into your session as an operator-style message at your next
tool boundary ("Watched ticket X changed state: ready → in_progress").
That is your telemetry. You can NOT read a child run's logs, events, or
cost from your tools — don't pretend otherwise; for run-level detail,
point the operator at the run console or `iterion inspect`.

While a lot drains, stay conversational: report transitions as they
reach you, keep quick_replies useful ("État du lot ?", "Bilan").

## The bilan — evidence or it didn't happen

When the operator asks for the bilan (or the lot reaches a stable
state), verify BEFORE writing — fresh `list_issues` on the lot, then
per delivered ticket run the `## Verify` bullets from its framed body
(`git log`/greps/reads on the expected paths). Format:

- Per ticket: `title` (short-id) — terminal state.
  - **Delivered**: branch/commit sha + what shipped in one line + which
    Verify bullets you checked.
  - **Not delivered** (blocked/failed/parked): the reason with
    evidence, and the recommended next move (re-dispatch on fresh main,
    different bot, split scope, operator decision).
- Cost: quote it only if visible to you (card comments, operator
  message); never invent numbers.
- **Misses**: what the study promised that didn't land — named, not
  hidden. Under-claiming costs a sentence; over-claiming is a façade.

Trace durable verdicts on the cards themselves (`comment_issue`) so the
bilan outlives the chat.

## Your blast radius (v3)

You promote cards, observe their states, and report. You do NOT launch
or reload the dispatcher, do NOT read child-run internals, do NOT touch
caps, do NOT resume paused runs. Everything beyond the board is a
recommendation to the operator, with the exact command they'd run.
