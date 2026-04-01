# Iterion

Workflow orchestration engine with a custom DSL (`.iter` files).

**Module:** `github.com/SocialGouv/iterion`

## Build & Test

All commands must be run through `devbox run` (Go and tooling are managed by devbox):

```bash
devbox run -- task build          # Build binary → ./iterion
devbox run -- task test           # Run unit tests
devbox run -- task test:e2e       # Run end-to-end tests
devbox run -- task test:race      # Tests with race detector
devbox run -- task lint           # go fmt + go vet
devbox run -- task check          # lint + test
devbox run -- task clean          # Remove build artifacts
```

Or directly with Go:

```bash
devbox run -- go build -o iterion ./cmd/iterion
devbox run -- go test ./...
```

## Project Structure

- `cmd/iterion/` — CLI entry point (hand-rolled command parsing, no framework)
- `cli/` — CLI command implementations (init, validate, run, inspect, resume, diagram, editor)
- `parser/` — Lexer, parser, tokens, diagnostics for the .iter DSL
- `ast/` — Abstract Syntax Tree definitions
- `ir/` — Intermediate Representation compilation and validation
- `runtime/` — Workflow execution engine (branch scheduling, events, budget)
- `store/` — Run persistence (JSON-based, versioned artifacts, events.jsonl)
- `model/` — Executor registry (GoaiExecutor), schema validation, event hooks
- `recipe/` — Recipe handling for tool adapters and execution policies
- `tool/` — Tool registry, policies, and adapters
- `delegate/` — Delegation backends (claude_code, codex subprocess execution)
- `server/` — HTTP server for editor backend
- `editor/` — Web UI (React/Vite/TypeScript with XYFlow)
- `log/` — Leveled logger (error, warn, info, debug, trace)
- `benchmark/` — Metrics collection and reporting
- `unparse/` — IR back to .iter serialization
- `astjson/` — AST JSON utilities
- `e2e/` — End-to-end test suite
- `examples/` — Example .iter workflow files
- `grammar/` — DSL grammar specification (EBNF)

## Key Dependencies

- Go 1.25.0
- `github.com/zendev-sh/goai` v0.4.0 — AI model SDK (vendored in `vendor/`)

## Architecture

`.iter` files are parsed into an **AST**, compiled into an **IR** (directed graph of nodes and edges), validated, then executed by the **runtime** engine. Nodes include Agent (LLM), Judge, Router, Join, Human (pause/resume), Tool, and terminal nodes (Done/Fail). The runtime supports parallel branch scheduling, loop detection, budget enforcement, and resumable execution.

### Compilation Pipeline

```
.iter source → Lexer (indent-sensitive tokens) → Parser (recursive-descent) → AST
  → ir.Compile() → IR Workflow (nodes + edges + schemas + prompts + budget)
  → ir.Validate() → Diagnostics (codes C001–C019: reachability, routing, cycles, etc.)
  → runtime.Engine.Run() → execution with events, budget, and persistence
```

### Node Types

| Type | Description |
|------|-------------|
| **Agent** | LLM node with tools, structured I/O, optional delegation (claude_code, codex) |
| **Judge** | LLM node producing verdicts (typically no tools) |
| **Router** | Deterministic routing: `fan_out_all` (parallel) or `condition` (branching) |
| **Join** | Branch aggregation: `wait_all` or `best_effort` strategy, with `require` list |
| **Human** | Pause/resume: `pause_until_answers`, `auto_answer`, or `auto_or_pause` mode |
| **Tool** | Direct shell command execution (no LLM) |
| **Done** | Terminal: workflow success |
| **Fail** | Terminal: workflow failure |

### DSL Quick Reference

**Top-level blocks:** `vars:`, `prompt <name>:`, `schema <name>:`, node declarations (`agent`, `judge`, `router`, `join`, `human`, `tool`), `workflow <name>:`

**Edge syntax:**
```
src -> dst                              # default edge
src -> dst when <field>                 # conditional (boolean field from src output)
src -> dst when not <field>             # negated condition
src -> dst as loop_name(5)              # bounded loop (max 5 iterations)
src -> dst with {field: "{{ref}}"}      # data mapping
```

**Reference syntax:** `{{input.field}}`, `{{vars.name}}`, `{{outputs.node_id}}`, `{{outputs.node_id.field}}`, `{{artifacts.name}}`

**Budget block:** `max_parallel_branches`, `max_duration`, `max_cost_usd`, `max_tokens`, `max_iterations`

### Key Interfaces

- `NodeExecutor` (`runtime/engine.go`) — `Execute(ctx, node, input) → (output, error)`, abstraction between engine and execution backend
- `GoaiExecutor` (`model/executor.go`) — production `NodeExecutor` impl wrapping goai, handles LLM calls, tools, retries, delegation
- `Backend` (`delegate/delegate.go`) — delegation interface for external CLI agents (claude_code, codex)
- `RunStore` (`store/store.go`) — file-backed persistence for runs, events, artifacts, interactions
- `Workflow` (`ir/ir.go`) — compiled execution unit with Nodes, Edges, Schemas, Prompts, Vars, Loops, Budget

