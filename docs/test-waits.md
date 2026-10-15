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
| `broker`, `service_hook_observers` | No bound: the fact is already true when the select is entered. `Publish` fans out with a non-blocking send performed by its caller, and `Cancel`/`CloseRun` close the subscriber channel under the write lock, so each is read with a `default` (see the two recipes below). |
| `file_event_source`, `file_log_source`, `service_eventsource`, `service_stream` | Retain file-tail/event delivery bounds and deliberate no-event observations. |
| `manager` | No real-clock bound left: the waits run in a `testing/synctest` bubble and assert exact virtual figures, and what a `Cancel`/`Deregister` leaves behind is read with a `default`. |
| `service_drain`, `reconcile_shutdown` | Retain shutdown and cancellation contracts: the budget handed to `Drain`/`Stop` IS what they exercise, and the facts they wait for come from a spawned goroutine. |
| `service_stop_background` | `StopBackground` awaits the reconcile goroutine, so that `done` is read with a `default`. `pipelineStop` is only the scheduler's stop SIGNAL — `StopBackground` closes it itself — so the `default` there witnesses the signal and the WORKER is witnessed by its goroutine frame, as the alert manager's is: a poll-until-settled, not a bound. |
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

## Two recipes for a bound a scheduler can decide

#1471 asks a liveness bound to keep >= 10x headroom over the legitimate
latency, or to be replaced by a synchronisation point. These two shapes are
what "replaced" looks like in practice; #2362 and #2447 applied them to the
last tight bounds of `pkg/runview` and the duration-deadline spend witness.

**An in-process lifecycle witness reads VIRTUAL time.** `pkg/runview`'s
`Manager` is pure in-process state, so every hand-off test that reads a WAIT
runs in a bubble and asserts an exact figure: a refusal that must not pay the
grace spends `== 0`, a wait for the previous runner `== previousRunnerTeardown`,
a caller whose deadline expires `== callerBudget`, a `Stop` that drains
`== runnerTeardown`. The production grace then costs the suite nothing, so the
test exercises the real `registerHandoffGrace` instead of shortening it. The
facts that need no wait at all — a context closed by `Cancel`, a `done` closed
by `Deregister` — are read with the `default` recipe below instead, outside any
bubble. Two traps:

- A `t.Fatalf` while a goroutine the bubble spawned is still blocked exits the
  main bubble goroutine with a blocked one behind it, and synctest reports
  `deadlock: main bubble goroutine has exited but blocked goroutines remain` —
  a panic that aborts the test BINARY. The failing assertion is still printed;
  what is lost is every test after it (measured on `pkg/runview`: 247 of 739
  tests ran, 1 failure reported instead of 3). Buying that back takes two
  separate things, and a join alone is not one of them. First, a goroutine the
  bubble spawns must be unblockable WITHOUT the test's cooperation —
  `releaseAfter`'s only block is a `time.Sleep`, which virtual time always
  completes — or carry an escape the test owns, like the `release` channel
  `TestManager_StopDrains` closes in its defer, after the last assertion so it
  can never decide one. Second, it must be joined by a receive the test
  DEFERS, so the join runs on the Goexit path too: `releaseAfter`'s `released`
  (which the goroutine closes, hence a join and never an escape), StopDrains'
  `gone` per run. Measured on this tree, each shape under a failing `t.Fatalf`:
  the deferred join ALONE over a goroutine blocked on a channel trades one
  binary-aborting panic for another — `deadlock: all goroutines in bubble are
  blocked`, and the next test in the file does not run — while escape + join,
  and virtual sleep + join, each report the failure as their own and let the
  next test run.
- `time.After` racing a fact that is already true is a verdict the host
  decides: a select picks at random among the arms that are ready, and a second
  of starvation makes the timer one of them (measured on a ready timer against
  a closed channel: the timer wins 500/1000). Such a fact is read with a
  non-blocking select and a `default`, or joined with `synctest.Wait` — never
  with a timer. "Already true" is a claim about the PRODUCER: a non-blocking
  send the test's own goroutine performed, a `close` under a lock the callee
  took, a worker the callee awaited before returning.

**A duration-budget witness owns the budget's clock.** A short `max_duration`
is two things at once: how long the node under observation is HELD, and a race
against everything upstream of it — the engine's construction, the store, the
entry node. `expireDurationBudget` (`pkg/runtime/budget_deadline_test.go`)
separates them: the budget runs on `budgetTestClock` through
`Engine.budgetClock`, frozen, so the pre-exec gate always sees the full
allowance and the node is always reached and always armed; the node itself
spends the cap once it has started, which is what makes the engine's post-exec
verdict read a budget genuinely out of time. The per-node deadline is
untouched — it is still armed from the remaining budget and still kills the
node on the real clock — and the fixture reports `started`/`armed` so a premise
that never held fails as itself.

Falsified both ways, with 400ms of upstream work standing in for a loaded
merge-queue runner: the pre-change spend witness fails with the queue's own
message (`the timed-out node's tokens never reached the run: 0`), and the
pre-change `TestNodeDeadlineFromDurationBudget` PASSES with
`context.WithDeadline` removed from `engine_exec.go` — the regression it guards,
green. Converted, both are green 3/3 under the same load and both go red in
0.03s on that mutant, where the old witness only reached the harness timeout.
