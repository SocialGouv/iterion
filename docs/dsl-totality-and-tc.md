# DSL totality, Turing-completeness, and the static-predictability surface

This page is the canonical answer to two questions that sound opposed but are
not: **is the `.bot` DSL Turing-complete?** and **how much does compilation
guarantee statically?** The short answer is *both, by design* — predictability
and expressive power live on different axes, and the layered design (ADR-050)
keeps the strong static guarantees as the **default** while making
Turing-completeness an explicit **opt-in**.

See also: [ADR-050](adr/050-dsl-turing-completeness-fuel-liveness.md) (the
decision), [references/diagnostics.md](references/diagnostics.md) (every
compile-time check), [dsl.md](dsl.md) (the language).

## Two axes, not one dial

Expressive power is decided independently at two layers:

| Layer | Construct | Posture | What bounds it |
|-------|-----------|---------|----------------|
| **Expression** (`compute`, `when`) | `map`/`filter`/`reduce`, indexing, `sort/keys/values/slice/sum/min/max/flatten`, arithmetic | **Always total** (terminating) | Non-first-class lambdas (no constructible fixpoint) + `maxEvalVisits = 100_000` work budget + parse-depth 256 |
| **Graph** (nodes + edges) | bounded loop `as name(N)` | **Total by default** | Mandatory literal/expr iteration cap; **C019** rejects any undeclared cycle |
| **Graph** | unbounded loop `as name(unbounded [<fuel>])` | **Turing-complete (opt-in)** | Runtime **fuel** + a **liveness monitor**, not a static proof |

A `.bot` that opts into nothing is **statically terminating**. The only way to
reach Turing-completeness is to type the keyword `unbounded` — explicit,
greppable, and flagged by diagnostics.

## Why the default is total

- **Every cycle must be a declared bounded loop.** The compiler runs a
  three-colour DFS (`validateUndeclaredCycles`, [pkg/dsl/ir/validate.go](../pkg/dsl/ir/validate.go))
  and emits **C019** for any back-edge without an `as name(...)` declaration. A
  graph with no `unbounded` loop has a statically-bounded number of node
  executions.
- **The expression layer can never loop.** Lambdas exist only at a combinator
  call site, applied once per element of an already-materialised finite slice —
  there is no way to store a lambda, return it, or recurse, so no fixpoint is
  constructible (the Dhall/Starlark posture). The `maxEvalVisits` budget bounds
  even adversarial nested combinators ([pkg/dsl/expr/expr.go](../pkg/dsl/expr/expr.go),
  `TestExpr_VisitBudget`).

## How opt-in Turing-completeness works

`as name(unbounded)` removes the user iteration cap. Termination is **relocated
from compile-time to runtime** by two mechanisms in
[pkg/runtime/engine.go](../pkg/runtime/engine.go) /
[pkg/runtime/helpers.go](../pkg/runtime/helpers.go):

1. **Fuel** (`resolveLoopMax`): effective ceiling = per-loop fuel, else
   `budget.max_iterations`, else `defaultUnboundedFuel = 1000` — **never 0**, so
   there is no silent infinity. Out-of-fuel falls through the back-edge to the
   exit path (clean in-graph termination, not an abort).
