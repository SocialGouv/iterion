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
  parallel. The queue is tuned against that cap — `max_entries_to_build: 2`
  (at most two entries under CI at once, instead of five) and
  `min_entries_to_merge: 3` (merges land in batches of 3-5, so the workflows
  that fire on every push to `main` — Runner Image, Trivy, Sandbox, Brew Tap —
  run once per batch). A lone PR still merges after
  `min_entries_to_merge_wait_minutes` (5).
- **Required checks** (the fast, reliable ones): `test`, `race`, `vendor-check`,
  `mongo-conformance`, `golangci`, `brand`, `revi/review` — and
  `nats-conformance` once an admin adds it to ruleset 18857412. `fmt-check` is
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

  > **Promoting a check to required is a two-file change.** The eight advisory
  > jobs — `nats-conformance`, `cloud-e2e`, `helm-lint`, `govulncheck`,
  > `desktop-vet-linux`, `desktop-vet-cross`, `docs-links`, `docs-build` — carry
  > `if: github.event_name != 'merge_group'` in `.github/workflows/tests.yml`:
  > a job that cannot block a merge should not hold a runner slot the queue
  > needs. (`fmt-check` is not listed above and carries no skip: it is STAGED
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
  `test`, `race`, `vendor-check`, `mongo-conformance`, `golangci` and `brand`
  route to the organisation's `arc-runners` scale set on `merge_group`, because
  the 20-job cap above is what makes a cycle slow. That scale set is **outside this repository**, and it was dead
  and unnoticed for over a year before 2026-09-08.

  > **If nobody can merge and the checks never report, this is the first thing
  > to try.** Set the repository variable **`CI_SELF_HOSTED` to `off`**
  > (Settings → Secrets and variables → Actions → Variables) and every job
  > goes back to GitHub-hosted runners on the next run. A variable rather than
  > an edit to `tests.yml`, deliberately: repairing by merging does not work
  > when merging is what is broken. Unset means on. Alerting on the scale set
  > itself is still missing — SocialGouv/iterion#983.

- No required human approval (`required_approving_review_count: 0`) — the bot
  factory's own adversarial review + the checks are the gate; a reviewer still
  merges deliberately.

## Direct pushes (admin bypass)

Repository **admins bypass the queue** (`bypass_mode: always`) — a hotfix can
still be pushed straight to `main` (e.g. un-break a red main fast). Use it
sparingly; the queue is the default path. Non-admins must go through a PR + the
queue.

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
gh api graphql -f query='mutation($id:ID!){ dequeuePullRequest(input:{pullRequestId:$id}){ clientMutationId } }' \
  -f id="$(gh pr view <n> --repo <owner>/<repo> --json id --jq .id)"
gh pr merge <n> --repo <owner>/<repo> --squash --admin
```

Before using it, the same proofs the queue would have demanded must already be
green **on the PR head**: every required check (`test`, `race`, `vendor-check`,
`mongo-conformance`, `golangci`, `brand`) and `revi/review`. The bypass skips the
*rebuild against the queue's other members*, nothing else — so it is legitimate
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
  every rebuild, its cause, read from the clock — a direct push to `main` (a
  release, a direct merge, any other push) committed at most 90 seconds before
  the rebuild, or the queue itself (an entry ahead failed or left, a dequeue
  and a re-enqueue) — and the state of the build it replaced: for a push,
  whether it was already green or still running when the push hit (the queue
  does not cancel the run of a group it discards; a build that had already
  finished red had ejected its pull request, so its rebuild is a re-enqueue,
  counted as the queue's). **Queue
  builds discarded by a direct push** is the line the release hold above
  exists to shrink. Runs are read day by day and checked against the count the
  API announces: a query answers at most 1000 runs, and paging has returned a
  run twice;
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
