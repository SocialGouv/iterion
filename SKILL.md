---
name: iterion-dsl
description: >
  Use when asked to write .iter workflow files, create Iterion DSL workflows,
  define multi-agent orchestration pipelines, or debug .iter syntax errors.
  Triggers on: "write an .iter file", "iterion workflow", "iter DSL",
  "multi-agent pipeline", "workflow orchestration", "agent judge router".
version: 0.1.0
---

# Iterion DSL Reference

Iterion is a workflow orchestration engine for multi-agent AI pipelines. Workflows are defined in `.iter` files using an **indentation-sensitive DSL** (2 spaces per level). One workflow per file.

## File Structure

Blocks must appear in this order:

```
vars:             ## optional — global variables
mcp_server X:     ## optional — MCP server declarations
prompt X:         ## prompt templates (referenced by name)
schema X:         ## typed data contracts (referenced by name)
agent X:          ## LLM nodes with tools
judge X:          ## LLM nodes producing verdicts (typically no tools)
router X:         ## deterministic or LLM-based routing
human X:          ## human-in-the-loop interaction points
tool X:           ## direct command execution (no LLM)
compute X:        ## deterministic expression node (no LLM, no shell)
workflow X:       ## entry point, edges, budget
```

Comments start with `##` and extend to end of line.

## Node Types

| Type | Purpose | Key Properties |
|------|---------|----------------|
| `agent` | LLM node with tools | model/backend, input, output, system, user, tools, session |
| `judge` | LLM verdict node | Same as agent (semantically: produces verdicts, rarely uses tools) |
| `router` | Fan-out or conditional routing | mode (fan_out_all/condition/round_robin/llm) |
| `human` | Human-in-the-loop pause | interaction (human/llm/llm_or_human), instructions |
| `tool` | Direct shell command | command, [input], output |
| `compute` | Deterministic expression node (no LLM, no shell) | input, output, expr (key→expression mapping) |
| `done` | Terminal: success | Reserved identifier, not declared — used as edge target |
| `fail` | Terminal: failure | Reserved identifier, not declared — used as edge target |

## Agent / Judge Properties

Agents and judges share identical syntax. All properties are optional except `model` or `backend` (one is required).

```iter
agent my_agent:
  model: "claude-sonnet-4-20250514"       ## LLM model identifier (supports ${ENV_VAR})
  backend: "claude_code"                   ## execution backend: claw | claude_code | codex
  input: my_input_schema                   ## schema reference for input data
  output: my_output_schema                 ## schema reference for structured output
  publish: my_artifact                     ## persist output as named artifact
  system: my_system_prompt                 ## prompt reference for system message
  user: my_user_prompt                     ## prompt reference for user message
  session: fresh                           ## fresh | inherit | fork | artifacts_only
  tools: [Read, Edit, Write, Bash]         ## tool capability names
  tool_policy: [Bash]                         ## tool policy refs (same syntax as tools)
  tool_max_steps: 15                       ## max tool-use iterations (0 = unlimited)
  max_tokens: 8000                         ## per-call output cap (0 = backend default)
  reasoning_effort: high                   ## low | medium | high | xhigh | max
  readonly: true                           ## not considered mutating for workspace safety
  interaction: llm_or_human                ## none | human | llm | llm_or_human
  interaction_prompt: my_interaction_prompt ## prompt ref for LLM interaction decisions
  interaction_model: "claude-haiku-4-5-20251001"  ## model for interaction (fallback to model)
  await: wait_all                          ## wait_all | best_effort (convergence strategy)
  mcp:                                     ## MCP configuration block
    inherit: true
    servers: [my_mcp_server]
    disable: [unwanted_server]
  compaction:                              ## per-node compaction override (optional)
    threshold: 0.85                         ## fraction of context window in (0, 1]
    preserve_recent: 4                      ## minimum recent turns kept verbatim (>= 1)
```

