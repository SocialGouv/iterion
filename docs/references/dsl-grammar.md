# Iterion `.bot` grammar reference

This is the readable inventory of the syntax accepted by the current parser. The machine-oriented counterpart is [`grammar/iterion_v1.ebnf`](../grammar/iterion_v1.ebnf). Parsing success is only the first stage: the IR compiler then checks declarations, types, references, graph structure, mode-specific properties, loops, resources, and capabilities.

The property tables on this page are **generated** from the parser's property registry ([`pkg/dsl/spec`](../../pkg/dsl/spec/spec.go), `task dsl:gen`), which a conformance test holds to the parser in both directions; the complete per-kind reference, blocks included, is [dsl-properties.md](dsl-properties.md).

Notation: `{x}` means zero or more, `[x]` is optional, and `a | b` is an alternative. Indentation is significant; examples use two spaces. `#` comments (`##` is the same comment) and blank lines are ignored between constructs.

## File declarations

```ebnf
file = [ dsl_header ] { top_level_decl } ;

dsl_header = "dsl" ":" INT NEWLINE ;   (* the syntax profile, on the first significant line; absent = 1 *)

top_level_decl = vars | presets | attachments | secrets | mcp_server
               | prompt | schema | cursor | supervisor
               | agent | judge | router | human | tool | compute
               | emit | wait | await_answers | fail | subbot | group | use | workflow
               | contract ;
```

At most one top-level `vars`, `presets`, `attachments`, and `secrets` block is retained. Named declarations may repeat only when their names remain unique after compilation.

## Lexical values

Identifiers match `[A-Za-z_][A-Za-z0-9_]*`; quote kebab-case skill names and other values containing punctuation. A DSL string can use:

```text
key: "escaped string"
key: `raw string: $SHELL and "quotes" stay literal`
key: |
  block scalar
  with preserved newlines
```

