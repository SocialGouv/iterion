[← Documentation index](README.md) · [← Iterion](../README.md)

# The `.iter` DSL

Workflows are written in a declarative, indentation-significant language. The formal grammar is in [`grammar/iterion_v1.ebnf`](grammar/iterion_v1.ebnf).

## Variables

Define typed variables at the top level. These can be set at runtime with `--var key=value`:

```iter
vars:
  pr_title: string
  max_retries: int = 3
  verbose: bool = false
  config: json = { "key": "value" }
  tags: string[] = ["security", "performance"]
```

Supported types: `string`, `bool`, `int`, `float`, `json`, `string[]`.

## Prompts

Reusable prompt templates with `{{...}}` interpolation:

```iter
prompt review_system:
  You are a code reviewer specializing in {{vars.language}}.
  Focus on: {{vars.review_rules}}

prompt review_user:
  Review this code:
  {{input.code}}
  Previous feedback: {{outputs.prior_review.summary}}
```

## Schemas

Typed data contracts for structured agent I/O:

```iter
schema review_result:
  approved: bool
  summary: string
  issues: string[]
  confidence: string [enum: "low", "medium", "high"]
  score: float
  metadata: json
```

Supported field types: `string`, `bool`, `int`, `float`, `json`, `string[]`. Strings support `[enum: ...]` constraints.

## Node Types

### Agent

The primary execution unit. Calls an LLM with prompts, optionally uses tools, and returns structured output:

```iter
agent reviewer:
  model: "claude-sonnet-4-20250514"
  input: review_request
  output: review_result
  system: review_system
  user: review_user
  session: fresh
  tools: [git_diff, read_file, search_codebase]
  tool_max_steps: 10
  publish: review_artifact
  reasoning_effort: high
  readonly: true
```

| Property | Description |
|----------|-------------|
| `model` | LLM model identifier (supports `${ENV_VAR}`) |
| `delegate` | Offload to external agent: `claude_code` (recommended) or `codex` (discouraged, see [Delegation](delegation.md)) |
| `input` / `output` | Schema references for structured I/O |
| `publish` | Persist output as a named artifact |
| `system` / `user` | Prompt references |
| `session` | Context mode: `fresh` (default), `inherit`, `fork`, or `artifacts_only` |
| `tools` | List of allowed tool names |
| `tool_max_steps` | Max tool-use iterations (0 = unlimited) |
| `reasoning_effort` | Extended thinking: `low`, `medium`, `high`, `xhigh`, `max` |
| `readonly` | If `true`, prevents tool side effects (workspace safety) |

### Judge

Structurally identical to agents, but semantically intended for evaluation — typically no tools:

```iter
judge compliance_check:
  model: "claude-sonnet-4-20250514"
  input: plan_schema
  output: verdict_schema
  system: compliance_system
  user: compliance_user
```

### Router

Branches execution into parallel or conditional paths. Four modes are available:

```iter
router dispatch:
  mode: fan_out_all      # Send to ALL outgoing edges in parallel

router branch:
  mode: condition        # Route based on `when` clauses on edges

router alternate:
  mode: round_robin      # Cycle through targets one per iteration

router smart:
  mode: llm              # LLM decides which target(s) to route to
  model: "claude-sonnet-4-20250514"
  system: routing_prompt
  multi: true            # Allow selecting multiple targets
```

> For a deep dive on routing modes, edge rules, and convergence patterns, see [`routers.md`](routers.md).

### Join

Converges parallel branches back into a single path:

```iter
join merge:
  strategy: wait_all     # Wait for all branches (or: best_effort)
  require: [branch_a, branch_b]
  output: merged_result
```

- `wait_all` — waits for every incoming branch
- `best_effort` — proceeds when required branches finish, tolerates failures on others

### Human

Pauses the workflow for human input, or lets an LLM handle it:

```iter
## Always pause for human answers
human approval:
  input: approval_request
  output: approval_response
  instructions: approval_prompt
  mode: pause_until_answers
  min_answers: 1

## LLM auto-answers (never pauses)
human auto_review:
  mode: auto_answer
  model: "claude-sonnet-4-20250514"
  system: auto_review_prompt
  output: review_decision

## LLM decides whether to pause or auto-answer
human conditional:
  mode: auto_or_pause
  model: "claude-sonnet-4-20250514"
  system: decision_guidance
  instructions: review_questions
  output: review_decision
```

Resume a paused run with `iterion resume --run-id <id> --file <file> --answer key=value`.

### Tool

Direct shell command execution — no LLM involved:

```iter
tool run_tests:
  command: "make test"
  output: test_result
```

