# ADR-014: a dispatched run that pauses for input is parked, not retried

- **Status**: Accepted
- **Date**: 2026-06-02
- **Authors**: devthejo
- **Code context**: [`pkg/dispatcher/commands.go`](../../pkg/dispatcher/commands.go)
  (`finishRun` — the new `ErrRunPaused` / `ErrRunPausedOperator` branch),
  [`pkg/dispatcher/loop.go`](../../pkg/dispatcher/loop.go) (`runWorker` posts
  the dispatch error; `dispatch` takes the claim and transitions to
  `RunningState`), [`pkg/dispatcher/retry.go`](../../pkg/dispatcher/retry.go)
  (`resumableRunID` — paused statuses deliberately stay excluded),
  [`pkg/dispatcher/native/adapter.go`](../../pkg/dispatcher/native/adapter.go)
  (`ListCandidates` returns only **unclaimed** issues; `SweepStaleClaims`),
  [`pkg/runtime/engine.go`](../../pkg/runtime/engine.go) (`ErrRunPaused`,
  `ErrRunPausedOperator`), [`pkg/runner/loop.go`](../../pkg/runner/loop.go)
  (the cloud runner's correct ack-not-retry handling — the asymmetry this
  ADR closes). Related: [ADR-011 dispatcher retry attempt cap](015-dispatcher-retry-attempt-cap.md)
  and [ADR-013 dispatch.attachments unsupported](013-dispatcher-attachments-unsupported.md)
  (same theme — turning a silent dispatcher failure into honest behaviour).

## Context

`runtime.Engine.Run` returns `ErrRunPaused` when a workflow suspends at a
human node (run status `paused_waiting_human`) and `ErrRunPausedOperator`
on an operator soft-pause from the run console (status `paused_operator`).
Both are documented as **not failures** — they leave a valid checkpoint plus
a pending interaction record, and the run resumes via `Engine.Resume`.

The dispatcher had **zero** handling for either sentinel. `runWorker` posts
the dispatch error to `finishRun`, whose `switch` had only three arms:
`err == nil`, `errors.Is(err, context.Canceled)`, and a `default:` that
treated *every other* error as a failure → `giveUpIfExhausted` →
`scheduleRetry`. A pause therefore fell into the failure arm with these
consequences:

- The issue was reverted to its source state and a retry scheduled. The
  retry minted a **fresh** run — `resumableRunID` only resumes
  `failed_resumable` / `cancelled` / `paused_operator`, and
  `paused_waiting_human` is excluded — which re-ran from the top, re-hit the
  same human node, and paused again.
- The cycle repeated until `MaxAttempts`, at which point the issue was moved
  to `FailedState` ("blocked") — or looped forever when no cap was set.
- Each cycle orphaned a `paused_waiting_human` run on disk. The bot's
  escalation *question* was never surfaced as a question; the operator saw a
  misleading "failed: run paused waiting for human input" retry, and if they
  manually answered an orphaned run via the studio, that resume raced the
  dispatcher's next fresh retry for the same issue.

This is reachable on the dispatcher's primary path: the catalog bots it
routes to escalate to humans (`bots/feature-dev/main.bot` declares four
`interaction: human` nodes; `whole_improve_loop`, `secured-renovacy`,
`docs-refresh`, `branch_improve_loop` carry `human` / `llm_or_human` nodes). It
is a genuine gap, not an intended-unsupported case: the cloud runner already
handles it correctly — `pkg/runner/loop.go` acks `ErrRunPaused` /
`ErrRunPausedOperator` as a benign checkpoint rather than naking for
redelivery — and nothing guards against dispatching a human-pausing
workflow (`config.go` does not reject them; `engine_runner.go` wires no
auto-answer resolver). The dispatcher was simply the one consumer that
hadn't been taught the distinction.

## Decision

`finishRun` gains an explicit branch, ahead of the claim release and the
failure `switch`, for `errors.Is(err, runtime.ErrRunPaused) ||
errors.Is(err, runtime.ErrRunPausedOperator)`. A paused run is **parked**:

- **Keep the tracker claim.** This is the load-bearing choice.
  `ListCandidates` returns only *unclaimed* eligible issues, so the retained
  claim — not a state change — is what stops the next tick re-dispatching
  the issue. `RunningState` is itself an eligible candidate state, so
  releasing the claim (the previous unconditional behaviour) would re-pick
  the issue regardless of whether it stayed in `in_progress` or reverted to
  `ready`.
