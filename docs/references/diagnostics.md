# Iterion DSL — Validation Diagnostics

All diagnostic codes emitted during compilation (`ir.Compile`) and validation (`ir.Validate`), plus the bundle-consistency codes (`C2xx`) that `iterion validate` reports for a packaged bot. Diagnostics are either **errors** (block execution) or **warnings** (informational).

## Compilation Diagnostics

| Code | Severity | Description | Cause | Fix |
|------|----------|-------------|-------|-----|
| **C001** | error | Unknown node reference | An edge references a node that is not declared | Declare the node or fix the name typo |
| **C002** | error | Unknown schema reference | A node's `input:` or `output:` references an undeclared schema | Declare the schema or fix the name |
| **C003** | error | Unknown prompt reference | A node's `system:` or `user:` references an undeclared prompt | Declare the prompt or fix the name |
| **C004** | error | Bad template reference | A `{{...}}` template expression is malformed | Use `{{vars.X}}`, `{{input.X}}`, `{{outputs.node.field}}`, `{{artifacts.X}}`, `{{attachments.X}}`, `{{loop.name.iteration}}`, or `{{run.id}}` |
| **C005** | error | Duplicate loop definition | Multiple edges share a loop name but disagree on `max_iterations` | Use the same `max_iterations` value or use different loop names |
| **C006** | error | No workflow found | The file has no `workflow` declaration | Add a `workflow name:` block |
| **C007** | error | Multiple workflows | More than one `workflow` block found | V1 supports one workflow per file — remove extras |
| **C008** | error | Missing entry node | The `entry:` node name doesn't match any declared node | Fix the entry name or declare the node |
| **C018** | error/warning | Missing model/backend or LLM interaction requirements | Agents/judges without `model:` or `backend:` are errors only when no default supervisor model and no auto-detectable runtime credentials are available. `mode: llm` routers without either value produce a warning and use the built-in runtime default. Human nodes using `interaction: llm` or `interaction: llm_or_human` must set `model:` or `interaction_model:` and must declare `output:`. | Add `model: "..."`, `backend: "..."`, or configure detectable credentials/defaults for agents/judges; set explicit model/backend for LLM routers when you do not want runtime defaulting; for LLM-backed human nodes add the interaction model and output schema. |
| **C024** | error | Duplicate MCP server | A `mcp_server` name is declared more than once | Use unique names for each MCP server |
| **C025** | error | Invalid MCP server config | MCP server misconfigured (e.g., stdio without command, http/sse without url) | Match properties to transport type: stdio needs `command`; http and sse need `url` and must not set `command` or `args` |
| **C030** | warning | Codex backend discouraged | A node uses `backend: "codex"` | Codex is still supported but has limitations (cannot configure tool set, fills its own context window, weaker integration). Prefer `backend: "claude_code"` for tool-using agents or `claw` (default) with an OpenAI model (`model: "openai/gpt-5.4-mini"`) for judges/reviewers. |
| **C039** | error | Compute node has no expressions | A `compute` node was declared without any `expr: key: "<expression>"` entries | Add at least one expression mapping an output schema field to an expression — or remove the node |
| **C040** | error | Expression failed to parse | An expression in a `compute` node or in a quoted `when "..."` clause isn't valid | Check operators, parentheses, namespace prefixes (`vars / input / outputs / artifacts / loop / run`), and built-in calls (`length`, `concat`, `unique`, `contains`, `join`, `if`) |
| **C041** | error | Duplicate node id | Two declarations share the same node name across agents/judges/routers/humans/tools/computes | Rename one — node ids are a single global namespace |
| **C042** | error | Reserved node name | A user node is named `done` or `fail` (those are reserved terminal targets) | Pick a different node name |
| **C044** | error | Invalid sandbox mode | A node or workflow's `sandbox:` mode is outside the accepted set (`""`, `none`, `auto`, `inline`); or inline mode is missing an image/build or sets both | Set `sandbox:` to `auto`, `none`, `inline`, or omit it. Block-form sandbox config with `image:`, `build:`, `env:`, `mounts:`, or `network:` compiles as inline mode unless `mode:` is specified; inline requires exactly one of `image:` or `build:`. |
| **C045** | error | Sandbox auto without config | Reserved diagnostic code; not currently emitted by compile/validation. Normal CLI/runtime auto mode supplies a default `iterion-sandbox-slim:<version>` fallback when no `.devcontainer/devcontainer.json` is present | No compile-time action. If an embedder disables the default image and runtime reports a missing devcontainer, add `.devcontainer/devcontainer.json`, provide a default image, or use inline `sandbox:` with `image:`/`build:` (see [docs/sandbox.md](../sandbox.md)). |
| **C046** | error | Invalid budget cost | `budget.max_cost_usd` is negative, NaN, or infinity | Use a non-negative finite USD amount, or omit the field to disable the cost cap. |

