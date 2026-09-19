# The repository map and graph

**Audience.** An agent (or a contributor) about to look for something in
this tree, and a bot author who wants a workflow to orient itself without
paying for the search twice. Read it when a task starts with "where is…",
"what else uses…", or "what breaks if I change…".

Two layers, and they are deliberately different things.

| | Committed maps | The graph |
|---|---|---|
| What | Three markdown indexes under `docs/references/` | Nodes and edges over code, docs and workflows |
| Where | In the tree, committed | `.iterion/map/` — built on demand, gitignored |
| Cost | Read or grep like any file | 0.7 s to build, then instant |
| Written by | `task map:gen`, gated by `task map:check` | `iterion map build` |
| Good at | "What is this package for", "which skills exist" | "Who holds this seam", "is there a path", "who breaks" |

## The committed maps

- [`references/map-packages.md`](references/map-packages.md) — every Go
  package, its purpose in its own words, and the **interfaces it
  exposes**. That last column is the one worth the file: this project's
  architecture is that a new capability implements an existing seam
  rather than branching the core, so "which interfaces exist, and where"
  is the question that costs the most greps.
- [`references/map-docs.md`](references/map-docs.md) — every page and
  ADR, with the ADR's **status**, which is what says whether a decision
  still holds.
- [`references/map-bots.md`](references/map-bots.md) — every bundle, and
  every skill's `name: description` — the same pair a model sees before
  deciding to open the file.

They are generated: `task map:gen` writes them, and
`TestGeneratedMapsAreFresh` fails when they drift from the tree, so
freshness rides the required `test` check. **Do not hand-edit them** —
the next `map:gen` overwrites your edit, and the gate turns red in the
meantime.

## The graph

```bash
iterion map build                              # 9 811 nodes, 29 199 edges, 0.7 s
iterion map find MemoryStore                   # where is it, and what kind of thing is it
iterion map impact sym:pkg/knowledge.MemoryStore --depth 2
iterion map neighbours bot:review-pr
iterion map path pkg:pkg/server sym:pkg/knowledge.MemoryStore
iterion map rank --seeds sym:pkg/knowledge.MemoryStore --limit 10
```

Node ids are stable and greppable: `pkg:<dir>`, `file:<path>`,
`sym:<dir>.<Name>`, `doc:<path>`, `bot:<name>`, `skill:<bot>/<file>`,
`node:<bot>/<workflow>#<node>`.

Edges are `imports`, `contains`, `declares`, `calls`, `references`,
`links`, `flows`, `uses`. Two deserve a sentence:

- **`references`** — a symbol named without being called: a parameter
  type, a struct field. On an **interface** it is the only edge there is.
  A graph with only `calls` reports "nothing depends on this" about every
  seam in the architecture, which is how the first version of this one
  behaved before the relation existed.
- **`flows`** — the `.bot` DAG, taken from the compiler, not inferred
  from the text. `iterion diagram` renders the same fact from the same
  source; when the two disagree, one of them has a bug.

In a session, the same queries are MCP tools: `local_map_find`,
`local_map_neighbours`, `local_map_impact`, `local_map_path`.

## Using it from a `.bot`

Two ways, and the cheap one comes first.

**Read the committed maps as files.** They are in the tree the workflow
checked out, so a prompt can reference them and an agent node can open
them. Nothing to configure.

**Query the graph from a `tool` node** when the workflow needs a relation
rather than a description:

```
tool who_holds_the_seam:
  command: "iterion map impact sym:pkg/knowledge.MemoryStore --depth 2 --limit 20"

workflow main:
  entry: who_holds_the_seam
  who_holds_the_seam -> plan
```

The first `iterion map` call in a fresh checkout builds the graph
(seconds); later calls reuse the cache under `.iterion/map/`.

**There is deliberately no `map:` DSL block.** A block would be the
engine growing a branch for a single consumer; this project builds a seam
at the *second* variant, not the first. When a second workflow needs it,
that is the moment — not before.

## What this is not

Stated plainly, because a capability whose limits are undocumented gets
trusted past them.

- **Not semantic.** There is no embedding and no model call anywhere in
  the build. It finds what is *named*, not what is *meant*: a search for
  "authentication" finds things called that, not things that do that.
  The refusal is dated and has a reopening condition —
  [context-retrieval-state-of-the-art.md](references/context-retrieval-state-of-the-art.md).
- **Symbols are Go-only, and exported-only.** The docs and bot layers are
  language-agnostic; the symbol layer is not. Unexported helpers are not
  nodes.
- **Call and reference edges are resolved by NAME**, from each file's own
  import table — short of what `go/types` would prove. An unexported
  local with an exported name can produce one edge too many. The cost is
  a slightly noisy ranking, not a wrong answer about a path.
- **Doc links only connect pages that exist.** A broken link produces no
  edge; counting them is [#1233](https://github.com/SocialGouv/iterion/issues/1233)'s
  job, and inventing nodes for missing files would corrupt every "is
  there a path" answer.
- **`vendor/`, `.works/`, `.repos/`, `testdata/` and the run scratch are
  not in it.** A graph that indexed `vendor/` would be mostly `vendor/`.
- **It describes the tree it was built from.** The cache is keyed on a
  fingerprint of every `.go`, `.md`, `.bot` and `go.mod`; any change
  rebuilds all of it. It is never patched in place, because an index that
  describes a repository that no longer exists is worse than none.
- **It is not yet proven to pay for itself.** Whether it reduces what a
  run spends on orientation is measured by `iterion bench discovery`, and
  it is [#1482](https://github.com/SocialGouv/iterion/issues/1482) that
  decides. If it does not, it is deleted, not kept.
