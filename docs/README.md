# Documentation

This index describes the current repository state. Guides and references below are maintained against `main`; ADRs, dated plans, audits, reviews, and bot-run bilans are point-in-time records and may intentionally describe an older state.

## Start here

| Page | Purpose |
|---|---|
| [quickstart.md](quickstart.md) | Zero to a real agent workflow — running, watched, and landed — for developers already using AI coding tools. |
| [current-state.md](current-state.md) | Living as-built overview: shipped surfaces, runtime/backend status, security defaults, cloud control plane, and known limits. |
| [why-iterion.md](why-iterion.md) | Product rationale, workflow patterns, and the asymptote lens. |
| [install.md](install.md) | Install through the CLI, studio, desktop, Docker, cloud, dispatcher, scheduler, or TypeScript SDK. |
| [examples.md](examples.md) | Maintained bot catalogue and focused DSL examples. |
| [cli-reference.md](cli-reference.md) | Complete top-level CLI map plus the commonly used commands and flags. |
| [visual-editor.md](visual-editor.md) | Browser-based studio, graph editor, launch forms, and live diagnostics. |
| [skill.md](skill.md) | Install Iterion guidance into AI coding agents. |

For the architectural trade-off against prompt-only orchestration, read [why-not-prompt-orchestration.md](why-not-prompt-orchestration.md); [philosophy.md](philosophy.md) states the stance that arbitrates an open design question. [asymptote-bench.md](asymptote-bench.md) and [thinking-metrics.md](thinking-metrics.md) cover workflow-quality measurement, and [improvement-ratchet.md](improvement-ratchet.md) names what carries a gain from one run to the next.

## Author `.bot` workflows

### Language and graph construction

| Page | Topic |
|---|---|
| [dsl.md](dsl.md) | Language guide and map of every declaration, node family, edge form, and workflow control. |
| [references/dsl-grammar.md](references/dsl-grammar.md) | Readable grammar derived from the parser surface. |
| [grammar/iterion_v1.ebnf](grammar/iterion_v1.ebnf) | Formal EBNF counterpart. |
| [grammar/V1_SCOPE.md](grammar/V1_SCOPE.md) | Living boundary of the additively evolved V1 grammar and AST. |
| [references/diagnostics.md](references/diagnostics.md) | Authoritative sparse catalogue: DSL C001–C199 plus C240–C248, and bundle checks C200–C234. |
| [routers.md](routers.md) | Five routing modes, per-item fan-out, and convergence. |
| [groups-iteration-subbots.md](groups-iteration-subbots.md) | `group`/`use`, edge `foreach`, `fan_out_each`, resources, and nested bots. |
| [human-in-the-loop.md](human-in-the-loop.md) | Human nodes and all six interaction values, including the node-specific `none` and `async` behavior. |
| [async-interaction.md](async-interaction.md) | `interaction: async` — `ask_user_async` questions that do not block, and the `await_answers` sync point (ADR-081). |
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
| [ultracode.md](ultracode.md) | `reasoning_effort: ultracode` — the mode that sends `xhigh` on the wire and grants the multi-agent orchestration prerogative, and the models that carry it. |
| [secrets.md](secrets.md) | Local sealed secret store and CLI workflow. |
| [secrets-reference.md](secrets-reference.md) | Unified map of local/cloud secret kinds and sealing boundaries. |
| [privacy_filter.md](privacy_filter.md) | Built-in PII redaction and restoration tools. |

### Authoring practice

| Page | Topic |
|---|---|
| [workflow_authoring_pitfalls.md](workflow_authoring_pitfalls.md) | Required reading for code-mutating workflows: anti-façade and anti-Goodhart rules. |
| [references/patterns.md](references/patterns.md) | Reusable graph patterns. |
| [references/productive-session-patterns.md](references/productive-session-patterns.md) | Minimal-framing patterns learned from productive agent sessions. |
| [references/external-methodologies.md](references/external-methodologies.md) | IACDM and AI-DLC cross-checked against iterion: what they validate, the rules imported into the pitfalls doc, and what iterion deliberately does differently. |
| [references-bootstrap.md](references-bootstrap.md) | Building grounded reference packs for bot skills. |

## Run and operate locally

