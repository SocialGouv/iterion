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
| **C018** | error | Missing model or delegate | An agent/judge has neither `model:` nor `delegate:`, and `ITERION_DEFAULT_SUPERVISOR_MODEL` is not set | Add `model: "..."` or `delegate: "..."` to the node |
| **C024** | error | Duplicate MCP server | A `mcp_server` name is declared more than once | Use unique names for each MCP server |
| **C025** | error | Invalid MCP server config | MCP server misconfigured (e.g., stdio without command, http without url) | Match properties to transport type: stdio needs `command`, http needs `url` |
| **C029** | warning | Interaction on non-delegate node | `interaction` is set on an agent/judge without `delegate:` | Interaction forwarding only works with delegation backends — add `delegate:` or remove `interaction:` |

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
| **C023** | error | LLM-only property on non-LLM router | Properties `model`, `system`, `user`, or `multi` are set on a router that isn't `mode: llm` | Remove these properties or change the mode to `llm` |
| **C024** | error | Invalid reasoning effort | `reasoning_effort` has a value other than `low`, `medium`, `high`, `extra_high` | Use one of the four valid values |
| **C026** | error | Invalid loop iterations | A loop's `max_iterations` is less than 1 | Set `max_iterations` to at least 1 |
| **C028** | error | Duplicate with-mapping key | The same `with` key appears on multiple non-conditional edges to the same target | Use unique keys, or make edges conditional/convergent |

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

**"I get C018 (missing model or delegate)"**
Every agent and judge needs either `model: "..."` or `delegate: "..."`. You can also set the `ITERION_DEFAULT_SUPERVISOR_MODEL` environment variable as a fallback.