**Backends:**
- `claw` — default, in-process LLM call. Recommended for read-only LLM nodes (judges, reviewers, planners).
- `claude_code` — recommended for nodes that need real tool/shell access (implementers, fixers).
- `codex` — supported but discouraged (raises C030); the integration is less ergonomic and tool gating is limited.

**Session modes:**
- `fresh` — new context (default)
- `inherit` — inherit parent session (forbidden on convergence points)
- `fork` — non-consuming fork from parent (forbidden on convergence points)
- `artifacts_only` — only persistent artifacts in context

**Interaction modes:**
- `none` — no interaction capability (default for agent/judge)
- `human` — always pause for human input (default for human nodes)
- `llm` — LLM auto-answers interaction questions
- `llm_or_human` — LLM decides whether to answer or escalate to human

## Router Properties

```iter
router my_router:
  mode: fan_out_all          ## fan_out_all | condition | round_robin | llm
```

**Modes:**
- `fan_out_all` — send to ALL outgoing edges in parallel
- `condition` — route based on `when` clauses on edges
- `round_robin` — cycle through targets one at a time
- `llm` — LLM decides which target(s) to route to (requires additional properties)

**LLM router additional properties (only valid with `mode: llm`):**

```iter
router my_llm_router:
  mode: llm
  model: "claude-sonnet-4-20250514"   ## required for llm mode
  system: router_system_prompt        ## prompt ref (optional)
  user: router_user_prompt            ## prompt ref (optional)
  multi: true                         ## allow multi-target selection (optional)
```

Routers do NOT support `await` — they are fan-out sources, not convergence targets.

## Human Properties

```iter
human my_gate:
  input: gate_input_schema
  output: gate_output_schema
  publish: gate_result
  instructions: gate_instructions_prompt   ## prompt ref shown to human
  interaction: human                       ## human | llm | llm_or_human
  interaction_prompt: decision_prompt      ## prompt ref for LLM decisions
  interaction_model: "claude-haiku-4-5-20251001"
  min_answers: 1                           ## minimum human answers required
  model: "claude-sonnet-4-20250514"        ## required for llm/llm_or_human modes
  system: human_system_prompt              ## prompt ref for LLM system prompt
  await: wait_all                          ## convergence strategy
```

## Tool Properties

Direct command execution without LLM. Supports `${ENV_VAR}` in command (resolved at compile time) and `{{input.field}}` for runtime values.

```iter
tool run_tests:
  command: "make test"
  input: test_input_schema       ## optional — lets {{input.X}} render in command
  output: test_result_schema
  await: wait_all
```

A `string[]` field rendered via `{{input.list_field}}` expands as space-joined items, so a tool can take an arbitrary list of arguments without manual concatenation.

## Compute Properties

Deterministic node — evaluates a list of expressions over `vars / input / outputs / artifacts / loop / run` and emits a structured output. No LLM, no shell. Useful for streak detection, boolean combinations, counters, simple aggregations that shouldn't burn tokens.

```iter
schema streak_state:
  consecutive_passes: int
  ready: bool

compute streak:
  input: streak_input               ## optional input schema
  output: streak_state              ## required output schema
  expr:
    consecutive_passes: "loop.refine.iter"
    ready: "outputs.review.passed && loop.refine.iter >= 2"
  await: wait_all
```

Built-ins available inside expressions: `length(x)`, `concat(a, b, …)`, `unique(list)`, `contains(list, item)`. A `compute` node with no `expr` entries raises C039; an unparseable expression raises C040.

## Schema Syntax

Typed data contracts referenced by nodes. Field types: `string`, `bool`, `int`, `float`, `json`, `string[]`.

```iter
schema review_result:
  summary: string
  blockers: string[]
  approved: bool
  confidence: string [enum: "low", "medium", "high"]
  details: json
  score: float
  retry_count: int
```

## Prompt Syntax

Free-form text templates with `{{...}}` interpolation. Referenced by name from node properties.

