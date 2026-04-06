# Routers

Routers control how execution flows through the workflow graph. When a router is reached, it decides which downstream node(s) to activate next. There are four routing modes, each suited to different orchestration patterns.

## Overview

- **`fan_out_all`** — run all downstream branches in parallel
- **`condition`** — pick one branch based on a boolean field from a previous node
- **`round_robin`** — cycle through branches in order, one per traversal
- **`llm`** — let an LLM decide which branch(es) to take

## Syntax

```iter
router <name>:
  mode: fan_out_all | condition | round_robin | llm
```

LLM routers accept additional properties:

```iter
router fix_router:
  mode: llm
  model: "anthropic/claude-sonnet-4-6"   # or delegate: "claude_code"
  system: routing_prompt                  # optional prompt ref
  user: user_prompt                       # optional prompt ref
  multi: true                             # select multiple routes (default: false)
```

Using `model` makes a direct API call. Using `delegate` routes through a delegation backend (like `claude_code` or `codex`) — useful when no raw API key is available. If neither is set, the engine falls back to a built-in default model.

---

## `fan_out_all` — parallel dispatch

This is the default mode. The router sends execution to **every** outgoing edge simultaneously. Each target runs in its own branch, and branches converge at a downstream `join` node.

```iter
router review_fanout:
  mode: fan_out_all

workflow example:
  ...
  review_fanout -> claude_review
  review_fanout -> codex_review
  claude_review -> review_join
  codex_review -> review_join
```

The router itself is a pass-through — it forwards its input unchanged to all targets. The number of concurrent branches is bounded by the `max_parallel_branches` budget setting. For workspace safety, only one mutating branch (an agent or human with tools) is allowed at a time; read-only branches can run freely in parallel.

---

## `condition` — boolean branching

A condition router picks a single target based on boolean fields in the upstream node's output. The routing logic is expressed on the edges, not in the router itself.

```iter
router decision:
  mode: condition

workflow example:
  ...
  judge -> decision
  decision -> fix_agent when not approved
  decision -> done when approved
```

When the `judge` node produces `{ "approved": true }`, the edge `decision -> done` is taken. When `approved` is false (or absent), the `when not approved` edge matches instead. If no conditional edge matches, the first unconditional edge is used as a fallback.

> **Note:** Condition routing is syntactic sugar — the same `when` / `when not` evaluation happens after every node, not just routers. The condition router makes the branching intent explicit in the graph.

---

## `round_robin` — cyclic alternation

Each time the router is traversed, it selects the **next** outgoing edge in declaration order, wrapping around after the last one.

```iter
router refine_selector:
  mode: round_robin

workflow example:
  ...
  val_judge -> refine_selector when not ready as refine_loop(4)
  refine_selector -> claude_refine
  refine_selector -> codex_refine
```

| Traversal | Selected target |
|-----------|----------------|
| 1st | `claude_refine` |
| 2nd | `codex_refine` |
| 3rd | `claude_refine` |
| 4th | `codex_refine` |

The counter persists across pause/resume cycles — if a run is paused and later resumed, the alternation picks up where it left off. This mode is ideal for alternating between agents (e.g. Claude and Codex) in a refinement loop, avoiding the need to duplicate nodes.

---

## `llm` — AI-driven routing

An LLM reads the workflow context and decides which route to take. This is the only mode that makes an LLM call.

### How it works

1. The engine collects all outgoing edge targets as **route candidates** (e.g. `["fix_code", "fix_docs", "fix_tests"]`).
2. A system prompt (yours, plus an appended routing instruction) tells the LLM to pick from these candidates.
3. The LLM produces structured output matching an auto-generated schema:
   - Single mode: `{ "selected_route": "fix_code", "reasoning": "..." }`
   - Multi mode: `{ "selected_routes": ["fix_code", "fix_tests"], "reasoning": "..." }`
4. The engine validates the selection and dispatches accordingly. In multi mode, selected targets run in parallel (like `fan_out_all`, but only for the subset chosen by the LLM).

### Single route example

```iter
prompt routing_prompt:
  Based on the review findings, decide whether
  the code, the docs, or the tests need fixing.

router fix_router:
  mode: llm
  model: "anthropic/claude-sonnet-4-6"
  system: routing_prompt

workflow example:
  ...
  fix_router -> fix_code
  fix_router -> fix_docs
  fix_router -> fix_tests
```

### Multi route example

With `multi: true`, the LLM can select several routes at once. Selected targets run in parallel and converge at a join node.

```iter
router fix_router:
  mode: llm
  delegate: "claude_code"
  system: routing_prompt
  multi: true

workflow example:
  ...
  fix_router -> fix_code
  fix_router -> fix_docs
  fix_router -> fix_tests
  fix_code -> fix_join
  fix_docs -> fix_join
  fix_tests -> fix_join
```

### Model resolution

When using `model`, the engine resolves the model identifier through this chain:
1. The `model` field value (with environment variable expansion)
2. The `ITERION_DEFAULT_SUPERVISOR_MODEL` environment variable
3. Built-in default: `anthropic/claude-sonnet-4-6`

When using `delegate`, the named backend (e.g. `claude_code`, `codex`) handles the LLM call entirely — no API key or model configuration needed on the Iterion side.

---

## Convergence and joins

Parallel branches — whether from `fan_out_all` or `llm` multi-mode — must converge at a `join` node downstream. The engine detects the convergence point automatically. See the join node documentation for `wait_all` vs `best_effort` strategies.

---

## Compile-time checks

The compiler catches common mistakes at compile time:

- **LLM-only properties on non-LLM routers** — using `model`, `delegate`, `system`, `user`, or `multi` on a `fan_out_all`, `condition`, or `round_robin` router is an error.
- **Missing model and delegate on LLM routers** — if neither `model` nor `delegate` is set, a warning is emitted (the built-in default model will be used at runtime).
- **Conditional edges on LLM routers** — LLM routers must use unconditional edges because the LLM decides the route, not edge conditions.
