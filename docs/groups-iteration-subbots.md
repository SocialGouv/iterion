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
- The element is resolved against the current index *every iteration* — a node
  references `{{each.scan.item}}` in its own prompt/command, so the first
  iteration (entered via the forward edge) sees the element too.
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

plan -> run_ticket
run_ticket -> merge when validated
```

- The child's **terminal-node output** is mapped to `outputs.<subbot>.<field>`,
  so downstream `when`/`with` reference it normally.
- `needs:` leases a resource for the duration of the child run; the leased
  instance id (from a named pool) is passed to the child as `_lease_<resource>`
  so it can pick e.g. a worktree index.
- A depth guard bounds nested subbot recursion. Diagnostic **C119** (no `source`).
- The runtime invokes a host-supplied `SubbotRunner`; the `iterion run` CLI
  wires one that compiles + runs the child sharing the parent store. (Studio /
  cloud wiring + parent↔child run linkage in the UI are follow-ons.)

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

run_ticket -> collect when validated   # collect: await: best_effort
```

See [examples/composition/](../examples/composition/) for a runnable tool-only
demonstration.