## Validation Diagnostics

| Code | Severity | Description | Cause | Fix |
|------|----------|-------------|-------|-----|
| **C009** | error | Session at convergence point | A node with `await:` (or multiple incoming sources) uses `session: inherit` or `session: fork` | Change to `session: fresh` or `session: artifacts_only` |
| **C010** | error | Multiple unconditional edges | A non-router node has more than one unconditional outgoing edge | Keep only one default edge, or use a router for fan-out |
| **C011** | error | Ambiguous conditions | Same condition field appears twice with same polarity from the same source | Remove the duplicate edge or use different conditions |
| **C012** | error | Missing fallback | A node has conditional edges but no unconditional fallback and conditions aren't exhaustive | Add `when not X` to complement `when X`, or add an unconditional edge |
| **C013** | error | Condition field not boolean | A `when` clause references a field that isn't `bool` in the source output schema | Change the schema field to `bool` |
| **C014** | error | Condition field not found | A `when` clause references a field that doesn't exist in the source output schema | Add the field to the schema or fix the field name |
| **C015** | error | Else without conditional sibling | An `else` edge has no `when` sibling from the same source — a fallback with nothing to fall back from is just an unconditional edge wearing a misleading keyword | Add the conditional sibling(s), or use a plain `src -> dst` edge |
| **C016** | error | Unreachable node | A declared node cannot be reached from the workflow's `entry:` node | Add edges to reach the node, or remove the unused declaration |
| **C017** | error | History ref not in loop | `{{outputs.node.history}}` is used but the referenced node is not part of any declared loop | Add a loop declaration (`as loop_name(N)`) to the edge cycle, or remove the `.history` reference |
| **C019** | error | Undeclared cycle | A cycle (back-edge) exists without any loop declaration on its edges | Add `as loop_name(N)` to the back-edge to bound the cycle |
| **C020** | error | Round-robin too few edges | A `round_robin` router has fewer than 2 unconditional outgoing edges | Add at least 2 outgoing edges for alternation |
| **C021** | error | LLM router too few edges | An `llm` router has fewer than 2 outgoing edges | Add at least 2 outgoing edges for the LLM to choose from |
| **C022** | error | LLM router edge has condition | An edge from an `llm` router has a `when` clause | Remove the `when` clause — LLM routers select targets directly |
| **C023** | error | LLM-only property on non-LLM router | Properties `model`, `backend`, `system`, `user`, `multi`, or `reasoning_effort` are set on a router that isn't `mode: llm` | Remove these properties or change the mode to `llm` |
| **C026** | error | Invalid loop iterations | A loop's `max_iterations` is less than 1 | Set `max_iterations` to at least 1 |
| **C027** | error | Invalid reasoning effort | `reasoning_effort` has a value other than `low`, `medium`, `high`, `xhigh`, `max`, `ultracode` | Use one of the six valid values |
| **C028** | error | Duplicate with-mapping key | The same `with` key appears on multiple non-conditional edges to the same target | Use unique keys, or make edges conditional/convergent |
| **C029** | error | Unknown outputs node reference | A `{{outputs.<node>...}}` template targets a node not declared anywhere in the file | Declare the node or fix the typo |
| **C031** | error | outputs ref field not in output schema | `{{outputs.<node>.<field>}}` references a field absent from that node's `output:` schema | Reference an existing field, or add the field to the schema |
| **C032** | warning | outputs ref on schemaless node | `{{outputs.<node>.<field>}}` targets a node that has no `output:` schema, so the field cannot be verified | Add an `output:` schema to the source node, or drop the field access |
| **C033** | error | Undeclared variable | `{{vars.X}}` (or `vars.X` inside an expression) targets a variable not declared in the file-level or workflow-level `vars:` block | Declare the variable, or fix the name |
| **C034** | error | input ref field not in input schema | `{{input.<field>}}` references a field absent from the consuming node's `input:` schema | Reference an existing field, or add it to the schema |
| **C035** | error | Unknown artifact | `{{artifacts.X}}` targets an artifact never produced via `publish:` | Add `publish: <name>` on a prior node, or fix the artifact name |
| **C036** | error | Reference to non-reachable node | `{{outputs.<node>...}}` targets a node not reachable from the entry before the consumer | Reorder the graph or wire an edge so the producer runs first |
| **C037** | warning | Node max_tokens exceeds workflow budget | A node-level `max_tokens` is greater than the workflow's `budget.max_tokens` | Lower the node cap, or raise the workflow budget |
| **C038** | error | Unsupported MCP auth type | `mcp_server.auth.type` is something other than `oauth2` (the only wired type) | Drop the `auth:` block, or change `type` to `oauth2` |
| **C043** | error | Invalid compaction values | `compaction.threshold` is outside `(0, 1]` or `compaction.preserve_recent` is `< 1` | Use a fraction like `0.85` for `threshold` and an integer `>= 1` for `preserve_recent`; omit either to inherit the default |
| **C047** | warning | Memory enabled on unsupported backend | A node sets `memory: enabled: true` but its resolved backend doesn't read the field — only the `claw` backend wires memory tools today. The check is informational; the run still proceeds | Drop the `memory:` block, or switch the node to `backend: claw` (or another backend that has memory wired). |
| **C048** | error | Memory missing scope | A node sets `memory: enabled: true` without a non-empty `scope: <name>` — the runtime needs the scope to locate `~/.iterion/projects/<key>/memory/<scope>/` | Add `scope: <name>` to the node's `memory:` block (the scope becomes the directory the `memory_read` / `memory_write` / `memory_list` tools operate against). |
| **C049** | warning | Artifact labels without publish | A node sets `artifact_labels:` but has no `publish:`, so the labels have no artifact to attach to | Add `publish: <name>`, or remove the `artifact_labels:` (judge nodes never publish, so their labels are dropped at compile time) |
| **C050** | error | Duplicate attachment | An attachment name is declared more than once across file-level and workflow-level `attachments:` blocks | Rename the duplicate, or merge the definitions |
| **C051** | error | Attachment / var name collision | An attachment name collides with a declared `vars:` entry | Rename one of them — attachments and vars share a single template namespace |
| **C052** | error | Invalid attachment MIME | An `accept_mime:` entry is not in `type/subtype` form (e.g. `image/png`, `application/pdf`) | Use `type/subtype` MIME values, optionally with `*` subtype wildcards |
| **C053** | error | Unknown attachment reference | `{{attachments.X}}` references an attachment that is not declared in a file-level or workflow-level `attachments:` block | Declare the attachment, or fix the name |
| **C054** | error | Unknown attachment sub-field | `{{attachments.<name>.<subfield>}}` uses a sub-field the runtime does not expose | Drop the sub-field or pick a supported one (`path`, `url`, `mime`, `size`, `sha256`) |
| **C055** | error | Bad prompt include | A prompt `{{include "..."}}` marker could not be resolved: the file is missing, is a directory, exceeds the 256 KiB cap, uses an absolute path, or escapes the `.bot` directory | Point the include at an existing file inside the `.bot`'s directory subtree, below the size cap |
| **C060** | error | Playwright MCP server requires browser-capable sandbox image | An MCP server with the Playwright transport is configured but the workflow's sandbox image is not browser-capable | Use `ghcr.io/socialgouv/iterion-sandbox-browser` (or another browser-capable image whose name matches the validator predicate, such as one containing `sandbox-browser` or `sandbox-full-browser`), or remove the Playwright MCP server |
| **C070** | error | Preset references unknown variable | A `presets:` entry sets a key that does not match any name in `vars:` | Add the variable to `vars:`, or remove/rename the preset key |
| **C071** | error | Preset value type mismatch | A `presets:` value's type (string/int/bool/list) does not match the declared `vars:` type | Cast the value to the declared type, or change the var's type |
| **C072** | error | Duplicate preset name | The same preset name appears more than once in the `presets:` block | Rename or merge the duplicate preset |
| **C080** | warning | Unknown capability | A `capabilities:` entry isn't in the built-in registry (currently: `board.read`, `board.create`, `board.move`, `board.assign`, `board.label`, `board.close`, `board.comment`, `watch.subscribe`, `watch.unsubscribe`) | Either fix the typo or accept the warning — unknown caps still propagate to the executor (the registry is open for extension) |
| **C081** | error | Malformed capability | A `capabilities:` entry doesn't match the shape `domain` or `domain.action` (lowercase letters/digits/underscores) | Use the lowercase `domain.action` form, e.g. `board.create` |
| **C082** | warning | Board capability inside sandbox | A node grants a `board.*` capability while running under a sandbox — the stdio `__mcp-board` transport is unavailable, the runtime falls back to the HTTP transport on the iterion server | No action required if the iterion HTTP server is reachable from the sandbox; otherwise drop the capability or disable the sandbox for that node |
| **C083** | warning | Unknown cursor reference | An agent/judge `cursors:` setting references a cursor name not declared at workflow scope | Declare it with `cursor <name>:` or drop the setting — see [docs/cursors.md](../cursors.md) |
| **C084** | error | Invalid cursor value | A cursor invocation value is not in the enum, falls outside `[0, 1]`, or doesn't match any band. `${VAR}` values defer to runtime | Use a declared enum value or a numeric in range; for env-driven values, ensure the substituted result is valid |
| **C085** | error | Malformed cursor declaration | A `cursor <name>:` block declares neither `values:` nor `bands:`, declares both, has overlapping bands, or has a range outside `[0, 1]` | Pick exactly one form (enum or numeric); ensure bands cover disjoint sub-ranges of `[0, 1]` |
| **C086** | error | Duplicate cursor name | The same `cursor <name>:` declaration appears twice in one source | Rename one of them, or merge their `values:` / `bands:` entries |
| **C087** | warning | Unknown provider | A `provider:` chain token is outside the known provider set | Fix the provider name, or accept the warning for a newly-added provider |
| **C088** | warning | Provider chain ignored | A multi-element `provider:` chain is set on a backend that ignores the hint (`claw`/`codex`) | Drop the extra chain elements, or switch to a backend that honours provider hints |
| **C089** | warning | Ultracode model gate | `reasoning_effort: ultracode` on a model other than `claude-opus-4-8` — the orchestration half is 4.8-only, so it degrades to plain `xhigh` | Use `model: "claude-opus-4-8"` for full ultracode, or accept the `xhigh` degrade |
| **C090** | error | Duplicate secret | A secret name is declared more than once in the `secrets:` block | Rename or merge the duplicate |
| **C091** | error | Secret / var name collision | A secret name collides with a declared `vars:` entry | Rename one — secrets and vars share a template namespace |
| **C092** | error | Invalid secret host | A secret's egress host scoping (Layer 2 `hosts:`) is ill-formed | Use valid host entries (hostnames / domains) |
| **C093** | error | Unknown secret reference | `{{secrets.X}}` references a secret not declared in the `secrets:` block | Declare the secret, or fix the name |
| **C094** | error | Malformed file secret | An `as: file` secret declaration is malformed | Provide a valid `value:`/`env:` and file-mount form |
| **C095** | error | Unsupported secret sub-field | `{{secrets.X.<subfield>}}` uses a sub-field the runtime does not expose | Drop the sub-field, or reference `{{secrets.X}}` directly |
| **C097** | error | Unbounded loop without fuel | An `as name(unbounded)` loop has no fuel ceiling (neither a per-loop `unbounded <N>` nor a workflow `budget.max_iterations`) — the "no silent infinity" invariant | Add a per-loop fuel (`as name(unbounded 200)`) or a workflow `budget.max_iterations` |
| **C098** | warning | Unbounded loop without exit | An `unbounded` loop's body has no edge leaving the loop — only fuel/liveness can stop it | Add a `when`-exit (convergence condition) so the loop terminates by its own logic |
| **C100** | error | Review without worktree | `interaction: review` without `worktree: auto` — there is nothing to merge | Add `worktree: auto`, or drop the review interaction |
| **C101** | warning | Review URL unknown ref | `review_url` references an output node that does not exist | Fix the node reference, or remove `review_url` |
| **C102** | error | Invalid compress value | `compress:` is not one of `on`, `off`, `ultra` | Use `on`, `off`, or `ultra` |
| **C103** | error | Invalid policy | A Verified Action tool node's `policy:` is not one of `required`, `recover`, `best_effort` (ADR-044) | Use a known policy value |
| **C104** | error | Recovery without postcondition | A tool node configures `recovery:` (or `policy: recover`) without a `postcondition:` — the deterministic truth oracle that makes adaptive recovery safe | Add a `postcondition:`, or drop the recovery |
| **C105** | error | Recovery on a gate | Recovery rungs are attached to a GATE (a node where `recipe == postcondition`) | Remove the `recovery:` block — gates stay deterministic; never attach LLM recovery to a gate |
| **C106** | warning | Recovery without recover policy | `recovery:` bounds are present but `policy:` is not `recover` — dead config | Set `policy: recover`, or remove the recovery bounds |
| **C107** | warning | Expression operand type mismatch | A comparison inside a `compute` or quoted `when "..."` expression has statically-known operands of incompatible type classes (e.g. `string[] == int`, `count < "x"`) | Compare compatible types. Inference is conservative: `json` (= any) fields, vars, and unresolved refs bail to "no opinion" and are never flagged |
| **C108** | warning | when-expression not boolean | A quoted `when "<expr>"` is a bare numeric value (e.g. `when "count"`) rather than a boolean — int/float coerce to truthy, which is rarely the author's intent | Use a comparison such as `when "count > 0"`. Bare `bool`, `string[]`, and `string` values ride the documented truthy idiom and are not flagged |
| **C109** | error | Var default type mismatch | A `vars:` entry's default literal type doesn't match its declared type (e.g. `count: int = "x"`) | Fix the default to match the type. `int`→`float` widening is allowed (`ratio: float = 5`); `json` and `string[]` accept loose literals and are never flagged |
| **C110** | error | Invalid permission | `permission:` is not one of `off`, `ask`, `deny` | Use `off`, `ask`, or `deny` |
| **C111** | warning | Permission rules without gate | `allow`/`ask`/`deny` rule lists are declared but the resolved permission mode is `""`/`off`, so they never apply | Set `permission: ask` or `deny`, or remove the rule lists |
| **C112** | warning | Tool-node permission inert | `permission:` on a tool node — parsed but not enforced (a tool node runs a fixed command, not an agent) | Remove the `permission:` from the tool node; gate the agent nodes instead |
| **C113** | error | fan_out_each without over | A `fan_out_each` router has no `over:` array source | Add `over: "{{...array...}}"` to the router |
| **C114** | error | fan_out_each property on non-fan_out_each | `over`/`as`/`key`/`depends_on` set on a router that isn't `mode: fan_out_each` | Remove the property, or change the router mode |
| **C115** | error | fan_out_each edge count | A `fan_out_each` router must have exactly one outgoing template edge | Keep a single template edge from the router |
| **C116** | error | Use references unknown group | A `use ... as` statement references a `group` that is not declared | Declare the group, or fix the name |
| **C117** | error | Use param mismatch | A `use` provides an unknown param, or omits a declared one | Match the group's declared params exactly |
| **C118** | error | foreach conflicts with loop | An edge combines `as foreach` with `as <loop>` (mutually exclusive) | Use one iteration form per edge |
| **C119** | error | subbot without source | A `subbot` node has no `source:` child `.bot` | Add `source: <path>.bot` to the subbot |
| **C120** | warning | Index on scalar | A subscript `[...]` is applied to a statically-scalar value (string/bool/int/float), which is not indexable | Index an array/map, or drop the subscript |
| **C121** | error | Enum literal never matches | A `when "field == 'literal'"` / `!=` comparison (or a `compute` expression) compares an enum-typed field against a literal that is not one of its `enum:` values — the comparison can never match, so it is almost always a typo | Use a declared enum value, or fix the field's `enum:` set. `json` fields and unresolved refs are never flagged |
| **C122** | error | Invalid node timeout | An agent/judge `timeout:` is not a positive Go duration (after `${VAR:-default}` expansion) | Use a positive Go duration string, e.g. `timeout: "20m"` or `"1200s"` |
| **C123** | error | Multiple else edges | A source node has more than one `else` edge — two fallbacks firing on the same miss is the C010 ambiguity under a new name | Keep exactly one `else` per source |
| **C124** | error | Else alongside unconditional | A source node has both an `else` edge and a bare unconditional edge — two competing defaults | `else` IS the fallback: remove the bare unconditional edge, or drop the `else` keyword |
| **C125** | error | Var enum on non-string type | A `vars:` entry declares an `[enum: ...]` constraint on a non-string type (e.g. `count: int [enum: "a"]`) — enums constrain string values only | Declare the var as `string`, or drop the constraint |
| **C126** | error | Var default not in enum | An enum-constrained var's default is not one of the declared values (e.g. `mode: string [enum: "a", "b"] = "c"`). Launch-provided values get the same check at run start: the engine rejects any `--var` / HTTP / dispatcher / preset value outside the enum set | Use one of the enum values as the default, or extend the list |
| **C127** | warning | Duplicate var enum value | The same value appears more than once in a var's `[enum: ...]` list — the duplicate is dropped (first occurrence kept, order preserved) | Remove the duplicate value |
| **C128** | warning | Sandbox opt-out | The workflow (or a node) explicitly declares `sandbox: none` — every tool and shell command runs directly on the host/runner with its credentials and filesystem, while sandboxing is the engine default | Remove the `sandbox: none` block to run sandboxed; keep the opt-out only if the flow genuinely needs the host environment |
| **C129** | error | `file` field outside a human pause | A schema used as the `output:` of a node that never pauses for an operator declares a `file`-typed field — any non-human node, or a `human` node with `interaction: llm` (auto-answered by a model) or `interaction: review` (output is the engine-built verdict). Only an operator upload at a real pause produces one | Move the `file` field to a human node's `output:` schema with `interaction: human` (or `llm_or_human`, which can escalate to a pause), or use `string` if the node computes a path itself |
| **C130** | error | Reserved answer key | A `human` node's `output:` schema declares a key the engine writes on resume (`_attachments`, which carries the gate's ad-hoc operator uploads) — the operator's answer would be silently replaced | Rename the field. The reservation applies only to human gates; elsewhere the name is an ordinary field |
| **C131** | error | Invalid auto_memory value | `auto_memory:` (workflow or agent/judge node) is not one of `on`, `off` — a typo would silently read as "inherit", i.e. off | Use `on` or `off`, or drop the field to inherit |
| **C132** | warning | auto_memory on an unsupported backend | An agent/judge node explicitly declares `auto_memory: on` but its `backend:` does not consume MEMORY.md (`claude_code`, `claw` and `pi` do) | Switch to one of those backends, or drop the `auto_memory:` field |
| **C170** | error | Invalid memory visibility | `memory: visibility:` has an unknown value | Use a known visibility (`bot`/`project`/`cross_project`/`user`/`org`/`global`) |
| **C171** | error | Memory visibility conflict | `memory: visibility:` is combined with the legacy `project_root:` | Use `visibility:` alone — drop the legacy `project_root:` |
| **C172** | warning | Malformed provider step | A `provider:` chain element of the `provider:model` form has an empty provider or model part | Provide both parts, e.g. `anthropic:claude-sonnet-4-6` |
| **C174** | warning | Command ignored | A per-node `command:` CLI-binary override is set on a backend that ignores it (`claw`/`codex`) — only `claude_code` honors it | Switch to `backend: "claude_code"`, or drop the `command:` |
| **C190** | warning | Supervisor watches non-agent | A `supervisor` `watches:` a node id that isn't an agent node | Watch an agent node, or fix the node id |
| **C191** | warning | Malformed supervisor | A `supervisor` declaration is malformed (e.g. a bad cooldown duration) | Use a valid Go duration for cooldown |
| **C192** | error | Duplicate supervisor | The same `supervisor <name>:` is declared twice | Rename or merge |
| **C193** | warning | Unknown supervisor prompt | A supervisor's `system:` references an undeclared prompt | Declare the prompt, or fix the name |
| **C194** | error | Invalid resource capacity | A `resources.<name>` capacity is ≤ 0 | Use a capacity ≥ 1 |
| **C195** | error | Unknown resource in needs | A `needs:` references a resource not declared in the `resources:` block | Declare the resource, or fix the name |
| **C196** | error | Event node without name | An `emit`/`wait` node has no `event:` name (ADR-051) | Add `event: "<name>"` |
| **C197** | error | Wait without timeout | A `wait` node has no (or an invalid/non-positive) `timeout:` — the mandatory bound, the "no silent infinity" invariant for events | Add `timeout: "30s"` (a positive Go duration) |
| **C198** | warning | Dangling event | A `wait` awaits an event no `emit` produces (it can only ever time out), or an `emit` produces an event no `wait` consumes (dead event) | Pair each `wait` with an `emit` of the same event name (a `wait` on an externally-sourced event is expected to warn until external-event support lands) |
| **C199** | warning | Invalid skill reference | A `skills:` entry (on a node or the workflow) is not a valid skill name — a single path segment of letters/digits/`.`/`-`/`_`, not starting with a dot (ADR-059). Existence in the library is resolved at run time, not here, so an unknown-but-well-formed name does not warn | Fix the name; quote kebab-case names (`skills: ["changelog-writer"]`) |
| **C240** | error | Async interaction on human node | `interaction: async` is set on a `human` node — async questions are posted by agent/judge nodes; a human node IS the blocking question | Move `interaction: async` to the asking agent/judge and use an `await_answers` node as the sync point |
| **C241** | error | await_answers without timeout | An `await_answers` node has no (or an invalid/non-positive) `timeout:` — the mandatory bound, the "no silent infinity" invariant | Add `timeout: "30m"` (a positive Go duration) |
| **C242** | warning | await_answers with dead from | An `await_answers` `from:` names a node that is missing or not an `interaction: async` agent/judge — no async question can originate there, so the await can only ever time out | Fix the `from:` reference, or set `interaction: async` on the referenced node |

