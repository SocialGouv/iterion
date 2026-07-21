[← Iterion](../README.md)

# Documentation

This index describes the current repository state. Guides and references below are maintained against `main`; ADRs, dated plans, audits, reviews, and bot-run bilans are point-in-time records and may intentionally describe an older state.

## Start here

| Page | Purpose |
|---|---|
| [why-iterion.md](why-iterion.md) | Product rationale, workflow patterns, and the asymptote lens. |
| [install.md](install.md) | Install through the CLI, studio, desktop, Docker, cloud, dispatcher, scheduler, or TypeScript SDK. |
| [examples.md](examples.md) | Maintained bot catalogue and focused DSL examples. |
| [cli-reference.md](cli-reference.md) | Complete top-level CLI map plus the commonly used commands and flags. |
| [visual-editor.md](visual-editor.md) | Browser-based studio, graph editor, launch forms, and live diagnostics. |
| [skill.md](skill.md) | Install Iterion guidance into AI coding agents. |

For the architectural trade-off against prompt-only orchestration, read [why-not-prompt-orchestration.md](why-not-prompt-orchestration.md). [asymptote-bench.md](asymptote-bench.md) and [thinking-metrics.md](thinking-metrics.md) cover workflow-quality measurement.

## Author `.bot` workflows

### Language and graph construction

| Page | Topic |
|---|---|
| [dsl.md](dsl.md) | Language guide and map of every declaration, node family, edge form, and workflow control. |
| [references/dsl-grammar.md](references/dsl-grammar.md) | Readable grammar derived from the parser surface. |
| [grammar/iterion_v1.ebnf](grammar/iterion_v1.ebnf) | Formal EBNF counterpart. |
| [grammar/V1_SCOPE.md](grammar/V1_SCOPE.md) | Living boundary of the additively evolved V1 grammar and AST. |
| [references/diagnostics.md](references/diagnostics.md) | Authoritative sparse catalogue: DSL C001–C199 and bundle checks C200–C230. |
| [routers.md](routers.md) | Five routing modes, per-item fan-out, and convergence. |
| [groups-iteration-subbots.md](groups-iteration-subbots.md) | `group`/`use`, edge `foreach`, `fan_out_each`, resources, and nested bots. |
| [human-in-the-loop.md](human-in-the-loop.md) | Human nodes and all five interaction values, including the node-specific `none` behavior. |
| [cursors.md](cursors.md) | Prompt-calibration cursor declarations and node activation. |
| [supervisors.md](supervisors.md) | Concurrent run watchers and steering messages. |
| [dsl-totality-and-tc.md](dsl-totality-and-tc.md) | Language totality, fuel, liveness, and Turing-completeness boundaries. |

### Inputs, capabilities, and reuse

| Page | Topic |
|---|---|
| [recipes.md](recipes.md) | In-source presets and external recipe overlays. |
| [attachments.md](attachments.md) | File and image inputs. |
| [bundles.md](bundles.md) | Deterministic `.botz` packaging with skills and resources. |
| [import.md](import.md) | Lossy, non-executing import of Claude Code workflow JavaScript into draft `.bot` files. |
| [backends.md](backends.md) | Backend/model matrix, harness behavior, and provider support. |
| [delegation.md](delegation.md) | Choosing in-process `model:` calls or delegated CLI `backend:` execution. |
| [permissions.md](permissions.md) | Workflow/node permission modes and allow/ask/deny rules. |
| [skills-library.md](skills-library.md) | Local project/global skill library and DSL `skills:` references. |
| [plugins.md](plugins.md) | Local and cloud plugins, built-ins, MCP injection, and git-backed org sources. |
| [memory-and-knowledge.md](memory-and-knowledge.md) | Memory scopes, visibility, lifecycle, CLI, and cloud quotas. |
| [web-search.md](web-search.md) | Tiered search/fetch/browser capabilities. |
| [ultracode.md](ultracode.md) | Output-compression modes and backend limits. |
| [secrets.md](secrets.md) | Local sealed secret store and CLI workflow. |
| [secrets-reference.md](secrets-reference.md) | Unified map of local/cloud secret kinds and sealing boundaries. |
| [privacy_filter.md](privacy_filter.md) | Built-in PII redaction and restoration tools. |

### Authoring practice

| Page | Topic |
|---|---|
| [workflow_authoring_pitfalls.md](workflow_authoring_pitfalls.md) | Required reading for code-mutating workflows: anti-façade and anti-Goodhart rules. |
| [references/patterns.md](references/patterns.md) | Reusable graph patterns. |
| [references/productive-session-patterns.md](references/productive-session-patterns.md) | Minimal-framing patterns learned from productive agent sessions. |
| [references-bootstrap.md](references-bootstrap.md) | Building grounded reference packs for bot skills. |

## Run and operate locally

