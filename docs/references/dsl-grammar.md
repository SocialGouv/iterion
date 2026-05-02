# Iterion DSL V1 — Formal Grammar

This is the complete grammar specification for `.iter` files, updated to reflect all features supported by the parser.

Notation: `UPPER_CASE` = terminals/tokens, `lower_case` = non-terminals, `{ x }` = 0..N, `[ x ]` = optional, `( x | y )` = alternative, `"..."` = literal.

The DSL uses **significant indentation** (2 spaces per level). `INDENT` and `DEDENT` are virtual tokens emitted by the lexer.

---

## File Structure

```ebnf
file = { top_level_decl } ;

top_level_decl = vars_decl
               | mcp_server_decl
               | prompt_decl
               | schema_decl
               | agent_decl
               | judge_decl
               | router_decl
               | human_decl
               | tool_node_decl
               | compute_decl
               | workflow_decl
               | comment ;
```

## Comments

```ebnf
comment = "##" REST_OF_LINE ;
```

Comments start with `##` and extend to end of line. Allowed anywhere a top-level declaration or edge is expected.

## Variables

```ebnf
vars_decl = "vars" ":" NEWLINE INDENT { var_field } DEDENT ;
var_field = IDENT ":" type_expr [ "=" literal ] NEWLINE ;
type_expr = "string" | "bool" | "int" | "float" | "json" | "string[]" ;
literal   = STRING_LIT | INT_LIT | FLOAT_LIT | BOOL_LIT ;
```

## MCP Server Declarations

```ebnf
mcp_server_decl = "mcp_server" IDENT ":" NEWLINE INDENT { mcp_server_prop } DEDENT ;

mcp_server_prop = "transport" ":" mcp_transport     NEWLINE
                | "command"   ":" STRING_LIT         NEWLINE
                | "args"      ":" "[" STRING_LIT { "," STRING_LIT } "]" NEWLINE
                | "url"       ":" STRING_LIT         NEWLINE
                | "auth"      ":" NEWLINE INDENT { mcp_auth_prop } DEDENT ;

mcp_auth_prop = "type"       ":" STRING_LIT NEWLINE
              | "auth_url"   ":" STRING_LIT NEWLINE
              | "token_url"  ":" STRING_LIT NEWLINE
              | "revoke_url" ":" STRING_LIT NEWLINE
              | "client_id"  ":" STRING_LIT NEWLINE
              | "scopes"     ":" "[" STRING_LIT { "," STRING_LIT } "]" NEWLINE ;

mcp_transport = "stdio" | "http" | "sse" ;
```

Only `auth.type: "oauth2"` is wired today; other types raise C038.

- `stdio` requires `command`, forbids `url`
- `http` requires `url`, forbids `command` and `args`
- `sse` is recognized but not supported in V1

## Prompts

```ebnf
prompt_decl = "prompt" IDENT ":" NEWLINE INDENT prompt_body DEDENT ;
prompt_body = { PROMPT_TEXT_LINE } ;
```

Prompt text may contain template interpolations: `{{vars.X}}`, `{{input.X}}`, `{{outputs.node.field}}`, `{{artifacts.X}}`. Environment variables use `${VAR_NAME}` syntax.

## Schemas

```ebnf
schema_decl  = "schema" IDENT ":" NEWLINE INDENT { schema_field } DEDENT ;
schema_field = IDENT ":" field_type [ enum_constraint ] NEWLINE ;
field_type   = "string" | "bool" | "int" | "float" | "json" | "string[]" ;
enum_constraint = "[" "enum" ":" STRING_LIT { "," STRING_LIT } "]" ;
```

## Agent

