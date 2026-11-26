# When a test measures its host

Re-audit for [#2912](https://github.com/SocialGouv/iterion/issues/2912),
2026-11-03, against `9cdd04bae`. The six previously recorded packages and
the Kubernetes probe are collected here; historical failures and re-check
dates remain in [the flake register](flakes.md).

| Package / witness | What the current witness measures | Remaining dependence |
|---|---|---|
| `pkg/lease`: `TestRun_aLeaderWhosePassesFindNothingToDoKeepsItsLease` | Virtual-time lease renewal across four TTLs; no healthy peer may take over | The scenario runs in `synctest`, including its waits |
| `pkg/dsl/parser`: `TestBrokenListSplicesStayLinear` | Quadrupling input against a 6x ratio of counted stream and stack-replay steps | None: no clock. A median wall-clock ratio read 8.1x on a loaded pod ([#3731](https://github.com/SocialGouv/iterion/issues/3731)) |
| `pkg/dsl/expr`: `TestEvalWorkBudgetCountsNodeEvaluations` | An actual budget-exceeded refusal on 18M node evaluations | A two-minute wall-clock tripwire remains; it is not a virtual-time proof |
| `pkg/botinstall`: `TestAnInstallTakesTheLockOfEveryFileItWritesOrRemoves` | Named lock refusal and unchanged bytes while an owned lock is held | Two-second lock wait and ten-second watchdog remain |
| `pkg/backend/delegate`: `TestBackground_ADeliveredResultNoLongerHoldsTheGrace` | Delivered-result notification, wrap-up request and elapsed time | Ten-second ceiling against the configured twenty-second result wait remains |
| `pkg/runtime`: `TestFanOutStopsOnUndecidedEffects` | Persisted ambiguous effect after signalled branch startup, across stop causes | Deadline cases still use two seconds and explicitly diagnose expiry before branch startup |
| `pkg/sandbox/kubernetes`: `TestDetectResolvesNoKubectlUnderAPlantedPATH` | Trusted PATH resolves the fixture; planted PATH refuses before namespace lookup | T41 injects an absent owned namespace path; the real pod mount no longer determines the assertion |

The Kubernetes failure was a host **shape**, not a slow lookup: ARC runners
are pods and have `/var/run/secrets/kubernetes.io/serviceaccount/namespace`.
Before `6cedc5599`, a trusted fixture PATH could therefore make `Detect`
succeed, while the test expected “not running in a kubernetes pod”. The
existing fix owns that prerequisite through `inClusterNamespacePath`.

Rechecked with a temporary overlay that supplies an existing namespace file
before calling the original witness: it passes. Removing only the T41
namespace pin in a second overlay fails on the original assertion:
`Detect err = <nil>, want the namespace check's`. The repository source stays
unchanged. This verifies both host shapes without changing the real mount,
skipping a test, or increasing a timeout.

The remaining wall-clock bounds above are recorded limitations, not fresh
claims of immunity to load. A recurrence keeps its original failure and gets
a signal, ratio or virtual-time fix; increasing a constant is not the repair.

## Wrap-up read

`TestBackground_AWrapUpTheCLINeverTakesIsAFastNamedError` exercises a CLI that
reads the wrap-up but never replays it. In the local full gate on 2026-11-04,
the correct named timeout fired after 2.18 s while the subprocess had not yet
logged that read ([PR #3007](https://github.com/SocialGouv/iterion/pull/3007)).
A FIFO holding the fake CLI before its drain reproduces the same failure.

This scenario now arms the real take timer, then waits for a complete tagged
wrap-up frame in the fake CLI transcript. An expired tick stays expired;
production and its deadlines are unchanged. Phase events and partial frames
cannot satisfy the barrier. The existing context still fails a CLI that
never reads. Removing the production take arm or the actual send fails the
witness. Re-check on 2026-11-11 and on changes to the timer or fake CLI.

## What the codemode posture bench paid for measuring its host

Five, from [the posture bench](deployment-posture-bench.md)
([#3155](https://github.com/SocialGouv/iterion/issues/3155)). Each was a green
that meant nothing, and each was found by writing the mutant rather than by
re-reading the code.

- **A bound the leg trips, then a guard that was itself the flake.** The
  spinner burned ~11 s against a 10 s bound, so siblings died of their own
  limit while the leg blamed the bombs. A second version *asserted* a margin,
  comparing WALL time to a CPU bound: the same setting passed at 14.9 s and
  failed at 17.0 s over a healthy deployment at `--cpus 0.5`, the runner's own
  request. Removed — `cpu_time_exceeded` already says it. Both groups now
  carry explicit wide bounds; the bombs hit `wall_clock_exceeded` at
  `--cpus 0.1` on the default 30 s wall.
- **A goja regex without a backreference is not a bomb**: `/(a+)+$/` compiles
  to linear RE2, only `/(a+)+\1b$/` forces backtracking.
- **`docker cp` under a `tmpfs` mount** reports success for a file the
  container cannot see — the copy lands in the layer the mount shadows.
- **`kill(pid, 0)` cannot tell a zombie from a running process**, and whether
  a reaped pid disappears depends on PID 1. Measured: with the entrypoint
  overridden to `sleep`, which never waits, a descendant dead within 10 ms
  kept its pid and the leg called it a survivor, 3 runs out of 3. The shipped
  image runs `tini`; the oracle depends on neither and reads
  `/proc/<pid>/stat`.
- **A bench cannot ask a confined child about its environment**: `Confine`
  calls `prctl(PR_SET_DUMPABLE, 0)`, so a same-uid reader gets `EACCES`
  whatever the child holds. Measured: mutating the constructor to
  `cmd.Env = os.Environ()` left such a leg green 8/8. That guarantee belongs
  to `TestTheChildIsBuiltWithNoEnvironment`.

## WebSocket fixture shutdown

[#3013](https://github.com/SocialGouv/iterion/issues/3013): original ack witness fails cleanup 5/100 under race; removing the fixture join reproduces it.

Test server owns stream subscriptions: stop admission, cancel, then join event/chunk producers before directory cleanup. An unsubscribe ack alone does not join them. Ack assertions and production behavior unchanged; corrected ack100 and family3 pass.

Re-check: 2026-11-13; stream fixture lifecycle changes.


## Mongo conformance deadline

Measured 2026-11-12 over eight consecutive runs: success at 12:31, 13:46, 14:05, 14:33, 14:48 and cancellation at 15:18, 15:19 against `timeout-minutes: 15` — the job runs at **92-99 % of its budget** and tips over on a slow runner.

Infrastructure, **no test verdict** — a cancelled required check is not a pass. Nothing in the suite failed in either cancelled run. The cure is capacity, not an assertion: the store harness runs alone with its own `-timeout 15m` (its transactions and change streams saturate a shared mongod) and the job cap is `timeout-minutes: 55`, the sum of its budgets under the queue's 60. Rerun the job alone rather than the whole run

Re-check: At the next cancellation, with the durations above.


## Fan-out admission deadline

[#2852](https://github.com/SocialGouv/iterion/issues/2852), [#3013](https://github.com/SocialGouv/iterion/issues/3013): the 2 s guard re-flaked before run creation; a 2.2 s pre-entry delay reproduces it.

Each scenario uses synctest with the real 2 s context deadline and unchanged 25 ms grace. The outer test owns scratch and grace restoration until every bubble goroutine joins. Deadline Err/Cause, outcome and event assertions remain; cause-only, omitted classification, abandoned-result and producer-release mutations fail.

Re-check: 2026-11-13; fan-out lifecycle changes.
