# External Methodologies Cross-Check — IACDM & AI-DLC 2026

Two independent 2026 methodology papers mapped onto iterion's mechanisms:
what they validate, the handful of rules worth importing (folded into
[workflow_authoring_pitfalls.md](../workflow_authoring_pitfalls.md#cross-checked-rules-from-convergent-methodologies-iacdm--ai-dlc-2026)),
and what iterion deliberately does differently. Read this when weighing the
next methodology paper against iterion, or when talking to operators who
arrive speaking these frameworks' vocabulary.

**Sources.**

- **IACDM** — *Interactive Adversarial Convergence Development Methodology*
  (J. Moreira, [arXiv:2604.16399](https://arxiv.org/abs/2604.16399)). Core
  claim: the **verification gap** is structural — an LLM is a stochastic
  generator with no internal `V(artifact) → {correct, incorrect}`, so *"the
  tool is irrelevant; the process is determinative."* Mechanisms: 8 phases
  with discrete gates held by Verification Agents (VA-automatic =
  tests/linters/compilers with a binary verdict; VA-human = the operator,
  for semantic adequacy), adversarial critique through specialized lenses
  (each owning an exclusive failure class), a persistent `specs/` knowledge
  repo, context granularization. Evidence: 20+ projects at a single
  institution (N=1, self-acknowledged, with testable hypotheses).
- **AI-DLC 2026** — *AI-Driven Development Lifecycle*
  (AWS + [The Bushido Collective](https://github.com/thebushidocollective/ai-dlc)).
  Work decomposed into Intents → Units → Bolts, driven by programmatically
  verifiable **completion criteria** and **backpressure** (quality gates
  that reject non-conforming work without prescribing the approach); three
  human-involvement modes (HITL / OHOTL / AHOTL) matched to work
  verifiability; knowledge artifacts; ops-as-specs.

## What they independently validate (no action needed)

Both papers converge — from different starting points — on choices iterion
pre-arbitrated and shipped:

| Their concept | iterion's shipped form |
|---|---|
| Backpressure over prescription; programmatic completion criteria | Termination contract (`axis_complete ∧ gates green`) + `verify_run` on the REAL exit code (ADR-044/058) |
| Verification is always external to the generator (GA/VA model) | "Gates stay deterministic — never an LLM judgment" (C103–C106) |
| "Ralph Wiggum pattern" / the 19-agent trap: a simple loop with good feedback beats complex multi-agent architectures | ADR-058 v2: ONE `campaign` agent + deterministic gate + bounded continuation loop, which replaced the reviewer/fixer relay fleet-wide |
| Validation theater / RLHF sycophancy; request refutation, never validation | Goodhart + façade ([workflow_authoring_pitfalls.md](../workflow_authoring_pitfalls.md)); adversarial review posture |
| "Scope presence blindness": modules that were never implemented do not fail tests | The goai→claw façade case study + the right-artifact discipline (`git add -N` / `git add -A`) |
| Convergence gate + the "convergence perfectionism" antipattern | The asymptote rule + bounded `max_passes` + `iterion bench asymptote` |
| A gate verifies *completeness of critique*, not absence of findings | sec-audit's `scan_health`: hard-fail when the scanner floor produced no output — the zero-finding façade, caught deterministically |
| Ops-as-specs (scheduled / reactive triggers) | `iterion schedule`, the `pkg/trigger` spine, manifest `invocations:` |
| Persistent knowledge layer (`specs/`, knowledge artifacts) | The three knowledge channels: workspace memory / board issues / committed bilans |
| Context granularization ("one session per phase, one phase per module") | Fresh context per pass, per-node sessions, sub-agent offloading |
| Mode matched to work verifiability, not just risk | `permission:` precedence, `interaction:` modes, supervisors — per node, not per run |

The shared thesis — *quality comes from the process, not the model* — is
iterion's product thesis ([philosophy.md](../philosophy.md)): a
deterministic orchestration engine over interchangeable backends.

## The imported delta

Folded into the pitfalls doc as
[cross-checked rules](../workflow_authoring_pitfalls.md#cross-checked-rules-from-convergent-methodologies-iacdm--ai-dlc-2026):

1. **Teach-back, not confirmation** — invert the agreement bias: the agent
   restates the mission; the human judges the restatement.
2. **Verify divergence, not correctness** — refutation framing for every
   self-check.
3. **Scope inventory** — absence doesn't fail tests; check presence
   deterministically.
4. **Cost-tier switch point** — cheap models on well-specified units once
   context is externalized.
5. **A lens must own a failure class** + concentration analysis at
   synthesis.
6. **The ~40–60% context budget** — cap unbounded inputs before the LLM.
7. **Criteria escape rate** — a defect that passed the gates is a gate bug.

First application: the feature-dev and whole-improve-loop campaigns
teach-back an ambiguous mission via `ask_user_async` (ADR-081) and keep
working under stated assumptions — a non-blocking variant that IACDM's
operator-gated Phase 0 cannot express.

**Parked candidates** (worth a look when the need shows up): *visual
backpressure* (vision-model comparison of implementation screenshots
against a design reference, for UI bots — the Playwright/browser-pane
plumbing exists); *pre-launch adversarial spec review* (a cheap judge
attacks the mission statement — contradictions, hidden complexity,
unvalidated assumptions — before an expensive campaign burns budget on the
wrong problem).

## Vocabulary mapping (for operators arriving from these frameworks)

| Their term | iterion mechanism |
|---|---|
| HITL — human validates each step | `permission: ask`, `interaction: human`, human nodes, plan gates |
| OHOTL — human observes live, may interrupt, no blocking gate | `supervisor` blocks, operator chat/steering, `interaction: async` |
| AHOTL — human sets criteria, reviews output | campaign + deterministic verify gate + termination contract |
| Completion criteria | termination-contract flags + the gate's all-of conjunction |
| Backpressure | verify gates / scanners / diagnostics — they reject, never prescribe |
| VA-automatic / VA-human | tool & compute gate nodes / human gates + review scope |
| Bolt | one pass of a `continuation_loop` |
| Knowledge artifacts / `specs/` | memory scopes, bundle skills, committed bilans |
| Criteria escape rate | gate escapes, noted in a bot's bilan ("Findings / misses") |

## Deliberately not adopted

- **The 8-phase liturgy with VA-human at most gates** (IACDM) —
  operator-centric by construction (its own L5 limitation concedes team
  scalability is open); iterion runs headless, so human gates are the
  exception (review scope, plan gates) and deterministic gates the rule.
- **"<15% structural change between versions" as a stopping criterion** —
  author-calibrated, N=1; a machine-checkable done-flag ∧ green gates is
  stronger, and is what ships.
- **Unconditional Phase-0 discovery on every non-trivial task** — the wrong
  default for bounded/mechanical missions; iterion scopes teach-back to
  *ambiguous* missions, first pass only, non-blocking.
