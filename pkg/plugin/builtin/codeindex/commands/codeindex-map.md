---
description: Orient in this codebase from the codeindex index — structure, entry points and where the weight sits.
argument-hint: "[module or directory to focus on]"
---

# Map this codebase

Build an orientation briefing using the `codeindex` MCP tools. The server is
pinned to this workspace, so omit the `repo` argument everywhere.

If `$ARGUMENTS` names a module or directory, scope the whole briefing to it
(pass it as `scope`, or as `module` to `mermaid`); otherwise cover the repo.

1. `list_memories()` — if a previous run already wrote a project map, read it
   and build on it instead of starting over.
2. `scan_summary()` — size, languages, HEAD commit. Note if `capped` is true:
   the walk hit its file limit and everything below is partial.
3. `workspaces()` — if this is a monorepo, report the packages, their dependency
   direction and any cycle. Do not treat the repo as one unit when it is not.
4. `repo_map(budgetTokens: 2000)` — the load-bearing files and their exported
   signatures.
5. `mermaid()` — the module graph.

Then write the briefing:

- **What this is** — one paragraph, grounded in what you actually saw.
- **Structure** — the modules/packages and what each owns.
- **Entry points** — where execution starts, and the main flows.
- **Where the weight sits** — the highest-centrality files from `repo_map`, and
  what makes them load-bearing.
- **Diagram** — the mermaid graph.
- **Unknowns** — anything the index could not settle. Say so plainly rather
  than filling the gap with a plausible guess.

Finish by offering to `write_memory("project-map", …)` so the next run starts
from this instead of re-deriving it.
