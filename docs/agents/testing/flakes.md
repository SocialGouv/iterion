# Machine-dependent tests — annotate, measure, re-check

A machine-dependent failure stays visible. Record the test, machine class,
evidence (log or SHA), disposition and next re-check date. Keep the assertion;
neither deleting the test nor silently skipping it turns an unknown result
green. A missing prerequisite produces a named, fast refusal and an explicit
unverified result in the gate report. A rerun records its elapsed time and
environment alongside the first failure, not instead of it.

Fix the class: inventory every caller, isolate the resource at a shared helper,
and prove that restoring the defect reddens the witness on its own assertion.
Process fixtures own their scratch directories, HOME, configuration and child
lifetimes. Network access is not a remedy for an unexplained hang; see
[the hermetic gate decision](../../adr/137-local-gate-hermetic-by-default.md).

## Register

| Test / check | Machine class | Evidence (log / SHA) | Disposition | Re-check |
|---|---|---|---|---|
| `TestEngineRun_IsolatedSubbotSiblingsOverlap` | Concurrent isolated children registering worktrees in one repository | [#2670](https://github.com/SocialGouv/iterion/issues/2670); an empty `commondir` in a pending registration reproduces Git's exact refusal | Runtime setup, reconstruction and cleanup serialize their Git registry commands within the process, keyed by the physical common directory. A virtual-time witness holds an incomplete registration, checks main/linked/symlink spellings and an independent repository, then releases it. Removing the gate or making it global fails the assertions. The original three-child execution-overlap witness remains unchanged. | 2026-11-04, and on runtime worktree lifecycle changes |
| `TestRunSpreadCoverageStrictCheck/a_Forbidden_plus_an_expired_deadline_still_names_the_Role` | Loaded ARC test-unit runner; shell startup races a 200 ms probe | [#2571](https://github.com/SocialGouv/iterion/issues/2571#issuecomment-6260520538), [PR run](https://github.com/SocialGouv/iterion/actions/runs/39146366377) | The production decision `spreadCoverageFailureCause` consumes the explicit cluster error and probe error. The witness stages both facts directly; other rows retain real kubectl shims for Forbidden, timeout and node coverage. Reversing the precedence or dropping the probe error at the caller fails the assertions. The budget and Loki cases were already repaired by `20e4aeb4c` and `5ee792110`; ARC remains tracked in #2564. | 2026-11-04, and on doctor diagnostic changes |
| `TestShutdown_DrainDoesNotLeakAgainstASerializingTracker` | Loaded ARC merge-queue runner under `-race` | [#2072](https://github.com/SocialGouv/iterion/issues/2072), [merge-group failure](https://github.com/SocialGouv/iterion/actions/runs/36852060560): 1/32 claims leaked despite the deadline-bucket assertion | Virtual time now drives the serial tracker; the native store's kernel watcher lives outside the bubble. Refresh and release must each enter with their entire budget, every call must have a deadline, and all 32 real cards must return to Ready without a claim. Moving the revert budget before admission, dropping its bound, or skipping release fails the witness. | 2026-11-04, and on drain budget changes |
| `TestDeliveryProbeOnlyAWellFormedAnswerCanHold` | Parallel ARC socket fixtures | [#2745 merge-group failure](https://github.com/SocialGouv/iterion/actions/runs/40582838423/job/122466535484): a non-redirecting row's target received a request; the reservation witness proves the old refused address can be reclaimed | The shared `deadAddress` fixture now owns a bound, non-listening socket until cleanup. Another fixture cannot claim it; the kernel still refuses connections. Keep every verdict, retry-count and zero-redirect assertion. | 2026-11-03, and on probe fixture changes |
| `TestBackground_NoWrapUpInsideATurnTheCLIStarted` | Loaded ARC merge-queue runner; fake CLI and stream reader scheduled independently | [#2745 merge-group failure](https://github.com/SocialGouv/iterion/actions/runs/40582838423/job/122466535537), commit `b7f7f0ca8`; a local 800ms reader delay reproduces it | **Corrected, verified 2026-10-27**: keep the CLI round trip, test the expired timer before and after assistant text and at the turn's close with the existing explicit-clock lifecycle fixture. Removing either in-turn guard fails the witness. Ten race repetitions and the delayed-reader scenario pass. | 2026-11-03, and on lifecycle timer changes |
| `TestLaunchDetached_NamesTheDroppedOverrides` | Operator workstation with an installed iterion outside PATH | [`e40def071`](https://github.com/SocialGouv/iterion/commit/e40def0715d8a3c1841c504c984c1f4ddd1a158b), including its mutation record | **Corrected, verified 2026-10-24**: `proc.SetInstallPathCandidates` makes the fixed install tier injectable; the witness empties that tier and pins HOME and ITERION_BIN. | 2026-10-31, and on binary resolver changes |
| `TestRepoRootFindsTheNearestRealGitEntry` | Operator machine with a foreign, empty `/tmp/.git`, recreated by an unidentified process | Operator observation recorded in the [#2585 mission](https://github.com/SocialGouv/iterion/issues/2585), baseline `e40def071`; no archived log supplied | Environment parasite, creator unresolved. Inspect ownership/type/content; remove **only the confirmed empty directory** with `rmdir /tmp/.git` before taking a verdict. Record the purge and rerun; a refusal from `rmdir` stops the purge. Never recursively delete it or weaken repository-discovery assertions. | 2026-10-31, and before the next affected verdict |
| `internal/ciguard`'s script guards — every helper that hands a script path to a child shell | Any machine with a warm `GOCACHE`, including a CI job that restored one | Measured 2026-10-29 while mutating `scripts/ci/baked-actions-manifest.sh` for [#2435](https://github.com/SocialGouv/iterion/issues/2435): two mutants that deleted a rule of the generator came back `ok (cached)`, green on a rule that was gone | `go test` keys a cached result on the files the TEST process opened; a script a child shell reads is invisible to that bookkeeping. `scriptUnderTest` reads the script (and refuses an empty one) before `exec.Command`, at every such helper of the package. With the cache deliberately warm, the same two mutants then redden on their own subtests. `-count=1` hides the class for one run; the read fixes it for every run | 2026-11-05, and whenever a new guard shells out |
| `🧪 Tests` workflow, queued jobs | Saturated ARC runner pool | Operator observation recorded in the [#2585 mission](https://github.com/SocialGouv/iterion/issues/2585): runs remain queued for hours; no individual run URL supplied | Infrastructure delay, **no test verdict**. Attach the run URL, SHA, queued/start timestamps and runner class on recurrence. Measure queue wait separately from execution; use the [queue runbook](../../merge-policy.md#measuring-the-queue). Missing/cancelled jobs never count as passing. | 2026-10-31, and at the next landing |
| `TestRun_aLeaderWhosePassesFindNothingToDoKeepsItsLease` (pkg/lease) | Loaded ARC runner | [41236299236](https://github.com/SocialGouv/iterion/actions/runs/41236299236) (`63aac5608`), 0.93 s; also on [#2732](https://github.com/SocialGouv/iterion/pull/2732)'s mongo job; green locally ×5 | The 150 ms lease TTL IS the mechanism under test: a descheduled holder loses it legitimately. Virtual clock is the fix — [#2854](https://github.com/SocialGouv/iterion/issues/2854). | 2026-11-07 |
| `TestBrokenListSplicesStayLinear` (pkg/dsl/parser) | Loaded ARC runner under `-race` | [41233597165](https://github.com/SocialGouv/iterion/actions/runs/41233597165), 3.42 s; [41236299236](https://github.com/SocialGouv/iterion/actions/runs/41236299236), 24.14 s; local ~1.9x | T41: the ratio reads min/min — load only inflates; a quadratic rescue scores ≥ 4x even at its best. | 2026-11-07, and on parser splices |
| `TestEvalWorkBudgetCountsNodeEvaluations` (pkg/dsl/expr) | Loaded ARC runner under `-race` | [41236299236](https://github.com/SocialGouv/iterion/actions/runs/41236299236): refusal at 20.06 s vs 10 s; 1.45 s locally | T41: tripwire at 2 min — its target is the budget not biting (~1 h). The refusal stays asserted | 2026-11-07 |
| `TestAnInstallTakesTheLockOfEveryFileItWritesOrRemoves` (pkg/botinstall) | Loaded ARC runner under `-race` | [41236299236](https://github.com/SocialGouv/iterion/actions/runs/41236299236): the lock sweep blew its 300 ms on a stalled shard; green locally ×3 | T41: test `installLockWait` at 2 s (production unchanged); the bounded wait and the 10 s watchdog are untouched. | 2026-11-07 |
| `TestSiblingBrokenListsParseInLinearTime` (pkg/dsl/parser) | Loaded ARC runner under `-race` | [41243004752](https://github.com/SocialGouv/iterion/actions/runs/41243004752) (`5058166bc`): 6.43 s vs 5 s; ~150 ms locally | T42: bound at a minute — the quadratic regression it prices takes minutes at this size | 2026-11-07 |

| `TestBackground_ADeliveredResultNoLongerHoldsTheGrace` (pkg/backend/delegate) | Loaded ARC runner under `-race` | [main witness on the T43 tip](https://github.com/SocialGouv/iterion/actions/runs/41243004752) `dfd8c8db4`: 4.15 s against the 4 s ceiling; green locally ×3 | T44: the ceiling moves to 10 s — what it prices is the grace HELD past a delivered result, which parks the run for the 20 s result wait; the margin stays decisive. | 2026-11-08, and on the background-session grace |
| `signOutMachineTransitions` (studio) — "holds the start while the named session is held" | Loaded ARC studio job | [T43 witness](https://github.com/SocialGouv/iterion/actions/runs/41243004752): the OIDC landing listed 4 elements where 2 were expected (a 500-answering logout retried under load); the file passes 3× locally | Load-induced duplication in the landing's pending path, unreproduced. One occurrence; a recurrence gets a ticket and a query-dedup fix — the assertion stays. | 2026-11-08, and on the sign-out machine |

Re-check on 2026-10-24, `e40def071` plus the #2585 working diff: the jailed
`task check` reproduces the `/tmp/.git` failure. `stat` reports an empty
directory owned by `jo`, mode `0555`; `rmdir /tmp/.git` refuses with
`Device or resource busy`. The verdict remains red in this environment;
no recursive removal or forced unmount substitutes for the failed purge.

## Worked example: an installed binary defeats an empty PATH

`TestLaunchDetached_NamesTheDroppedOverrides` expects a detached launch to
refuse after logging dropped overrides. Emptying PATH alone cannot prove that:
the resolver also searches fixed installation paths, including the operator's
home. The fix supplies an empty install-candidate seam, an owned HOME and an
empty ITERION_BIN. The production resolver and its sandbox caller share that
seam; a second resolver would reopen the class.

The mutation recorded in `e40def071` makes the seam ineffective and removes
the HOME pin on a machine with an installed binary. The test then fails its
own expected-refusal assertion. A process leak warning is supporting evidence,
not that assertion. Re-check on both a machine with the installation and an
isolated worker, recording the exact SHA and elapsed time.
