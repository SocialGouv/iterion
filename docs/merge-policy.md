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
- **Required checks** (the fast, reliable ones): `test`, `race`, `vendor-check`,
  `mongo-conformance`, `golangci`, `revi/review` — and `nats-conformance` once
  an admin adds it to ruleset 18857412 (a token with `repo` scope can read the
  ruleset but gets a 404 on PUT). The `nats-conformance` job runs the JetStream
  schema-rollout integration tests (#481); until it is required, a regression
  there merges green. The slow container-image build is intentionally NOT
  required — it builds on merge to `main` and would stall the queue 12 min/PR.
- No required human approval (`required_approving_review_count: 0`) — the bot
  factory's own adversarial review + the checks are the gate; a reviewer still
  merges deliberately.

## Direct pushes (admin bypass)

Repository **admins bypass the queue** (`bypass_mode: always`) — a hotfix can
still be pushed straight to `main` (e.g. un-break a red main fast). Use it
sparingly; the queue is the default path. Non-admins must go through a PR + the
queue.

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
