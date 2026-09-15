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
  `mongo-conformance`, `golangci`, `revi/review` — and `nats-conformance` once
  an admin adds it to ruleset 18857412. Editing that ruleset from the API needs
  `PUT /repos/{owner}/{repo}/rulesets/{id}` with the **complete** representation
  (`name`, `target`, `enforcement`, `bypass_actors`, `conditions`, `rules`); a
  `PATCH`, or a `PUT` missing any of those, answers `404` — which reads exactly
  like a permission ceiling and is not one. Read the ruleset first and send it
  back with the one field changed. The `nats-conformance` job runs the JetStream
  schema-rollout integration tests (#481); until it is required, a regression
  there merges green. The slow container-image build is intentionally NOT
  required — it builds on merge to `main` and would stall the queue 12 min/PR.

  > **Promoting a check to required is a two-file change.** The four advisory
  > jobs — `nats-conformance`, `cloud-e2e`, `helm-lint`, `govulncheck` — carry
  > `if: github.event_name != 'merge_group'` in `.github/workflows/tests.yml`:
  > a job that cannot block a merge should not hold a runner slot the queue
  > needs. Adding one to this ruleset **without deleting its skip** is worse
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
- **Three required checks run on self-hosted runners** — `test`,
  `vendor-check` and `golangci` route to the organisation's `arc-runners`
  scale set on `merge_group`, because the 20-job cap above is what makes a
  cycle slow. That scale set is **outside this repository**, and it was dead
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
`mongo-conformance`, `golangci`) and `revi/review`. The bypass skips the
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
  `main` — the release commit, and until recently the brew-tap commit —
  invalidates and replays the head group. Removing the tap push from `main` is
  what buys every PR back its second CI run; an exception buys it for one.

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
