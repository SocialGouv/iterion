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

## Start from a template

Fill a validated shape rather than writing the graph from the grammar:

```sh
iterion bots templates                       # the gallery, one line per template
iterion bots create <slug> --template <id>   # a compiling bundle under bots/<slug>/
```

Each shape is a complete, commented workflow of a form the catalog bots are
made of; it compiles as rendered, and a test holds it to its form. Pick the
one whose graph matches, then edit the prompts, the vars and the edges:

| Template | Shape |
|---|---|
| `campaign-loop` | an entry gate (unset `verify_command` = typed refusal) → one agent in passes → a `tool` running the repo's own checks (needs `jq`, pinned in the bundle's `devbox.json` for any image that ships devbox; every iterion image ships both) → a `compute` gate → a bounded loop, with a typed `fail` at exhaustion |
| `review-fanout` | a `tool` scope gate (empty scope = typed refusal) → `router fan_out_all` → two read-only reviewers under `permission: deny` with a read-only allow list → a `compute` with `await: wait_all` → a typed blocked verdict |
| `plan-gate-implement` | read-only plan → `human` gate (bounded re-plan) → implement in a worktree |
| `scheduled-digest` | collect (`tool`, needs `jq`, pinned in the bundle's `devbox.json`) → digest (agent) → verify the artifact (`tool`), the cron in the manifest |
| `per-ticket-subbots` | list (`tool`) → `fan_out_each` → an isolated `subbot` per item → `compute` fan-in |
| `verified-action` | entry gates (unset or TAKEN `tag` = typed refusal) → an agent prepares → a `tool` with `goal` + `postcondition` + `policy: recover` + `recovery` |
| `async-questions` | an `interaction: async` agent → an `await_answers` gate → a finalizer |
| `multi-file` | the graph in `main.bot`, the prompts in `prompts/*.md`, the knowledge in `skills/` |

`blank`, `daily-digest`, `code-reviewer`, `docs-writer` and `issue-triager`
render the single-agent workflow (one adaptive agent carrying the mission).
More complete workflows to copy from: `docs/references/patterns.md`.

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
agent, judge, router, human, tool, compute, emit, wait, await_answers, subbot,
group, use, workflow
```

Keep exactly one compiled workflow per file. `#` starts a comment (`##` is the
same comment). Strings may be quoted, backtick-delimited raw strings, or block
scalars where accepted.

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
| `await_answers` | Deterministic sync point for async operator questions; requires a timeout. |
| `subbot` | A real nested run from another `.bot`. |
| `done` / `fail` | Reserved success and intentional-failure terminals. |

Agents and judges can set model/backend/provider, input/output schemas, prompt
references, tools/capabilities/skills, permissions, MCP, memory, compaction,
sandboxing, limits, resources, publication, and convergence. Consult the
grammar instead of guessing a property name.

Use one of the six session modes:

```text
fresh | inherit | inherit_if_available | fork | artifacts_only | persist
```

`persist` resumes this node's own last CLI session on re-entry (packed
StateRef, ADR-089). It is not inherit-from-parent. Trunk-only (C243).
Supported backends: `claude_code`, `pi`, `codex`.

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

```iter fragment
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

```iter fragment:edges
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

## Rules the grammar does not show

Every one of these is enforced by the compiler or the runtime, and none of
them can be read off the syntax tables — authors reverse-engineer them from
shipped bots, so they are written here:

- **A tool node's stdout is its output.** With `output:` declared, the
  command must print a JSON object matching the schema; other stdout is
  wrapped as `{"result": "…"}` and the fields downstream expect are absent. A
  non-zero exit fails the node — wrap a command whose failure is a *result*
  (a failing test suite) so it exits 0 and reports `passed: false`.
- **A loop needs an exhaustion exit.** `src -> body as name(N)` next to a bare
  `src -> exit` is the one legal pair of unconditional edges (the back-edge is
  exempt from C010); without the bare edge a spent loop leaves the node with
  no edge to take and the run fails `NO_OUTGOING_EDGE` (the log names the
  exhausted loop). `as name(N)` allows N back-edge CROSSINGS — N+1 executions
  of the body — so a var that counts passes feeds the cap as `passes - 1`
  (the `campaign-loop` template derives it in its gate, re-evaluated on every pass).
- **`outputs.*` needs no threading.** `{{outputs.<node>.<field>}}` is
  readable from any node that runs after the producer; `{{input.<field>}}`
  only carries the node's declared input and what an edge `with` mapped.
