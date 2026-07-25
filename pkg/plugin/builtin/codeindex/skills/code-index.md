---
name: code-index
description: Query the codeindex repo index (symbols, callers, references, indexed search, hotspots, complexity, architecture rules) via the codeindex MCP tools before reading or grepping code.
---

# Code index (codeindex)

This workspace has a **deterministic repo index** built by codeindex and served
over MCP. Before grepping the whole tree or guessing how symbols connect, query
the index — it answers structural questions from pre-built artifacts, with no
LLM and no full-text scan.

The tools are exposed under the `codeindex` server. The server is **pinned to
this workspace**, so every tool's `repo` argument is optional — omit it.
(Iterion namespaces them `mcp.codeindex.*` for the claw backend and
`mcp__codeindex__*` for the claude_code backend; they appear in your tool list
automatically when the plugin is enabled.)

## Orient before you read

- **`repo_map(budgetTokens)`** — the densest single read of an unfamiliar
  codebase: highest-PageRank files with their key exported signatures, fitted to
  a token budget. Start here.
- **`scan_summary()`** — file count, language histogram, HEAD commit. Confirms
  what you are actually looking at.
- **`workspaces()`** — monorepo packages, their dependency graph and a
  topological build order. Run this before assuming the repo is one unit.
- **`mermaid(module)`** — module graph as a diagram, optionally focused.

## Find things

- **`search(query)`** — ranked lexical search (BM25) over symbol names, path
  segments and markdown headings. This is the right tool for *"where is auth
  handled?"*. Prefer it over `grep` for concept-shaped questions.
- **`grep(pattern)`** — regex over file contents when you need exact literal
  matches. Returns bounded, sorted `(file, line, text)` hits.
- **`find_symbol(namePath)`** — declarations by name or `Class/method` path;
  `includeBody: true` returns the source, so you often need no file read at all.
- **`symbols_overview(file)`** — every symbol declared in ONE file, in
  declaration order. The fastest way to understand a file without reading it.

## Before you change anything

- **`find_references(name)`** — the blast radius, in three labeled tiers: `defs`,
  line-precise `callSites`, and file-level `referencingFiles`. Confidence drops
  across the tiers and the labels say so — trust `callSites` over
  `referencingFiles`, which can include homonyms.
- **`callers(name)`** — the call-site index for one symbol.
- **`dead_code()`** — candidates in two honest tiers: `unreferenced` (nothing
  binds or mentions it) and `uncalled` (referenced in a type or re-export
  position but never called). Neither tier is proof; verify before deleting.

## Judge risk

- **`hotspots()`** — churn × size: where changes and defects concentrate.
- **`coupling()`** — files that repeatedly change together. Hidden dependencies
  that no import statement reveals — check this before a "small" edit.
- **`complexity(file)`** — cyclomatic estimates, most complex first;
  `risk: true` ranks complexity × churn instead.
- **`check_rules(rules)`** — validate forbidden edges, import cycles and orphans
  against the link-graph. A CI-gate-shaped answer to "did I break the layering?"

## Edit symbolically

`replace_symbol_body`, `insert_after_symbol`, `insert_before_symbol` operate on
AST line spans by name path, so you do not have to reproduce surrounding context
the way a text patch does. Supply the full declaration including indentation.
Ambiguity is an error listing the candidates — qualify with `file`.

## Remember across runs

`write_memory` / `read_memory` / `list_memories` persist short markdown notes
under `.codeindex/memories/`. Call `list_memories()` early and read what looks
relevant; write back the project map, build commands or conventions you had to
work out, so the next run does not re-derive them.

## Workflow

1. `list_memories()`, then `repo_map()` to orient.
2. `search()` to locate the area; `find_symbol(includeBody)` to read just the
   declaration you need.
3. For anything you plan to change: `find_references()` for blast radius, then
   `coupling()` if the file is load-bearing.
4. Edit — symbolically where it fits.
5. `write_memory()` for anything worth not re-deriving.

## Trust and staleness

Everything is **static analysis, not an LSP**: bindings are extracted and
resolved, never executed. Edge and reference tiers carry explicit confidence
labels — read them rather than treating every hit as certain.

The index is built and refreshed by the plugin's lifecycle (`index`/`refresh`),
incrementally. If a lookup returns nothing for a symbol you know exists, the
index may be stale or the walk may have been capped (`scan_summary` reports
`capped`) — say so and fall back to reading the source directly.
