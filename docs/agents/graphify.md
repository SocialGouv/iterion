# Knowledge graph — graphify

`graphify-out/` holds a queryable knowledge graph of this repository — Go/TS
symbols, docs and ADRs clustered into named communities — plus a
plain-language `GRAPH_REPORT.md` (god nodes, surprising connections,
suggested questions) and an interactive `graph.html`. It is **regenerable
from source and gitignored**; nothing in it is authoritative — the code and
`docs/` stay the truth.

## Ask it when

| Need | Command |
|---|---|
| Architecture / codebase questions, before grepping | `graphify query "<question>"`, `graphify path <A> <B>`, `graphify explain <node>` |
| After changing **code** | `graphify update .` — AST layer only, no LLM, no API cost. A local post-commit hook (`graphify hook install` — operator-side, **not part of the repo**) runs exactly this automatically. |
| After changing **docs** | Semantic refresh through the graphify skill (`/graphify <path> --update`). Self-sufficient with `GEMINI_API_KEY` set; otherwise the skill orchestrates it with host subagents. The semantic cache only re-pays changed files. |
| Reading it | Start from `graphify-out/GRAPH_REPORT.md`; browse `graphify-out/graph.html`. |

## Scope and freshness

`.graphifyignore` defines what enters the graph: code + curated docs, minus
vendored/worktree/generated noise and secrets — `.env` and `.secrets/` never
enter the graph nor the semantic extraction payload sent to the LLM API. The
report header carries the commit it was built from; compare it with
`git rev-parse HEAD` before trusting freshness.

## Installing (optional, operator tooling)

graphify is **not required** to contribute — every reflex above is guarded on
`graphify-out/graph.json` existing. To install it locally:

```bash
uv tool install "graphifyy[gemini]" --force   # the [gemini] extra is required for doc semantics
graphify install --platform <harness>          # claude, codex, kimi, pi, agents, …
graphify hook install                          # optional: post-commit AST refresh
```

Semantic extraction sends doc **content** to the configured LLM API — review
`.graphifyignore` before widening it.

## Known limits

- `.bot`/`.skill` files — this repo's central artifact — are not parsed by
  graphify (no extractor for the DSL). The engine map and
  [dsl-and-runtime.md](dsl-and-runtime.md) remain the reference there.
- Community names come from an LLM labeling pass (`graphify label .`); treat
  them as hints, not ground truth.
