[← Documentation index](README.md) · [← Iterion](../README.md)

# Iterion Cloud — the multi-tenant control plane for AI agents

**Audience.** Anyone evaluating iterion as a multi-tenant platform — a
platform engineer about to `helm install`, a tech-lead choosing between
self-hosting and managed, or an operator who needs to explain to a
review board what iterion *is* before they let it near production.

Iterion is two things in one repository:

1. **An open-source workflow engine** you run locally (`iterion run`,
   `iterion studio`, the desktop app) — the same engine the rest of the
   docs cover.
2. **A self-hostable multi-tenant platform** built on top of that engine:
   orgs, teams, BYOK LLM keys, generic + bot-secret bindings, inbound
   webhooks for four providers, NATS-queued runner pods, per-org quotas,
   audit log, PATs, SMTP onboarding. We call this deployment mode
   **Iterion Cloud**: the control plane and execution platform for running
   agent workflows across teams.

> **The control plane for AI agents.** Apps have Linux. The cloud has
> Kubernetes. AI agents have Iterion.

> **Terminology.** An *agent workflow* is the deployable workload packaged as
> `.bot` or `.botz` and identified as a `bot` by the existing CLI and API. An
> *agent* is one LLM-powered node inside that workflow.

## The governed execution loop

A `.bot` is a deployable agent workflow: a versioned graph that can combine
agents, judges, tools, routers, human gates, and deterministic compute. In
cloud mode the loop is end-to-end:

```mermaid
sequenceDiagram
  participant SRC as external event<br/>(forge/CI/cron)
  participant RUN as runner pod<br/>(iterion)
  participant EXT as external system<br/>(forge/Slack/...)

  SRC->>RUN: POST /api/webhooks (iwh_ token)
  Note over RUN: admit (auth/rate/quota)<br/>publish to NATS queue<br/>runner claims + executes<br/>binds org credentials<br/>(BYOK key + file secret)
  RUN->>EXT: agent workflow acts (commit,<br/>review, post note...)
  RUN-->>SRC: 202 launched + run id
```

The event is the **trigger**; the agent workflow is the **governed workload**;
the org-bound credentials let it **act in the user's own system** (GitLab MR
comment, GitHub PR review, Mattermost note); the result lands back in
that system without anyone clicking a UI in between.

## A concrete walkthrough — GitLab MR → Revi review

The operator's seed (a one-off):

1. Helm-install the chart against Mongo + NATS + S3 (see
   [cloud-deployment.md](cloud-deployment.md)).
2. Bootstrap the super-admin and create an org for the team
   (see the [Iterion Cloud admin guide](cloud-admin-guide.md)).
3. The org admin opens `/teams/<id>` in the studio → **Integrations** →
   "Connect a forge" → GitLab → paste a project access token (or use OAuth
   when an OAuth app is configured) → **Enable a repo** → pick the project →
   check `review-pr` → **Enable**. In one action iterion creates the GitLab
   project hook (pointing at its own inbound URL, with a fresh `iwh_` secret
   it holds on both ends), the matching webhook config, and the
   `forge_token` binding so Revi posts the review under the org's own GitLab
   account. The operator never sees or pastes a token URL. See
   [forge-integrations.md](forge-integrations.md).

   *(The raw `Webhooks` / `Secrets` / `Bindings` tabs remain for advanced /
   non-forge cases — e.g. a generic JSON trigger — and for hand-wiring a
   webhook the operator wants to manage themselves. Configs created by
   Integrations are marked managed and can only be torn down from the
   Integrations tab.)*

After that, every MR-open in the GitLab project triggers Revi
autonomously:

| Step | Where the code lives | What happens |
|---|---|---|
| GitLab POSTs the merge_request event | the forge | Header `X-Gitlab-Token: iwh_…` |
| Token check + rate/quota gates | [pkg/server/middleware_webhook.go](../pkg/server/middleware_webhook.go) | 401/410/429/403 if anything fails |
| Parse + filter (open/reopen on allowed project) | [pkg/webhooks/gitlab/parser.go](../pkg/webhooks/gitlab/parser.go) | 200 `filtered` for noise (updates, label edits) |
| Launch admission (org suspend / concurrency / cost cap / run quota) | [pkg/server/launch_gate.go](../pkg/server/launch_gate.go) | 402/403/429 with a stable reason token |
| Bundle the org's BYOK keys + bound secrets, seal them | [pkg/secrets/run_secrets.go](../pkg/secrets/run_secrets.go) | Per-run AES-GCM, run-id-bound AAD |
| Publish on NATS `iterion.queue.runs` | [pkg/queue/nats/nats.go](../pkg/queue/nats/nats.go) | KEDA scales the runner pool on lag |
| A runner pod claims, unseals, runs the `review-pr` workflow | [pkg/runner/loop.go](../pkg/runner/loop.go) | One in-flight run per pod (MaxAckPending=1) |
| The workflow clones, reviews, posts the note via `forge_token` | the workflow | The org's identity, not iterion's |
| Audit row + delivery row + Prometheus counters | [pkg/audit/audit.go](../pkg/audit/audit.go) + [pkg/cloud/metrics/metrics.go](../pkg/cloud/metrics/metrics.go) | Both tenant and platform timelines |

