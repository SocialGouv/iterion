# Merge policy — main is protected by a merge queue

`main` is guarded by a GitHub **merge queue** (ruleset "main protected — merge
queue", enforcement `active`). This closes the semantic inter-PR conflict class:
two PRs that are each green against `main`-at-branch-time can still break `main`
when combined (observed 2026-07-12: #120×#121 didn't compile combined; #128×#130
conflicted). The queue rebuilds each PR against the queue head — `main` + every
PR already ahead of it in the queue — and merges only if that combined tree is
green.

## How it works

- A PR is **merged through the queue**, not directly. Click "Merge when ready"
  (or `gh pr merge <n> --auto --squash`) to enqueue it.
- The queue creates a temporary branch = `main` + earlier-queued PRs + this PR,
  runs the required checks on it, and squash-merges only if green. Grouping is
  `ALLGREEN` (a failing entry drops out; the rest still merge).
- **A queue entry is a full CI cycle**, and the organisation shares 20
  concurrent jobs across every repository — so how many entries build at once
  is what decides how long a merge takes, far more than how long any test runs.
  Measured 2026-09-08: nine jobs totalling ~31 min of compute took 44 min of
  wall clock, 19 of them waiting for a first slot, while five entries built in
  parallel — so the queue was set to `max_entries_to_build: 2`. Since
  2026-09-29 every required check from `tests.yml` runs on the organisation's
  `arc-runners` scale set (below), which the cap does not bound, and the queue
  builds **three** entries at once (`max_entries_to_build: 3`); four once a
  week at three shows the scale set keeps up — runner pods not left pending,
  the queue's latency in `task ci:queue-stats`. `min_entries_to_merge: 3`
  (merges land in batches of 3-5, so the workflows that fire on every push to
  `main` — Tests, Container Image, Trivy, and through Container Image's
  completion Runner Image and Sandbox Base Images — run once per batch). A
  lone PR still merges after `min_entries_to_merge_wait_minutes` (5).
- **How long the queue waits for a check** — `check_response_timeout_minutes`
  (60), read from the ruleset on 2026-10-29 with
  `gh api repos/SocialGouv/iterion/rulesets/18857412` (that GET is also the
  rollback of any change to it, since the PUT below wants the complete
  representation). An entry whose required check never reports is dequeued
  `checks_timed_out` after that long, and so is every entry behind it.
  Measured 2026-10-15 (SocialGouv/iterion#2377): a required job sat **70
  minutes** `queued` on `arc-runners` with no runner while its siblings ran,
  and the queue ejected the pull request. The pool had capacity — the Actions
  service was holding the job on a phantom runner, the ghost class
  `.github/workflows/arc-canary.yml` exists to detect.

  > **The alert and the dequeue currently race, by arithmetic.** The canary
  > pages on a probe nothing picked up within `STALL_SECONDS` (1500 s, 25 min,
  > `scripts/ci/arc-canary-verdict.sh`), it probes every 30 minutes, and its
  > watchdog gives the period it judges a 5-minute head start — so dispatch
  > can stop being trustworthy up to **30 + 25 + 5 = 60 minutes** before anyone
  > is paged, exactly the queue's own patience. The page is supposed to land
  > while a human can still set `CI_SELF_HOSTED` to `off` (below), not after
  > the queue has given up. Closing that gap is one field of the ruleset —
  > `check_response_timeout_minutes` to 90 — and an admin's PUT, which is what
  > is left on #2377; `internal/ciguard` holds the three numbers above to the
  > ceiling this file claims, and holds every site that writes the timeout to
  > one value.
  >
  > **One probe per pool (#3701).** Since the runners split into the small,
  > large and heavy pools of #3694, the canary runs one probe job per pool,
  > routed through that pool's `CI_*_RUNNER` variable, and judges each pool on
  > its own job: a pool still waiting no longer reddens the others, and the
  > heavy pool, saturated by design while the post-merge witness runs, pages
  > after 90 minutes instead of 25 (`CANARY_STALL_SECONDS`; it carries no
  > required check). `scripts/ci/arc-canary-aggregate.sh` folds the verdicts:
  > any red pool pages. Once a pool's variable is set, its probe also checks
  > that the pool runs the image `ci/arc-runner/` defines
  > (`scripts/ci/arc-runner-freshness.sh`), since that pin lives outside this
  > repository.
- **Required checks** (the fast, reliable ones): `test`, `vendor-check`,
  `golangci`, `brand`, `revi/review` — and `nats-conformance` once an admin
  adds it to ruleset 18857412. `race` and `mongo-conformance` are **held** out
  of the ruleset while the
  [experimental delivery mode](adr/173-experimental-delivery-mode.md) lasts
  ([below](#light-runs-heavy-main)), and return to it when the mode ends.
  `test` and `race` are AGGREGATOR CONTEXTS, not test-running jobs: the work
  lives in the gated legs (the `test-unit` matrix, the `test-e2e` matrix,
  `studio`, `test-native-exec`, the `race-shard` matrix) behind them, and each aggregator
  re-verifies every leg's conclusion under `if: ${{ !cancelled() && <tap skip> }}`
  — the run-state clause combined with the one verified skip
  ([below](#the-one-verified-skip-the-homebrew-tap-update)), so an aggregator
  skips exactly the entries its legs skip and runs on every other one. The
  required NAMES stay stable — a PR that renamed one could never pass its own
  queue build — while work scales sideways (shards, new legs). `test-e2e`
  partitions Go's discovered top-level Test, Example and Fuzz corpus names
  into four shards; subtests follow their parent, and each shard runs under
  `-race` with a 2400-second test timeout — the package's only run, so
  `race-shard` leaves it out of its partition and `test` speaks for e2e under
  the detector (#3664). Reproduce one with
  `pattern=$(CGO_ENABLED=1 GOFLAGS=-race go run -mod=vendor ./scripts/ci/testshards -tests ./e2e -n 4 -i 0)`
  then `CGO_ENABLED=1 go test -race -mod=vendor -count=1 -run "$pattern" -timeout 2400s ./e2e`,
  in the same devbox and build environment, under `set -euo pipefail`.
  Discovery has its own configurable `-list-timeout` (five minutes by default;
  CI passes ten minutes, the cold `-race` compile on an ARC pod; zero
  disables it); its failure stops execution. Discovery owns a Unix process
  group through cancellation and output drainage, using the shared
  `ITERION_PROC_WAIT_DELAY` override; a descendant starting another session
  escapes that group. This new test-discovery mode refuses Windows, whose
  process helper cannot provide group ownership. Package sharding is unchanged.
  `test-native-exec` is the one leg off the scale set: `pkg/sandbox/kubernetes`'s
  native witnesses need a Bubblewrap mount ARC pods refuse, so `test-unit` and
  `race-shard` leave that package out and this job proves it, plain and under
  `-race`, on `vars.CI_NATIVE_EXEC_RUNNER || 'ubuntu-latest'` behind the same
  fork, dependency-bot and `CI_SELF_HOSTED` guards; both aggregators `needs:` it
  (#3662). Every gating `go test` skips the flake quarantine
  (`scripts/ci/test-quarantine.tsv`, one open ticket per row), which the
  nightly `test-quarantine` workflow runs instead (#3663).
  The legs
  themselves carry the tap skip and nothing else, a contract `internal/ciguard`'s
  `gatedJobs` enforces: a leg any other condition skips reports Success on the
  pull request while reddening its queue entry forever, and a leg without the
  skip spends the heavy minutes the skip exists to save. The one exception is
  the heavy legs, which also skip a light run, the merge queue included
  ([below](#light-runs-heavy-main)). `fmt-check` is
  STAGED: it reports on the pull request AND in the queue, so the context is
  already there the day the ruleset names it, and `internal/ciguard`'s
  `requiredChecks` carries it. Editing that ruleset from the API needs
  `PUT /repos/{owner}/{repo}/rulesets/{id}` with the **complete** representation
  (`name`, `target`, `enforcement`, `bypass_actors`, `conditions`, `rules`); a
  `PATCH`, or a `PUT` missing any of those, answers `404` — which reads exactly
  like a permission ceiling and is not one. Read the ruleset first and send it
  back with the one field changed. The `nats-conformance` job runs the JetStream
  schema-rollout integration tests (#481); until it is required, a regression
  there merges green. The slow container-image build is intentionally NOT
  required — it builds on merge to `main` and would stall the queue 12 min/PR.
  A required check carries no `if:`, with one verified exception: the
  Homebrew tap update ([below](#the-one-verified-skip-the-homebrew-tap-update))
  — which the two aggregators carry combined with their `!cancelled()` clause.

  > **Promoting a check to required is a two-file change.** The seven advisory
  > jobs — `nats-conformance`, `helm-lint`, `govulncheck`,
  > `desktop-vet-linux`, `desktop-vet-cross`, `docs-links`, `docs-build` — carry
  > `if: github.event_name != 'merge_group'` in `.github/workflows/tests.yml`,
  > and the heavy `cloud-e2e` and `mongo-conformance` the heavy literal, which
  > skips the queue too: a job that cannot block a merge should not hold a
  > runner slot the queue needs. (`fmt-check` is not listed above and carries no merge-queue skip: it is STAGED
  > to become required — named in `internal/ciguard`'s `requiredChecks`, not
  > yet in the ruleset — and stays in the queue so its context exists the day
  > it names it.) Adding one to this ruleset
  > **without deleting its skip** is worse
  > than a stalled queue: a job skipped by a job-level `if:` reports
  > **Success**, so the required check is satisfied by a job that never ran —
  > every entry merges green on a check that did not execute. (A
  > *workflow*-level filter is the opposite: the check never reports and the
  > queue hangs until `check_response_timeout_minutes`. Same word, opposite
  > failure, and the silent one is the one this file's skips produce.)
  > Nothing in the repository can catch it — the required list lives in the
  > ruleset — so the two edits go together, by hand.

  > **An advisory check that nobody reads is not a check.** `docs` is
  > deliberately not required — blocking a hotfix on a documentation build
  > would be the wrong trade — but the consequence is that a broken publish
  > rides `main` in silence: the queue never looks at it, so every following
  > merge inherits a red `main` without having caused it. Measured
  > 2026-09-10: one wrong file extension in a doc link (`monaco.ts` for
  > `monaco.tsx`) kept the site from building across **six consecutive
  > commits and ~100 minutes**, found only by reading the run list by hand.
  > `.github/workflows/docs.yml` now reports on itself — a failed publish on
  > `main` opens ONE issue labelled `ci:docs-build` (later failures comment
  > on it rather than pile up), and the next successful build closes it. The
  > alert is a state, not a stream. Any other advisory job whose failure
  > nobody would notice wants the same treatment rather than promotion to
  > required.
- **Every required check from `tests.yml` runs on self-hosted runners** —
  `test`, `vendor-check`, `golangci` and `brand` route to the organisation's
  `arc-runners` scale set on `merge_group`, and the heavy `race` shards and
  `mongo-conformance` route there on push, the nightly, a dispatch and
  `ci:full`, because
  the 20-job cap above is what makes a cycle slow. That scale set is **outside this repository**, and it was dead
  and unnoticed for over a year before 2026-09-08.

  Three runner classes route it (#3694), and every self-hosted job of
  `tests.yml` and `merge-queue-gate.yml` is classified into one by its
  measured peak. Each class ends its `runs-on` expression on its own
  repository variable, `vars.CI_<CLASS>_RUNNER || 'arc-runners'`:

  | class | variable | jobs |
  | --- | --- | --- |
  | small | `CI_SMALL_RUNNER` | the `test` and `race` aggregators, `fmt-check`, `vendor-check`, `govulncheck`, `docs-build`, `desktop-vet-cross`, `brand`, `helm-lint`, the merge-queue-gate mirror |
  | large | `CI_LARGE_RUNNER` | the heavy jobs the queue requires: `test-unit`, `golangci`, `studio`; and `nats-conformance`, whose JetStream suite does not fit 500m under its 5-minute package bound |
  | heavy | `CI_HEAVY_RUNNER` | the heavy layer the merge queue does not require, run after the merge (a `ci:full` pull request's `test` and `race` still wait on it): `race-shard`, `test-e2e`, `mongo-conformance`, `cloud-e2e` |

  The variable names the class's scale set (the small one is
  `arc-runners-small`, 500m / 1 Gi). **Unset means `arc-runners`**: with a
  variable empty, its jobs route exactly as before, so setting it is the
  switch and clearing it is the rollback. The three spellings keep every
  clause — the `CI_SELF_HOSTED` lever, the fork guard and the dependency-bot
  guard — and `internal/ciguard`'s `TestSelfHostedRoutingExpressionsAgree`
  pins them byte for byte, one base with only the tail swapped, and which job
  carries which: a new job in those two files whose `runs-on` names
  `arc-runners` or a class variable is classified in that test, never
  silently accepted. `test-native-exec` is pinned by
  `native_exec_runner_test.go` to the same base with its own
  `CI_NATIVE_EXEC_RUNNER` tail, whatever class its neighbours take: no class
  variable can send its Bubblewrap mount to an ARC pod. The other workflows on
  `arc-runners` (`arc-canary`, `arc-kind-probe`, `test-quarantine`) are
  outside this guard and stay on `arc-runners`. `arc-canary` probes
  `arc-runners` only: once a class variable is set, a dead scale set behind
  it stalls that class unpaged — for `CI_SMALL_RUNNER`, the required `test`,
  `vendor-check`, `brand` and the queue branch's `revi/review` mirror (and
  the unrequired `fmt-check`); for `CI_LARGE_RUNNER`, the required `golangci`
  and, through its legs, `test` — until the queue's
  `check_response_timeout_minutes`; for `CI_HEAVY_RUNNER` no queue check, but
  a `ci:full` pull request's required `test` (through `test-e2e`) and its
  `race` (through `race-shard`) wait too, and with them the admin-merge lane
  that requires them; so do the heavy jobs of the push witness and the
  nightly, and with them the next push run. Set a
  variable only once the canary (or a probe) covers its scale set (#3694).
  The repair is clearing that variable, or `CI_SELF_HOSTED=off`.

  > **If nobody can merge and the checks never report, this is the first thing
  > to try.** Set the repository variable **`CI_SELF_HOSTED` to `off`**
  > (Settings → Secrets and variables → Actions → Variables) and every job
  > goes back to GitHub-hosted runners on the next run. A variable rather than
  > an edit to `tests.yml`, deliberately: repairing by merging does not work
  > when merging is what is broken. Unset means on. Alerting on the scale set
  > itself is still missing — SocialGouv/iterion#983. Since the mega-job split,
  > one queue entry holds ~12 concurrent GitHub-hosted jobs on this lane (the
  > gated legs plus the race-shard matrix): the escape hatch still works, but
  > it now spends 12 of the org's 20 shared slots per entry — one fork PR from
  > another repository can starve it, so expect slowness, not silence.

- No required human approval (`required_approving_review_count: 0`) — the bot
  factory's own adversarial review + the checks are the gate; a reviewer still
  merges deliberately.

## Light runs, heavy main

Decided 2026-11-25, in two steps. Every pull request used to pay the whole
suite twice — on its own run, then on its `merge_group` run — about 25 ARC
jobs each on a scale set of ~40 concurrent pods; queue latency reached 4-14
hours and 8 of 12 merge-group runs were cancelled. #3683 took the heavy legs
off the light pull request; the
[experimental delivery mode](adr/173-experimental-delivery-mode.md) (#3691)
takes them out of the merge queue too, and runs them after the merge. So each
layer gates where it pays:

| layer | pull request | merge queue | push to `main`, nightly, dispatch |
| --- | --- | --- | --- |
| `test-unit`, `studio`, `test-native-exec`, `vendor-check`, `golangci`, `brand`, `fmt-check` | runs | runs | runs |
| `race-shard`, `test-e2e` (heavy legs) | skipped, unless labelled `ci:full` | skipped | runs |
| `mongo-conformance`, `cloud-e2e` (heavy, no aggregator) | skipped, unless labelled `ci:full` | skipped | runs |
| other advisory jobs | runs | skipped | runs |
| `revi/review` | runs | mirrored | — |

The nightly is `tests.yml`'s own `schedule` (`47 1 * * *`): the full suite on
`main` once a day, in a concurrency group of its own so no merge cancels it.
Every push to `main` shares one concurrency group that never cancels the run
in progress: at most one push run is in flight and one pending, a newer push
replacing the pending run. The heavy load after merges is thus one run at a
time, the witness of the merged batch — every pull request merged since the
last run started — not one heavy run per merge. A red push run names a batch,
and main-watch's suspects are the pull requests merged since the last green
run.

Automatic releases (`version.yml` on a merge and nightly) can tag and publish
a commit whose heavy layer has not yet run or has not yet finished: a heavy
regression can ship in a released version before the push witness or the
nightly judges it. Gating the release on a green Tests run for its sha is a
deferred decision.

`test-unit` stays on the pull request: it is the fast signal (sharded three
ways, a 30-minute ceiling against the heavy legs' 50) and carries the
freshness gate, gofmt and vet — the checks behind the queue's most frequent
ejection, two pull requests regenerating the same artefact.
`test-native-exec` stays too: one small job on a GitHub-hosted VM.

The aggregators stay honest. `test` and `race` read `LIGHT_RUN`, the negation
of the heavy legs' own condition, evaluated by GitHub, and accept `skipped`
from a heavy leg only when it is `true` — in `merge_group` and on a pull
request without `ci:full`; on push, the nightly, a dispatch and a `ci:full`
pull request it is `false`, and a skipped heavy leg is as red as a failed one.
`internal/ciguard` pins the condition byte for byte on the heavy legs,
`mongo-conformance`, `cloud-e2e` and both aggregators, evaluates it per event,
and runs each aggregator's step under the result.

To run the heavy jobs on a pull request — `race-shard` and `test-e2e` for a
change to concurrency, the race-sensitive engine, or e2e itself;
`mongo-conformance` for a change to anything Mongo-backed;
`cloud-e2e` for a change to `charts/` or the cloud stack — add the `ci:full`
label, then push: a label alone starts no run, since `labeled` is not among
the triggers.
A light pull request's green `test` and `race` never ran the heavy legs, so it
is not admin-mergeable: an admin merge also gives up the queue's rebuild
against the entries ahead of it, so it owes the heavy legs on the pull request
(the [parked gate](#a-parked-gate-decided-2026-11-10) and the
[fast lane](#the-fast-lane-for-an-incident-fix-and-its-exact-procedure) say
what they owe).

**A red `main` is owned, not waited out.** The heavy layer now finds its
regressions after the merge, on the push run or the nightly. Until the
main-watch workflow (#3692) does it on its own, the session that sees a red
push or nightly run of `tests.yml` reruns the failed jobs once
(`gh run rerun <id> --failed`): green on rerun, the failing test is a flake —
a row in `scripts/ci/test-quarantine.tsv` with its open ticket; red again, it
is a regression — revert the merge that brought it, or fix forward, within
four hours. The mode's exit criteria (a regression rate, a flake count, an
untriaged red `main`) are in the ADR, and ending it puts `race` and
`mongo-conformance` back in the ruleset and the heavy legs back in the queue,
in one change.

## main-watch: a red main is rerun, quarantined or reverted

Decided 2026-11-25 (#3692). The queue lets the heavy layer reach `main`
unproven, so `main`'s own runs are the witness, and a deterministic workflow,
`.github/workflows/main-watch.yml` (no agent), acts on every run of
`🧪 Tests` on `main`, push or schedule:

| what the run shows | what main-watch does |
| --- | --- |
| red on attempt 1 | reruns the failed jobs, once, only if it is `main`'s newest run (push or nightly): a rerun joins the concurrency group and could cancel a newer run, so an older red run is left to the newer one, which main-watch names |
| green on the rerun: a **flake** | a `flaky:` ticket per failing test under epic #1410, or a comment on the open one (a person's `flaky:` ticket naming the package and the test counts), and one pull request adding the rows to `scripts/ci/test-quarantine.tsv`, auto-merge armed once — never again once it lost its arming: disabled by a person or GitHub, removed from the merge queue, or held by it |
| red on the rerun: a **regression** | one `main red` ticket naming the failing tests and the pull requests merged since the last green run |
| still red 4 h after the red run started | a pull request reverting the most recently merged suspect, auto-merge armed before the ticket records it, and a comment on the original; a revert that cannot be armed is recorded on the ticket for a person; a revert opened and armed whose record failed is recorded by the next pass, never armed again |
| green (a flake pass that fails still lets it close the episode) | every revert pull request of the episode still open closes, armed or not (named on the ticket, or found by its suspect's `main-watch/revert-N` branch), then the `main red` ticket closes |

Failing tests are read from the failed jobs' logs (`--- FAIL` lines, and the
tests a `-timeout` panic lists). A job that failed with no FAIL line is
infrastructure: rerun only, never quarantined, never reverted. A job past its
`timeout-minutes` concludes cancelled: the ticket names it as a hang (and the
`<pkg>.test` the runner killed), never quarantined, never reverted. A rerun
cancelled by a newer push leaves no verdict: main-watch says so and lets the
newer run judge `main`. A row is added
only for a test of a job that skips the list (`test-unit`, `test-e2e`,
`race-shard`, `test-native-exec`, read from `tests.yml` itself); a flake in
another job gets its ticket and no row, and so does a flake in a skipping job
whose `runs-on` never names the scale set (`test-native-exec`, on a VM for
its Bubblewrap mount): the nightly witness runs on the scale set with
checkout and Go only, so the row would redden it instead. Suspects come from the `(#N)` squash
subjects between the two runs' commits; the most recent is the one merged
last. A `(#N)` naming an issue (a commit landed outside a pull request) hands
the revert to a person. main-watch never reverts a revert or one of its own `main-watch/*`
changes, reverts once per red episode, and hands a revert that does not apply
to a person on the ticket.

A half-hour schedule runs the revert clock and judges `main`'s newest run, so
a missed event is caught; markers in the tickets and branch names keyed by run
and by pull request make a second judgement a no-op, and a flake pass that failed
half-way is finished by the next, its sub-issue filing included, a ticket a person filed under another issue left there; a quarantine
pull request merged or closed gets nothing pushed. A marker counts only in an issue or comment
written by `github-actions[bot]`, and a person's `flaky:` ticket only when its
author is an owner, member or collaborator: anyone can open an issue here. The job's
`GITHUB_TOKEN` reruns, writes the tickets and comments on the pull requests; the token-bureau App token
pushes the branches and opens the pull requests, whose checks a
`GITHUB_TOKEN` pull request would never start. The job runs only for push and
schedule runs of `main` from this repository, checks out `main`, interpolates
nothing into a shell, and `scripts/ci/mainwatch` refuses any other run again;
`internal/ciguard/main_watch_test.go` pins each line.

To see a decision without acting, dispatch main-watch with `dry_run` (the
default) and a `run_id` — everything is read, every write printed. To stop it,
disable the workflow (`gh workflow disable main-watch.yml`); to keep a
reverted change, close its revert pull request and fix `main`.

## The one verified skip: the Homebrew tap update

`brew-update.yml` opens one pull request a night, `chore(brew): update tap to
vX`, that moves the `version` and `sha256` values of `Formula/iterion.rb` and
`Cask/iterion-desktop.rb` and nothing else. No heavy check's verdict depends
on those values (`internal/ciguard`'s tap tests rewrite them), and running the
checks anyway cost ~57 minutes of compute per pass — on the pull request, then again in the
queue, where the entry held one of the three build slots for ~25 minutes
(`test` 24.1, `race` 22.3, `mongo-conformance` 4.0, `golangci` 3.2,
`fmt-check` 1.8, `vendor-check` 1.0 min, measured on #2314).

So the gated legs (`test-unit`, `test-e2e`, `studio`, `test-native-exec`,
`race-shard`; the heavy two combine it with the light-run clause), the
aggregators `test` and `race` that speak for them (combined with their
`!cancelled()` clause), `golangci`, `vendor-check` and `fmt-check` carry one
condition, the only `if:` a required check may carry
here: they skip a pull request whose title, or a queue entry whose commit
message, starts with `chore(brew): update tap to ` (the queue squashes a
single-commit pull request under its commit subject — the repository's
`COMMIT_OR_PR_TITLE` — so `brew-tap-pr.sh` puts the prefix in both). A title is
only a claim and a skipped job reports **Success**, so the claim is verified
where nothing can skip the verification:

- `brand` — required, never skipped, and already a reader of both tap files —
  runs `scripts/ci/tap-entry-check.sh` with the **same predicate, evaluated**
  (never re-matched: GitHub's `startsWith` ignores case). It fails unless the
  diff touches the two tap files alone, matched exactly and with rename
  detection off, and moves only their `version` and `sha256` values, each
  from one value `update-brew-tap.sh` writes to another (a dotted numeric
  version, a 64-hex digest) — a line added, a comment, a `desc`, a Ruby
  interpolation or an empty digest is refused: the pull request's test merge
  against its base, the queue entry against the entry it was built on.
- In the queue it also reads the **live grouping strategy**
  (`GET /repos/{owner}/{repo}/rules/branches/main`) and fails unless it is
  `ALLGREEN`. Entries stack — each is built on the one ahead of it — so a tap
  entry's skipped checks vouch for nothing the entries ahead brought.
  `ALLGREEN` merges an entry only on its own checks; under `HEADGREEN` a green
  tail would merge everything ahead of it. **Switching the queue to
  `HEADGREEN` ejects every tap entry** until the skip leaves `tests.yml` — by
  design.
- `internal/ciguard` holds the pieces together: the expression byte for byte
  on each job, `brand` without it and without `needs:` (a job whose dependency
  is skipped is skipped too, and passes), the verifier step unconditional (no
  `if:`, `continue-on-error`, `shell:` or `defaults:`) and fed the same
  predicate, the tap pull request's title and commit subject starting with it.

The verdict that counts is the queue's. On the pull request the verifier
judges the test merge against the base it was built on, and retargeting a
pull request (an `edited` event) re-runs nothing; the queue judges every entry
again against the entry ahead of it, and a direct merge bypasses every check
anyway. The workflow file is the pull request's own, so a pull request that
also edits `tests.yml` can drop the verifier with the rest — the class every
check here shares, which review and `revi/review` answer, not this check.

A code pull request titled like a tap update does not merge: its heavy checks
skip, `brand` refuses it, and the entry leaves the queue. A push to `main`
evaluates the predicate false, so `main` still runs everything.

The tap pull request also opens at night. `brew-update.yml` runs when
`version.yml`'s nightly run completes (00:03–02:21 UTC on its 27 runs since
2026-09-11), not on a cron of its own: GitHub fired its 04:37 UTC cron between
10:46 and 12:21 UTC, inside the queue's working hours. Over 2026-09-12..10-12
each hour from 00:00 to 05:59 UTC saw 2-7 merges in all, against 19-37 for
each hour from 06:00 to 21:59. It taps the newest tag whose CLI artefacts are
complete, so the nightly's own tag, still building, waits for the next night;
a cask left behind is caught up once its release holds the desktop ZIP; and
nothing walks the tap back — `update-brew-tap.sh` never writes a version
below the one a file carries, and `brew-tap-pr.sh` opens a pull request only
for what `main` does not carry yet, never from a branch whose files sit below
`main`'s (tap branches outlive their merged pull request here). A
desktop-only release (its CLI build red) reaches the cask only through a
dispatch of `brew-update.yml` with its tag, as long as the formula has not
moved past that tag.

## Direct pushes (admin bypass)

Repository **admins bypass the queue** (`bypass_mode: always`) — a hotfix can
still be pushed straight to `main` (e.g. un-break a red main fast). Use it
sparingly; the queue is the default path. Non-admins must go through a PR + the
queue.
For reviewed chantier work, the [operator direct-landing lane](#the-operator-direct-landing-lane-decided-2026-10-22) generalises this bypass — and owes it what a hotfix does not: the train, the locally reproduced gate and the greppable trailers.

### A parked gate (decided 2026-11-10)

`revi/review` can stay `PENDING` for days with nothing wrong with the pull
request: `iterion-forge-core[bot]` posts a `<!-- iterion:gate-paused -->`
comment naming a **dated automatic resume** and the upstream error. Measured on
2026-11-10 (#3132): a 429 `[1310] Weekly/Monthly Limit Exhausted` on the
reviewer node parked the run for ~103 h while the other six required checks
were green. Read that comment before concluding anything about the PR, and
before pushing again — a push restarts the review sooner but lands on the same
exhausted quota.

**The first move is not this bypass.** It is the one
[review-and-merge.md](agents/workflow/review-and-merge.md#a-parked-gate-is-a-rerouting-signal-not-a-wait)
names: identify the blocked credential and **lend a live one** at runtime
(`iterion remote admin llm oauth set …`, no redeploy), which needs the
operator's explicit go. This bypass is for when that is unavailable or refused.
Re-shaping the *pools* under pressure is a different act — deployment
configuration ([sovereign pools](sovereign-pools.md)), decided outside an
incident.

**Then the operator (admin) may merge that pull request, bypassing the parked
gate.** What it owes:

1. **The other required checks green, read as a SET** — `gh pr checks`, not
   the one you are waiting on. The required set is exactly `test`,
   `vendor-check`, `golangci`, `brand` and `revi/review` (ruleset 18857412),
   and the bypass owes the held `race` and `mongo-conformance` green too, as
   it did before the [experimental delivery mode](adr/173-experimental-delivery-mode.md)
   held them out; the legs behind the two aggregators (`test-unit (n)`,
   `test-e2e (n)`, `studio`, `test-native-exec`, `race-shard (n)`) are deliberately **not** required and
   are where the work is. Queued, cancelled, missing **or skipped** is not
   success: a job skipped by a job-level `if:` reports SUCCESS, so a required
   check that never ran satisfies the gate
   ([tests.yml](../.github/workflows/tests.yml) says so of the Homebrew-tap
   skip, which `test`, `race`, `vendor-check`, `golangci` and `fmt-check`
   carry). On a [light pull request](#light-runs-heavy-main) `test` and
   `race` turn green with the heavy legs skipped and `mongo-conformance`
   skipped: before the admin merge the pull request carries the `ci:full`
   label, has been pushed since, and every `race-shard (n)`, `test-e2e (n)`
   and `mongo-conformance` reads `success` on its head, not `skipped`.
2. **A local adversarial loop, and the trailer says which kind.** At the bar
   [the loop](agents/workflow/adversarial-review-loop.md) states — a round on
   the diff, each finding AND its proposed fix verified independently, a
   bounded closing round on the corrections — it carries the gate's own weight
   (operator arbitration, 2026-11-11), and nothing is being traded away.
   **Below the bar it stands in with a measured residual**: of 8 logged gate
   verdicts, 4 found a class the converged internal rounds had skirted, and
   those rounds were below it. That residual is assumed explicitly in the
   merge, and it is **not assumable on a wide diff** — there, reach the bar,
   wait for the window, or lend a credential, as the incident lane scopes
   itself the same way ("reckless when it is a wide refactor", below).
3. **The checks belong to the head AND to the base being merged.** A commit
   pushed after they ran invalidates them. Less obvious and more dangerous:
   `strict_required_status_checks_policy` is **false** on this ruleset, so a
   green set computed against an older `main` stays green and GitHub neither
   re-runs nor flags it. Re-pushing refreshes the merge ref, but it does not
   converge here: `main` advances every 20–40 minutes on a busy day — the release
   bot pushes with no PR at all — so a CI cycle finishes behind the tip it was
   chasing, and the queue, which would settle it, is the thing the parked gate
   closed. So **build the integrated tree locally and run the gate on it**: a
   fresh worktree on `origin/main`, `git merge --no-ff` each branch going in, then
   the gate members the diffs touch. That tests the merge ref, which the non-strict
   policy never does. Measured on 2026-11-10 with four branches: `main` had
   re-pinned `docs/merge-gate.md` in the reading golden and one branch changed the
   **adjacent** line (`docs/merge-policy.md`), so the hunks overlapped — green
   apart, conflicting combined. Resolve a golden conflict by regenerating it
   (`git checkout --ours`, then `corpus-reading --write`, then check only your own
   files moved), never by picking a side; and note `rerere` will offer a replay
   from the train merge that is not valid for a rebase.
4. **The trailers, greppable** — `Adversarial-Rounds:` / `Adversarial-Model:`
   (the reviewer marked **self** when the loop was a self-review), plus
   `Gate-Bypass: revi/review parked (quota) — #<pr>` in the squash body, since
   an ungreppable bypass is a bypass that did not happen.
5. **Verify what landed, and stop a reviver.** As for any admin merge:
   `git diff --stat` the landed tree against what you reviewed, and check
   `iterion remote runs list` for a parked review run that a redelivery
   re-claimed after the close
   ([the known gap](merge-gate.md#a-paused-review-says-so-on-the-pr)).

**What this bypass gives up, stated plainly.** The required set ran on real CI,
on the merge ref as of its run, the heavy legs included through `ci:full` —
that is its strength over a locally reproduced
gate. What it does not have is the queue's one unique guarantee: the rebuild
against **current** `main` and against the entries ahead of it, which is what
closes the semantic inter-PR conflict class (two PRs each green against
`main`-at-branch-time that break `main` combined — regenerated artefacts such as
the reading golden being the most frequent shape). On that axis this bypass is
weaker than both the queue and the direct-landing lane's train.

**What it does not license**: merging while another required check is red,
skipped or unresolved; skipping the loop because the diff looks small; or
reading the pause as a verdict. And `bots/review-pr` is the gate the tenant
repositories that require its context lean on — a parked gate is a reason to
lend it a credential, never to quietly lower what it guards.

### The fast lane for an incident fix, and its exact procedure

The queue is a single writer with one lane: a green incident fix waits behind
every comfort PR enqueued before it. Measured on 2026-09-06, a fix that closed
a production incident class waited **4 h 42** at ~1 PR/h, with the head group
rebuilt every ~15 minutes (each rebuild restarts the ~35-minute test workflow
for every member), while `estimatedTimeToMerge` reported ~2 h throughout.

There is **no automatic fast lane, by decision**: a label-driven direct merge
would bypass the very rebuild that closes the semantic inter-PR conflict class
the queue exists for. The escape hatch is a **human admin**, deliberately, and
this is how it is done — the shape matters, because two of the three obvious
invocations do the wrong thing:

```sh
# 1. `gh pr merge <n> --squash` WITHOUT --admin does NOT merge: it enqueues.
# 2. `--admin` on a PR already in the queue answers "already queued".
# So: dequeue first, then merge with --admin.
gh api graphql -f query='mutation($id:ID!){ dequeuePullRequest(input:{id:$id}){ clientMutationId } }' \
  -f id="$(gh pr view <n> --repo <owner>/<repo> --json id --jq .id)"
gh pr merge <n> --repo <owner>/<repo> --squash --admin
```

Not for one pull request of a burst whose other entries sit queued:
[one lane per burst](#one-lane-per-burst-decided-2026-11-24).

Before using it, the same proofs the queue would have demanded must already be
green **on the PR head**: every required check (`test`, `vendor-check`,
`golangci`, `brand`), the held `race` and `mongo-conformance`, and
`revi/review`. A light pull request's `test` and `race` are green with the
heavy legs skipped, so the PR first carries the `ci:full` label and a push
after it, and every `race-shard (n)`, `test-e2e (n)` and `mongo-conformance`
reads `success` on the head, not `skipped`. With that, the bypass skips the *rebuild against the queue's other
members*; without it, it also skips `-race` and e2e — so it is legitimate
when the PR is small, or touches files no queued PR touches, and reckless when
it is a wide refactor.

After an admin merge, **verify what actually landed**: the queue has merged a
stale head before (see the note below), and a bypass has no group build to
catch it.

```sh
git fetch origin main
git diff --stat <pr-head-sha> origin/main -- $(git diff --name-only "$(git merge-base <pr-head-sha> origin/main)" <pr-head-sha>)
```

An empty diff means the merge carries the head you reviewed.

Two consequences worth stating plainly:

- **The autonomous pilot cannot use this.** An agent has no admin rights and
  must not be given them; an incident fix launched by a bot waits in the queue
  like everything else, or a human takes it through the hatch above.
- **The durable fix is queue throughput, not exceptions.** Every direct push to
  `main` invalidates every merge group in flight: each one rebuilds from
  scratch on the new base. That is why the brew-tap update lands through the
  queue as its own pull request, and why the release waits for the queue to
  drain ([below](#releases-and-the-queue)); an exception buys the rebuild back
  for one PR only.

## Releases and the queue

A release is a direct push to `main` — `version.yml` runs release-it, which
commits `chore: release vX.Y.Z` and its tag, the token-bureau App bypassing
the ruleset — so it invalidates every merge group in flight like any other
direct push. Releasing after every merge discards the build of whatever is
queued behind the merged PR while it runs. Measured with `task ci:queue-stats`
over 2026-09-22..28: 58 releases for 102 merges, and 20 queue
builds discarded by a direct push while they ran — 15 by a release commit, 5 by
a pull request merged directly (31 of the 102 skipped the queue through the
admin bypass, and each of those is a direct push too). Over 2026-09-01..28: 387
of 1096 queue builds, 242 of them by a release.

So `version.yml` holds a **merged pull request's** release while a merge group
is building — any `gh-readonly-queue/main/*` branch exists — and the merge that
drains the queue releases everything merged since the last tag. The **nightly**
(22:27 UTC) and a **`workflow_dispatch`** release regardless, without reading
the queue at all: the nightly is the backstop for a queue that drains without a merge (its
last entry ejected) or never drains, and a dispatch is an operator's explicit
request. A release delayed by a busy queue therefore waits at most until the
next drain or the next nightly; to ship now, dispatch it.

GitHub sometimes leaves a `gh-readonly-queue/main/*` branch behind after its
group merged (seen on mastodon, cilium, zed, flutter). Counted as a live
group, one such orphan would hold every merged PR's release until the nightly.
A branch is named after the pull request at the tail of its group
(`pr-<n>-<base>`), and a group only exists while that pull request waits in
the queue, so the hold counts a branch only while its pull request is **open**
— and, as a backstop for one dequeued but kept open, while its head commit
(dated when the entry was queued; rebuilds keep the date) is less than a day
old; from their last enqueue to their merge, the 221 queue merges of
2026-09-15..29 waited 16 minutes at the median and 80 at most.
The job's log names every branch it ignored with its pull request's state:
check it, then **delete it** — `gh api -X DELETE
repos/SocialGouv/iterion/git/refs/heads/gh-readonly-queue/main/pr-<n>-<sha>`.

The hold is **best effort, not a lock**. The branch list is read just before
the run moves to `main`'s tip — in that order, so a group that merges between
the two holds the release rather than leaving release-it on a stale tree — and
release-it still builds for a few minutes before it pushes; a group that starts
between the read and the push rebuilds once. What is left is measured, not
assumed: `task ci:queue-stats` counts the queue builds discarded by a release
([Measuring the queue](#measuring-the-queue)) — which also counts the nightly
and dispatched releases that invalidate on purpose; the script cannot tell them
apart from a merged PR's release that slipped through the window.

Two release runs never overlap, whatever triggered them: `version.yml` has one
concurrency group for every trigger, with `queue: max`, and each run that
releases moves to `main`'s tip before release-it — a run queued behind a
release finds it already tagged and releases nothing (a nightly or a dispatch
checks for new commits first, since a dispatch's explicit increment would
otherwise cut an empty release). Overlap is what must never happen — when a
push is rejected, release-it rolls back by deleting the remote tag of the
version it computed, and if the other run had just pushed that same version,
that is the other run's tag. `queue: max` is there because a group otherwise
keeps a single pending run and silently replaces it with the next one: a merged
pull request's run could replace a pending nightly or dispatch, then hold its
release while the queue builds.

## The major hold

release-it cuts a major from a single `feat!:` commit, and an explicit
dispatch increment overrides the bump its changelog recommends — so the first
breaking change after a `v7` tag would auto-cut `v8`, and a dispatched `minor`
would ship the breaking changes under a 7.x minor. `version.yml` therefore
asks a **major hold** before release-it; the decision lives in
[`scripts/ci/release-major-hold.sh`](../scripts/ci/release-major-hold.sh),
exercised by `internal/ciguard/release_major_hold_test.go`:

- an **automatic run** — a merged pull request or the nightly — whose
  recommended version is the next major ends **green** with a notice naming
  the version: no tag is cut, main keeps carrying the breaking change;
- a **dispatch** of `patch`, `minor`, an explicit same-major version or a
  blank input on `main` is **refused** (red) while a major is pending;
- only the dispatch of the **exact** `N+1.0.0` cuts the major; the word
  `major` and any higher version are refused with that message;
- a ref other than `main` is not guarded here — the hold protects main alone,
  and the `release/*` line owns its own rules.

With no breaking change pending, every run releases exactly as before. The
release hold decides *whether this run may tag* — how the major is cut, and
when, is a separate decision, not this hold's.

## A rebuilt group's older runs are cancelled

GitHub rebuilds a queue group whenever its base moves — a push to `main`, an
entry ahead failing or leaving — and leaves the discarded group's runs to
finish. They hold `arc-runners` pods for a verdict nobody reads, which is the
churn half of the queue's cost: over 2026-10-21/22, 14 of 38 completed
`merge_group` runs were red and the five pull requests involved were rebuilt
repeatedly, each rebuild leaving a full Tests run behind it.

[`.github/workflows/merge-queue-churn.yml`](../.github/workflows/merge-queue-churn.yml)
cancels them, and only them. The branch of a queue entry is
`gh-readonly-queue/<base>/pr-<n>-<base sha>`: the `pr-<n>` prefix survives a
rebuild, the base sha is the generation. A run is cancelled when it carries
the same prefix as the run doing the cancelling, on a different branch, is not
completed, and was created strictly before it. So the newest generation kills
the ones it replaced; every job of one generation shares its branch and cannot
touch its siblings (the three `race` shards included); an equal timestamp
cancels nothing, since a tie is not an order and the cost of guessing is a
required check reported `cancelled` — a red entry.

Required check NAMES are untouched: the janitor is its own workflow, nothing
requires it, and the ruleset never waits for it. It runs on `ubuntu-latest` on
purpose — queued behind the pod shortage it exists to relieve, it would arrive
after the runs it was meant to kill — with `actions: write` as its only scope
and no checkout. `internal/ciguard/queue_churn_test.go` runs the shipped jq
filter against crafted run listings, in both list orders.

## Measuring the queue

`devbox run -- task ci:queue-stats -- --since 2026-09-22 --until 2026-09-28`
(`scripts/diagnostics/queue-stats.sh`, read-only, needs an authenticated `gh`)
answers the three questions a queue change has to be judged on, over a fixed
window in UTC so two periods compare:

- **how long a merge takes** — minutes from a pull request's first enqueue to
  its merge, p50/p75/p90/max, per author (the brew-tap bot's PRs are counted
  apart), for the pull requests the queue merged; the ones merged directly —
  the admin bypass, a dequeue followed by an admin merge included — are
  counted apart, since each is a push to `main`. Also the removals from the
  queue that were not a merge (`failed_checks`, `merge_conflict`, `manual`),
  among the pull requests merged in the window;
- **what the window paid for** — the Tests runs on `merge_group`, and for
  every rebuild, its cause, read from the new group's **base** — the sha
  `gh-readonly-queue/main/pr-<n>-<base sha>` carries: a base that moved to a
  direct push to `main` (a release, a direct merge, any other push) IS that
  push, whatever the delay, while an unchanged base, a queue merge or a
  speculative queue branch is the queue itself (an entry ahead failed or left,
  a dequeue and a re-enqueue) — and the state of the build it replaced: for a
  push, whether it was already green or still running when the push hit
  (GitHub does not cancel the run of a group it discards — the janitor below
  does, so a discarded build now reads as `cancelled` rather than as a full
  run nobody waited for; a build that had already finished red had ejected its
  pull request, so its rebuild is a re-enqueue, counted as the queue's).
  **Queue builds discarded by a direct push** is the line the release hold
  above exists to shrink. The attribution is
  [`queue-rebuild-cause.awk`](../scripts/diagnostics/queue-rebuild-cause.awk),
  its own program so a harness can run it:
  `internal/ciguard/queue_stats_test.go` holds each of its rules to a crafted
  table — the previous rule blamed a push only within 90 seconds of the
  rebuild, and GitHub rebuilt #1967's group 114 seconds after v3.217.0's
  commit, so the discarded build was counted as the queue's own churn. Runs
  are read day by day and checked against the count the API announces: a query
  answers at most 1000 runs, and paging has returned a run twice;
- **what failed the queue** — for each failed build, the failing jobs and
  steps and, unless `--no-logs`, the `--- FAIL:` tests in their logs, ranked.
  A build is named after the pull request at the tail of its group, so a
  failure that an entry ahead caused repeats in every group behind it. A test
  that fails the queue once and never again is a flake candidate; one that
  fails it with a generated-file freshness message is two PRs editing the same
  generated artefact.

## For the bot factory

A bot opens a PR → the operator/agent reviews (the adversarial in-loop review
already ran during the bot loop) → `gh pr merge <n> --auto --squash` enqueues
it. Overlapping bot PRs (e.g. two per-run store features) are then serialized
and rebuilt-combined automatically — no more manual rebase-and-fix.

`scripts/merge-guard.sh` remains as a **local pre-flight** (rebuild the combined
tree before enqueuing) when you want to check overlap before spending queue CI.

## Prerequisite that made this viable

The `race` job was flaky (timing ceilings too tight under the CI `-race`
full-parallel load). A flaky required check stalls the whole queue, so the
dispatcher test deadlines were raised to generous ceilings (commit 5368500f5)
before enabling the queue.

## Reverting

The ruleset is instantly reversible by an admin:
`gh api -X DELETE repos/SocialGouv/iterion/rulesets/18857412`.


### A job reports inotify “too many open files” (#1198)

`inotify_init` can report `EMFILE` for the process descriptor limit or the
real user's inotify-instance limit. The latter can be shared by several
runner containers with the same host UID. Read the job's **Watcher resource
limits** step and the failing process's `watcher resources` error before
choosing a remedy. The error retains its original errno and reports the
real UID, descriptor ceilings, visible process descriptors, an ordinary
file-open observation and the inotify sysctls. It neither changes a limit
nor skips the watcher test. Counts are observations at failure time, not an
atomic inventory of every container on the node.

An ordinary file opening successfully while inotify initialization returns
`EMFILE`, with descriptors well below `nofile_soft`, points toward the
per-user instance ceiling. A failed ordinary open and a reached descriptor
ceiling point toward process FD exhaustion. Confirm with the runner/node
operator before changing shared limits or concurrency. The configuration
in `SocialGouv/infra-apps/arc-runners/values.yaml` controls the shared scale
set; the repository's `CI_SELF_HOSTED=off` switch remains the emergency
fallback, not a silent automatic response to this error.

#### Bounded reproduction on the ARC worker

On 2026-09-13 an ARC CI job reported real UID 1001, an identity UID map,
`RLIMIT_NOFILE=1048576`, `max_user_instances=128` and
`max_user_watches=228254`. A separate diagnostic Job on
`worker-nodepool-node-e6dab0` (Linux 5.15.0-134-generic) reproduced that
128-instance boundary under the previously checked unused UID 2147480000:

```json
{"uid":2147480000,"max_user_instances":128,"nofile":[1048576,1048576],"child":{"held":128,"errno":24},"other_process_inotify_errno":24,"ordinary_fd":"open_ok"}
```

One process held all 128 instances; the next call in it and a second process
failed with `EMFILE`, while `/dev/null` still opened. Closing the descriptors
immediately restored capacity. The Job added no watches, changed no sysctl
and was deleted after completion. No runner remained during inspection, so
this is a reproduction of the shared resource boundary, not a measurement
of the historical CI peak.

The exact [Job manifest](../scripts/diagnostics/inotify-shared-uid.yaml) is
retained for operators. Before an approved repeat, choose the node explicitly
and verify the dedicated UID has no processes/inotify instances on that node;
never substitute the runner UID. The script refuses ceilings above 512, has
no retries, and is bounded by a 120-second deadline and 128 MiB memory limit.
It requires Linux and the pinned image's Python 3. It is an operator diagnostic,
not a CI step or an automatically deployed resource.

A proposed configurable floor is tracked separately in the infrastructure PR
linked from #1198. Keep the ticket open until approved activation and
representative concurrent CI runs demonstrate the result.

## The operator direct-landing lane (decided 2026-10-22)

Operator-led chantiers may bypass the queue entirely: no PR, a **train**
(the branch rebased onto `origin/main`, the full required-check set reproduced
locally ON THE INTEGRATED TREE), then a direct push to `main`. The lane's
contract — the exhaustive local gate, the converged adversarial loop, the
`Adversarial-Rounds:` / `Adversarial-Model:` / `Landing-Train:` trailers, the
post-hoc CI witness, the red-`main` priority, and the duty to warn sessions
with queued PRs or unpushed branches — lives in
[the review-and-merge page](agents/workflow/review-and-merge.md#the-operator-direct-landing-lane-decided-2026-10-22).
Every existing PR keeps the queue and its `revi/review` blocking gate — save
where [a parked gate](#a-parked-gate-decided-2026-11-10) opens the narrower
admin merge, or a burst
[lands end to end](#one-lane-per-burst-decided-2026-11-24); the lane is the
admin bypass generalised from hotfixes to reviewed chantier work, and its bypass is greppable in the trailers by design.

### One lane per burst (decided 2026-11-24)

A burst — several pull requests landing in the same stretch — takes **one
lane, chosen up front**, and keeps it to the end:

- **the merge queue alone**: every pull request goes through the queue
  (`gh pr merge <n> --auto --squash`), and none of its entries is
  admin-merged while the burst lasts; or
- **the direct-landing lane end to end**: dequeue every entry of the burst
  first, run one train of the exact set on the current `main`, then land
  that set; the lane's full proof (the converged adversarial loop and the
  train) replaces their `revi/review`.

Never admin-merge one pull request while other entries of the same burst sit
in the queue. Each direct merge moves `main`, the base of every queued group,
so GitHub rebuilds those groups and their in-flight `merge_group` runs become
verdicts nobody reads ([the rebuild](#a-rebuilt-groups-older-runs-are-cancelled)).
Measured on 2026-11-24: `task ci:queue-stats` counts 22 in-flight
`merge_group` builds discarded by direct merges that day, 18 of them from the
burst's 17 direct merges (it counts only the first group of each rebuilt
chain, so the real cost is higher), on top of the queue's own timeout
evictions.
