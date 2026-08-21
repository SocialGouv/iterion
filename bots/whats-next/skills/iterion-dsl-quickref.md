---
name: iterion-dsl-quickref
description: Iterion DSL cheatsheet — load this only when whats-next.bot's next_action recommends writing or modifying a .bot / .botz workflow.
---

# Iterion DSL Quickref — for whats-next.bot's `emit_action` and the rare DSL-writing recommendation

Load this skill only in these two situations:

1. **`emit_action` is about to recommend authoring a new `.bot` /
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
  mode: string [enum: "autonomous", "interview"] = "autonomous"  # string-only closed value set; default + launch values validated (C125-C127)

secrets:                            # optional; agent sees only an opaque placeholder
  github_token: "${GITHUB_TOKEN}"   #   __ITERION_SECRET_github_token__, materialised at exec
  deploy_key:
    value: "${DEPLOY_KEY}"
    hosts: ["api.github.com"]        # egress scoping (Layer 2). Reference as {{secrets.deploy_key}}
  kubeconfig:                        # FILE secret: mounted read-only in the sandbox; agent gets the path
    as: file                        #   {{secrets.kubeconfig}} renders /run/iterion/secrets/kubeconfig
    env: KUBECONFIG                 #   optional env var pointing at the file
    optional: true                  #   skip the mount (no error) when unresolved

schema verdict:
  approved:   bool
  summary:    string
  confidence: string

prompt my_system:
  Imperative-voice instructions. Reference {{vars.feature_prompt}}
  or {{input.field}} or {{outputs.upstream_node.field}}.

cursor ambition:                  # optional prompt-engineering dial (see docs/cursors.md)
  values:
    cautious: "Stick to the stated request."
    ambitious: "Surface 2-3 adjacent improvements."

agent worker:
  backend: "claw"
  model:   "openai/gpt-5.5"
  ...

workflow my_workflow:
  entry: worker
  compress: on                    # optional: compress agent shell output run-wide (on|ultra|off)
  auto_memory: on                 # optional: let agent/judge nodes keep a MEMORY.md across runs
                                  # (on|off, OFF by default — a run is hermetic unless it opts in)
  worker -> done