The same chain handles an on-demand re-review: a reviewer types `/revi`
in the MR's notes, GitLab POSTs a Note Hook to the same URL, iterion's
GitLab handler recognises the command word
([pkg/webhooks/gitlab/note.go:IsReviewCommand](../pkg/webhooks/gitlab/note.go)),
and a fresh review fires under a different idempotency key.

## The primitives

| Primitive | Owns | Where |
|---|---|---|
| **Webhook token** (`iwh_…`) | inbound auth + tenant resolution + per-org rate/quota | [docs/webhooks.md](webhooks.md) · [pkg/webhooks/](../pkg/webhooks/) |
| **Workflow catalog** (`bot_id`) | what a webhook can launch — agent workflows discovered from `ITERION_BOTS_PATH` | [docs/bundles.md](bundles.md) |
| **BYOK API keys + bindings** | the org's LLM and forge credentials, sealed at rest | [docs/secrets-reference.md](secrets-reference.md) · [pkg/secrets/](../pkg/secrets/) |
| **Runner pool** | claims a queued run, unseals the bundle, runs the workflow | [docs/cloud-architecture.md](cloud-architecture.md) · [pkg/runner/](../pkg/runner/) |
| **Orgs + quotas + audit** | the multitenancy and metering layer | [docs/quotas-and-limits.md](quotas-and-limits.md) · [pkg/orgusage/](../pkg/orgusage/) · [pkg/audit/](../pkg/audit/) |
| **PATs** (`iap_…`) | long-lived, programmatic API access for CI/SDKs | [Iterion Cloud admin guide](cloud-admin-guide.md) · [pkg/pat/](../pkg/pat/) |

Everything is **opt-in**. A self-hosted iterion that hasn't enabled
webhooks is still a perfectly fine multi-tenant studio (`iterion server`
in cloud mode); a `helm install` that turns on webhooks but no
PrometheusRule still runs — the chart's defaults are non-destructive
([charts/iterion/README.md](../charts/iterion/README.md)).

## Reading map

If you arrived here looking for…

| You want… | Read |
|---|---|
| The conceptual loop (you're here) | this page |
| The webhook reference (auth modes, providers, idempotency, CRUD API) | [webhooks.md](webhooks.md) |
| The quota / metering / enforcement contract (denial tokens, HTTP semantics) | [quotas-and-limits.md](quotas-and-limits.md) |
| The platform-operator + org-admin runbook (UI + curl side by side) | [Iterion Cloud admin guide](cloud-admin-guide.md) |
| Every kind of secret and where it lives | [secrets-reference.md](secrets-reference.md) (engine-side layers in [secrets.md](secrets.md)) |
| The full REST surface (every endpoint, auth class, purpose) | [cloud-rest-api.md](cloud-rest-api.md) |
| Memory + knowledge spaces (visibilities, quotas, REST) | [memory-and-knowledge.md](memory-and-knowledge.md) |
| Control plane vs data plane, queue internals, multitenancy enforcement | [cloud-architecture.md](cloud-architecture.md) |
| How to install the chart | [cloud-deployment.md](cloud-deployment.md) · [charts/iterion/README.md](../charts/iterion/README.md) |
| The OPERATOR-side bootstrap (super-admin, SSO, suspending an org) | [cloud-admin.md](cloud-admin.md) |
| The END-USER-side flows (login, BYOK, OAuth-forfait, PATs, reset) | [cloud-user.md](cloud-user.md) |
| Outbound run-completion callbacks (the original "webhook" feature) | [outbound-callbacks.md](outbound-callbacks.md) |

The ready-to-run agent workflows iterion ships — Nexie, Featurly, Billy,
Revi, Seki and friends — live in [examples.md](examples.md) and
[bundles.md](bundles.md). Iterion orchestrates and dispatches them through the
same governed execution loop.
