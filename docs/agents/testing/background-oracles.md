# Background sessions — prove the decision before measuring duration

The fake Claude CLI and the engine are separate processes. A session's
`bgRun.elapsed` includes process startup, scheduling, script pacing and process
cleanup. It cannot distinguish an unwanted lifecycle wait from a slow launch.
In [#3008](https://github.com/SocialGouv/iterion/issues/3008), the nested-agent
witness returned the correct `GOT t1` with no error but failed its 1.5 s ceiling
on ARC. Adding only 1.3 s before the fake CLI's first read reproduced that
failure on `f611e2741`, at 1.661 s.

## Immediate endings: the CLI round trip and the decision

Twelve no-wait tests keep their real `runBgSession` round trip and their result,
error, structured-output, lost-work, phase and stdin assertions. Their former
wall-time ceilings are replaced by checks using the existing `lifecycleAt`
fixture, the production tracker and `bgLifecycle`:

- An error or an unengaged lifecycle returns at the original result's close.
  Parse that result; rebuilding a success from its text loses `is_error` and
  the result subtype.
- An idle session chooses exactly its idle-settle timer and ends at that
  deadline without a nudge or a request for a report. The script must leave
  the idle undisturbed through this window. A later nested notification is
  outside the prefix being proved.

The helper consumes the same script lines, counts each result's reentry, and
advances pacing sleeps on an explicit clock. It refuses interactive directives
instead of simulating a second runtime. The idle check is after the result's
close, so neither a result nor a turn is pending in the select loop. It checks
the cancellation guard before settling; a tracker's state alone does not say
what the select loop has already consumed. The real CLI run continues to prove
that these decisions connect to the stream protocol. Its background timer is
the real `time.Timer`, decorated per session to record the actual `Reset`
argument. Each idle-ending witness also requires that its final armed duration
is at most the idle-settle window. Recording only what `bgLifecycle.timer`
chose would miss a select loop that mistakenly arms the auto-turn grace.
This measures the requested duration, not when the OS schedules the fire;
the existing cancellation and intervening-turn witnesses cover early endings.

The no-wait inventory, with the common `TestBackground_` prefix omitted:

| Test | Decision |
|---|---|
| `WorkFinishedMidTurnEndsAtTheFirstResult` | Idle-settle |
| `SubagentNestedNotificationDoesNotHoldTheParent` | Idle-settle |
| `ForegroundTaskNotificationDoesNotHoldTheParent` | Idle-settle |
| `ErrorResultEndsTheLifecycleAndReportsTheLostWork` | Return at error result |
| `AmbientHousekeepingIsNotWorkTheAgentLaunched` | Return at result |
| `AmbientSubagentsDoNotHoldTheSession` (two cases) | Return at result |
| `AnOpenEndedShellDoesNotHoldTheSession` | Idle-settle |
| `NoAutoTurnGraceWithNothingLeftEndsAtOnce` | Idle-settle, even with grace off |
| `AnErrorResultMarkedSuccessStillEndsTheLifecycle` | Return at `is_error` result |
| `AShellLeavingTheSetIsNeverAwaited` | Idle-settle before shell notification |
| `ANestedAgentLeavingTheSetIsNeverAwaited` | Idle-settle before nested notification |
| `AForegroundAgentAloneDoesNotEngage` | Return at result |

## Inventory boundary: budgets are different assertions

The audit of `bgRun` and its constructor found 186 `runBgSession` calls, all in
`claude_code_background_test.go`. Before this change, 26 conditions read
`elapsed`: the twelve above and fourteen below. Diagnostic uses of elapsed
remain. The script-derived event-order floor using `at` and `startedAt` also
remains; it is not a no-wait ceiling.

These fourteen assertions measure a budget, a minimum wait, or the precedence
of one budget over another. They are unchanged by #3008, and still measure wall
time including startup. Migrating them needs their own explicit timer or
before/after-boundary witnesses; an idle/return oracle would not replace their
guarantees.

| Test | Duration contract |
|---|---|
| `StallGuardIsLiveAfterTheWorkSettled` | Stall watchdog and recovery |
| `ASubagentsOwnAgentHoldsTheSession` | Wait while nested work runs, then end |
| `HeldWorkBackWithoutATurnHasABudget` | Wave wait budget |
| `AMonitorsTurnsEndTheSessionAfterTheGrace` | Grace bounds monitor turns |
| `ProgressAtRestDoesNotPushTheGraceBack` | Progress does not slide the grace |
| `AHeldResultThatNeverComesIsWaitedForBoundedly` | Result-wait minimum and maximum |
| `AMonitorAnsweredWithShellsStopsAtTheWaitBudget` | Source-turn ceiling |
| `ADeliveredResultNoLongerHoldsTheGrace` | Grace wins over an old result wait |
| `AHeldResultThatNeverReachesTheQueueIsRecordedLost` | Result-wait minimum and maximum |
| `AResultThatNeverComesIsBoundedByResultWaitUnderAMonitor` | Result wait under monitor turns |
| `TheResultWaitIsTheDocumentedBound` | Documented result-wait duration |
| `NoWaitBudgetIsNoCeilingOnAMonitorsTurns` | Disabled ceiling, grace still applies |
| `AMonitorChainOfShellsEndingMidTurnStopsAtTheWaitBudget` | Source-turn ceiling |
| `AShellsEndIsNoResultOnItsWay` | Auto-turn grace wins over result wait |

The existing explicit-clock tests of rest-clock restart, result-on-its-way
timestamps, source-turn ceilings and wrap-up answer waits are precedents for
that separate work. Keep the shared session watchdog as a wedge safety net;
it is not the oracle deciding whether an immediate ending waited.
