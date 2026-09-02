# 🧩 The `.bot` DSL

**Agent workflows, as code.** Define readable, versioned workflows in a declarative, indentation-significant language.

Source files end in `.bot`; deterministic bundles end in `.botz`.

This page is the language guide. For exact accepted syntax use the [readable grammar](references/dsl-grammar.md), the [formal EBNF](grammar/iterion_v1.ebnf), and the [diagnostic catalogue](references/diagnostics.md). The parser, IR compiler, and validators under [`pkg/dsl/`](../pkg/dsl/) remain the implementation source of truth.

A `.bot` file travels through a fixed pipeline before it runs:

```mermaid
flowchart LR
  SRC(["📄 .bot source"]) --> LEX["🔤 Lexer<br/>indent-aware tokens"]
  LEX --> PAR["🌳 Parser<br/>recursive descent"]
  PAR --> AST["AST"]
  AST --> IR["🧩 IR compile<br/>nodes · edges · schemas"]
  IR --> VAL{{"✅ Validate<br/>C001–C199 diagnostics"}}
  VAL --> RUN(["⚙️ Runtime<br/>execute · budget · persist"])
```

## File shape

A file may contain these top-level declarations:

```text
vars, presets, attachments, secrets, mcp_server,
prompt, schema, cursor, supervisor,
agent, judge, router, human, tool, compute, emit, wait, await_answers, subbot,
group, use, workflow
```

Declarations may appear in any order subject to validation. `##` starts a comment. Values accept quoted strings, backtick-delimited raw strings, and `|` block scalars where the grammar expects a string.

## Inputs and reusable values

### Variables and presets

```iter
vars:
  project: string
  mode: string [enum: "autonomous", "interview"] = "autonomous"
  max_retries: int = 3
  verbose: bool = false
  threshold: float = 0.8
  config: json = "{\"key\":\"value\"}"
  tags: string[] = "[\"security\",\"performance\"]"

presets:
  quick:
    max_retries: 1
    verbose: true
```

Supported types are `string`, `bool`, `int`, `float`, `json`, and `string[]`. Only strings accept `[enum: ...]`; defaults and launch values must belong to the declared set. Runtime precedence is `--var` over `--preset`, recipe values, and declaration defaults. See [recipes](recipes.md).

A workflow may declare an additional `vars:` block. Top-level and workflow variables are merged during compilation.

### Attachments

```iter
attachments:
  specification: file
    description: "Product specification"
    accept_mime: ["application/pdf", "text/markdown"]
    required: true
  mockup: image
```

Attachments are uploaded/persisted inputs, not scalar vars. They are available as `{{attachments.specification}}`, `.path`, `.url`, `.mime`, `.size`, and `.sha256`. A workflow may also carry an `attachments:` block. See [attachments](attachments.md).

### Secrets

```iter
secrets:
  forge_token: "${FORGE_TOKEN}"
  deploy_key:
    value: "${DEPLOY_KEY}"
    as: file
    mount_path: "/run/iterion/secrets/deploy_key"
    env: "GIT_SSH_KEY_PATH"
    hosts: ["github.com", "api.github.com"]
    optional: false
    description: "SSH key used by the deploy step"
```

Value secrets render as opaque placeholders and are materialised only at execution sinks. File secrets render as their mounted path. A declaration without `value:` resolves by name from the local/cloud store. Use `{{secrets.forge_token}}` or `{{secrets.deploy_key.path}}`; undeclared references are compile errors. The protection layers, egress scoping, stores, and limitations are documented in [secrets](secrets.md) and the [secrets reference](secrets-reference.md).

## Prompts, schemas, and templates

### Prompts

```iter
prompt review_system:
  You are a reviewer for {{vars.project}}.

prompt review_user:
  Review:
  {{input.code}}
  Previous result: {{outputs.prior.summary}}
```

`{{include "relative/path.md"}}` inlines a file at compile time. Paths are relative to the `.bot`, may not escape its directory (including through symlinks), and are capped at 256 KiB. Included content may contain normal runtime templates.

### Schemas

