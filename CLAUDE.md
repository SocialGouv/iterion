# Iterion

Workflow orchestration engine with a custom DSL (`.iter` files).

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
