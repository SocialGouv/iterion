# Iterion `.bot` grammar reference

This is the readable inventory of the syntax accepted by the current parser. The machine-oriented counterpart is [`grammar/iterion_v1.ebnf`](../grammar/iterion_v1.ebnf). Parsing success is only the first stage: the IR compiler then checks declarations, types, references, graph structure, mode-specific properties, loops, resources, and capabilities.

Notation: `{x}` means zero or more, `[x]` is optional, and `a | b` is an alternative. Indentation is significant; examples use two spaces. `##` comments and blank lines are ignored between constructs.

## File declarations

```ebnf
file = { top_level_decl } ;

top_level_decl = vars | presets | attachments | secrets | mcp_server
               | prompt | schema | cursor | supervisor
               | agent | judge | router | human | tool | compute
               | emit | wait | await_answers | subbot | group | use | workflow ;
```

At most one top-level `vars`, `presets`, `attachments`, and `secrets` block is retained. Named declarations may repeat only when their names remain unique after compilation.

## Lexical values

Identifiers match `[A-Za-z_][A-Za-z0-9_]*`; quote kebab-case skill names and other values containing punctuation. A DSL string can use:

```iter
key: "escaped string"
key: `raw string: $SHELL and "quotes" stay literal`
key: |
  block scalar
  with preserved newlines
```

Raw strings have no backtick escape. `## strict-escape: on` in the leading file comments opts quoted strings into standard escape interpretation. Lists are bracketed and comma-separated. Depending on the property, elements are identifiers, strings, tool refs (`mcp.server.*`), or either.

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
prompt = "prompt" IDENT ":" INDENT { free_text_line } DEDENT ;

schema = "schema" IDENT ":" INDENT { schema_field } DEDENT ;
schema_field = IDENT ":" ( type | "file" ) [ enum ] ;
```

Prompt text may contain runtime `{{...}}` references and compile-time `{{include "relative/file"}}` directives. Schema fields accept the six variable types, plus `file` — an operator-supplied binary valid only on a human node's schema; the compiler rejects it elsewhere ([C129](diagnostics.md)).

## MCP declarations and activation

```ebnf
mcp_server = "mcp_server" IDENT ":" INDENT { mcp_server_property } DEDENT ;
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
cursor = "cursor" IDENT ":" INDENT
           [ "description:" STRING ]
           ( "values:" INDENT { IDENT ":" STRING } DEDENT
           | "bands:" INDENT { STRING ":" STRING } DEDENT )
         DEDENT ;

cursors = "cursors:" INDENT
            { IDENT ":" ( IDENT | STRING | NUMBER | BOOL ) }
          DEDENT ;

supervisor = "supervisor" IDENT ":" INDENT
               { "watches:" ident_list | "model:" STRING
               | "system:" IDENT | "cooldown:" STRING
               | "max_evals:" INT }
             DEDENT ;
