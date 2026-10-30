# Documentation

This index describes the current repository state. Guides and references below are maintained against `main`; ADRs, dated plans, audits, reviews, and bot-run bilans are point-in-time records and may intentionally describe an older state.

## Start here

| Page | Purpose |
|---|---|
| [getting-started.md](getting-started.md) | One copy-paste prompt that has your AI agent install the CLI and skill, connect to iterion.cloud, and verify readiness to deploy. |
| [current-state.md](current-state.md) | Living as-built overview: shipped surfaces, runtime/backend status, security defaults, cloud control plane, and known limits. |
| [why-iterion.md](why-iterion.md) | Product rationale, workflow patterns, and the asymptote lens. |
| [install.md](install.md) | Install through the CLI, studio, desktop, Docker, cloud, dispatcher, scheduler, or TypeScript SDK. |
| [examples.md](examples.md) | Maintained bot catalogue and focused DSL examples. |
| [cli-reference.md](cli-reference.md) | Complete top-level CLI map plus the commonly used commands and flags. |
| [visual-editor.md](visual-editor.md) | Browser-based studio, graph editor, launch forms, and live diagnostics. |
| [assistant-dock.md](assistant-dock.md) | The assistant reachable from every studio route: which bot answers (manifest-driven `chat:` registry), its page-context chip, dragging a run/card/bot onto the composer, and how it differs from run steering. |
| [skill.md](skill.md) | Install Iterion guidance into AI coding agents. |

For the architectural trade-off against prompt-only orchestration, read [why-not-prompt-orchestration.md](why-not-prompt-orchestration.md). [asymptote-bench.md](asymptote-bench.md) and [thinking-metrics.md](thinking-metrics.md) cover workflow-quality measurement, and [improvement-ratchet.md](improvement-ratchet.md) names what carries a gain from one run to the next. [references/context-retrieval-state-of-the-art.md](references/context-retrieval-state-of-the-art.md) measures what finding your way around this repository costs (`iterion bench discovery`) and maps the indexing / knowledge-graph literature onto what iterion can actually finance — read it before proposing an index, a code graph or an embedding store. The generated indexes themselves are [references/map-packages.md](references/map-packages.md) (packages, with the interfaces each exposes), [references/map-docs.md](references/map-docs.md) (pages and ADRs, with status) and [references/map-bots.md](references/map-bots.md) (bundles and skills) — rewritten by `task map:gen`, held to the tree by `task map:check`.

## Comparisons

- [layering.md](layering.md) — the package layers as an executable guard: who may import whom, the pinned exception count, the ratchet.
- [Iterion and alternatives](comparisons/index.md) — agent workflow architectures, developer experience and decision criteria.
- [Feature matrix](comparisons/feature-matrix.md) — 21 features across 10 products, with sources and conditions.
- [Iterion feature inventory](comparisons/feature-inventory.md) — 140 criteria across 16 categories.
- [Methodology and sources](comparisons/methodology.md) — capability scope and proposed integrations.

## Author `.bot` workflows

### Language and graph construction

| Page | Topic |
|---|---|
| [dsl.md](dsl.md) | Language guide and map of every declaration, node family, edge form, and workflow control. |
| [references/dsl-grammar.md](references/dsl-grammar.md) | Readable grammar derived from the parser surface. |
| [references/dsl-properties.md](references/dsl-properties.md) | Every kind's properties, value shapes and meaning — generated from the parser's registry (`task dsl:gen`), held to the parser by a conformance test. |
| [grammar/iterion_v1.ebnf](grammar/iterion_v1.ebnf) | Formal EBNF counterpart. |
| [grammar/V1_SCOPE.md](grammar/V1_SCOPE.md) | Living boundary of the additively evolved V1 grammar and AST. |
| [references/diagnostics.md](references/diagnostics.md) | Authoritative sparse catalogue: DSL C001–C199 plus async C240–C242, and bundle checks C200–C234. |
| [routers.md](routers.md) | Five routing modes, per-item fan-out, and convergence. |
| [groups-iteration-subbots.md](groups-iteration-subbots.md) | `group`/`use`, edge `foreach`, `fan_out_each`, resources, and nested bots. |
| [human-in-the-loop.md](human-in-the-loop.md) | Human nodes and all six interaction values, including the node-specific `none` and `async` behavior. |
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
| [models.md](models.md) | The model registry (`GET /api/models`, `iterion models`), the picker's capability guard, and the operator's persisted model choice. |
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
| [references/dsl-authoring-probe.md](references/dsl-authoring-probe.md) | The protocol that measures whether an agent writes a correct bot on the first draft (three specs, what to record, the baseline and the targets). |
| [references/productive-session-patterns.md](references/productive-session-patterns.md) | Minimal-framing patterns learned from productive agent sessions. |
| [references-bootstrap.md](references-bootstrap.md) | Building grounded reference packs for bot skills. |

