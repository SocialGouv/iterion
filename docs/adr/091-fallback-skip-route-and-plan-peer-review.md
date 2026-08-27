# ADR-091: `action: skip` terminal route + `when:` route gate — and the cross-model plan phase they serve

- **Status**: Accepted
- **Date**: 2026-08-25
- **Authors**: Jo (arbitration), Claude
- **Extends**: [ADR-087](087-cross-backend-model-fallback-chain.md)
- **Code**:
  [pkg/dsl/ir/validate_fallbacks.go](../../pkg/dsl/ir/validate_fallbacks.go)
  (`checkFallbackAction`, `checkFallbackWhen`),
  [pkg/backend/model/executor_retry.go](../../pkg/backend/model/executor_retry.go)
  (`dispatchChain` skip outcome),
  [pkg/backend/model/executor_resolve.go](../../pkg/backend/model/executor_resolve.go)
  (`fallbackWhenActive`),
  [pkg/backend/model/executor_build_task.go](../../pkg/backend/model/executor_build_task.go)
  (`fillZeroValues`),
  [pkg/reviewtopology/resolve.go](../../pkg/reviewtopology/resolve.go)
  (`ResolvePlanReview`, `InjectLLMFamiliesIfDeclared`, `InjectAll`,
  `FamiliesFromCredentialNames`),
  [pkg/server/cloudpublisher/publisher.go](../../pkg/server/cloudpublisher/publisher.go)
  (queued-run injection)

## Context

The campaign bots (feature-dev, app-dev, branch-improve-loop,
whole-improve-loop) plan "in stride" inside one adaptive agent (ADR-058
v2). No external eye challenges the plan before implementation burns
budget. The requested improvement: a **pair reviewer from another model
family** critiques the plan by default whenever a second family is
credentialed, the **plan's author** (same session) challenges the
critique and integrates what holds, and a mid-run peer unavailability
(forfait window shut, provider down, credential revoked) resolves per an
operator policy — *pause and retry when the forfait returns*, or
*continue and ignore*.

"Pause and retry" already exists end to end: an unhandled node failure is
`failed_resumable`, and the run-level usage-window retry
(pkg/retrypolicy + the runner sweeper / `--auto-resume`) parks the run
until the provider window reopens. "Continue and ignore" had **no
primitive**: expressing "this node is optional" took a fan_out router, a
no-op sibling branch and a `best_effort` merge — four plumbing nodes per
bot, the exact Nth-variant smell CLAUDE.md's philosophy names as "the
seam is missing". And the policy could not be picked per run, because
graph topology is static.

Two supporting facts were also unreachable from a bot: *which model
families the run's credentials back* (a bot cannot probe host or sealed
credentials), and — on cloud — nothing injected the existing
`review_mode` resolution at all: queued runs ran the bots' raw `auto`
defaults regardless of tenant credentials.

## Decision

1. **`action: skip` — a terminal fallback route.** A named route may
   declare `action: skip` instead of a backend/model/provider: when the
   chain's walk reaches it (through the same `on:` classification as any
   route), the node **completes** with a zero-value output — every
   schema field at its zero value — stamped `_skipped: true`,
   `_fallback_used: true`, `_served_by: <route>`. Loud by construction:
   the `model_fallback` event fires on the fall-through, and a
   downstream deterministic compute reads the stamp. Compile guards
   (C173): unknown `action:`, skip + backend/model/provider
   (contradiction), skip not last (unreachable routes).

2. **`when:` — a per-route gate over vars.** Any route may declare
   `when: "<expr>"`, evaluated at dispatch against the run's vars; a
   false gate removes the route from the chain. The compiler checks the
   expression parses and references only `vars.*`. This is what lets ONE
   node express both unavailability policies, chosen by an ordinary
   `--var`: `wait` = the skip route's gate is false → the failure stays
   resumable → the usage-window retry; `skip` = the gate arms the
   terminal route.

3. **Credential-derived vars, generalised.** `pkg/reviewtopology` gains
   two opt-in injections beside `review_mode`: `plan_review`
   (auto → `on` iff ≥ 2 distinct families are credentialed —
   family-agnostic, no role hardcoded) and `llm_families` (the raw
   sorted family list, so a future bot builds its OWN policy in a
   compute without a new engine role var). One `InjectAll` at every
   launch surface. On cloud, the publisher derives the family set from
   what actually **sealed** into the run's bundle (BYOK, oauth-forfait,
   pool grant, platform tier) and injects into the run doc + RunMessage —
   closing the standing `review_mode` queued-run gap. An empty bundle
   (runner env fallback, unknowable at publish) injects nothing.

