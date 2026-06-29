---
name: code-knowledge-graph
description: Query the repo-falcon code knowledge graph (symbol lookup, call graphs, package architecture, change impact) via the falcon MCP tools before reading or editing code.
---

# Code knowledge graph (repo-falcon)

This workspace has a **deterministic code knowledge graph** built by
repo-falcon and served over MCP. Before grepping the whole tree or guessing how
symbols connect, query the graph — it answers structural questions in
milliseconds from a pre-indexed snapshot (no LLM, no full-text scan).

The tools are exposed under the `falcon` server. Use them like any other MCP
tool. (Iterion namespaces them `mcp.falcon.*` for the claw backend and
`mcp__falcon__*` for the claude_code backend — the agent sees them in its tool
list automatically when the repo-falcon plugin is enabled.)

## When to use which tool

- **`falcon_symbol_lookup(name)`** — find every definition and call site of a
  symbol. Use this before renaming/changing a function: it lists callers
  (impact) and callees deterministically, where grep would miss indirect refs.
- **`falcon_file_context(path)`** — a file's dependencies (what it imports/calls)
  and dependents (what would break if you change it). Use before editing a file.
- **`falcon_architecture()`** — package structure + dependency direction; flags
  cycles. Use to orient at the start of a task.
- **`falcon_package_info(name)`** — files, symbols, and deps of one package.
- **`falcon_search(query)`** — structural search for files/symbols/packages by
  name. Cheaper and more precise than text grep for "where is X defined".
- **`falcon_path(src, dst)`** — shortest dependency path between two
  symbols/packages. Use to understand how two areas are coupled.
- **`falcon_hubs()`** — highest-degree nodes (the load-bearing symbols). Touch
  these carefully; changes ripple widely.
- **`falcon_communities()`** — named clusters and their members — a map of the
  codebase's natural modules.
- **`falcon_insights()`** — surprising connections / bridge nodes worth knowing
  before a refactor.

## Workflow

1. At the start of a code task, call `falcon_architecture()` to orient.
2. For any symbol you plan to change, call `falcon_symbol_lookup(name)` to learn
   its blast radius **before** editing.
3. For any file you plan to edit, call `falcon_file_context(path)` to see its
   dependents.
4. Use `falcon_search` instead of broad `grep` when locating definitions.

The snapshot is refreshed by the plugin's lifecycle (`index`/`refresh`). If a
lookup returns nothing for a symbol you know exists, the snapshot may be stale —
note it and fall back to reading the source directly.
