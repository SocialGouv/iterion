# ADR-009: Per-day LLM spend cap — shared ledger + pre-exec pause, not a new run status

- **Status**: Accepted
- **Date**: 2026-05-30
- **Authors**: devthejo
- **Code context**: [`pkg/clock/`](../../pkg/clock/),
  [`pkg/store/spend.go`](../../pkg/store/spend.go),
  [`pkg/runtime/spendcap.go`](../../pkg/runtime/spendcap.go),
  [`pkg/runtime/budget.go`](../../pkg/runtime/budget.go) (`checkBudgetBeforeExec`,
  `recordAndCheckBudget`) and [`pkg/runtime/pause.go`](../../pkg/runtime/pause.go)
  (`handleCostCapPause`),
  [`pkg/dispatcher/loop.go`](../../pkg/dispatcher/loop.go) (`refreshCostCap`),
  [`pkg/server/limits.go`](../../pkg/server/limits.go)

## Context

We needed a per-project daily LLM spend cap: track cumulative spend per
`(project, day)`, pause every running run plus the dispatcher when the cap
is crossed, offer a one-click "override for today" (logged), and auto-reset
at the next day. Several design choices had non-obvious trade-offs that the
code alone would not justify.

### Decisions and the alternatives rejected

**1. "Project" = the RunStore root (per store-dir), not a git-repo project key.**
A dispatcher owns one store-dir and studio/CLI share `~/.iterion`. Keying
the ledger on the store root means we never have to stamp a project key on
every run (the session-continuity `~/.iterion/projects/<key>` scheme would
have required threading a derived key through `CreateRun`, resume, and the
dispatcher). The cost: two different git repos that deliberately share one
store get a single combined cap. That is acceptable for the common
one-store-per-project layout and is revisitable behind the same
`SpendStore` interface if per-repo separation is ever needed.

**2. The cap pauses with the existing `paused_operator` status, not a new
`paused_waiting_human` sentinel.** The feature brief said
"`paused_waiting_human` with a sentinel reason", but that status expects
human *answers* on resume and would need special-casing so the
override/next-day path resumes without questions. `paused_operator`
already means "soft, resumable, no answer required, resumes via the
cancellation-style restore path" — exactly the cap's semantics. We reuse
it and put the sentinel in the `run_paused` event's `reason` field
(`cost_cap_daily`). Critically, we also **reuse `ErrRunPausedOperator`**
rather than minting a new error sentinel, so every existing resumable-pause
handler (runner loop, resume dispatch, dispatcher retry classification)
treats a cap pause correctly with zero new wiring. A new sentinel would
have silently fallen through those `errors.Is` branches and been
misclassified as a failure.

**3. Record spend post-exec, decide the pause pre-exec — split across the
two existing budget hooks.** The per-run budget already has this exact
shape (`checkBudgetBeforeExec` + `recordAndCheckBudget`). We fold the cap
into the same seam: `recordAndCheckBudget` writes the run's cumulative cost
to the shared ledger (no pause decision), and `checkBudgetBeforeExec`
reads the ledger and pauses *before* a not-yet-executed node. This anchors
the checkpoint at a node that hasn't run, so resume re-executes cleanly
instead of double-running the node that tripped the cap. The cap check is
deliberately placed *before* the `rs.budget == nil` early-return so it
works for workflows that declare no `budget:` block.

**4. "Pause every running run" is achieved through the shared ledger, not
a fan-out signal.** A single engine can only pause itself. Rather than
build a control-plane that reaches into every run's pause channel, each run
re-reads the shared `<store>/spend/<day>.json` ledger at its next node
boundary and self-pauses when the day is over cap. One run tripping the cap
therefore pauses all the others within one node each (eventual, not
instantaneous), and the dispatcher's `refreshCostCap` gate stops launching
new work. The trade-off — up to one extra node of spend per in-flight run
before it notices — matches the already-documented "soft enforcement" of
the per-run budget and avoids a much larger synchronous-pause mechanism.

**5. Idempotent accumulation by cumulative-per-run, not deltas.** The
ledger stores `runs_contributed[runID] = that run's latest cumulative cost`
and recomputes the day total as the monotonic sum. A resumed or
re-executed node re-records its run's cumulative (overwriting, never
adding), so restarts and resumes cannot double-count. Delta-based
accounting would over-count on every resume.