```ebnf
agent_decl = "agent" IDENT ":" NEWLINE INDENT { agent_prop } DEDENT ;

agent_prop = "model"              ":" STRING_LIT              NEWLINE
           | "backend"            ":" STRING_LIT              NEWLINE
           | "input"              ":" IDENT                   NEWLINE
           | "output"             ":" IDENT                   NEWLINE
           | "publish"            ":" IDENT                   NEWLINE
           | "system"             ":" IDENT                   NEWLINE
           | "user"               ":" IDENT                   NEWLINE
           | "session"            ":" session_mode             NEWLINE
           | "tools"              ":" tool_list                NEWLINE
           | "tool_policy"        ":" tool_policy_list         NEWLINE
           | "tool_max_steps"     ":" INT_LIT                 NEWLINE
           | "max_tokens"         ":" INT_LIT                 NEWLINE
           | "reasoning_effort"   ":" reasoning_effort_value   NEWLINE
           | "readonly"           ":" BOOL_LIT                NEWLINE
           | "interaction"        ":" interaction_mode          NEWLINE
           | "interaction_prompt" ":" IDENT                    NEWLINE
           | "interaction_model"  ":" STRING_LIT               NEWLINE
           | "await"              ":" await_mode                NEWLINE
           | "mcp"                ":" NEWLINE INDENT { mcp_config_prop } DEDENT
           | "compaction"         ":" NEWLINE INDENT { compaction_prop } DEDENT ;

tool_policy_list = "[" tool_ref { "," tool_ref } "]" ;
tool_list        = "[" tool_ref { "," tool_ref } "]" ;
tool_ref         = IDENT { "." IDENT } [ "." "*" ] ;
```

`backend` accepts the names of built-in delegation backends —
`claw` (default, in-process), `claude_code`, `codex`. `model` and
`backend` are independent: a node can set both (provider/model spec
forwarded to the backend) or only one (a `model` alone uses the
default `claw` path; a `backend` alone uses that backend's own model
configuration).

## Judge

```ebnf
judge_decl = "judge" IDENT ":" NEWLINE INDENT { judge_prop } DEDENT ;
```

Judge properties are identical to agent properties. Semantically, judges produce verdicts and typically don't use tools.

## Router

```ebnf
router_decl = "router" IDENT ":" NEWLINE INDENT { router_prop } DEDENT ;

router_prop = "mode"             ":" router_mode             NEWLINE
            | "model"            ":" STRING_LIT              NEWLINE  (* llm mode only *)
            | "backend"          ":" STRING_LIT              NEWLINE  (* llm mode only *)
            | "system"           ":" IDENT                   NEWLINE  (* llm mode only *)
            | "user"             ":" IDENT                   NEWLINE  (* llm mode only *)
            | "multi"            ":" BOOL_LIT                NEWLINE  (* llm mode only *)
            | "reasoning_effort" ":" reasoning_effort_value  NEWLINE ;

router_mode = "fan_out_all" | "condition" | "round_robin" | "llm" ;
```

Routers do NOT support `await`. Properties `model`, `backend`, `system`, `user`, `multi`, `reasoning_effort` are only valid with `mode: llm` (diagnostic C023 otherwise). `llm` and `round_robin` routers also need at least 2 outgoing edges (C020/C021), and `llm` router edges must not carry `when` clauses (C022).

## Human

```ebnf
human_decl = "human" IDENT ":" NEWLINE INDENT { human_prop } DEDENT ;

human_prop = "input"              ":" IDENT           NEWLINE
           | "output"             ":" IDENT           NEWLINE
           | "publish"            ":" IDENT           NEWLINE
           | "instructions"       ":" IDENT           NEWLINE
           | "interaction"        ":" interaction_mode  NEWLINE
           | "interaction_prompt" ":" IDENT            NEWLINE
           | "interaction_model"  ":" STRING_LIT       NEWLINE
           | "min_answers"        ":" INT_LIT          NEWLINE
           | "model"              ":" STRING_LIT       NEWLINE
           | "system"             ":" IDENT            NEWLINE
           | "await"              ":" await_mode        NEWLINE ;
```

## Tool

```ebnf
tool_node_decl = "tool" IDENT ":" NEWLINE INDENT { tool_node_prop } DEDENT ;

tool_node_prop = "command" ":" STRING_LIT  NEWLINE
               | "input"   ":" IDENT       NEWLINE
               | "output"  ":" IDENT       NEWLINE
               | "await"   ":" await_mode   NEWLINE ;
```

`input:` is optional but useful when the command renders structured data via `{{input.field}}` template substitution. String-array fields (`string[]`) expand into the command line as space-joined items.

## Compute

Deterministic node — evaluates a list of expressions over the
`vars / input / outputs / artifacts / loop / run` namespaces and
emits a structured output. No LLM, no shell. Useful for streak
detection, boolean combinations, counters, simple aggregations.

