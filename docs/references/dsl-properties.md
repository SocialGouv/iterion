# Iterion `.bot` property reference

Every declaration kind, node kind and block of the DSL, with the properties
each accepts, the shape of every value and one line on what it means. This
page is **generated** from the parser's property registry
([`pkg/dsl/spec`](../../pkg/dsl/spec/spec.go)) by `iterion dsl spec --write`
(`task dsl:gen`): the registry is held to the real parser by a conformance
test in both directions — every property listed here is accepted by the
parser, every property the parser accepts is listed here — and `task
dsl:check` fails when this page no longer matches the registry. The same
registry gives an unknown property its remedy (`iterion validate`'s `fix:`
line: the closest accepted name, the block a name belongs to, the kind's own
list) and feeds the property section of the authoring skills.

The readable grammar is [dsl-grammar.md](dsl-grammar.md); the semantics of
each property live in the [DSL guide](../dsl.md). The value forms:

| Form | Written as |
|---|---|
| string | a quoted string: `"…"`, a backtick raw string, or a `\|` block scalar |
| ident | a bare name: a declared prompt, schema or node, or an accepted word |
| string\|ident | either of the two |
| int, number, bool | an unquoted literal (`3`, `0.8`, `true`) |
| one of … | one of the listed bare words |
| ident list, string list, tool list, skill list | an inline `[a, b]` list; tool refs may be dotted (`mcp.server.*`), a quoted element is the literal name |
| map | `{ KEY: "v" }` inline, or an indented `KEY: v` block |
| with { … } | `with { key: "value", … }` |
| block → kind | an indented block whose lines are that kind's properties |

<!-- dsl-spec:begin reference -->
_Generated from the parser's property registry (`pkg/dsl/spec`) by `iterion dsl spec --write`; do not edit by hand. A conformance test holds the registry to the parser in both directions._

### prompt

A named text block, referenced by `system:` / `user:` / `instructions:`; its body is free text with {{…}} references and {{include "file"}} directives. Blank lines in the body are dropped by the lexer; a bare header declares an empty prompt.

A top-level declaration: `prompt <name>:`.

Entries: `indented text lines` — Free text; the first line's indentation is stripped from every line.

### schema

A structured-output shape; a bare header declares an empty schema.

A top-level declaration: `schema <name>:`.

Entries: `field: string | bool | int | float | json | string[] | file [enum: "a", "b"]` — One field per line; `file` is valid only on the output schema of a human node whose interaction collects operator bytes (C129); the enum constraint applies to strings.

### cursor

A prompt-engineering dial: an enum (values:) or a numeric band map (bands:) over [0, 1], each entry carrying a prompt fragment (C083–C086).

A top-level declaration: `cursor <name>:`.

