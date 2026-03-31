# Iterion

**Declarative workflow orchestration engine for AI-driven tasks.**

Iterion lets you author complex, multi-agent LLM workflows as readable `.iter` files — combining agents, judges, routers, human-in-the-loop interactions, parallel branching, bounded loops, and budget enforcement into a single, auditable execution graph.

## Table of Contents

- [Features](#features)
- [Quickstart](#quickstart)
  - [Install](#install)
  - [Your first workflow](#your-first-workflow)
  - [Going further](#going-further)
- [The `.iter` DSL](#the-iter-dsl)
  - [Top-Level Declarations](#top-level-declarations)
  - [Node Types](#node-types)
  - [Workflows](#workflows)
  - [Edges and Control Flow](#edges-and-control-flow)
  - [Template Expressions](#template-expressions)
- [CLI Reference](#cli-reference)
  - [init](#init)
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
- [Examples](#examples)
- [Development](#development)
  - [Prerequisites](#prerequisites)
  - [Dev Container](#dev-container)
  - [Building](#building)
  - [Testing](#testing)
  - [Project Structure](#project-structure)
- [License](#license)

---

## Features

- **Declarative DSL** — Human-readable `.iter` files with indentation-based syntax (YAML/Python-style)
- **Multi-agent orchestration** — Chain agents, judges, routers, and joins into complex workflows
- **Human-in-the-loop** — Pause for human input, auto-answer via LLM, or let the LLM decide when to ask
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

## Quickstart

### Install

Install the latest binary:

```bash
curl -fsSL https://socialgouv.github.io/iterion/install.sh | sh
```

Or install to a custom directory (no sudo):

```bash
INSTALL_DIR=. curl -fsSL https://socialgouv.github.io/iterion/install.sh | sh
```

**Windows** (PowerShell):

```powershell
Invoke-WebRequest -Uri "https://github.com/socialgouv/iterion/releases/latest/download/iterion-windows-amd64.exe" -OutFile iterion.exe
```

You can also download binaries manually from the [latest release page](https://github.com/socialgouv/iterion/releases/latest).

### Your first workflow

The fastest way to get started is `iterion init`:

```bash
mkdir my-project && cd my-project
iterion init
```

This creates:
- `pr_refine_single_model.iter` — a complete PR refinement workflow (review → plan → act → verify loop)
- `.env.example` — API key template
- `.gitignore` — excludes `.iterion/` run data and `.env`

**Step 1 — Configure your API keys:**

```bash
cp .env.example .env
# Edit .env and set your ANTHROPIC_API_KEY (or OPENAI_API_KEY)
source .env && export ANTHROPIC_API_KEY
```

**Step 2 — Validate the workflow:**

```bash
iterion validate pr_refine_single_model.iter
```

```
── Validate: pr_refine_single_model.iter ──
  Workflow:        pr_refine_single_model
  Nodes:           10
  Edges:           11

  result: OK
```

**Step 3 — Run the workflow:**

```bash
iterion run pr_refine_single_model.iter \
  --var pr_title="Fix auth middleware session handling" \
  --var review_rules="No SQL injection, no hardcoded secrets, all errors handled" \
  --var compliance_rules="OWASP top 10, no sensitive data in logs"
```

Here's the flow:

```
context_builder ──▶ reviewer ──▶ planner ──▶ compliance_check
                                                │
                                   approved ◀───┤───▶ not approved
                                      │                    │
                                      ▼               refine_plan ◀─╮
                                  act_on_plan              │        │
                                      │         compliance_check_after_refine
                                      ▼                    │        │
                                 final_verify     approved/rejected─╯
                                   │       │
                              approved   not approved
                                 │         │
                                done    (restart from context_builder, max 3×)
```

The workflow will:

1. **context_builder** — Gathers PR context (diff, changed files, repo structure) using tools like `git_diff`, `read_file`, `search_codebase`
2. **reviewer** — Produces a structured review with issues, blockers, and recommendations
3. **planner** — Turns the review into an ordered remediation plan (inherits the reviewer's session)
4. **compliance_check** — Judge evaluates whether the plan meets compliance rules
   - If **approved** → proceeds to act
   - If **not approved** → enters a refinement loop (up to 4 iterations) where `refine_plan` adjusts the plan and `compliance_check_after_refine` re-evaluates
5. **act_on_plan** — Applies the approved plan to the codebase using tools (`write_file`, `patch`, `run_command`, etc.)
6. **final_verify** — Judge evaluates the corrected PR
   - If **approved** → `done`
   - If **not approved** → restarts the entire flow (up to 3 outer loops)

**Step 4 — Inspect the results:**

```bash
# List all runs
iterion inspect

# View a specific run with its event log
iterion inspect --run-id <run_id> --events

# Show full artifact contents
iterion inspect --run-id <run_id> --full
```

Run artifacts are stored in `.iterion/runs/<run_id>/` — including the event log (`events.jsonl`), node outputs, and any published artifacts.

**Visualize the workflow (optional):**

```bash
iterion diagram pr_refine_single_model.iter --detailed
```

This outputs a Mermaid diagram you can paste into any compatible renderer (GitHub Markdown, [Mermaid Live Editor](https://mermaid.live), etc.).

### Going further

The [`examples/`](examples/) directory contains workflows of increasing complexity:

| File | What it adds |
|------|-------------|
| [`pr_refine_single_model.iter`](examples/pr_refine_single_model.iter) | **Start here** — Single model, review→plan→act→verify loop |
| [`pr_refine_dual_model_parallel.iter`](examples/pr_refine_dual_model_parallel.iter) | Router + Join for parallel dual-model review |
| [`pr_refine_dual_model_parallel_compliance.iter`](examples/pr_refine_dual_model_parallel_compliance.iter) | Human approval gate + compliance routing |
| [`ci_fix_until_green.iter`](examples/ci_fix_until_green.iter) | Tool nodes, outer retry loops, CI integration |
| [`recipe_benchmark.iter`](examples/recipe_benchmark.iter) | Recipe system for model/prompt benchmarking |

See [`examples/FIXTURES.md`](examples/FIXTURES.md) for detailed documentation on each fixture.

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
| `human <name>` | Human interaction node — pauses, auto-answers, or conditionally pauses |
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

**Human** — Human interaction node with three modes:

```
## Always pause for human input (default)
human approval:
  input: approval_request
  output: approval_response
  instructions: approval_prompt
  mode: pause_until_answers
  min_answers: 1

## LLM auto-answers — never pauses
human auto_review:
  input: review_input
  output: review_decision
  mode: auto_answer
  model: "claude-sonnet-4-20250514"
  system: auto_review_prompt

## LLM decides whether to pause or auto-answer
human conditional_review:
  input: review_input
  output: review_decision
  mode: auto_or_pause
  model: "claude-sonnet-4-20250514"
  system: decision_guidance
  instructions: review_questions
```

| Mode | Behavior | Requires |
|------|----------|----------|
| `pause_until_answers` | Always pauses for human input (default) | `instructions` |
| `auto_answer` | LLM generates answers matching the output schema | `model`, `output` |
| `auto_or_pause` | LLM decides: returns `needs_human_input: bool` + answers | `model`, `output` |

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

### init

Initialize a new project with an example workflow and environment configuration:

```bash
iterion init              # Initialize current directory
iterion init my-project   # Initialize a new directory
iterion init --json       # JSON output
```

Creates `pr_refine_single_model.iter`, `.env.example`, and `.gitignore`. Idempotent — will not overwrite existing files.

### validate

Parse, compile, and validate a workflow file without executing it:

```bash
iterion validate <file.iter>
iterion validate examples/pr_refine_single_model.iter --json
```

Reports errors and warnings with diagnostic codes, file positions, and descriptions.

### run

Execute a workflow:

```bash
iterion run <file.iter> [flags]
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
iterion inspect [flags]
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
iterion resume --run-id <id> --file <file.iter> [flags]
```

| Flag | Description |
|------|-------------|
| `--answer key=value` | Provide an answer (repeatable) |
| `--answers-file <file>` | Load answers from a JSON file |
| `--store-dir <dir>` | Run store directory |

### diagram

Generate a Mermaid diagram from a workflow file:

```bash
iterion diagram <file.iter> [--detailed]
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
│  │pause/│   │ exec  │   │terminal│   │error │  │
│  │ auto │   │       │   │        │   │      │  │
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

Use with `iterion run --recipe recipe.json <file.iter>`.

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

## Development

This section is for contributors working on the iterion codebase itself.

### Prerequisites

- [Devbox](https://www.jetify.com/devbox) — portable dev environment (installs Go, Task, Node)
- [direnv](https://direnv.net/) — auto-activates the Devbox shell when you `cd` into the repo
- [goai](https://github.com/zendev-sh/goai) — LLM provider abstraction (local dependency at `~/goai`)

**Setup:**

```bash
# Hook direnv into your shell (~/.bashrc, ~/.zshrc, etc.)
eval "$(direnv hook bash)"   # or: eval "$(direnv hook zsh)"

# Allow the .envrc in this repo
direnv allow
```

After `direnv allow`, the Devbox environment (Go 1.23, Task, Node) activates automatically whenever you enter the project directory.

### Dev Container

The repository includes a `.devcontainer/` configuration for VS Code / GitHub Codespaces:

```jsonc
// .devcontainer/devcontainer.json
{
  "image": "jetpackio/devbox:latest",
  "features": { "ghcr.io/devcontainers/features/node:1": { "version": "lts" } }
}
```

### Building

```bash
task build          # → ./iterion

# or directly:
go build -o iterion ./cmd/iterion
```

### Testing

```bash
task test           # Unit tests
task test:e2e       # End-to-end tests
task test:race      # Tests with race detector
task lint           # go fmt + go vet
task check          # lint + test
task clean          # Remove build artifacts

# or directly:
go test ./...
go test -v ./parser ./ir ./runtime ./store ./cli ./model ./e2e
```

The test suite includes unit tests across all packages plus end-to-end scenarios in `e2e/`. See [`e2e/SCENARIOS.md`](e2e/SCENARIOS.md) for the full test coverage matrix.

### Project Structure

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

## License

Copyright (c) Iterion AI. All rights reserved.