4. **The plan phase in the four campaign bots.** Opt-in by resolution:
   `plan_topology` (compute) → `plan` (author, claude family, read-only)
   → `plan_review` (peer, `claw` + `openai/gpt-5.6-sol` by default,
   read-only tools, carrying the skip route) → `plan_gate` (compute
   reading `_skipped`) → `plan_revise` (the SAME author session via
   `_session_id`, challenges + integrates) → `campaign`. One revision
   turn, no loop — the campaign remains the arbiter of reality, so the
   asymptote discipline is untouched. `plan_review: off` routes entry
   straight to the campaign: the v2 shape, byte-identical behaviour.

## Alternatives rejected

- **A node-level `on_error: skip` field.** More orthogonal (tool/compute
  too) but a wider engine surface, no failure-class filtering without
  reinventing `on:`, and partial overlap with both `fallbacks:` and the
  ADR-044 recovery ladder. The fallbacks chain already owns failure
  classification, ordering and loud degradation; skip composes with real
  routes there (try a metered key, THEN give up).
- **Graph-level best_effort contortion.** Works today, but 4 plumbing
  nodes per bot × N bots, and the policy cannot be a `--var`.
- **A plan-revision loop.** Rejected for the same reason v1's
  reviewer/fixer relays were retired (ADR-058): oscillation surface. The
  author gets ONE challenge turn; residual disagreement rides to the
  campaign as context.

## Amendments from the adversarial round (2026-08-25)

The pre-merge adversarial review (3 agents, findings verified by
execution) hardened the design; all folded in before first release:

- **A filtered skip refuses UNCLASSIFIED failures** (bare CLI exits,
  flattened sandbox errors) on both the execute-failure and the
  build-error walks — converting an indescribable failure into a
  zero-value success is a lie; `on: [any]` opts in explicitly. One
  arbitrated nuance: a build error routes on the LAST execute failure's
  category, so after a matching outage (say usage_window) an
  unbuildable rescue route no longer disarms the skip — the trade-off
  is that a mis-configured rescue is then absorbed into the skip
  instead of surfacing; the `model_fallback` events still record both
  hops.
