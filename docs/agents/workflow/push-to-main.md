# Push to main — operator runbook

The [operator direct-landing lane](review-and-merge.md#the-operator-direct-landing-lane-decided-2026-10-22)
is admin-only. It requires the exhaustive local gate (`task ci:gate` and its
checked-in mapping of required CI jobs), a converged review loop and an
integration train. If that target or mapping is absent at the checkout's SHA,
the lane is closed: `task check` alone does not replace race, vendor and Mongo
conformance checks. The separate [incident bypass](../../merge-policy.md#the-fast-lane-for-an-incident-fix-and-its-exact-procedure)
retains its own requirements.

## Prepare a fresh worktree

Use a [dedicated worktree](worktrees.md), confirm its branch and base SHA, and
keep all generated files and scratch evidence there. Provision dependencies
before starting a timed gate; setup downloads are not test egress.

```sh
devbox run -- pnpm -C studio install --prefer-offline
# Stage intended new/deleted documentation: generators enumerate git ls-files.
devbox run -- task gen
devbox run -- task corpus:reading
# Only if a deliberately edited pinned document changes the reading corpus:
devbox run -- task corpus:reading -- --write
# Inspect generated diffs before validation.
devbox run -- task check
```

`--prefer-offline` uses the package cache where possible; a cold cache still
needs an explicitly provisioned setup network. A gate run keeps egress denied
by default. A test that genuinely needs an external service names its target,
endpoint and explicit opt-in; opening all gate traffic is not a hang fix.
Missing dependencies, sandbox capabilities and credentials are named refusals,
not invisible success. Record machine effects in the [flake register](../testing/flakes.md).

## Review, integrate, push, witness

1. **Give peers advance notice.** Before the first push of a wave, notify
   sessions with queued PRs and unpushed branches. A main advance invalidates
   merge groups and costs rebases. Identify the scope, base and intended wave.
2. **Review the exact diff.** Run the [adversarial loop](adversarial-review-loop.md),
   verify findings, fix their class and re-attack the fixes. An explicitly
   operator-authorized **self-review + admin bypass** records that it is self
   review, its mutations and residual findings; it is not a fabricated peer
   review or Revi verdict. It does not waive the local gate or train. Existing
   PRs still owe `revi/review` under the normal merge contract.
3. **Integrate before taking the verdict.** Rebase onto current `origin/main`
   in the integration branch. Run the complete local gate on that integrated
   tree, retaining each member's log, exit code, SHA and machine class. Never
   hide an exit code behind a pipe. Fix a red train before pushing; an advance
   of main requires reintegration and a fresh verdict.
4. **Carry the evidence in trailers.** The landing commit contains
   `Adversarial-Rounds:` (actual count, estimate and ceiling),
   `Adversarial-Model:` (actual reviewer, explicitly marked self when relevant)
   and `Landing-Train:` (train and evidence directory). Use the loop's trailer
   format; do not invent counts or a green check for a queued job.
5. **Let the authorized operator land.** Push the reviewed train to main
   without force, respecting the lane's preconditions. Verify the landed SHA
   and follow the CI witness runs. Queued, missing and cancelled runs are
   unverified; a red main takes priority until it is green.
