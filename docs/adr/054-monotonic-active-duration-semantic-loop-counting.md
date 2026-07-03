# ADR-054 — Monotonic active-duration + semantic loop counting

Status: **accepted** (2026-07-03; shipped `85ea12d7f`).

## Context

The studio's run console showed a run's "duration" and per-node "iteration"
counts. Dogfooding `whole_improve_loop` (run `019f2247`) surfaced two skews
that made a run's telemetry actively misleading:

1. **Active-duration counted OS suspend.** `active_duration_ms` was derived in
   the runview reducer by summing wall-clock **event-timestamp** windows
   (`run_started/resumed → paused/failed/interrupted/finished`). When the host
   was suspended overnight (23:56→05:42, a 5h46m gap with no events, the
   process frozen by the OS), that gap fell *inside* a resume→failure window
   with no `run_paused` event, so the reducer kept accruing it. A run with ~9h
   of real work displayed **~15h**. A naive fix (a periodic heartbeat that
   subtracts large inter-event gaps) was rejected: it would wrongly subtract
   legitimate long LLM thinking.

   The engine already had the correct number: `SharedBudget.Snapshot()`
   measures elapsed with Go's **CLOCK_MONOTONIC** (`time.Since`), which
   **freezes during OS suspend but advances during LLM thinking** — exactly
   the right semantics — and is preserved across resume via `Restore`
   (`time.Now().Add(-elapsed)`). The display simply wasn't using it.

2. **Iteration display was an off-by-one.** Logs label a node `claude_reviewer#48`
   from `task.Iteration` (the semantic loop counter `review_loop.iteration`).
   The studio `IterationCrumb` showed `position/total` from the node's
   **physical execution array**, which includes resume RE-executions (a resume
   from a mid-iteration checkpoint re-runs that iteration), so the UI drifted
   above the semantic counter.

## Decision

- Surface the engine's monotonic active-elapsed as the **authoritative**
  display value. `Event.ActiveMs int64` (new, `omitempty`) is stamped on every
  event at `AppendEvent` time via `SetActiveDurationFn` — the exact twin of the
  `LogOffset` / `SetLogPositionFn` pattern — returning the run's
  `SharedBudget` monotonic elapsed in ms. The runview reducer adopts `ActiveMs`
  as the `ActiveDurationMs` base (max-guarded); the wall-clock event-window
  summation stays **only** as a fallback for pre-fix events (`ActiveMs == 0`).
  Cross-store + cloud-runner parity: the Mongo store and the cloud runner wire
  `SetActiveDurationFn` the same way (the runner owns the per-run engine, so it
  registers the setter, twin of its log-writer wiring).
- The studio shows the **semantic** `loop_iteration` (matching the `node#N`
  log label) instead of the physical execution index; the execution position
  moves to a tooltip. A run-level `⟳ <loop> current/max` indicator (sourced
  from `iteration_path` max — resume-dedup-safe — and `run_started` bounds)
  gives a "real loops" count distinct from any per-node execution count.

## Consequences

- Run telemetry is trustworthy: OS suspend excluded, long thinking counted,
  resume-safe, with no heuristic threshold.
- A new persisted event field (`ActiveMs`), backward compatible: old runs with
  `ActiveMs == 0` render via the wall-clock fallback.
- **Limitation:** budget-less workflows produce a nil `SharedBudget`, so
  `ActiveMs` stays 0 and those runs use the wall-clock fallback (documented at
  `Event.ActiveMs` / `Engine.activeBudget`). The target scenario — overnight
  loop bots — all declare budgets.
- This telemetry was the precondition for diagnosing the loop-bot yield
  problem behind [ADR-055](055-unit-convergent-adaptive-improve-loop.md):
  without it the waste read as "15h / 49 iterations" and was invisible.
