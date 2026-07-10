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
| [install.md](install.md) | …you want to install Iterion. Covers all eight delivery modes (CLI, studio, desktop, Docker, cloud, dispatcher, scheduler, SDK). |
| [visual-editor.md](visual-editor.md) | …you want a browser-based drag-and-drop workflow editor. |
| [desktop.md](desktop.md) | …you want the native window app with multi-project + OS keychain + auto-update. |
| [examples.md](examples.md) | …you want to learn from working `.bot` files. |
| [skill.md](skill.md) | …you want your AI coding agent (Claude Code, Cursor, Copilot…) to author workflows for you. |

## Authoring `.bot` workflows

| Page | Topic |
|---|---|
| [dsl.md](dsl.md) | Full DSL reference — variables, prompts, schemas, node types, edges, templates, MCP, budget, worktree/sandbox. |
| [routers.md](routers.md) | Deep dive on routing modes (`fan_out_all`, `condition`, `round_robin`, `llm`) and convergence patterns. |
| [human-in-the-loop.md](human-in-the-loop.md) | Pause for human input — the `human` node, form widgets, and the four interaction modes (`human` / `llm` / `llm_or_human` / `review`). |
| [recipes.md](recipes.md) | Run the same workflow with different presets (vars, prompts, budget, success criteria). |
| [delegation.md](delegation.md) | When to use `backend:` (claude_code, codex) vs `model:` (claw). |
| [attachments.md](attachments.md) | Attaching files / images to prompts. |
| [bundles.md](bundles.md) | Packaging a workflow + skills + prompts as a deterministic `.botz` archive. |
| [skills-library.md](skills-library.md) | The standalone skill library (`~/.iterion/skills/`, `iterion skill`) referenced from workflows via the DSL `skills:` field. |
| [security-bots.md](security-bots.md) | The `sec-audit-source` + `sec-audit-deps` bundles — universal security auditors (source-code SAST and supply-chain malware) with cross-run FP / package memory. |
| [privacy_filter.md](privacy_filter.md) | Built-in PII redaction tools. |
| [workflow_authoring_pitfalls.md](workflow_authoring_pitfalls.md) | **Required reading before authoring workflows that commit code.** Goodhart's law, façade patterns, prompt + judge anti-patterns. |
| [references/dsl-grammar.md](references/dsl-grammar.md) | Readable grammar reference. |
| [references/patterns.md](references/patterns.md) | 10 reusable workflow patterns with annotated snippets. |
| [references/diagnostics.md](references/diagnostics.md) | All validation diagnostic codes (C001–C086, sparse). |
| [grammar/iterion_v1.ebnf](grammar/iterion_v1.ebnf) | Formal EBNF grammar. |

## Running and operating

| Page | Topic |
|---|---|
| [cli-reference.md](cli-reference.md) | Every `iterion` subcommand and its flags. |
| [resume.md](resume.md) | Resume / failure / cancellation matrix. |
| [sandbox.md](sandbox.md) | Per-run container isolation (Docker/Podman/Kubernetes + CONNECT proxy). |
| [observability/README.md](observability/README.md) | Prometheus metrics, OTLP traces, Grafana dashboards. |
| [persisted-formats.md](persisted-formats.md) | On-disk format spec for `run.json`, `events.jsonl`, artifacts, interactions. |

## Cloud platform (Bot-as-a-Service)

Iterion ships as a self-hostable multi-tenant platform — orgs + teams,
BYOK LLM keys, inbound webhooks for 4 forges, NATS-queued runner pool,
per-org quotas, audit log, PATs, SMTP onboarding. We call it
**Bot-as-a-Service** (BaaS).

