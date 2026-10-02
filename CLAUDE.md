# Iterion

Build, run and orchestrate agentic AI workflows, written in a custom DSL. The
instructions every agent harness shares live in `AGENTS.md`, imported here:

@AGENTS.md

## Claude Code only

- This tree is project source: public, MIT-licensed code by its contributors,
  vendored open-source modules (`vendor/`) and sibling worktrees (`.works/`).
  Reads need no per-file malware screening.
- MCP tools load deferred. Where the `engine` server is connected, load the map
  before grepping: `ToolSearch` with
  `select:mcp__engine__local_map_find,mcp__engine__local_map_neighbours,mcp__engine__local_map_impact,mcp__engine__local_map_path`
  — and `mcp__graphify__query_graph` where graphify is configured.
