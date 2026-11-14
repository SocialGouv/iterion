# Boundary witnesses — prove the transition

The [flake register](flakes.md) links the affected families. These witnesses
retain real production paths; their synchronization makes the transition under
test happen before the assertion, without increasing a wall-clock allowance.

## Mongo replay and watch

In [PR #3050's run](https://github.com/SocialGouv/iterion/actions/runs/41897913872),
`TestMongoSource_EventsNoBoundaryGap` reported 29/40 events. Its producer was
not joined before the 30-second deadline. A local counterexample pauses that
producer after 29 committed appends: the old assertion fails with 29 delivered
and 29 persisted; releasing the producer yields all 40 without duplicates.
This proves an ambiguity in the oracle, not the cause of the historical CI run.
That job also hit its 15-minute ceiling, a separate capacity issue.

The replacement uses Mongo's command monitor on a dedicated source client;
injections use the store's separate writer client. Two cases exercise the same
`SubscribeEvents` path over a real replica set:

| Injection point | Required delivery | Production mutation |
|---|---|---|
| After the backfill reply has returned a closed, one-event cursor | The established watch delivers the new event | Backfill before Watch loses it: `[0,2]` |
| After Watch succeeds, before backfill | The overlapping event appears exactly once | Removing watch deduplication delivers `[0,1,1,2]` |

The first watch `getMore` inserts a final marker. On receipt, the test requires
three persisted events and exactly `[0,1,2]` in delivery order. It also checks
the initial snapshot, command order and absence of reconnects. Both mutations
compile and fail those assertions. The retained 30-second context is a wedge
guard; a timeout is never reported as proof of a gap. This is a boundary
contract, not a throughput benchmark or a proof of every reconnect path.

## Lease handover

In [PR #3052's run](https://github.com/SocialGouv/iterion/actions/runs/41897991723),
`TestGateSweep_aSickHolderHandsOverToAPeerThatReachesTheForge` observed a healthy
peer but no voluntary handover: the original holder stepped down because it
could not prove renewal within 125 ms. That is a different transition from the
one the test intends to exercise.

The memory-store campaign scenarios use a synctest bubble. TTLs, pass pacing,
renewal and retries share virtual time; real filesystem operations in the gate
fixture still run. The complete six-scenario inventory is:

| Package | Scenario and retained assertion |
|---|---|
| lease | All passes unreachable: a healthy peer takes over |
| lease | Far side refuses: the original holder keeps its lease |
| lease | Idle passes: the original holder remains elected through four TTLs (already virtual before #3060) |
| lease | Handover retry: the sick replica observes the configured backoff |
| server | Board sync: a peer sweeps and the campaign logs voluntary handover |
| server | Gate sweep: a peer offers the run and the campaign logs voluntary handover |

No TTL or deadline increases. The forge server fixture registers `Hub.Stop`
with test cleanup: the bubble exposed three previously leaked hub goroutines.
Keep cleanup inside the bubble, so it also verifies termination. Tests of
individual health verdicts and term cancellation do not schedule campaigns and
retain their existing synchronous witnesses.

## Parser lookup

PR #3050's race shard also failed `TestBrokenListsParseInLinearTime`: ten times
the input took 30.4 times as long, above its ratio threshold of 25. The parser
tree was byte-identical to main. Sequential minima do not establish that both
measurements experienced the same load; this observation alone does not prove
the open-block lookup rescanned its input.

`TestOpenBlocksReadEachTokenOnce` already counts actual token reads on both the
broken-list and nested/spliced shapes. Retain it and its semantic cross-checks;
remove the redundant time ratio. Forcing the real `openBlocks` implementation
to reset on every call makes that witness reject 199,950,002 reads for 80,001
tokens and 14,507,500 for 31,001. ASTs and diagnostics stay identical on three
comparison inputs: the mutation changes work, not the expected parse result.

`TestOpenBlockResetPreservesOperationAccounting` calibrates that counter across
a reset, with and without skipped spans. Reading an unchanged prefix does no
extra work; resetting and rereading counts the work again. Clearing the counter
on reset fails this calibration, including when combined with the rescan.
The claim concerns this lookup, not the complexity of every parser operation.

The separate `TestBrokenListSplicesStayLinear` ratio remains unchanged. Its
historical timings were 3.42 s in [41233597165](https://github.com/SocialGouv/iterion/actions/runs/41233597165)
and 24.14 s in [41236299236](https://github.com/SocialGouv/iterion/actions/runs/41236299236),
with a local ratio around 1.9. These are separate observations, not proof that
every timing-based test is stable or that the CI slowdown's cause is known.


## React title publication

[PR #3067's merge group](https://github.com/SocialGouv/iterion/actions/runs/41923523178/job/126901843479)
rendered the configured legal heading while the tab still had its loading
title. The heading commit does not establish that the passive effect publishing
`document.title` has run. All three ready-title assertions in `Legal.test.tsx`
now wait for that exact title with Testing Library's existing budget; restoring
the previous title after unmount remains a synchronous assertion.

A private deferred-publication fixture makes the old assertions fail and the
new ones pass. It is a synthetic ordering test, not a reconstruction of React's
CI schedule. Omitting publication, keeping the wrong title, omitting only the
second document's title, or omitting cleanup each still fails the corresponding
assertion. No production code, timeout or expected title changes.

## Delegate pricing isolation

[PR #3067's unit job](https://github.com/SocialGouv/iterion/actions/runs/41931266477/job/126928459573)
passed the OpenCode dialect assertions, then failed a child test's TempDir
cleanup with `directory not empty`. Its zero-cost fake reaches the cost
estimator, whose claw registry can refresh asynchronously and recreate a cache
under the test's mutable HOME. The modelspecs aggregator has a separate refresh
path. A controlled local transport demonstrates writes after cache removal;
it does not reproduce the exact CI cleanup interleaving.

Delegate TestMain now uses `pricingtest.Isolate`, as model and runner already
do, and since #3257 so does every test binary that links a live tier (cost and
modelspecs, below pricingtest, on their own; pkg/claw under its own guard),
under a guard reading the dependency graph. A fresh cache with a different price must leave the static estimate intact.
The shared helper uses an empty `NewSeeded` registry: its former fixed
`iterion-modelspecs-test-absent.json` path could actually exist. The helper's
test first reads a valid planted cache through an explicit registry, then
requires isolation to ignore it while preserving explicit seeded overrides.

Removing isolation exposes the planted claw price and detached write;
disabling claw alone still permits the aggregator's local fetch. Restoring
the helper's old disk-backed constructor fails its planted-cache assertion.
Provider-supplied costs and explicitly seeded model specs remain available.
No cleanup retry, timeout increase or production pricing policy changes.