```

## Node types

| Type | Use | Notes |
|---|---|---|
| `agent` | LLM with tools and structured I/O | Most common |
| `judge` | LLM verdict, no mutation | Tools optional |
| `router` | Branch selection | Modes: `fan_out_all`, `fan_out_each`, `condition`, `round_robin`, `llm` |
| `human` | Pause for human input | `interaction: human | llm | llm_or_human | review` (`async` is agent/judge-only — C240) |
| `tool` | Deterministic shell | No LLM; uses `{{input.x}}` templates with auto shell-escape |
| `compute` | Deterministic expression | No LLM, no shell. Use for passthrough, derived booleans, loop guards. |
| `emit` | Publish a run-scoped event | `event: "<name>"` + optional `with { k: "{{ref}}" }` payload. No LLM, no shell (ADR-051). |
| `wait` | Block a branch until an event fires | `event: "<name>"` + **mandatory** `timeout: "30s"` (the bornage, C197) + optional `output:` schema for the payload. Pair with `emit` in a parallel `fan_out_all` branch for reactive coordination. |
| `await_answers` | Sync point for async human questions (ADR-081) | Optional `from: <node>` + **mandatory** `timeout:` (C241). Parks its branch until every pending `ask_user_async` question is answered; output is `{answers: [...]}`. The asking agent declares `interaction: async` (grants `ask_user_async` + `await_answers` tools — the agent keeps working while questions are pending, answers arrive in its message queue). |
| `subbot` | Run another `.bot` as a nested run | `source:` + `with { ... }` + `output:`; child may contain loops |
| `done` / `fail` | Built-in terminals | Never declare them |

Every declarable node kind accepts an optional `description: "…"` — a human-readable
label the run console shows instead of the humanized node id (the raw id stays
available as tooltip/suffix).

## Reuse & iteration (see docs/groups-iteration-subbots.md)

- **`group <name>(params):` + `use <group> as <prefix> with { p: "v" }`** — reusable node
  cluster (compile-time macro). Instance nodes are addressed `prefix.node`; `{{params.X}}`
  is substituted from the bindings.
- **`router mode: fan_out_each` + `over:`/`as:`/`key:`/`depends_on:`** — one parallel branch
  per element of a collection, topologically scheduled by `key`/`depends_on`; element exposed
  as `{{outputs.<router>.<as>.<field>}}`. `await: best_effort` on the convergence node.
  A loop or `as foreach` **inside** that template (or a `fan_out_all` / llm `multi` branch)
  is C244 — use a `subbot` for per-item retry, or wrap the router from the join.
- **`src -> dst as foreach name(item in "{{coll}}")`** — ordered, stateful iteration; element
  via `{{each.name.item|index|count|first|last|empty}}`. Trunk only (C244 inside parallel branches).
- **`resources:` (counting or `["a","b"]` lease pool) + node `needs:`** — bound the concurrency
  of an operation independent of `max_parallel_branches`.

## Agent/judge properties

```iter
agent w:
  backend: "claw"               # or claude_code / codex / pi / kimi / grok
  model:   "openai/gpt-5.5"     # claw with openai/* prefix
  reasoning_effort: high        # low | medium | high | xhigh | max | ultracode
                                # ultracode = xhigh + multi-agent orchestration prerogative;
                                # reliable only on claude-opus-4-8 (else warns C089, runs as xhigh)
  input:   request_schema
  output:  result_schema
  system:  w_system
  user:    w_user
  session: fresh                # fresh | inherit | inherit_if_available | fork | artifacts_only | persist
  tools:   [bash, read_file, glob, grep, write_file, file_edit]
  tool_max_steps: 30
  max_tokens: 4096              # output cap (per LLM call)
  compress: on                  # opt-in command-output compression (on|ultra|off, off by default);
                                # rewrites shell commands via the rtk plugin to save 60-90% output tokens (docs/plugins.md)
  auto_memory: on               # a persistent MEMORY.md (on|off, off by default). One iterion-owned space
                                # per (bot, repo) that claude_code, claw and pi all read/write, persisted
                                # through the memory store so it survives a worktree or a cloud pod.
                                # Distinct from the memory: block below. C132 warns on other backends.
  readonly: true                # runtime-blocks mutation tools
  interaction: human            # surfaces ask_user via MCP
  interaction_prompt: ask_msg   # used when interaction is llm or llm_or_human
  interaction_model: "openai/gpt-5.5"
  capabilities: [board.read, board.create, board.move]   # opens MCP-gated tools
  skills: ["changelog-writer", "house-style"]   # skill-library refs (quote kebab names);
                                # mirrored into .claude/skills/ + listed under a ## Skills
                                # prompt section (also settable workflow-wide). docs/skills-library.md
  # watch.subscribe / watch.unsubscribe (claw backend): mcp.iterion_watch.*
  # — subscribe a run to a board issue; the runtime queues a message to the
  #   run whenever that issue changes state (track dispatched tickets)
  await: wait_all               # only when the node has multiple incoming edges
  compaction:                   # model-aware compaction (per-node override)
    threshold: 0.9              # fraction of context window
    preserve_recent: 8          # keep last N turns verbatim
  mcp:                          # node-scoped MCP servers
    inherit: true               # inherit workflow-level servers
    servers: []                 # plus these
  cursors:                      # prompt-engineering calibration (see docs/cursors.md)
    enabled: true
    ambition: ambitious         # enum value declared in `cursor ambition:` above
    depth: 0.7                  # numeric → matched against `bands:` declarations