> **Note on `C103`–`C106` (Verified Actions, ADR-044):** these four codes are
> the adaptive-recovery firewall on deterministic ACTION tool nodes. The
> enum-literal type check that earlier releases emitted as `C103` is now
> `C121` — a `C103` always means *invalid policy*. See
> [docs/adr/044-adaptive-recovery-for-deterministic-action-nodes.md](../adr/044-adaptive-recovery-for-deterministic-action-nodes.md).

> **Historical code-reuse note:** earlier releases reused `C030` for
> two cases. `C029` was introduced for the validator-side
> *unknown outputs node reference* error; `C030` now only flags the
> compile-time *Codex backend discouraged* warning. If an older log
> shows `C030` on an `outputs.<unknown>` reference, treat it as the
> modern `C029`.

## Bundle Consistency Diagnostics (manifest ↔ workflow)

These `C2xx` diagnostics come from `pkg/bundlelint`, emitted when `iterion validate`
runs on a **bundle** (a `.botz` archive or a directory with `manifest.yaml` +
`main.bot`). They cross-check the manifest against the *compiled workflow* —
something neither the manifest parser (`pkg/bundle`) nor the DSL compiler
(`pkg/dsl/ir`) can do alone, because each only sees one side. They are reported
under a separate `bundle_diagnostics` list in `--json` output. All are warnings
except **C230**; warnings are surfaced but do not fail validation.