## Run and operate locally

| Page | Topic |
|---|---|
| [bot-invocations.md](bot-invocations.md) | Manifest-driven command, board, schedule, and forge invocation modes. |
| [resume.md](resume.md) | Current resume states, checkpoint semantics, overrides, and stale-run safeguards. |
| [merge-policy.md](merge-policy.md) | The merge queue guarding `main`: how it works, the admin bypass and the incident fast lane, releases, measuring the queue, reverting. |
| [review-merge-gate.md](review-merge-gate.md) | Review-environment conversation and final merge gate. |
| [merge-gate.md](merge-gate.md) | The deterministic merge-gate check: verdicts, gate contexts, refusal and interruption repair, and the gate-switch procedure. |
| [sandbox.md](sandbox.md) | Docker, Podman, and Kubernetes isolation, devbox provisioning, egress policy, and ADR-127 undecided-effect propagation/engine-exec inventory. |
| [scheduling.md](scheduling.md) | Cron schedules, sub-minute keepalive, overlap guards, and audit history. |
| [dispatcher.md](dispatcher.md) | Tracker polling, leases, retries, hooks, and per-issue bot dispatch. |
| [native-tracker.md](native-tracker.md) | File-backed kanban tracker used by the dispatcher and studio. |
| [session-board.md](session-board.md) | Session and pipeline board projections. |
| [repo-scope.md](repo-scope.md) | Project root and repository-scope behavior. |
| [settings-precedence.md](settings-precedence.md) | CLI, environment, project, user, and workflow precedence. |
| [environment-variables.md](environment-variables.md) | Reference of the `ITERION_*` environment variables and their effects. |
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

## Cloud / agent control plane

Start with the [Iterion Cloud overview](cloud-overview.md) for the event → queued run → result-posted-back loop, or [cloud.md](cloud.md) for the deployable components.

### Users, teams, and integrations

| Page | Topic |
|---|---|
| [cloud-user.md](cloud-user.md) | Login, teams, invitations, credentials, PATs, and password reset. |
| [forge-integrations.md](forge-integrations.md) | GitHub/GitLab/Forgejo connections, app/token setup, and bot enablement. |
| [forge-permissions.md](forge-permissions.md) | Least-privilege forge permissions and minted-token scopes. |
| [forge-conversations.md](forge-conversations.md) | Command/reply routing for PR/MR conversations. |
| [webhooks.md](webhooks.md) | Inbound provider/generic webhooks, authentication, idempotency, and CRUD. |
| [webhooks/retry.md](webhooks/retry.md) | Early delivery receipts, bounded launch retry sweep, super-admin retry API and terminal outcomes. |
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
| [cloud-admin-guide.md](cloud-admin-guide.md) | Platform and organization administration. |
| [cloud-admin.md](cloud-admin.md) | Bootstrap admin, SSO, credentials, and rotation. |
| [cloud-backup.md](cloud-backup.md) | Mongo/S3 backup and restore. |
| [cloud-troubleshooting.md](cloud-troubleshooting.md) | Symptoms-first cloud troubleshooting. |
| [cloud-public-exposure-checklist.md](cloud-public-exposure-checklist.md) | Pre-exposure security and reliability checklist. |
| [sovereign-pools.md](sovereign-pools.md) | Sovereign runner pools: team→pool mapping, registry, per-pool topology, admission, server-surface refusals, and the admin surface. |
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
| [agents/](agents/) | **Working on iterion itself**: the doctrine tree every agent session and contributor follows — the required before-merge review loop, bot-authoring rules, testing and dogfood discipline, the operational runbook index. [AGENTS.md](../AGENTS.md), the instruction root every agent loads, routes to it. |
| [architecture.md](architecture.md) | End-to-end compiler, runtime, backend, persistence, control-plane, automation, and extension architecture. |
| [development.md](development.md) | Reproducible toolchain, task graph, tests, and repository structure. |
| [e2e_coverage.md](e2e_coverage.md) | Stubbed end-to-end coverage map. |
| [e2e-coverage-matrix.md](e2e-coverage-matrix.md) | Feature coverage inventory, Studio browser advisory lane, skip semantics and regression witnesses. |
| [live-e2e-coverage.md](live-e2e-coverage.md) | Credentialed/live E2E coverage and compile guards. |

## Diagrams

Drawn from the code: the Mongo data model as entity-relationship diagrams and the access-control matrix.

