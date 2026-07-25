# Reuse & iteration: groups, foreach, subbots

This page documents the composition + iteration constructs added on top of the
data-driven fan-out (`fan_out_each`) and named resources (`resources:`/`needs:`)
described in [dsl.md](dsl.md) and [routers.md](routers.md). Together they let a
`.bot` **reuse a cluster of nodes**, **iterate over a collection** (in parallel
or in order), and **compose whole bots**.

| Construct | Kind | What it does |
|-----------|------|--------------|
| `group` / `use` | compile-time macro | Reuse a named cluster of nodes + internal edges, instantiated N times |
| `as foreach name(item in coll)` | edge clause | Iterate a body once per element of a collection, **in order**, stateful |
| `router mode: fan_out_each` | router | One **parallel** branch per element, topologically scheduled (`key`/`depends_on`) — see [routers.md](routers.md) |
| `subbot` | node | Run another `.bot` as a nested run (a real run — it may contain loops) |
| `resources:` / `needs:` | concurrency | Named counting semaphores / instance-lease pool — see [dsl.md](dsl.md) |

---

## `group` — reusable node clusters (compile-time macro)

A `group` declares a parameterised cluster of nodes and the edges between them.
`use <group> as <prefix>` clones it: every node gets the id `<prefix>.<name>`,
the internal edges are rewired, and `{{params.X}}` is substituted from the
`with { ... }` bindings — all **at compile time**, so a group never reaches the
runtime. External edges address an instance's nodes via the dotted reference
`prefix.node`.

```
group review_block(target, max_fix):
  judge check:
    model: "anthropic/claude-sonnet-4-6"
    output: verdict
  tool fix:
    command: "echo fixing {{params.target}}"
    output: empty
  check -> fix when not approved
  fix -> check as fix_loop("{{params.max_fix}}")

use review_block as r1 with { target: "frontend", max_fix: "3" }

workflow w:
  entry: analyze
  analyze -> r1.check
  r1.check -> done when approved
```

- Params are substituted into tool `command`/`script`, router `over`, compute
  expressions, edge `when`/`as`/`with` — the template-bearing fields. Prompt
  *references* (names) are not parameter targets; parametrise prompt **text**
  via the bound values flowing through `with`.
- Two `use`s of the same group must use distinct prefixes; a colliding
  `prefix.node` id is caught by the standard duplicate-node check.
- Diagnostics: **C116** (unknown group), **C117** (unknown/missing param).

## `as foreach` — ordered, stateful iteration

A back-edge `... as foreach <name>(<item> in "<collection>")` re-enters its body
once per element of the collection, **in order**, stopping when the collection
is exhausted. The current element is exposed under the `each.<name>` namespace:

```
start -> proc
proc  -> proc as foreach scan(item in "{{outputs.start.items}}")
proc  -> done
```

Inside the body: `{{each.scan.item}}` (drills into object fields, e.g.
`{{each.scan.item.id}}`), `{{each.scan.index}}`, `{{each.scan.count}}`,
`{{each.scan.first}}`, `{{each.scan.last}}`, `{{each.scan.empty}}`.

- Use `foreach` for **ordered / stateful** iteration; use `fan_out_each` for an
  **independent parallel map**.
- The element is resolved against the current index *every iteration*. A node
  references `{{each.scan.item}}` in its own prompt/command (resolved at execution,
  regardless of which edge entered it), so the first iteration — entered via the
  forward edge — sees the element too. Data flow is **edge-driven**: if instead
  you thread the element through an edge `with { ... }`, bind it on *both* the
  forward-entry and the back-edge (they are separate data flows).
- An empty collection runs the body once (the forward-entry run) with
  `each.<name>.empty == true`; gate with `when not each.<name>.empty` if the
  body must not run on an empty list.
- `as foreach` and `as <loop>` are mutually exclusive on one edge (**C118**).

## `subbot` — run another `.bot` as a node

A `subbot` node runs a child `.bot` as a **real nested run** in the same store.
Because it is a full run (not a fan-out branch) the child **may contain loops** —
which is exactly why a per-element quality chain with retry loops is expressed
as a subbot rather than inlined into a `fan_out_each` branch.

```
subbot run_ticket:
  source: "child.bot"                 # resolved relative to the parent .bot
  with { issue: "{{outputs.plan.id}}" }   # → the child's vars
  output: ticket_verdict              # schema of the child's terminal output
  needs: worktree_slot                # optional resource lease for the child run
  isolated: true                      # child confines writes to its own run/worktree

plan -> run_ticket
run_ticket -> merge when validated
```

- The child's **terminal-node output** is mapped to `outputs.<subbot>.<field>`,
  so downstream `when`/`with` reference it normally.
- `needs:` leases a resource for the duration of the child run; the leased
  instance id (from a named pool) is passed to the child as `_lease_<resource>`
  so it can pick e.g. a worktree index.