### Error Handling

- **RuntimeError** (`runtime/errors.go`) — structured error with `ErrorCode`, `Message`, `NodeID`, `Hint`, `Cause`
  - Codes: `NODE_NOT_FOUND`, `NO_OUTGOING_EDGE`, `LOOP_EXHAUSTED`, `BUDGET_EXCEEDED`, `EXECUTION_FAILED`, `WORKSPACE_SAFETY`, `TIMEOUT`, `CANCELLED`, `JOIN_FAILED`, `RESUME_INVALID`
- **Diagnostics** (`ir/validate.go`) — compile-time warnings/errors with codes C001–C019 (unknown refs, routing issues, unreachable nodes, undeclared cycles, etc.)
- **Sentinel errors**: `ErrRunPaused` (resumable), `ErrRunCancelled`, `ErrBudgetExceeded`

### Store & Persistence

```
<store-dir>/runs/<run_id>/
  run.json              # Run metadata (status, inputs, checkpoint)
  events.jsonl          # Timestamped events (one per line, monotonic seq)
  artifacts/<node>/<v>.json   # Versioned node outputs
  interactions/<id>.json      # Human interaction records (questions/answers)
```

**Run statuses:** `running` → `paused_waiting_human` → `finished` | `failed` | `cancelled`

**Key event types:** `run_started`, `node_started`, `llm_request`, `llm_retry`, `tool_called`, `artifact_written`, `human_input_requested`, `run_paused`, `run_resumed`, `join_ready`, `edge_selected`, `budget_warning`, `budget_exceeded`, `run_finished`, `run_failed`

### Concurrency

- **Fan-out/join**: Router `fan_out_all` spawns parallel branches, Join aggregates results
- **Semaphore**: buffered channel enforces `max_parallel_branches` budget
- **Workspace safety**: only one mutating branch allowed (agents/humans with tools); multiple read-only branches OK
- **Shared budget**: mutex-protected token/cost/duration tracking across all branches

## CLI Commands

```
iterion init [dir]                      # Scaffold new project
iterion validate <file.iter>            # Parse and validate workflow
iterion run <file.iter> [flags]         # Execute workflow (--var, --recipe, --timeout, --store-dir)
iterion inspect [--run-id] [--events]   # View run state and events
iterion resume --run-id --file --answers-file  # Resume paused run with human answers
iterion diagram <file.iter> [--view]    # Generate Mermaid diagram (compact|detailed|full)
iterion editor [--port] [--dir]         # Launch visual workflow editor
iterion version                         # Print version
```

Global flags: `--json` (machine output), `--help`

## Testing Patterns

- `tmpStore()` — creates temp directory-backed RunStore for test isolation
- `compileFixture()` — loads and compiles .iter files from `examples/` directory
- **Scenario executor** (`e2e/e2e_test.go`) — configurable stub with `.on(nodeID, handler)` for per-node behavior
- Table-driven subtests with standard `testing` package
- `task test:live` — runs E2E with real Claude/Codex CLIs (requires API keys)

## CI/CD

- **tests.yml** — on push/PR: gofmt, go vet, unit tests, e2e tests
- **release.yml** — on git tags (v*): multi-platform builds (linux/darwin/windows × amd64/arm64), GitHub release
- **version.yml** — conventional changelog via release-it, version from `package.json`

## Conventions

- No external linter beyond `go fmt` and `go vet`
- Tests use the standard `testing` package — no test frameworks
- Binary name is `iterion` (ignored in .gitignore)
- Store data lives in `.iterion/` (ignored in .gitignore)
- Hand-rolled CLI — no framework (simple loop + switch in `cmd/iterion/main.go`)
- `CGO_ENABLED=0`, version/commit injected via ldflags from `package.json` + git
- Single external dependency: goai (vendored)
- Event-driven observability via `events.jsonl` — no structured logging library
- Output abstraction: `Printer` (`cli/output.go`) with human and JSON modes

<!-- BEGIN FALCON -->
## RepoFalcon Code Knowledge Graph

This repository has a pre-built code knowledge graph. You MUST use the `falcon_*` MCP tools to understand the codebase before making changes.

**Mandatory workflow:**
1. At the start of every task, call `falcon_architecture` to understand the project structure
2. Before modifying any file, call `falcon_file_context` with its path to see what depends on it
3. Before renaming or refactoring a symbol, call `falcon_symbol_lookup` to find all usages
4. To understand a package's role, call `falcon_package_info` instead of reading files one by one
5. Use `falcon_search` instead of grep/glob for finding symbols, files, or packages by name
6. After major refactoring (renamed packages, moved files), call `falcon_refresh` to re-index

These tools are faster and more accurate than grep — they use a pre-computed dependency graph with full symbol resolution.

If the MCP tools are unavailable, read `.falcon/CONTEXT.md` for a static architecture summary as a fallback.
<!-- END FALCON -->
