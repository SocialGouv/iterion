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
                | "url"       ":" STRING_LIT         NEWLINE ;

mcp_transport = "stdio" | "http" | "sse" ;
```

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
           | "delegate"           ":" STRING_LIT              NEWLINE
           | "input"              ":" IDENT                   NEWLINE
           | "output"             ":" IDENT                   NEWLINE
           | "publish"            ":" IDENT                   NEWLINE
           | "system"             ":" IDENT                   NEWLINE
           | "user"               ":" IDENT                   NEWLINE
           | "session"            ":" session_mode             NEWLINE
           | "tools"              ":" tool_list                NEWLINE
           | "tool_max_steps"     ":" INT_LIT                 NEWLINE
           | "reasoning_effort"   ":" reasoning_effort_value   NEWLINE
           | "readonly"           ":" BOOL_LIT                NEWLINE
           | "interaction"        ":" interaction_mode          NEWLINE
           | "interaction_prompt" ":" IDENT                    NEWLINE
           | "interaction_model"  ":" STRING_LIT               NEWLINE
           | "await"              ":" await_mode                NEWLINE
           | "mcp"                ":" NEWLINE INDENT { mcp_config_prop } DEDENT ;
```

## Judge

```ebnf
judge_decl = "judge" IDENT ":" NEWLINE INDENT { judge_prop } DEDENT ;
```

Judge properties are identical to agent properties. Semantically, judges produce verdicts and typically don't use tools.

## Router

```ebnf
router_decl = "router" IDENT ":" NEWLINE INDENT { router_prop } DEDENT ;

router_prop = "mode"   ":" router_mode   NEWLINE
            | "model"  ":" STRING_LIT    NEWLINE    (* llm mode only *)
            | "system" ":" IDENT         NEWLINE    (* llm mode only *)
            | "user"   ":" IDENT         NEWLINE    (* llm mode only *)
            | "multi"  ":" BOOL_LIT      NEWLINE ;  (* llm mode only *)

router_mode = "fan_out_all" | "condition" | "round_robin" | "llm" ;
```

Routers do NOT support `await`. Properties `model`, `system`, `user`, `multi` are only valid with `mode: llm` (diagnostic C023 otherwise).

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
               | "output"  ":" IDENT       NEWLINE
               | "await"   ":" await_mode   NEWLINE ;
```

## Workflow

```ebnf
workflow_decl = "workflow" IDENT ":" NEWLINE INDENT
                  [ workflow_vars ]
                  workflow_entry
                  [ workflow_mcp ]
                  [ workflow_budget ]
                  [ workflow_interaction ]
                  { edge | comment }
                DEDENT ;

workflow_vars  = "vars" ":" NEWLINE INDENT { var_field } DEDENT ;
workflow_entry = "entry" ":" IDENT NEWLINE ;

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

workflow_interaction = "interaction" ":" interaction_mode NEWLINE ;
```

## Edges

```ebnf
edge = IDENT "->" IDENT [ when_clause ] [ loop_clause ] [ with_block ] NEWLINE ;

when_clause = "when" [ "not" ] IDENT ;
loop_clause = "as" IDENT "(" INT_LIT ")" ;
with_block  = "with" "{" NEWLINE { with_mapping } "}" ;
with_mapping = IDENT ":" STRING_LIT NEWLINE ;
```

## Shared Enumerations

```ebnf
session_mode           = "fresh" | "inherit" | "fork" | "artifacts_only" ;
await_mode             = "wait_all" | "best_effort" ;
interaction_mode       = "human" | "llm" | "llm_or_human" ;
reasoning_effort_value = "low" | "medium" | "high" | "extra_high" ;
```

## Template Expressions

Inside `STRING_LIT` in with-blocks and prompt bodies:

```ebnf
template_expr = "{{" template_ref "}}" ;

template_ref = "vars"      "." IDENT
             | "input"     "." IDENT
             | "outputs"   "." IDENT [ "." IDENT ]
             | "artifacts" "." IDENT ;
```

Special: `{{outputs.node_id.history}}` returns the array of all outputs from a node across loop iterations. Only valid if the node is in a declared loop.

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

`vars`, `prompt`, `schema`, `agent`, `judge`, `router`, `human`, `tool`, `workflow`, `entry`, `mcp`, `mcp_server`, `budget`, `model`, `input`, `output`, `publish`, `system`, `user`, `session`, `tools`, `tool_max_steps`, `reasoning_effort`, `readonly`, `interaction`, `interaction_prompt`, `interaction_model`, `await`, `delegate`, `mode`, `instructions`, `min_answers`, `command`, `multi`, `transport`, `args`, `url`, `autoload_project`, `inherit`, `servers`, `disable`, `when`, `not`, `as`, `with`, `enum`, `fresh`, `fork`, `artifacts_only`, `fan_out_all`, `condition`, `round_robin`, `llm`, `wait_all`, `best_effort`, `done`, `fail`, `true`, `false`, `string`, `bool`, `int`, `float`, `json`, `string[]`, `max_parallel_branches`, `max_duration`, `max_cost_usd`, `max_tokens`, `max_iterations`, `low`, `medium`, `high`, `extra_high`.
