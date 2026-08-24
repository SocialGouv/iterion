# ADR-052 — Mono/dual review topology for bi-model review-loop bots

Status: **accepted → implemented** (2026-07-01). Pilot
(branch-improve-loop) + rollout to whole-improve-loop, feature-dev,
docs-refresh, secured-renovacy; resolution wired on the CLI, studio/API,
and dispatcher launch surfaces. Live `iterion bench asymptote` +
dogfood validation of the pilot is the remaining gate before treating the
mono asymptote as production-proven (needs provider-credential headroom).

## Context

Five catalog bots run a **bi-model review loop**: a router alternates
`reviewer_claude` (backend `claude_code`, a "claude"-family model) and
`reviewer_gpt` (backend `claw`, a "gpt"-family model), each feeding its
same-family fixer, until a `streak_check` compute signals convergence.
Two model *families* are the point — the loop's robustness comes from a
second family catching the first's blind spots, and (in 4 of the 5 bots)
the stop condition literally requires a **cross-family** double-approval.

That DUAL topology is robust but **costly and rigid**: it needs two
provider families to be available, and even when only one is (or when the
operator wants to be frugal) it still pays for both. We want the bots to
run either DUAL (unchanged) or MONO (one family, ~half the LLM calls),
resolved at launch, keeping DUAL the default.

Two facts discovered in the code shaped the design (they corrected the
initial sketch):

1. **`round_robin` ignores `when` edge guards.** `execRoundRobin`
   ([pkg/runtime/routing.go](../../pkg/runtime/routing.go)) collects only
   `!edge.IsConditional()` edges and rotates by `counter % len(edges)`.
   So there is no "keep round_robin, add a guard" shortcut — a
   **`condition` router is mandatory** to gate the second family in mono.

2. **The cross-family requirement is encoded in `stop`, not emergent.**
   branch-improve-loop / feature-dev / docs-refresh / secured-renovacy
   put `input.family != loop.review_loop.previous_output.family` directly
   in the `stop` expression. In MONO the family never changes, so that
   clause is never satisfiable → the loop would run to `max_iterations`
   and fail. Only whole-improve-loop is already mono-safe (its `stop` is
   count-based: `clean_streak >= num_chunks + 1`, family-agnostic).

## Decision

**A. Resolve the topology out of band, inject as vars.** A bot cannot
probe host credentials, so a small package `pkg/reviewtopology` resolves
the topology at launch and injects two vars the DSL reads:

- `Resolve(detect.Report, override) → (mode, monoFamily)`: `auto` → `mono`
  on the preferred available family; `mono`/`dual` force it. **`auto` is
  frugal by design** (amended 2026-07-28): dual costs a full reviewer pass
  per family on every run, and with the merge gate wired every push
  re-reviews, so cross-family confirmation has to be asked for rather than
  fall out of merely having two providers configured. `auto` only picks
  the family for you. Family map (operator decision):
  `{anthropic, zai} → claude`, `{openai} → gpt`; cloud providers
  (foundry/bedrock/vertex) are out of scope for v1. anthropic+zai is
  therefore **one** family. `auto` falls back to `dual` only when NO
  participating family is detected at all, so an unconfigured host fails
  on the missing credential the normal way instead of on an empty router.
- `InjectIfDeclared(wf, inputs, report, override)`: opt-in — writes
  `review_mode` + `mono_family` **only** when the workflow declares a
  `review_mode` var. Precedence: explicit flag/toggle > a `--var`
  `review_mode` > `auto`.

**B. DSL: round_robin → condition, driven by the loop counter.** Each
bot's review router becomes `mode: condition` with a binary select:

- the **gpt** edge is guarded by
  `if(vars.review_mode == 'mono', vars.mono_family == 'gpt',
  loop.review_loop.iteration % 2 == 1)`,
- the **claude** edge is the **unconditional default** (required by C012;
  also a safe fallback that never dead-ends on `NO_OUTGOING_EDGE`).

In DUAL the parity on `loop.review_loop.iteration` reproduces the old
round_robin exactly: the counter is monotonic (0-based, +1 per review
pass) and the router is **not** a `review_loop` entry (the loop's only
entry is `streak_check`), so the loop-reset rule
([engine_exec.go](../../pkg/runtime/engine_exec.go)) never fires on the
loop-back into the router — it stays in lockstep with the retired
`roundRobinCounters`. In MONO the guard collapses to a single family;
the other reviewer (and, since fixers follow the actual reviewer's
`family`, its fixer) never runs.

