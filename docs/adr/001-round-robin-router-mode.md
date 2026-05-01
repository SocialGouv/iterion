# ADR-001: Adding the `round_robin` mode to Router

- **Status**: Accepted
- **Date**: 2026-04-01
- **Authors**: devthejo
- **Workflow context**: `examples/dual_model_plan_implement_review.iter`

## Context

The Iterion v1 DSL orchestrates multi-agent workflows with a small set of
primitives: `agent`, `judge`, `router`, `join`, `human`, `tool`. The router
currently supports a single mode — `fan_out_all` — which spawns every
outgoing branch in parallel.

When a workflow needs to **alternate between two agents** at each iteration
of a loop (e.g. Claude refines on turn 1, Codex on turn 2), the v1 DSL
forces a **cross-pair pattern**: structurally duplicating the nodes and
crossing the rejection edges between the two pairs.

This pattern surfaced while designing the `dual_model_plan_implement_review.iter`
workflow, which orchestrates:
- Parallel planning (Claude + Codex) with a Claude-driven merge
- A validation/refinement loop with the refiner alternating between models
- Implementation with the implementer alternating between models
- Parallel review with a fall-back to planning on rejection

### Measured cost of the cross-pair pattern

| Metric | With cross-pair | With `round_robin` (estimated) |
|---|---|---|
| Nodes | 46 | ~23 |
| Edges | 60 | ~30 |
| `.iter` lines | ~550 | ~280 |
| Duplicated prompts | 0 (shared) | 0 |
| Duplicated nodes | 23 (everything except prompts/schemas) | 0 |

The duplication is purely structural — every duplicated node has the same
delegate, the same prompts, the same schemas. Its only purpose is to
provide an anchor point for different edges.

## Decision

Add a `round_robin` mode to the `router` node in the v1 DSL.

### Syntax

```iter
router refine_selector:
  mode: round_robin
```

### Semantics

- On each traversal of the router, **exactly one** outgoing edge is
  activated, selected by a cyclic counter: `edge_index = counter % len(edges)`
- The counter is **auto-incremented** on each traversal
- The counter is **persisted** in the run state (analogous to `loopCounters`)
- The counter starts at 0 (the first edge declared in the workflow)
- On `resume` after pause, the counter is restored from the store

### Usage example

```iter
router refine_selector:
  mode: round_robin

agent claude_refine:
  delegate: "claude_code"
  ...

agent codex_refine:
  delegate: "codex"
  ...

workflow example:
  ...
  val_judge -> refine_selector when not ready as refine_loop(4)

  refine_selector -> claude_refine with { ... }
  refine_selector -> codex_refine with { ... }

  claude_refine -> val_fanout with { ... }
  codex_refine -> val_fanout with { ... }
  ...
```

On the first pass: `claude_refine` is selected.
On the second pass: `codex_refine` is selected.
On the third pass: `claude_refine` again. And so on.

### Simplified workflow

With `round_robin`, the `dual_model_plan_implement_review.iter` workflow
collapses to:

```
plan_fanout (fan_out_all) → claude_plan + codex_plan → plans_join → merge_plans
  → val_fanout (fan_out_all) → claude_val + codex_val → val_join → val_judge
    → [ready] → impl_selector (round_robin) → claude_implement | codex_implement
    → [not ready] → refine_selector (round_robin) → claude_refine | codex_refine
      → val_fanout (loop)
  → review_fanout (fan_out_all) → claude_review + codex_review → review_join → review_judge
    → [approved] → done
    → [not approved] → plan_fanout (outer loop with reviews)
```

23 nodes, zero duplication, intent visible at a glance.

## Alternatives considered

### 1. Status quo — cross-pair pattern only

The cross-pair pattern works and requires no runtime changes. It is used
in several existing examples (`todo_app_full_dual_model_delegate.iter`,
`feature_request_dual_model.iter`).

**Rejected because**: duplication grows combinatorially. Alternation
between 2 agents doubles the node count. With 3 agents the cross-pair
would produce 3× the nodes with 6 crossed paths. At 4, the explosion is
unmanageable. The pattern does not scale.

