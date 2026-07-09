# ADR-058 — Minimal framing: lean on the agent, don't cage it in a graph

Status: **accepted** (2026-07-03). Amends the improve-loop lineage
(ADR-011 chunking → ADR-055 per-unit convergence → ADR-057 axis-sweep) with the
lesson those three iterations kept re-learning the hard way: for a code-writing
campaign bot, **the deficit is framing, not capability**. Piloted by rewriting
`whole_improve_loop` v1 (axis-sweep, ~16 nodes) → v2 (campaign, 8 nodes).

## Context

Three successive redesigns of `whole_improve_loop` each decomposed the work into
more deterministic nodes — chunk→review→streak, then per-unit
transform/verify/review/commit, then enumerate→transform→review→commit_item→
re_enumerate. Every version underperformed a human running one Claude Code
session on the same axis. The v1 axis-sweep's `enumerate` (a single blocking node
told to scan the whole repo and emit an exhaustive work-list) ran **75 minutes
without finishing** on `pkg,cmd`, got forfait-capped, and banked **zero commits**
— the same "nothing lands" outcome as the chunked version, now caused by the
scaffolding itself.

The operator's repeated correction: *"ce qui rend nos bots inefficaces c'est
qu'on essaye de trop les cadrer avec des règles et flow contre-productif"* and
*"inspire-toi de mes sessions qui fonctionnent plutôt que d'inventer des
patterns"*. The empirical backing
([docs/references/productive-session-patterns.md](../references/productive-session-patterns.md),
mined from ~2 900 local sessions): a productive session is a **one-sentence
brief → the agent maintains a LIVING todo AND commits in stride**
(`TODO COMMIT TODO COMMIT TODO TODO COMMIT…`; 178 todos / 97 commits in one
session), **~3.3 commits/active-hour sustained to 48h**, and — decisively —
*"the pattern dominates the model"* and *"once framed, a campaign runs itself"*.
The rigid graph was not adding safety; it was fragmenting the exact flow that
makes the manual pattern work.

## Decision

Design code-writing campaign bots as **one capable agent + a mission + standing
autonomy**, and have the engine provide **only what it uniquely adds and what
does not cage the flow**. Concretely, `whole_improve_loop` v2:

- **`campaign`** — ONE `claude_code` agent, whole-repo, full tools, whose
  `system:` prompt is a *contract that reproduces the productive-session shape
  agent-side*, not a decomposition: a LIVING todo (born from a brief
  exploration, re-prioritized — never frozen upfront phases); the repeated unit
  *locate → smallest change → build → test → semantic commit*, one site at a
  time; **commit each site as you finish it** (cadence in the contract, not a
  node); the pre-existing-failure **baseline + skip permission**; **ask the
  human only on a real decision**; a required **termination output**
  (`axis_complete` + `commits_this_pass` + `sites_remaining`).
- **`verify_build`/`verify_run`** — the deterministic, stack-agnostic build/test
  gate: the tight real-feedback loop (red → straight back to `campaign` with the
  log). This is engine value the manual loop can't cheaply enforce.
- **A bounded continuation loop** (`gate → campaign as continuation_loop(max_passes)`)
  until `axis_complete`, plus the opt-in MR path.
- **git IS the durable state** — no worklist/cursor scratch file; an interrupted
  run keeps every committed site, a fresh pass reads `git log` and continues.

Retired from v1: `enumerate`, `next_item`, `transform`, the `alt`+`reviewer_*`+
`review_gate` per-item review, `commit_item`, `advance`, `re_enumerate`, the
worklist/cursor state, and the `review_mode`/`mono_family`/`max_items` vars.
16 nodes → 8. The per-item cross-family review left the core loop (the campaign
self-reviews via build/test + judgment, the manual pattern); it may return later
as an *optional amplification*, never as the mechanism.

## The rule (for any code-writing bot)

Put the **work-unit and commit cadence in the agent's CONTRACT**, not in separate
deterministic nodes. Add engine scaffolding only for the three things an agent
alone can't do well: a **real-feedback verify gate**, **termination/continuation**
(a machine-checkable done-signal + a bounded loop), and **persistence/resume**
(git as state; closure artifact). Give the **baseline + skip permission**. Then
get out of the way — *once framed, a campaign runs itself*. Anti-Goodhart guards
(workflow_authoring_pitfalls.md) and universality still apply, but as light
contracts, not a cage.

## Alternatives considered

