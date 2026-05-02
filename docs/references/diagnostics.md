# Iterion DSL — Validation Diagnostics

All diagnostic codes emitted during compilation (`ir.Compile`) and validation (`ir.Validate`). Diagnostics are either **errors** (block execution) or **warnings** (informational).

## Compilation Diagnostics

| Code | Severity | Description | Cause | Fix |
|------|----------|-------------|-------|-----|
| **C001** | error | Unknown node reference | An edge references a node that is not declared | Declare the node or fix the name typo |
| **C002** | error | Unknown schema reference | A node's `input:` or `output:` references an undeclared schema | Declare the schema or fix the name |
| **C003** | error | Unknown prompt reference | A node's `system:` or `user:` references an undeclared prompt | Declare the prompt or fix the name |
| **C004** | error | Bad template reference | A `{{...}}` template expression is malformed | Use `{{vars.X}}`, `{{input.X}}`, `{{outputs.node.field}}`, or `{{artifacts.X}}` |
| **C005** | error | Duplicate loop definition | Multiple edges share a loop name but disagree on `max_iterations` | Use the same `max_iterations` value or use different loop names |
| **C006** | error | No workflow found | The file has no `workflow` declaration | Add a `workflow name:` block |
| **C007** | error | Multiple workflows | More than one `workflow` block found | V1 supports one workflow per file — remove extras |
| **C008** | error | Missing entry node | The `entry:` node name doesn't match any declared node | Fix the entry name or declare the node |
| **C018** | error | Missing model or backend | An agent/judge has neither `model:` nor `backend:`, and `ITERION_DEFAULT_SUPERVISOR_MODEL` is not set | Add `model: "..."` or `backend: "..."` to the node |
| **C024** | error | Duplicate MCP server | A `mcp_server` name is declared more than once | Use unique names for each MCP server |
| **C025** | error | Invalid MCP server config | MCP server misconfigured (e.g., stdio without command, http without url) | Match properties to transport type: stdio needs `command`, http needs `url` |
| **C030** | warning | Codex backend discouraged | A node uses `backend: "codex"` | Codex is still supported but has limitations (cannot configure tool set, fills its own context window, weaker integration). Prefer `backend: "claude_code"` for tool-using agents or `claw` (default) with an OpenAI model (`model: "openai/gpt-5.4-mini"`) for judges/reviewers. |
| **C039** | error | Compute node has no expressions | A `compute` node was declared without any `expr: key: "<expression>"` entries | Add at least one expression mapping an output schema field to an expression — or remove the node |
| **C040** | error | Expression failed to parse | An expression in a `compute` node, in a quoted `when "..."` clause, or in a template ref isn't valid | Check operators, parentheses, namespace prefixes (`vars / input / outputs / artifacts / loop / run`), and built-in calls (`length`, `concat`, `unique`, `contains`) |
| **C041** | error | Duplicate node id | Two declarations share the same node name across agents/judges/routers/humans/tools/computes | Rename one — node ids are a single global namespace |
| **C042** | error | Reserved node name | A user node is named `done` or `fail` (those are reserved terminal targets) | Pick a different node name |

## Validation Diagnostics

