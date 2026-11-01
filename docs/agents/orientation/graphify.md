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

The payoff is in the follow-up, not the first call. `query_graph` answers a
broad match with a flat node list truncated to a ~2000-token budget — a
lease/watch-health question matched 7 043 nodes and printed 72 (measured
2026-10-24). So query narrow (two or three domain terms), read the files and
symbols it names from the first lines, then expand with `get_node` /
`get_neighbors` / `shortest_path` and `get_community`. A bare label can be
ambiguous: retry with `get_node` or `get_neighbors` as `<path>::<symbol>`,
taking the path from the error's own list. `shortest_path` and `graphify
path` fuzzy-match bare labels, ignore `path::symbol` and node ids, and on a
"no path" `shortest_path` drops its ambiguity warning — trust them only
between labels `get_node` showed unambiguous, and reach for `get_neighbors`
or the deterministic map otherwise. Cite the community and the file, and
confirm in the code before asserting anything.

## Scope and freshness

`.graphifyignore` defines what enters the graph: code and curated docs, minus
tests named as such (Go `*_test.go`, 37 % of the nodes and 70 % of the edges
when measured on 2026-10-01; JS/TS `*.test.ts(x)`, `*.test.mjs`,
`*.spec.ts(x)`; Python `test_*.py`), vendored, worktree and generated noise,
and secrets — `.env` and `.secrets/` never enter the graph nor the payload
sent to the LLM API.

**Nothing committed to this repository refreshes the graph** — refresh is
operator-local. On this machine a Claude Code session-start hook does it:
compare the built commit with HEAD, start `graphify update` through
`systemd-run --user`, and print the position in the session context. The
built commit is the last key of `graph.json`
(`tail -c 200 <primary-checkout>/graphify-out/graph.json` — a linked worktree
has no `graphify-out/` of its own; read the primary checkout's copy, first
row of `git worktree list`) and the report's "Built from commit" line. Stale
does not mean useless: trust the graph for code older than the gap, and grep
for anything newer than it.

| After changing | Refresh |
|---|---|
| code | `graphify update .` — AST only, no LLM, no API cost. Deleted code passes on its own; after widening `.graphifyignore`, add `--force` — the update refuses to drop nodes of files it did not re-extract. |
| docs | the graphify skill's semantic refresh (`/graphify <path> --update`), which sends the changed docs' content to the configured LLM API (`GEMINI_API_KEY`) and re-pays only changed files. |

Run both from the primary checkout when you sit in a linked worktree — an
update or build inside the worktree creates a divergent `graphify-out/` that
no hook refreshes and no server serves.

`graphify hook install` adds git `post-commit`/`post-checkout` hooks (and
registers a `graph.json` merge driver in `.git/config` and the tracked
`.gitattributes`), but the hooks miss `pull`, `merge` and `reset`, skip linked
worktrees, and a refresh started from a sandboxed agent shell dies with that
shell — which is why the automatic refresh runs from a session-start hook
through `systemd-run --user`, outside any agent sandbox.

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
