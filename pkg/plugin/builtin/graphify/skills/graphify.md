---
name: graphify
description: Build and query a knowledge graph of this repo (code + docs) with the graphify CLI — use for architecture/orientation questions and "how does X connect to Y" before deep-diving.
---

# Knowledge graph (graphify)

graphify turns this workspace (code, docs, papers, even media) into a persistent
knowledge graph you can query for orientation and cross-cutting questions. Unlike
a pure code-symbol graph, it captures semantic relationships across docs and code
together. It runs as a CLI; results land on disk under `graphify-out/`.

## Build / refresh

The plugin's lifecycle handles building (`graphify {{workspace}}`) and
incremental updates (`graphify {{workspace}} --update`). If `graphify-out/` does
not exist yet, ask the operator to run `iterion plugin run graphify index`, or
invoke the build yourself if you have shell access:

```sh
graphify .            # full build → graphify-out/graph.json + GRAPH_REPORT.md
graphify . --update   # incremental rebuild after code changes
```

## Query (uses the existing graph.json)

```sh
graphify query "how does the executor dispatch to a backend?"   # BFS context
graphify query "..." --dfs                                      # trace one path
graphify path "Engine" "RunStore"                               # shortest path
graphify explain "ClawExecutor"                                 # plain-language node
```

## Read the artifacts directly

- `graphify-out/GRAPH_REPORT.md` — god nodes (most central symbols), surprising
  connections, suggested questions, community names. Read this first to orient.
- `graphify-out/graph.json` — the full node/edge graph (large) for programmatic
  traversal.

## Workflow

1. For an unfamiliar repo, read `graphify-out/GRAPH_REPORT.md` to learn the
   central modules and how they connect.
2. For a specific "how are A and B related" question, run `graphify path A B` or
   `graphify query "..."`.
3. After substantial changes, refresh with `--update` so later queries reflect
   the new code.

If the graph is missing or stale, say so and fall back to reading source.
