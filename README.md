# Iterion

**Declarative workflow orchestration engine for AI-driven tasks.**

Iterion lets you author complex, multi-agent LLM workflows as readable `.iter` files — combining agents, judges, routers, human-in-the-loop interactions, parallel branching, bounded loops, and budget enforcement into a single, auditable execution graph.

## Table of Contents

- [Features](#features)
- [Quick Start](#quick-start)
- [Installation](#installation)
- [The `.iter` DSL](#the-iter-dsl)
  - [Top-Level Declarations](#top-level-declarations)
  - [Node Types](#node-types)
  - [Workflows](#workflows)
  - [Edges and Control Flow](#edges-and-control-flow)
  - [Template Expressions](#template-expressions)
- [CLI Reference](#cli-reference)
  - [validate](#validate)
  - [run](#run)
  - [inspect](#inspect)
  - [resume](#resume)
  - [diagram](#diagram)
- [Architecture](#architecture)
  - [Compiler Pipeline](#compiler-pipeline)
  - [Runtime Engine](#runtime-engine)
  - [Persistence Layer](#persistence-layer)
- [Recipes](#recipes)
- [Project Structure](#project-structure)
- [Development](#development)
  - [Prerequisites](#prerequisites)
  - [Building](#building)
  - [Testing](#testing)
- [Examples](#examples)
- [License](#license)

---

## Features

- **Declarative DSL** — Human-readable `.iter` files with indentation-based syntax (YAML/Python-style)
- **Multi-agent orchestration** — Chain agents, judges, routers, and joins into complex workflows
- **Human-in-the-loop** — Pause execution for human input, resume with answers
- **Parallel branching** — Fan-out via routers, converge with join nodes (wait_all / best_effort)
- **Bounded loops** — Retry and refinement cycles with configurable iteration limits
- **Budget enforcement** — Caps on tokens, cost (USD), duration, and iterations
- **Structured I/O** — Typed schemas for inputs and outputs with enum constraints
- **Artifact versioning** — Per-node, per-iteration versioned outputs persisted to disk
- **Event sourcing** — Append-only JSONL event log for full observability and replay
- **Pause/resume** — Checkpoint-based suspension and resumption of workflow runs
- **Mermaid diagrams** — Auto-generate visual workflow diagrams from `.iter` files
- **Recipe system** — Bundle workflows with presets (vars, prompts, budgets) for benchmarking
- **Tool policies** — Allowlist-based access control with exact, namespace, and wildcard matching
- **Provider-agnostic** — Supports multiple LLM providers (Claude, OpenAI, etc.) via goai

---

## Quick Start

```bash
# Build the binary
go build -o iterion ./cmd/iterion

# Validate a workflow
./iterion validate examples/pr_refine_single_model.iter

# Run a workflow
./iterion run examples/pr_refine_single_model.iter \
  --var pr_title="Fix auth bug" \
  --var review_rules="No SQL injection" \
  --var compliance_rules="OWASP top 10"

# Inspect run results
./iterion inspect --run-id run_1234567890 --events

# Generate a Mermaid diagram
./iterion diagram examples/pr_refine_single_model.iter --detailed
```

---

## Installation

### From Source

```bash
git clone https://github.com/iterion-ai/iterion.git
cd iterion
go build -o iterion ./cmd/iterion
```

**Requirements:**
- Go 1.23.8+
- [goai](https://github.com/zendev-sh/goai) — LLM provider abstraction layer

### Dev Container

The repository includes a `.devcontainer/` configuration for VS Code / GitHub Codespaces:

```jsonc
// .devcontainer/devcontainer.json
{
  "image": "jetpackio/devbox:latest",
  "features": { "ghcr.io/devcontainers/features/node:1": { "version": "lts" } }
}
```

---

## The `.iter` DSL

Iterion workflows are written in a declarative, indentation-significant DSL. The formal grammar is defined in [`grammar/iterion_v1.ebnf`](grammar/iterion_v1.ebnf).

### Top-Level Declarations

| Declaration | Purpose |
|-------------|---------|
| `vars` | Global variables with types and optional defaults |
| `prompt <name>` | Reusable prompt templates with `{{...}}` interpolation |
| `schema <name>` | Typed data schemas for structured agent I/O |
| `agent <name>` | LLM agent node — executes prompts, uses tools, produces structured output |
| `judge <name>` | LLM judge node — evaluates and produces verdicts (no tools by default) |
| `router <name>` | Branching node — `fan_out_all` or `condition` mode |
| `join <name>` | Convergence node — `wait_all` or `best_effort` strategy |
| `human <name>` | Human interaction node — pauses for external input |
| `tool <name>` | Direct tool/command execution node |
| `workflow <name>` | Workflow graph definition with entry point, budget, and edges |

### Node Types

**Agent** — The primary execution unit. Calls an LLM with system/user prompts, uses tools, and returns structured output:

```
agent reviewer:
  model: "claude-sonnet-4-20250514"
  input: review_request
  output: review_result
  system: review_system
  user: review_user
  session: fresh
  tools: [git_diff, read_file, search_codebase]
  tool_max_steps: 10
```

**Judge** — An evaluator that produces a verdict. Semantically identical to an agent but intended for assessment tasks:

```
judge compliance_check:
  model: "claude-sonnet-4-20250514"
  input: plan_compliance_request
  output: compliance_verdict
  system: compliance_system
  user: compliance_user
  session: fresh
```

**Router** — Branches execution into parallel or conditional paths:

```
router dispatch:
  mode: fan_out_all    # or: condition
```

**Join** — Converges parallel branches:

```
join merge:
  strategy: wait_all   # or: best_effort
  require: [branch_a, branch_b]
  output: merged_result
```

**Human** — Pauses the workflow for human input:

```
human approval:
  input: approval_request
  output: approval_response
  instructions: approval_prompt
  mode: pause_until_answers
  min_answers: 1
```

**Tool** — Executes a command directly:

```
tool run_tests:
  command: "go test ./..."
  output: test_result
```

### Workflows

A workflow ties nodes together with an entry point, optional budget, and edges:

```
workflow my_workflow:
  vars:
    input_text: string
    max_retries: int = 3

  entry: first_agent

  budget:
    max_duration: "30m"
    max_cost_usd: 10
    max_tokens: 400000
    max_iterations: 5
    max_parallel_branches: 2

  first_agent -> reviewer with {
    context: "{{outputs.first_agent}}"
  }

  reviewer -> done when approved
  reviewer -> first_agent when not approved as retry_loop(3)
```

### Edges and Control Flow

Edges connect nodes and support conditions, loops, and data mapping:

```
# Unconditional edge with data mapping
agent_a -> agent_b with {
  input_field: "{{outputs.agent_a}}"
}

# Conditional edge
judge -> done when approved
judge -> retry_agent when not approved

# Bounded loop
judge -> retry_agent when not approved as my_loop(5)

# Template references in data mapping
node_a -> node_b with {
  context: "{{outputs.node_a}}",
  config: "{{vars.my_var}}",
  prior: "{{artifacts.published_name}}"
}
```

### Template Expressions

Templates use `{{...}}` interpolation with the following references:

| Reference | Description |
|-----------|-------------|
| `{{vars.name}}` | Workflow variable |
| `{{input.field}}` | Current node's input field |
| `{{outputs.node_id}}` | Output of a previously executed node |
| `{{outputs.node_id.field}}` | Specific field from a node's output |
| `{{artifacts.name}}` | Published artifact by name |

### Schemas

Schemas define typed structures for agent inputs and outputs:

```
schema review_result:
  approved: bool
  summary: string
  issues: string[]
  confidence: string [enum: "low", "medium", "high"]
```

Supported types: `string`, `bool`, `int`, `float`, `json`, `string[]`.

---

## CLI Reference

All commands support the `--json` flag for machine-readable output.

### validate

Parse, compile, and validate a workflow file without executing it:

```bash
./iterion validate <file.iter>
./iterion validate examples/pr_refine_single_model.iter --json
```

Reports errors and warnings with diagnostic codes, file positions, and descriptions.

### run

Execute a workflow:

```bash
./iterion run <file.iter> [flags]
```

| Flag | Description |
|------|-------------|
| `--var key=value` | Set workflow variable (repeatable) |
| `--recipe <file>` | Apply a recipe preset file |
| `--run-id <id>` | Use a specific run ID (default: auto-generated) |
| `--store-dir <dir>` | Run store directory (default: `.iterion`) |
| `--timeout <duration>` | Global timeout (e.g., `30m`, `1h`) |

### inspect

Inspect run state and history:

```bash
./iterion inspect [flags]
```

| Flag | Description |
|------|-------------|
| `--run-id <id>` | Inspect a specific run |
| `--events` | Include event log |
| `--full` | Show full artifact contents |
| `--store-dir <dir>` | Run store directory |

Without `--run-id`, lists all runs in the store.

### resume

Resume a paused workflow run:

```bash
./iterion resume --run-id <id> --file <file.iter> [flags]
```

| Flag | Description |
|------|-------------|
| `--answer key=value` | Provide an answer (repeatable) |
| `--answers-file <file>` | Load answers from a JSON file |
| `--store-dir <dir>` | Run store directory |

### diagram

Generate a Mermaid diagram from a workflow file:

```bash
./iterion diagram <file.iter> [--detailed]
```

Output can be pasted into any Mermaid-compatible renderer (GitHub Markdown, Mermaid Live Editor, etc.).

---

## Architecture

### Compiler Pipeline

Iterion uses a classic three-stage compiler architecture:

```
.iter file
    │
    ▼
┌─────────┐     ┌─────────┐     ┌──────────┐
│  PARSE  │────▶│ COMPILE │────▶│ VALIDATE │
│ Lexer + │     │ AST→IR  │     │  Static  │
│ Parser  │     │ Resolve │     │  Checks  │
└─────────┘     └─────────┘     └──────────┘
    │                │                │
    ▼                ▼                ▼
   AST              IR          Diagnostics
```

1. **Parse** (`parser/`) — Lexer tokenizes the `.iter` file; recursive-descent parser produces an AST
2. **Compile** (`ir/compile.go`) — Transforms AST to IR, resolves template references, binds schema/prompt refs
3. **Validate** (`ir/validate.go`) — Static analysis: reachability, edge routing correctness, condition validity

### Runtime Engine

The engine (`runtime/engine.go`) walks the IR graph, executing nodes according to their type:

```
┌─────────────────────────────────────────────────┐
│                 Runtime Engine                   │
│                                                  │
│  ┌──────┐   ┌───────┐   ┌────────┐   ┌──────┐  │
│  │Agent │   │ Judge │   │ Router │   │ Join │  │
│  │      │   │       │   │        │   │      │  │
│  │ LLM  │   │ LLM   │   │fan_out │   │merge │  │
│  │+tools│   │verdict│   │  cond  │   │wait  │  │
│  └──────┘   └───────┘   └────────┘   └──────┘  │
│                                                  │
│  ┌──────┐   ┌───────┐   ┌────────┐   ┌──────┐  │
│  │Human │   │ Tool  │   │  Done  │   │ Fail │  │
│  │pause │   │ exec  │   │terminal│   │error │  │
│  └──────┘   └───────┘   └────────┘   └──────┘  │
│                                                  │
│  Budget Tracker │ Event Emitter │ Artifact Store │
└─────────────────────────────────────────────────┘
```

**Run lifecycle:**
`running` → `paused_waiting_human` → `running` → `finished` | `failed` | `cancelled`

### Persistence Layer

All run state is persisted to disk under a configurable store directory (default: `.iterion/`):

```
.iterion/runs/
  <run_id>/
    run.json                     # Run metadata & checkpoint
    events.jsonl                 # Append-only event log
    artifacts/
      <node_id>/
        0.json, 1.json, ...     # Versioned node outputs
    interactions/
      <interaction_id>.json      # Human Q&A exchanges
```

See [`docs/persisted-formats.md`](docs/persisted-formats.md) for the full specification.

---

## Recipes

Recipes bundle a workflow with preset configurations for comparison and benchmarking:

```json
{
  "name": "fast_review",
  "workflow_ref": {
    "name": "pr_refine_single_model",
    "path": "examples/pr_refine_single_model.iter"
  },
  "preset_vars": {
    "review_rules": "Focus on security only"
  },
  "prompt_pack": {
    "review_system": "You are a security-focused reviewer."
  },
  "budget": {
    "max_duration": "10m",
    "max_cost_usd": 5.0
  },
  "evaluation_policy": {
    "primary_metric": "approved",
    "success_value": "true"
  }
}
```

Use with `./iterion run --recipe recipe.json <file.iter>`.

---

## Project Structure

```
iterion/
├── cmd/iterion/       # CLI entry point and command router
├── cli/               # Command implementations (run, validate, inspect, resume, diagram)
├── ast/               # Abstract syntax tree node definitions
├── parser/            # Lexer and recursive-descent parser
├── grammar/           # EBNF grammar specification and language scope docs
├── ir/                # Intermediate representation, compiler, validator, Mermaid generator
├── runtime/           # Execution engine, budget tracking, parallel orchestration
├── store/             # File-backed persistence (runs, events, artifacts, interactions)
├── model/             # LLM executor, model registry, event hooks, schema validation
├── tool/              # Tool adapter and allowlist-based access policy
├── recipe/            # Recipe/preset management
├── benchmark/         # Benchmark runner, metrics collection, reporting
├── examples/          # Reference .iter workflow files
├── e2e/               # End-to-end test scenarios
├── docs/              # On-disk format specification
└── plans/             # Development roadmap and phase prompts
```

---

## Development

### Prerequisites

- Go 1.23.8+
- [goai](https://github.com/zendev-sh/goai) — LLM provider abstraction

### Building

```bash
go build -o iterion ./cmd/iterion
```

### Testing

```bash
# Run all tests
go test ./...

# Run tests for a specific package
go test ./parser
go test ./ir
go test ./runtime
go test ./store
go test ./cli
go test ./model
go test ./e2e

# Verbose output
go test -v ./...
```

The test suite includes unit tests across all packages plus end-to-end scenarios in `e2e/`. See [`e2e/SCENARIOS.md`](e2e/SCENARIOS.md) for the full test coverage matrix.

---

## Examples

The [`examples/`](examples/) directory contains reference workflows of increasing complexity:

| File | Description | Primitives |
|------|-------------|------------|
| [`pr_refine_single_model.iter`](examples/pr_refine_single_model.iter) | PR refinement with a single model in a review→plan→act→verify loop | agent, judge, human, done, fail, bounded loops, publish, session modes, tools |
| [`pr_refine_dual_model_parallel.iter`](examples/pr_refine_dual_model_parallel.iter) | Dual-model parallel PR review with router/join | All above + router (fan_out_all), join (wait_all), parallel branches |
| [`pr_refine_dual_model_parallel_compliance.iter`](examples/pr_refine_dual_model_parallel_compliance.iter) | Adds a compliance gate and human approval to the parallel workflow | All above + human node, conditional routing |
| [`ci_fix_until_green.iter`](examples/ci_fix_until_green.iter) | Iterative CI fix loop: run tests → diagnose → fix → rerun | Tool nodes, outer loops, tool_max_steps |
| [`recipe_benchmark.iter`](examples/recipe_benchmark.iter) | Benchmark harness for comparing model/prompt configurations | Recipes, evaluation policies, preset vars |

See [`examples/FIXTURES.md`](examples/FIXTURES.md) for detailed documentation on each fixture.

---

## License

Copyright (c) Iterion AI. All rights reserved.