- **Never quote a `{{ref}}` in a `command:`.** The runtime shell-escapes it as
  one word; your own quotes close its quoting (C137).
- **`expr:` values and quoted `when` are expressions, not templates**: write
  `input.x`, never `{{input.x}}` (C040).
- **`#` is a comment everywhere EXCEPT inside a string, a prompt body or a
  block scalar, where it is text** — `# Approve the plan?` in a prompt reaches
  the model as a heading. A literal `{{…}}` example belongs in prose, not in a
  prompt (every reference in a prompt is validated).
- **A blank line inside a prompt body is dropped.** The lexer skips blank
  and space-only lines under a prompt header, so a paragraph break reaches
  the model as a single newline; put a heading or a line of prose where the
  model must see a break, and a multi-line `{{…}}` value under a heading or
  inside a ``` fence, or it runs into the line that follows it.
- **A typed refusal is `fail <name>:`** with an UPPER_SNAKE `code:` — the bare
  `-> fail` target carries no code. The engine's own codes are reserved
  (C248 names them: `BUDGET_EXCEEDED`, `TIMEOUT`, … — the list is
  `pkg/store/lifecycle.go`'s `ReservedFailureCodes`); pick a name of the
  bot's own. `resumable: true` is honoured only when the fail node has ONE
  predecessor (the guard that routed in — a resume re-evaluates it); with
  several it degrades to terminal with a warning.
- **The `run.*` namespace, in a `compute` expr or a quoted `when`:**
  `run.elapsed_seconds`, `run.max_duration_seconds`, `run.cost_usd`,
  `run.max_cost_usd`, `run.tokens`, `run.max_tokens`, `run.iterations`,
  `run.max_iterations`, `run.id` — the run's own consumption and its
  EFFECTIVE caps (after `--max-*`, the recipe, the platform ceiling). A cap
  of 0 means UNBOUNDED, so guard a ratio with `run.max_duration_seconds > 0`
  first, and a workflow with no `budget:` block has no caps to read at all.
- **A prompt reference to a node that has not run yet renders as its literal
  placeholder** (`{{outputs.verify.detail}}` on the first pass of a loop
  prints exactly that); only `loop.*`, `vars.*` and a human node's
  instructions render empty. Thread a previous pass's output through the
  back-edge's `with` mapping onto an `input:` schema, with a deterministic
  entry `compute` giving the first pass the same shape — the
  `campaign-loop` template shows it.
- **The loop is declared on the back-edge** (`gate -> campaign … as
  passes(N)`), and the exhaustion exit leaves the SAME node. C244 judges
  the loop edge's two ENDPOINTS, not the cycle's contents: a back-edge
  between two trunk nodes may span a `fan_out_all` router and its branches.
  The workflow's `entry` may be a loop target.
- **`when x` and `when not x` on the same source are the exhaustive pair
  (C012)**; `else` needs a `when` sibling (C015), and a bare edge beside an
  `else` is refused (C124) — the only bare edge beside guards is a loop's
  exhaustion exit.
- **A Verified Action's `recovery:` block only acts under `policy: recover`**
  — under any other policy it is dead config (C106); the postcondition's
  JSON stdout is the node's output on every rung, the skip included.
- **Parallel branches may hold ONE mutating node**; reviewers that fan out
  together are all `readonly: true` — a declaration the engine trusts, not
  one it checks — or the run is refused at the fan-out (`WORKSPACE_SAFETY`),
  not at validate. Read-only in FACT too: never `git add -N .` in a
  parallel branch — it takes `.git/index.lock` and is FATAL when the
  sibling holds it (`git diff`'s own stat refresh just skips), and the
  loser's empty findings read as an approve. Read untracked files with
  `git ls-files --others --exclude-standard -z | xargs -0 -I{} git diff
  --no-index -- /dev/null {}` (exit 1 per file, 123 for the batch: a diff,
  not a failure). A `router` and a `fail` node take no `output:`.
- **A `worktree: auto` run starts from the anchor COMMIT, and only what it
  COMMITS reaches your checkout**: staged, unstaged and untracked work is
  not in the worktree (that is the isolation), and at the end a dirty tree
  is wip-banked as a commit on the storage branch `iterion/run/<name>` and
  never merged — a deliverable written at a workspace path is on that
  branch, not where the bot promised it, after a check inside the worktree
  read green. `worktree:` unset means `auto`, so a shape whose deliverable
  is a file, or a reviewer of "pending changes", writes `worktree: none`
  (or commits, or diffs a `base` ref) and gates an EMPTY scope as a typed
  refusal (the `review-fanout` template's `scope` tool).
- **`jq` ships in every sandbox image; `python3` only from `-full` up.** The
  DEFAULT image is `iterion-sandbox-slim` (`sandbox/slim/Dockerfile`: jq, no
  python3); `-full` and the `-sec` layered on it add python3. So a tool that
  must turn text into the JSON its `output:` schema wants uses `jq -Rs`
  unless the workflow PINS an image that has more; a missing interpreter
  degrades the output to `{"result": …}` silently.
- **A `fan_out_each` fan-in sees ONE output per node id.** At the collector
  (`await: wait_all` / `best_effort`) the branches' outputs are merged
  last-write-wins, so `{{outputs.<node>.<field>}}` after the fan-in is one
  item's result, not a list, and a downstream `compute` cannot aggregate
  the items from `outputs.*`. Give each item its own record instead (a
  board card, a subbot child run's own artifacts) — the
  `per-ticket-subbots` template shows it.

## Property reference

What each kind accepts, from the parser's own registry — the one list an
unknown property is checked against, so a name that is not here draws E012
with the closest accepted name in its `fix:` line.

<!-- dsl-spec:begin skill -->
Generated from the parser's property registry (`iterion dsl spec --write`). Forms: `str` quoted string · `id` bare name · `str|id` either · `int` `num` `bool` literals · `a|b` one of · `"a|b"` one of, quoted · `[id]` `[str]` `[tool]` `[skill]` lists, inline `[a, b]` or one `- item` per indented line · `map` `{K: "v"}` or an indented block · `with{}` a `with { k: "v" }` map · `{kind}` an indented block described under that kind.

- `prompt` — entries `indented text lines`
- `schema` — entries `field: string | bool | int | float | json | string[] | file [enum: "a", "b"]`
- `cursor` — description str · values {cursor.values} · bands {cursor.bands}
- `cursor.values` (`values:` in cursor) — entries `name: "prompt fragment"`
- `cursor.bands` (`bands:` in cursor) — entries `"lo..hi": "prompt fragment"`
- `supervisor` — watches [id] · model str · system id · cooldown str · max_evals int · monitors [str]
- `mcp_server` — transport stdio|http|sse · command str · args [str] · url str · auth {auth}
- `auth` (`auth:` in mcp_server) — type str · auth_url str · token_url str · revoke_url str · client_id str · scopes [str]
- `group` — entries `node declarations and edges (src -> dst)`
- `use` — entries `use g as p with { param: "value" }`
- `vars` (`vars:` in the file, workflow) — entries `name: type [enum: "a", "b"] [= default]`
- `presets` (`presets:` in the file) — entries `name: (indented) var: literal`
- `attachments` (`attachments:` in the file, workflow) — entries `name: file | image`
- `attachment` (`attachments:` in attachments) — description str · accept_mime [str] · required bool
- `secrets` (`secrets:` in the file) — entries `name: "value"`
- `secret` (`secrets:` in secrets) — value str · as value|file · mount_path str · env str|id · optional bool · hosts [str] · description str
- `agent` / `judge` — description str · model str · backend str · provider str · command str · input id · output id · publish id · artifact_labels [tool] · system id · user id · session fresh|inherit|inherit_if_available|fork|artifacts_only|persist · tools [tool] · tool_policy [tool] · capabilities [tool] · skills [skill] · tool_max_steps int · max_tokens int · reasoning_effort low|medium|high|xhigh|max|ultracode · timeout str · readonly bool · full_access bool · images [str] · interaction none|human|llm|llm_or_human|review|async · interaction_prompt id · interaction_model str · await wait_all|best_effort · compress on|ultra|off · auto_memory on|off · permission off|ask|deny · needs id|[id] · fallbacks {fallback} · mcp {mcp} · compaction {compaction} · memory {memory} · sandbox none|auto|{sandbox} · cursors {cursors}
- `router` — description str · mode fan_out_all|fan_out_each|condition|round_robin|llm · model str · backend str · provider str · system id · user id · multi bool · reasoning_effort low|medium|high|xhigh|max|ultracode · over str · as id · key id · depends_on id · needs id|[id]
- `human` — description str · input id · output id · publish id · artifact_labels [tool] · instructions id · system id · model str · interaction none|human|llm|llm_or_human|review|async · interaction_prompt id · interaction_model str · min_answers int · await wait_all|best_effort · review_url str · posture human_required|agent_verdict_ok · merge_strategy squash|merge · merge_into str|id · max_turns int
- `tool` — description str · command str · script str · language js|node|py|python|python3|sh|bash · input id · output id · publish id · artifact_labels [tool] · await wait_all|best_effort · sandbox none|auto|{sandbox} · compress on|ultra|off · permission id · needs id|[id] · parallel_safe bool · goal str · postcondition str · policy required|recover|best_effort · recovery {recovery} · action id · connection id · params {params} · retry str · timeout str
- `params` (`params:` in tool) — entries `key: "value"`
- `recovery` (`recovery:` in tool) — max_repair_attempts int · max_agent_attempts int · model str · agent_tools [tool]
- `compute` — description str · input id · output id · publish id · artifact_labels [tool] · await wait_all|best_effort · expr {expr}
- `expr` (`expr:` in compute) — entries `field: "expression"`
- `subbot` — description str · source str · with with{} · output id · needs id|[id] · isolated bool
- `emit` — description str · event str · with with{}
- `wait` — description str · event str · timeout str · output id
- `await_answers` — description str · from str|id · timeout str
- `fail` — description str · code str|id · message str · resumable bool
- `workflow` — entry id · vars {vars} · attachments {attachments} · budget {budget} · resources {resources} · mcp {mcp} · compaction {compaction} · sandbox none|auto|{sandbox} · worktree auto|none · default_backend str · compress on|ultra|off · auto_memory on|off · loop_budget_guard on|off · repo_devbox on|off · workspace_checkpoint on|off · permission off|ask|deny · allow [str] · ask [str] · deny [str] · tool_policy [tool] · capabilities [tool] · skills [skill] · interaction none|human|llm|llm_or_human|review|async
- `budget` (`budget:` in workflow) — max_parallel_branches int · max_duration str · max_cost_usd num · max_tokens int · warn_tokens int · max_iterations int
- `resources` (`resources:` in workflow) — entries `name: <int> | ["member-a", "member-b"]`
- `compaction` (`compaction:` in workflow, agent, judge) — threshold num · preserve_recent int
- `memory` (`memory:` in agent, judge) — enabled bool · scope str · autoload [str] · read bool · write bool · pre_compact_inject bool · project_root bool (profile ≤1) · visibility "bot|project|cross_project|user|org|global"
- `mcp` (`mcp:` in workflow, agent, judge) — autoload_project bool · inherit bool · servers [id] · disable [id]
- `sandbox` (`sandbox:` in workflow, agent, judge, tool) — mode none|auto|inline · image str · build {sandbox.build} · user str · workspace_folder str · host_state auto|none · post_create str · env map · mounts [str|id] · network {sandbox.network}
- `sandbox.build` (`build:` in sandbox) — dockerfile str · context str · args map
- `sandbox.network` (`network:` in sandbox) — mode open|allowlist|denylist · preset str|id · inherit replace|append · rules [str|id]
- `cursors` (`cursors:` in agent, judge) — enabled bool — entries `cursor_name: ident | number | "string"`
- `fallback` (`fallbacks:` in agent, judge) — backend str · model str · provider str · on [id] · metered bool · action skip · when str
<!-- dsl-spec:end -->

## Validate in a loop, against the right build

Write, then `iterion validate --json <file>`: every finding carries its
source position and a `fix:` line, so correct the file at the position
given, never by guessing. Loop until `valid` is true, then `iterion diagram`
to check the shape, and only then run. From Claude Code the MCP
`local_validate` tool returns the same JSON. Validate with the build the bot
will run on (the `requires.iterion` floor in its manifest): a builtin or a
property a newer engine added compiles on that engine and dies on an older
one at validation or at its first evaluation — an unknown builtin name is
C040, an argument count the older evaluator cannot satisfy is C138.

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
C240–C242, C243 (`session: persist` in a fan-out body), and C244 (loop in a
parallel-branch body); bundle codes in C200–C234. Do not assume the numeric ranges are
contiguous. For bundles, also check
[`docs/bundles.md`](docs/bundles.md). For current CLI flags, use
`iterion <command> --help` and [`docs/cli-reference.md`](docs/cli-reference.md).
