# ADR-050 — DSL Turing-completeness: relocate termination to fuel + liveness

Status: **accepted (Layer 0 + Layer B shipped)** (2026-06-30) — the bounded
expression-layer combinators (Layer 0) and the unbounded-loop tier with its
fuel + liveness safeguard (Layer B) are implemented and tested. The
recursion tier (Layer A) is **deferred** (it depends on the `subbot` construct,
which lives on the unmerged groups/foreach/subbot epic, not on this tree). The
mutable-state tier (Layer C) is a documented **non-goal** for now. The
cloud-side forced budget ceiling is wired (`ir.Budget.ClampToCeiling` +
`applyCloudBudgetCeiling`).

## Context

The iterion `.bot` DSL was deliberately **total** (guaranteed-terminating),
non-Turing-complete, at two levels:

- **Expression layer** (`compute` / `when`, `pkg/dsl/expr`) — a total
  primitive-recursive evaluator: no loops, no recursion, immutable values, a
  fixed builtin set, parse-depth capped at 256.
- **Graph layer** — every cycle had to be an explicitly bounded loop
  (`as name(N)`); an undeclared cycle is a compile error (C019). Budgets
  (`max_iterations/tokens/cost/duration`) were a soft secondary rail.

The question raised: **should we make the DSL Turing-complete?** The honest
finding is that the "problems" of doing so are softer than the doctrine
implies:

- The static-termination guarantee was **already partial**: loop caps may be
  data-driven templates (`as loop("{{outputs.X.cap}}")`) resolved only at
  runtime, and `as loop(1000000)` is "total" yet practically endless.
- The **real** safety (cost / duration / capacity) was always enforced at
  *runtime* by `SharedBudget` — which is already a fuel meter
  (`iterationsUsed++` per node).
- Convergence-to-asymptote comes from `streak_check`/`when`-exits, **orthogonal**
  to the loop cap. TC does not degrade it.
- Turing-completeness already exists **at the leaves**: a `tool`/`agent` node
  runs arbitrary shell. The DSL skeleton's job was only ever to *bound* those
  TC leaves.