**6. `SpendStore` is an optional interface (`AsSpendStore`), not a method on
`RunStore`.** Adding the three ledger methods to the `RunStore` interface
would have forced the cloud Mongo store to implement them. The daily cap is
a local-mode feature today, so we follow the established `TurnStore` /
`ToolBlobStore` optional-interface pattern: only `FilesystemRunStore`
implements it, and a nil `SpendStore` cleanly disables the cap.

**7. A `Clock` abstraction was introduced (`pkg/clock`) solely so the
day-boundary reset is testable.** The codebase otherwise calls
`time.Now()` directly; we did not refactor that. The cap engine takes a
`Clock` so tests advance a `FakeClock` across UTC midnight and assert the
reset deterministically.

## Consequences

- Enforcement is **soft**, inheriting the per-run budget's characteristics:
  parallel branches and the last node of the tripping run can overshoot by
  a bounded amount. This is documented, not a bug.
- The run-side limit is configured via `ITERION_MAX_COST_PER_DAY_USD` (env)
  or `runview.WithDailyCostCap`, and the dispatcher via its
  `limits.max_cost_per_day_usd` config block — two surfaces, one
  `DailyCapGuard` enforcement engine. (The brief's "global config `limits:`
  block" was reduced to an env var because `pkg/config` is the cloud
  control-plane config, not the path studio/CLI runs read; documented as a
  deviation in the implementation.)
- Spend is under-counted for models absent from the pricing table
  (`cost.EstimateUSD` returns 0), so the cap can under-fire — same
  limitation as the existing budget.
- Cross-process writers (dispatcher + a separate `iterion run`) reconcile
  via atomic read-modify-write under the store mutex with last-writer-wins;
  this is single-host-safe but not a distributed lock.
- Override is audit-logged in the ledger itself (`granted_by` /
  `granted_at` / `note`) and auto-clears when the UTC day rolls over.

## Addendum — 2026-05-30 (review fixes)

Two enforcement gaps in the initial implementation were caught in review
and fixed; both made the cap silently under-fire on real surfaces.

1. **Dispatcher-launched runs were not enforced.** `EngineRunner.Dispatch`
   (`pkg/dispatcher/engine_runner.go`) built the engine without
   `runtime.WithDailyCap`, so dispatcher runs neither recorded spend into
   the ledger nor self-paused. The `refreshCostCap` gate therefore read a
   ledger that dispatcher activity never wrote to and never tripped —
   despite `limits.max_cost_per_day_usd` being a *dispatcher* config field.
   Fix: `buildSpec` now attaches a `DailyCap` guard (new `DispatchSpec.DailyCap`
   field) built from the dispatcher's **singleton** `SpendStore`
   (`Dispatcher.newDailyCapGuard`), and `Dispatch` wires it via
   `WithDailyCap`. Using the one shared store instance across every
   concurrent dispatched run is load-bearing: all ledger read-modify-writes
   serialise on a single mutex, so concurrent runs can't clobber each
   other's `runs_contributed` entry (a per-run `store.New()` would race on
   `<store>/spend/<day>.json`).

2. **Fan-out branch spend escaped the ledger.** Decision #4's "pause every
   running run via the shared ledger" only covered the trunk: branch nodes
   recorded usage to `rs.budget` but never to the daily ledger, so all
   spend inside `fan_out_all` branches (common in catalog bots' parallel
   review) was invisible to the cap — not the bounded overshoot the
   original Consequences claimed, but *uncounted entirely*. Fix: the branch
   executor (`pkg/runtime/fan_out.go`) now records each branch's cumulative
   spend into the ledger under a **per-branch key** (`<runID>#<branchID>`).
   Because `AddSpend` sums across keys (and is monotonic-max within a key),
   concurrent branches aggregate correctly and stay idempotent on resume —
   recording under the bare `runID` would have let branches clobber one
   another. The pause decision still happens on the trunk's pre-exec path,
   so the bounded-overshoot characteristic in Consequences still holds; it
   is the *accounting* that is now complete.