```iter
schema review_request:
  code: string

schema review_result:
  approved: bool
  summary: string
  issues: string[]
  confidence: string [enum: "low", "medium", "high"]
  score: float
  metadata: json
```

Schemas define structured node inputs/outputs. Field types match variable types (`string`, `bool`, `int`, `float`, `json`, `string[]`); string fields may carry enum constraints. A seventh type, `file`, declares an operator-supplied binary and is valid only on a human node's `output_schema` — no model can produce one, so the compiler rejects it elsewhere with [C129](references/diagnostics.md). See [human-in-the-loop](human-in-the-loop.md).

### Template namespaces

| Reference | Meaning |
|---|---|
| `{{vars.name}}` | Resolved workflow variable. |
| `{{input.field}}` | Current node input (prompts, tool commands, compute exprs). On an edge `with` mapping: the **source node's output** — the payload available when the edge fires. A router copies its input to its output (an `llm` router also records `selected_route`/`selected_routes` and `reasoning` on that same map). An **entry** router’s input is the run payload; a **mid-graph** router only has what its incoming `with` mappings supplied (C032 if `{{input.x}}` names something else). Launch-time values use `{{vars.name}}`. |
| `{{outputs.node}}` / `.field` | Prior node output or a field within it. |
| `{{outputs.node.history}}` | Outputs accumulated across loop iterations. |
| `{{artifacts.name}}` | Published artifact. |
| `{{attachments.name}}` / `.path` / `.url` / `.mime` / `.size` / `.sha256` | Attachment metadata. |
| `{{secrets.name}}` / `.path` | Opaque value placeholder or mounted file-secret path. |
| `{{loop.name.iteration}}` / `.max` / `.previous_output` | Declared-loop state. |
| `{{each.name.item}}` / `.index` / `.count` / `.first` / `.last` / `.empty` | Sequential edge-`foreach` state. |
| `{{run.id}}` | Current run id. |
| `{{params.name}}` | `group` parameter during compile-time expansion. |

`fan_out_each` also exposes the current item as `{{outputs.<router>.<as-name>}}`. Environment expressions use `${NAME}` (and supported default forms) before execution. In a tool `command` or `script`, `{{!input.field}}` is the explicit raw-substitution form; ordinary `{{input.field}}` is shell-escaped. Use the raw form only when the value is intentionally executable shell syntax, because it crosses the command-injection boundary.

## LLM nodes: `agent` and `judge`

`agent` performs work; `judge` is the semantically evaluative twin. They accept the same properties.

```iter
agent reviewer:
  description: "Read-only branch reviewer"
  backend: "claude_code"
  model: "anthropic/claude-sonnet-4-6"
  provider: "anthropic,zai"
  input: review_request
  output: review_result
  system: review_system
  user: review_user
  session: fresh
  tools: [bash, read_file, grep]
  tool_policy: [git.*, read_file]
  capabilities: [board.read]
  skills: ["review-playbook"]
  tool_max_steps: 10
  max_tokens: 12000
  reasoning_effort: high
  timeout: "20m"
  readonly: true
  publish: review_artifact
  artifact_labels: [review, branch]
```

Important property groups:

| Group | Properties |
|---|---|
| Model execution | `model`, `backend`, `provider`, and the `claude_code`-compatible binary override `command`. See [backends](backends.md) and [delegation](delegation.md). |
| Data/prompt | `input`, `output`, `system`, `user`, `publish`, `artifact_labels`, `description`. |
| Conversation | `session: fresh\|inherit\|inherit_if_available\|fork\|artifacts_only\|persist`, `interaction`, `interaction_prompt`, `interaction_model`. `persist` (ADR-089) resumes **this node's own** last CLI conversation on re-entry (claude_code / pi / codex); judges and humans stay graph nodes. Trunk-only (C243). |
| Tools/access | `tools`, `tool_policy`, `capabilities`, `skills`, `permission`, `mcp`, `sandbox`. |
| Limits | `tool_max_steps`, `max_tokens`, `reasoning_effort`, `timeout`, `compaction`, `compress`. |
| Scheduling | `await`, `needs`, and the workspace-safety assertion `readonly`. |
| Backend-specific | `full_access` and `images` are honored by the Codex backend; other backends ignore them. |
| Persistent context | `memory` and `cursors`. |