```

Backend rules:
- `openai/*` models MUST use `backend: "claw"`.
- `claude_code` only for nodes that need the native Skill tool
  or Claude Code-specific MCP servers.
- `claw` and `claude_code` BOTH use snake_case tool names.
- `provider:` routes credentials per node. It accepts a single
  hint (`anthropic`/`zai`/`openai`/`auto`) OR an ordered fallback
  chain (`provider: "zai,anthropic"`): on a hard failure beyond
  retries the runtime falls through to the next provider
  transparently (generalises `RESCUE_PROVIDER`). Each element may
  pin its own model with `provider:model`
  (`provider: "zai:glm-5.2,anthropic:claude-opus-4-8"`) so the
  fall-through swaps BOTH the hint and the wire model; a model-less
  element inherits the node `model:`. Honoured by `claude_code`
  (same-API family); `claw`/`codex` use only the first hint
  (compiler warns C088). Malformed `provider:model` warns C172.
  Single values are unchanged.
- `fallbacks:` declares complete alternative ROUTES (backend + model +
  hint) for what `provider:` cannot do: continuing on another backend
  when a CLI forfait's window shuts. Named entries, declaration order =
  try order:

  ```
  agent implement:
    backend: "claude_code"
    model: "claude-opus-5"
    tools: [read_file, run_command]
    fallbacks:
      api:
        backend: "claw"
        model: "anthropic/claude-opus-5"
        on: [usage_window]
  ```

  `on:` defaults to `[usage_window, unavailable]` — NOT `any` (a budget
  cap re-fails identically everywhere) and NOT `auth` (a dead credential
  deliberately pauses for a human). An unclassifiable failure always
  routes. A fall-through emits a `model_fallback` event and stamps
  `_fallback_used` / `_served_by` on the output, so a deterministic gate
  can fail closed on a degraded input. Errors: a route changing
  `backend:` must pin its own `model:` (C173); a route that cannot
  enforce the node's `permission:` gate, or that crosses the claw⇄CLI
  boundary with an empty `tools:` list, is refused (C176). See ADR-087.

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
- `persist` — resume **this** node's last conversation on
  re-entry (ADR-089). Trunk-only (C243); CLI backends with a
  packed StateRef. Not inherit-from-parent.

## Edges

```iter
src -> dst                                        # unconditional
src -> dst when approved                          # bool field on src.output
src -> dst when not approved
src -> dst when "!approved && length(blockers) > 0"   # expression
src -> dst else                                   # explicit fallback: fires only when no sibling `when` matched
src -> dst as loop_name(10)                       # bounded loop (literal cap — UNQUOTED int)
src -> dst as loop_name("{{outputs.x.cap}}")      # bounded loop (data-driven cap; quote ONLY a template)
src -> dst as loop_name(unbounded)                # unbounded: runs until a when-exit; fuel from budget.max_iterations
src -> dst as loop_name(unbounded 500)            # unbounded with a per-loop fuel ceiling
src -> dst with { field: "{{outputs.src.x}}" }    # data mapping
```

Rules:
1. Every cycle MUST be bounded — `as name(N)`, a data-driven cap, or `as name(unbounded)`.
   `unbounded` needs a fuel ceiling (per-loop `(unbounded N)` or `budget.max_iterations`) — else C097.
   A runtime liveness monitor also halts an unbounded loop that makes no progress (fixpoint).
   A literal cap is an UNQUOTED int — `as fix(2)`. Quotes mean a template
   (`as fix("{{vars.cap}}")`); a quoted plain int `as fix("2")` is read as
   the int 2, but a quoted non-numeric cap `as fix("two")` is an E002 error.
2. Conditional edges must be exhaustive (or have an unconditional fallback).
3. Edge `with {}` values MUST be strings — int/bool literals fail with E002. Use `"true"` / `"0"` if needed, then coerce in compute.
4. Edge order matters for conditional fallthrough.
5. `as <loop>` / `as foreach` cannot originate inside a `fan_out_all`, `fan_out_each`,
   or llm `multi: true` body (**C244**). The walk stops at structural joins; a loop
   head elected only by its own back-edge stays in the body. A loop after a
   non-elected `await:` is not claimed. Wrap from the join, or use a `subbot`.

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
- `interaction: review` — a guided review-&-merge gate: a
  companion LLM walks the human through testing the change via a
  multi-turn dialogue, then **squash-merges the run's worktree
  during the pause** when approved. Requires `worktree: auto`
  (C100). See below.

### Collecting a file from the operator

A `file`-typed schema field renders a drop zone at the gate; the
operator's upload becomes a run attachment and the answer is a
descriptor (`path` / `filename` / `mime` / `size` / `sha256`).

```iter
schema music_gate:
  approved: bool
  music: file
  notes: string

human pick_soundtrack:
  output: music_gate
```

```iter
prompt mix:
  Master the track at {{outputs.pick_soundtrack.music.path}}
```

- Only a node that PAUSES for an operator may declare a `file` field —
  no LLM can produce bytes. Compile error (**C129**) on any non-human
  node, and on a `human` node with `interaction: llm` (auto-answered, so
  the model would invent the path) or `interaction: review` (output is
  the engine-built verdict). `llm_or_human` is fine: it can escalate to
  a real pause. A gate must not declare `_attachments` either — the
  engine owns that key (**C130**).
- `file` fields are optional; branch downstream if the workflow cannot
  proceed without one.
- Every ORDINARY human gate also has an unconditional "attach a file"
  button requiring no DSL; those land on `_attachments` as a list of the
  same descriptors. `interaction: review` gates are the exception — no
  attach button, and their resume builds the verdict instead of carrying
  answers, so `_attachments` never reaches the workflow there.
- CLI: `--answer music=@./theme.mp3`. The `@` convention applies only to
  `file`-typed fields — other answers pass through verbatim; inside a
  file field, `@@` escapes a literal `@`.

### Review-&-merge gate (`interaction: review`)

```iter
human ship_review:
  interaction: review
  model: "anthropic/claude-sonnet-4-6"   # the companion (writes test steps + verdict)
  system: companion_system               # companion contract prompt
  output: review_verdict                 # decision/confidence/blockers — routes downstream edges
  review_url: "{{outputs.provision.url}}" # optional: env to open & test (studio Browser pane)
  posture: human_required                # human_required (default) | agent_verdict_ok
  merge_strategy: squash                 # squash (default) | merge
  merge_into: current                    # current (default) | none | <branch>
  max_turns: 8                           # dialogue asymptote backstop
```

```iter
ship_review -> done   when "decision == 'approved'"
ship_review -> implement when "decision == 'changes_requested'" as fix_loop(5)
ship_review -> fail    # default fallback
```

The operator (studio) can: reply to continue the dialogue,
**Approve & merge**, **Force-merge** (skips the verdict; git
guards still apply), or **Request changes** (routes the
`changes_requested` edge). With `posture: agent_verdict_ok` a
high-confidence companion approval auto-merges without a click.
The gate requires `worktree: auto` — it squash-merges the run's
commits when approved. Reference: `examples/review-merge-gate.bot`,
`docs/review-merge-gate.md`.

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

Add `publish: <name>` to a `tool` (or `compute`, or agent/human)
node to persist its output as a versioned artifact — surfaced in the
studio Artifacts tab and `iterion report`, referenceable downstream as
`{{artifacts.<name>}}`. Deterministic, no LLM cost: `publish:` only
redirects the already-computed output into the store.

Optionally add `artifact_labels: [plan, ...]` alongside `publish:` to
categorise the artifact, so the studio's Artifacts tab groups it (e.g. a
"Plans" group). Plan-shaped (`plan`/`text` body) and verdict-shaped
(`approved`/`blockers`/…) outputs are auto-labelled `plan`/`verdict` even
without the field. Diagnostic C049 warns if `artifact_labels:` is set
without `publish:` (nothing to attach to).

### Verified Action — adaptive recovery for ACTION tool nodes (ADR-044)

An ACTION tool node (commit, git ops, file writes, scanner-invoke) can
opt into a recovery ladder so a brittle recipe self-heals instead of
hard-blocking. Add the optional quad — `goal` + recipe (`command`/`script`)
+ `postcondition` + `policy`:

```iter
tool commit_changes:
  command: `git add -A && git commit -F - <<< {{input.msg}}`
  goal: "Commit the upgrade; working tree clean except known caches."
  postcondition: `cd {{input.workspace_dir}} && ! git status --porcelain | grep -q . && printf '{"sha":"%s"}' "$(git rev-parse HEAD)"`
  policy: recover            # required | recover | best_effort
  recovery:
    max_repair_attempts: 2   # rung 3 (self-repair) bound
    max_agent_attempts: 0    # rung 4 (agent recovery) — OFF by default, opt-in
    model: "anthropic/claude-sonnet-4-6"   # recovery LLM (default: sonnet)
```

Runtime ladder (the **postcondition is the single source of truth** at
every rung — success keys on it, **never on the exit code**, because exit
codes lie: "nothing to commit" exits 1 though the goal may already hold):
1. **idempotent skip** — postcondition already met? skip the recipe (resume-safe).
2. **recipe** — run it; postcondition met → done (~95% path).
3. **self-repair** (`policy: recover`) — an LLM proposes a corrected command
   from `{goal, recipe, stdout, stderr}`; runtime re-runs it deterministically
   (the corrected command is recorded as a `tool_called` event — auditable).
4. **agent recovery** (`recover` + `max_agent_attempts > 0`) — an agent
   achieves the goal with real tools. Opt-in.
5. **policy** — still unmet: `required`/`recover` → fail (resumable);
   `best_effort` → warn + continue.

The postcondition's JSON stdout becomes the node output, so downstream
`{{outputs.<id>.field}}` is populated on every rung (incl. skip).

GATES STAY DETERMINISTIC. A verification gate (`scan_health`, `streak_check`,
coverage) is the degenerate quad: `recipe == postcondition`, no recovery,
`policy: required`. **Never attach LLM recovery to a gate** — that
reintroduces the façade risk the gate exists to prevent. The compiler
enforces this: **C103** invalid policy · **C104** recovery without a
postcondition · **C105** recovery on a gate (`recipe == postcondition`) ·
**C106** recovery bounds under a non-`recover` policy. A node with no
`postcondition` behaves exactly as before (recipe only, exit code = success).

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

### Expression operators & builtins

- Operators: `&& || ! == != < <= > >= + - * / %` (and `and`/`or`/`not`).
- Indexing: `arr[0]`, `m["key"]`, `people[0].name` (OOB / missing key → nil).
- Builtins: `length`, `concat`, `unique`, `contains`, `join`, `tail`,
  `if(cond, then, else)`, `sort`, `keys`, `values`, `slice(arr, start, end)`,
  `sum`, `min`, `max`, `flatten`.
- Bounded combinators (lambda, applied once per element of a finite list):
  - `map(arr, x => x.field)`
  - `filter(arr, x => x.score > 5)`
  - `reduce(arr, 0, (acc, x) => acc + x.score)`
  These are total (no recursion / no fixpoint); a per-eval visit budget bounds
  deeply nested combinators.

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
   `branch_improve_loop`, `secured-renovacy`, `docs-refresh`)
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
