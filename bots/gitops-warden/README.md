# gitops-warden — "Warden" 🛡

A gitops merge gate. Every merge request on the bound repository is
classified against a POLICY — the platform's rules, not a reviewer's taste —
and receives one verdict comment plus a `gitops/conformance` commit status.
In `enforce` mode, a fully-clean verdict also **approves the merge request
and arms merge-when-pipeline-succeeds**; anything else **escalates to the
pinned reviewers** with the gate red. Fail-closed everywhere: unknown,
doubt, unreadable, too-big, renamed keys, added files, secrets, datastores
the platform offers managed — all escalate.

- Doctrine: [ADR-124](../../docs/adr/124-gating-bot-merge-gestures.md) and
  [merge-gate.md → "Gating bots"](../../docs/merge-gate.md).
- The default classes: [skills/review-policy.md](skills/review-policy.md).
- A repo tightens or widens them (value-level only) via
  `review-policy.md (the policy_path var pins the real path)` on its target branch — an MR editing its own
  policy escalates.

## Posture

Ships `mode: dry_run` (comment + gate, zero gestures). After calibration,
pin `mode: enforce` per repo through the integration's `launch_vars`; the
publish grant's capabilities (minted from the manifest scopes
`approvals`/`merge`/`reviewers`) must also be present, and the operator pin
`forge_publish_mutations=false` disarms every repo at once without a
release.

## Binding

Works with the standard repo-bots provisioning; pairs with `review-pr` on
the same repo (separate gate contexts — `revi/review` judges code quality,
`gitops/conformance` judges the policy). `reviewers` (space-separated
logins) is pinned per repo: it names who the escalation requests.

## The fold (deterministic)

`publish_verdict`'s python owns the decision table; `classify` only
classifies. Escalate on: unreadable/unparseable/over-cap diff, workspace
anomaly, stale launch head, target-modified policy, fallback-served judge,
any file class `escalate`/`unknown`, any blocking platform finding, any
non-empty doubt, an unreadable verdict, or an explicit `escalate`. Driven
by `bots/gitops_warden_gate_test.go`.