`readonly: true` forces delegated agents into a read-only sandbox and classifies the node as non-mutating for parallel workspace safety. `full_access: true` is a high-authority Codex-only opt-in; `readonly` wins if both are present.

Node-level nested blocks include:

```iter
agent worker:
  # ...model/prompts...
  compaction:
    threshold: 0.85
    preserve_recent: 4
  memory:
    enabled: true
    scope: "campaign"
    autoload: ["CONTEXT_BRIEF.md"]
    read: true
    write: true
    pre_compact_inject: true
    project_root: true
    visibility: "bot"
  cursors:
    enabled: true
    rigor: high
  mcp:
    inherit: true
    servers: [repo_tools]
    disable: [legacy_server]
```

See [memory and knowledge](memory-and-knowledge.md), [cursors](cursors.md), [permissions](permissions.md), [skills](skills-library.md), and [sandboxing](sandbox.md).

## Routers and convergence

Iterion has five router modes:

```iter
router all_reviews:
  mode: fan_out_all

router per_ticket:
  mode: fan_out_each
  over: "{{outputs.plan.tickets}}"
  as: ticket
  key: id
  depends_on: deps

router decision:
  mode: condition

router alternate:
  mode: round_robin

router smart:
  mode: llm
  model: "anthropic/claude-sonnet-4-6"
  system: routing_prompt
  multi: true
```

- `fan_out_all` activates every outgoing edge.
- `fan_out_each` replays exactly one unconditional template edge per runtime array item; `key`/`depends_on` optionally impose a dependency DAG.
- `condition` makes edge guards explicit.
- `round_robin` selects one outgoing edge per traversal in declaration order.
- `llm` selects one or several candidates and is the only router mode that makes a model call.

Parallel branches converge at an `agent`, `judge`, `human`, `tool`, or `compute` node:

```iter
compute collect:
  output: collection_result
  await: wait_all       # or best_effort
  expr:
    completed: "true"
```

Routers are fan-out sources and never declare `await`. See [routers](routers.md) and [composition/iteration/sub-bots](groups-iteration-subbots.md).

## Human interaction

```iter
human approval:
  description: "Release approval"
  input: approval_request
  output: approval_response
  instructions: approval_prompt
  interaction: human
  min_answers: 1
```

`interaction` is one of `none`, `human`, `llm`, `llm_or_human`, `review`, or `async`. A review gate additionally accepts `review_url`, `posture`, `merge_strategy`, `merge_into`, and `max_turns`. The `async` mode is an agent/judge mode (not a human-node mode): the node posts non-blocking questions with `ask_user_async` and keeps working, syncing on demand via an `await_answers` node — see [async interaction](async-interaction.md). Human nodes may also publish labeled artifacts and converge with `await`. See [human-in-the-loop](human-in-the-loop.md) and [review/merge gate](review-merge-gate.md).

Resume a pause with `iterion resume --run-id <id> --file workflow.bot --answer key=value`.

## Deterministic nodes

### `tool`

A tool executes either a shell command or a script; it does not call an LLM.

```iter
tool run_tests:
  description: "Run the repository test suite"
  command: `make test`
  output: test_result
  publish: test_result_artifact
  permission: ask
  needs: [test_slot]
```

`command` and `script` are mutually exclusive. A script adds `language: js|py|sh|bash` (default `sh`). Tools also accept `input`, `output`, `publish`, `artifact_labels`, `await`, `sandbox`, `compress`, `permission`, and `needs`.

Verified Actions add a deterministic outcome check and bounded recovery:

```iter
tool deploy:
  command: `./deploy.sh`
  goal: "The service is deployed and healthy"
  postcondition: `./scripts/check-health.sh`
  policy: recover       # required | recover | best_effort
  recovery:
    max_repair_attempts: 2
    max_agent_attempts: 1
    model: "anthropic/claude-sonnet-4-6"
    agent_tools: [read_file, bash]
```