- `isolated:` (default `false`) is the mirror of an agent/judge node's
  `readonly:`. See [Fanning subbots out in parallel](#fanning-subbots-out-in-parallel)
  below — it is what makes the parallel pattern legal.
- A depth guard bounds nested subbot recursion. Diagnostic **C119** (no `source`).
- The runtime invokes a host-supplied `SubbotRunner`; both the `iterion run`
  CLI (`pkg/cli/run.go`) and the studio's in-process engine
  (`pkg/runview/subbot.go`, wired for Launch AND Resume) provide one that
  compiles + runs the child sharing the parent store. Children carry
  `ParentRunID`, which is what folds them into the parent's card on the
  `/pipelines` board.

### Human gates inside a child (pause / park / resume)

A child `.bot` may contain `human` nodes. When the child pauses, the **child
run** persists `paused_waiting_human` (checkpoint + interaction) and the
parent's subbot node **parks**: the parent stays `running`, holding its branch
slot, while `AwaitSubbotTerminal` polls the store. The pending review surfaces
on the parent's pipeline-board card (with its exact child `run_id`/`node_id`);
answering it from the card's sidebar — or `iterion resume --run-id <child>` —
resumes the child **externally**, and once it reaches a terminal state the
parent picks up its terminal output (reconstructed from the child's
`node_finished` events) and continues. Several gates in one child, and gates
across parallel `isolated:` children, all work the same way — each pause is
just another answerable review on the parent's card. A child that ends
`failed`/`failed_resumable`/`cancelled` after its pause fails the parent's
subbot node (resuming the PARENT re-runs that subbot with a fresh child).

A human node may receive a `media_refs: json` input containing
`[{"path":"render.png","caption":"What to validate"}]`. Each path must name an
image, audio, or video file previously written by that run under
`$ITERION_ARTIFACT_FILES_DIR`. The runtime resolves the paths against the real
artifact manifest, discards invented/unsafe references, and the pipeline board
renders the validated media beside the active review form. Guided
`interaction: review` companions receive the same bounded media manifest and
can attach relevant files to each turn automatically. Binary bytes never ride
in the checkpoint or board JSON; only authenticated run-file references do.

Runnable demo: [examples/pipeline-board-demo](../examples/pipeline-board-demo/main.bot).

### Fanning subbots out in parallel

A subbot runs a **whole child `.bot`** that may do anything, so the
workspace-safety guard conservatively treats it as **mutating**: it refuses to
fan the *same* subbot template out concurrently (`max_parallel_branches > 1`)
over a `fan_out_each`, because two children could race the shared git
worktree/index. By default the only legal subbot fan-out is therefore
**serialized** (`max_parallel_branches: 1`).

`isolated: true` **opts out of that guard**. It is an author assertion that the
child **does not mutate the parent's shared workspace** — it confines every
write to its own run store (`runs/<child_run_id>/`) and/or its own worktree —
so N replays are safe to run at once. It is the exact analogue of `readonly:`
on an agent/judge node: the runtime cannot *prove* the child is
workspace-independent, so you certify it, and the guard then admits the
parallel fan-out (both `fan_out_each` and static `fan_out_all`).

```
router dispatch:
  mode: fan_out_each
  over: "{{outputs.plan.tickets}}"
  as: ticket

subbot run_ticket:
  source: "episode.bot"
  with { id: "{{outputs.dispatch.ticket.id}}" }
  isolated: true        # each child writes only to its own run store

dispatch -> run_ticket
run_ticket -> collect
```

> **Contract — use `isolated:` ONLY when true.** If the child *does* write the
> shared worktree (e.g. it commits code to the same checkout), leaving it
> serialized (`max_parallel_branches: 1`) or giving each child a real worktree
> (`needs: <worktree_pool>` + a child that keys its worktree off `_lease_*`) is
> the safe path. A false `isolated:` assertion re-opens exactly the parallel
> shared-workspace race the guard exists to prevent.

Parallel child runs are **data-race safe**: they share the parent's `RunStore`,
whose per-run artifacts/events/checkpoints are isolated by `run_id` and guarded
by a store-wide lock, and each child gets its own engine + (if `worktree: auto`)
its own distinct git worktree. Concurrency is bounded only by the parent's
`max_parallel_branches` (and any `needs:` lease pool). Two accounting/hygiene
boundaries are worth knowing before a heavy fan-out:

- **Budget does not compose across the boundary.** A child run is budgeted
  purely from its own `.bot`; the parent's `max_cost_usd` / `max_tokens` /
  `max_duration` do **not** bound the children, so N parallel children can
  collectively spend well past the parent's cap. Bound them with a per-child
  budget block, the run-level `--timeout`, and an explicit
  `max_parallel_branches`.
- **Same-target board writes are not cross-child atomic.** If several parallel
  subbots hold `board.*` capabilities and mutate the **same** issue or board
  config, the last writer wins (writes never corrupt the file, but an update
  can be lost). Give each child a distinct target, or serialize board-mutating
  subbots.

---

## Putting it together

The headline pattern — *map a sub-bot over a dependency DAG of work items, each
in its own worktree slot* — combines all of these:

```
resources:
  worktree_slot: 5

router dispatch:
  mode: fan_out_each
  over: "{{outputs.plan.tickets}}"
  as: ticket
  key: id
  depends_on: deps

dispatch -> run_ticket with { id: "{{outputs.dispatch.ticket.id}}" }

subbot run_ticket:
  source: "implement_ticket.bot"
  with { issue: "{{outputs.dispatch.ticket.id}}" }
  output: ticket_verdict
  needs: worktree_slot
  isolated: true                       # each child mutates only its leased worktree

run_ticket -> collect when validated   # collect: await: best_effort
```

`isolated: true` is what makes this parallel — the `worktree_slot` lease gives
each child a distinct worktree, and `isolated:` certifies that the child
confines its writes there rather than to the parent's shared checkout, so the
safety guard admits the concurrent replays. Without it the fan-out would be
rejected (see [Fanning subbots out in parallel](#fanning-subbots-out-in-parallel));
drop `isolated:` and add `max_parallel_branches: 1` if the children genuinely
share one workspace.

See [examples/composition/](../examples/composition/) for a runnable tool-only
demonstration.