- **Free the concurrency slot** (the `slotsByState` decrement already ran
  above the branch), so a parked run never pins `max_concurrent`.
- **Do not** `scheduleRetry` / `giveUpIfExhausted`, and **do not** revert the
  in-progress transition. The issue stays in `RunningState` — a worker is
  legitimately parked on it, awaiting input.
- **Stamp `last_run`** (already wired) so the issue links straight to the
  paused run and its pending interaction. The operator answers + resumes
  from the run console; the resumed run drives the issue forward on
  completion.

This mirrors the cloud runner's ack-not-retry handling and closes the
asymmetry between the two execution paths.

### Alternatives rejected

1. **Introduce a dedicated `waiting_input` board state** and move the issue
   there with a reason + run link. The cleanest *product* answer and the
   reviewer's framing of the "ideal" fix, but a larger change: a board-schema
   migration (every existing `board.json`), studio column + drag rules, and
   tracker-adapter mapping for GitHub/Forgejo. Deferred as a follow-up; it
   can supersede this ADR. The parking-by-claim decision here is the
   small, safe fix that removes the production harm now.
2. **Release the claim and leave the issue in `RunningState` (or revert to
   `ready`).** Rejected: both states are eligible candidates, so the next
   tick re-dispatches — the exact loop being fixed.
3. **Move the issue to `FailedState` ("blocked").** Rejected: it stops
   re-dispatch but is a lie — the run did not fail, it asked a question.
   "Blocked" is precisely the mislabel the bug produced at the end of its
   retry spiral.
4. **Add `paused_waiting_human` to `resumableRunID` and resume on retry.**
   Rejected: resuming a `paused_waiting_human` run with no answers supplied
   immediately re-pauses (or errors) — it adds churn without progress.
   Resume is the operator's action (with answers), not the dispatcher's.
   `resumableRunID` is left unchanged.
5. **Leave it as-is.** Rejected: a designed-in bot behaviour (escalation)
   degrades into wasted model spend, orphaned runs, a misleading "failed"
   signal, and an eventual false "blocked" — squarely the silent-failure /
   hard-to-reconcile axis under audit.

The non-obvious trade-off: a parked issue **stays claimed**, and the
dispatcher stops tracking it. Two accepted costs follow. (a) Across a daemon
restart the claim is swept (`isStaleLocalMarker` fires once the owning pid
dies). `in_progress` stays eligible (crash-recovery), but the dispatcher
**re-parks** a paused `last_run` instead of minting a sibling from entry —
a planner restarted from `init_film` is not "the same pause". (b)
If the operator resumes the run to completion out-of-band, the parked
sweep (`reconcileParked`) files the card; a resume that re-pauses is
re-parked by `reconcileStrandedPaused`. We chose **claim-as-parking**
over a new state machine because it is correct in-process with no
schema/UI churn; the awaiting-input column and the stranded-pause
sweep closed the original follow-ups.

## Consequences

- A dispatched run that pauses for human input (or an operator soft-pause) is
  no longer retried into "blocked". It stays claimed and in `RunningState`,
  with `last_run` linking to the paused run so the operator can answer +
  resume from the run console. No wasted re-runs, no orphaned paused runs,
  no misleading failure signal.
- `max_concurrent` is unaffected: the parked run frees its slot, so other
  issues keep dispatching while it waits.
- Behaviour for genuine failures and cancellation is unchanged — the new
  branch only intercepts the two paused sentinels.
- Coverage: `TestFinishRun_PausedForInputIsParkedNotRetried`
  (`pkg/dispatcher/commands_paused_test.go`) asserts, across both sentinels
  and the retry/give-up configs, that the claim is retained, no retry is
  scheduled, the issue is not moved to `blocked`/reverted, and the slot is
  freed.
- Open follow-ups (out of scope here, tracked for a later change): a
  dedicated "waiting for input" surface on the board/dispatcher dashboard
  (today a parked issue reads as a normal `in_progress` card), reconciling a
  completed out-of-band resume so the lingering claim is released without a
  restart, and an optional dispatch-time warning when a routed workflow
  contains human-pausing nodes with no auto-answer.
