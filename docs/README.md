[← Iterion](../README.md)

# Documentation

Welcome to the Iterion docs. Pages are organised by audience and topic.

## Getting started

| Page | Read this if… |
|---|---|
| [install.md](install.md) | …you want to install Iterion. Covers all six delivery modes (CLI, web editor, desktop, Docker, cloud, SDK). |
| [visual-editor.md](visual-editor.md) | …you want a browser-based drag-and-drop workflow editor. |
| [desktop.md](desktop.md) | …you want the native window app with multi-project + OS keychain + auto-update. |
| [examples.md](examples.md) | …you want to learn from working `.iter` files. |
| [skill.md](skill.md) | …you want your AI coding agent (Claude Code, Cursor, Copilot…) to author workflows for you. |

## Authoring `.iter` workflows

| Page | Topic |
|---|---|
| [dsl.md](dsl.md) | Full DSL reference — variables, prompts, schemas, node types, edges, templates, MCP, budget, worktree/sandbox. |
| [routers.md](routers.md) | Deep dive on routing modes (`fan_out_all`, `condition`, `round_robin`, `llm`) and convergence patterns. |
| [recipes.md](recipes.md) | Run the same workflow with different presets (vars, prompts, budget, success criteria). |
| [delegation.md](delegation.md) | When to use `delegate:` (claude_code, codex) vs `model:` (claw). |
| [attachments.md](attachments.md) | Attaching files / images to prompts. |
| [privacy_filter.md](privacy_filter.md) | Built-in PII redaction tools. |
| [workflow_authoring_pitfalls.md](workflow_authoring_pitfalls.md) | **Required reading before authoring workflows that commit code.** Goodhart's law, façade patterns, prompt + judge anti-patterns. |
| [references/dsl-grammar.md](references/dsl-grammar.md) | Readable grammar reference. |
| [references/patterns.md](references/patterns.md) | 10 reusable workflow patterns with annotated snippets. |
| [references/diagnostics.md](references/diagnostics.md) | All validation diagnostic codes (C001–C043). |
| [grammar/iterion_v1.ebnf](grammar/iterion_v1.ebnf) | Formal EBNF grammar. |

## Running and operating

| Page | Topic |
|---|---|
| [cli-reference.md](cli-reference.md) | Every `iterion` subcommand and its flags. |
| [resume.md](resume.md) | Resume / failure / cancellation matrix. |
| [sandbox.md](sandbox.md) | Per-run container isolation (Docker/Podman/Kubernetes + CONNECT proxy). |
| [observability/README.md](observability/README.md) | Prometheus metrics, OTLP traces, Grafana dashboards. |
| [persisted-formats.md](persisted-formats.md) | On-disk format spec for `run.json`, `events.jsonl`, artifacts, interactions. |

## Cloud mode

| Page | Topic |
|---|---|
| [cloud.md](cloud.md) | Architecture overview — server + runner + Mongo + NATS + S3. |
| [cloud-deployment.md](cloud-deployment.md) | Operator runbook: secrets, NetworkPolicy, observability, resume, migration. |

## Architecture & contributing

| Page | Topic |
|---|---|
| [architecture.md](architecture.md) | Compiler pipeline, runtime engine, persistence layout. |
| [adr/](adr/) | Architecture Decision Records (router semantics, AssetServer proxy, runview separation, privacy tools). |
| [development.md](development.md) | Build, test, project structure — for contributors working on the Iterion codebase itself. |
| [desktop-architecture.md](desktop-architecture.md) | Desktop app's proxy-based AssetServer architecture (Wails v2 + embedded `pkg/server`). |
| [desktop-build.md](desktop-build.md) | Local build flow + Docker reproducible builder + per-OS deps. |
| [desktop-distribution.md](desktop-distribution.md) | Release signing + Ed25519 keypair setup. |
| [desktop-qa.md](desktop-qa.md) | QA checklist for releases. |
| [e2e_coverage.md](e2e_coverage.md) | End-to-end test coverage map. |