Raw strings have no backtick escape. Under `dsl: 2` (the syntax profile, declared on the file's first significant line) a quoted string reads the standard escapes `\"` `\\` `\n` `\t` `\r` `\0`; under profile 1 (no header) every backslash is kept verbatim unless a `# strict-escape: on` line (or `## strict-escape: on`) sits among the first 31 lines of the file (line 32 counts only when it ends the file), before its first line of code — a profile-1 rule frozen as it is; the directive is refused under profile 2 (E042). One plain bare word is also a string value (`backend: claw`). Lists are bracketed and comma-separated, or written one `- item` per line indented under the property; both forms read as the same list in every profile. Depending on the property, elements are identifiers, strings, tool refs (`mcp.server.*`), or either.

Scalar declaration literals are strings, integers, floats, or booleans. JSON and `string[]` defaults/preset values therefore use a quoted JSON representation.

## Variables, presets, attachments, and secrets

### Variables and presets

```ebnf
vars = "vars:" INDENT { IDENT ":" type [ enum ] [ "=" literal ] } DEDENT ;
type = "string" | "bool" | "int" | "float" | "json" | "string[]" ;
enum = "[enum:" STRING { "," STRING } "]" ;

presets = "presets:" INDENT
            { IDENT ":" INDENT { IDENT ":" literal } DEDENT }
          DEDENT ;
```

`vars:` is valid at top level and inside a workflow; `presets:` is top-level only. Enum constraints apply only to strings.

### Attachments

```ebnf
attachments = "attachments:" INDENT { attachment } DEDENT ;
attachment = IDENT ":" ( "file" | "image" )
             [ INDENT { attachment_property } DEDENT ] ;
attachment_property = "description:" STRING
                    | "accept_mime:" string_list
                    | "required:" BOOL ;
```

Attachments are valid at top level and inside a workflow.

### Secrets

```ebnf
secrets = "secrets:" INDENT { secret } DEDENT ;
secret = IDENT ":" [ STRING ] [ INDENT { secret_property } DEDENT ] ;
secret_property = "value:" STRING
                | "as:" ( "value" | "file" )
                | "mount_path:" STRING
                | "env:" string_or_ident
                | "optional:" BOOL
                | "hosts:" string_list
                | "description:" STRING ;
```

`secrets:` is top-level only. The short form supplies `value`; the block form may omit it so the runtime resolves a stored secret by declaration name.

## Prompts and schemas

```ebnf
prompt = "prompt" IDENT ":" [ INDENT { free_text_line } DEDENT ] ;

schema = "schema" IDENT ":" [ INDENT { schema_field } DEDENT ] ;
schema_field = IDENT ":" ( type | "file" ) [ enum ] ;
```

A `prompt`, `schema`, `mcp_server`, `cursor`, `supervisor`, `group`, `contract` or `workflow` header may stand with no body at all — followed by a **blank line** and another declaration, or by the end of the file — and declares an empty one: the studio saves a declaration the moment it is created, before it has a field or a line (an empty workflow then draws the compiler's own diagnostics, no entry first). The blank line is what tells an empty declaration from a body at the wrong indentation (a group's members are top-level keywords themselves): a header followed directly by an unindented line, or by an indented comment alone, is still the indentation error. Node declarations (`agent`, `tool`, …) keep needing a body. A **block** header inside a declaration or at the top level — `vars:`, `budget:`, `memory:`, `mcp:`, `auth:`, `cursors:`, `recovery:`, `compaction:`, `resources:`, `presets:`, `attachments:`, `secrets:`, `sandbox:` and its `build:`/`network:`, a contract's `inputs:`/`outputs:`/`criteria:`/`effects:` and a port's `file:` — may stand bare under the same rule and declares an empty block, kept as the author wrote it rather than dropped: a nested block ends at its parent's dedent or before a blank line and a sibling; a top-level block needs the blank line (or the end of the file), having no dedent. What the empty block means is the compiler's: an empty `mcp:` wires nothing, a bare `sandbox:` is the inline block form (C044 until it carries `image:` or `build:`). `fallbacks:` is the exception — a chain with no route is refused by name.

Prompt text may contain runtime `{{...}}` references and compile-time `{{include "relative/file"}}` directives. Schema fields accept the six variable types, plus `file` — an operator-supplied binary valid only on a human node's schema; the compiler rejects it elsewhere ([C129](diagnostics.md)).

## MCP declarations and activation

```ebnf
mcp_server = "mcp_server" IDENT ":" [ INDENT { mcp_server_property } DEDENT ] ;
mcp_server_property = "transport:" ( "stdio" | "http" | "sse" )
                    | "command:" STRING
                    | "args:" string_list
                    | "url:" STRING
                    | "auth:" INDENT { auth_property } DEDENT ;
auth_property = "type:" STRING | "auth_url:" STRING | "token_url:" STRING
              | "revoke_url:" STRING | "client_id:" STRING
              | "scopes:" string_list ;

mcp_config = "mcp:" INDENT { mcp_property } DEDENT ;
mcp_property = "autoload_project:" BOOL | "inherit:" BOOL
             | "servers:" ident_list | "disable:" ident_list ;
```

`stdio` uses `command`/`args`; `http` and `sse` use `url`. Only OAuth2 authorization-code/PKCE auth is wired. `mcp:` is accepted on workflows, agents, and judges; workflow scope uses `autoload_project`, while node scope uses `inherit`.

## Cursor and supervisor declarations

```ebnf
cursor = "cursor" IDENT ":" [ INDENT
           [ "description:" STRING ]
           ( "values:" INDENT { IDENT ":" STRING } DEDENT
           | "bands:" INDENT { STRING ":" STRING } DEDENT )
         DEDENT ] ;

cursors = "cursors:" INDENT
            { IDENT ":" ( IDENT | STRING | NUMBER | BOOL ) }
          DEDENT ;

supervisor = "supervisor" IDENT ":" [ INDENT
               { "watches:" ident_list | "model:" STRING
               | "system:" IDENT | "cooldown:" STRING
               | "max_evals:" INT }
             DEDENT ] ;
```

`cursors:` activates declared cursors on an agent/judge; the reserved `enabled:` key gates the block. A supervisor is concurrent run metadata, not a graph node.

## Agents and judges

```ebnf
agent = "agent" IDENT ":" INDENT { llm_property } DEDENT ;
judge = "judge" IDENT ":" INDENT { llm_property } DEDENT ;
```

They share the exact property surface (a tool-ref list accepts dotted refs and a trailing `.*`, and a quoted element is the literal name; `reasoning_effort` also takes a quoted runtime value):

<!-- dsl-spec:begin table agent -->
| Property | Value | Meaning |
|---|---|---|
| `description` | string | Free-text description shown by the studio and the reports |
| `model` | string | Model id the backend serves, e.g. "anthropic/claude-opus-5"; empty takes the backend's default; a {{vars.x}} reference resolves (vars only), then ${VAR:-default} |
| `backend` | string | Execution backend: claw, claude_code, codex, pi, kimi or grok; a {{vars.x}} reference resolves (vars only), then ${VAR:-default} |
| `provider` | string | Provider hint for credential resolution, e.g. "anthropic"; a {{vars.x}} reference resolves (vars only), then ${VAR:-default} |
| `command` | string | Executable that drives a CLI backend, overriding its default binary |
| `input` | ident | Schema the node's input is validated against |
| `output` | ident | Schema the node's structured output must match |
| `publish` | ident | Artifact name the output is published under (read back as {{artifacts.<name>}}) |
| `artifact_labels` | tool list | Labels stamped on the published artifact; a quoted element is the literal label |
| `system` | ident | Prompt declaration used as the system prompt |
| `user` | ident | Prompt declaration used as the user message |
| `session` | one of `fresh`, `inherit`, `inherit_if_available`, `fork`, `artifacts_only`, `persist` | How the node's LLM session relates to the previous node's |
| `session_slot` | ident | Named durable session slot; requires session: persist |
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
<!-- dsl-spec:end -->

Nested blocks:

```ebnf
compaction = "compaction:" INDENT
               { "threshold:" NUMBER | "preserve_recent:" INT }
             DEDENT ;

memory = "memory:" INDENT
           { "enabled:" BOOL | "scope:" STRING | "autoload:" string_list
           | "read:" BOOL | "write:" BOOL | "pre_compact_inject:" BOOL
           | "project_root:" BOOL | "visibility:" STRING }
         DEDENT ;
```

## Routers

```ebnf
router = "router" IDENT ":" INDENT { router_property } DEDENT ;
router_mode = "fan_out_all" | "fan_out_each" | "condition"
            | "round_robin" | "llm" ;
```

<!-- dsl-spec:begin table router -->
| Property | Value | Meaning |
|---|---|---|
| `description` | string | Free-text description shown by the studio and the reports |
| `mode` | one of `fan_out_all`, `fan_out_each`, `condition`, `round_robin`, `llm` | Routing mode |
| `model` | string | llm mode only (C023 otherwise): Model id the backend serves, e.g. "anthropic/claude-opus-5"; empty takes the backend's default; a {{vars.x}} reference resolves (vars only), then ${VAR:-default} |
| `backend` | string | llm mode only (C023 otherwise): Execution backend: claw, claude_code, codex, pi, kimi or grok; a {{vars.x}} reference resolves (vars only), then ${VAR:-default} |
| `provider` | string | Provider hint for credential resolution, e.g. "anthropic"; a {{vars.x}} reference resolves (vars only), then ${VAR:-default} |
| `system` | ident | llm mode only (C023 otherwise): Prompt declaration used as the system prompt |
| `user` | ident | llm mode only (C023 otherwise): Prompt declaration used as the user message |
| `multi` | bool | llm mode only (C023 otherwise): the model may select several outgoing edges |
| `reasoning_effort` | one of `low`, `medium`, `high`, `xhigh`, `max`, `ultracode` | llm mode only (C023 otherwise): Reasoning effort; ultracode is xhigh plus multi-agent orchestration, reliable on Opus 4.8 and the Claude 5 family (Opus 5, Fable 5.1) only (C089 warns elsewhere); a quoted string is env-substituted at runtime |
| `over` | string | fan_out_each: expression naming the collection to iterate |
| `as` | ident | fan_out_each: alias each item is bound to ({{each.<as>}}) |
| `key` | ident | fan_out_each: item field that names each branch |
| `depends_on` | ident | fan_out_each: item field naming the branch this one waits for (requires key) |
| `needs` | ident \| ident list | Resource(s) leased from the workflow's resources: block for the node's duration |
<!-- dsl-spec:end -->

`description`, `mode` and `needs` apply to every router; the model properties to `llm`; `over`, `as`, `key` and `depends_on` to `fan_out_each`, which requires `over` and exactly one unconditional outgoing template edge. `depends_on` requires `key`. Routers never accept `await`.

## Human nodes

```ebnf
human = "human" IDENT ":" INDENT { human_property } DEDENT ;
```

Accepted properties are `description: STRING`, `input/output/publish: IDENT`, `artifact_labels: tool_ref_list`, `instructions/system/interaction_prompt: IDENT`, `interaction: interaction_mode`, `interaction_model/model/review_url: STRING`, `min_answers/max_turns: INT`, `await: await_mode`, and the string-or-identifier review fields `posture`, `merge_strategy`, and `merge_into`.

## Tool and compute nodes

### Tool

```ebnf
tool = "tool" IDENT ":" INDENT { tool_property } DEDENT ;
```

<!-- dsl-spec:begin table tool -->
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
| `action` | ident | Connector operation to call, `connector.resource.verb` — exclusive with command:/script: (ADR-098, C260) |
| `connection` | ident | The connection binding that authenticates the action (C261) |
| `params` | block → [params](#params) | The action's arguments, by the operation's own parameter keys |
| `retry` | string | Action: how many EXTRA attempts, e.g. `3`; a duration is refused and empty means none (C265). Inert without `action:` (C266) |
| `timeout` | string | Action: bound on one call, e.g. "30s" (C265). Inert without `action:` (C266) |
<!-- dsl-spec:end -->

`command` and `script` are mutually exclusive. Recovery accepts `max_repair_attempts: INT`, `max_agent_attempts: INT`, `model: STRING`, and `agent_tools: tool_ref_list`.

### Compute

```ebnf
compute = "compute" IDENT ":" INDENT { compute_property } DEDENT ;
compute_property = "description:" STRING | "input:" IDENT | "output:" IDENT
                 | "publish:" IDENT | "artifact_labels:" tool_ref_list
                 | "await:" await_mode
                 | "expr:" INDENT { IDENT ":" STRING } DEDENT ;
```

Each `expr` string is parsed by the bounded expression language described below.

## Event and nested-run nodes

```ebnf
emit = "emit" IDENT ":" INDENT
         { "description:" STRING | "event:" STRING | with_block }
       DEDENT ;

wait = "wait" IDENT ":" INDENT
         { "description:" STRING | "event:" STRING
         | "timeout:" STRING | "output:" IDENT }
       DEDENT ;

subbot = "subbot" IDENT ":" INDENT
           { "description:" STRING | "source:" STRING | with_block
           | "output:" IDENT | "needs:" needs_value | "isolated:" BOOL }
         DEDENT ;
```

`wait` requires a timeout. A subbot launches the child source as a real nested run.

## Groups and uses

```ebnf
group = "group" IDENT [ "(" [ IDENT { "," IDENT } ] ")" ] ":"
        [ INDENT { agent | judge | router | human | tool | compute | edge } DEDENT ] ;

use = "use" IDENT "as" IDENT [ with_block ] ;
```

Groups are compile-time macros. `use` bindings substitute `{{params.name}}`; expanded nodes are addressed as `<prefix>.<node>`.

## Workflows

```ebnf
workflow = "workflow" IDENT ":" INDENT { workflow_member } DEDENT ;
```

Workflow members — the properties below and the edges (`src -> dst …`) — may appear in any order:

<!-- dsl-spec:begin table workflow -->
| Property | Value | Meaning |
|---|---|---|
| `entry` | ident | Node the run starts at; a dotted name addresses a group instance's node |
| `contract` | ident | The bot's public contract (a top-level `contract` declaration), bound to the program (C300–C304) |
| `vars` | block → [vars](#vars) | Workflow-scoped vars (merged with the file's) |
| `attachments` | block → [attachments](#attachments) | Workflow-scoped attachments |
| `budget` | block → [budget](#budget) | Run caps, each overridable by the matching run flag |
| `resources` | block → [resources](#resources) | Named semaphores and pools nodes lease with needs: |
| `mcp` | block → [mcp](#mcp) | MCP servers active for the run |
| `compaction` | block → [compaction](#compaction) | Default compaction thresholds |
| `sandbox` | one of `none`, `auto`, or a block → [sandbox](#sandbox) | Sandbox for this scope: a bare mode (none, auto) or an indented block — the inline form, which needs image: or build: (C044) |
| `worktree` | ident — `auto`, `none` | auto runs the workflow in a fresh git worktree, finalised into a branch; none runs in place |
| `default_backend` | string | Backend for nodes that name none; a {{vars.x}} reference resolves (vars only), then ${VAR:-default} |
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
<!-- dsl-spec:end -->

Budget fields are `max_parallel_branches: INT`, `max_duration: STRING`, `max_cost_usd: NUMBER`, `max_tokens: INT`, `warn_tokens: INT` (advisory-only — crossing it emits a `budget_warning`), and `max_iterations: INT`.

Resources are either counting semaphores or named-member pools:

```iter fragment:workflow
resources:
  browser: 2
  worktree: ["slot-a", "slot-b"]
```

Nodes acquire them with `needs: browser` or `needs: [browser, worktree]`.

## Public contract

```ebnf
contract = "contract" IDENT ":" NEWLINE [ INDENT { contract_prop } DEDENT ] ;
contract_prop = "display_name" ":" str_value NEWLINE
              | "responsibility" ":" str_value NEWLINE
              | "version" ":" INT_LIT NEWLINE
              | ( "inputs" | "outputs" ) ":" NEWLINE [ INDENT { contract_port } DEDENT ]
              | "criteria" ":" NEWLINE [ INDENT { contract_criterion } DEDENT ]
              | "effects" ":" NEWLINE [ INDENT { contract_effect } DEDENT ] ;
contract_port = IDENT ":" port_type NEWLINE [ INDENT { contract_port_prop } DEDENT ] ;
port_type = ( IDENT | "string[]" ) { "[" "]" } ;
json_value = "null" | bool_value | INT_LIT | FLOAT_LIT | STRING_LIT
           | "[" [ json_value { "," json_value } ] "]"
           | "{" [ json_member { "," json_member } ] "}" ;
```

A `contract` is the bot's public face — inputs, outputs, delivered files, deterministic criteria, visible effects — declared once at top level, named by the workflow's `contract:`, and checked against the program: an input is a declared var, required exactly when the var has no default (C300), an output names the node and field that produce it — `from: build.pr_url`, or `from: build` for a file (C301) — and a criterion names a port in the singular (`input.goal`, `output.pr_url`) and a registered `kind` (C302; an unregistered kind is declared, not evaluated, C303). `default:` and `params:` take **one JSON value on one line** — `"text"`, `12`, `true`, `null`, `[...]` or `{key: value}` — with no bare word (unlike every string property), no signed number and no exponent, which the `.bot` text cannot write. The full productions are in [iterion_v1.ebnf](../grammar/iterion_v1.ebnf); the decision and its bounds in [ADR-099](../adr/099-public-contracts.md). A bare `contract` header, or a bare `inputs:` / `outputs:` / `criteria:` / `effects:` header and a port's bare `file:`, declares an empty one under the rule of every [empty declaration and block](#prompts-and-schemas): a blank line, the parent's dedent or the end of the file must follow the bare header. A comment survives a studio save only at the file's head, before the first declaration: written inside a contract, as inside any declaration, it is dropped when the studio writes the file back.

```iter
vars:
  goal: string

schema report:
  summary: string
  pr_url: string

prompt build_user:
  Implement {{vars.goal}} and report the pull request URL.

agent build:
  model: "anthropic/claude-sonnet-4-6"
  user: build_user
  output: report

contract feature:
  display_name: "Feature dev"
  responsibility: "Implements a feature and opens a pull request"
  version: 1
  inputs:
    goal: string
      description: "What to build"
  outputs:
    pr_url: string
      from: build.pr_url
  criteria:
    goal_is_not_empty:
      kind: min_length
      port: input.goal
      params: {min: 1}
  effects:
    opens_pr:
      description: "Opens a pull request on the repository"

workflow feature_dev:
  contract: feature
  entry: build
  build -> done
```

<!-- dsl-spec:begin table contract -->
| Property | Value | Meaning |
|---|---|---|
| `display_name` | string | Explicit human-readable name |
| `responsibility` | string | The single responsibility this bot fulfils |
| `version` | int | Public contract version, 1 or more (C300); defaults to 1 |
| `inputs` | block → [contract.ports](#contractports) | Named typed values and files the bot takes; each one is a declared var (C300) |
| `outputs` | block → [contract.ports](#contractports) | Named typed values and files the bot produces on success; each one names the node and field that produce it (C301) |
| `criteria` | block → [contract.criteria](#contractcriteria) | Deterministic registered checks on a port; prose is not executable |
| `effects` | block → [contract.effects](#contracteffects) | Visible effects, including paid operations |
<!-- dsl-spec:end -->

A port (an `inputs:` / `outputs:` entry, `name: type`):

<!-- dsl-spec:begin table contract.port -->
| Property | Value | Meaning |
|---|---|---|
| `description` | string | Meaning of the value |
| `required` | bool | Mandatory port (default true). An input mirrors its var — required exactly when the var has no default — and a written value that disagrees is refused (C300) |
| `nullable` | bool | Permit an explicit null value (default false); on an input whose var has no default, the one way to be optional — omitted, the var is null (C300) |
| `default` | json value | Typed default of an optional input: the var's default, read as the launch reads a value of its type (a `string[]` or `json` var's text as a list or an object) — written, it must be the var's (C300); omitted, the var's is the port's. One JSON value on one line — `"text"`, `12`, `true`, `null`, `[...]`, `{key: value}` — with no signed number and no exponent, which the text cannot write and the compiler refuses from a document (C302) |
| `min_items` | int | Minimum array cardinality (C300 on an input, C301 on an output) |
| `max_items` | int | Maximum array cardinality (C300 on an input, C301 on an output) |
| `from` | ident | Producer of an output: `node.field` for a value, `node` for a file (C301); refused on an input (C300) |
| `file` | block → [contract.file](#contractfile) | Properties of a delivered or consumed file; existence and provenance are the runtime's checks |
<!-- dsl-spec:end -->

A port's `file:` block:

<!-- dsl-spec:begin table contract.file -->
| Property | Value | Meaning |
|---|---|---|
| `media_type` | string | Expected media type |
| `min_bytes` | int | Minimum file size |
| `schema` | ident | Schema of structured file contents |
<!-- dsl-spec:end -->

A criterion (a `criteria:` entry):

<!-- dsl-spec:begin table contract.criterion -->
| Property | Value | Meaning |
|---|---|---|
| `kind` | ident | Registered deterministic validator (see the criteria table; a plugin's may be dotted); an unregistered kind is declared but not evaluated (C303) |
| `port` | ident | Checked port, singular: `input.<name>` or `output.<name>` (C302) |
| `params` | json value | Parameters validated against the criterion's parameter declaration (C302): one JSON object on one line, e.g. `{min: 2}` |
<!-- dsl-spec:end -->

An effect (an `effects:` entry):

<!-- dsl-spec:begin table contract.effect -->
| Property | Value | Meaning |
|---|---|---|
| `description` | string | Externally visible operation |
| `paid` | bool | The operation may incur a charge; unknown cost remains unknown |
<!-- dsl-spec:end -->

## Sandbox block

Short form:

```iter fragment:workflow
sandbox: auto   # or none / inline
```

Block form accepts:

```ebnf
sandbox = "sandbox:" INDENT
            { "mode:" ( "auto" | "none" | "inline" )
            | "image:" STRING | build | "user:" STRING
            | "workspace_folder:" STRING | "host_state:" IDENT
            | "post_create:" STRING | "env:" string_map
            | "mounts:" string_or_ident_list | network }
          DEDENT ;

build = "build:" INDENT
          { "dockerfile:" STRING | "context:" STRING | "args:" string_map }
        DEDENT ;

network = "network:" INDENT
            { "mode:" IDENT | "preset:" string_or_ident
            | "inherit:" IDENT | "rules:" string_or_ident_list }
          DEDENT ;
```

Block form without `mode` implies `inline`. `image` and `build` are mutually exclusive. Sandbox blocks are accepted on workflows, agents, judges, and tools.

## Edges

```ebnf
edge = node_ref "->" node_ref { "->" node_ref } { when_or_else | iteration | with_block } ;
node_ref = IDENT { "." IDENT } | "done" | "fail" ;

when_or_else = "when" ( [ "not" ] IDENT | STRING ) | "else" ;
iteration = "as" IDENT "(" ( INT | STRING | "unbounded" [ INT ] ) ")"
          | "as foreach" IDENT "(" IDENT "in" STRING ")" ;
with_block = "with" "{" { IDENT ":" ( STRING | INT | FLOAT | BOOL ) [ "," ] } "}" ;
```

A chain `a -> b -> c` is one edge per arrow, in order; the clauses belong to the last edge (a clause before a further arrow is E032). A non-string `with` literal is read as its text (`n: 3` maps `"3"`). Clauses may occur in any order, but each kind may occur at most once. `when` and `else` are mutually exclusive. A quoted `when` is parsed as an expression. Every graph cycle must be declared by a named loop or finite `foreach`; `unbounded` loops require a fuel source and an exit edge.

## Expression language

`compute.expr` values and quoted `when` clauses use this precedence grammar:

```ebnf
expr = or ;
or = and { ( "||" | "or" ) and } ;
and = not { ( "&&" | "and" ) not } ;
not = ( "!" | "not" ) not | comparison ;
comparison = add [ ( "==" | "!=" | "<" | "<=" | ">" | ">=" ) add ] ;
add = multiply { ( "+" | "-" ) multiply } ;
multiply = unary { ( "*" | "/" | "%" ) unary } ;
unary = "-" unary | postfix ;
postfix = primary { "[" expr "]" } ;
primary = number | string | bool | path | call | lambda_call | "(" expr ")" ;
path = IDENT { "." IDENT } ;
call = IDENT "(" [ expr { "," expr } ] ")" ;
lambda_call = ( "map" | "filter" ) "(" expr "," lambda ")"
            | "reduce" "(" expr "," expr "," lambda ")" ;
lambda = ( IDENT | "(" IDENT { "," IDENT } ")" ) "=>" expr ;
```

Standard namespaces are `vars`, `input`, `outputs`, `artifacts`, `loop`, and `run`. Built-ins are `length`, `concat`, `unique`, `contains`, `join`, `tail`, `if`, `sort`, `keys`, `values`, `slice`, `sum`, `min`, `max`, `flatten`, `floor`, `round`, `map`, `filter`, and `reduce`. `min`/`max` accept either one array or two or more values (arguments are flattened one level). Lambdas are confined to finite combinators, expression depth is capped, and one evaluation may visit at most 100,000 elements.

## Template references

Normal runtime references have at least `namespace.path` and may continue through dotted fields:

```ebnf
template = "{{" [ "!" ] namespace "." IDENT { "." IDENT } "}}" ;
namespace = "vars" | "input" | "outputs" | "artifacts" | "attachments"
          | "secrets" | "loop" | "each" | "run" ;
```

`{{!input.command}}` requests raw substitution only while rendering a tool shell command; it disables shell escaping and must be restricted to trusted input. Group expansion separately consumes `{{params.name}}` before normal reference parsing.

Namespace-specific shapes and constraints are detailed in the [DSL guide](../dsl.md). Unknown or malformed references are diagnostics, not silently empty templates.

## Semantic validation

Syntax-valid files can still fail compilation for duplicate ids, unknown schemas/prompts/nodes, invalid templates, unreachable nodes, non-exhaustive routing, undeclared cycles, router-mode property misuse, unsafe fan-out, bad resource references, capability mismatches, and invalid sandbox/secret/cursor configuration.

Use `iterion validate file.bot`. The authoritative sparse code ranges are DSL C001–C199 (plus the async-interaction band C240–C242) and bundle checks C200–C234; see [diagnostics](diagnostics.md).
