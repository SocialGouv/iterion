# 🔄 Iterion

**Declarative workflow orchestration for AI agents.**

Define complex, multi-agent LLM workflows as readable `.iter` files — chain agents, judges, routers, human gates, parallel branches, bounded loops, and budget caps into a single, auditable execution graph.

> ⚠️ **This project is highly experimental.** APIs, DSL syntax, and storage formats may change without notice. Use at your own risk in production environments. Feedback and contributions are welcome!

---

## Table of Contents

- [What is Iterion?](#what-is-iterion)
- [Features](#features)
- [Quick Start](#quick-start)
- [A Taste of the DSL](#a-taste-of-the-dsl)
- [Documentation](#documentation)
- [License](#license)

---

## 🧩 What is Iterion?

*If you've ever noticed yourself repeating the same prompt-and-review patterns while vibe-coding with an LLM — "ask the model, eyeball the diff, ask it to fix what it missed, run the tests, ask again" — and wondered how to **automate and optimize** that loop, Iterion is built for you.* Capture the pattern once as an `.iter` workflow, give it budget caps, parallel reviewers, judges and human gates, and let the engine run it deterministically every time.

Iterion is a workflow engine that turns `.iter` files into executable AI pipelines. You describe *what* your agents should do — review code, plan fixes, check compliance, ask a human — and Iterion handles *how*: scheduling branches in parallel, enforcing budgets, persisting state, and routing between nodes.

```
.iter file → Parse → Compile → Validate → Execute
                                            │
                    ┌───────────────────────┐│
                    │  agents, judges,      ││
                    │  routers, joins,      ││
                    │  humans, tools        ││
                    │  running in parallel  ││
                    │  with budget tracking ││
                    └───────────────────────┘│
                                            ▼
                              results, artifacts, event log
```

Think of it as a DAG runner purpose-built for LLM workflows — with first-class support for things like structured I/O, conversation sessions, human-in-the-loop pauses, and cost control.

---

## 📋 Features

### Authoring & orchestration

- 📝 **Declarative DSL** — Human-readable `.iter` files with indentation-based syntax
- 🤖 **Multi-agent orchestration** — Chain agents, judges, routers, and joins into complex graphs
- 🖥️ **Visual editor** — Browser-based workflow builder with drag-and-drop, live validation, and source view
- 🙋 **Human-in-the-loop** — Pause for human input, auto-answer via LLM, or let the LLM decide when to ask
- 🔀 **Parallel branching** — Fan-out via routers, converge with join nodes (`wait_all` / `best_effort`)
- 🧭 **4 routing modes** — `fan_out_all`, `condition`, `round_robin`, and `llm`-driven routing
- 🔁 **Bounded loops** — Retry and refinement cycles with configurable iteration limits
- 🔲 **Structured I/O** — Typed schemas for inputs and outputs with enum constraints
- 🔗 **MCP support** — Declare MCP servers directly in `.iter` files (`stdio`, `http`)
- 🧪 **Recipe system** — Bundle workflows with presets for comparison and benchmarking
- 📐 **Mermaid diagrams** — Auto-generate visual workflow diagrams (compact / detailed / full)

### Execution & runtime

- 🔌 **Delegation** — Offload execution to external agents (Claude Code, Codex) with full tool access — works with Claude and ChatGPT/Codex subscriptions
- 🌐 **Provider-agnostic** — In-process `claw` backend supports Anthropic and OpenAI (validated), plus Bedrock, Vertex, Foundry (via the vendored `claw-code-go` SDK)
- 💰 **Budget enforcement** — Shared, mutex-protected caps on tokens, cost (USD), duration, parallel branches, and loop iterations
- 🛡️ **Tool policies** — Allowlist-based access control with exact, namespace, and wildcard matching
- 🌳 **Worktree auto-finalization** — `worktree: auto` runs the workflow in a fresh git worktree, persists commits to a named branch, and fast-forwards the current branch on success — see [docs/resume.md](docs/resume.md)
- 🛡️ **Per-run sandbox** — `sandbox: auto` isolates each run inside a Docker/Podman container with the worktree bind-mounted at `/workspace` and an HTTP CONNECT proxy enforcing a network allowlist — see [docs/sandbox.md](docs/sandbox.md)
- 🔐 **Privacy filter** — Built-in Go-native `privacy_filter` / `privacy_unfilter` tools redact and restore PII (emails, phones, IBANs, credit cards, URLs, ~25 secret patterns) — see [docs/privacy_filter.md](docs/privacy_filter.md)

### Persistence & observability

- 📦 **Artifact versioning** — Per-node, per-iteration versioned outputs persisted to disk
- 📊 **Event sourcing** — Append-only JSONL event log for full replay and debugging
- ⏯️ **Resumable runs** — Checkpoint-based resume from `failed_resumable` / `paused_waiting_human` / `cancelled` states — see [docs/resume.md](docs/resume.md)
- 📈 **Observability stack** — Prometheus `/metrics`, OTLP traces, and a self-contained docker-compose stack with pre-built Grafana dashboards — see [docs/observability/README.md](docs/observability/README.md)

### Distribution & integration

- ☁️ **Cloud mode** — Multi-tenant Helm deployment with MongoDB + S3-compatible blob store + NATS JetStream queue; KEDA-scaled runner pool; per-run Kubernetes sandbox pods
- 🧰 **TypeScript SDK** — [`@iterion/sdk`](sdks/typescript/) wraps the CLI with typed `run` / `resume` / `events` streaming for Node, Deno, and Bun apps
- 🧠 **AI agent skill** — Install as a skill in Claude Code, Codex, Cursor, Windsurf, GitHub Copilot, Cline, Aider, and other AI coding agents

---

## 🚀 Quick Start

### Pick your install

Same engine, six delivery modes — pick the one that fits your workflow:

| Mode | Best for | Install | Docs |
|---|---|---|---|
| 🖥️ **CLI** | Scripted runs, CI/CD pipelines | `curl -fsSL https://socialgouv.github.io/iterion/install.sh \| sh` | [install.md](docs/install.md) |
| 🌐 **Web editor** | Visual workflow design (browser-based) | Bundled with the CLI: `iterion editor` | [visual-editor.md](docs/visual-editor.md) |
| 🪟 **Desktop app** | Native window, multi-project, OS keychain, auto-update | Download `iterion-desktop` from [Releases](https://github.com/SocialGouv/iterion/releases/latest) | [desktop.md](docs/desktop.md) |
| 🐳 **Docker** | Zero-install runs, reproducible CI | `docker run --rm ghcr.io/socialgouv/iterion:latest` | [install.md#docker](docs/install.md#docker) |
| ☁️ **Cloud / server** | Multi-tenant deployment, shared run store, REST/WS API | `helm install iterion oci://ghcr.io/socialgouv/charts/iterion` | [cloud.md](docs/cloud.md) |
| 📦 **TypeScript SDK** | Programmatic invocation from Node/Deno/Bun | `npm install @iterion/sdk` | [sdks/typescript/](sdks/typescript/) |

All six invoke the same Go core. The DSL, runtime, persistence and observability are identical — they only differ in how you reach them.

### Your first workflow

```bash
# Scaffold a new project
mkdir my-project && cd my-project
iterion init

# Configure your API key
cp .env.example .env
# Edit .env → set ANTHROPIC_API_KEY (or OPENAI_API_KEY)
source .env && export ANTHROPIC_API_KEY

# Validate the workflow
iterion validate pr_refine_single_model.iter

# Run it
iterion run pr_refine_single_model.iter \
  --var pr_title="Fix auth middleware" \
  --var review_rules="No SQL injection, no hardcoded secrets"
```

`iterion init` creates a complete PR refinement workflow (review → plan → compliance check → act → verify) that you can run immediately.

### Inspect results

```bash
iterion inspect                          # List all runs
iterion inspect --run-id <id> --events   # View a specific run with events
iterion report --run-id <id>             # Generate a detailed report
```

All run data (events, artifacts, interactions) is stored in `.iterion/runs/`.

---

## ✨ A Taste of the DSL

Here's the simplest possible workflow — an agent reviews code and decides pass/fail:

```iter
prompt review_system:
  You are a code reviewer. Evaluate the submission
  and decide if it meets quality standards.

prompt review_user:
  Review this code:
  {{input.code}}

schema review_input:
  code: string

schema review_output:
  approved: bool
  summary: string

agent reviewer:
  model: "${MODEL}"
  input: review_input
  output: review_output
  system: review_system
  user: review_user

workflow minimal:
  entry: reviewer
  reviewer -> done when approved
  reviewer -> fail when not approved
```

That's it — 28 lines. The agent gets a code input, produces a structured `{approved, summary}` output, and the workflow routes to `done` or `fail` based on the verdict.

From here you can add judges for multi-pass review, routers for parallel fan-out, human gates for approval, bounded loops for retry, budget caps for cost control, and more — see [docs/dsl.md](docs/dsl.md) for the full reference.

---

## 📚 Documentation

The full documentation lives under [`docs/`](docs/) — start with the [documentation index](docs/README.md). Highlights:

**Get going**
- [docs/install.md](docs/install.md) — every install method (CLI, desktop, Docker, Helm, SDK)
- [docs/visual-editor.md](docs/visual-editor.md) — browser-based workflow editor
- [docs/desktop.md](docs/desktop.md) — native desktop app
- [docs/examples.md](docs/examples.md) — workflows of increasing complexity (starter → advanced)
- [docs/skill.md](docs/skill.md) — install Iterion as an AI agent skill (Claude Code, Cursor, Copilot…)

**Author workflows**
- [docs/dsl.md](docs/dsl.md) — full `.iter` DSL reference
- [docs/routers.md](docs/routers.md) — routing modes deep dive
- [docs/recipes.md](docs/recipes.md) — preset-driven runs (benchmarking, prompt comparison)
- [docs/delegation.md](docs/delegation.md) — `model:` vs `delegate:` (claude_code, codex)
- [docs/attachments.md](docs/attachments.md) — file/image attachments in prompts
- [docs/privacy_filter.md](docs/privacy_filter.md) — built-in PII redaction tools
- [docs/workflow_authoring_pitfalls.md](docs/workflow_authoring_pitfalls.md) — required reading before authoring workflows that commit code

**Run & operate**
- [docs/cli-reference.md](docs/cli-reference.md) — every `iterion` subcommand and flag
- [docs/resume.md](docs/resume.md) — resume / failure / cancellation matrix
- [docs/sandbox.md](docs/sandbox.md) — per-run container isolation
- [docs/observability/README.md](docs/observability/README.md) — Prometheus, OTLP, Grafana
- [docs/persisted-formats.md](docs/persisted-formats.md) — on-disk format spec
- [docs/cloud.md](docs/cloud.md) + [docs/cloud-deployment.md](docs/cloud-deployment.md) — cloud mode overview + operator runbook

**Architecture & contributing**
- [docs/architecture.md](docs/architecture.md) — compiler pipeline, runtime engine, persistence
- [docs/adr/](docs/adr/) — architecture decision records
- [docs/development.md](docs/development.md) — build, test, project structure for contributors

**References**
- [docs/references/dsl-grammar.md](docs/references/dsl-grammar.md) — readable grammar
- [docs/references/diagnostics.md](docs/references/diagnostics.md) — all C001–C043 codes
- [docs/references/patterns.md](docs/references/patterns.md) — 10 reusable workflow patterns
- [docs/grammar/iterion_v1.ebnf](docs/grammar/iterion_v1.ebnf) — formal EBNF grammar

---

## 📄 License

Apache-2.0. See `LICENSE` for the full text. Copyright © SocialGouv.
