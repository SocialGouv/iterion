# Waiting for engine state in tests

Issue #930 separates the condition a test asserts from how quickly its host can
run git, a shell, or a filesystem operation. A fixed 1.5–120 second timeout
around that work was also measuring unrelated host load.

## In-process workflows

The runtime fan-out/resume-barrier regressions and the runner's parked human
gate run inside `testing/synctest.Test`. Go's virtual clock advances when the
bubble's goroutines are durably blocked; CPU scheduling and filesystem work do
not spend the test's virtual deadline. Existing cancellation-grace and lost
barrier bounds still detect a workflow that cannot progress.

The runner human-gate test explicitly waits for quiescence, checks that the
child persisted `paused_waiting_human` and the parent has not returned, then
cancels the parent and joins the result. It no longer needs to spend 1.5 real
seconds proving that a human has not answered.

## Real-process service fixtures

Service launch, resume, subbot control, reconciliation and restart tests keep a
real clock. `waitForSubbotStatus` observes the persisted state and reports a
terminal child or parent immediately. `awaitRunCompletion` joins the service's
Done channel. Neither predicts how long creating a worktree or running a shell
should take.

`runWaitContext` bounds one wait by the EARLIER of two figures (`runWaitTimeout`
derives them; its own unit test pins the edge cases). First, a per-operation
ceiling — `30s * waitSlowdown`, the package's existing scale-up of the figure
these waits carried. `t.Deadline()` alone would not do: it is one absolute
instant for the whole test BINARY, so a single wait that never satisfies would
spend the package's entire remaining `-timeout` (~10 min on the unit job's
default, ~30 min under the race job's `-timeout 1800s`) and hand every test
scheduled after it a near-expired deadline. Second, what is left of the harness
deadline minus `waitDeadlineMargin` (30 s), so the wait fails as its own named
assertion — with the run goroutine joined and the `t.TempDir` removable — rather
than as a package-wide harness panic naming whichever test was in flight. A
deadline already inside that margin yields a non-positive bound, i.e. an
already-expired context: the caller reports immediately instead of waiting past
the harness. `go test -timeout=0` has no deadline and keeps its meaning: no
wall-clock ceiling at all. Polling every 50 ms is an observation cadence, not an
execution budget.

`runWaitTimeout` is the package's ONE wait policy: the older `waitBudget`
(scale a caller's hand-picked figure, clamp it to the harness) lost its last
`pkg/runview` caller here and was removed with it, keeping only its two
constants. The `e2e` package keeps its own copy — it is a separate package with
live callers.

Do not wrap these real-process service fixtures in synctest: external process
I/O and their polling/background workers do not provide the same durable-block
contract as the in-process fixtures.

## Nearby wait audit

The service test sweep included `time.After`, `context.WithTimeout` and
`time.Now().Add`; not every duration is an estimate of engine speed.

| Test family | Disposition |
|---|---|
| `subbot_human_gate`, `subbot_reconcile`, `subbot_child_control`, `subbot_restart` | Persisted child-state and joined-run waits use the operation ceiling clamped to the harness deadline. The explicit ten reconciliation passes remain the reconciliation oracle. |
| `service_launch_{budget,dispatch_fields,loop_budget,pause}` | Completion waits use the operation ceiling clamped to the harness deadline. |
| `service_resume_{budget,hash,snapshot}` | Completion waits use the operation ceiling clamped to the harness deadline. |
| `broker`, `service_hook_observers` | Retain bounds on event delivery; no git/engine setup in the wait. |
| `file_event_source`, `file_log_source`, `service_eventsource`, `service_stream` | Retain file-tail/event delivery bounds and deliberate no-event observations. |
| `manager`, `service_drain`, `reconcile_shutdown`, `service_stop_background` | Retain shutdown, cancellation and background-worker lifecycle contracts. |
| `periodic_reconcile`, `reattach` | Retain bounded observations of reconciliation/timer behaviour in their separate fixtures. |
| `merge_claim`, `service_runs_skip_log`, `service_test` | Backdated timestamps construct old records; they are not wait deadlines. |
| `subbot_{restart,child_control,human_gate,reconcile}` service teardown | `stopService` keeps the separate 30-second shutdown budget (`waitDeadlineMargin`); it bounds service teardown, not child workflow progress. A `context.Background()` there is unbounded: `Manager.Stop`'s `ctx.Done()` arm is nil and can never fire, so a wedged run goroutine blocks teardown forever. |

## Falsification performed

With a temporary two-second real subprocess delay immediately before the runner
starts its child engine, the old human-gate test failed after 2.06 seconds; the
new test passed after 2.07 seconds. The delay was removed afterward.

Changing the park to use the already-cancelled child context made the new test
fail with `park returned before the parent was cancelled`. Removing the resume
barrier release made both each/all error-path regressions fail with `resume
hung`. Those production mutations were also removed: this change contains only
tests and this wait audit.

`TestFanOutAbandonedBranchDoesNotRaceRunState` states its value as "running
exactly this interleaving under the CI `-race` job", and virtual time elapses
its 100 ms `branchCancelGracePeriod` the instant the bubble blocks durably — so
the conversion was falsified on the oracle itself, twice, under
`go test -race -count=3`:

- Removing the retired-epoch guard (`parallelExecutionState.updateBranch`'s
  `if p.retired { return false }`, the ADR-095 write the final assertion
  covers) failed the converted test with six `DATA RACE` reports.
- A deliberately unsynchronized probe — written by the main loop in `finalize`
  right after it releases the wedged branch, read by that branch as it wakes —
  was reported as a race, naming the abandoned branch's goroutine through
  `launchBranches` → `execBranch` → `executeNodeForBranch`. The overlap the
  test exists for is therefore still real inside a bubble: synctest virtualizes
  the clock, not goroutine scheduling.

Both mutations were removed. Two earlier candidates did NOT discriminate
(sharing the trunk's loop-counter map into `enclosingLoopCounters`; the main
loop writing `rs.vars` on the loop-edge traversal) — they are simply not on the
abandoned branch's read path, in either the synctest or the pre-conversion
shape, so they say nothing about the conversion.