```

`cursors:` activates declared cursors on an agent/judge; the reserved `enabled:` key gates the block. A supervisor is concurrent run metadata, not a graph node.

## Agents and judges

```ebnf
agent = "agent" IDENT ":" INDENT { llm_property } DEDENT ;
judge = "judge" IDENT ":" INDENT { llm_property } DEDENT ;
```

They share the exact property surface:

| Property | Value |
|---|---|
| `description` | string |
| `model`, `backend`, `provider`, `command` | string |
| `input`, `output`, `publish`, `system`, `user` | identifier reference |
| `artifact_labels` | tool-ref-style identifier list |
| `session` | `fresh`, `inherit`, `inherit_if_available`, `fork`, `artifacts_only`, `persist` |
| `tools`, `tool_policy`, `capabilities` | tool-ref list; dotted refs and trailing `.*` are accepted |
| `skills` | quoted string or dotted-identifier list |
| `tool_max_steps`, `max_tokens` | integer |
| `reasoning_effort` | `low`, `medium`, `high`, `xhigh`, `max`, `ultracode`, or quoted runtime value |
| `timeout` | duration string |
| `readonly`, `full_access` | boolean |
| `images` | string list |
| `interaction` | `none`, `human`, `llm`, `llm_or_human`, `review`, `async` |
| `interaction_prompt` | prompt identifier |
| `interaction_model` | string |
| `await` | `wait_all`, `best_effort` |
| `compress` | `off`, `on`, `ultra` |
| `permission` | `off`, `ask`, `deny` |
| `needs` | one resource identifier or an identifier list |
| `mcp`, `compaction`, `memory`, `sandbox`, `cursors` | nested blocks described here |

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

| Property | Applies to |
|---|---|
| `description: STRING`, `mode: router_mode`, `needs: needs_value` | all routers |
| `model: STRING`, `backend: STRING`, `provider: STRING`, `system: IDENT`, `user: IDENT`, `multi: BOOL`, `reasoning_effort: effort` | `llm` |
| `over: STRING`, `as: IDENT`, `key: IDENT`, `depends_on: IDENT` | `fan_out_each` |

`fan_out_each` requires `over` and exactly one unconditional outgoing template edge. `depends_on` requires `key`. Routers never accept `await`.

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

| Property | Value |
|---|---|
| `description`, `command`, `script`, `goal`, `postcondition` | string |
| `language` | `js`, `py`, `sh`, `bash` |
| `input`, `output`, `publish` | identifier |
| `artifact_labels` | tool-ref list |
| `await` | `wait_all`, `best_effort` |
| `sandbox` | sandbox block |
| `compress` | `off`, `on`, `ultra` |
| `permission` | `off`, `ask`, `deny` |
| `needs` | one identifier or identifier list |
| `parallel_safe` | boolean |
| `policy` | `required`, `recover`, `best_effort` |
| `recovery` | nested block |

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
        INDENT { agent | judge | router | human | tool | compute | edge } DEDENT ;

use = "use" IDENT "as" IDENT [ with_block ] ;
```

Groups are compile-time macros. `use` bindings substitute `{{params.name}}`; expanded nodes are addressed as `<prefix>.<node>`.

## Workflows

```ebnf
workflow = "workflow" IDENT ":" INDENT { workflow_member } DEDENT ;
```

Workflow members may appear in any order:

| Member | Value |
|---|---|
| `vars`, `attachments` | blocks described above |
| `entry` | plain or dotted node reference |
| `default_backend` | string |
| `tool_policy`, `capabilities` | tool-ref list |
| `skills` | skill-ref list |
| `mcp`, `budget`, `resources`, `compaction`, `sandbox` | nested blocks |
| `interaction` | interaction mode |
| `worktree` | `auto`, `none` |
| `compress` | `off`, `on`, `ultra` |
| `permission` | `off`, `ask`, `deny` |
| `allow`, `ask`, `deny` | string list |
| edge | graph transition |

Budget fields are `max_parallel_branches: INT`, `max_duration: STRING`, `max_cost_usd: NUMBER`, `max_tokens: INT`, `warn_tokens: INT` (advisory-only — crossing it emits a `budget_warning`), and `max_iterations: INT`.

Resources are either counting semaphores or named-member pools:

```iter
resources:
  browser: 2
  worktree: ["slot-a", "slot-b"]
```

Nodes acquire them with `needs: browser` or `needs: [browser, worktree]`.

## Sandbox block

Short form:

```iter
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
edge = node_ref "->" node_ref { when_or_else | iteration | with_block } ;
node_ref = IDENT { "." IDENT } | "done" | "fail" ;

when_or_else = "when" ( [ "not" ] IDENT | STRING ) | "else" ;
iteration = "as" IDENT "(" ( INT | STRING | "unbounded" [ INT ] ) ")"
          | "as foreach" IDENT "(" IDENT "in" STRING ")" ;
with_block = "with" "{" { IDENT ":" STRING [ "," ] } "}" ;
```

Clauses may occur in any order, but each kind may occur at most once. `when` and `else` are mutually exclusive. A quoted `when` is parsed as an expression. Every graph cycle must be declared by a named loop or finite `foreach`; `unbounded` loops require a fuel source and an exit edge.

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

Standard namespaces are `vars`, `input`, `outputs`, `artifacts`, `loop`, and `run`. Built-ins are `length`, `concat`, `unique`, `contains`, `join`, `tail`, `if`, `sort`, `keys`, `values`, `slice`, `sum`, `min`, `max`, `flatten`, `floor`, `round`, `map`, `filter`, and `reduce`. Lambdas are confined to finite combinators, expression depth is capped, and one evaluation may visit at most 100,000 elements.

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