2. **Liveness monitor** (`loopStalled`): a per-loop signature (a hash of the
   source node's output). Unchanged for `maxLoopStall = 3` consecutive crossings
   ⇒ the loop is at a fixpoint ⇒ the back-edge falls through. This catches
   *practical* non-termination (a loop making no progress) better than any
   static analysis could.

That is enough for Turing-completeness: an `unbounded` loop + a **carried
accumulator** + a `compute` node (arithmetic over the carried state) + a
`when`-exit is a **while-loop-with-state**, bounded by fuel. Two accumulator
mechanisms exist, and they are not interchangeable:

- a **self-feeding `with:` mapping** on the back-edge carries a node's output
  straight back as its next input — the *immediate* predecessor. This is the
  one to use for an accumulating recurrence (`n := n - 1`).
- `loop.<name>.previous_output` is a one-turn-lagged **snapshot** of the source
  output, built for *convergence detection* (compare this turn against the last,
  as in `streak_check`). It is intentionally empty for a loop's first crossings,
  so it is the wrong tool for an immediate-predecessor recurrence.

The deterministic demonstrator
[examples/turing/countdown.bot](../examples/turing/countdown.bot) shows the
self-feeding form with no LLM in the loop, terminating by its `when`-exit (not by
fuel).

Two diagnostics keep the opt-in honest: **C097** (error — an `unbounded` loop
*must* have a fuel ceiling) and **C098** (warning — an `unbounded` loop whose
body has no exit edge, so only fuel/liveness can ever stop it).

## The static-predictability surface

Compilation is a two-phase pipeline that emits ~110 diagnostics
(C001–C199 + bundlelint C200–C230, both sparse ranges). The full catalogue, with severity and fix,
is in [references/diagnostics.md](references/diagnostics.md); a drift guard
(`TestDiagCodesAreDocumented`) and a uniqueness guard (`TestDiagCodesAreUnique`)
in [pkg/dsl/ir/diag_codes_test.go](../pkg/dsl/ir/diag_codes_test.go) keep that
catalogue accurate and every code unambiguous.

| Phase | Owns | Examples |
|-------|------|----------|
| `ir.Compile` (structural, AST → IR) | declarations, references resolve, shapes | C001 unknown node, C002/C003 unknown schema/prompt, C041 duplicate id, C046 budget shape |
| `ir.validate` (semantic, graph) | reachability, routing, cycles, types | C016 reachability, C010/C012 router exhaustiveness, C019 undeclared cycle, C097 mandatory fuel, C107/C108/C120/C121 expr types |

**Statically guaranteed** (a `.bot` that compiles clean cannot violate these):
every node reachable from `entry` (C016); every edge target and `{{ref}}`
resolves (C001/C029–C036/C053/C093/C195); router branches are exhaustive or have
a fallback (C010/C012); no accidental infinite loop (C019); every `unbounded`
loop has a fuel ceiling (C097); typed comparisons are type-compatible
(C107/C108/C109/C120/C121); budget fields are well-formed (C046).

## What is deliberately *not* static (the honest boundary)

Per Rice's theorem, a compile-time proof that a run halts **by its own logic** is
undecidable — and iterion only ever provided it partially (loop caps can be
runtime-resolved templates). The design relocates that one guarantee to
runtime, and is explicit about the rest:

| Not statically guaranteed | Enforced instead by |
|---------------------------|---------------------|
| Termination *logic* of an `unbounded` loop | Fuel + liveness monitor (runtime), C097/C098 (compile heuristics) |
| Actual LLM / tool output shape | Schemas are **advisory**, not contracts — the runtime passes outputs verbatim |
| Cost / duration / capacity | `SharedBudget` at runtime (`max_cost_usd`/`max_tokens`/`max_duration`); cloud `ClampToCeiling` |
| Liveness of external systems (MCP, webhooks, images) | Runtime connect/spawn attempt |
| `${ENV}` resolution in `model:`/`command:`/… | Runtime expansion (compile-time skips `$`-bearing values) |

Crucially, **every operational guarantee iterion actually sells** — bounded
cost/duration/capacity, resumability, convergence-to-asymptote, tenant
isolation — is runtime-budget-enforced and therefore unaffected by going
Turing-complete. The only thing lost is a static *proof of self-halting*, which
was always partial.

## TL;DR

- **Turing-complete?** Yes — opt-in, via `as name(unbounded)` + `compute` +
  `loop.previous_output` + `when`-exit, bounded by runtime fuel + liveness.
- **Statically predictable?** Yes, strongly — ~110 compile-time diagnostics over
  a two-phase pipeline, with the default (no `unbounded`) still statically
  terminating. The runtime boundary is small, documented, and is exactly the one
  Rice's theorem makes unavoidable.
