[← docs index](README.md) · [← cloud-deployment.md](cloud-deployment.md) · [← cloud-troubleshooting.md](cloud-troubleshooting.md)

# Cloud — public exposure checklist

This page is a pre-flight checklist for **opening an iterion deployment to traffic outside your private network** (i.e., putting it behind a public Ingress, on the internet, or accessible to users you don't fully trust). Every item below is a hard prerequisite — skipping any of them is how incidents happen.

If you are running iterion only on a private network (VPN, internal cluster, single-tenant lab), this page is informational. The checklist is geared toward multi-tenant or internet-exposed deployments.

This is not a substitute for a full security review. It is the minimum bar.

---

## 1. Authentication is enforced on every public route

- [ ] **Auth enforcement on** in the server config: confirm `DisableAuth` is **not** set in the iterion server config (cloud mode requires auth by default; see `pkg/config/config.go`). There is no `AUTH_REQUIRED` env var — auth is gated by the `DisableAuth` config field, which must remain `false`/unset in any public deployment.
- [ ] **Every `/api/*` route** is gated by `requireAuth` (verified in `pkg/server/server.go`). The health endpoints (`/healthz`, `/readyz`) and auth bootstrap routes are the only intentional exceptions; both are read-only and reveal no tenant state.
- [ ] **JWT signing key** rotated from the chart default. `ITERION_JWT_SECRET` must be a base64-encoded signing key with at least 32 decoded random bytes (for example, `openssl rand -base64 48`), held in a Kubernetes Secret, not the values file.
- [ ] **SSO / OIDC** wired if you have ≥ 2 users. See [cloud-admin.md](cloud-admin.md) for OIDC bootstrap.
- [ ] **Super-admin account** created with a strong password and 2FA enabled where the IdP supports it.

How to verify: try `curl https://<host>/api/runs` with no token — must return 401. Try with an expired token — must return 401. Try with a valid token from tenant A reading tenant B's run — must return 404 or 403.

## 2. Multi-tenant isolation is real

- [ ] **All Mongo queries** include the active tenant filter. Sample by inspecting the access log: every query line should show a `tenantID=<id>` matching the JWT.
- [ ] **All blob keys** are namespaced under `runs/<run-id>/...` and the run-id is itself a tenant-scoped value. No cross-tenant prefix collision possible.
- [ ] **Audit trail requirement** is covered for your deployment: collect the server/runner structured logs plus Mongo auth-store records in your log aggregator, retain them for ≥ 90 days, and verify they cover login, run launch, API-key issuance, and team-membership changes. There is no dedicated persistent audit-log package in the current tree, so treat this as an operator control/backlog item rather than an in-repo subsystem.
- [ ] **Admin-only endpoints** (`/api/admin/*`) require both `requireAuth` AND a role check (`requireSuperAdmin`).

How to verify: log in as user A on tenant A, take their JWT, try to call `/api/runs/<a-run-id-from-tenant-B>` — must return 404 or 403, never 200.

## 3. Network isolation

- [ ] **NetworkPolicy enabled** on the iterion namespace. Use [examples/networkpolicy-egress.yaml](../charts/iterion/examples/networkpolicy-egress.yaml) as a starting point and trim to the minimum your workflows need.
- [ ] **Egress allowlist** denies the public internet by default; only the LLM provider endpoints, package registries (if sandbox.image isn't pre-baked), source forges (if workflows clone repos), and in-cluster data-plane services are reachable.
- [ ] **CNI plugin** is NetworkPolicy-aware. Calico, Cilium, Antrea, and Weave all enforce; flannel-only clusters silently *ignore* NetworkPolicy. Verify with `kubectl describe networkpolicy …` or by observing that `kubectl exec` into a runner pod cannot reach `1.1.1.1`.
- [ ] **Ingress controller** terminates TLS with a valid certificate (Let's Encrypt, internal CA, etc.). HTTP→HTTPS redirect enforced.
- [ ] **Per-run sandbox** uses a CONNECT proxy with its own host allowlist (default preset = LLM endpoints + npm/pypi/golang + github/gitlab/bitbucket + Nix cache). See [sandbox.md](sandbox.md). NetworkPolicy and CONNECT proxy are *layered*; do not rely on one alone.

How to verify: from a runner pod with sandbox active, attempt `curl https://example.com/` — must fail. Attempt `curl https://api.anthropic.com/` — must succeed.

## 4. Secrets management

- [ ] **No secrets in values files.** Every secret (Mongo URI, S3 keys, JWT secret, OAuth client secrets, BYOK API keys, OIDC client secret) is held in a Kubernetes Secret or external secret store (Vault, AWS Secrets Manager, etc.) and pulled via `valueFrom` / external-secrets.
- [ ] **Per-tenant BYOK** keys are encrypted at rest in Mongo (envelope encryption, key in a Secret outside Mongo). The plaintext API key is never logged. See [cloud-admin.md](cloud-admin.md).
- [ ] **Secret rotation** has a documented cadence. JWT signing key, S3 credentials, and OIDC client secret are rotated at least quarterly. The rotation runbook is captured in [cloud-deployment.md](cloud-deployment.md).
- [ ] **OAuth Claude Pro/Max forfait** is *not* used in third-party deployments — it violates Anthropic ToS. Use API keys, Bedrock, or Vertex instead.

How to verify: `kubectl describe pod <iterion-server-pod> | grep -i secret` — env vars must reference `valueFrom.secretKeyRef`, never literal `value:`.

## 5. Image supply chain

- [ ] **Trivy CI** runs on PRs, pushes to `main`, weekly schedule, and post-image builds; the workflow publishes SARIF/results summaries for HIGH/CRITICAL findings but intentionally does **not** fail the PR or image release by itself. If you require a hard gate, configure branch protection or a code-scanning rule outside the current workflow. See [.github/workflows/trivy.yml](../.github/workflows/trivy.yml).
- [ ] **Image references** in production values use `image: ghcr.io/socialgouv/iterion:<digest>@sha256:…`, not floating tags like `:latest` or `:v1`.
- [ ] **Sandbox image** is pre-built and digest-pinned. The Kubernetes driver rejects `sandbox.build:` by design — production deployments reference a CI-built `iterion-sandbox-slim:<version>` digest.
- [ ] **Cosign / sigstore** signatures verified at admission for production. (Optional but recommended; falls outside the chart's scope.)

How to verify: `helm template ./charts/iterion -f values-prod.yaml | grep image:` — every image string ends in `@sha256:…`.

## 6. Observability + alerting

- [ ] **Prometheus** scrapes `/metrics` on the server. The chart's `metrics.podMonitor.enabled` switch wires this when prometheus-operator is installed.
- [ ] **OTLP traces** exported to a collector if you have one. Set `OTEL_EXPORTER_OTLP_ENDPOINT` (or the traces-specific `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`) on server + runner.
- [ ] **Log format** is JSON in production. `ITERION_LOG_FORMAT=json` set on server + runner; verified by piping a server log line through `jq`.
- [ ] **Alert rules** exist for: `/readyz` 503 > 1m, NATS queue depth > 100 sustained, runner OOM kills, Trivy failed scan, JWT signing-key proximity to expiry.
- [ ] **Runbook** linked from each alert. The runbook is `cloud-troubleshooting.md` plus alert-specific notes.

How to verify: kill a runner pod; `/api/runs/<id>/events` should not stream new events (run hangs); the alert for queue-depth or stale-run should fire within its configured window.

## 7. Probes + autoscaling

- [ ] **livenessProbe** and **readinessProbe** wired on **server** AND **runner** deployments. Both shipped in the chart; verify they appear in `kubectl get deploy -o yaml | grep -i probe`.
- [ ] **KEDA ScaledObject** drives the runner pool from NATS queue depth. Consumer name is parameterised (`runner.keda.consumerName`); two parallel deployments do not collide on the same JetStream consumer.
- [ ] **PodDisruptionBudget** for the server (chart ships one). Cluster auto-upgrades won't take down all server replicas at once.

How to verify: `kubectl get scaledobject -n <ns> -o yaml | grep consumer` shows the parameterised name. Drain a runner node; KEDA recreates pods on a remaining node within ~30s.

## 8. Resource limits + cost guards

- [ ] **CPU / memory limits** on server + runner pods. Without limits, a runaway claude_code session can OOM the node.
- [ ] **Per-run budget caps** enforced via the workflow's `budget:` block AND a server-side default override (operator can refuse to launch a run with no budget).
- [ ] **Per-tenant rate limits** on `/api/runs/launch` and `/api/runs/resume` to prevent a single misconfigured client from saturating the queue.
- [ ] **Storage quotas** on the S3 bucket (lifecycle policy on objects > 90 days unless explicitly retained).

## 9. Backup + recovery

- [ ] **Mongo** is a replica set with point-in-time recovery enabled (Atlas, MongoDB Cloud Manager, or in-cluster operator with continuous backup).
- [ ] **S3 bucket** has versioning enabled and a cross-region replication policy if your RTO requires it.
- [ ] **NATS JetStream** state is *not* the source of truth — runs and events are durable in Mongo. NATS reseed from scratch is acceptable; document the procedure.
- [ ] **Disaster recovery drill** run at least once before public exposure. Restore a Mongo backup to a fresh cluster; verify a paused run resumes from the restored checkpoint.

## 10. Documentation + runbooks

- [ ] **On-call runbook** linked from your alert manager. Minimum contents: how to scale the runner pool manually, how to release a stale lock, how to rotate the JWT signing key, how to disable a misbehaving tenant.
- [ ] **Privacy / data residency** policy documented. What data does iterion store about a run? Where? For how long? How can a tenant request deletion?
- [ ] **Incident response** plan with named owner, paging escalation, and post-mortem template.
- [ ] **Public status page** (or equivalent) committed to. If `/readyz` 503s, downstream users need a way to know without paging the on-call manually.

---

## Sign-off

| Role | Name | Date | Notes |
|---|---|---|---|
| Operator (deploys + monitors) | | | |
| Security reviewer | | | |
| Privacy / legal reviewer | | | |
| On-call manager | | | |

A deployment is ready for public exposure when **all four sign-offs are present and dated**, and **every checkbox above is ticked**. Half-completed checklists are how incidents are made.

If a box can't be ticked, do not open the deployment to traffic until it can — or accept the risk explicitly, in writing, with a named owner.