- **Keep the v1 sweep, just bound `enumerate`** (per-package waves). Rejected:
  it doubles down on the enumerate/transform decomposition the operator
  identified as the problem, and still fragments the living-todo + in-stride-
  commit flow the manual pattern depends on.
- **Pure agent, no verify gate.** Rejected: the mined data (rule 8, the CI-grind
  variant) shows the *tight real-feedback loop* is the single highest-leverage
  amplification; a deterministic gate close to the change is exactly the engine's
  value-add over an unassisted agent.
- **Keep per-item cross-family review as the core.** Rejected as over-framing for
  the *default* loop; the campaign self-reviews via build/test + judgment.
  Adversarial cross-family review returns as an opt-in amplification when the
  axis warrants it.

## Consequences

- The improve loop is now ~4 functional nodes; a code-writing campaign is a
  contract to read, not a graph to trace. Future campaign bots start from this
  shape.
- ADR-057's "encode the work-list sweep" guidance is **superseded for the improve
  loops**: the work-list is the agent's living todo (in-memory + git), not a
  persisted structure the graph iterates.
- Family rollout (ADR-057 §rollout still holds in spirit): `branch_improve_loop`
  becomes the same campaign scoped to the branch diff; the shared value is the
  verify gate + continuation + resume, factored once.
- Validation: static (validate 8/9 no undeclared cycle, e2e rewritten, universality
  green) is done; a live dogfood on a real axis is the remaining proof, forfait-
  gated. The v1 sweep already proved the *mechanics* of verified incremental
  commits (run 019f27d4, 4 WriteFileAtomic commits); v2 removes the framing that
  made whole-repo runs stall.

## Rollout addendum (2026-07-07) — fleet-wide application

The pattern shipped across the catalog, one commit per bot, each
`task check`-green; the git log of 2026-07-07 is the rollout record.

**Full v2 conversions** (campaign + deterministic verify gate + bounded
continuation loop + git-as-state; `review_mode`/`mono_family` retired
where present): feature-gap-fill (`gap_closed`), test-coverage
(`coverage_complete`, keeping the negative-space new-test-code floor in
the deterministic gate, now measured against the run base), feature-dev
(`feature_complete`, MR tail kept; the DOGFOOD PILOT — run 019f3bb4
shipped `iterion validate --strict` in one pass, 11m33s, 2 in-stride
commits, gate converged), docs-refresh (`docs_aligned`; the
deterministic scan/manifest machinery kept in full, enforce_fix_scope
reborn as a detect-only scope gate, the inter-run cache now fed by the
manifest's mechanical verified_pairs), adr-cartograph (`adrs_aligned`;
scan/survey/manifest kept, handoffs moved into the campaign contract),
rgaa-audit (light: detect_ui absorbed as the campaign's phase 0;
single-pass, deterministic scan_health/cap_findings unchanged).

**Calibrated (structure kept where it is a PROPERTY, the fakeable part
hardened):** dep-update-guard — the read-only security_audit stays
separate from the mutating align (anti-prompt-injection separation
behind a deterministic verdict gate) and commit-after-green stays
(shared PR branch); the LLM `validate` self-report was replaced by the
deterministic verify_build/verify_run gate, fail-closed.
secured-renovacy — Phase 1's per-package pipeline untouched (ADR-055
unit-convergence with reified security/revert/SBOM gates); only the
Phase-2 cross-family relay became a campaign + deterministic gate.

**Audited, deliberately NOT converted:** sec-audit-source's remediation
ladder — `project_review_input` statically projects a 4-field input so
`reviewer_isolation` is un-influencable by tainted upstream text (an
isolation a prompt contract cannot replicate), `reattack` is a
fresh-session adversarial lens, and the three rungs are already
deterministic with a fail-closed aggregate. That is diverse-lens
security verification (the same doctrine as its multi-voter triage
core), not over-framing. Also not converted (shape is the product):
review-pr, bmady, adr-rechallenge, evolve, whats-next,
supply-shield(-cve), sec-audit-deps, revi-converse — these received
only calibrated contract clauses (G5, sub-agent fan-out, security
dismissal asymmetry) where a real gap existed.

Doctrine updates in the same rollout: CLAUDE.md + workflow_authoring_
pitfalls.md asymptote sections rewritten around the v2 mechanism (the
streak/pushback machinery is preserved as the mandatory recipe for any
NEW reviewer loop); ADR-052 addendum marks the topology a generic
opt-in; `bots/review_topology_test.go` deleted with the machinery guard
living in `e2e/review_topology_test.go`.
