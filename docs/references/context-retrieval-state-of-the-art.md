# Context retrieval — the state of the art, mapped onto iterion

**Audience.** Anyone about to propose an index, a repo map, a code graph
or an embedding store — for the agents that work *on* this repository, or
for the bots iterion runs *for* users. Read this before writing the
proposal, not after: four of the five findings below eliminate an option
that looks obvious from the outside.

This page follows the shape of
[external-methodologies.md](external-methodologies.md): what the outside
work validates, what iterion imports, what it deliberately does
differently, and what it refuses — with the date and the condition that
would reopen the refusal.

---

## What this repository already spends

Measured on the operator's own store, 2026-09-19, over the 200 most
recently created runs — reproduce with:

```bash
devbox run -- ./iterion bench discovery --last 200 --output -
```

| | |
|---|---|
| Runs read / with a tool call | 200 / 186 |
| Nodes / never called a tool | 772 / 331 |
| Tool calls, classified | 5 600 of 6 604 (**85 %**) |
| discovery · mutation · other · unknown | **4 121** · 1 219 · 260 · 1 004 |
| Calls before a node's first write | 1 956, of which **1 355** were reads or searches |
| Tool output pulled into contexts | **15.2 MiB** |
| Tool wall time | **40 min 44 s** |
| Tokens spent by nodes that never wrote a byte | **2 151 453 of 8 523 614 attributable — 25.2 %**, carried by 54 of the 361 such nodes (the rest are `tool` nodes, which spend none) |

And the size of what all that is searching:

| Corpus | Size |
|---|---|
| Go files (excluding `vendor/`) | 3 798 |
| Markdown tracked by git | 941 files, 10.6 MB (≈ 2.6 M tokens) |
| of which `docs/*.md` | 311 files, 5.2 MB, including 109 ADRs |
| of which bot skills | 186 files, 1.6 MB |
| `.bot` workflows | 165 tracked (`git ls-files '*.bot'`), 78 outside `testdata/` |
| Agent instruction tree | `CLAUDE.md` 15.5 KB **injected every turn** + `docs/agents/` 180 KB on demand |

**One number bounds the rest: 639 of 772 nodes carry no recorded token
spend.** Usage is written once per node, at node end; the split *inside*
a node — still looking versus now writing — is reported by no backend
path, so nothing here imputes it. See
[pkg/benchmark/discovery](../../pkg/benchmark/discovery) for what the
event stream can and cannot attribute.

---

## Five findings that decide the design

### 1. Progressive disclosure is the cheapest win, and it also improves accuracy

Anthropic's Tool Search takes a full tool library from 77 000 to 8 700
tokens (−85 %) *and* moves Opus 4.5 from 79.5 % to 88.1 % on MCP
evaluation tasks; code execution with MCP reports 150 000 → 2 000 tokens
(−98.7 %); an Agent Skill costs ~100 tokens of name and description until
the task matches it. Cloudflare, Anthropic and Cursor converged on the
same answer independently.

**iterion already does this, by hand.** `CLAUDE.md` is a router of
one-line entries; `docs/agents/README.md` states the contribution rule
that keeps it short; a skill exposes only its frontmatter until it is
needed ([pkg/skilllib/frontmatter.go](../../pkg/skilllib/frontmatter.go));
`memory:` has an index and `autoload:`
([pkg/knowledge/iface.go](../../pkg/knowledge/iface.go)). Anything
proposed below has to beat *that* baseline, not a naive one.

### 2. Grep versus an index: the sign flips with repository size

A replication measured MCP retrieval at **4.1× more tokens than grep on a
33-file repo, and 86 % cheaper on a 249-file one** — same model, same
tasks, opposite sign. Weigh it as what it is: a weekend blog replication
on two author-written repositories, 32 runs, one open model. It is the
pivot for "this repository sits past the crossover", so that conclusion
is a hypothesis this project measures for itself rather than a result it
inherits. Claude Code's deliberate no-index posture is a
cost-curve position, not a dogma: it also buys freshness (no index lag),
no second attack surface, and no embedding of proprietary code.

