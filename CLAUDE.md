# Iterion

Workflow orchestration engine with a custom DSL (`.iter` files).

**Module:** `github.com/SocialGouv/iterion`

## Build & Test

All commands must be run through `devbox run` (Go and tooling are managed by devbox):

```bash
devbox run -- task build          # Build binary → ./iterion
devbox run -- task test           # Run unit tests
devbox run -- task test:e2e       # Run end-to-end tests (stub executor)
devbox run -- task test:live       # Run all live e2e tests (requires API keys, uses -tags live)
devbox run -- task test:live:review  # Run session continuity review/fix live test
devbox run -- task test:live:kanban  # Run kanban board plan/implement/review live test
devbox run -- task test:live:full    # Run exhaustive DSL coverage live test
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

The Go code follows the standard `cmd/` + `pkg/` layout. Three top-level Go directories:

- `cmd/iterion/` — CLI entry point (Cobra-based, one file per command)
- `pkg/` — All library code, grouped by role (see breakdown below)
- `e2e/` — End-to-end test suite (kept at root by Go convention)

Other top-level directories: `editor/` (React/Vite frontend), `examples/` (.iter workflows), `docs/` (incl. `docs/grammar/` EBNF and `docs/references/` patterns/diagnostics), `scripts/`, `vendor/`.

### `pkg/` breakdown

- `pkg/dsl/` — DSL pipeline (parser → AST → IR)
  - `parser/` — Lexer, parser, tokens, diagnostics for the .iter DSL
  - `ast/` — AST definitions and `MarshalFile`/`UnmarshalFile` (JSON encoder for AST)
  - `ir/` — Intermediate Representation compilation and validation
  - `unparse/` — IR back to .iter serialization
  - `types/` — Shared enums (transports, field types, session/router/await/interaction modes)
- `pkg/backend/` — Execution stack (LLM + tools)
  - `model/` — Executor registry (`ClawExecutor`), schema validation, event hooks
  - `delegate/` — Delegation backends (claude_code, codex subprocess; claw in-process)
  - `tool/` — Tool registry, policies, adapters
  - `mcp/` — MCP server lifecycle, configuration, health checks
  - `recipe/` — Recipe handling for tool adapters and execution policies
  - `cost/` — Cost estimation and budgeting
  - `llmtypes/` — LLM SDK abstraction (`LLMTool`, `FatalToolError`, `ModelCapabilities`)
- `pkg/runtime/` — Workflow execution engine (branch scheduling, events, budget, recovery dispatch)
- `pkg/store/` — Run persistence (JSON-based, versioned artifacts, events.jsonl)
- `pkg/server/` — HTTP server for editor backend (embedded static UI)
- `pkg/cli/` — CLI command implementations (init, validate, run, inspect, resume, diagram, editor, version)
- `pkg/benchmark/` — Metrics collection and reporting
- `pkg/log/` — Leveled logger (error, warn, info, debug, trace) — public so e2e tests can construct it
- `pkg/internal/` — Internal utilities (not importable outside `pkg/`)
  - `appinfo/` — Build-time version/commit injection (LDFLAGS targets)

## Key Dependencies

- Go 1.25.0
- `claw-code-go` (sibling repo, vendored under `vendor/claw-code-go/`) — native multi-provider LLM client. iterion uses `claw-code-go/pkg/api.Client.StreamResponse` directly via `pkg/backend/model/generation.go` for in-process LLM calls (anthropic + openai validated; bedrock/vertex/foundry available but untested).

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
| **Router** | Routing node with 4 modes: `fan_out_all`, `condition`, `round_robin`, `llm` (see `docs/routers.md`) |
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

### Backend selection

Three backends are wired:
- `claw` (default, in-process) — recommended for read-only LLM nodes (judges, reviewers, planners). Use any provider model claw supports, e.g. `model: "openai/gpt-5.4-mini"` or `model: "anthropic/claude-sonnet-4-6"`.
- `claude_code` — recommended for nodes that need real tool/shell access (implementers, fixers).
- `codex` — **supported but discouraged**. The IR compiler emits `C030` for any node using it. Reasons: codex SDK cannot configure its tool set (`AllowedTools`/`CanUseTool` don't gate the built-in shell), it tends to fill its own context window, and its iterion integration is less ergonomic. Live tests (`task test:live`) still exercise codex for compatibility coverage; new workflows should not adopt it.

### Key Interfaces

- `NodeExecutor` (`pkg/runtime/engine.go`) — `Execute(ctx, node, input) → (output, error)`, abstraction between engine and execution backend
- `ClawExecutor` (`pkg/backend/model/executor.go`) — production `NodeExecutor` impl, dispatches to `delegate.Backend` (claude_code, codex, claw); for direct LLM calls (e.g. `human` nodes) it uses `pkg/backend/model/generation.go` (`GenerateTextDirect` / `GenerateObjectDirect`) which calls `claw-code-go/pkg/api.Client.StreamResponse` and aggregates the streaming response.
- `Backend` (`pkg/backend/delegate/delegate.go`) — delegation interface for execution backends. CLI-based backends (claude_code, codex) shell out; the `claw` backend (`pkg/backend/model/claw_backend.go`) calls claw-code-go directly via the generation engine above.
- `RunStore` (`pkg/store/store.go`) — file-backed persistence for runs, events, artifacts, interactions
- `Workflow` (`pkg/dsl/ir/ir.go`) — compiled execution unit with Nodes, Edges, Schemas, Prompts, Vars, Loops, Budget

### Error Handling

- **RuntimeError** (`pkg/runtime/errors.go`) — structured error with `ErrorCode`, `Message`, `NodeID`, `Hint`, `Cause`
  - Codes: `NODE_NOT_FOUND`, `NO_OUTGOING_EDGE`, `LOOP_EXHAUSTED`, `BUDGET_EXCEEDED`, `EXECUTION_FAILED`, `WORKSPACE_SAFETY`, `TIMEOUT`, `CANCELLED`, `JOIN_FAILED`, `RESUME_INVALID`
- **Diagnostics** (`pkg/dsl/ir/validate.go`) — compile-time warnings/errors with codes C001–C019 (unknown refs, routing issues, unreachable nodes, undeclared cycles, etc.)
- **Sentinel errors**: `ErrRunPaused` (resumable), `ErrRunCancelled` (resumable with checkpoint), `ErrBudgetExceeded`
- **Resumable failures**: Most runtime failures produce `failed_resumable` status with a checkpoint. See `docs/resume.md` for the exhaustive matrix.

### Store & Persistence

```
<store-dir>/runs/<run_id>/
  run.json              # Run metadata (status, inputs, checkpoint)
  events.jsonl          # Timestamped events (one per line, monotonic seq)
  artifacts/<node>/<v>.json   # Versioned node outputs
  interactions/<id>.json      # Human interaction records (questions/answers)
  report.md             # Generated by `iterion report` — chronological run report