| Page | Topic |
|---|---|
| [baas-overview.md](baas-overview.md) | Start here — the event → autonomous agent → result-posted-back loop, a concrete GitLab-MR → Revi walkthrough, the primitives table. |
| [webhooks.md](webhooks.md) | Inbound webhooks (GitLab + `/revi`, GitHub, Forgejo/Gitea, generic) — auth modes, idempotency, CRUD API. |
| [forge-integrations.md](forge-integrations.md) | Connect a GitLab/GitHub/Forgejo repo (OAuth/PAT) and auto-provision the webhook + token binding when you enable a bot — the self-serve replacement for the manual webhook chain. |
| [quotas-and-limits.md](quotas-and-limits.md) | Run/cost/concurrency/rate caps, denial reasons + HTTP semantics, Prometheus metrics. |
| [baas-admin-guide.md](baas-admin-guide.md) | Platform operator + org admin runbook (UI paths + curl), DLQ triage, audit, PATs. |
| [secrets-reference.md](secrets-reference.md) | The single map of every secret kind (BYOK, generic, bindings, file, OAuth-forfait, tokens) and the sealing model. |
| [cloud-rest-api.md](cloud-rest-api.md) | Every REST endpoint grouped by domain, auth class, purpose. |
| [cloud-cli.md](cloud-cli.md) | `iterion remote` — drive the whole cloud instance from the CLI (login, env-var CI mode, typed subcommands per domain). |
| [memory-and-knowledge.md](memory-and-knowledge.md) | Memory visibilities, per-org + per-space quotas, REST surface. |
| [cloud-architecture.md](cloud-architecture.md) | Control plane vs data plane, run lifecycle + sealed bundle, queue internals, multitenancy enforcement layers. |
| [outbound-callbacks.md](outbound-callbacks.md) | The mirror direction — runs POSTing their result back to the launcher. |

## Cloud mode — operator

| Page | Topic |
|---|---|
| [cloud.md](cloud.md) | Architecture overview — server + runner + Mongo + NATS + S3. |
| [cloud-deployment.md](cloud-deployment.md) | Operator runbook: secrets, NetworkPolicy, observability, resume, migration. |
| [cloud-admin.md](cloud-admin.md) | Multitenant admin guide: bootstrap super-admin, SSO config, BYOK + OAuth-forfait, secret rotation. |
| [cloud-user.md](cloud-user.md) | User-facing guide: login, teams, BYOK, OAuth subscriptions, invitations, PATs, password reset. |
| [cloud-troubleshooting.md](cloud-troubleshooting.md) | Symptoms-first reference: queued runs not starting, hangs, /readyz 503s, WS streaming, budget overruns, Trivy CVE findings. |
| [cloud-public-exposure-checklist.md](cloud-public-exposure-checklist.md) | 10-section checklist before opening a deployment to public traffic. Hard prerequisites: auth, multitenancy, NetworkPolicy, secrets, image supply chain, observability, probes, budgets, backups, runbooks. |

## Architecture & contributing

| Page | Topic |
|---|---|
| [architecture.md](architecture.md) | Compiler pipeline, runtime engine, persistence layout. |
| [adr/](adr/) | Architecture Decision Records (router semantics, AssetServer proxy, runview separation, privacy tools). |
| [development.md](development.md) | Build, test, project structure — for contributors working on the Iterion codebase itself. |
| [bot-runs/](bot-runs/) | **Bot validation & knowledge base** — one dated bilan per catalog-bot dogfood run (what it caught/missed, lessons, engine bugs surfaced). Read a bot's file before launching it. |
| [desktop-architecture.md](desktop-architecture.md) | Desktop app's proxy-based AssetServer architecture (Wails v2 + embedded `pkg/server`). |
| [desktop-build.md](desktop-build.md) | Local build flow + Docker reproducible builder + per-OS deps. |
| [desktop-distribution.md](desktop-distribution.md) | Release signing + Ed25519 keypair setup. |
| [desktop-qa.md](desktop-qa.md) | Developer-facing smoke checklist (AssetServer / runtime-injection regressions). |
| [desktop-qa-checklist.md](desktop-qa-checklist.md) | Per-platform release QA matrix (Boot, Multi-proj, Settings, Onboarding, Run, Browser, Auto-update, Crash, Disconnect) with assignment grid for human testers. |
| [desktop-release-checklist.md](desktop-release-checklist.md) | Pre-tag sign-off: code freeze, versioning, signing prerequisites, dry run, trigger, post-publish verification, rollback, Ed25519 key rotation. |
| [e2e_coverage.md](e2e_coverage.md) | End-to-end test coverage map. |