At 3 798 Go files and 941 markdown files, this repository is far past the
scale of either side of that replication. That is an argument for
measuring, not for assuming: the `bench discovery` numbers above exist so
the claim can be checked here rather than imported.

### 3. A code graph pays *procedural* scaffolds most — and a `.bot` is one

RepoGraph (ICLR 2025) reports +32.8 % relative on SWE-bench-Lite, and
notes the gain is **larger on procedural frameworks than on agent-based
ones**, because a deterministic flow can exploit a plug-in that an
improvising agent merely rediscovers. SWE-ContextBench (2026) is the
counterweight: agents with embedding search or graph navigation do *not*
consistently beat a plain mini-agent, and the bottleneck it identifies is
**consolidation** — agents touch 70 %+ of the relevant code but retain
only 50–70 % of it in their final context.

Read together, these two results point at the same place: the payoff is
highest where the flow is declared in advance. **That is exactly what a
`.bot` is** — a DAG the engine knows at compile time, with per-node tool
grants. iterion is the scaffold shape where a graph earns its keep, and
the graph of the workflow itself is a fact iterion owns and nobody else
has.

### 4. What costs money is LLM extraction — and this repository has no API key

Microsoft's LazyGraphRAG work puts GraphRAG indexing at ≈ **1 000×** a
vector index; the $1 544-versus-$1.45-per-million-tokens pair is a
third-party derivation from the same AP-News benchmark at 2024 pricing,
not a Microsoft figure. LazyGraphRAG, LightRAG and KET-RAG all exist to
defer or avoid that extraction pass.

iterion's own runs go through OAuth subscription backends, not API keys —
the constraint is written into this repo's `.graphifyignore`, which
excludes all markdown from the operator's trial graph *because* semantic
extraction would need a key. So the only index this project can finance
is a **deterministic** one: AST, imports, markdown links, the `.bot` DAG.
That is a constraint, and it happens to be the right one — deterministic
means exact, reproducible, and gateable in CI, which an LLM-extracted
graph is not.

### 5. Embedded graph databases churn; do not couple the engine to one

Kuzu was archived in October 2025 after its company was acquired, leaving
LadybugDB as a community fork; RedisGraph reached end of life in 2023,
leaving FalkorDB (SSPL) as its successor. Two stranded user bases in two
years. A flat artifact plus a traversal written in Go does not die with
its vendor — and for a single repository's graph, recursive traversal
over a few hundred thousand edges is not the hard part.

---

## The families, and what each would cost here

| Approach | What it is | What it would cost iterion | Verdict |
|---|---|---|---|
| **Hand-written router** (`CLAUDE.md` + `docs/agents/`) | Curated one-line index, read on demand | Already paid; drifts silently, no witness | **Keep**, and give it a generated twin |
| **Repo map** (Aider: tree-sitter + personalised PageRank, budget-packed) | Ranked signatures under a token budget | Cheap; tree-sitter approximates what Go's own parser knows exactly | **Import the idea**, not the parser |
| **LSP symbol tools** (Serena) | Precise go-to-definition / references via a language server | Reactive only; no docs, no `.bot`; useless to a bot in a sandbox | Complementary, not the commons |
| **SCIP / LSIF index** | Compiler-grade cross-repo symbol index | Real precision, heavy toolchain, symbols only | Not now; revisit if cross-repo lands |
| **Graph database** (Neo4j, FalkorDB, Kuzu/Ladybug) | Cypher over a property graph | A service (or a fork risk) for one repo's graph | **Refused** — finding 5 |
| **GraphRAG family** (GraphRAG, LightRAG, LazyGraphRAG) | LLM-extracted entity graph + community summaries | Needs an API key this project does not have | **Refused** — finding 4 |
| **Vector / hybrid index** (claude-context et al.) | Embeddings + BM25, Merkle-diff re-index | Same key problem; plus staleness and an embedding of private code | **Refused** — finding 4 |
| **Temporal memory graphs** (Zep/Graphiti, Mem0, Cognee) | Facts with validity intervals, for agent memory | Solves *what an agent keeps*, not *what it finds* | Belongs to the memory epic, tracked apart |
| **Generated wiki** (DeepWiki, OpenWiki) | A repo wiki an agent retrieves from | The 2026 middle layer iterion lacks | **Import the shape**, deterministic content only |

