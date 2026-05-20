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
  session: fresh                # fresh | inherit | inherit_if_available | fork | artifacts_only
  tools:   [bash, read_file, glob, grep, write_file, file_edit]
  tool_max_steps: 30
  max_tokens: 4096              # output cap (per LLM call)
  readonly: true                # runtime-blocks mutation tools
  interaction: human            # surfaces ask_user via MCP
  interaction_prompt: ask_msg   # used when interaction is llm or llm_or_human
  interaction_model: "openai/gpt-5.5"
  capabilities: [board.read, board.create, board.move]   # opens MCP-gated tools
  await: wait_all               # only when the node has multiple incoming edges
  compaction:                   # model-aware compaction (per-node override)
    threshold: 0.9              # fraction of context window
    preserve_recent: 8          # keep last N turns verbatim
  mcp:                          # node-scoped MCP servers
    inherit: true               # inherit workflow-level servers
    servers: []                 # plus these
```

Backend rules:
- `openai/*` models MUST use `backend: "claw"`.
- `claude_code` only for nodes that need the native Skill tool
  or Claude Code-specific MCP servers.
- `claw` and `claude_code` BOTH use snake_case tool names.

Session-mode notes:
- `fresh` (default) — new context every call.
- `inherit` — hard-requires `_session_id` to resolve on the
  input. Fails if absent. Use when the upstream node is
  guaranteed to be the same backend and same model.
- `inherit_if_available` (v0.6.0+) — same as `inherit` but
  silently falls back to `fresh` when no parent session
  exists. Safe across loop boundaries where the first
  iteration has no parent.
- `fork` — clones the parent session but diverges from it.
- `artifacts_only` — pulls upstream artifacts but no
  conversation history.

## Edges

```iter
src -> dst                                        # unconditional
src -> dst when approved                          # bool field on src.output
src -> dst when not approved
src -> dst when "!approved && length(blockers) > 0"   # expression
src -> dst as loop_name(10)                       # bounded loop
src -> dst with { field: "{{outputs.src.x}}" }    # data mapping
```

Rules:
1. Every cycle MUST be bounded (`as name(N)`).
2. Conditional edges must be exhaustive (or have an unconditional fallback).
3. Edge `with {}` values MUST be strings — int/bool literals fail with E002. Use `"true"` / `"0"` if needed, then coerce in compute.
4. Edge order matters for conditional fallthrough.

## Human node

```iter
human ask_priorities:
  input:  ask_schema
  output: ask_schema
  instructions: ask_priorities_prompt    # shown to the human
  interaction: human                     # human | llm | llm_or_human
  interaction_prompt: ask_priorities_llm # prompt used in llm-auto mode
  interaction_model: "openai/gpt-5.5"    # model used in llm-auto mode
  min_answers: 1
```

- `interaction: human` (default for `human` nodes) — pauses
  the run until the operator answers.
- `interaction: llm` — auto-answers using `interaction_model`
  + `interaction_prompt`, no human pause.
- `interaction: llm_or_human` — LLM tries first; if it sets
  `_escalate=true` the run pauses for human input.

## Tool node

```iter
tool commit_changes:
  command: sh
  args: ["-c", "git add -A && git commit -m {{input.msg}}"]
  readonly: false                # opt-out of workspace-safety read-only mode
  await: wait_all                # only when the node has multiple incoming edges
```

Tool commands run via `sh -c` (POSIX). Template substitutions
auto-escape strings, but `string[]` substitutions split into
multiple argv tokens — use positional argv + `--` sentinels
when passing multi-element arrays.

## Template references

| Form | Meaning |
|---|---|
| `{{vars.x}}` | workflow var |
| `{{input.field}}` | this node's input |
| `{{outputs.id}}` / `{{outputs.id.field}}` | upstream node output |
| `{{outputs.id.history}}` | array across loop iterations |
| `{{loop.<name>.iteration}}` | current loop count |
| `{{loop.<name>.previous_output}}` | last iter's output of the loop's tail |
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
Reference `input.x`, `outputs.x.y`, `loop.<name>.previous_output.x`
directly without `{{...}}`.

## Workflow block

```iter
workflow my_wf:
  entry: first_node
  default_backend: "claude_code"      # default backend for every node
  interaction: llm_or_human           # workflow-wide escalation policy
  tool_policy: [bash, read_file]      # default tool policy applied to all nodes

  budget:
    max_parallel_branches: 1
    max_duration: "1h"
    max_cost_usd: 10
    max_tokens: 1000000
    max_iterations: 30

  compaction:                         # workflow-wide compaction default
    threshold: 0.9
    preserve_recent: 8

  mcp:                                # workflow-wide MCP server registry
    servers:
      - name: my_server
        transport: stdio
        command: my-mcp-server
        args: []

  worktree: auto                      # see "Worktree and sandbox" below
  sandbox:  auto

  ## Edges go here
  first_node -> done
```

## Worktree and sandbox

```iter
workflow safe:
  worktree: auto                      # fresh git worktree per run
  sandbox:  auto                      # reads .devcontainer/devcontainer.json
  entry:    first_node
```

Block-form sandbox:

```iter
workflow isolated:
  sandbox:
    image: "ghcr.io/socialgouv/iterion-sandbox-slim:v0.13"
    # or build:
    #   dockerfile: "Dockerfile.sandbox"
    #   context: "."
    #   args: { BASE: "alpine:3.20" }
    user: "1000:1000"
    network:
      mode: allowlist                 # allowlist | inherit | none
      preset: default                 # LLM + npm/pypi/golang + git hosts
      inherit: false                  # add to (not replace) the preset
      rules:
        - host: "registry.example.com"
          port: 443
```

Sandbox top-level modes: `auto`, `none`, or the block form
above. `network.preset: default` already covers LLM
endpoints, npm/pypi/golang/cargo, github/gitlab/bitbucket
and the Nix cache — only add `rules:` for private hosts.

## When you really do need to author DSL

The whats-next pipeline almost never needs to author DSL. If
`emit_action` is genuinely about to recommend a new `.bot` file:

1. Check that none of the five existing bots
   (`feature_dev`, `whole_improve_loop`,
   `branch_improve_loop`, `secured-renovacy`, `doc-align`)
   covers the use case. Usually one does.
2. If a new bot really is needed, the `next_action` should be
   "manually author a new bot at `examples/<slug>/main.bot`"
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