```ebnf
compute_decl = "compute" IDENT ":" NEWLINE INDENT { compute_prop } DEDENT ;

compute_prop = "input"  ":" IDENT       NEWLINE
             | "output" ":" IDENT       NEWLINE
             | "await"  ":" await_mode   NEWLINE
             | compute_expr_block ;

compute_expr_block  = "expr" ":" NEWLINE INDENT { compute_expr_entry } DEDENT ;
compute_expr_entry  = IDENT ":" STRING_LIT NEWLINE ;
```

The `expr` block maps every output schema field that should be
populated by the compute node to a quoted expression. Built-ins
available inside expressions: `length(x)`, `concat(a, b, …)`,
`unique(list)`, `contains(list, item)`. A `compute` node with no
`expr` entries raises C039.

Example:

```iter
schema streak_state:
  consecutive_passes: int
  ready: bool

compute streak:
  output: streak_state
  expr:
    consecutive_passes: "loop.refine.iter"
    ready: "outputs.review.passed && loop.refine.iter >= 2"
```

## Workflow

```ebnf
workflow_decl = "workflow" IDENT ":" NEWLINE INDENT
                  { workflow_prop | edge | comment }
                DEDENT ;

workflow_prop = workflow_vars
              | workflow_entry
              | workflow_default_backend
              | workflow_tool_policy
              | workflow_worktree
              | workflow_mcp
              | workflow_budget
              | workflow_compaction
              | workflow_interaction ;

workflow_vars            = "vars"            ":" NEWLINE INDENT { var_field } DEDENT ;
workflow_entry           = "entry"           ":" IDENT NEWLINE ;
workflow_default_backend = "default_backend" ":" STRING_LIT NEWLINE ;
workflow_tool_policy     = "tool_policy"     ":" tool_policy_list NEWLINE ;
workflow_worktree        = "worktree"        ":" worktree_mode NEWLINE ;

worktree_mode = "auto" | "none" ;
```

Workflow body entries are accepted in any order by the parser. Exactly
one usable `entry:` is required semantically; a missing entry or an
entry that does not name a declared node is reported by C008 during IR
compilation.

`worktree: auto` runs the workflow inside a per-run git worktree at
`<store-dir>/worktrees/<run-id>/` so the user's main working tree
stays untouched; on a clean exit the worktree is removed automatically,
on failure it is preserved so the operator can inspect. Omit the
field (or set `none`) to run in place. See
[examples/vibe_feature_dev.iter](../../examples/vibe_feature_dev.iter)
for a workflow that opts in.

```ebnf
workflow_mcp = "mcp" ":" NEWLINE INDENT { mcp_config_prop } DEDENT ;

mcp_config_prop = "autoload_project" ":" BOOL_LIT              NEWLINE
                | "inherit"          ":" BOOL_LIT              NEWLINE
                | "servers"          ":" "[" IDENT { "," IDENT } "]" NEWLINE
                | "disable"          ":" "[" IDENT { "," IDENT } "]" NEWLINE ;

workflow_budget = "budget" ":" NEWLINE INDENT { budget_prop } DEDENT ;

budget_prop = "max_parallel_branches" ":" INT_LIT     NEWLINE
            | "max_duration"          ":" STRING_LIT  NEWLINE
            | "max_cost_usd"          ":" NUMBER_LIT  NEWLINE
            | "max_tokens"            ":" INT_LIT     NEWLINE
            | "max_iterations"        ":" INT_LIT     NEWLINE ;

workflow_compaction = "compaction" ":" NEWLINE INDENT { compaction_prop } DEDENT ;

compaction_prop = "threshold"       ":" NUMBER_LIT NEWLINE
                | "preserve_recent" ":" INT_LIT    NEWLINE ;

workflow_interaction = "interaction" ":" interaction_mode NEWLINE ;
```

`compaction.threshold` is a fraction of the model's context window in
`(0, 1]` (default `0.85`), and `compaction.preserve_recent` is the
minimum number of recent turns kept verbatim (default `4`). The block
is also valid on agent and judge nodes; either property may be
omitted to inherit the workflow / engine default. Out-of-range values
raise C043.

## Edges

```ebnf
edge = IDENT "->" IDENT [ when_clause ] [ loop_clause ] [ with_block ] NEWLINE ;

when_clause = "when" ( [ "not" ] IDENT | STRING_LIT ) ;
loop_clause = "as" IDENT "(" INT_LIT ")" ;
with_block  = "with" "{" NEWLINE { with_mapping } "}" ;
with_mapping = IDENT ":" STRING_LIT NEWLINE ;
```