Supports `${ENV_VAR}` in the command string.

### Terminal Nodes

Every workflow must end at `done` (success) or `fail` (failure). These are built-in — you don't declare them.

## Workflows, Edges & Control Flow

A workflow ties nodes together with an entry point, optional budget, and edges:

```iter
workflow pr_review:
  entry: context_builder

  budget:
    max_duration: "30m"
    max_cost_usd: 10
    max_tokens: 400000

  context_builder -> reviewer
  reviewer -> done when approved
  reviewer -> context_builder when not approved as retry(3)
```

**Edge syntax:**

```iter
src -> dst                              # Unconditional
src -> dst when approved                # Conditional (bool field from src output)
src -> dst when not approved            # Negated condition
src -> dst as loop_name(5)              # Bounded loop (max 5 iterations)
src -> dst with {                       # Data mapping
  context: "{{outputs.src}}",
  config: "{{vars.my_var}}"
}
```

**Edge rules:**

- Non-router nodes can have at most one unconditional edge
- Conditional edges must be exhaustive (`when X` + `when not X`) or have an unconditional fallback
- All cycles must be declared with `as name(N)` — undeclared cycles are a compile error
- Inside loops, you can access iteration history with `{{outputs.node_id.history}}`

## Template Expressions

Templates use `{{...}}` interpolation:

| Reference | Description |
|-----------|-------------|
| `{{vars.name}}` | Workflow variable |
| `{{input.field}}` | Current node's input field |
| `{{outputs.node_id}}` | Full output of a previously executed node |
| `{{outputs.node_id.field}}` | Specific field from a node's output |
| `{{outputs.node_id.history}}` | Array of all outputs across loop iterations |
| `{{artifacts.name}}` | Published artifact by name |

Environment variables are supported with `${ENV_VAR}` syntax (resolved at compile time).

## MCP Servers

You can declare MCP (Model Context Protocol) servers directly in your `.iter` files:

```iter
mcp_server code_tools:
  transport: stdio
  command: "npx"
  args: ["-y", "@anthropic-ai/claude-code-mcp"]

mcp_server api_server:
  transport: http
  url: "http://localhost:3000/mcp"
```

Agents can then reference these servers:

```iter
agent worker:
  model: "claude-sonnet-4-20250514"
  mcp:
    servers: [code_tools, api_server]
```

Supported transports: `stdio` (requires `command`), `http` (requires `url`).

## Budget

Control costs and prevent runaway execution:

```iter
budget:
  max_parallel_branches: 4    # Concurrent branch limit
  max_duration: "30m"         # Global timeout
  max_cost_usd: 10.0          # Cost cap in USD
  max_tokens: 500000          # Total token budget
  max_iterations: 50          # Loop iteration limit
```

The budget is shared across all branches. When a limit is hit, the engine emits a `budget_exceeded` event and stops the run.

## Worktree & Sandbox

Two top-level workflow directives let a run isolate itself from the host:

```iter
workflow safe_pr_fix:
  worktree: auto       # Run inside a fresh git worktree; persist commits to a branch on success
  sandbox: auto        # Run all tool/agent calls inside a Docker/Podman container
  entry: planner
  ...
```

- **`worktree: auto`** — the engine creates `<store-dir>/worktrees/<run-id>`, executes the workflow there, then on a clean exit creates a persistent branch (default `iterion/run/<friendly-name>`) and fast-forwards the user's currently-checked-out branch to that HEAD. Override with `--merge-into`, `--branch-name`, `--merge-strategy`, or `--auto-merge=false`. See [resume.md](resume.md).
- **`sandbox: auto`** — reads `.devcontainer/devcontainer.json` and runs each agent/tool node inside an isolated container with the worktree bind-mounted at `/workspace`, plus an HTTP CONNECT proxy enforcing a network allowlist. Use `iterion sandbox doctor` to verify host capabilities. The `claw` backend cannot yet be sandboxed (a future `cmd/iterion-claw-runner` binary will close that gap). See [sandbox.md](sandbox.md).

---

## Related references

- [`grammar/iterion_v1.ebnf`](grammar/iterion_v1.ebnf) — formal EBNF grammar
- [`references/dsl-grammar.md`](references/dsl-grammar.md) — readable grammar reference
- [`references/diagnostics.md`](references/diagnostics.md) — all validation diagnostic codes (C001–C043)
- [`references/patterns.md`](references/patterns.md) — 10 reusable workflow patterns
- [`attachments.md`](attachments.md) — attachment handling
- [`workflow_authoring_pitfalls.md`](workflow_authoring_pitfalls.md) — required reading before authoring workflows that commit code