| Property | Value | Meaning |
|---|---|---|
| `description` | string | Free-text description shown by the studio and the reports |
| `values` | block → [cursor.values](#cursorvalues) | Enum form: one `name: "fragment"` per line, order preserved |
| `bands` | block → [cursor.bands](#cursorbands) | Numeric form: one `"lo..hi": "fragment"` per line |

### cursor.values

The enum values of a cursor.

A block opened by `values:` inside `cursor`.

Entries: `name: "prompt fragment"` — Order is the position a numeric invocation snaps to.

### cursor.bands

The numeric bands of a cursor.

A block opened by `bands:` inside `cursor`.

Entries: `"lo..hi": "prompt fragment"` — The range is parsed by the compiler (C085 when malformed).

### supervisor

A concurrent LLM watcher of agent nodes that enqueues steering messages the watched node reads at its next turn (docs/supervisors.md); run metadata, not a graph node.

A top-level declaration: `supervisor <name>:`.

| Property | Value | Meaning |
|---|---|---|
| `watches` | ident list | Agent nodes the supervisor is armed for |
| `model` | string | Model id the backend serves, e.g. "anthropic/claude-opus-5"; empty takes the backend's default |
| `system` | ident | Prompt declaration used as the system prompt |
| `cooldown` | string | Minimum delay between two evaluations, e.g. "2m" |
| `max_evals` | int | Upper bound on evaluations per run |
| `monitors` | string list | Event patterns armed from the first event (the CLI --monitor grammar) |

### mcp_server

An MCP server the workflow may activate: stdio (command/args) or http/sse (url), optionally OAuth2.

A top-level declaration: `mcp_server <name>:`.

| Property | Value | Meaning |
|---|---|---|
| `transport` | one of `stdio`, `http`, `sse` | How the server is reached |
| `command` | string | stdio: the executable |
| `args` | string list | stdio: its arguments |
| `url` | string | http / sse: the endpoint |
| `auth` | block → [auth](#auth) | OAuth2 authorization-code/PKCE settings |

### auth

OAuth2 settings of an MCP server (only authorization-code/PKCE is wired).

A block opened by `auth:` inside `mcp_server`.

| Property | Value | Meaning |
|---|---|---|
| `type` | string | "oauth2" |
| `auth_url` | string | Authorization endpoint |
| `token_url` | string | Token endpoint |
| `revoke_url` | string | Revocation endpoint (optional) |
| `client_id` | string | OAuth client id |
| `scopes` | string list | Scopes requested |

### group

A reusable node cluster with parameters, whose body holds agent/judge/router/human/tool/compute declarations and edges; instantiated by `use`, expanded at compile time (C141 warns on a use of an empty group). Prompts read `{{params.name}}`.

A top-level declaration: `group <name>(<param>, …):`.

Entries: `node declarations and edges (src -> dst)` — Nodes are addressed as <prefix>.<node> once instantiated.

### use

One instance of a group; the with map binds its parameters. No body.

A top-level declaration: `use <group> as <prefix> [with { <param>: "value", … }]`.

Entries: `use g as p with { param: "value" }` — A single line, no indented body.

### vars

Typed run parameters, overridable with --var and presets.

A block opened by `vars:` inside the top level, `workflow`.

Entries: `name: type [enum: "a", "b"] [= default]` — type is string, bool, int, float, json or string[]; the enum constraint applies to strings; a json/string[] default is a quoted JSON text.

### presets

Named bundles of var values selected with --recipe / --preset.

A block opened by `presets:` inside the top level.

Entries: `name: (indented) var: literal` — Each entry is a preset name with one `var: literal` line per value.

### attachments

Operator-supplied files and images the run receives.

A block opened by `attachments:` inside the top level, `workflow`.

Entries: `name: file | image` — An entry may open an indented sub-block; an entry's sub-block is a [attachment](#attachment).

### attachment

The optional sub-block of one attachment.

An entry opened by `attachments:` inside `attachments`.

| Property | Value | Meaning |
|---|---|---|
| `description` | string | Free-text description shown by the studio and the reports |
| `accept_mime` | string list | MIME types accepted |
| `required` | bool | The run cannot start without it |

### secrets

Secrets the run resolves by name from the team's or the local store (docs/secrets.md).

A block opened by `secrets:` inside the top level.

Entries: `name: "value"` — The value on the header line is optional: a bare `name:` (with or without a sub-block) resolves the stored secret by name; an entry's sub-block is a [secret](#secret).

### secret

The optional sub-block of one secret.

An entry opened by `secrets:` inside `secrets`.

| Property | Value | Meaning |
|---|---|---|
| `value` | string | Inline value — prefer the stored secret, resolved by name |
| `as` | ident — `value`, `file` | How the secret is materialised: value (env/template) or file |
| `mount_path` | string | as: file — the path inside the sandbox |
| `env` | string\|ident | Environment variable that receives the value |
| `optional` | bool | A missing secret does not fail the launch |
| `hosts` | string list | Hosts the secret may be sent to |
| `description` | string | Free-text description shown by the studio and the reports |

### agent

An LLM node with tools, structured I/O and any backend.

A node: `agent <name>:` at the top level or inside a `group`.

| Property | Value | Meaning |
|---|---|---|
| `description` | string | Free-text description shown by the studio and the reports |
| `model` | string | Model id the backend serves, e.g. "anthropic/claude-opus-5"; empty takes the backend's default |
| `backend` | string | Execution backend: claw, claude_code, codex, pi, kimi or grok |
| `provider` | string | Provider hint for credential resolution, e.g. "anthropic" |
| `command` | string | Executable that drives a CLI backend, overriding its default binary |
| `input` | ident | Schema the node's input is validated against |
| `output` | ident | Schema the node's structured output must match |
| `publish` | ident | Artifact name the output is published under (read back as {{artifacts.<name>}}) |
| `artifact_labels` | tool list | Labels stamped on the published artifact; a quoted element is the literal label |
| `system` | ident | Prompt declaration used as the system prompt |
| `user` | ident | Prompt declaration used as the user message |
| `session` | one of `fresh`, `inherit`, `inherit_if_available`, `fork`, `artifacts_only`, `persist` | How the node's LLM session relates to the previous node's |
| `tools` | tool list | Tools the node may call; restricts claw (C135 on a name it lacks), inert on a CLI backend |
| `tool_policy` | tool list | Tool-policy entries applied on top of tools |
| `capabilities` | tool list | Board capabilities opened to the node: board.create, board.move, board.read, … (C080/C081) |
| `skills` | skill list | Skill-library skills mirrored into the run's .claude/skills |
| `tool_max_steps` | int | Upper bound on tool-call rounds in one execution |
| `max_tokens` | int | Output-token cap per call |
| `reasoning_effort` | one of `low`, `medium`, `high`, `xhigh`, `max`, `ultracode` | Reasoning effort; ultracode is xhigh plus multi-agent orchestration, reliable on Opus 4.8 and the Claude 5 family (Opus 5, Fable 5.1) only (C089 warns elsewhere); a quoted string is env-substituted at runtime |
| `timeout` | string | Duration the node may run, e.g. "20m" |
| `readonly` | bool | Declares the node mutates no workspace file, so it may run beside another branch |
| `full_access` | bool | Grants the backend its full tool access |
| `images` | string list | Image paths sent with the prompt |
| `interaction` | one of `none`, `human`, `llm`, `llm_or_human`, `review`, `async` | How the node asks the operator (ADR-081) |
| `interaction_prompt` | ident | Prompt the llm interaction mode answers with in the operator's place |
| `interaction_model` | string | Model the llm interaction mode uses |
| `await` | one of `wait_all`, `best_effort` | Convergence rule when several incoming branches reach the node |
| `compress` | ident — `on`, `ultra`, `off` | Command-output compression: on, ultra or off (C102) |
| `auto_memory` | ident — `on`, `off` | The backend's own auto-memory: on or off (C131/C132) |
| `permission` | ident — `off`, `ask`, `deny` | Tool-permission gate: off, ask or deny (C110–C112) |
| `needs` | ident \| ident list | Resource(s) leased from the workflow's resources: block for the node's duration |
| `fallbacks` | block → [fallback](#fallback) | Ordered, NAMED alternative routes taken when the primary fails (ADR-087); a chain with no route is refused |
| `mcp` | block → [mcp](#mcp) | MCP servers active for the node |
| `compaction` | block → [compaction](#compaction) | Context-compaction thresholds of the node's session |
| `memory` | block → [memory](#memory) | iterion's shared-memory tools and scopes for the node |
| `sandbox` | one of `none`, `auto`, or a block → [sandbox](#sandbox) | Sandbox for this scope: a bare mode (none, auto) or an indented block — the inline form, which needs image: or build: (C044) |
| `cursors` | block → [cursors](#cursors) | Prompt-engineering dials activated on the node (docs/cursors.md) |

### judge

An LLM node producing verdicts; same surface as an agent, typically without tools.

A node: `judge <name>:` at the top level or inside a `group`.

| Property | Value | Meaning |
|---|---|---|
| `description` | string | Free-text description shown by the studio and the reports |
| `model` | string | Model id the backend serves, e.g. "anthropic/claude-opus-5"; empty takes the backend's default |
| `backend` | string | Execution backend: claw, claude_code, codex, pi, kimi or grok |
| `provider` | string | Provider hint for credential resolution, e.g. "anthropic" |
| `command` | string | Executable that drives a CLI backend, overriding its default binary |
| `input` | ident | Schema the node's input is validated against |
| `output` | ident | Schema the node's structured output must match |
| `publish` | ident | Artifact name the output is published under (read back as {{artifacts.<name>}}) |
| `artifact_labels` | tool list | Labels stamped on the published artifact; a quoted element is the literal label |
| `system` | ident | Prompt declaration used as the system prompt |
| `user` | ident | Prompt declaration used as the user message |
| `session` | one of `fresh`, `inherit`, `inherit_if_available`, `fork`, `artifacts_only`, `persist` | How the node's LLM session relates to the previous node's |
| `tools` | tool list | Tools the node may call; restricts claw (C135 on a name it lacks), inert on a CLI backend |
| `tool_policy` | tool list | Tool-policy entries applied on top of tools |
| `capabilities` | tool list | Board capabilities opened to the node: board.create, board.move, board.read, … (C080/C081) |
| `skills` | skill list | Skill-library skills mirrored into the run's .claude/skills |
| `tool_max_steps` | int | Upper bound on tool-call rounds in one execution |
| `max_tokens` | int | Output-token cap per call |
| `reasoning_effort` | one of `low`, `medium`, `high`, `xhigh`, `max`, `ultracode` | Reasoning effort; ultracode is xhigh plus multi-agent orchestration, reliable on Opus 4.8 and the Claude 5 family (Opus 5, Fable 5.1) only (C089 warns elsewhere); a quoted string is env-substituted at runtime |
| `timeout` | string | Duration the node may run, e.g. "20m" |
| `readonly` | bool | Declares the node mutates no workspace file, so it may run beside another branch |
| `full_access` | bool | Grants the backend its full tool access |
| `images` | string list | Image paths sent with the prompt |
| `interaction` | one of `none`, `human`, `llm`, `llm_or_human`, `review`, `async` | How the node asks the operator (ADR-081) |
| `interaction_prompt` | ident | Prompt the llm interaction mode answers with in the operator's place |
| `interaction_model` | string | Model the llm interaction mode uses |
| `await` | one of `wait_all`, `best_effort` | Convergence rule when several incoming branches reach the node |
| `compress` | ident — `on`, `ultra`, `off` | Command-output compression: on, ultra or off (C102) |
| `auto_memory` | ident — `on`, `off` | The backend's own auto-memory: on or off (C131/C132) |
| `permission` | ident — `off`, `ask`, `deny` | Tool-permission gate: off, ask or deny (C110–C112) |
| `needs` | ident \| ident list | Resource(s) leased from the workflow's resources: block for the node's duration |
| `fallbacks` | block → [fallback](#fallback) | Ordered, NAMED alternative routes taken when the primary fails (ADR-087); a chain with no route is refused |
| `mcp` | block → [mcp](#mcp) | MCP servers active for the node |
| `compaction` | block → [compaction](#compaction) | Context-compaction thresholds of the node's session |
| `memory` | block → [memory](#memory) | iterion's shared-memory tools and scopes for the node |
| `sandbox` | one of `none`, `auto`, or a block → [sandbox](#sandbox) | Sandbox for this scope: a bare mode (none, auto) or an indented block — the inline form, which needs image: or build: (C044) |
| `cursors` | block → [cursors](#cursors) | Prompt-engineering dials activated on the node (docs/cursors.md) |

### router

A routing node: fan_out_all, fan_out_each, condition, round_robin or llm (docs/routers.md). Never takes await.

A node: `router <name>:` at the top level or inside a `group`.

| Property | Value | Meaning |
|---|---|---|
| `description` | string | Free-text description shown by the studio and the reports |
| `mode` | one of `fan_out_all`, `fan_out_each`, `condition`, `round_robin`, `llm` | Routing mode |
| `model` | string | llm mode only (C023 otherwise): Model id the backend serves, e.g. "anthropic/claude-opus-5"; empty takes the backend's default |
| `backend` | string | llm mode only (C023 otherwise): Execution backend: claw, claude_code, codex, pi, kimi or grok |
| `provider` | string | Provider hint for credential resolution, e.g. "anthropic" |
| `system` | ident | llm mode only (C023 otherwise): Prompt declaration used as the system prompt |
| `user` | ident | llm mode only (C023 otherwise): Prompt declaration used as the user message |
| `multi` | bool | llm mode only (C023 otherwise): the model may select several outgoing edges |
| `reasoning_effort` | one of `low`, `medium`, `high`, `xhigh`, `max`, `ultracode` | llm mode only (C023 otherwise): Reasoning effort; ultracode is xhigh plus multi-agent orchestration, reliable on Opus 4.8 and the Claude 5 family (Opus 5, Fable 5.1) only (C089 warns elsewhere); a quoted string is env-substituted at runtime |
| `over` | string | fan_out_each: expression naming the collection to iterate |
| `as` | ident | fan_out_each: alias each item is bound to ({{each.<as>}}) |
| `key` | ident | fan_out_each: item field that names each branch |
| `depends_on` | ident | fan_out_each: item field naming the branch this one waits for (requires key) |
| `needs` | ident \| ident list | Resource(s) leased from the workflow's resources: block for the node's duration |

### human

A pause point the operator answers (interaction human, the default), an LLM answers (llm / llm_or_human), or a review gate (review).

A node: `human <name>:` at the top level or inside a `group`.

| Property | Value | Meaning |
|---|---|---|
| `description` | string | Free-text description shown by the studio and the reports |
| `input` | ident | Schema the node's input is validated against |
| `output` | ident | Schema the node's structured output must match |
| `publish` | ident | Artifact name the output is published under (read back as {{artifacts.<name>}}) |
| `artifact_labels` | tool list | Labels stamped on the published artifact; a quoted element is the literal label |
| `instructions` | ident | Prompt shown to the operator as the question |
| `system` | ident | Prompt declaration used as the system prompt |
| `model` | string | Model id the backend serves, e.g. "anthropic/claude-opus-5"; empty takes the backend's default |
| `interaction` | one of `none`, `human`, `llm`, `llm_or_human`, `review`, `async` | How the node asks the operator (ADR-081) |
| `interaction_prompt` | ident | Prompt the llm interaction mode answers with in the operator's place |
| `interaction_model` | string | Model the llm interaction mode uses |
| `min_answers` | int | Answers required before the node resumes |
| `await` | one of `wait_all`, `best_effort` | Convergence rule when several incoming branches reach the node |
| `review_url` | string | review: the PR/MR the gate reviews (a {{…}} reference is accepted) |
| `posture` | string\|ident — `human_required`, `agent_verdict_ok` | review: who may merge — human_required (default) or agent_verdict_ok; not validated at compile, another word reads as the default |
| `merge_strategy` | string\|ident — `squash`, `merge` | review: squash (default) or merge; not validated at compile |
| `merge_into` | string\|ident | review: current (default), none or a branch name |
| `max_turns` | int | review: conversation turns before the gate escalates |

### tool

Direct shell execution, no LLM: `command:` runs through bash -c, `script:` through the interpreter `language:` names; with `output:` the command prints schema-shaped JSON on stdout. A Verified Action adds goal + postcondition + policy + recovery (ADR-044).

A node: `tool <name>:` at the top level or inside a `group`.

| Property | Value | Meaning |
|---|---|---|
| `description` | string | Free-text description shown by the studio and the reports |
| `command` | string | Shell command, run through bash -c (exclusive with script) |
| `script` | string | Inline script run by the interpreter language: names |
| `language` | ident — `js`, `node`, `py`, `python`, `python3`, `sh`, `bash` | Interpreter for script: |
| `input` | ident | Schema the node's input is validated against |
| `output` | ident | Schema the node's structured output must match |
| `publish` | ident | Artifact name the output is published under (read back as {{artifacts.<name>}}) |
| `artifact_labels` | tool list | Labels stamped on the published artifact; a quoted element is the literal label |
| `await` | one of `wait_all`, `best_effort` | Convergence rule when several incoming branches reach the node |
| `sandbox` | one of `none`, `auto`, or a block → [sandbox](#sandbox) | Sandbox for this scope: a bare mode (none, auto) or an indented block — the inline form, which needs image: or build: (C044) |
| `compress` | ident — `on`, `ultra`, `off` | Command-output compression: on, ultra or off (C102) |
| `permission` | ident | Parsed for symmetry but NOT enforced on a tool node (C112 warns): the command runs directly, the gate is an agent's |
| `needs` | ident \| ident list | Resource(s) leased from the workflow's resources: block for the node's duration |
| `parallel_safe` | bool | Declares the node safe to run beside a mutating branch |
| `goal` | string | Verified action: what the command is for, in one line |
| `postcondition` | string | Verified action: command whose exit code is the truth oracle at every rung |
| `policy` | ident — `required`, `recover`, `best_effort` | Verified action: required (default), recover or best_effort (C103–C106) |
| `recovery` | block → [recovery](#recovery) | Verified action: the self-heal ladder's bounds |

### recovery

Bounds of a Verified Action's recovery ladder (idempotent-skip → recipe → self-repair → agent → policy).

A block opened by `recovery:` inside `tool`.

| Property | Value | Meaning |
|---|---|---|
| `max_repair_attempts` | int | Self-repair rungs before the agent rung |
| `max_agent_attempts` | int | Agent rungs before the policy decides |
| `model` | string | Model id the backend serves, e.g. "anthropic/claude-opus-5"; empty takes the backend's default |
| `agent_tools` | tool list | Tools the recovery agent may call |

### compute

A deterministic expression node: each expr entry is evaluated by the bounded expression language into an output field.

A node: `compute <name>:` at the top level or inside a `group`.

| Property | Value | Meaning |
|---|---|---|
| `description` | string | Free-text description shown by the studio and the reports |
| `input` | ident | Schema the node's input is validated against |
| `output` | ident | Schema the node's structured output must match |
| `publish` | ident | Artifact name the output is published under (read back as {{artifacts.<name>}}) |
| `artifact_labels` | tool list | Labels stamped on the published artifact; a quoted element is the literal label |
| `await` | one of `wait_all`, `best_effort` | Convergence rule when several incoming branches reach the node |
| `expr` | block → [expr](#expr) | One `field: "expression"` per output field |

### expr

The expressions of a compute node.

A block opened by `expr:` inside `compute`.

Entries: `field: "expression"` — An expression over vars, input, outputs, artifacts, loop and run — not a {{template}}.

### subbot

Runs another .bot as a nested child run; its outputs read back as {{outputs.<subbot>.<field>}} (C119).

A node: `subbot <name>:` at the top level or inside a `group`.

| Property | Value | Meaning |
|---|---|---|
| `description` | string | Free-text description shown by the studio and the reports |
| `source` | string | Path of the child .bot, relative to this file |
| `with` | with { … } | Child vars; {{…}} references are allowed in the values |
| `output` | ident | Schema the node's structured output must match |
| `needs` | ident \| ident list | Resource(s) leased from the workflow's resources: block for the node's duration |
| `isolated` | bool | Asserts the child runs in its own workspace |

### emit

Publishes a named run-scoped event with an immutable payload (ADR-051).

A node: `emit <name>:` at the top level or inside a `group`.

| Property | Value | Meaning |
|---|---|---|
| `description` | string | Free-text description shown by the studio and the reports |
| `event` | string | Event name |
| `with` | with { … } | Payload fields |

### wait

Blocks its branch until the named event fires; the timeout is mandatory (ADR-051, C196–C198).

A node: `wait <name>:` at the top level or inside a `group`.

| Property | Value | Meaning |
|---|---|---|
| `description` | string | Free-text description shown by the studio and the reports |
| `event` | string | Event name awaited |
| `timeout` | string | Duration after which the wait fails, e.g. "30s" (mandatory) |
| `output` | ident | Schema the node's structured output must match |

### await_answers

Parks its branch until every pending ask_user_async question of `from:` (or the whole run) is answered; output {answers: […]} (ADR-081, C241/C242).

A node: `await_answers <name>:` at the top level or inside a `group`.

| Property | Value | Meaning |
|---|---|---|
| `description` | string | Free-text description shown by the studio and the reports |
| `from` | string\|ident | Node whose async questions are awaited; omit for the whole run |
| `timeout` | string | Duration after which the node fails, e.g. "30m" (mandatory) |

### fail

A named terminal failure with a typed code and a message (C247 checks the UPPER_SNAKE code, C248 refuses a reserved one).

A node: `fail <name>:` at the top level or inside a `group`.

| Property | Value | Meaning |
|---|---|---|
| `description` | string | Free-text description shown by the studio and the reports |
| `code` | string\|ident | Error code, UPPER_SNAKE (bare or quoted) |
| `message` | string | Message; {{…}} references are rendered |
| `resumable` | bool | Leaves the run failed_resumable instead of failed |

### workflow

The graph: entry, edges (`src -> dst [when …|else] [as loop(N)] [with {…}]`), and the run-wide settings; a bare header declares an empty workflow (C008).

A top-level declaration: `workflow <name>:`.

| Property | Value | Meaning |
|---|---|---|
| `entry` | ident | Node the run starts at; a dotted name addresses a group instance's node |
| `vars` | block → [vars](#vars) | Workflow-scoped vars (merged with the file's) |
| `attachments` | block → [attachments](#attachments) | Workflow-scoped attachments |
| `budget` | block → [budget](#budget) | Run caps, each overridable by the matching run flag |
| `resources` | block → [resources](#resources) | Named semaphores and pools nodes lease with needs: |
| `mcp` | block → [mcp](#mcp) | MCP servers active for the run |
| `compaction` | block → [compaction](#compaction) | Default compaction thresholds |
| `sandbox` | one of `none`, `auto`, or a block → [sandbox](#sandbox) | Sandbox for this scope: a bare mode (none, auto) or an indented block — the inline form, which needs image: or build: (C044) |
| `worktree` | ident — `auto`, `none` | auto runs the workflow in a fresh git worktree, finalised into a branch; none runs in place |
| `default_backend` | string | Backend for nodes that name none |
| `compress` | ident — `on`, `ultra`, `off` | Command-output compression: on, ultra or off (C102) |
| `auto_memory` | ident — `on`, `off` | The backend's own auto-memory: on or off (C131/C132) |
| `loop_budget_guard` | ident — `on`, `off` | Decline a loop's back-edge the budget cannot fund: on (default) or off (C133) |
| `repo_devbox` | ident — `on`, `off` | Load the target repo's devbox.json toolchain: on (default) or off (C134) |
| `workspace_checkpoint` | ident — `on`, `off` | Mid-run preservation of a copy-based sandbox's workspace as a checkpoint branch pushed to the run's own remote: on (default) or off (C139) |
| `permission` | ident — `off`, `ask`, `deny` | Tool-permission gate: off, ask or deny (C110–C112) |
| `allow` | string list | Permission rules always allowed, Tool(pattern) syntax |
| `ask` | string list | Permission rules that pause for approval |
| `deny` | string list | Permission rules always blocked |
| `tool_policy` | tool list | Run-wide tool-policy entries |
| `capabilities` | tool list | Run-wide board capabilities |
| `skills` | skill list | Run-wide skill-library skills |
| `interaction` | one of `none`, `human`, `llm`, `llm_or_human`, `review`, `async` | Default interaction mode for the run's nodes that set none |

### budget

Run caps; a zero or absent cap is the engine default, and each is overridable per run without editing the .bot.

A block opened by `budget:` inside `workflow`.

| Property | Value | Meaning |
|---|---|---|
| `max_parallel_branches` | int | Concurrent branches (0 = engine default) |
| `max_duration` | string | Wall-clock cap, e.g. "4h" |
| `max_cost_usd` | number | Spend cap in USD |
| `max_tokens` | int | Total token cap |
| `warn_tokens` | int | Advisory: crossing it emits budget_warning |
| `max_iterations` | int | Total node executions; also the fuel of an unbounded loop (C097) |

### resources

Named resources nodes lease with needs:.

A block opened by `resources:` inside `workflow`.

Entries: `name: <int> | ["member-a", "member-b"]` — A count is a semaphore; a quoted list is a pool whose members are leased one at a time.

### compaction

When and how a session's context is compacted.

A block opened by `compaction:` inside `workflow`, `agent`, `judge`.

| Property | Value | Meaning |
|---|---|---|
| `threshold` | number | Context fraction (0–1) that triggers compaction |
| `preserve_recent` | int | Recent messages kept verbatim |

### memory

iterion's shared-memory tools for a node (docs/memory-and-knowledge.md); distinct from auto_memory.

A block opened by `memory:` inside `agent`, `judge`.

| Property | Value | Meaning |
|---|---|---|
| `enabled` | bool | Open the memory tools to the node |
| `scope` | string | Memory scope the tools read and write |
| `autoload` | string list | Documents injected at node start |
| `read` | bool | Allow memory_read |
| `write` | bool | Allow memory_write |
| `pre_compact_inject` | bool | Re-inject memory before a compaction |
| `project_root` | bool | Key the space on the repository root rather than the working directory (legacy; exclusive with visibility) |
| `visibility` | string — `bot`, `project`, `cross_project`, `user`, `org`, `global` | Who sees the space (C170); quoted |

### mcp

Which MCP servers are active; an empty block wires nothing (C135 stays an error).

A block opened by `mcp:` inside `workflow`, `agent`, `judge`.

| Property | Value | Meaning |
|---|---|---|
| `autoload_project` | bool | Workflow scope: load the repository's .mcp.json servers (default true) |
| `inherit` | bool | Node scope: inherit the workflow's active servers (default true) |
| `servers` | ident list | mcp_server declarations activated |
| `disable` | ident list | Servers removed from the ambient set |

### sandbox

Per-run container isolation (docs/sandbox.md): the short form names a mode, the block form is inline and needs image: or build: (C044).

A block opened by `sandbox:` inside `workflow`, `agent`, `judge`, `tool`.

| Property | Value | Meaning |
|---|---|---|
| `mode` | ident — `none`, `auto`, `inline` | none, auto (devcontainer.json or the published slim image) or inline (C044 on another word) |
| `image` | string | Container image (exclusive with build) |
| `build` | block → [sandbox.build](#sandboxbuild) | Dockerfile build, local docker only (V2-6) |
| `user` | string | Container user |
| `workspace_folder` | string | Mount point of the workspace inside the container |
| `host_state` | ident — `auto`, `none` | Mount ~/.iterion and ~/.claude into the container: auto or none |
| `post_create` | string | Command run once after the container starts |
| `env` | map | Environment variables |
| `mounts` | string\|ident list | Extra bind mounts |
| `network` | block → [sandbox.network](#sandboxnetwork) | Egress policy (open by default) |

### sandbox.build

A Dockerfile build of the sandbox image (docker driver only).

A block opened by `build:` inside `sandbox`.

| Property | Value | Meaning |
|---|---|---|
| `dockerfile` | string | Dockerfile path |
| `context` | string | Build context |
| `args` | map | Build arguments |

### sandbox.network

Network egress of the sandbox, enforced by a CONNECT proxy on the host.

A block opened by `network:` inside `sandbox`.

| Property | Value | Meaning |
|---|---|---|
| `mode` | ident — `open`, `allowlist`, `denylist` | open (no proxy), allowlist or denylist (C044 on another word) |
| `preset` | string\|ident | Rule preset, e.g. "iterion-default" |
| `inherit` | ident — `replace`, `append` | How a node's rules compose with the workflow's: omit to merge (the default), or replace / append (C044 on another word) |
| `rules` | string\|ident list | Hosts and globs; a leading ! negates |

### cursors

Cursor activation on a node: the reserved enabled: key plus one setting per declared cursor.

A block opened by `cursors:` inside `agent`, `judge`.

| Property | Value | Meaning |
|---|---|---|
| `enabled` | bool | Gate for the whole block (an explicit block opts in) |

Entries: `cursor_name: ident | number | "string"` — A value name, a position in [0, 1], or a quoted string for ${VAR} substitution.

### fallback

One named route of a fallbacks: chain (ADR-087/ADR-091), tried in declaration order.

An entry opened by `fallbacks:` inside `agent`, `judge`.

| Property | Value | Meaning |
|---|---|---|
| `backend` | string | Execution backend: claw, claude_code, codex, pi, kimi or grok |
| `model` | string | Model id the backend serves, e.g. "anthropic/claude-opus-5"; empty takes the backend's default |
| `provider` | string | Provider hint for credential resolution, e.g. "anthropic" |
| `on` | ident list over `usage_window`, `auth`, `unavailable`, `transient_exhausted`, `any` | Failure classes that take this route (default usage_window, unavailable; never any or auth by default) |
| `metered` | bool | The route spends a metered API key (credential hint) |
| `action` | ident — `skip` | skip: complete the node with a zero-value output stamped _skipped instead of failing |
| `when` | string | Expression over vars that gates the route |

<!-- dsl-spec:end -->