| Code | Severity | Description | Cause | Fix |
|------|----------|-------------|-------|-----|
| **C200** | warning | dispatch_vars key not a workflow var | A `manifest.dispatch_vars` key names no variable in the workflow `vars:` block | Declare the var in `main.bot`, or fix/remove the manifest key — an undeclared key is silently dropped at dispatch time |
| **C201** | warning | context_vars key not a workflow var | An `invocations[].context_vars` key names no workflow var | Same as C200 |
| **C202** | warning | schedule.default_vars key not a workflow var | An `invocations[].schedule.default_vars` key names no workflow var | Same as C200 |
| **C203** | warning | launch_vars key not a workflow var | A `forge.webhook.launch_vars` key names no workflow var | Same as C200 |
| **C204** | warning | args_var not a workflow var | An `invocations[].args_var` names no workflow var, so the trigger's free-text payload is dropped | Declare the var, or fix the name |
| **C210** | warning | forge secret not declared | The forge secret the bot expects (`forge.secret`, default `forge_token`) has no matching declaration in the `main.bot` `secrets:` block | Declare `secrets: { <name>: { as: file, optional: true } }`, or point `forge.secret` at an existing secret. Only checked when the bot is forge-triggerable (has `forge.events` or a `kind: forge` invocation) |
| **C211** | warning | forge secret not a file mount | The forge secret is declared but not `as: file` — managed forge tokens are bound as a file mount | Set `as: file` on the secret declaration |
| **C220** | warning | manifest capability granted by no node | A `manifest.capabilities` entry is granted by no workflow-level or node-level `capabilities:` list | Add it to a node's `capabilities:`, or drop it from the manifest (it is documentation-only otherwise) |
| **C221** | warning | frontmatter capabilities override manifest | The `main.bot` `## ---` frontmatter declares `capabilities:` that differ from and silently override the manifest's | Keep one source of truth — drop the frontmatter list or align the two |
| **C230** | error | per-bot memory name mismatch | A node uses `memory: visibility: bot`, but the manifest name, workflow name, and bundle dir name are not all identical — so the bot's memory tree splits across CLI (workflow name) and dispatcher (bundle name) launches | Make all three identical |
| **C231** | warning | skill has no `name:` | A `skills/*.md` file has no `name:` frontmatter, so it is undiscoverable by name once mirrored into `.claude/skills/` | Add `name: <kebab-case-id>` to the skill frontmatter |
| **C232** | warning | skill has no `description:` | A `skills/*.md` file has no `description:` frontmatter, so the router (Nexie) has no signal for when to select it | Add a `description:` saying what the skill is for and when it applies |
| **C233** | warning | skill `description:` too terse | A skill `description:` is present but too short to route on (e.g. "Security stuff") | Describe what the skill does and the situation it applies to. Routability only — no phrasing template is imposed |
| **C234** | warning | duplicate skill name | Two `skills/*.md` files declare the same `name:`, so one silently clobbers the other when mirrored | Give each skill a unique `name:` |

The skill-authoring checks (**C231–C234**) guard *routability* — that a skill
can be discovered and chosen by the router — not prose style. They impose no
phrasing template and are all warnings, so a skill gap never fails validation.

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
For agents and judges, add `model: "..."` or `backend: "..."`, set `ITERION_DEFAULT_SUPERVISOR_MODEL`, or configure detectable backend credentials. For `mode: llm` routers, either set an explicit `model:`/`backend:` or accept the warning and runtime default. For human nodes with `interaction: llm` or `interaction: llm_or_human`, add `model:` or `interaction_model:` and declare an `output:` schema.
