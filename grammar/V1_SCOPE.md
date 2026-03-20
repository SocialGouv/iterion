# V1 Scope — Iterion Grammar & AST

## Primitives Covered by the V1 Grammar and AST

| Primitive | DSL Keyword | AST Node | Notes |
|-----------|-------------|----------|-------|
| Variables | `vars:` | `VarsBlock`, `VarField` | Top-level and workflow-level |
| Prompts | `prompt <name>:` | `PromptDecl` | Free text with `{{...}}` |
| Schemas | `schema <name>:` | `SchemaDecl`, `SchemaField` | Types: string, bool, int, float, json, string[]; enum constraint |
| Agent | `agent <name>:` | `AgentDecl` | model, input, output, publish, system, user, session, tools, tool_max_steps |
| Judge | `judge <name>:` | `JudgeDecl` | Structurally identical to agent |
| Router | `router <name>:` | `RouterDecl` | Modes: fan_out_all, condition |
| Join | `join <name>:` | `JoinDecl` | Strategies: wait_all, best_effort; require, output |
| Human | `human <name>:` | `HumanDecl` | input, output, publish, instructions, mode, min_answers |
| Tool (node) | `tool <name>:` | `ToolNodeDecl` | command, output (direct execution without LLM) |
| done / fail | (reserved) | Edge targets | No declaration, recognized by the parser |
| Workflow | `workflow <name>:` | `WorkflowDecl` | vars, entry, budget, edges |
| Budget | `budget:` | `BudgetBlock` | max_parallel_branches, max_duration, max_cost_usd, max_tokens, max_iterations |
| Edge | `src -> dst` | `Edge` | with, when, as (loop) |
| When | `when [not] <cond>` | `WhenClause` | Condition + negation |
| Loop | `as <name>(<N>)` | `LoopClause` | Named and bounded loop |
| With | `with { ... }` | `WithEntry` | Inter-node data mapping |
| Session | `session:` | `SessionMode` | fresh, inherit, artifacts_only |
| Publish | `publish:` | Field on Agent/Judge/Human | Persistent artifact |
| Template | `{{...}}` | In string values | vars.X, input.X, outputs.X[.Y], artifacts.X |
| Env refs | `${...}` | In string values | Runtime resolution |
| Comments | `## ...` | `Comment` | In file and in workflow |

## Explicitly Out of V1

| Concept | Reason |
|---------|--------|
| **Imports / includes** | One file = one workflow. No module system in V1. |
| **Node inheritance** | No `extends` or node composition. Duplication is acceptable. |
| **Composite types in schemas** | No nested types or `map`. `json` serves as a catch-all type. |
| **Complex conditional expressions** | `when` takes a simple identifier, no compound boolean expressions (&&, \|\|). |
| **Router mode: condition with expressions** | The `condition` mode is declared but complex conditional routing rules are out of V1. |
| **Sub-workflows / workflow calls** | A workflow cannot call another workflow. |
| **Retry / backoff on nodes** | Handled at runtime/policy level, not in the DSL. |
| **Per-node timeouts** | Only the global budget `max_duration` is supported in V1. |
| **Dynamic variables** | Vars are declared statically; no computed vars. |
| **Annotations / metadata** | No free-form annotation system on nodes. |
| **Semantic validation** | The grammar and AST do not validate cross-references; that is handled in P2 (AST → IR compilation). |
| **Template typing** | `{{...}}` are opaque strings in the AST; type checking is done at compilation. |
| **Multi-workflow per file** | Technically possible in the grammar (the `Workflows` field is a slice), but one workflow per file is the V1 convention. |