**C. Topology-aware `stop`.** The four family-clause bots OR-in the mono
flag:

```
… && (vars.review_mode == 'mono' || input.family != loop.review_loop.previous_output.family)
```

In DUAL this is unchanged (still requires cross-family); in MONO it
converges on two consecutive clean self-approvals — the accepted mono
trade-off (no cross-model verification; the deterministic engagement +
verify gates still hold). whole-improve-loop's count-based `stop` is
unchanged.

**D. All launch surfaces resolve.** The CLI (`iterion run --review-mode`),
the studio/API (`runview.LaunchSpec.ReviewMode` ← `launchRunRequest`
`review_mode`), and the dispatcher (`EngineRunner.Dispatch`, honouring a
per-ticket `review_mode` bot_arg) all call `InjectIfDeclared` before
`Engine.Run`. An unresolved `auto` (e.g. a bot launched by a surface that
somehow skipped resolution) behaves as DUAL — a pure non-regression.

## Consequences

- **Frugality:** mono ≈ halves LLM calls and needs only one provider
  family. **Adaptivity cost:** mono loses the second family's blind-spot
  coverage; the engagement gate (real tool telemetry) becomes the primary
  anti-façade guard, so mono leans harder on it (cf. the recent
  `ForceInitialToolUse` fix).
- **Convergence risk** of round_robin→condition is guarded by
  `iterion bench asymptote` on both modes (the pilot gate) and by a
  deterministic stub e2e (`e2e/testdata/review_topology_mini.bot` +
  `e2e/review_topology_test.go`) proving dual alternates, auto→mono, and mono
  fires exactly one family — all converge.
- **Regression guard:** `bots/review_topology_test.go` fails if any
  review-loop bot reverts to `mode: round_robin` or drops the topology
  vars / gpt-edge guard.
- **Follow-ons:** a studio Launch-modal toggle (the API field exists; the
  React control is TODO), and classifying cloud providers into families.

## References

- [pkg/reviewtopology](../../pkg/reviewtopology/resolve.go)
- Router semantics: [pkg/runtime/routing.go](../../pkg/runtime/routing.go),
  [pkg/runtime/engine_exec.go](../../pkg/runtime/engine_exec.go)
- Reference wiring: [bots/branch-improve-loop/main.bot](../../bots/branch-improve-loop/main.bot)
- Convergence contract: "Review loops must converge to an asymptote" in
  [CLAUDE.md](../../CLAUDE.md), [docs/asymptote-bench.md](../asymptote-bench.md)

## Addendum (2026-07-07) — flagship bots migrated to ADR-058; topology becomes a generic opt-in

The five bots this ADR was built for have all left the cross-family
reviewer shape: whole-improve-loop and branch-improve-loop first
(ADR-058 v2, 2026-07-03), then feature-dev, docs-refresh and
secured-renovacy Phase 2 (the fleet-wide rollout, 2026-07-07). Their
convergence oracle is now the deterministic verify gate + termination
contract of the v2 campaign shape; those five no longer declare the
`review_mode`/`mono_family` vars.

Two catalog bots still do, and are the topology's live consumers:
`review-pr` (bots/review-pr/main.bot:188,193) and `evolve`
(bots/evolve/main.bot:37,41). Both default to `mono` — since the
2026-07-29 resolver change, `auto` itself resolves to mono, so
cross-family confirmation is an explicit `--var review_mode=dual` rather
than something a host opts into by having two providers configured.

The MACHINERY built here stays, deliberately:
- `pkg/reviewtopology` (resolution + `InjectIfDeclared`) and its three
  call surfaces (CLI `--review-mode`, studio/API `review_mode`,
  dispatcher bot_arg) remain a **generic opt-in facility** — a future
  or third-party reviewer-loop bot re-adopts the topology by declaring
  the vars and using a `condition` router.
- The non-vacuous guard moved from `bots/review_topology_test.go`
  (deleted — its enforced list became empty) to
  `e2e/review_topology_test.go` + `e2e/testdata/review_topology_mini.bot`,
  which drive DUAL/MONO/auto behaviour end-to-end against the runtime.

The ADR body above remains historically accurate for what was built and
why; per ADR-058, cross-family review is an *optional amplification*,
no longer the default convergence mechanism.