`parallel_safe: true` is a narrowly scoped assertion for `fan_out_each`: concurrent replays must write only to disjoint item-keyed targets. It does not make a tool generally read-only.

### `compute`

`compute` evaluates bounded expressions without an LLM or shell:

```iter
schema stats:
  count: int
  ready: bool

compute summarize:
  input: review_result
  output: stats
  expr:
    count: "length(input.issues)"
    ready: "input.approved && length(input.issues) == 0"
```

Expressions support field/index access, arithmetic/comparison/boolean operators, conditional/map/filter/reduce forms, and the total built-ins `length`, `concat`, `unique`, `contains`, `join`, `tail`, `if`, `sort`, `keys`, `values`, `slice`, `sum`, `min`, `max`, and `flatten`. They share namespaces with quoted `when` expressions and are bounded by an evaluation-work limit; see [DSL totality](dsl-totality-and-tc.md).

### `emit` and `wait`

These nodes coordinate concurrent branches through immutable run-scoped events:

```iter
emit publish_ready:
  event: "ready"
  with {
    revision: "{{outputs.build.sha}}"
  }

wait await_ready:
  event: "ready"
  timeout: "30s"
  output: ready_payload
```

`wait.timeout` is mandatory: the language does not permit an unbounded silent wait.

## Reuse and nested execution

### `group` / `use`

Groups are compile-time macros containing agents, judges, routers, humans, tools, computes, and internal edges. Each use prefixes cloned node ids and substitutes `{{params.*}}`.

```iter
group check(rule):
  agent inspect:
    model: "anthropic/claude-sonnet-4-6"
    user: inspect_prompt

use check as security with {
  rule: "security"
}

workflow grouped:
  entry: security.inspect
  security.inspect -> done
```

External workflow edges address expanded nodes as `<prefix>.<node>`.

### `subbot`

```iter
subbot run_ticket:
  description: "Implement one planned ticket"
  source: "child.bot"
  with {
    issue: "{{outputs.dispatch.ticket.id}}"
  }
  output: ticket_result
  needs: [worktree_slot]
  isolated: true
```

A subbot is a real nested run with its own loops, state, and budget. Parent budget totals do not aggregate child budgets. `isolated: true` is a workspace-safety assertion, not automatic isolation: use it only when the child cannot mutate the parent checkout.

See [groups, iteration, resources, and sub-bots](groups-iteration-subbots.md) for pause/resume, board-write, and concurrency boundaries.

## Cursors and supervisors

A cursor declares reusable prompt calibration; a supervisor is a concurrent watcher, not a graph node:

```iter
cursor rigor:
  description: "Review strictness"
  values:
    normal: "Focus on material defects."
    high: "Demand direct evidence and adversarial checks."

supervisor guard:
  watches: [worker]
  model: "anthropic/claude-sonnet-4-6"
  system: supervision_policy
  cooldown: "30s"
  max_evals: 20
```

Cursor declarations use either `values:` or numeric `bands:`, never both. Supervisors enqueue node-scoped steering messages while watched nodes are active. See [cursors](cursors.md) and [supervisors](supervisors.md).

## MCP servers

```iter
mcp_server code_tools:
  transport: stdio
  command: "npx"
  args: ["-y", "@example/code-tools"]

mcp_server remote_tools:
  transport: http
  url: "https://tools.example.com/mcp"
  auth:
    type: "oauth2"
    auth_url: "https://tools.example.com/oauth/authorize"
    token_url: "https://tools.example.com/oauth/token"
    client_id: "iterion"
    scopes: ["tools.read"]
```

Supported transports are `stdio`, `http`, and `sse`. Workflow `mcp:` blocks may set `autoload_project`, `servers`, and `disable`; node blocks use `inherit`, `servers`, and `disable`.