```

The checkpoint embedded in `run.json` is the authoritative source for resume — events are observational only. See `docs/persisted-formats.md` for field semantics.

**Run statuses:** `running` → `paused_waiting_human` → `finished` | `failed` | `failed_resumable` | `cancelled`

**Key event types:** `run_started`, `node_started`, `llm_request`, `llm_retry`, `tool_called`, `artifact_written`, `human_input_requested`, `run_paused`, `run_resumed`, `join_ready`, `edge_selected`, `budget_warning`, `budget_exceeded`, `run_finished`, `run_failed`

### Resume from Failed/Cancelled Runs

The engine saves a checkpoint after every successful node execution. When a run fails or is cancelled, the checkpoint is preserved, enabling `iterion resume` to restart from the failing node without re-executing upstream nodes.

**Resumable statuses:** `paused_waiting_human` (needs answers), `failed_resumable` (automatic retry), `cancelled` (user-interrupted, checkpoint preserved)

**All failure scenarios are resumable** except:
- `FailNode` reached (intentional workflow termination → `failed`, no checkpoint)
- First node fails before any checkpoint exists (→ `failed`, must restart)

Common resumable failures: transient LLM errors (rate limit, timeout), budget exceeded (increase budget + resume), schema validation errors (fix workflow + `--force`), context timeout/cancellation, fan-out branch failures, router failures.

**`--force` flag**: allows resume even when the `.iter` source has changed (e.g., bug fix). Without `--force`, a hash mismatch produces an error.

See `docs/resume.md` for the exhaustive failure matrix.

### Concurrency

- **Fan-out/join**: Router `fan_out_all` spawns parallel branches, Join aggregates results
- **Semaphore**: buffered channel enforces `max_parallel_branches` budget
- **Workspace safety**: only one mutating branch allowed (agents/humans with tools); multiple read-only branches OK
- **Shared budget**: mutex-protected token/cost/duration tracking across all branches

## Authoring `.iter` workflows that touch real code

**Before writing or amending any `.iter` workflow that has the power to
commit code, read [docs/workflow_authoring_pitfalls.md](docs/workflow_authoring_pitfalls.md).**
It captures hard-won lessons about Goodhart's law in workflow design,
the façade pattern that LLM agents reach for when goals are
under-specified, and concrete rules for prompts, scanners, and judges
that resist metric-gaming. Skipping it has a real cost — the
goai → claw-code-go migration ran for 3 hours and produced a
96%-parity-reported façade because these lessons weren't yet codified.

## CLI Commands

```
iterion init [dir]                      # Scaffold new project
iterion validate <file.iter>            # Parse and validate workflow
iterion run <file.iter> [flags]         # Execute workflow (--var, --recipe, --timeout, --store-dir)
iterion inspect [--run-id] [--events]   # View run state and events
iterion resume --run-id --file [--answers-file] [--force]  # Resume paused/failed/cancelled run
iterion diagram <file.iter> [--view]    # Generate Mermaid diagram (compact|detailed|full)
iterion editor [--port] [--dir]         # Launch visual workflow editor
iterion report --run-id <id> [--store-dir] [--output]  # Generate chronological run report
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
- CLI built with Cobra (`github.com/spf13/cobra`) — one file per command in `cmd/iterion/`
- `CGO_ENABLED=0`, version/commit injected via ldflags from `package.json` + git
- External LLM SDK: claw-code-go (vendored), used directly via `pkg/api`
- Event-driven observability via `events.jsonl` — no structured logging library
- Output abstraction: `Printer` (`pkg/cli/output.go`) with human and JSON modes