- **A skip outcome names the LAST executed route as its backend**
  (`chainOutcome.BackendName`): the runner's cost accumulator keys its
  claw double-count exclusion on that name, so an empty value (→ the
  node's requested backend) would erase a metered route's real spend
  from the org cap and the credpool donor ledger. Known limit: a chain
  that burned on two backends keeps one label — the residual error is a
  BOUNDED OVER-count (a claw share already priced per-step may be
  counted again under the last route's label), never a disappearance;
  a cap that closes early is conservative, one that cannot see spend is
  not.
- **Dual injection no longer writes `mono_family: ""`** — it violated
  the `[enum]` review-pr/evolve declare and killed the run at the launch
  enum gate (proven live; the local `--var review_mode=dual` path had
  been broken since the enums landed, and the cloud injection would have
  imported it).
- **`ITERION_PLAN_REVIEW`** is the deployment-wide brake between `--var`
  and auto: platform-tier credentials can flip `auto` on for every
  tenant, including webhook/cron lanes with no per-run surface. Set it
  on the **server** (studio/API) env — the runner consumes
  already-resolved vars and never reads it; an unrecognised value reads
  `off` (a brake fails safe).
- **`when:` must reference declared vars** (C173): an absent var reads
  as false at dispatch and would silently disarm the route.
- **Skip observability**: `model_fallback` carries `to_action: "skip"`
  (empty `to_backend`), and the run header's fallbacks chip lists
  skipped nodes (`FallbackUsage.skipped`) despite the absent `_backend`.
- **Every campaign_input field is explicitly mapped on every edge into
  the campaign, and the loop back-edges blank the plan fields** — an
  unmapped field is not `""` but the raw `{{input.x}}` placeholder
  leaking into the prompt, and forward-edge mappings re-apply on every
  loop re-entry (a stale pass-1 plan would re-anchor later passes).
- **billy/willy budgets** gained the phase's headroom (+30m/+$15).
- Known-and-accepted (board findings filed): `readonly:` is enforced on
  codex/pi only — on claude_code it is intent, not a sandbox (a
  pre-existing repo-wide posture, review-pr included); and a
  `session: inherit_if_available` node resumed cross-pod can hold a
  dead `_session_id` — the engine seam wanted is "resume failure under
  inherit_if_available ⇒ fresh". *(The second is CLOSED — see the third
  amendment.)*

## Second amendment (2026-08-26, from Revi's pre-merge gate review)

- **The shipped bots' skip route is `on: [any]`** (R1b58ea): under the
  default `sandbox: auto` a claw failure flattens to a STRING at the
  `__claw-runner` IPC boundary — no typed `ErrRateLimited` survives — so
  it classifies UNCLASSIFIED, which a filtered skip refuses by design;
  the operator's `skip` policy would silently become `wait`. The
  operator's `skip` genuinely means "the peer must never block", so the
  unfiltered opt-in is the honest semantics. The filtered-skip guard
  stays for authors of unsandboxed nodes. The underlying class — typed
  error classification lost across the claw sandbox IPC, which ALSO
  blinds the run-level usage-window retry for sandboxed claw nodes — is
  a pre-existing engine gap filed on the board (ADR-087 stage-3
  territory: a typed error envelope on the wire).
- `plan_review_policy` carries `[enum: "wait","skip"]` (a typo'd --var
  now fails at launch instead of silently selecting wait), and
  `fillZeroValues` emits `float64` for int fields (the JSON-shaped
  contract ValidateOutput and a store round-trip expect).

## Third amendment (2026-08-27, from the first `/billy` dogfood on this repo)

Two of the decisions above met a real run and moved
([docs/bot-runs/branch-improve-loop.md](../bot-runs/branch-improve-loop.md),
[docs/revi-billy-loop.md](../revi-billy-loop.md)):

- **The `inherit_if_available ⇒ fresh` seam shipped**, closing the
  known-and-accepted item above — and with a WIDER scope than asked.
  `delegate.Task.SessionOptional` is set for `inherit_if_available` AND
  for `persist`: both say the session is best-effort, and a cloud resume
  that replaces the sandbox container kills the CLI's session files for
  either. On an UNCLASSIFIED failure the executor drops the session
  (evicting the claw node-session store too, so "fresh" is fresh on
  every backend) and retries once. Unclassified ONLY: auth /
  usage_window / unavailable are credential- or model-level, and
  transient_exhausted is a provider-side cause the session had no part
  in — degrading there would buy a second full retry budget under an
  outage and discard continuity for nothing.
  Widening to `persist` is deliberate: a wedged node returns
  `error_during_execution` in ~2.6s on EVERY resume, so "fail loudly and
  let the run-level retry handle it" is not a live option — the retry
  re-hits the same dead file. Losing one loop iteration's memory is the
  cheaper failure.
- **The degrade is loud**, on the same terms this ADR set for the skip
  route: a `session_degraded` store event (the restore-side twin of
  `persist_session_degraded`) and a `_session_degraded: true` stamp on
  the node's own output, so a deterministic gate can fail closed on an
  amnesiac input. It is deliberately NOT a `model_fallback` — the same
  backend, model and credential served; what degraded is the node's
  INPUT.
- **`plan_review_policy` now defaults to `skip` for
  `branch-improve-loop` only.** The discriminator is not the bot's
  identity but the property that it is invoked ON an external artifact:
  a fixer that parks on an OPTIONAL cross-model reviewer holds someone's
  pull request hostage, while a parked campaign on a workspace blocks
  nobody. The other three campaign bots keep `wait`. Residual risk,
  filed rather than fixed: a permanently dead peer credential now
  degrades every run silently, and the only operator signal is the run
  console — the fixer's PR comment does not yet say "plan peer skipped".

## Consequences

- A judge served by the skip route emits a schema-valid, zero-value
  verdict. The guardrail is the same as ADR-087's: gates that consume
  such outputs MUST read `_skipped` / `_fallback_used` (the shipped
  `plan_gate` does).
- `when:` failures at dispatch (an eval error the compiler could not
  foresee) deactivate the route with a warning rather than failing the
  node — a broken fallback gate must not take down the primary it backs
  up. The compile-time vars-only check makes this path exceptional.
- The four bots' `plan_review` default is `auto`: hosts with one family
  see no behaviour change; hosts (or cloud tenants) with two get the
  peer-reviewed plan automatically. Activation on the prod instance is
  exactly one credential provisioning
  ([docs/cloud-llm-credentials.md](../cloud-llm-credentials.md)).
- Extending the phase to the other campaign bots is a bundle change
  (copy the fragment; add the bot to `bots/plan_phase_test.go`), no
  engine PR — tracked as follow-on work once the pattern is
  dogfood-proven.
