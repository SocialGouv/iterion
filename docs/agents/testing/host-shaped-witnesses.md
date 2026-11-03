# When a test measures its host

Re-audit for [#2912](https://github.com/SocialGouv/iterion/issues/2912),
2026-11-03, against `9cdd04bae`. The six previously recorded packages and
the Kubernetes probe are collected here; historical failures and re-check
dates remain in [the flake register](flakes.md).

| Package / witness | What the current witness measures | Remaining dependence |
|---|---|---|
| `pkg/lease`: `TestRun_aLeaderWhosePassesFindNothingToDoKeepsItsLease` | Virtual-time lease renewal across four TTLs; no healthy peer may take over | The scenario runs in `synctest`, including its waits |
| `pkg/dsl/parser`: `TestBrokenListSplicesStayLinear` | Quadrupling input against an 8x ratio, using medians of three measurements | Uneven wall-clock stalls can still skew the ratio; the register's earlier min/min description was stale |
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
