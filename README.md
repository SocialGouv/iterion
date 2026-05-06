# 🔄 Iterion

**Declarative workflow orchestration for AI agents.**

Define complex, multi-agent LLM workflows as readable `.iter` files — chain agents, judges, routers, human gates, parallel branches, bounded loops, and budget caps into a single, auditable execution graph.

> ⚠️ **This project is highly experimental.** APIs, DSL syntax, and storage formats may change without notice. Use at your own risk in production environments. Feedback and contributions are welcome!

---

## Table of Contents

- [What is Iterion?](#what-is-iterion)
- [Four ways to use Iterion](#four-ways-to-use-iterion)
- [Quickstart](#quickstart)
- [A Taste of the DSL](#a-taste-of-the-dsl)
- [Features](#features)
- [Visual Editor (web)](#visual-editor-web)
- [Desktop App](#desktop-app)
- [Cloud Mode](#cloud-mode)
- [The `.iter` DSL](#the-iter-dsl)
  - [Variables](#variables)
  - [Prompts](#prompts)
  - [Schemas](#schemas)
  - [Node Types](#node-types)
  - [Workflows, Edges & Control Flow](#workflows-edges--control-flow)
  - [Template Expressions](#template-expressions)
  - [MCP Servers](#mcp-servers)
  - [Budget](#budget)
- [CLI Reference](#cli-reference)
- [Delegation](#delegation)
- [AI Agent Skill](#ai-agent-skill)
- [Recipes](#recipes)
- [Examples](#examples)
- [Architecture](#architecture)
- [Development](#development)
- [License](#license)

---

## 🧩 What is Iterion?

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

## 🎛️ Four ways to use Iterion

Same engine, four delivery modes — pick the one that fits your workflow:

| Mode | Best for | How to start |
|---|---|---|
| 🖥️ **CLI** | Scripted runs, CI/CD pipelines, quick iteration | `iterion run workflow.iter` |
| 🌐 **Web editor** | Visual workflow design on your dev machine | `iterion editor` (opens browser) |
| 🪟 **Desktop app** | Native window with multi-project, OS keychain, auto-update | Download `iterion-desktop` from Releases |
| ☁️ **Cloud / server** | Multi-tenant deployment, shared run store, REST/WS API | Helm chart in `helm/iterion/` |

All four invoke the same Go core. The DSL, runtime, persistence and observability are identical — they only differ in how you reach them. Pick CLI for automation, the visual editor for daily editing, the desktop app if you want a one-click install with managed credentials, or cloud mode when teams need a shared, always-on instance.

---

## 🚀 Quickstart

### Install the CLI

```bash
curl -fsSL https://socialgouv.github.io/iterion/install.sh | sh
```

Or install to a custom directory (no sudo needed):

```bash
INSTALL_DIR=. curl -fsSL https://socialgouv.github.io/iterion/install.sh | sh
```

<details>
<summary>Windows (PowerShell)</summary>

```powershell
Invoke-WebRequest -Uri "https://github.com/socialgouv/iterion/releases/latest/download/iterion-windows-amd64.exe" -OutFile iterion.exe
```

</details>

You can also download binaries from the [latest release](https://github.com/socialgouv/iterion/releases/latest). Builds are available for Linux, macOS (Intel + Apple Silicon), and Windows.

> **Want a native window instead of a CLI + browser?** Skip to [Desktop App](#desktop-app) — it ships an installable `.AppImage` / `.app` / `.exe` with the editor pre-wired, OS-keychain credentials and auto-update.

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
# List all runs
iterion inspect

# View a specific run with events
iterion inspect --run-id <id> --events

# Generate a detailed report
iterion report --run-id <id>
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

From here you can add judges for multi-pass review, routers for parallel fan-out, human gates for approval, bounded loops for retry, budget caps for cost control, and more.

---

## 📋 Features

- 📝 **Declarative DSL** — Human-readable `.iter` files with indentation-based syntax
- 🤖 **Multi-agent orchestration** — Chain agents, judges, routers, and joins into complex graphs
- 🖥️ **Visual editor** — Browser-based workflow builder with drag-and-drop, live validation, and source view
- 🙋 **Human-in-the-loop** — Pause for human input, auto-answer via LLM, or let the LLM decide when to ask
- 🔀 **Parallel branching** — Fan-out via routers, converge with join nodes (`wait_all` / `best_effort`)
- 🧭 **4 routing modes** — `fan_out_all`, `condition`, `round_robin`, and `llm`-driven routing
- 🔁 **Bounded loops** — Retry and refinement cycles with configurable iteration limits
- 💰 **Budget enforcement** — Caps on tokens, cost (USD), duration, and iterations
- 🔌 **Delegation** — Offload execution to external agents (Claude Code, Codex) with full tool access — works with Claude and ChatGPT/Codex subscriptions
- 🔲 **Structured I/O** — Typed schemas for inputs and outputs with enum constraints
- 🔗 **MCP support** — Declare MCP servers directly in `.iter` files (`stdio`, `http`)
- 📦 **Artifact versioning** — Per-node, per-iteration versioned outputs persisted to disk
- 📊 **Event sourcing** — Append-only JSONL event log for full observability and replay
- ⏸️ **Pause/resume** — Checkpoint-based suspension and resumption of runs
- 📐 **Mermaid diagrams** — Auto-generate visual workflow diagrams
- 🧪 **Recipe system** — Bundle workflows with presets for comparison and benchmarking
- 🛡️ **Tool policies** — Allowlist-based access control with exact, namespace, and wildcard matching
- 🌐 **Provider-agnostic** — Supports multiple LLM providers (Claude, OpenAI, Bedrock, Vertex, Foundry) via the vendored `claw-code-go` SDK
- 🧠 **AI agent skill** — Install as a skill in Claude Code, Codex, Cursor, and other AI agents

---

## 🌐 Visual Editor (web)

Iterion includes a browser-based visual workflow editor built with React and XYFlow. Served by your local `iterion` binary — no installation beyond the CLI.

```bash
iterion editor                     # Launch on default port (4891), opens browser
iterion editor --port 8080         # Custom port
iterion editor --dir ./workflows   # Custom working directory
iterion editor --no-browser        # Don't auto-open browser
```

**What you get:**

- **Canvas** — Drag-and-drop node graph with auto-layout, zoom, search, and keyboard shortcuts
- **Node library** — Drag pre-built node types (agent, judge, router, join, human, tool) onto the canvas
- **Property editor** — Edit node properties, schemas, prompts, and edge conditions in a side panel
- **Source view** — Split-pane view showing the raw `.iter` source alongside the visual graph
- **Live diagnostics** — Real-time validation errors and warnings as you edit (codes C001–C029)
- **File watching** — Detects external file changes via WebSocket and syncs automatically
- **Undo/redo** — Full edit history
- **Run console** — Launch a workflow from the editor and watch events stream live

This mode is the simplest way to design and iterate locally. If you want a packaged native window instead (no browser, OS-keychain credentials, auto-update), see the [Desktop App](#desktop-app).

---

## 🪟 Desktop App

A native desktop build (Wails v2) wraps the visual editor in its own window with multi-project switching, OS-keychain credential storage, first-run onboarding, and Ed25519-signed auto-update. Two binaries ship side-by-side: `iterion` (CLI) and `iterion-desktop` (this app).

### Install

Pick the artefact that matches your OS from [the latest GitHub Release](https://github.com/SocialGouv/iterion/releases/latest) (filenames start with `iterion-desktop-`). Each tag publishes:

| Platform | File | Size | Notes |
|---|---|---|---|
| Linux x86_64 | `iterion-desktop-linux-amd64.AppImage` | ~110 MB | Self-contained, click-to-run |
| Linux x86_64 | `iterion-desktop-linux-amd64.deb` | ~16 MB | Debian/Ubuntu/Mint package — `apt`-managed install + uninstall, declares deps |
| Linux x86_64 | `iterion-desktop-linux-amd64.tar.gz` | ~16 MB | Raw binary + README; needs `libwebkit2gtk-4.1-0` + `libgtk-3-0` + `libsoup-3.0-0` |
| Linux arm64 | `iterion-desktop-linux-arm64.{AppImage,deb,tar.gz}` | same | same |
| macOS Intel + Apple Silicon | `iterion-desktop-darwin-universal.zip` | ~80 MB | Universal `.app` (lipo'd, runs natively on both archs) |
| Windows x64 | `iterion-desktop-windows-amd64.exe` | ~50 MB | Portable single executable |
| Windows x64 | `iterion-desktop-windows-amd64-installer.exe` | ~50 MB | NSIS installer (per-user, Start Menu integration) |
| Windows arm64 | `iterion-desktop-windows-arm64.{exe,-installer.exe}` | same | same |

#### Linux

**AppImage** (no system deps):
```bash
chmod +x iterion-desktop-linux-amd64.AppImage
./iterion-desktop-linux-amd64.AppImage
```

**Debian/Ubuntu/Mint** (.deb — apt manages deps + uninstall):
```bash
sudo apt install ./iterion-desktop-linux-amd64.deb
iterion-desktop
```

**Raw binary** (smaller, requires WebKit + GTK runtime):
```bash
# Debian/Ubuntu/Mint/Pop!_OS:
sudo apt install libgtk-3-0 libwebkit2gtk-4.1-0 libsoup-3.0-0
# Fedora/RHEL:
sudo dnf install gtk3 webkit2gtk4.1 libsoup3

tar -xzf iterion-desktop-linux-amd64.tar.gz
chmod +x iterion-desktop
./iterion-desktop
```

#### macOS

```bash
unzip iterion-desktop-darwin-universal.zip
xattr -d com.apple.quarantine Iterion.app   # one-off Gatekeeper unblock (V1 builds are unsigned)
open Iterion.app
```

You can also drag `Iterion.app` to `/Applications/` for a permanent install.

#### Windows

- **Portable** : double-click `iterion-desktop-windows-amd64.exe`. SmartScreen will warn ("Unknown publisher" — V1 is unsigned) → "More info" → "Run anyway".
- **Installer** : run `iterion-desktop-windows-amd64-installer.exe` for a per-user install with Start Menu shortcut.

### First launch

The desktop app boots into a Welcome wizard that asks you to:
1. Pick or create a project folder (a directory containing `.iter` files).
2. Configure API keys (stored in the OS keychain — Keychain on macOS, Credential Manager on Windows, Secret Service on Linux). Skippable; you can also rely on environment variables in your shell.
3. Verify external CLIs (`claude`, `codex`) detection — only needed if you use `delegate:` in your workflows.

After onboarding the editor opens on your project. Multi-project switcher is in the top-left.

### Auto-update

The desktop app polls GitHub for new releases every 4 hours (configurable in Settings → Updater) and offers in-app update on detection. Manifests and artefacts are Ed25519-signed.

### Build locally

For contributors / power users :

- [docs/desktop-build.md](docs/desktop-build.md) — local build flow + Docker reproducible builder + per-OS deps + cross-compile matrix
- [docs/desktop-architecture.md](docs/desktop-architecture.md) — proxy-based AssetServer architecture
- [docs/desktop-distribution.md](docs/desktop-distribution.md) — release signing + Ed25519 keypair setup
- [docs/desktop-qa.md](docs/desktop-qa.md) — QA checklist for releases

---

## ☁️ Cloud Mode

A long-running server deployment that targets multi-tenant teams. Same Go core as the CLI, but exposes the editor + run engine through HTTP/WS to a shared instance, persists runs to a Mongo + S3-compatible blob store, and (optionally) dispatches jobs to a runner pool via NATS.

```yaml
# helm/iterion/values.yaml — minimal example
mongo:
  uri: "mongodb://mongo:27017/iterion"
blob:
  endpoint: "https://s3.example.com"
  bucket: "iterion-runs"
queue:
  nats: "nats://nats:4222"
```

- Deploy with `helm upgrade --install iterion helm/iterion -f values.yaml`.
- Local cloud-mode test stack: `task cloud:up` brings up Mongo + NATS + MinIO + iterion via docker-compose.
- Container image: `ghcr.io/socialgouv/iterion:latest` (built by `.github/workflows/image.yml` on every main push and tag).
- Health probes: `GET /healthz` (always 200, liveness) and `GET /readyz` (200 when local-mode dependencies pass).

This mode is still actively evolving. The CLI/web/desktop modes write run state to the local filesystem (`.iterion/`); cloud mode threads the same store interface to MongoDB + S3 + NATS so multiple workers can share state.

---

## 📝 The `.iter` DSL

Workflows are written in a declarative, indentation-significant language. The formal grammar is in [`docs/grammar/iterion_v1.ebnf`](docs/grammar/iterion_v1.ebnf).

### Variables

Define typed variables at the top level. These can be set at runtime with `--var key=value`:

```iter
vars:
  pr_title: string
  max_retries: int = 3
  verbose: bool = false
  config: json = { "key": "value" }
  tags: string[] = ["security", "performance"]
```

Supported types: `string`, `bool`, `int`, `float`, `json`, `string[]`.

### Prompts

Reusable prompt templates with `{{...}}` interpolation:

```iter
prompt review_system:
  You are a code reviewer specializing in {{vars.language}}.
  Focus on: {{vars.review_rules}}

prompt review_user:
  Review this code:
  {{input.code}}
  Previous feedback: {{outputs.prior_review.summary}}
```

### Schemas

Typed data contracts for structured agent I/O:

```iter
schema review_result:
  approved: bool
  summary: string
  issues: string[]
  confidence: string [enum: "low", "medium", "high"]
  score: float
  metadata: json
```

Supported field types: `string`, `bool`, `int`, `float`, `json`, `string[]`. Strings support `[enum: ...]` constraints.

### Node Types

#### Agent

The primary execution unit. Calls an LLM with prompts, optionally uses tools, and returns structured output:

```iter
agent reviewer:
  model: "claude-sonnet-4-20250514"
  input: review_request
  output: review_result
  system: review_system
  user: review_user
  session: fresh
  tools: [git_diff, read_file, search_codebase]
  tool_max_steps: 10
  publish: review_artifact
  reasoning_effort: high
  readonly: true
```

| Property | Description |
|----------|-------------|
| `model` | LLM model identifier (supports `${ENV_VAR}`) |
| `delegate` | Offload to external agent: `claude_code` (recommended) or `codex` (discouraged, see [Delegation](#delegation)) |
| `input` / `output` | Schema references for structured I/O |
| `publish` | Persist output as a named artifact |
| `system` / `user` | Prompt references |
| `session` | Context mode: `fresh` (default), `inherit`, `fork`, or `artifacts_only` |
| `tools` | List of allowed tool names |
| `tool_max_steps` | Max tool-use iterations (0 = unlimited) |
| `reasoning_effort` | Extended thinking: `low`, `medium`, `high`, `xhigh`, `max` |
| `readonly` | If `true`, prevents tool side effects (workspace safety) |

#### Judge

Structurally identical to agents, but semantically intended for evaluation — typically no tools:

```iter
judge compliance_check:
  model: "claude-sonnet-4-20250514"
  input: plan_schema
  output: verdict_schema
  system: compliance_system
  user: compliance_user
```

#### Router

Branches execution into parallel or conditional paths. Four modes are available:

```iter
router dispatch:
  mode: fan_out_all      # Send to ALL outgoing edges in parallel

router branch:
  mode: condition        # Route based on `when` clauses on edges

router alternate:
  mode: round_robin      # Cycle through targets one per iteration

router smart:
  mode: llm              # LLM decides which target(s) to route to
  model: "claude-sonnet-4-20250514"
  system: routing_prompt
  multi: true            # Allow selecting multiple targets
```

> For a deep dive on routing modes, edge rules, and convergence patterns, see [`docs/routers.md`](docs/routers.md).

#### Join

Converges parallel branches back into a single path:

```iter
join merge:
  strategy: wait_all     # Wait for all branches (or: best_effort)
  require: [branch_a, branch_b]
  output: merged_result
```

- `wait_all` — waits for every incoming branch
- `best_effort` — proceeds when required branches finish, tolerates failures on others

#### Human

Pauses the workflow for human input, or lets an LLM handle it:

```iter
## Always pause for human answers
human approval:
  input: approval_request
  output: approval_response
  instructions: approval_prompt
  mode: pause_until_answers
  min_answers: 1

## LLM auto-answers (never pauses)
human auto_review:
  mode: auto_answer
  model: "claude-sonnet-4-20250514"
  system: auto_review_prompt
  output: review_decision

## LLM decides whether to pause or auto-answer
human conditional:
  mode: auto_or_pause
  model: "claude-sonnet-4-20250514"
  system: decision_guidance
  instructions: review_questions
  output: review_decision
```

Resume a paused run with `iterion resume --run-id <id> --file <file> --answer key=value`.

#### Tool

Direct shell command execution — no LLM involved:

```iter
tool run_tests:
  command: "make test"
  output: test_result
```

Supports `${ENV_VAR}` in the command string.

#### Terminal Nodes

Every workflow must end at `done` (success) or `fail` (failure). These are built-in — you don't declare them.

### Workflows, Edges & Control Flow

A workflow ties nodes together with an entry point, optional budget, and edges:

```iter
workflow pr_review:
  entry: context_builder

  budget:
    max_duration: "30m"
    max_cost_usd: 10
    max_tokens: 400000

  context_builder -> reviewer
  reviewer -> done when approved
  reviewer -> context_builder when not approved as retry(3)
```

**Edge syntax:**

```iter
src -> dst                              # Unconditional
src -> dst when approved                # Conditional (bool field from src output)
src -> dst when not approved            # Negated condition
src -> dst as loop_name(5)              # Bounded loop (max 5 iterations)
src -> dst with {                       # Data mapping
  context: "{{outputs.src}}",
  config: "{{vars.my_var}}"
}
```

**Edge rules:**

- Non-router nodes can have at most one unconditional edge
- Conditional edges must be exhaustive (`when X` + `when not X`) or have an unconditional fallback
- All cycles must be declared with `as name(N)` — undeclared cycles are a compile error
- Inside loops, you can access iteration history with `{{outputs.node_id.history}}`

### Template Expressions

Templates use `{{...}}` interpolation:

| Reference | Description |
|-----------|-------------|
| `{{vars.name}}` | Workflow variable |
| `{{input.field}}` | Current node's input field |
| `{{outputs.node_id}}` | Full output of a previously executed node |
| `{{outputs.node_id.field}}` | Specific field from a node's output |
| `{{outputs.node_id.history}}` | Array of all outputs across loop iterations |
| `{{artifacts.name}}` | Published artifact by name |

Environment variables are supported with `${ENV_VAR}` syntax (resolved at compile time).

### MCP Servers

You can declare MCP (Model Context Protocol) servers directly in your `.iter` files:

```iter
mcp_server code_tools:
  transport: stdio
  command: "npx"
  args: ["-y", "@anthropic-ai/claude-code-mcp"]

mcp_server api_server:
  transport: http
  url: "http://localhost:3000/mcp"
```

Agents can then reference these servers:

```iter
agent worker:
  model: "claude-sonnet-4-20250514"
  mcp:
    servers: [code_tools, api_server]
```

Supported transports: `stdio` (requires `command`), `http` (requires `url`).

### Budget

Control costs and prevent runaway execution:

```iter
budget:
  max_parallel_branches: 4    # Concurrent branch limit
  max_duration: "30m"         # Global timeout
  max_cost_usd: 10.0          # Cost cap in USD
  max_tokens: 500000          # Total token budget
  max_iterations: 50          # Loop iteration limit
```

The budget is shared across all branches. When a limit is hit, the engine emits a `budget_exceeded` event and stops the run.

---

## ⌨️ CLI Reference

All commands support `--json` for machine-readable output and `--help` for usage details.

### `iterion init`

Scaffold a new project with an example workflow:

```bash
iterion init              # Current directory
iterion init my-project   # New directory
```

Creates `pr_refine_single_model.iter`, `.env.example`, and `.gitignore`. Idempotent — won't overwrite existing files.

### `iterion validate`

Parse, compile, and validate a workflow without running it:

```bash
iterion validate workflow.iter
```

Reports errors and warnings with diagnostic codes (C001–C029), file positions, and descriptions.

### `iterion run`

Execute a workflow:

```bash
iterion run workflow.iter [flags]
```

| Flag | Description |
|------|-------------|
| `--var key=value` | Set workflow variable (repeatable) |
| `--recipe <file>` | Apply a recipe preset (JSON) |
| `--run-id <id>` | Use a specific run ID (default: auto-generated) |
| `--store-dir <dir>` | Run store directory (default: `.iterion`) |
| `--timeout <duration>` | Global timeout (e.g. `30m`, `1h`) |
| `--log-level <level>` | Log verbosity: `error`, `warn`, `info`, `debug`, `trace` |
| `--no-interactive` | Don't prompt on TTY; exit on human pause |

### `iterion inspect`

View run state and history:

```bash
iterion inspect                          # List all runs
iterion inspect --run-id <id>            # View a specific run
iterion inspect --run-id <id> --events   # Include event log
iterion inspect --run-id <id> --full     # Show full artifact contents
```

### `iterion resume`

Resume a paused workflow run with human answers:

```bash
iterion resume --run-id <id> --file workflow.iter --answer key=value
iterion resume --run-id <id> --file workflow.iter --answers-file answers.json
```

### `iterion diagram`

Generate a Mermaid diagram from a workflow:

```bash
iterion diagram workflow.iter              # Compact view (default)
iterion diagram workflow.iter --detailed   # Include node properties
iterion diagram workflow.iter --full       # Include templates and loop details
```

Paste the output into any Mermaid-compatible renderer (GitHub Markdown, [Mermaid Live Editor](https://mermaid.live), etc.).

### `iterion report`

Generate a chronological report for a completed run:

```bash
iterion report --run-id <id>
iterion report --run-id <id> --output report.md
```

The report includes:
- **Summary table** — workflow name, status, duration, tokens, cost, model calls
- **Artifacts table** — all published artifacts with versions
- **Timeline** — chronological reconstruction of every node execution, edge selection, verdict, branch lifecycle, and budget warning

### `iterion editor`

Launch the visual workflow editor:

```bash
iterion editor                     # Default port 4891
iterion editor --port 8080         # Custom port
iterion editor --dir ./workflows   # Custom directory
iterion editor --no-browser        # Don't auto-open browser
```

### `iterion version`

Print version and commit hash.

---

## 🔌 Delegation

For tasks that need full tool access (file editing, shell commands, git operations), you can delegate agent execution to an external CLI agent instead of making direct LLM API calls:

```iter
agent implementer:
  delegate: "claude_code"          # recommended (codex is supported but discouraged)
  input: plan_schema
  output: result_schema
  system: implementation_prompt
  tools: [read_file, write_file, run_command, git_diff]
```

| Backend | Status | What it does |
|---------|--------|-------------|
| `claude_code` | recommended | Runs the `claude` CLI as a subprocess with full tool access |
| `claw` (default) | recommended for read-only / judges | In-process multi-provider LLM client (Anthropic, OpenAI, …) — use with `model: "openai/gpt-5.4-mini"` etc. |
| `codex` | **discouraged** | Runs the `codex` CLI as a subprocess. Cannot configure its tool set, tends to fill its own context window, and has weaker iterion integration. The compiler emits a `C030` warning per node. Kept for compatibility — prefer `claude_code` or `claw`+OpenAI in new workflows. |

> 💡 `claude_code` works with your Claude subscription (Pro/Max/Team/Enterprise) — no separate API key required. `claw` calls provider APIs directly and needs the corresponding API key (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, …).

Delegation is useful for agents that need to *act* on the codebase (write files, run tests, execute commands). For agents that only need to *think* (review, judge, plan), use `model:` directly — it's lighter weight and faster.

You can mix both in the same workflow. A common pattern is using `model:` for reviewers and judges, and `delegate:` for implementers:

```iter
agent reviewer:
  model: "claude-sonnet-4-20250514"    # Direct API call — fast, read-only
  readonly: true

agent implementer:
  delegate: "claude_code"              # Full agent — can edit files
  tools: [read_file, write_file, patch, run_command]
```

---

## 🧠 AI Agent Skill

Iterion ships as an **Agent Skill** compatible with Claude Code, Codex, Cursor, Windsurf, GitHub Copilot, Cline, Aider, and other AI coding agents. Once installed, your agent knows the full `.iter` DSL and can write correct workflows for you.

### Install the skill

```bash
npx skills add https://github.com/SocialGouv/iterion --skill iterion-dsl
```

### What the skill provides

| File | Content |
|------|---------|
| [`SKILL.md`](SKILL.md) | Complete DSL reference — node types, properties, edge syntax, templates, budget, MCP |
| [`SKILL-run-and-refine.md`](SKILL-run-and-refine.md) | Practice guide for running, debugging and iteratively refining `.iter` workflows against real data |
| [`docs/references/dsl-grammar.md`](docs/references/dsl-grammar.md) | Formal grammar specification (EBNF) |
| [`docs/references/patterns.md`](docs/references/patterns.md) | 10 reusable workflow patterns with annotated snippets |
| [`docs/references/diagnostics.md`](docs/references/diagnostics.md) | All validation diagnostic codes (C001–C030) with causes and fixes |
| [`examples/skill/`](examples/skill/) | 4 minimal, self-contained `.iter` examples |

### Usage

Once installed, just ask your agent to write workflows:

- *"Write an .iter workflow that reviews a PR with two parallel reviewers"*
- *"Create an iterion pipeline that fixes CI failures in a loop"*
- *"Add a human approval gate before the deployment step"*

The agent will use the skill reference to produce valid `.iter` files that pass `iterion validate`.

---

## 🧪 Recipes

Recipes let you run the same workflow with different configurations without editing the `.iter` file. They're useful for benchmarking models, comparing prompts, or creating reusable presets:

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

```bash
iterion run workflow.iter --recipe fast_review.json
```

Recipes can override variables, prompts, budgets, and define success criteria for automated evaluation.

---

## 📚 Examples

The [`examples/`](examples/) directory contains workflows of increasing complexity. Start simple and work your way up:

### 🟢 Starter

| File | Description |
|------|-------------|
| [`skill/minimal_linear.iter`](examples/skill/minimal_linear.iter) | 28 lines — single agent with conditional pass/fail |
| [`skill/human_gate.iter`](examples/skill/human_gate.iter) | Human approval gate pattern |
| [`skill/loop_with_judge.iter`](examples/skill/loop_with_judge.iter) | Simple bounded loop with judge evaluation |
| [`skill/parallel_fan_out_join.iter`](examples/skill/parallel_fan_out_join.iter) | Basic fan-out/join parallelism |

### 🟡 Intermediate

| File | Description |
|------|-------------|
| [`pr_refine_single_model.iter`](examples/pr_refine_single_model.iter) | PR refinement: review → plan → compliance → act → verify loop |
| [`ci_fix_until_green.iter`](examples/ci_fix_until_green.iter) | Automated CI fix loop: diagnose → plan → fix → rerun tests |
| [`session_review_fix.iter`](examples/session_review_fix.iter) | Session continuity with `inherit` and `fork` modes |
| [`llm_router_task_dispatch.iter`](examples/llm_router_task_dispatch.iter) | LLM-driven routing decisions |

### 🔴 Advanced

| File | Description |
|------|-------------|
| [`pr_review.iter`](examples/pr_review.iter) | Parallel dual-reviewer PR analysis with judge synthesis |
| [`pr_refine_dual_model_parallel.iter`](examples/pr_refine_dual_model_parallel.iter) | Dual-model parallel review with router/join |
| [`dual_model_plan_implement_review.iter`](examples/dual_model_plan_implement_review.iter) | Enterprise dual-LLM orchestration with round-robin routing and delegation |
| [`recipe_benchmark.iter`](examples/recipe_benchmark.iter) | Model/prompt benchmarking with recipe presets |

See [`examples/FIXTURES.md`](examples/FIXTURES.md) for detailed documentation on each example.

---

## 🏗️ Architecture

### Compiler Pipeline

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

1. **Parse** (`pkg/dsl/parser/`) — Indent-sensitive lexer + recursive-descent parser produces an AST
2. **Compile** (`pkg/dsl/ir/compile.go`) — Transforms AST to IR, resolves template references, binds schemas and prompts
3. **Validate** (`pkg/dsl/ir/validate.go`) — Static analysis with 29 diagnostic codes: reachability, routing correctness, cycle detection, schema validation, and more

### Runtime Engine

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

The engine walks the IR graph, executing nodes and selecting edges. Key runtime features:

- **Parallel branches** — router `fan_out_all` spawns concurrent branches, limited by `max_parallel_branches`
- **Workspace safety** — only one mutating branch at a time; multiple read-only branches are OK
- **Shared budget** — mutex-protected token/cost/duration tracking across all branches
- **Checkpoint-based pause/resume** — the checkpoint in `run.json` is the authoritative resume source
- **Event sourcing** — every step is recorded in `events.jsonl` for observability and debugging

**Run lifecycle:** `running` → `paused_waiting_human` → `running` → `finished` | `failed` | `cancelled`

### Persistence

All run state is persisted under a configurable store directory (default: `.iterion/`):

```
.iterion/runs/<run_id>/
  run.json                     # Run metadata & checkpoint
  events.jsonl                 # Append-only event log
  artifacts/<node_id>/
    0.json, 1.json, ...       # Versioned node outputs
  interactions/<id>.json       # Human Q&A exchanges
  report.md                    # Generated run report
```

See [`docs/persisted-formats.md`](docs/persisted-formats.md) for the full specification.

---

## 🛠️ Development

This section is for contributors working on the Iterion codebase itself.

### Prerequisites

- [Devbox](https://www.jetify.com/devbox) — portable dev environment (installs Go, Task, Node)
- [direnv](https://direnv.net/) — auto-activates the Devbox shell

```bash
eval "$(direnv hook bash)"   # or: eval "$(direnv hook zsh)"
direnv allow
```

The repository also includes a `.devcontainer/` configuration for VS Code / GitHub Codespaces.

### Build & Test

```bash
task build          # → ./iterion
task test           # Unit tests
task test:e2e       # End-to-end tests (stub executor)
task test:live      # Live e2e tests (requires API keys)
task test:race      # Tests with race detector
task lint           # go fmt + go vet
task check          # lint + test
task editor:dev     # Start editor in dev mode (HMR)
task editor:build   # Build editor frontend
```

Or directly with Go:

```bash
go build -o iterion ./cmd/iterion
go test ./...
```

### Project Structure

The Go code follows the standard `cmd/` + `pkg/` layout:

```
iterion/
├── cmd/iterion/         # CLI entry point (Cobra, one file per command)
├── pkg/
│   ├── dsl/             # DSL pipeline
│   │   ├── parser/      # Lexer, recursive-descent parser, diagnostics
│   │   ├── ast/         # AST definitions and JSON marshaling
│   │   ├── ir/          # IR compilation and validation
│   │   ├── unparse/     # IR → .iter serialization
│   │   └── types/       # Shared enums (transports, session/router modes…)
│   ├── backend/         # Execution stack (LLM + tools)
│   │   ├── model/       # Executor registry, schema validation, event hooks
│   │   ├── delegate/    # Delegation backends (claude_code, codex, claw)
│   │   ├── tool/        # Tool registry, policies, adapters
│   │   ├── mcp/         # MCP server lifecycle, configuration, health checks
│   │   ├── recipe/      # Recipe handling for tool adapters and policies
│   │   ├── cost/        # Cost estimation and budgeting
│   │   └── llmtypes/    # LLM SDK abstraction
│   ├── runtime/         # Workflow execution engine (scheduling, budget, recovery)
│   ├── store/           # File-backed persistence (runs, events, artifacts)
│   ├── server/          # HTTP server for the editor backend (embedded UI)
│   ├── cli/             # CLI command implementations
│   ├── benchmark/       # Metrics collection and reporting
│   ├── log/             # Leveled logger
│   └── internal/        # Internal utilities (e.g. appinfo)
├── editor/              # Web UI (React/Vite/TypeScript + XYFlow)
├── examples/            # Reference .iter workflows + companion docs
├── e2e/                 # End-to-end test suite (stub + live)
├── docs/                # Format specifications, references, ADRs, observability
├── scripts/             # Build helpers
└── vendor/              # Vendored Go modules (incl. claw-code-go)
```

**Key dependencies:** Go 1.25.0 and [`claw-code-go`](https://github.com/ethpandaops/claw-code-go) (vendored under `vendor/claw-code-go/`) — a multi-provider LLM client. iterion uses `claw-code-go/pkg/api.Client.StreamResponse` directly for in-process LLM calls (Anthropic and OpenAI validated; Bedrock/Vertex/Foundry available).

---

## 📄 License

Apache-2.0. See `LICENSE` for the full text. Copyright © SocialGouv.
