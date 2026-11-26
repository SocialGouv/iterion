# ADR-173 — Experimental delivery mode: the merge queue gates on the light layer, the heavy layer runs on `main` and every night

- **Status**: Accepted — the operator decided it on 2026-11-25 (#3691); the ruleset half (`race` and `mongo-conformance` out of ruleset 18857412) was applied the same day, the workflow half lands with this record.
- **Date**: 2026-11-25
- **Deciders**: jo (the decision), Claude (the workflow and its guards)
- **Relates to**: [ADR-105](105-ci-build-cache-on-the-org-runners.md) (the scale set the queue runs on) · [ADR-137](137-local-gate-hermetic-by-default.md) (the local gate a direct landing owes)
- **Serves**: [#3691](https://github.com/SocialGouv/iterion/issues/3691) · follows [#3683](https://github.com/SocialGouv/iterion/issues/3683) · compensated by [#3692](https://github.com/SocialGouv/iterion/issues/3692)

## Context

Until #3683 every pull request paid the whole suite twice: on its own run, then on its `merge_group` run, about 25 ARC jobs each on a scale set of ~40 concurrent pods. Measured on 2026-11-24/25 ([merge policy](../merge-policy.md#light-runs-heavy-main)): queue latency of 4 to 14 hours, and 8 of 12 merge-group runs cancelled. #3683 took the heavy legs (`race-shard`, `test-e2e`) off the light pull request and left them in the queue, where every entry still paid them, and a rebuilt group paid them again.

The project is experimental: many chantiers wait on the queue, and iterating fast is worth more, for now, than proving every merge under the race detector before it lands. The operator's reading of the same window (#3691) is that the ejections that were not regressions came mostly from that heavy layer — the `-race` shards and the e2e package — not from the unit layer.

## Decision

While the project is experimental, the **merge queue gates on the light layer**: `test` (over `test-unit`, `studio`, `test-native-exec`, and `test-e2e` skipped), `vendor-check`, `golangci`, `brand`, the staged `fmt-check`, and `revi/review`. Ruleset 18857412 requires `test`, `vendor-check`, `golangci`, `brand` and `revi/review`.

The **heavy layer** — `race-shard`, `test-e2e`, `mongo-conformance`, `cloud-e2e` — runs after the merge instead:

- on the **debounced push to `main`**: the first push after `CI_HEAVY_EVERY_HOURS` hours (a repository variable, 6 by default) without a completed heavy run on `main` runs the heavy layer, and every other push runs the light layer only (#3737). The `heavy-gate` job decides it from the run history (`scripts/ci/heavy-gate.sh`): a run counts when one of its `race-shard` jobs concluded success or failure inside the window. When it cannot read the history or the window it warns and runs the heavy layer, and `CI_HEAVY_EVERY_HOURS=0` runs it on every push. It never runs in the merge queue;
- on a **nightly schedule** (`47 1 * * *`, `.github/workflows/tests.yml`), a backstop: GitHub delays or drops this repository's crons, so the debounced push carries the cadence, and a nightly that fires counts as a heavy run of the window;
- on a **dispatch**, and on a **pull request labelled `ci:full`**, for a change an author wants proved before it lands.

One literal decides it, the heavy jobs' own condition: true on any event that is neither a pull request, a queue build nor a push — so a trigger added later runs the full suite by default — on a push `heavy-gate` did not answer `false` (a gate that failed leaves its output empty, and the push runs heavy), and on a `ci:full` pull request. The aggregators `test` and `race` read its negation (`LIGHT_RUN`) and forgive a `skipped` heavy leg on a light run only; on a heavy push, the nightly, a dispatch and `ci:full` a skipped heavy leg is red. `internal/ciguard` evaluates that literal per event and runs each aggregator's step under the result (`event_layers_test.go`).

`race` and `mongo-conformance` are **held** checks (`heldChecks` in `internal/ciguard`): out of the ruleset, still reporting, `race` an aggregator held to the required one's contract. In the queue `race` vouches for `test-native-exec` alone, and says so.

## Compensation

Moving the heavy layer after the merge means a regression it alone sees lands on `main`. Three mechanisms pay for that:

1. **The push witness.** Every push to `main` runs in one shared concurrency group that never cancels the run in progress: one run in flight, at most one pending, each newer push replacing the pending run, and each run's test jobs judge the tip, hence every group merged since the previous run started; the advisory `docs-links` diff compares against the last push's parent only. A light push run proves the light layer on the tip; the heavy layer is proved by the next heavy run — the debounced push, at most `CI_HEAVY_EVERY_HOURS` after the last one while `main` moves — and that run judges every batch merged since the last heavy run. A red heavy job therefore names the batches since the last green heavy run, not a group. main-watch must tell a heavy run from a light one ([#3757](https://github.com/SocialGouv/iterion/issues/3757)): a green light run says nothing about the heavy layer, and a heavy regression's suspects reach back to the last green heavy run.
2. **The nightly.** A backstop: when GitHub fires it, the full suite runs on `main` under its own concurrency group, so no merge cancels it, and it counts as the window's heavy run.
3. **The main-red discipline**, automated by the main-watch workflow of [#3692](https://github.com/SocialGouv/iterion/issues/3692) and owed by hand until it lands: a red push or nightly run is rerun once; green on rerun, the failing test is a flake and goes to `scripts/ci/test-quarantine.tsv` with an open ticket; red twice, it is a regression, and the merge that brought it is reverted, or fixed forward, within four hours. A red `main` nobody owns is the failure mode this mode cannot afford.

## Exit criteria

The mode ends — `race` and `mongo-conformance` return to the ruleset, and `merge_group` leaves the heavy literal, in the same change — when any of these holds:

- **more than one regression reaches `main` in a week**, two weeks running (a red push or nightly run that a revert or a fix, not a rerun, turned green);
- **more than three new flakes are quarantined over two weeks**: the post-merge layer is then too noisy to read;
- **a red `main` stays untriaged for more than one working day**, twice in two weeks: the compensation is not being paid;
- **the project leaves experimental** — the operator's call, at the latest when an external tenant depends on `main`.

The operator reviews these at each retrospective, from `gh run list --workflow tests.yml --branch main` (events `push` and `schedule`) and the history of `scripts/ci/test-quarantine.tsv`.

Ending the mode is one decision in two places, never two: the ruleset names `race` and `mongo-conformance` again, and `tests.yml` puts the heavy legs back in `merge_group`. A held check named by the ruleset while the queue still skips its work would pass on a check that never ran.

## Consequences

- A queue entry costs the light layer only, so entries build and merge faster; the group rebuilds that used to restart seven `-race` pods restart none.
- A race or e2e regression is found after it merged, on the push run, and costs a revert instead of an ejection.
- Automatic releases (`version.yml` on a merge and nightly) can tag and publish a commit whose heavy layer has not yet run or has not yet finished: a heavy regression can ship in a released version before the push witness or the nightly judges it. Gating the release on a green Tests run for its sha is a deferred decision.
- An admin merge still owes the heavy legs on the pull request (`ci:full`): it also gives up the queue's rebuild against the entries ahead of it ([merge policy](../merge-policy.md#a-parked-gate-decided-2026-11-10)). Every admin landing also runs `task ci:gate` on the integrated tree and, an incident fix aside, waits for an empty queue; the operator's [extended train lane](../merge-policy.md#the-extended-train-lane-decided-2026-11-25) lands a burst on that gate without waiting for its pull requests' CI (#3686).
- The local gate (`task ci:gate`) still runs `test:race` and `ci:mongo-conformance` as members, but no longer counts `race` and `mongo-conformance` toward its completeness claim, which follows the required set.