```iter
prompt review_system:
  You are a senior code reviewer.
  Analyze the code changes and produce a structured review.

prompt review_user:
  Review this PR.

  Title: {{input.pr_title}}
  Diff: {{input.diff}}
  Previous feedback: {{outputs.previous_review.summary}}
  Saved context: {{artifacts.pr_context}}
  Config: {{vars.review_rules}}
```

**Template references:**
- `{{vars.X}}` — workflow variable
- `{{input.X}}` — current node's input field
- `{{outputs.node_id}}` — full output of a node
- `{{outputs.node_id.field}}` — specific field from a node's output
- `{{outputs.node_id.history}}` — array of all outputs across loop iterations (only valid for nodes in a loop)
- `{{artifacts.X}}` — persistent artifact by name
- `{{loop.<name>.iter}}` — current 0-based iteration of a declared loop
- `{{loop.<name>.previous.<field>}}` — previous iteration's field value on the loop's controlling node
- `{{run.id}}`, `{{run.store_dir}}` — run-scoped metadata
- `${ENV_VAR}` — environment variable (resolved at compile time)

## Workflow Block

```iter
workflow my_workflow:
  vars:
    pr_title: string
    max_retries: int = 3
    verbose: bool = false

  entry: first_node

  default_backend: "claude_code"   ## fallback backend for nodes that don't set their own
  tool_policy: [Bash]                 ## workflow-level tool policy refs
  worktree: auto                    ## auto | none — see "Worktree isolation" below

  budget:
    max_parallel_branches: 4
    max_duration: "30m"
    max_cost_usd: 10.0
    max_tokens: 500000
    max_iterations: 50

  compaction:                       ## workflow-level compaction defaults
    threshold: 0.85
    preserve_recent: 4

  interaction: llm_or_human     ## workflow-level default for all nodes

  mcp:
    autoload_project: true
    servers: [my_server]
    disable: [unwanted]

  ## Edges
  first_node -> second_node
  second_node -> done
```

### Worktree isolation

`worktree: auto` runs the workflow inside a per-run git worktree at `<store-dir>/worktrees/<run-id>/` so the user's main working tree stays untouched and WIP edits stay invisible to the workflow. Clean exits automatically remove that worktree. Failed, cancelled, or error exits preserve it and log the path so the operator can inspect it and, when desired, clean it up manually with `git worktree remove --force <path>`. Omit the field (or set `none`) to run in place. See [examples/vibe_feature_dev.iter](examples/vibe_feature_dev.iter) for a workflow that opts in.

## Edge Syntax

```
src -> dst [when [not] field] [as loop_name(max_iter)] [with { mappings }]
```

**Components:**
- `when field` — route only if `field` is true in source output (field must be `bool`)
- `when not field` — route only if `field` is false
- `when "<expression>"` — route on a quoted compound expression (same namespaces and built-ins as compute: `vars / input / outputs / artifacts / loop / run`, plus `length / concat / unique / contains`)
- `as loop_name(N)` — declare a bounded loop (max N iterations)
- `with { ... }` — data mappings for the target node's input

**Examples:**

```iter
## Simple edge
agent_a -> agent_b

## Conditional edges (must cover all cases or have a fallback)
judge -> done when approved
judge -> fixer when not approved

## Bounded loop
judge -> agent as refine_loop(5) when not approved with {
  feedback: "{{outputs.judge.summary}}",
  history: "{{outputs.agent.history}}"
}

## Data mapping
builder -> reviewer with {
  pr_context: "{{outputs.builder}}",
  rules: "{{vars.review_rules}}"
}
```

**Edge rules:**
- Non-router nodes can have at most ONE unconditional edge
- Conditional edges must either be exhaustive (`when X` + `when not X`) or have an unconditional fallback
- `fan_out_all` / `round_robin` / `llm` routers can have multiple unconditional edges
- `llm` router edges must NOT have `when` conditions (the LLM decides routing)
- `round_robin` and `llm` routers need at least 2 outgoing edges
- Cycles MUST have a loop declaration (`as name(N)`) — undeclared cycles produce diagnostic C019
- `done` and `fail` are reserved targets — do not declare them as nodes

