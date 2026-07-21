# iterion Helm chart

Deploys Iterion Cloud, the multi-tenant **control plane for AI agents**.
It combines an HTTP/WS API with the embedded studio, a KEDA-scalable runner
pool consuming the NATS JetStream queue, and the Mongo + S3 run store. One
image, two deployments (`server` + `runner`).

- Platform overview: [Iterion Cloud overview](../../docs/cloud-overview.md)
- Operator runbook: [docs/cloud-deployment.md](../../docs/cloud-deployment.md) and [Iterion Cloud admin guide](../../docs/cloud-admin-guide.md)

## Requirements

- Kubernetes ≥ 1.28, Helm ≥ 3.8 (OCI)
- MongoDB 6+ **replica set** (change streams + transactions), NATS 2.10+
  with JetStream, an S3-compatible bucket — bundled sub-charts exist for
  all three (dev), external services recommended (prod)
- Optional: [KEDA](https://keda.sh) for queue-depth runner autoscaling,
  prometheus-operator for the PodMonitor / PrometheusRule, cert-manager
  or equivalent for ingress TLS

## Install

```bash
# Released chart (published to GHCR on every release)
helm upgrade --install iterion oci://ghcr.io/socialgouv/charts/iterion \
  --version <semver> -f my-values.yaml

# Dev stack — bundled Mongo/NATS/MinIO, single replica, fixed dev keys
helm upgrade --install iterion ./charts/iterion -f charts/iterion/values-dev.yaml

# Prod shape — external Mongo/NATS/S3, HPA, NetworkPolicy, PodMonitor
helm upgrade --install iterion ./charts/iterion -f charts/iterion/values-prod.yaml -f my-overrides.yaml
```

Post-install, `NOTES.txt` prints the studio URL, where to find the
bootstrap super-admin temp password, and loud warnings when the bundled
MongoDB runs without auth or email is disabled.

## Values — top-level map

The full schema is `values.yaml` (every key commented inline); the
groups:

| Group | What it controls |
|---|---|
| `mode` | `local` (single-pod studio + PVC) vs `cloud` (the platform) |
| `image` | Repository/tag/pullPolicy — one image for server + runner |
| `server` | Replicas, resources, command/args, metrics port |
| `runner` | Pool enable/replicas/resources, `keda.*` (JetStream lag scaler), `sandbox.enabled` (+RBAC for per-run pods) |
| `config` | Non-secret env: `mongo.*`, `nats.*`, `s3.*`, `runner.*`, `auth.*` (public half: publicUrl, cookies, TTLs, signupMode, OIDC client ids), `smtp.*` (host/port/from — enables invitations + password-reset email), `orgDefaults.*` (platform-wide launch limits → `ITERION_ORG_DEFAULT_*`), `log.*` |
| `secrets` | Four bundles, each `create:` (chart-rendered, dev) or `existingSecret:` (sealed-secrets / external-secrets, prod): `llm` (fallback provider keys), `storage` (S3 creds), `auth` (`ITERION_JWT_SECRET`, `ITERION_SECRETS_KEY`, bootstrap admin, OIDC client secrets, `completionWebhookSecret`), `smtp` (relay credentials, server-only) |
| `service` / `ingress` | Exposure; pair ingress TLS with your issuer |
| `probes` | `/healthz` liveness, `/readyz` readiness (Mongo/NATS/S3 pings) |
| `networkPolicy` | Opt-in deny-all egress + allowlist (see `examples/networkpolicy-egress.yaml`) |
| `metrics` | `podMonitor.*` scrape config + `prometheusRule.*` starter alert pack (DLQ depth, heartbeat errors, orphan recoveries, launch denials, webhook throttling) |
| `persistence` | local-mode PVC only |
| `mongodb` / `nats` / `minio` | Bundled sub-charts (dev). **The bundled Mongo defaults to `auth.enabled: false` — dev only**, flip it or use an external URI for anything shared |

## Upgrade notes

- **SMTP** (`config.smtp.*` + `secrets.smtp`) — new; absent = email
  disabled (flows degrade gracefully, the UI hides forgot-password).
- **`secrets.auth.completionWebhookSecret`** — new; signs outbound
  run-completion callbacks (`X-Iterion-Signature`). Empty = unsigned
  (previous behaviour).
- **`config.orgDefaults.*`** — new platform-wide launch limits; all
  default to 0 (unlimited), so upgrades are behaviour-neutral until set.
- **PodDisruptionBudgets** render automatically once `server.replicas>1`
  (or the runner is KEDA-scaled) — no value to set.
- **`metrics.prometheusRule.enabled`** — opt-in; requires the
  PrometheusRule CRD.

## Smoke test

```bash
helm test iterion          # runs the chart's test-connection pod
kubectl get pods -l app.kubernetes.io/instance=iterion
```

Then walk the first-webhook loop in
[Iterion Cloud admin guide](../../docs/cloud-admin-guide.md).