The resolved set is **authoritative** on `claude_code`: iterion passes it via
`--mcp-config --strict-mcp-config`, so the operator's personal user-scope MCP
servers (`~/.claude.json`) do NOT boot inside bot nodes — a node's `mcp:`
block (plus the repo's `.mcp.json` through `autoload_project` and iterion's
own ask_user/board servers) is the complete truth. Set
`ITERION_CLAUDE_CODE_STRICT_MCP=0` to deliberately restore host-config
inheritance. (pi and claw are strict by construction — pi's MCP client only
connects declared servers, claw registers in-process tools.)

## Workflows and edges

A workflow selects the entry node, configures run-wide controls, and declares edges:

```iter
workflow review:
  entry: prepare
  default_backend: "claude_code"
  worktree: auto
  compress: on
  permission: ask
  allow: ["Read(*)", "Grep(*)"]
  ask: ["Bash(git push:*)"]
  deny: ["Bash(rm:*)"]
  capabilities: [board.read]
  skills: ["review-playbook"]

  budget:
    max_parallel_branches: 4
    max_duration: "30m"
    max_cost_usd: 10
    max_tokens: 400000
    warn_tokens: 300000
    max_iterations: 100

  resources:
    test_slot: 2
    worktree_slot: ["slot-a", "slot-b"]

  compaction:
    threshold: 0.85
    preserve_recent: 4

  sandbox: auto

  prepare -> reviewer
  reviewer -> done when approved
  reviewer -> prepare when not approved as retry(3)
```

Workflow controls are `vars`, `attachments`, `entry`, `default_backend`, `tool_policy`, `capabilities`, `skills`, `mcp`, `budget`, `resources`, `compaction`, `interaction`, `worktree`, `compress`, `permission`, `allow`, `ask`, `deny`, and `sandbox`.

#### Budget fields

`max_duration` is a Go duration **string**, and the only budget field
resolved through `${VAR:-default}` — `max_duration: "${RUN_BUDGET:-30m}"`
lets a bot be re-timed per environment without editing the `.bot`. Two
consequences worth knowing:

- A value that does not parse is **not** a compile diagnostic. The
  runtime logs `the duration cap is NOT ENFORCED for this run` at WARN
  and carries on with no time limit, so a `"2h3Om"` typo costs the cap
  silently unless you read the log.
- If `max_duration` was the *only* limit declared, that same typo drops
  the whole budget tracker (no cost, token, or iteration accounting
  either) — the tracker is only built when at least one limit resolved.

The numeric fields (`max_cost_usd`, `max_tokens`, `max_iterations`,
`warn_tokens`) are typed, so they fail at compile time instead
(`C046` for a malformed `max_cost_usd`).

#### Budget and loop back-edges

A loop's back-edge is declined when the budget can no longer fund another
iteration. The runtime prices one iteration by what the previous one
consumed — the distance between two consecutive arrivals at the loop's
decision point, on every axis the workflow actually caps (`max_cost_usd`,
`max_tokens`, `max_iterations`, `max_duration`) — and skips the back-edge
once another iteration would reach the threshold where the engine stops
starting nodes at all (90% of the cap). Stopping merely before the cap
would not be enough: the run would fall through into an exit path that
same threshold then refuses. The run instead leaves through its own exit
path with room to walk it — for the campaign shape below, the
`gate -> publish` fall-through that also serves loop exhaustion.

```iter
  gate -> publish when converged
  gate -> work as passes(4)
  gate -> publish            # exhausted, or unaffordable: ship what is banked
```

This matters for any loop that banks work as it goes (commits in stride, a
published report, a PR opened by a tail node). Without it a loop starts an
iteration it cannot pay for, dies mid-iteration on `BUDGET_EXCEEDED`, and
the tail that would have delivered the work never runs.

A loop is priced from the moment it is **entered**, and re-priced on each
re-entry — so a loop reached late in a run (a second phase) is charged for
its own iterations, never for the work that preceded it, and a nested loop
re-entered per outer iteration starts fresh. A loop that has not been
measured yet reports nothing rather than guessing. The prices ride the
checkpoint, so a resumed run keeps measuring across the pause.

The decline is visible, never silent: a `budget_warning` event carrying
`reason: loop_budget_guard` with the loop, the blocking dimension, the
remaining allowance, the price of the last iteration, and the axis's
`used`/`limit` (durations in seconds, with an explicit `unit`). A
conditional back-edge is only priced on a crossing where its `when`
actually holds.

