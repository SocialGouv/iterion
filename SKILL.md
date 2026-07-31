---
name: iterion-dsl
description: >
  Author, review, or debug Iterion .bot workflows and .botz bundles using the
  current indentation-sensitive DSL. Use for requests to create or change an
  Iterion workflow, design an agent graph, choose router/interaction/session
  modes, add tools or subbots, diagnose validation codes, or explain accepted
  .bot syntax.
---

# Author Iterion workflows

Treat the parser and compiler as the source of truth. Read
[`docs/dsl.md`](docs/dsl.md) for the language guide, then open only the detailed
reference needed for the task:

- exact syntax: [`docs/references/dsl-grammar.md`](docs/references/dsl-grammar.md)
- validation codes: [`docs/references/diagnostics.md`](docs/references/diagnostics.md)
- routing/convergence: [`docs/routers.md`](docs/routers.md)
- groups, iteration, resources, and subbots:
  [`docs/groups-iteration-subbots.md`](docs/groups-iteration-subbots.md)
- permissions and isolation: [`docs/permissions.md`](docs/permissions.md) and
  [`docs/sandbox.md`](docs/sandbox.md)

Do not infer syntax from an old bot or ADR when `iterion validate` and the
current references disagree.

## Build the workflow

1. Inspect neighboring maintained bots and the target repository's toolchain.
2. Declare inputs, prompts, schemas, and capabilities before designing prompts
   around implicit data.
3. Choose the smallest graph that exposes deterministic decisions as edges.
4. Add bounded loops, budgets, convergence, and workspace-safety assertions.
5. Validate after each structural change; fix diagnostics at their source.
6. Run or package only after validation succeeds.

Top-level declarations may appear in any order:

```text
vars, presets, attachments, secrets, mcp_server,
prompt, schema, cursor, supervisor,
agent, judge, router, human, tool, compute, emit, wait, subbot,
group, use, workflow
```

Keep exactly one compiled workflow per file. Use `##` comments. Strings may be
quoted, backtick-delimited raw strings, or block scalars where accepted.

## Select node kinds deliberately

| Kind | Use |
|---|---|
| `agent` | LLM work, optionally with tools and persistent conversation. |
| `judge` | The same syntax as `agent`, with evaluative intent. |
| `router` | Fan-out, per-item map, conditional, alternating, or LLM routing. |
| `human` | Durable form/review interaction and optional merge gate. |
| `tool` | Deterministic shell command or `js`/`py`/`sh`/`bash` script. |
| `compute` | Bounded, side-effect-free expressions. |
| `emit` / `wait` | Run-scoped event coordination; every `wait` needs a timeout. |
| `subbot` | A real nested run from another `.bot`. |
| `done` / `fail` | Reserved success and intentional-failure terminals. |

Agents and judges can set model/backend/provider, input/output schemas, prompt
references, tools/capabilities/skills, permissions, MCP, memory, compaction,
sandboxing, limits, resources, publication, and convergence. Consult the
grammar instead of guessing a property name.

Use one of the five session modes:

```text
fresh | inherit | inherit_if_available | fork | artifacts_only
```

Use one of the six interaction values:

```text
none | human | llm | llm_or_human | review | async
```

`none` rejects mid-step interaction on agents/judges. An explicit `none` on a
human node currently follows the normal human-pause path; prefer `human` there.
`async` lets an agent/judge post non-blocking `ask_user_async` questions and
sync on demand via `await_answers`.

## Route and converge

Iterion has five router modes:

```iter
router dispatch:
  mode: fan_out_each
  over: "{{outputs.plan.items}}"
  as: item
  key: id
  depends_on: deps
```

- `fan_out_all`: activate every outgoing edge.
- `fan_out_each`: replay one unconditional template edge per array item;
  optional `key`/`depends_on` impose a DAG.
- `condition`: make guarded edge selection explicit.
- `round_robin`: select one edge per traversal in declaration order.
- `llm`: ask a model to select one or several unconditional candidates.

Converge parallel branches on an `agent`, `judge`, `human`, `tool`, or
`compute` with `await: wait_all` or `await: best_effort`. Routers never accept
`await`.

Keep concurrent writes fail-closed. Mark an agent `readonly: true` only when it
cannot mutate the checkout. Mark a subbot `isolated: true` only when it writes
to its own store/worktree. Mark a tool `parallel_safe: true` only for
`fan_out_each` replays with disjoint item-keyed outputs. Otherwise serialize or
provide separate workspaces.

## Declare data flow and iteration

Use edge clauses in any order, at most once each:

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
  context: "{{outputs.src}}"
}
```

Declare every graph cycle with a bounded/runtime loop or explicit
`unbounded` fuel. Give guarded routes an exhaustive complement or `else`.
Use edge `foreach` for ordered stateful iteration and `fan_out_each` for an
independent parallel map.

Runtime templates include `vars`, `input`, `outputs`, `artifacts`,
`attachments`, `secrets`, `loop`, `each`, and `run`. Group expansion consumes
`{{params.name}}` at compile time. In tool commands, ordinary
`{{input.field}}` is shell-escaped; `{{!input.field}}` is deliberately raw and
must receive only trusted executable syntax.

## Prefer deterministic controls

- Put machine-checkable transformations in `compute`, not prompts.
- Put repository checks in `tool` nodes and make their results gate progress.
- Give every workflow finite budgets (`max_duration`, cost/tokens/iterations,
  and parallelism) proportional to its work.
- Use `resources`/`needs` for scarce tools or workspace slots.
- Use Verified Action `goal`, `postcondition`, `policy`, and `recovery` when a
  side effect needs a deterministic outcome check.
- Declare required sandbox tools in the repository or bot's `devbox.json`;
  do not rely on an interactive shell profile.

## Minimal example

```iter
prompt review_user:
  Review {{input.change}} and report only material defects.

schema review_input:
  change: string

schema review_output:
  approved: bool
  summary: string

judge review:
  model: "anthropic/claude-sonnet-4-6"
  input: review_input
  output: review_output
  user: review_user
  readonly: true

workflow review_change:
  entry: review
  review -> done when approved
  review -> fail when not approved
```

## Validate and inspect

```bash
iterion validate workflow.bot
iterion validate bundle.botz --json
iterion diagram workflow.bot --view full
iterion run workflow.bot --var key=value
```

Validation emits sparse DSL codes in C001–C199 plus the async-interaction band
C240–C242, and bundle codes in C200–C234. Do not assume the numeric ranges are
contiguous. For bundles, also check
[`docs/bundles.md`](docs/bundles.md). For current CLI flags, use
`iterion <command> --help` and [`docs/cli-reference.md`](docs/cli-reference.md).
