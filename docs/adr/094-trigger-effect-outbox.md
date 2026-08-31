# ADR-094 — Durable effect outbox for cloud board triggers

- Status: accepted (2026-08-31)
- Relates to: ADR-046 (trigger spine), the board direct-mode + `consume_labels`
  work, the lossy-bus doctrine in [docs/merge-gate.md](../merge-gate.md)
  ("every consumer needs a reconciliation net")

## Context

The cloud board tail (`pkg/server/trigger_cloud.go`) turned `board_events`
rows into trigger launches through four lossy steps in the wrong order:

1. `AdvanceTriggerCursor` (the per-tenant CAS electing the batch's publisher)
   ran **before** `bus.Publish`. A replica crash — a rolling deploy restarts
   the server every release — or a publish error (warn-only) between the two
   lost the whole batch **permanently**: an event the cursor has passed never
   comes back.
2. `NormalizeBoardEvent` collapsed a *transient* Mongo read error into the
   same `false` as a deleted card, so a one-tick Mongo blip silently dropped
   events the cursor then sailed past.
3. Core NATS is at-most-once: a publish with no live consumer, or a handler
   error (swallowed at `Warn` in `Evaluator.Handle`), was a delivered-and-lost
   event.
4. For `consume_labels` subscriptions the evaluator consumed the one-shot
   label **before** launching; a launch failure left the label spent and the
   launch never retried — the trigger's whole one-shot budget burned on a
   publisher hiccup.

Every other consumer of the lossy bus carries a reconciliation net (gate
sweep, usernotify sweep, dispatcher poll). The board-trigger effect path —
the one that *launches bots* — had none, and unlike those consumers it had no
upstream authority to re-offer the work: the cursor is the only memory, and
it had already moved.

## Decision

Replace publish-then-hope with a **durable effect outbox**: one row per
matched `(board event, subscription)` pair, in the per-tenant Mongo
collection `trigger_effects` (`pkg/dispatcher/boardmongo`, implementing
`trigger.EffectOutbox`; `trigger.MemoryEffectOutbox` is the test twin).

**Materialization order** (`cloudBoardSource.drainTenant`):
normalize + match **every** event of the batch first — a transient store
error aborts the batch with the cursor untouched (retry next tick); only a
definitively deleted card (`tracker.ErrNotFound`) is skipped — then upsert
the matched rows (`$setOnInsert`: racing replicas collapse on the row key),
and only **then** CAS-advance the cursor. Every crash point either
re-materializes idempotently or finds the rows already durable.

**Execution** (`trigger.EffectWorker`, ticked by every replica): rows are
claimed atomically with a lease (`FindOneAndUpdate`; an orphaned claim is
reclaimable after `EffectLease`), executed through the same Evaluator
pieces the bus path uses, and retried with exponential backoff up to
`MaxEffectAttempts`, after which the row parks as `failed` — a queryable
dead-letter, warned once. For `consume_labels` rows the worker persists
`ConsumeMarked` on the row **between** the atomic label consume and the
launch, so a launch failure (or a worker crash) retries the launch *without*
re-consuming: the one-shot is spent exactly once and the launch still
happens.

Cloud board events **no longer ride the bus at all** — the outbox is the
delivery. The evaluator's bus subscription remains for the other sources
(run outcomes, forge observational events).

## Delivery semantics (explicit)

- Steady state: exactly-once materialization (cursor CAS + row key),
  exactly-once execution (leased claim).
- Failure recovery: **at-least-once** execution. The effects tolerate it:
  board promote is idempotent (same-bot check), `consume_labels` launches
  are one-shot-guarded by `ConsumeMarked`. Two residual windows are accepted
  and documented: a crash between the label consume and the `ConsumeMarked`
  write can lose one launch (milliseconds, strictly better than the previous
  always-lost shape); a crash between a direct launch and `MarkDone` can
  double one launch.
- The **local** (single-process) path is unchanged: its in-proc bus delivers
  synchronously, the dispatcher poll nets the promote path, and there is no
  cursor whose advance could make a loss permanent. The asymmetry is
  deliberate — the outbox exists to protect the only path where a loss was
  unrecoverable.

## Round-1 adversarial hardenings (2026-09-01)

The first adversarial pass (4 executed-proof reviewers) found and closed
five holes in the initial cut:

- **Upstream seq holes.** `emit` allocates the per-tenant seq ($inc)
  *before* inserting, so seq N can be visible while N−1's insert is in
  flight — and the cursor would sail past N−1 forever. The tail now
  advances only over the **contiguous prefix**: a young gap (watched <
  30s, > boardmongo's 10s op timeout) truncates the batch; an older gap
  is a dead allocation (failed insert) stepped over with a Warn. The
  cursor **seed** reads the max *inserted* seq, not the allocator
  counter, for the same reason.
- **Claim fencing.** `ClaimDue` mints a `claim_id` per claim and every
  `Mark*` filters on it: a worker whose lease was stolen (its batch
  outlived the horizon) finds its late writes no-ops instead of
  resurrecting a done row. The worker also skips rows whose lease
  expired before it reached them.
- **Hung effects burn the budget.** A reclaim of an expired lease
  `$inc`s `attempts`; a row reclaimed past `MaxEffectAttempts` parks as
  the dead-letter — an effect that never returns can no longer be
  re-executed every lease forever.
- **Transient ≠ definitive, one seam lower.** A failing
  `SubscriptionStore.Get` retries; only `ErrSubscriptionNotFound`
  drops. Execution re-verifies `Match` (an operator-edited rule decides
  by its CURRENT terms) and refuses cross-tenant rows.
- **Liveness.** The drain unions the subscription-derived tenant list
  with tenants holding live effect rows (disabling the last
  subscription must not hibernate materialized rows); a head-of-line
  poison event (unreadable after 20 ticks) is skipped with ONE Error
  log instead of freezing the tenant; `done` rows carry a 7-day partial
  TTL (failed rows — the dead-letter — never expire).

## Consequences

- `NormalizeBoardEvent` now returns `(Event, bool, error)` — callers must
  distinguish "card gone" (skip) from "store failing" (abort before the
  cursor).
- `boardmongo.EnsureSchema` adds the `trigger_effects` collection and its
  `(tenant_id, state, not_before)` index. No queue-schema (RunMessage)
  impact; no server/runner ordering constraint — the collection appears on
  first write.
- Failed rows accumulate as dead-letters; an admin surface to list/replay
  them is follow-up work (they are plain Mongo documents until then).
- `cloudsched.ClaimTick` has the same CAS-before-effect shape but a
  different loss profile: a failed launch loses ONE occurrence and the next
  cron tick is the natural retry. That path stays at-most-once per
  occurrence by decision, with the failure warned; it does not get an
  outbox.
