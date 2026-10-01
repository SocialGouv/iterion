# Knowledge graph — graphify

`graphify-out/` holds a queryable knowledge graph of this repository — Go/TS
symbols, docs and ADRs clustered into named communities — plus a
plain-language `GRAPH_REPORT.md` (god nodes, surprising connections) and an
interactive `graph.html`. It is **operator-local, regenerable and
gitignored**: nothing in it is authoritative, the code and `docs/` stay the
truth.

## Which tool for which question

| Question | Tool |
|---|---|
| How does X work, which docs explain this code, what is central | graphify |
| Where is X, who implements Y, what breaks if Z changes, path A→B, anything about bots or `.bot` files | the deterministic map — [repo-map-and-graph.md](../../repo-map-and-graph.md) |

## Querying it

- **MCP** — `graphify-mcp <repo>/graphify-out/graph.json` serves
  `query_graph`, `get_neighbors`, `shortest_path`, `get_node`, `god_nodes`,
  `get_community`, `graph_stats` and the PR tools (`get_pr_impact`,
  `list_prs`, `triage_prs`). It keeps the parsed graph warm and re-reads
  `graph.json` on the next call after the file changes. In Claude Code the
  tools load deferred: `ToolSearch` with
  `select:mcp__graphify__query_graph,mcp__graphify__get_neighbors,mcp__graphify__shortest_path`.
- **CLI** — `graphify query "<question>"`, `graphify path <A> <B>`,
  `graphify explain <node>`. Each call re-parses the whole graph, and writes
  a stamp under `graphify-out/cache/`.
- **Reading** — start from `graphify-out/GRAPH_REPORT.md`.

## Scope and freshness

`.graphifyignore` defines what enters the graph: code and curated docs, minus
Go tests (`*_test.go`: 37 % of the nodes and 70 % of the edges when measured
on 2026-10-01), vendored, worktree and generated noise, and secrets — `.env`
and `.secrets/` never enter the graph nor the payload sent to the LLM API.

**Nothing in this repository refreshes the graph.** The commit it was built
from is the last key of `graph.json` (`tail -c 200 graphify-out/graph.json`)
and the report's "Built from commit" line; compare it with `git rev-parse
HEAD` before trusting an answer about recent code.

| After changing | Refresh |
|---|---|
| code | `graphify update .` — AST only, no LLM, no API cost. Deleted code passes on its own; after widening `.graphifyignore`, add `--force` — the update refuses to drop nodes of files it did not re-extract. |
| docs | the graphify skill's semantic refresh (`/graphify <path> --update`), which sends the changed docs' content to the configured LLM API (`GEMINI_API_KEY`) and re-pays only changed files. |

`graphify hook install` adds git `post-commit`/`post-checkout` hooks (and
registers a `graph.json` merge driver in `.git/config` and the tracked
`.gitattributes`), but the hooks miss `pull`, `merge` and `reset`, skip linked
worktrees, and a refresh started from a sandboxed agent shell dies with that
shell. To keep the graph fresh automatically, run `graphify update` from
outside the sandbox — for example a session-start hook that compares the built
commit with HEAD and starts the update through `systemd-run --user`.

## Installing (optional)

graphify is **not required** to contribute; every reflex in the tree is guarded
on `graphify-out/graph.json` existing.

```bash
uv tool install "graphifyy[gemini,mcp]" --force   # [gemini] for doc semantics, [mcp] for the server
graphify install --platform <harness>              # the skill: claude, codex, kimi, pi, agents, …
```

Build it once through the skill (`/graphify .`); `graphify update .` keeps the
code layer fresh from then on. Semantic extraction sends doc **content** to the
configured LLM API — review `.graphifyignore` before widening it.

## Known limits

- `.bot`/`.skill` files — this repo's central artifact — are not parsed by
  graphify; the deterministic map and [dsl-and-runtime.md](../engine/dsl-and-runtime.md)
  are the reference there.
- Community names come from an LLM labelling pass (`graphify label .`): treat
  them as hints, not ground truth.
