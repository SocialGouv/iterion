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

## Three recipes for a bound a scheduler can decide

#1471 asks a liveness bound to keep >= 10x headroom over the legitimate
latency, or to be replaced by a synchronisation point. These three shapes are
what "replaced" looks like in practice; #2362 and #2447 applied the first two to
the last tight bounds of `pkg/runview` and the duration-deadline spend witness,
#2487 the third to the node-session sweep, #2499 the first to `pkg/eventbus`'s
overrun post-check.

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

**A witness of a sweep's own cadence is HELD in it.** `pkg/internal/proc`'s
node-session sweep scans every `NodeSessionSettle` (50 ms) and ends on two scans
in a row that find nothing, so `TestSweepStopsAProcessThatShowsAfterAnEmptyScan`
had one settle to start a process and have its exec visible: #2487 measured it
losing in a merge-queue run that touched no file of the package, reading the
product as broken (`late (pid 57351) belongs to the session and is still
running`). Both implementations now carry a scan gate the sweep calls after
every scan and BEFORE it acts on what the scan found — `sweepScanGate`, a func
of the members the scan found, for the sweep that runs in-process;
`sweepScanGateFile`, a path the generated sh program appends each scan to and
waits at, for the program a sandbox runs. Both are inert by default (nil, and an
empty `GATE=''` whose `gate()` returns at once), and the program's gate is
bounded: a gate nobody releases prints an `iterion-node-session error` line, so
a held sweep is never read as a sweep that found nothing. EVERY scan is held and
released one at a time, the release naming the scan it frees (`.resume.<n>` for
the program), so a witness reads what each scan found from the sweep's own view
and stages its window at the scan its premise names.

