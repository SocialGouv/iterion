# Iterion

Workflow orchestration engine with a custom DSL (`.iter` files).

## Build & Test

```bash
task build          # Build binary → ./iterion
task test           # Run unit tests
task test:e2e       # Run end-to-end tests
task test:race      # Tests with race detector
task lint           # go fmt + go vet
task check          # lint + test
task clean          # Remove build artifacts
```

Or directly with Go:

```bash
go build -o iterion ./cmd/iterion
go test ./...
```

## Project Structure

- `cmd/iterion/` — CLI entry point
- `cli/` — CLI command implementations (validate, run, inspect, resume, diagram)
- `parser/` — Lexer, parser, tokens, diagnostics for the .iter DSL
- `ast/` — Abstract Syntax Tree definitions
- `ir/` — Intermediate Representation compilation and validation
- `runtime/` — Workflow execution engine (branch scheduling, events)
- `store/` — Run persistence (JSON-based, versioned artifacts)
- `model/` — Executor registry and schema validation
- `recipe/` — Recipe handling for tool adapters and execution policies
- `tool/` — Tool registry, policies, and adapters
- `e2e/` — End-to-end test suite
- `examples/` — Example .iter workflow files
- `grammar/` — DSL grammar specification (EBNF)

## Key Dependencies

- Go 1.23.8
- `github.com/zendev-sh/goai` — local dependency at `/home/devbox/goai`

## Architecture

`.iter` files are parsed into an **AST**, compiled into an **IR** (directed graph of nodes and edges), validated, then executed by the **runtime** engine. Nodes include Agent (LLM), Judge, Router, Join, Human (pause/resume), Tool, and terminal nodes (Done/Fail). The runtime supports parallel branch scheduling, loop detection, budget enforcement, and resumable execution.

## Conventions

- No external linter beyond `go fmt` and `go vet`
- Tests use the standard `testing` package — no test frameworks
- Binary name is `iterion` (ignored in .gitignore)
- Store data lives in `.iterion/` (ignored in .gitignore)