The guard is **on by default** and switched off through the usual
precedence chain — `--loop-budget-guard off` (on `run` and `resume`) →
the workflow's `loop_budget_guard: off` → `ITERION_LOOP_BUDGET_GUARD=off`
→ the default `on`. Turning it off restores the
run-until-you-hit-the-wall behaviour, hard failure included. An invalid
value is diagnostic **C133**, not a silent fall back to the default. The
90%-hard-limit and exceeded checks stay as the backstop for a single node
that overruns on its own.

The run-level override **travels** — onto the cloud queue
(`RunMessage.loop_budget_guard`, schema v7) and into a detached
subprocess — so a runner pod re-resolving the chain from its own empty
environment cannot quietly replace what the operator asked for. It is not
persisted on the run, so `iterion resume --loop-budget-guard` must
re-state it.

**Exit grace.** Once a cap is *spent* (100%+), the run may still walk
**forward** — never around a declared `loop` — spending up to **10%
beyond the declared cap** to reach a terminal node, so work it has already paid for
gets delivered (the PR opened, the report written) instead of dying on
disk. Every graced node is recorded as a `budget_exit_grace` event naming
the exceeded axis and its own used/limit pair. The allowance is
proportional and bounded: past `cap × 1.1` the run fails as
`BUDGET_EXCEEDED`, exactly as before — including the `max_duration` axis,
where a graced node is given a real deadline at the graced ceiling rather
than running unbounded. `ITERION_BUDGET_EXIT_GRACE` overrides the ratio;
`0` (or `off`/`no`/`false`/`none`) makes every declared cap **absolute** —
the setting for deployments where a cap must be a hard invoice ceiling
(shared instances, pooled credentials). It parses **fail-closed**: a value
outside `[0,1]`, or one that is not a number, also means 0, with a one-time
stderr warning — an operator reaching for this variable wants a *tighter*
policy, so an unreadable value must never grant the permissive default.

The grace is **refused** in two cases:

- **the loop budget guard is off** — the "no further iteration" half of the
  safety argument belongs to that guard, and with it lifted a graced run
  could take a back-edge and keep looping on a spent budget;

  The guard prices `loop`-named back-edges. A `foreach` back-edge is
  bounded by its collection rather than by affordability, so a graced run
  inside a `foreach` body keeps iterating until the proportional ceiling
  stops it — the spend bound holds either way, but "it cannot iterate
  again" is a promise only the declared-`loop` form makes.
- **the cap was imposed from outside the run** — a limit clamped by the
  platform ceiling or by a credential-pool donor's remaining allowance is an
  absolute promise to a third party, so the declared figure *is* the wall.
  The marker is set at one choke point (`Budget.ClampToCeiling`, only when
  it actually lowers something) and travels the cloud queue as
  `BudgetOverrides.cap_imposed`, so a runner pod enforces it too.

Both *exceeded* stop-paths — the check before a node runs and the overrun
noticed after one succeeds — go through the same decision, so a node is
never refused by a rule stricter than the one that admitted it. What the
second path still catches is a node whose **own** spend carries the run
past `cap × (1+ratio)`: it completes, and the run then ends. The grace
buys the node its chance to deliver, not immunity from the ceiling.

The grace only exists **past** the cap. The separate 90% hard limit, which
refuses to start a new node while an axis sits in `[90%, 100%)` to bound
concurrent overage, is **not** graced and is unchanged: it is reached only
when nothing is exceeded yet, so it stops a run *before* the grace could
ever apply. The counter-intuitive consequence is real — a run refused at
92% of its cap gets no grace, while a run already at 105% may walk on to
its terminal node. Raise the cap and resume for the former; the latter is
the case the grace was built for.

```iter
workflow campaign:
  entry: work
  loop_budget_guard: off    # this loop must burn its cap, not stop short
```

#### `max_cost_usd` only counts spend it can price