Two `when` forms:

- **Simple boolean field:** `when approved` / `when not approved`. The
  identifier must reference a `bool` field in the source node's output
  schema (validated by C013/C014).
- **Quoted expression:** `when "approved && batch_complete"` or
  `when "loop.refine.iter < 3"`. The body is parsed at compile time and
  may use the same namespaces as a compute expression
  (`vars / input / outputs / artifacts / loop / run`), comparison and
  boolean operators, and the built-ins `length`, `concat`, `unique`,
  `contains`. Useful for compound predicates that don't fit a single
  schema field.

## Shared Enumerations

```ebnf
session_mode           = "fresh" | "inherit" | "fork" | "artifacts_only" ;
await_mode             = "wait_all" | "best_effort" ;
interaction_mode       = "none" | "human" | "llm" | "llm_or_human" ;
reasoning_effort_value = "low" | "medium" | "high" | "xhigh" | "max" ;
worktree_mode          = "auto" | "none" ;
```

The previous five-level reasoning effort scale (`low | medium | high`) was extended to add `xhigh` and `max` for the highest-effort tiers exposed by recent reasoning models; the runtime model registry decides which levels each model actually supports.

## Template Expressions

Inside `STRING_LIT` in with-blocks and prompt bodies:

```ebnf
template_expr = "{{" template_ref "}}" ;

template_ref = "vars"      "." IDENT
             | "input"     "." IDENT
             | "outputs"   "." IDENT [ "." IDENT ]
             | "artifacts" "." IDENT
             | "loop"      "." IDENT [ "." IDENT { "." IDENT } ]
             | "run"       "." IDENT ;
```

Special: `{{outputs.node_id.history}}` returns the array of all outputs from a node across loop iterations. Only valid if the node is in a declared loop.

The `loop.<name>.iter` reference exposes the current 0-based iteration counter of a declared loop, and `loop.<name>.previous.<field>` resolves to the previous iteration's value of a field on the loop's controlling node. The `run` namespace exposes a small set of run-scoped values (`run.id`, `run.store_dir`). Both namespaces are also usable inside compute expressions and the quoted `when` form.

## Terminal Tokens

```ebnf
IDENT        = LETTER { LETTER | DIGIT | "_" } ;
STRING_LIT   = '"' { CHAR } '"' ;
INT_LIT      = DIGIT { DIGIT } ;
FLOAT_LIT    = DIGIT { DIGIT } "." DIGIT { DIGIT } ;
NUMBER_LIT   = INT_LIT | FLOAT_LIT ;
BOOL_LIT     = "true" | "false" ;
NEWLINE      = "\n" ;
INDENT       = (* increase in indentation level *) ;
DEDENT       = (* decrease in indentation level *) ;
```

## Reserved Keywords

`vars`, `prompt`, `schema`, `agent`, `judge`, `router`, `human`, `tool`, `compute`, `workflow`, `entry`, `mcp`, `mcp_server`, `budget`, `compaction`, `worktree`, `model`, `backend`, `default_backend`, `input`, `output`, `publish`, `system`, `user`, `session`, `tools`, `tool_policy`, `tool_max_steps`, `reasoning_effort`, `readonly`, `interaction`, `interaction_prompt`, `interaction_model`, `await`, `mode`, `instructions`, `min_answers`, `command`, `expr`, `multi`, `transport`, `args`, `url`, `auth`, `type`, `auth_url`, `token_url`, `revoke_url`, `client_id`, `scopes`, `autoload_project`, `inherit`, `servers`, `disable`, `threshold`, `preserve_recent`, `when`, `not`, `as`, `with`, `enum`, `fresh`, `fork`, `artifacts_only`, `fan_out_all`, `condition`, `round_robin`, `llm`, `wait_all`, `best_effort`, `none`, `human`, `llm_or_human`, `auto`, `done`, `fail`, `true`, `false`, `string`, `bool`, `int`, `float`, `json`, `string[]`, `max_parallel_branches`, `max_duration`, `max_cost_usd`, `max_tokens`, `max_iterations`, `low`, `medium`, `high`, `xhigh`, `max`, `stdio`, `http`, `sse`, `oauth2`.

The `delegate` keyword from earlier drafts has been removed — use `backend:` everywhere it was used (delegation backends are selected by name: `claw`, `claude_code`, `codex`).
