---
name: iterion-dsl-quickref
description: Iterion DSL cheatsheet — load this only when whats-next.bot's next_action recommends writing or modifying a .iter / .bot / .botz workflow.
---

# Iterion DSL Quickref — for whats-next.bot's `emit_action` and the rare DSL-writing recommendation

Load this skill only in these two situations:

1. **`emit_action` is about to recommend authoring a new `.iter` /
   `.bot`** (rare — most next_actions invoke an existing bot,
   not author a new one).
2. **`propose_roadmap` / `revise_roadmap` is considering a
   recommendation that would mutate an existing workflow file**
   (also rare — the existing bots cover most needs).

In the common path (recommend running an existing bot from
`[[iterion-bot-catalog]]`), you do NOT need this skill.

Source of truth: `docs/dsl.md` + `docs/references/dsl-grammar.md`.
Re-read those if you're uncertain — this file is a navigation
aid, not the spec.

## Top-level blocks

```iter
vars:
  feature_prompt: string
  workspace_dir:  string = "${PROJECT_DIR}"

schema verdict:
  approved:   bool
  summary:    string
  confidence: string

prompt my_system:
  Imperative-voice instructions. Reference {{vars.feature_prompt}}
  or {{input.field}} or {{outputs.upstream_node.field}}.

agent worker:
  backend: "claw"
  model:   "openai/gpt-5.5"
  ...

workflow my_workflow:
  entry: worker
  worker -> done
```

## Node types

| Type | Use | Notes |
|---|---|---|
| `agent` | LLM with tools and structured I/O | Most common |
| `judge` | LLM verdict, no mutation | Tools optional |
| `router` | Branch selection | Modes: `fan_out_all`, `condition`, `round_robin`, `llm` |
| `join` | Branch convergence | `wait_all` or `best_effort` |
| `human` | Pause for human input | `interaction: human | llm | llm_or_human` |
| `tool` | Deterministic shell | No LLM; uses `{{input.x}}` templates with auto shell-escape |
| `compute` | Deterministic expression | No LLM, no shell. Use for passthrough, derived booleans, loop guards. |
| `done` / `fail` | Built-in terminals | Never declare them |

## Agent/judge properties

```iter
agent w:
  backend: "claw"               # or "claude_code"; avoid "codex" (C030)
  model:   "openai/gpt-5.5"     # claw with openai/* prefix
  reasoning_effort: high        # low | medium | high | xhigh | max
  input:   request_schema
  output:  result_schema
  system:  w_system
  user:    w_user
  session: fresh                # fresh | inherit | fork
  tools:   [bash, read_file, glob, grep, write_file, file_edit]
  tool_max_steps: 30
  readonly: true                # runtime-blocks mutation tools
  interaction: human            # surfaces ask_user via MCP
```

Backend rules:
- `openai/*` models MUST use `backend: "claw"`.
- `claude_code` only for nodes that need the native Skill tool
  or Claude Code-specific MCP servers.
- `claw` and `claude_code` BOTH use snake_case tool names.

## Edges

```iter
src -> dst                                        # unconditional
src -> dst when approved                          # bool field on src.output
src -> dst when not approved
src -> dst when "!approved && len(blockers)>0"   # expression
src -> dst as loop_name(10)                       # bounded loop
src -> dst with { field: "{{outputs.src.x}}" }    # data mapping
```

Rules:
1. Every cycle MUST be bounded (`as name(N)`).
2. Conditional edges must be exhaustive (or have an unconditional fallback).
3. Edge `with {}` values MUST be strings — int/bool literals fail with E002. Use `"true"` / `"0"` if needed, then coerce in compute.
4. Edge order matters for conditional fallthrough.

## Template references

| Form | Meaning |
|---|---|
| `{{vars.x}}` | workflow var |
| `{{input.field}}` | this node's input |
| `{{outputs.id}}` / `{{outputs.id.field}}` | upstream node output |
| `{{outputs.id.history}}` | array across loop iterations |
| `{{loop.name.iteration}}` | current loop count |
| `{{loop.name.previous_output}}` | last iter's output of the loop's tail |
| `{{artifacts.name}}` | published artifact |
| `${ENV_VAR}` | compile-time env substitution |

`{{...}}` is parsed in every prompt block. Even literal examples
inside markdown code-fences trigger validation. Avoid example
strings like `{{vars.x}}` in prompts — describe them in prose
instead.

## Compute passthrough pattern

When you need to thread a value through a human node or
across a loop boundary:

```iter
schema carry:
  payload: json

compute pass_through:
  input:  carry
  output: carry
  expr:
    payload: "input.payload"
```

`expr:` values are quoted expressions (CEL-like), NOT templates.
Reference `input.x`, `outputs.x.y`, `loop.name.previous_output.x`
directly without `{{...}}`.

## Workflow block

```iter
workflow my_wf:
  entry: first_node

  budget:
    max_parallel_branches: 1
    max_duration: "1h"
    max_cost_usd: 10
    max_iterations: 30

  ## Edges go here
  first_node -> done
```

## Worktree and sandbox

```iter
workflow safe:
  worktree: auto                # iterion creates a fresh git worktree
  sandbox:  auto                # reads .devcontainer/devcontainer.json
  entry:    first_node
```

Sandbox modes: `auto`, `none`, or a block form (`image:`,
`network: {mode: ...}`, `user:`).

## When you really do need to author DSL

The whats-next pipeline almost never needs to author DSL. If
`emit_action` is genuinely about to recommend a new `.bot` file:

1. Check that none of the three existing bots
   (`vibe_feature_dev`, `vibe_review_alternating`,
   `secured-renovacy`) covers the use case. Usually one does.
2. If a new bot really is needed, the `next_action` should be
   "manually author a new bot at `examples/<slug>/bot.bot`"
   (with `bot_to_run="none"`) — NOT "auto-invoke
   `iterion run` on a non-existent file".
3. Record the desired bot shape in the plan markdown's "Next
   action" section so a human (or a future bot) can pick it up.

## What you do NOT do

- You do NOT recommend `bot_to_run` as the path of a `.bot` file
  that does not yet exist.
- You do NOT inline DSL examples that contain `{{...}}` — they
  break iterion's prompt validator.
- You do NOT use `delegate:` (the legacy field name). Use
  `backend:`.