A node's cost is known when the backend meters it (the `claude_code` and
`pi` CLIs report their own figure) or when the model resolves in one of
three price sources, in order: claw's live registry, then the spec
aggregator's published pair (models.dev via `pkg/backend/modelspecs` —
taken only when BOTH rates are positive, since a half-published pair would
price the other half at zero), then `pkg/backend/cost`'s static table.
When none answers, `cost.Annotate` deliberately omits `_cost_usd`: an
absent value means *no cost data*, never *this call was free*.

The budget honours that difference rather than folding the absence into a
`0.00` sample. Tokens burned at an unresolvable price are counted apart,
and the first time it happens under a declared `max_cost_usd` the run emits
one advisory `budget_warning` on dimension `cost_usd_unpriced`, whose
`detail` names how many node executions and how many tokens the ceiling
could not see *at that point*. That figure is a floor, not a total: the
warning is raised once per ceiling — the operator is told, not spammed —
while the counters keep climbing behind it, so a run that goes on to burn
forty unpriced nodes was told about the first. The run continues — an
operator may legitimately want it to — but the ceiling never again reads as
enforced when it is only partial.

What reaches it is a model absent from all three pricing sources —
typically one newer than the static table and not yet published, or one
whose published pair is half-known. A backend that publishes no dollar
figure of its own, like `codex`, is not a separate cause: it falls back to
those same sources, so it only goes unpriced when its model does. If the
warning fires, run `iterion models pricing` to see which source (if any)
answers, then either add the model to the table or expect `max_cost_usd` to
bind on the priced nodes only.

### The target repo's toolchain — `repo_devbox:`

Two `devbox.json` files can supply a run's binaries, and both are
honoured: the **bot's own**, shipped beside its `main.bot`, and the
**target repo's**, at the workspace root. `repo_devbox:` governs the
second one only.

It exists because "the repo pins a toolchain" and "this run needs that
toolchain" are not the same statement. A run that *builds* the repo needs
it. A run that reads a diff and writes comments does not — and pays for it
anyway: on iterion's own tree that bill is **319 Nix paths, 406 MiB
downloaded, 1.8 GiB unpacked** (a desktop GUI stack among them), before
the first node executes, on every review. A cold install can also outlast
the window a sandbox has to come up, which turns a cost into a dead run.

Default **on** — a repo that pins its toolchain usually pins it to be
built. Switched off through the usual chain: `--repo-devbox off` (on `run`
and `resume`) → the workflow's `repo_devbox: off` → `ITERION_REPO_DEVBOX`
→ the default `on`. An invalid value is diagnostic **C134**, not a silent
fall back. The bot's own `devbox.json` is never affected: a bot that
declares `crane` needs `crane` whatever repo it is pointed at.

A declined source is **reported, not dropped** — the
`sandbox_devbox_provisioned` event carries `skipped_sources: ["repo"]`
with the config it declined, and the run logs it. Without that, the only
trace of the decision would be a binary missing later, which reads as an
agent bug.

The override does **not** travel onto the cloud queue: what a cloud runner
needs is the *workflow's* declaration, which rides the `.bot` itself. So a
bot's `repo_devbox: off` holds in cloud, while `--repo-devbox` is a local
run's override.

```iter
workflow review_pr:
  entry: review
  repo_devbox: off    # this run reads the repo, it does not build it
```

### Edge forms

```iter
src -> dst
src -> dst when approved
src -> dst when not approved
src -> dst when "approved && length(outputs.scan.findings) == 0"
src -> fallback else
src -> dst as retry(5)
src -> dst as retry("{{outputs.plan.max_passes}}")
src -> dst as retry(unbounded 200)
src -> dst as foreach scan(item in "{{outputs.plan.items}}")
src -> dst with {
  context: "{{outputs.src}}",
  produced: "{{input.field}}",
  mode: "{{vars.mode}}"
}
```

Optional `when`/`else`, `as`, and `with` clauses may appear in any order, once each. `else` is the explicit fallback when no sibling guard matched. A quoted `when` uses the bounded expression language. In a `with` mapping, `{{input.field}}` is the source node's output (C034 checks that output schema); `{{vars.name}}` is a workflow variable; `{{outputs.node.field}}` names any prior node. There is no silent fallback from `input` to run-level inputs.