### 2. Sub-workflows / macros

Encapsulate the cross-pair pattern in a reusable sub-workflow to hide
the duplication.

**Rejected because**: the v1 DSL has no sub-workflow support. Adding it
would be a much larger change than a new router mode, with implications
for variable scoping, artifacts, and the store. Disproportionate to the
problem at hand.

### 3. Conditional router with user-provided state

Allow the router to evaluate an expression on a previous node's output
to choose an edge (e.g. `mode: condition`,
`when last_refiner == "claude" -> codex_refine`).

**Rejected because**: introduces a mini expression language into the DSL,
complicates parsing and validation, and `round_robin` covers the main
use case (deterministic alternation) more simply.

## Arguments in favor

### 1. Drastic surface reduction

Half of the `dual_model_plan_implement_review.iter` file is structural
boilerplate that adds nothing for the reader. Each duplicated node has
exactly the same delegate, the same prompts, the same schemas — only its
name differs to anchor different edges.

### 2. Readability and declarative intent

The cross-pair encodes the "alternate" intent indirectly, through graph
structure. A reader has to mentally reconstruct the pattern to recognize
that an alternation is happening. With `round_robin`, the intent is
explicit and declarative.

### 3. Maintainability

With cross-pair, modifying a prompt, a schema, or a `with {}` mapping
requires propagating the change across all pairs. A miss creates silent
divergence between pairs. With `round_robin`, each node exists exactly
once.

### 4. Composability

`round_robin` composes naturally with the other primitives:
- With bounded loops (`as loop(N)`): the alternation stops when the loop
  expires
- With `fan_out_all` upstream/downstream: you can alternate the
  implementer while parallelizing the reviewers
- Future extension to N agents without combinatorial explosion

### 5. New patterns become possible

- Team rotation across 3+ agents
- Asymmetric implementer/reviewer alternation
- Model diversity on repeated tasks (avoiding single-model bias)

## Arguments against

### 1. Introduces state in the router

Today, the `fan_out_all` router is **stateless**: it reads its edges and
spawns them. `round_robin` requires a persistent counter. This breaks
the invariant "a node depends only on its inputs and the graph."

**Mitigation**: `loopCounters` are already persistent runtime state,
managed and serialized analogously. `roundRobinCounters` follow exactly
the same pattern — this is not a precedent, it is a natural extension.

### 2. Semantics inside loops

When a `round_robin` is reached via a bounded loop, the question arises:
when do we increment the counter? On every traversal or on every full
loop cycle?

**Resolution**: increment on every traversal — that is the simplest and
most intuitive semantics. One loop cycle = one traversal = one
increment. The counter is a monotonically increasing integer modulo N.

### 3. Determinism and debugging

The execution path depends on the traversal history (the counter), not
only on node outputs. This complicates debugging: "why was Codex
chosen?" requires inspecting the counter's internal state.

**Mitigation**: emit a `router_selected` event in the run log, recording
the chosen edge and the counter value. The `inspect --events` tool makes
this information visible.

### 4. More complex graph validation

The IR compiler must enforce additional constraints for `round_robin`:
- At least 2 outgoing edges (otherwise it is a regular node)
- Target input schemas must be compatible (the same `with {}` feeds N
  targets)

**Mitigation**: these validations are simple to implement and follow the
existing model in `pkg/dsl/ir/validate.go`.

### 5. Risk of feature creep

After `round_robin`, demand will follow for `weighted_round_robin`,
`random`, `least_recently_used`...

**Mitigation**: limit v1 to `fan_out_all` and `round_robin`. Advanced
modes are future extensions, explicitly out of scope. The `RouterMode`
type is already an extensible enum.

## Implementation plan

### Files affected