| Page | Topic |
|---|---|
| [bot-invocations.md](bot-invocations.md) | Manifest-driven command, board, schedule, and forge invocation modes. |
| [resume.md](resume.md) | Current resume states, checkpoint semantics, overrides, and stale-run safeguards. |
| [merge-policy.md](merge-policy.md) | Worktree finalization, branch ownership, and merge authority. |
| [review-merge-gate.md](review-merge-gate.md) | Review-environment conversation and final merge gate. |
| [sandbox.md](sandbox.md) | Docker, Podman, and Kubernetes isolation, bot/repository `devbox.json` tool provisioning, and egress proxy policy. |
| [scheduling.md](scheduling.md) | Cron schedules, sub-minute keepalive, overlap guards, and audit history. |
| [dispatcher.md](dispatcher.md) | Tracker polling, leases, retries, hooks, and per-issue bot dispatch. |
| [native-tracker.md](native-tracker.md) | File-backed kanban tracker used by the dispatcher and studio. |
| [session-board.md](session-board.md) | Session and pipeline board projections. |
| [repo-scope.md](repo-scope.md) | Project root and repository-scope behavior. |
| [settings-precedence.md](settings-precedence.md) | CLI, environment, project, user, and workflow precedence. |
| [config-share.md](config-share.md) | Scoped, role-aware configuration sharing/editor surface. |
| [browser-pane.md](browser-pane.md) | Studio browser pane and isolation boundaries. |
| [post-mortem-shell.md](post-mortem-shell.md) | Controlled shell access after a run. |
| [persisted-formats.md](persisted-formats.md) | Filesystem run, checkpoint, event, artifact, interaction, attachment, plan, tool-blob, and message contracts. |
| [observability/README.md](observability/README.md) | Prometheus, OTLP, Grafana, and operational metrics. |

## Bots and security automation

| Page | Topic |
|---|---|
| [examples.md](examples.md) | All maintained repository bots and learning examples. |
| [security-bots.md](security-bots.md) | Source and dependency audit bots. |
| [security-bots-distributed.md](security-bots-distributed.md) | Distributed security-bot operation. |
| [security-patcher.md](security-patcher.md) | Security remediation workflow and boundaries. |

## Cloud / Bot-as-a-Service

Start with [baas-overview.md](baas-overview.md) for the event → queued run → result-posted-back loop, or [cloud.md](cloud.md) for the deployable components.

### Users, teams, and integrations

| Page | Topic |
|---|---|
| [cloud-user.md](cloud-user.md) | Login, teams, invitations, credentials, PATs, and password reset. |
| [forge-integrations.md](forge-integrations.md) | GitHub/GitLab/Forgejo connections, app/token setup, and bot enablement. |
| [forge-permissions.md](forge-permissions.md) | Least-privilege forge permissions and minted-token scopes. |
| [forge-conversations.md](forge-conversations.md) | Command/reply routing for PR/MR conversations. |
| [webhooks.md](webhooks.md) | Inbound provider/generic webhooks, authentication, idempotency, and CRUD. |
| [outbound-callbacks.md](outbound-callbacks.md) | Signed run-result callbacks to launchers. |
| [byok.md](byok.md) | Bring-your-own LLM API keys. |
| [oauth-forfait.md](oauth-forfait.md) | Delegated subscription/OAuth credentials. |
| [quotas-and-limits.md](quotas-and-limits.md) | Run, cost, concurrency, rate, memory, and storage limits. |
| [cloud-cli.md](cloud-cli.md) | Full `iterion remote` operator/user CLI. |
| [cloud-rest-api.md](cloud-rest-api.md) | REST surface by domain and authorization class. |

### Operators and architecture

| Page | Topic |
|---|---|
| [cloud-deployment.md](cloud-deployment.md) | Helm deployment, configuration, migration, and runbook. |
| [cloud-architecture.md](cloud-architecture.md) | Control/data planes, queue contract, isolation, and multitenancy. |
| [baas-admin-guide.md](baas-admin-guide.md) | Platform and organization administration. |
| [cloud-admin.md](cloud-admin.md) | Bootstrap admin, SSO, credentials, and rotation. |
| [cloud-backup.md](cloud-backup.md) | Mongo/S3 backup and restore. |
| [cloud-troubleshooting.md](cloud-troubleshooting.md) | Symptoms-first cloud troubleshooting. |
| [cloud-public-exposure-checklist.md](cloud-public-exposure-checklist.md) | Pre-exposure security and reliability checklist. |
| [ci-performance-buildkit-operator.md](ci-performance-buildkit-operator.md) | BuildKit operator and CI-cache tuning. |

## Desktop

| Page | Topic |
|---|---|
| [desktop.md](desktop.md) | End-user desktop application. |
| [desktop-architecture.md](desktop-architecture.md) | Wails/AssetServer proxy architecture. |
| [desktop-build.md](desktop-build.md) | Local and reproducible desktop builds. |
| [desktop-distribution.md](desktop-distribution.md) | Signing and distribution. |
| [desktop-qa.md](desktop-qa.md) | Developer smoke checks. |
| [desktop-qa-checklist.md](desktop-qa-checklist.md) | Cross-platform release QA matrix. |
| [desktop-release-checklist.md](desktop-release-checklist.md) | Tag, sign, publish, verify, and roll back. |

## Architecture and contribution

| Page | Topic |
|---|---|
| [architecture.md](architecture.md) | Parser/compiler pipeline, execution engine, persistence, and UI/server boundaries. |
| [development.md](development.md) | Reproducible toolchain, task graph, tests, and repository structure. |
| [e2e_coverage.md](e2e_coverage.md) | Stubbed end-to-end coverage map. |
| [live-e2e-coverage.md](live-e2e-coverage.md) | Credentialed/live E2E coverage and compile guards. |

## Point-in-time records

These collections are valuable evidence, but they do not override current code or the living references above:

- [adr/](adr/) — immutable architecture decision records; later ADRs may supersede earlier ones.
- [bot-runs/](bot-runs/) — dated dogfood bilans and lessons for each bot.
- [plans/](plans/) and [c082-board-emit-fix-plan.md](c082-board-emit-fix-plan.md) — implementation plans.
- [reviews/](reviews/) — dated codebase reviews.
- [security/](security/) — dated security audits.
- [studio-ux-audit-2026-07.md](studio-ux-audit-2026-07.md) — UX audit snapshot.