Quoted `when` expressions are evaluated in parallel branch bodies as well as on the trunk, against that branch's private outputs, artifacts, loop state, and shared run variables. Migration note: older runtimes skipped expression-form edges inside `fan_out_all`, `fan_out_each`, and `llm multi: true` branches, so an existing workflow may now take a guarded route that previously fell through to `else` or an unconditional edge.

Every cycle must carry an `as <loop>(...)` clause. A cap may be a literal, a runtime template, or `unbounded` with a fuel ceiling. If an unbounded loop omits its local fuel, `budget.max_iterations` must supply it; the runtime also applies a no-progress liveness monitor. `as foreach` is different: it walks a finite array sequentially and binds the `each.<name>` namespace.

A bounded loop or foreach may live wholly inside one `fan_out_all`, `fan_out_each`, or `llm` `multi: true` branch. Every branch/item owns independent counters, loop snapshots, outputs, artifact allocations, and a durable cursor; siblings may therefore finish after different numbers of iterations, and a restart or human pause resumes the same local scope without replaying completed iterations. The collector becomes ready only after those local lifecycles terminate, under the existing `wait_all` / `best_effort` policy.

**C244** is reserved for iteration with no unambiguous owner: an iteration edge on the fan-out router, a back-edge from the collector into a body (`join -> a1 as more`), a cycle crossing sibling branches, or a shared-node shape owned by more than one branch. A loop that wraps the fan-out from the join (`join -> router as outer(N)`) remains a normal trunk loop. Use a `subbot` when independent budgets, workspace isolation, or a reusable capability boundary are desired—not merely to obtain per-item counters. See [composition/iteration/sub-bots](groups-iteration-subbots.md).

Terminal targets `done` and `fail` are reserved and are never declared.

## Worktrees, sandboxing, permissions, and budgets

- `worktree: auto` executes in a per-run worktree and preserves a run branch. Final merge behavior depends on CLI/studio flags and delegated merge authority; see [merge policy](merge-policy.md) and [resume](resume.md).
- `sandbox: auto` resolves a devcontainer/default image. Block form supports image/build, user/workspace, host-state, environment, mounts, post-create, and network policy; see [sandbox](sandbox.md).
- `permission: off|ask|deny` plus allow/ask/deny rules creates an execution-time tool gate. CLI overrides are available; see [permissions](permissions.md).
- `compress: off|on|ultra` controls output compression where supported; see [ultracode](ultracode.md).
- Workflow budgets are shared across branches in that run. Hitting cost, token, duration, parallelism, or iteration limits emits budget events and stops/parks according to the failure path. Nested subbot runs retain their own budgets. `warn_tokens` is the advisory exception: crossing it emits a single `budget_warning` event (`advisory: true`) suggesting an audit of what consumed the tokens, and execution continues — use it instead of `max_tokens` when heavy consumption is legitimate (judge/rewrite loops going to their bounds) but worth an operator's look.
- `resources` are named semaphores. Integer values declare capacities; string arrays declare leaseable named members exposed to nodes that list the resource in `needs:`.

## Validation and references

Run `iterion validate workflow.bot` before execution. Diagnostics occupy sparse ranges: DSL/compiler/runtime consistency checks use C001–C199 plus the async-interaction band C240–C242, C243 (`session: persist` in a fan-out body), C244 (bounded iteration crossing a parallel-branch boundary), and C245 (trunk-only human mode in a parallel branch); bundle checks use C200–C234. The authoritative list is [references/diagnostics.md](references/diagnostics.md).

- [Readable grammar](references/dsl-grammar.md)
- [Formal EBNF](grammar/iterion_v1.ebnf)
- [Router semantics](routers.md)
- [Composition, iteration, resources, and sub-bots](groups-iteration-subbots.md)
- [Human interaction](human-in-the-loop.md)
- [Reusable workflow patterns](references/patterns.md)
- [Authoring pitfalls](workflow_authoring_pitfalls.md)