| Code | Severity | Description | Cause | Fix |
|------|----------|-------------|-------|-----|
| **C009** | error | Session at convergence point | A node with `await:` (or multiple incoming sources) uses `session: inherit` or `session: fork` | Change to `session: fresh` or `session: artifacts_only` |
| **C010** | error | Multiple unconditional edges | A non-router node has more than one unconditional outgoing edge | Keep only one default edge, or use a router for fan-out |
| **C011** | error | Ambiguous conditions | Same condition field appears twice with same polarity from the same source | Remove the duplicate edge or use different conditions |
| **C012** | error | Missing fallback | A node has conditional edges but no unconditional fallback and conditions aren't exhaustive | Add `when not X` to complement `when X`, or add an unconditional edge |
| **C013** | error | Condition field not boolean | A `when` clause references a field that isn't `bool` in the source output schema | Change the schema field to `bool` |
| **C014** | error | Condition field not found | A `when` clause references a field that doesn't exist in the source output schema | Add the field to the schema or fix the field name |
| **C016** | error | Unreachable node | A declared node cannot be reached from the workflow's `entry:` node | Add edges to reach the node, or remove the unused declaration |
| **C017** | error | History ref not in loop | `{{outputs.node.history}}` is used but the referenced node is not part of any declared loop | Add a loop declaration (`as loop_name(N)`) to the edge cycle, or remove the `.history` reference |
| **C019** | error | Undeclared cycle | A cycle (back-edge) exists without any loop declaration on its edges | Add `as loop_name(N)` to the back-edge to bound the cycle |
| **C020** | error | Round-robin too few edges | A `round_robin` router has fewer than 2 unconditional outgoing edges | Add at least 2 outgoing edges for alternation |
| **C021** | error | LLM router too few edges | An `llm` router has fewer than 2 outgoing edges | Add at least 2 outgoing edges for the LLM to choose from |
| **C022** | error | LLM router edge has condition | An edge from an `llm` router has a `when` clause | Remove the `when` clause — LLM routers select targets directly |
| **C023** | error | LLM-only property on non-LLM router | Properties `model`, `backend`, `system`, `user`, `multi`, or `reasoning_effort` are set on a router that isn't `mode: llm` | Remove these properties or change the mode to `llm` |
| **C024** | error | Invalid reasoning effort | `reasoning_effort` has a value other than `low`, `medium`, `high`, `xhigh`, `max` | Use one of the five valid values |
| **C026** | error | Invalid loop iterations | A loop's `max_iterations` is less than 1 | Set `max_iterations` to at least 1 |
| **C028** | error | Duplicate with-mapping key | The same `with` key appears on multiple non-conditional edges to the same target | Use unique keys, or make edges conditional/convergent |
| **C031** | error | outputs ref field not in output schema | `{{outputs.<node>.<field>}}` references a field absent from that node's `output:` schema | Reference an existing field, or add the field to the schema |
| **C032** | warning | outputs ref on schemaless node | `{{outputs.<node>.<field>}}` targets a node that has no `output:` schema, so the field cannot be verified | Add an `output:` schema to the source node, or drop the field access |
| **C033** | error | Undeclared variable | `{{vars.X}}` (or `vars.X` inside an expression) targets a variable not declared in the file-level or workflow-level `vars:` block | Declare the variable, or fix the name |
| **C034** | error | input ref field not in input schema | `{{input.<field>}}` references a field absent from the consuming node's `input:` schema | Reference an existing field, or add it to the schema |
| **C035** | error | Unknown artifact | `{{artifacts.X}}` targets an artifact never produced via `publish:` | Add `publish: <name>` on a prior node, or fix the artifact name |
| **C036** | error | Reference to non-reachable node | `{{outputs.<node>...}}` targets a node not reachable from the entry before the consumer | Reorder the graph or wire an edge so the producer runs first |
| **C037** | warning | Node max_tokens exceeds workflow budget | A node-level `max_tokens` is greater than the workflow's `budget.max_tokens` | Lower the node cap, or raise the workflow budget |
| **C038** | error | Unsupported MCP auth type | `mcp_server.auth.type` is something other than `oauth2` (the only wired type) | Drop the `auth:` block, or change `type` to `oauth2` |
| **C043** | error | Invalid compaction values | `compaction.threshold` is outside `(0, 1]` or `compaction.preserve_recent` is `< 1` | Use a fraction like `0.85` for `threshold` and an integer `>= 1` for `preserve_recent`; omit either to inherit the default |

> **Code reuse note:** `C030` is currently emitted with two distinct
> meanings — the compile-time *Codex backend discouraged* warning shown
> above, **and** a validator-side error for `outputs.<node>` references
> targeting a non-existent node. The latter is reported as code `C030`
> in error messages even though the row above describes the codex
> case. When you see `C030` for an `outputs.<unknown>` reference, fix
> the reference; when you see it for `backend: "codex"`, treat it as a
> warning. Disambiguating the codes is tracked as a follow-up.

## Quick Troubleshooting

**"I get C019 (undeclared cycle)"**
Every back-edge (edge that creates a cycle) needs `as loop_name(N)`. Example:
```iter
judge -> agent when not approved as retry(3) with { ... }
```

**"I get C009 (session at convergence)"**
Nodes that receive from multiple branches (via `await:` or fan-out) cannot use `session: inherit` or `fork`. Use `session: fresh` or `session: artifacts_only`.

**"I get C012 (missing fallback)"**
If you have `when approved`, you need either `when not approved` or an unconditional edge from the same source. Conditions must be exhaustive.

**"I get C018 (missing model or backend)"**
Every agent and judge needs either `model: "..."` or `backend: "..."`. You can also set the `ITERION_DEFAULT_SUPERVISOR_MODEL` environment variable as a fallback.