**Which scan the premise is about, and why the first one is a bet.** Holding the
FIRST scan put the premise back where the sleep had been: a seed's `exec` can
still be running when that scan reads it, and a process in the middle of an
execve takes its `/proc/<pid>/environ` with it for that instant, so the scan
files it as unreadable instead of a member.
`TestSweepStopsAProcessThatShowsAfterANonEmptyScan` asserted its held scan had
found the seed, and reddened on 2 of 40 serial runs of unchanged code
(`premise: the scan the sweep is held at found nothing`), then on 2 of the 3
`go` arms of a second series — a verdict on an execve, and idle-biased: 0 of
200 arms of the same binary under eight-way load, so a green loaded run says
nothing about it. (The sandbox is the right place to hunt it: it is a PID
namespace of its own, nine processes, which is the throwaway namespace this
package's header asks for.) The
premise is now a predicate given to `scanWindow.at`, which releases the scans
that do not meet it and judges the first that does — the pattern
`TestGuardFoundRefusesALiveIntruder` already applied to its own scan. And what
`awaitSessionHolds` proves is where a launcher BECOMES visible: until its own
execve it is a fork of the test binary, carrying the test binary's environment.
It never proves where a process stops being missable, so a witness that needs
ONE named scan to hold a process it started adds `awaitExeced`, which waits for
the name `/proc/<pid>/stat` gives — the file the last execve ran.

**The window the product opens is not only the first one.** A sweep that files
the members of its first scans and then stops re-discovering passed both
witnesses while their single window sat between scan 1 and scan 2: a mutant that
drops members new at the third scan of a phase was green on 18 of 18 arms. Each
witness now stages its window a second time, at a later scan and in the phase
that ENDS its sweep — the empty-scan witness in its TERM phase, which ends on
two scans that find nothing, and the non-empty one at the first scan to find
nothing, which only a KILL-phase scan after the TERM-ignoring seed was killed
ever is. A member dropped in a phase that hands over to the next one is
re-discovered for free by that phase's own first scan, so a window staged there
witnesses nothing. That mutant and its sh twin now redden 3 of 3 arms each, on
the product's own lines (`later (pid N) belongs to the session and is still
running`). The KILL-phase window also turns a capture that never was one: the
mutant that drops re-discovery in the KILL phase ALONE read as caught by
`TestSweepSignalsBeforeItsDeadline`, and is caught there 1 arm in 15 — by the
same execve miss, so the verdict was a race's. It reddens the non-empty witness
3 of 3, and the deadline witness is now green on it: when a sweep gives up is
that witness's subject, re-discovery is not. The phase deadlines are set wide for these witnesses (`sweepBudget`):
a phase that ends on its deadline skips the scan the window was staged for and
reads as a verdict on how long this host took to start one process — when a
sweep gives up is `TestSweepSignalsBeforeItsDeadline`'s subject, and it keeps
the knob.

Where the gate sits is what keeps the mutation honest. At the settle instead,
the mutant that ends on the FIRST empty scan never settles, and the witness
would redden on a premise timeout rather than on its own assertion. Measured, one
implementation at a time: `quiet == 2` → `quiet == 1` in `nodesession_linux.go`
reddens the `go` subtest in ~5s with the queue's own two lines (the seconds are `check`'s wait for a process that never dies, not the sweep's) and leaves the
`sh` ones green; `[ $quiet -ge 2 ]` → `-ge 1` in `nodeSessionSweepBody` reddens
both `sh` subtests and leaves `go` green. Green `-count=50`, `-race -count=10`,
and `-count=20` under two competing jailed runs of the same package.

Three sibling premises of the same witnesses rested on a 200 ms sleep — a leader
thread that must have exited, a `prctl(PR_SET_DUMPABLE, 0)` that must have run.
A loaded host does not fail them, it makes them PASS vacuously: the sweep then
finds an ordinary process and stops it, and the fallback path the witness exists
for is never exercised. Each is now an `awaitPremise` poll on the fact itself —
the process-wide `environ` unreadable while a thread's answers; an environment
nobody of this uid can read while the process still runs — so a slow host costs
time instead of a verdict, and a premise that never holds fails as itself.

The seeds are the other side of the same wall, and there load FAILS the witness
instead of passing it vacuously. A foreground seed returns the pid of a launcher
the test binary has only just forked, and until that launcher's own execve runs
it carries the test binary's environment and not the entry, so a scan misses it.
What it does not prove is the exec of the program the seed goes on to become:
that is the bet the two paragraphs above pay for.
`TestSweepStopsTheSessionAndNothingElse/go` bet all of
`populate()` on those execs and lost in a merge queue that touched no file of
the package (#2539): the guard refused (`the session misses [respawner …]`),
which is correct, but read as a product verdict. The world now awaits its seeds:
`w.awaitSeeded()` holds the test until every marked seed is visible to a scan
(`awaitSessionHolds`) — from `populate()` itself, and from the two witnesses
that seed directly (`TestSweepSignalsBeforeItsDeadline`,
`TestSweepDoesNotWaitOnASinceFileThatIsNoFile`). The controls need no await: a
sweep can only stop carriers of the entry, so a control a scan does not see yet
is trivially spared. The refusal stays witnessed: short-circuiting
`environHolds` in `scanNodeSession`, so seeded carriers are never found, reddens
the unfixed witnesses on the queue's own line and the fixed ones on the await's
premise. Green `-count=30`, `-race -count=2`, and `-count=10` under two
competing jailed runs of the same package.

**A production timer whose losing side is goroutine scheduling is arbitrated in
a bubble.** `pkg/eventbus`'s cancel path gives a subscriber one
`overrunPostCheckWindow` (20 ms) after the budget wait ends on an already-spent
caller ctx before it names the subscriber as an overrun; whether the worker's
done channel closes inside the window is decided by the scheduler, so
`TestBusSubscribeCancelExpiredCtxDoesNotWarnCleanSub` was a 100-iteration
real-clock bet — under merge-queue load it lost at iteration 99 and ejected two
PRs that touched no file of the package (#2499). The fact cannot be staged the
way the sweep's is: the worker's settle is CAUSED by the cancel the witness is
exercising, and the decision point sits inside a `sync.Once` closure that
cannot be driven stepwise. So the witness moved the arbitration itself into a
`synctest` bubble: virtual time advances only when every goroutine of the
bubble is durably blocked, and the goroutines the cancel path just signalled
are runnable while the post-check waits, so the window cannot elapse before
done closes — the witness decides the arbitration every iteration instead of
sampling it. The bubble's one blind spot: shrinking the production window
toward zero goes green here, since virtual time holds it open — that
miscalibration surfaces as operator-visible overrun warnings in real
shutdowns, not as a red queue. Mutants (the warn moved to the settled side of either
implementation's post-check; the post-check dropped for an immediate warn)
redden it on its own line at iteration 0.

