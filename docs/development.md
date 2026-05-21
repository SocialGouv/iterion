[← Documentation index](README.md) · [← Iterion](../README.md)

# Development

This page is for contributors working on the Iterion codebase itself.

## Prerequisites

- [Devbox](https://www.jetify.com/devbox) — portable dev environment (installs Go, Task, Node)
- [direnv](https://direnv.net/) — auto-activates the Devbox shell

```bash
eval "$(direnv hook bash)"   # or: eval "$(direnv hook zsh)"
direnv allow
```

The repository also includes a `.devcontainer/` configuration for VS Code / GitHub Codespaces.

## Build & Test

```bash
task build          # → ./iterion
task test           # Unit tests
task test:e2e       # End-to-end tests (stub executor)
task test:live      # Live e2e tests (requires API keys)
task test:race      # Tests with race detector
task lint           # go fmt + go vet
task check          # lint + test
task studio:dev     # Start studio in dev mode (HMR)
task studio:build   # Build studio frontend
```

Or directly with Go:

```bash
go build -o iterion ./cmd/iterion
go test ./...
```

## Project Structure

The Go code follows the standard `cmd/` + `pkg/` layout:

```
iterion/
├── cmd/
│   ├── iterion/         # CLI entry point (Cobra, one file per command — run, server, runner, sandbox, …)
│   └── iterion-desktop/ # Wails v2 desktop wrapper (proxy AssetServer over pkg/server)
├── pkg/
│   ├── dsl/             # DSL pipeline
│   │   ├── parser/         # Lexer, recursive-descent parser, diagnostics
│   │   ├── ast/            # AST definitions and JSON marshaling
│   │   ├── ir/             # IR compilation and validation (~60 diagnostic codes, C001–C086 sparse — incl. cursors C083–C086)
│   │   ├── unparse/        # IR → .iter serialization
│   │   ├── expr/           # Expression evaluator for compute nodes and quoted `when` clauses
│   │   ├── workflowfile/   # Workflow source loading + SHA-256 hashing (drives `iterion resume` change detection)
│   │   └── types/          # Shared enums (transports, session/router modes…)
│   ├── backend/         # Execution stack (LLM + tools)
│   │   ├── model/          # Executor registry, schema validation, event hooks
│   │   ├── delegate/       # Delegation backends (claude_code, codex, claw)
│   │   ├── tool/           # Tool registry, policies, adapters (incl. privacy_filter / privacy_unfilter)
│   │   ├── mcp/            # MCP server lifecycle, configuration, health checks
│   │   ├── recipe/         # Recipe handling for tool adapters and policies
│   │   ├── cost/           # Cost estimation and budgeting
│   │   ├── llmtypes/       # LLM SDK abstraction
│   │   ├── detect/         # Credential auto-detection (OAuth, API keys, AWS/GCP) — feeds the studio BackendStatusPill
│   │   └── tooldisplay/    # Human-readable rendering of tool calls for the run console / report
│   ├── runtime/         # Workflow execution engine (scheduling, budget, recovery, worktree finalization)
│   ├── dispatcher/      # Long-running dispatcher: actor + tracker adapters (native/github/forgejo) + native kanban store
│   ├── bundle/          # `.botz` bundle loader (workflow + skills + recipes + prompts + attachments)
│   ├── sandbox/         # Per-run container isolation (Docker/Podman/Kubernetes drivers + CONNECT proxy)
│   ├── store/           # File-backed persistence (runs, events, artifacts) + Mongo/S3 in cloud mode
│   ├── server/          # HTTP server: studio + run console + cloud REST/WS API
│   ├── runner/          # Cloud-mode runner pod consumer loop (NATS JetStream → execution)
│   ├── queue/           # NATS JetStream message contract & dispatch schema
│   ├── cloud/           # Cloud-mode helpers (Prometheus metrics registry, …)
│   ├── runview/         # Editor backend: WS event broker for live run streaming
│   ├── git/             # Editor backend: status / diff / log for the modified-files panel
│   ├── config/          # Runtime config (env vars + YAML, Mongo/NATS/S3/Sandbox/Runner sections)
│   ├── auth/            # Operator authentication primitives (SSO, session cookies) for cloud-mode endpoints
│   ├── identity/        # Operator identity types shared between auth, cloud, and dispatcher
│   ├── secrets/         # Secret resolution (env / file / KMS) shared across backends and sandbox
│   ├── cli/             # CLI command implementations
│   ├── benchmark/       # Metrics collection and reporting
│   ├── log/             # Leveled logger
│   └── internal/        # Internal utilities (appinfo, mongoutil for cloud-mode Mongo store, proc helpers)
├── studio/              # Web UI (React/Vite/TypeScript + XYFlow)
├── examples/            # Reference .iter workflows + companion docs
├── sdks/typescript/     # @iterion/sdk — typed CLI wrapper for Node/Deno/Bun
├── e2e/                 # End-to-end test suite (stub + live)
├── charts/iterion/      # Helm chart (server + runner Deployments, KEDA scaling, sandbox RBAC) — published to oci://ghcr.io/socialgouv/charts/iterion
├── docker/              # Cloud-mode container helpers (LLM CLI install, MinIO init)
├── docs/                # Format specs, references, ADRs, sandbox, privacy, observability
├── scripts/             # Build helpers
└── vendor/              # Vendored Go modules (incl. claw-code-go)
```

**Key dependencies:** Go 1.26.0 and [`claw-code-go`](https://github.com/SocialGouv/claw-code-go) (vendored under `vendor/github.com/SocialGouv/claw-code-go/`) — a multi-provider LLM client. iterion uses `claw-code-go/pkg/api.Client.StreamResponse` directly for in-process LLM calls (Anthropic and OpenAI validated; Bedrock/Vertex/Foundry available).