| File | Modification |
|---|---|
| `grammar/iterion_v1.ebnf` | Add `round_robin` to the `router_mode` rule |
| `grammar/V1_SCOPE.md` | Document the new mode |
| `pkg/dsl/ast/ast.go` | Add `RouterModeRoundRobin` to the `RouterMode` enum |
| `pkg/dsl/parser/` | Parse `round_robin` as a value of `mode:` |
| `pkg/dsl/ir/ir.go` | Add `RouterRoundRobin` to the IR `RouterMode` type |
| `pkg/dsl/ir/compile.go` | Compile the AST mode into IR |
| `pkg/dsl/ir/validate.go` | Validate ≥ 2 outgoing edges, compatible schemas |
| `pkg/runtime/engine.go` | Edge selection by `counter % len(edges)` in `execRouter` / `findNext` |
| `pkg/store/` | Serialize/deserialize `roundRobinCounters` in the run state |
| `pkg/cli/diagram.go` | Distinct visual representation for `round_robin` |

### State structure

```go
// In RunState or equivalent
type RunState struct {
    // ... existing fields ...
    LoopCounters       map[string]int  // existing
    RoundRobinCounters map[string]int  // new — key: router nodeID
}
```

### Runtime logic (pseudo-code)

```go
func (e *Engine) execRouter(ctx context.Context, rs *RunState, nodeID string) (string, error) {
    node := e.workflow.Graph.Nodes[nodeID]
    edges := e.workflow.Graph.EdgesFrom(nodeID)

    switch node.RouterMode {
    case ir.RouterFanOutAll:
        return e.execFanOut(ctx, rs, nodeID)

    case ir.RouterRoundRobin:
        counter := rs.RoundRobinCounters[nodeID]
        selectedEdge := edges[counter % len(edges)]
        rs.RoundRobinCounters[nodeID] = counter + 1
        // Resolve inputs via the selected edge's with{}
        // Execute the target node
        return selectedEdge.Target, nil
    }
}
```

### Tests

- **Unit**: parsing `round_robin`, compiling, validating (≥ 2 edges,
  < 2 edges = error)
- **Integration**: minimal workflow with `round_robin` over 2 targets,
  asserting alternation across 4 iterations
- **E2E**: workflow with `round_robin` + bounded loop + resume,
  asserting counter persistence
- **Regression**: ensure `fan_out_all` is unchanged

### Migration of existing workflows

Once `round_robin` is implemented, the
`dual_model_plan_implement_review.iter` workflow can be simplified from
46 to 23 nodes. Existing examples that use the cross-pair pattern
(`todo_app_full_dual_model_delegate.iter`,
`feature_request_dual_model.iter`) remain valid — cross-pair is a
usage pattern, not a DSL constraint.

## Consequences

- The v1 DSL gains a routing primitive that covers a frequent use case
  (agent alternation) without resorting to structural duplication
- The runtime gains an additional state vector (`roundRobinCounters`) to
  persist and restore
- Future workflows can express alternation patterns declaratively and
  concisely
- The cross-pair pattern remains available for cases where finer control
  is required
- The `RouterMode` type is ready for future extensions (`weighted`,
  `random`, etc.) without architectural changes

---

## Addendum (2026-04-28) — Backend recommendations

The `round_robin` pattern described above remains fully valid. However,
the original choice of illustrating alternation with **Claude Code +
Codex** is no longer recommended: since this ADR was written, accumulated
experience has shown that the `codex` backend has significant limitations
(its tool set cannot be configured, it tends to fill its own context
window, and its integration is less polished). The compiler now emits a
`C030` warning when a node uses `backend: "codex"`.

For new workflows using `round_robin`, prefer alternating between:
- `claude_code` (delegate) + the in-process `claw` API with an OpenAI
  model (`model: "openai/gpt-5.4-mini"`), or
- two `claude_code` instances configured with different Claude models
  (e.g. Sonnet vs. Opus), or
- two direct `claw` models (e.g. `anthropic/claude-...` vs.
  `openai/gpt-...`).

Historical examples that used `codex` in this role have been migrated in
the same commit; see `examples/dual_model_plan_implement_review.iter`
for the current version.
