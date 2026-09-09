# Workflow reliability rollout (#1006)

This document is the compatibility ledger for the reliability work. Each
tranche is independently deployable and is stacked on the previous one; an
operator can stop at any boundary without rewriting existing run records.

## Context contract (tranche A)

Every newly created in-process run carries `execution_context.version = 1`.
The record identifies:

- the Iterion run store (`run_store`), separately from business stores;
- the declared workspace isolation mode and logical workspace id;
- the compiled workflow revision, source root and optional bundle revision;
- root/parent run and parent-node lineage;
- the policy (`legacy`, `report`, or the future `enforce` gate).

The record contains identities and revisions only. It never stores credentials,
prompts, model output or business payloads. Older run documents omit the field
and remain readable; resume keeps the legacy behaviour until an admission
policy is explicitly enabled.

## Launch-surface matrix

| Surface | Context persisted | Admission owner | Planned tranche |
| --- | --- | --- | --- |
| local/API in-process launch | yes | runview service | A/B |
| local resume | existing context preserved | runview + runtime | B/C |
| detached launch | compatibility stamp | detached runner | B |
| cloud queued launch | queue/document propagation | publisher/runner | B |
| subbot child | parent/root reconciliation | runview subbot runner | B |
| watcher/Copi observation | diagnostic projection | watcher authority | F/G |

This matrix is intentionally explicit: a green unit test in one launch path is
not evidence that a queued or nested launch has the same context.

## Bounded invalid-output correction (tranche D)

Schema-invalid output is corrected only when the executor implements the
optional `runtime.OutputCorrector` capability. The engine allows at most the
configured budget (two calls by default, or `WithOutputCorrectionBudget(0)` to
disable it) for each node episode. Artifacts, events and outgoing edges are
written only after a corrected payload validates.

The episode ledger is persisted on the run document. It records the attempt
count, output/violation fingerprints and a terminal status (`succeeded`,
`exhausted` or `unchanged`). A corrector returning the same invalid payload is
stopped immediately, and a resume continues the existing budget rather than
starting a new loop. Executors must keep correction side-effect free; publish
and external-effect nodes remain behind the validation boundary.

## Coordinated retries and circuit breaker (tranche E)

Cloud runners retain the per-run retry budget, but usage-window failures also
update a tenant-scoped durable circuit keyed by workflow revision. Once the
shared failure threshold is reached, the next retry wave is delayed until the
breaker cooldown instead of starting one pod per run against the same provider
wall. A successful run clears the streak. The circuit is an optional Mongo
capability; local/filesystem stores keep their existing retry behaviour, and a
circuit-store outage falls back to the durable per-run retry rather than
dropping work. Threshold and cooldown are controlled by
`ITERION_RETRY_CIRCUIT_THRESHOLD` and `ITERION_RETRY_CIRCUIT_COOLDOWN`.

## Progress-sensitive watchers (tranche F)

Supervisors keep a durable cursor per `(run, supervisor, watched-node set)` on
the run document. The cursor stores the last progress fingerprint, the trigger
fingerprint that was last consumed, the last action, the evaluation window and
the run's completed evaluation count. The watch set is canonicalised (sorted,
deduplicated) before it is hashed, so reordering `watches:` — or the `iterion
supervise --node` flags — addresses the same cursor rather than silently
minting a fresh one.

What it buys:

- **A redelivered event cannot bypass the cooldown.** The trigger fingerprint
  is `(wake reason, last progress sample)`, so the same evidence never earns a
  second evaluation, even on a high-signal monitor wake that bypasses the
  ordinary cooldown. New progress produces a different fingerprint and stays
  eligible immediately.
- **A restart cannot re-enqueue a correction it already sent.** The consumed
  trigger is written *before* the steering message is enqueued, not after —
  the inbox append is durable and un-deduplicated, so the other ordering left
  a window where a crash replayed the correction. The reverse exposure is one
  intervention, which the next event re-earns.
- **`max_evals` caps the run, not the process.** The completed evaluation
  count rides the cursor, so a resumed run or a redeployed pod continues
  spending the same budget. Consecutive evaluator failures and the bot's
  `done` flag deliberately still reset on a restart.

Failed evaluator calls clear only the trigger fingerprint (the signal was not
consumed) and remain bounded by the existing consecutive-failure cap. The
cursor write is issued on a context detached from the coordinator's own
cancellation — otherwise the last write before a pod goes down, the one a
restart depends on, would be dropped by any store that honours `ctx` — and
every way it can fail is logged rather than swallowed.

Launch surfaces opt in through the `WatcherProgressStoreProvider` capability
method: `runview.Service` (studio, `iterion supervise --run-id`) and
`supervise.StoreInjector` (CLI `run`/`resume`, the dispatcher's engine runner,
and the cloud runner pod) both expose one, so the cursor is present where a
restart matters most. Observers with no run store behind them — the raw
`claude` session attach — keep the previous in-memory cooldown and a
per-process eval budget.

One bound worth knowing: the trigger-fingerprint half of the suppression needs
an observer that replays the run's history, because the restored fingerprint
has to be re-derived from the same last event. `runview.Service.ObserveRun`
replays from `events.jsonl` and dedupes by seq; `supervise.EventHub` is
live-only, so after a restart there it is the restored *cooldown* that holds
until the next event arrives.