So the only thing genuinely lost by going TC is a *static proof of halting*
(Rice's theorem) — a proof iterion only ever provided partially.

## Decision

**Make the DSL Turing-complete in opt-in layers, and RELOCATE the termination
guarantee from compile-time (decidable but restrictive, and already partial) to
runtime (resource-bounded fuel + a liveness monitor).** Keep the strong default:
a `.bot` that opts into nothing is still statically terminating (C019 + the
mandatory loop caps).

### Layer 0 — bounded expression combinators (non-TC, ships first)

Indexing (`arr[i]`, `m["k"]`, `x[0].field`), bounded higher-order combinators
(`map`/`filter`/`reduce` with `=>` lambdas), and total helpers
(`sort/keys/values/slice/sum/min/max/flatten`). Totality is preserved: the
lambda is **not** first-class (only at a combinator call site, applied once per
element of a finite slice, no recursion ⇒ no constructible fixpoint — the
Dhall/Starlark posture). A `maxEvalVisits` work budget bounds nested
combinators. Purely additive / backward-compatible. Diagnostic C120
(index-on-scalar; lambda arity / namespace-collision are enforced at parse).

### Layer B — unbounded loops bounded by fuel + liveness (opt-in TC iteration)

`as name(unbounded)` / `as name(unbounded <fuel>)`: no user iteration cap; the
loop runs until a `when`-exit (convergence). Termination is relocated to:

- **Fuel** — effective cap = per-loop fuel, else `budget.max_iterations`, else a
  hard default (`defaultUnboundedFuel`, never 0 ⇒ no silent infinity).
  Out-of-fuel falls through the loop-edge skip to the exit path (clean in-graph
  termination, not an abort).
- **Liveness monitor** — a per-loop progress signature (hash of the source
  output); unchanged for `maxLoopStall` consecutive crossings ⇒ fixpoint ⇒ the
  back-edge falls through. Catches *practical* non-termination better than any
  static analysis. Reset on signal change / loop re-entry; not persisted across
  resume (fresh stall window).

The cycle stays *declared*, so C019 still guards against accidental infinity.
New diagnostics: C097 (unbounded without a fuel ceiling — error, the
no-silent-infinity invariant) and C098 (unbounded loop whose body has no exit
edge — warning). The opt-in is the `unbounded` keyword itself (explicit,
greppable) — no new workflow keyword.

Layer B already makes the DSL **practically Turing-complete**: unbounded
iteration + the `loop.previous_output` accumulator + `compute` arithmetic +
conditional `when`-exits is a while-loop-with-state — sufficient for TC, bounded
by fuel.

### Layer A — unbounded recursion (deferred)

The clean TC-without-mutable-state route is unbounded `subbot` recursion
(replace the depth guard with a recursion fuel; a data-driven base case routes
to `Done`). It keeps immutability/resume/reproducibility 100% intact (a pure
functional language is TC with recursion + immutable values alone). **Deferred:
`subbot` is not in this tree** (it lives on the unmerged groups/foreach/subbot
epic). When that lands, add `allow_recursion` + `recursion_fuel`, self-recursion
detection, and C099 (recursion-without-base-case warning).

### Layer C — mutable shared state (non-goal for now)

A `state:`/blackboard tier breaks write-once immutability (a value-prop distinct
from termination) and complicates resume (per-iteration mutable snapshots).
`loop.previous_output` already covers the dominant carry-state case. Revisit
only with a concrete blackboard use case.

### Multitenant safeguard — cloud forces the ceiling

`ir.Budget.ClampToCeiling` lowers (or, for an unbudgeted bot, imposes) every
budget dimension to a platform maximum the tenant cannot raise. The runner
applies it (`applyCloudBudgetCeiling`) from `ITERION_CLOUD_MAX_ITERATIONS`/
`_MAX_TOKENS`/`_MAX_COST_USD`/`_MAX_DURATION`/`_MAX_PARALLEL_BRANCHES`. With the
launch gate + per-org quotas, multitenant safety is unconditional regardless of
what a bot declares.

## Consequences

- **What's lost** (honestly): a *compile-time proof* that a run halts by its own
  logic — unrecoverable (Rice), and only ever partial here. C098 is a heuristic,
  not a proof.
- **What's preserved**: every operational guarantee iterion actually sells —
  bounded cost/duration/capacity, resumability, convergence, tenant isolation —
  because they were always runtime-budget-enforced (fuel), plus the new liveness
  monitor catches practical non-termination.
- **Default trust anchor intact**: a `.bot` with no `unbounded` (and, later, no
  `allow_recursion`) is still statically terminating.

## Mitigation matrix

| Lost guarantee | Remedy | Recovered? |
|---|---|---|
| Static termination proof | C019 (no accidental infinity) + C097 (mandatory fuel) + C098 (no-exit warning) + fuel ⇒ resource-termination provable | Partial (resource, not logic) |
| Convergence / asymptote | `streak_check`/`when`-exit unchanged; fuel is the backstop; `iterion bench asymptote` measures it | Full (improved — no literal truncation) |
| Resumability | resume rebuilds `loopCounters` from events; budget exceed is already `failed_resumable`; liveness state resets fresh | Full |
| Multitenant safety | `ClampToCeiling` + launch gate + per-org quota | Full |
| Cost / runaway | `max_cost_usd` / `max_tokens` trip independently of iteration count | Full (pre-existing) |

## References

- Implementation: `pkg/dsl/expr` (Layer 0), `pkg/dsl/{ast,parser,ir,unparse}` +
  `pkg/runtime/{engine,helpers}.go` (Layer B), `pkg/dsl/ir/ir.go`
  `ClampToCeiling` + `pkg/runner/loop.go` `applyCloudBudgetCeiling` (cloud).
- Diagnostics: C097, C098 (unbounded loops); C120 (index-on-scalar).