| Page | Topic |
|---|---|
| [bot-invocations.md](bot-invocations.md) | Manifest-driven command, board, schedule, and forge invocation modes. |
| [mcp-server.md](mcp-server.md) | Driving iterion from an MCP client: `iterion mcp` on stdio, the `local_*`/`remote_*` tool families, and the `remote_api` escape hatch. |
| [resume.md](resume.md) | Current resume states, checkpoint semantics, overrides, and stale-run safeguards. |
| [review-scope.md](review-scope.md) | What a human gate shows the operator: the range since the previous gate, grouped by the node that changed it. |
| [workspace-versioning.md](workspace-versioning.md) | Content-addressed capture/restore of the files a run produces, and the scoped restore behind `iterion rewind`. |
| [worktree-pool.md](worktree-pool.md) | Where a `worktree: auto` store's disk goes, the runtime bound, and reclaiming it with `iterion clean`. |
| [merge-policy.md](merge-policy.md) | Worktree finalization, branch ownership, and merge authority. |
| [merge-gate.md](merge-gate.md) | The `revi/review` required check: the in-flight claim at launch, the verdict, and the two triggers guaranteeing a dead review still answers. |
| [review-merge-gate.md](review-merge-gate.md) | Review-environment conversation and final merge gate. |
| [revi-billy-loop.md](revi-billy-loop.md) | The Revi → Billy habit on this repository: comment `/billy` instead of hand-fixing a review's findings. |
| [sandbox.md](sandbox.md) | Docker, Podman, and Kubernetes isolation, bot/repository `devbox.json` tool provisioning, and egress proxy policy. |
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
| [observability.md](observability.md) | Process logs, Sentry/GlitchTip error tracking, and the opt-in tracing that rides the same client. |
| [observability/README.md](observability/README.md) | Prometheus, OTLP, Grafana, and operational metrics. |
| [sentry-feedback-loop.md](sentry-feedback-loop.md) | Reading production errors back: the Sentry MCP wiring and raw-API recipes for triaging a live crash. |

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
| [forge-security-read.md](forge-security-read.md) | Org-wide Dependabot-alert read access for a bot: the watch-only App, the token secret, and the coverage trap. |
| [forge-conversations.md](forge-conversations.md) | Command/reply routing for PR/MR conversations. |
| [github-board-sync.md](github-board-sync.md) | Keeping a GitHub Projects v2 board and the native board on the same tickets. |
| [ticket-context.md](ticket-context.md) | Plugging tracker tickets (Jira, GitHub/GitLab issues) into a review so it checks the PR against what the ticket asked. |
| [webhooks.md](webhooks.md) | Inbound provider/generic webhooks, authentication, idempotency, and CRUD. |
| [outbound-callbacks.md](outbound-callbacks.md) | Signed run-result callbacks to launchers. |
| [notifications.md](notifications.md) | Browser web-push notifications when a run pauses on a human gate or reaches a terminal state. |
| [byok.md](byok.md) | Bring-your-own LLM API keys. |
| [oauth-forfait.md](oauth-forfait.md) | Delegated subscription/OAuth credentials. |
| [cloud-llm-credentials.md](cloud-llm-credentials.md) | How a queued run gets its model access across the credential tiers — the runbook behind a `401`/`429` on a cloud run. |
| [credential-pool.md](credential-pool.md) | Mutualising contributors' unused subscription or API-key capacity: pledges, ceilings, audience, and leases. |
| [quotas-and-limits.md](quotas-and-limits.md) | Run, cost, concurrency, rate, memory, and storage limits. |
| [usage-caps.md](usage-caps.md) | Capping a subscription below the provider's own five-hour and weekly walls. |
| [cloud-cli.md](cloud-cli.md) | Full `iterion remote` operator/user CLI. |
| [cloud-rest-api.md](cloud-rest-api.md) | REST surface by domain and authorization class. |

### Operators and architecture

| Page | Topic |
|---|---|
| [cloud-deployment.md](cloud-deployment.md) | Helm deployment, configuration, migration, and runbook. |
| [cloud-architecture.md](cloud-architecture.md) | Control/data planes, queue contract, isolation, and multitenancy. |
| [cloud-admin-guide.md](cloud-admin-guide.md) | Platform and organization administration. |
| [cloud-admin.md](cloud-admin.md) | Bootstrap admin, SSO, credentials, and rotation. |
| [platform-bots.md](platform-bots.md) | Iterating on a bot on a live instance with no image rollout, plus the other runtime-mutable platform settings. |
| [outcome-router.md](outcome-router.md) | `ITERION_OUTCOME_ROUTER`: deciding a terminal run by its launch-frozen policy, and the rollout/emergency-stop procedure. |
| [probes-and-graceful-shutdown.md](probes-and-graceful-shutdown.md) | What `/healthz` and `/readyz` promise, the lame-duck window, and the termination-grace arithmetic. |
| [cloud-backup.md](cloud-backup.md) | Mongo/S3 backup and restore. |
| [cloud-queue-schema-rollout.md](cloud-queue-schema-rollout.md) | Shipping a `queue.RunMessage` schema bump without executing a payload whose semantics were silently dropped. |
| [cloud-troubleshooting.md](cloud-troubleshooting.md) | Symptoms-first cloud troubleshooting. |
| [cloud-public-exposure-checklist.md](cloud-public-exposure-checklist.md) | Pre-exposure security and reliability checklist. |
| [ci-pipeline-topology.md](ci-pipeline-topology.md) | Which image workflow owns which deployable artifact, and therefore which completion is a redeploy signal. |
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
| [architecture.md](architecture.md) | End-to-end compiler, runtime, backend, persistence, control-plane, automation, and extension architecture. |
| [development.md](development.md) | Reproducible toolchain, task graph, tests, and repository structure. |
| [e2e_coverage.md](e2e_coverage.md) | Stubbed end-to-end coverage map. |
| [live-e2e-coverage.md](live-e2e-coverage.md) | Credentialed/live E2E coverage and compile guards. |
| [e2e-coverage-matrix.md](e2e-coverage-matrix.md) | The single machine-parsed feature×coverage inventory — one row per operator-observable promise, each citing the test that proves it. |
| [brand.md](brand.md) | The mascot asset pipeline and which forge identity gets the avatar how. |

## Point-in-time records

These collections are valuable evidence, but they do not override current code or the living references above:

- [adr/](adr/) — immutable architecture decision records; later ADRs may supersede earlier ones.
- [bot-runs/](bot-runs/) — dated dogfood bilans and lessons for each bot.
- [c082-board-emit-fix-plan.md](c082-board-emit-fix-plan.md) — retained implementation plan.
- [changelog/](changelog/) — per-major changelog archives (`v0.md`, `v1.md`, `v2.md`); the current major stays in the repo-root `CHANGELOG.md`, rebuilt with `task changelog:gen`.
- [cli-permission-seam-spike.md](cli-permission-seam-spike.md) — completed spike preserving the `grok`/`kimi` permission-seam wire evidence.
- [reviews/](reviews/) — dated codebase reviews.
- [security/](security/) — dated security audits.
- [studio-ux-audit-2026-07.md](studio-ux-audit-2026-07.md) — UX audit snapshot.
