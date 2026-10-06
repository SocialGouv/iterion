# ADR-123 — A gating bot: merge-gate gestures as a declared capability, executed by the publish surface

- Status: accepted (2026-10-06, gitops-warden slice 1)
- Date: 2026-10-06
- Deciders: jo (direction), Claude (analysis)
- Relates to:
  - [118-merge-gate-settle-marks-and-verdict-attribution.md](118-merge-gate-settle-marks-and-verdict-attribution.md) —
    extends the verdict-order authority from commit statuses to the gestures that follow them.
  - `pkg/forge/reviews.go` (NewReview) — the doctrine line this ADR amends: reviews stay
    comment-only; gating becomes a separately-declared capability set.
  - `docs/merge-gate.md` — gains the "Gating bots" section.
  - Ticket #2235 (gitops-warden), epic #1395 (the merge gate).

## Context

The merge-gate doctrine to date: **"iterion bots advise, they do not gate the merge."** A
reviewer bot posts comments and a deterministic commit status (`revi/review`); a human
arbitrates — approves, merges, or pushes back. The gate machinery (statuses, verdict
ordering, reconcile, settle, human override) makes that advice *load-bearing* without
giving any bot a merge act.

Gitops repositories break the symmetry. The reviewer there is not advising a merge — it is
executing a POLICY: "this diff changes only image tags, resource values and non-secret
config; the platform offers these services managed; the merge is routine." The first
customer is a product gitops repo (SIAM, on the PIC forge, DevOps team) whose MRs from
product developers should merge without a human when — and only when — they are simple and
platform-conformant, and should escalate to devops reviewers otherwise.

Doing that with today's surface is impossible (no approve, no merge-when-pipeline-succeeds,
no reviewer request) or unsafe: the one existing auto-merge gesture, vetty's
`arm_automerge`, speaks raw GitHub GraphQL from a bash tool inside the run workspace —
GitHub-only by construction, invisible to the grant system, unkillable per repo.

## Decision

A bot MAY cast an approval, arm merge-when-pipeline-succeeds, and request reviewers — when
and only when:

1. **The verdict is a deterministic fold.** The LLM classifies; a compute node folds a
   closed vocabulary into approve/escalate with fail-closed rules (unknown = escalate,
   doubt = escalate, unreadable = escalate). No gesture ever follows an LLM judgment
   directly.
2. **The gesture crosses the server publish surface.** `/api/v1/forge/publish-review`
   gains an optional `verdict` block; the server executes approve → reviewers → arm
   through the team connection's live client. No forge credential in the run workspace,
   no provider API spoken from a bundle. This retires the vetty pattern (kept for GitHub
   until vetty migrates; marked legacy).
3. **Every mutation is pinned to the audited head** (`verdict.audited_sha` is REQUIRED —
   stricter than the gate status, whose legacy unpinned post stays for bundles that
   predate the pin). A moved head is a refusal, never a retarget: a stale verdict
   certifies nothing and arms nothing.
4. **The gate status is the anchor.** Verdict gestures execute only after the gate status
   posted on the same head, ordered by the same gate decision. A merge a bot armed always
   has a check on the forge saying whose verdict armed it; a superseded or refused gate
   carries the gestures down with it. Verdicts without an enabled gate are refused.
5. **Capability is minted, not assumed.** The run's publish grant carries capabilities
   (approve / merge / reviewers) resolved from the bot manifest's `forge.token_scopes`
   (`approvals` / `merge` / `reviewers` at write level). An unresolvable manifest mints
   none. The operator pin `forge_publish_mutations` (any value but `true`) strips every
   gesture capability at mint — the per-repo disarm that needs no release and no bot
   redeploy.
6. **Dry-run is the shipped posture.** The gesture-capable bundle defaults to `mode:
   dry_run` (comment + gate, zero verbs); `enforce` is an operator launch-vars pin,
   flipped after calibration, reversible by the same pin.

The forge layer speaks the gestures behind three optional-capability interfaces
(`forge.MergeApprover`, `forge.AutoMergeArmer`, `forge.PullReviewerSetter`, plus
`forge.MergeabilityReader` for the merge verdict read), implemented where the forge has a
native gesture: GitLab (POST approve with sha pin, PUT merge with
`merge_when_pipeline_succeeds`, reviewer_ids union) and GitHub (APPROVE review event,
GraphQL auto-merge with `expectedHeadOid`, requested_reviewers). MWPS is NOT a field on
`MergeOptions`/`MergePull`: arming and merging are different acts, and a bot that wants
the merge arms and lets the forge gate it. There is no merge-now gesture in the set.

Arming semantics both providers share: the FORGE performs the merge when its own
conditions hold (pipeline green per the repo's settings, no conflicts) — the bot never
bypasses a required check by arming; the required check remains the repo's own.

## Consequences

- `docs/merge-gate.md` gains a "Gating bots" section: the verdict block, the scopes, the
  kill switch, the calibration posture.
- The human override (`/revi approve` family) is unchanged and outranks a bot verdict
  through the existing claim authority: a bot verdict can never paint over a newer human
  decision, and vice versa.
- A gating bot's audit trail: every executed gesture is audited with the mode that
  claimed to drive it; provisioning logs which bots carry gesture scopes.
- Known residuals, accepted and documented: two concurrent reviewer-union writes can drop
  one addition (GitLab has no atomic reviewer-add; self-healing on the next publish); a
  peer gate going red after the arming is a race the arm-holder's re-run repairs (stock
  GitLab does not block a merge on external commit statuses — the bot reads peer
  statuses itself through the pull-request read twin).
- Rejected: bot-side forge credentials for gestures (the vetty pattern — ungrantable,
  unkillable, provider-bound); merge-now (bypasses the forge's own gates); mutations
  without a gate anchor (unauditable); unpinned mutations (the unaudited-commit-reaches-
  main shape); a persona named in the engine (capabilities are data, the bot declares).