---

## What iterion imports

- **A generated, committed, gated index** — the deterministic half of the
  repo-map and generated-wiki ideas, following this repo's own
  `dsl:gen`/`dsl:check`, `openapi:gen`/`openapi:check` pattern, where a Go
  test fails if the committed artifact drifts from its source. It exists:
  [map-packages.md](map-packages.md), [map-docs.md](map-docs.md),
  [map-bots.md](map-bots.md), written by `task map:gen`, held to the tree
  by `task map:check`. **Its benefit is not yet measured** — the `bench
  discovery` delta that decides whether it stays is issue #1482, and the
  rule stands: an index that does not reduce the orientation cost is
  deleted, not kept.
- **Budget packing**: an index is useless if reading it costs what it
  saves. Rankings are packed under a stated token budget, the way Aider
  packs a repo map.
- **Coverage as a first-class output**: every ratio this project publishes
  about retrieval states its denominator and its unknown share. The
  classifier in `pkg/benchmark/discovery` reports what it could *not*
  name, because a heuristic that hides its blind spots is worse than none.

## What iterion deliberately does differently

- **Deterministic extraction only.** Go's own parser, markdown links, the
  compiled `.bot` graph. No tree-sitter approximation where the real
  parser is in the standard library; no LLM pass.
- **One artifact, two audiences.** The same committed index serves the
  agent working on the repository and the bot running inside a sandbox.
  An MCP-only or IDE-only surface would serve the first and abandon the
  second — and the second is the product.
- **The workflow graph is part of the graph.** No external tool models
  `.bot` nodes, edges, skills and bot dependencies. iterion does, at
  compile time, for free — `iterion map build` records the compiler's own
  DAG as `flows` edges, and `iterion diagram` renders the same fact
  independently, which is how that half is checked.

The graph exists: `iterion map build`, 9 830 nodes and 31 352 edges over
this tree in 0.7 s, byte-identical over two cold builds, no database and
no dependency outside the standard library. Its import edges were checked
against `go list -deps` and its workflow edges against `iterion diagram` —
a graph verified only against itself proves nothing.

**And it is a heuristic, which an adversarial pass proved rather than
assumed.** Name-based resolution was documented here as costing "one
extra edge in a ranking rather than a wrong answer about a path"; that
sentence was false. A selector's field name was being matched against the
own package's symbols — 1 714 such edges — and `map path` reported a
two-hop route between two symbols that never touch. The class is closed
and the claim is now the weaker true one:
[repo-map-and-graph.md](../repo-map-and-graph.md#what-this-is-not) says a
path is a lead to verify, not a proof.

## What is refused, and what would reopen it

**Refused on 2026-09-19: embeddings and LLM-based extraction**, for the
repository's own discoverability commons. Reopening requires *both*:

1. a BYOK credential this project may spend on indexing
   ([docs/byok.md](../byok.md)), and
2. a `bench discovery` measurement showing the deterministic index left a
   gap that a semantic one closes.

Stated as a dated refusal rather than an omission, so the next session
re-opens it on evidence instead of re-discovering the question.

---

## Sources

Anthropic, *Equipping agents for the real world with Agent Skills* ·
Anthropic, *Code execution with MCP* and the Tool Search Tool ·
Ouyang et al., *RepoGraph* (ICLR 2025) · *SWE-ContextBench* (2026) ·
Microsoft Research, *LazyGraphRAG* · Aider, *Building a better repository
map with tree-sitter* · Serena (oraios/serena) · LangChain, *OpenWiki* ·
LadybugDB (the Kuzu fork) · vectorize.io, *Mem0 vs Zep (Graphiti)*.

Vendor benchmarks are quoted as vendor claims: the independent audits of
LongMemEval and LoCoMo found published agent-memory scores overstated
(one 96.6 % collapsing to 66.8 %), so no number above is load-bearing
except the ones measured on this repository.