## MCP Server Declarations

Top-level declarations for reusable MCP servers.

```iter
mcp_server code_tools:
  transport: stdio
  command: "npx"
  args: ["-y", "@anthropic-ai/claude-code-mcp"]

mcp_server api_server:
  transport: http
  url: "http://localhost:3000/mcp"
```

**Transport types:** `stdio` (requires `command`), `http` (requires `url`). SSE is not supported in V1.

## Convergence (Await)

There is NO `join` keyword. Convergence is handled by the `await` property on the receiving node.

When a node receives edges from multiple parallel branches (e.g., after a `fan_out_all` router), set `await` on the target:

```iter
router fan_out:
  mode: fan_out_all

agent worker_a:
  ## ...

agent worker_b:
  ## ...

judge merge:
  await: wait_all       ## waits for BOTH worker_a and worker_b
  ## ...
```

- `wait_all` — wait for all incoming branches before executing
- `best_effort` — proceed when possible, tolerate branch failures

**Important:** `session: inherit` and `session: fork` are forbidden on convergence points (diagnostic C009). Use `session: fresh` or `session: artifacts_only`.

## Variables

Top-level or workflow-level. Types: `string`, `bool`, `int`, `float`, `json`, `string[]`.

```iter
vars:
  workspace_dir: string = "${PROJECT_DIR}"
  max_retries: int = 3
  verbose: bool = false
```

Workflow-level vars override top-level vars with the same name. Pass values at runtime: `iterion run file.iter --var key=value`.

## Common Mistakes

1. **Missing `model` or `backend`** — Every agent/judge needs one. Set `model: "${MODEL}"` for env-based config or `backend: "claude_code"` for delegation. (Earlier drafts used `delegate:`; that keyword has been removed — use `backend:` everywhere.)

2. **`when` field not boolean** — Condition fields must be `bool` in the source output schema. Use `approved: bool` not `status: string`.

3. **Undeclared cycles** — Every back-edge must have `as loop_name(N)`. Without it you get C019 (infinite loop risk).

4. **`session: inherit` on convergence point** — Nodes with `await:` or multiple incoming sources cannot use `inherit` or `fork`. Use `fresh` or `artifacts_only`.

5. **Non-exhaustive conditions without fallback** — If you have `when approved` you must also have `when not approved` OR an unconditional fallback edge.

6. **Schema not declared** — Every schema referenced by `input:` or `output:` must be declared as a top-level `schema` block.

7. **`when` on LLM router edges** — LLM routers select targets directly. Don't add conditions to their edges.

8. **Single edge on round_robin/llm router** — These routers need at least 2 outgoing edges.

## Minimal Example

```iter
prompt review_system:
  You are a code reviewer. Evaluate the code and decide if it passes review.

prompt review_user:
  Review this code:
  {{input.code}}

schema review_input:
  code: string

schema review_output:
  approved: bool
  summary: string

agent reviewer:
  model: "${MODEL}"
  input: review_input
  output: review_output
  system: review_system
  user: review_user

workflow code_review:
  entry: reviewer
  reviewer -> done when approved
  reviewer -> fail when not approved
```

## CLI Quick Reference

```bash
iterion validate file.iter              # parse + compile + validate
iterion run file.iter --var key=value   # execute workflow
iterion diagram file.iter --view full   # generate Mermaid diagram
iterion inspect --run-id <id>           # view run state
iterion resume --run-id <id> --file f --answers-file a.json  # resume paused run
```

## See Also

- [references/dsl-grammar.md](references/dsl-grammar.md) — formal grammar specification
- [references/patterns.md](references/patterns.md) — common workflow patterns with examples
- [references/diagnostics.md](references/diagnostics.md) — all validation diagnostic codes
- [examples/skill/](examples/skill/) — minimal self-contained .iter examples
- [examples/](examples/) — production-grade workflow examples