| Page | Topic |
|---|---|
| [diagrams/README.md](diagrams/README.md) | Index, conventions, legend and glossary — how the diagrams are kept true. |
| [diagrams/data-model-identity.md](diagrams/data-model-identity.md) | Orgs, teams, users, memberships, invitations, personal credentials, SSO config, audit. |
| [diagrams/data-model-forge-bots.md](diagrams/data-model-forge-bots.md) | Forge connections, repository bindings, webhooks, bot tiers — Revi bound to a repository inside a team. |
| [diagrams/data-model-runs.md](diagrams/data-model-runs.md) | A run, its streams, the run tree, assistant supervision, the status machine. |
| [diagrams/data-model-credentials.md](diagrams/data-model-credentials.md) | Credentials at rest and in flight, the meters, the routing policy, the tier walk. |
| [diagrams/data-model-automation.md](diagrams/data-model-automation.md) | The trigger spine, the scheduled surfaces, the leases. |
| [diagrams/data-model-knowledge.md](diagrams/data-model-knowledge.md) | Memory, config shares, notifications. |
| [diagrams/access-control.md](diagrams/access-control.md) | Principal kinds, roles, the authorization decision points, the gate × principal matrix. |
| [diagrams/flow-auth-login.md](diagrams/flow-auth-login.md) | Flow — signing in and authenticating a request. |
| [diagrams/flow-org-team-lifecycle.md](diagrams/flow-org-team-lifecycle.md) | Flow — org and team lifecycle: suspend, soft-delete, purge. |
| [diagrams/flow-forge-connect-enable-bot.md](diagrams/flow-forge-connect-enable-bot.md) | Flow — connect a forge, enable a bot on a repository. |
| [diagrams/flow-webhook-to-run.md](diagrams/flow-webhook-to-run.md) | Flow — webhook-to-run: the admission gates, every refusal, and the one chokepoint. |
| [diagrams/flow-revi-review-merge-gate.md](diagrams/flow-revi-review-merge-gate.md) | Flow — The revi/review merge gate. |
| [diagrams/flow-cloud-run-lifecycle.md](diagrams/flow-cloud-run-lifecycle.md) | Flow — Cloud run lifecycle. |
| [diagrams/flow-credential-resolution.md](diagrams/flow-credential-resolution.md) | Flow — Credential resolution — which credential funds a cloud run. |
| [diagrams/flow-secrets-sealed-bundle.md](diagrams/flow-secrets-sealed-bundle.md) | Flow — the sealed credentials bundle. |
| [diagrams/flow-config-share.md](diagrams/flow-config-share.md) | Flow — config-share: the mint, the two access paths, the fail-closed write. |
| [diagrams/flow-triggers-and-schedules.md](diagrams/flow-triggers-and-schedules.md) | Flow — triggers and schedules: five sources, two deliveries, three clocks. |
| [diagrams/flow-pause-resume-rewind.md](diagrams/flow-pause-resume-rewind.md) | Flow — Pause, resume and rewind. |
| [diagrams/flow-board-dispatcher.md](diagrams/flow-board-dispatcher.md) | Flow — Board dispatch. |
| [diagrams/flow-bot-resolution-overrides.md](diagrams/flow-bot-resolution-overrides.md) | Flow — Bot resolution — team row, platform override, baked catalog. |
| [diagrams/flow-memory-knowledge.md](diagrams/flow-memory-knowledge.md) | Flow — memory and knowledge: four writers, one seam, two adapters. |
| [diagrams/flow-sandbox-isolation.md](diagrams/flow-sandbox-isolation.md) | Flow — sandbox isolation. |
| [diagrams/flow-notifications-callbacks.md](diagrams/flow-notifications-callbacks.md) | Flow — notifications and callbacks: one outcome event, three outbound families. |
| [diagrams/flow-bot-to-running-graph.md](diagrams/flow-bot-to-running-graph.md) | Flow — From a `.bot` to a running graph. |
| [diagrams/flow-assistant-dock-session-board.md](diagrams/flow-assistant-dock-session-board.md) | Flow — the assistant dock, run watches and the session board. |

## Point-in-time records

These collections are valuable evidence, but they do not override current code or the living references above:

- [adr/](adr/) — immutable architecture decision records; later ADRs may supersede earlier ones.
- [bot-runs/](bot-runs/) — dated dogfood bilans and lessons for each bot.
- [c082-board-emit-fix-plan.md](c082-board-emit-fix-plan.md) — retained implementation plan.
- [reviews/](reviews/) — dated codebase reviews.
- [security/](security/) — dated security audits.
- [studio-ux-audit-2026-07.md](studio-ux-audit-2026-07.md) — UX audit snapshot.
- [upstream/](upstream/) — draft reports and measured evidence for upstream (Anthropic) findings; nothing here is filed without an operator decision.
