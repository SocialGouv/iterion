[← Iterion](../README.md)

# Documentation

Welcome to the Iterion docs. Pages are organised by audience and topic.

## Background

| Page | Read this if… |
|---|---|
| [why-iterion.md](why-iterion.md) | …you want the origin story, the patterns we've seen work, the asymptote lens that motivated the engine, and the workflow-lab dimension. Helps you decide whether Iterion fits how you work. |
| [why-not-prompt-orchestration.md](why-not-prompt-orchestration.md) | …you're wondering whether a single Claude Code (or similar) session calling sub-agents could replace Iterion. Lays out the structural guarantees a compiled IR provides and the three-question heuristic for choosing between the two approaches. |
| [asymptote-bench.md](asymptote-bench.md) | …you want to measure a workflow's inter-session reliability curve via `iterion bench asymptote`. |

## Getting started

| Page | Read this if… |
|---|---|
| [install.md](install.md) | …you want to install Iterion. Covers all seven delivery modes (CLI, web editor, desktop, Docker, cloud, dispatcher, SDK). |
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
| [delegation.md](delegation.md) | When to use `backend:` (claude_code, codex) vs `model:` (claw). |
| [attachments.md](attachments.md) | Attaching files / images to prompts. |
| [bundles.md](bundles.md) | Packaging a workflow + skills + prompts as a deterministic `.botz` archive. |
| [security-bots.md](security-bots.md) | The `sec-audit-source` + `sec-audit-deps` bundles — universal security auditors (source-code SAST and supply-chain malware) with cross-run FP / package memory. |
| [privacy_filter.md](privacy_filter.md) | Built-in PII redaction tools. |
| [workflow_authoring_pitfalls.md](workflow_authoring_pitfalls.md) | **Required reading before authoring workflows that commit code.** Goodhart's law, façade patterns, prompt + judge anti-patterns. |
| [references/dsl-grammar.md](references/dsl-grammar.md) | Readable grammar reference. |
| [references/patterns.md](references/patterns.md) | 10 reusable workflow patterns with annotated snippets. |
| [references/diagnostics.md](references/diagnostics.md) | All validation diagnostic codes (C001–C082, sparse). |
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
| [cloud-admin.md](cloud-admin.md) | Multitenant admin guide: bootstrap super-admin, SSO config, BYOK + OAuth-forfait, secret rotation. |
| [cloud-user.md](cloud-user.md) | User-facing guide: login, teams, BYOK, OAuth subscriptions, invitations. |
| [cloud-troubleshooting.md](cloud-troubleshooting.md) | Symptoms-first reference: queued runs not starting, hangs, /readyz 503s, WS streaming, budget overruns, Trivy CVE findings. |
| [cloud-public-exposure-checklist.md](cloud-public-exposure-checklist.md) | 10-section checklist before opening a deployment to public traffic. Hard prerequisites: auth, multitenancy, NetworkPolicy, secrets, image supply chain, observability, probes, budgets, backups, runbooks. |

## Architecture & contributing

| Page | Topic |
|---|---|
| [architecture.md](architecture.md) | Compiler pipeline, runtime engine, persistence layout. |
| [adr/](adr/) | Architecture Decision Records (router semantics, AssetServer proxy, runview separation, privacy tools). |
| [development.md](development.md) | Build, test, project structure — for contributors working on the Iterion codebase itself. |
| [desktop-architecture.md](desktop-architecture.md) | Desktop app's proxy-based AssetServer architecture (Wails v2 + embedded `pkg/server`). |
| [desktop-build.md](desktop-build.md) | Local build flow + Docker reproducible builder + per-OS deps. |
| [desktop-distribution.md](desktop-distribution.md) | Release signing + Ed25519 keypair setup. |
| [desktop-qa.md](desktop-qa.md) | Developer-facing smoke checklist (AssetServer / runtime-injection regressions). |
| [desktop-qa-checklist.md](desktop-qa-checklist.md) | Per-platform release QA matrix (Boot, Multi-proj, Settings, Onboarding, Run, Browser, Auto-update, Crash, Disconnect) with assignment grid for human testers. |
| [desktop-release-checklist.md](desktop-release-checklist.md) | Pre-tag sign-off: code freeze, versioning, signing prerequisites, dry run, trigger, post-publish verification, rollback, Ed25519 key rotation. |
| [e2e_coverage.md](e2e_coverage.md) | End-to-end test coverage map. |
