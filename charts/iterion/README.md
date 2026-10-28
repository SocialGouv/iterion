# iterion Helm chart

**Build, run and orchestrate agentic AI workflows.** This chart deploys Iterion
Cloud — the multi-tenant control plane — on your own cluster.
It combines an HTTP/WS API with the embedded studio, a KEDA-scalable runner
pool consuming the NATS JetStream queue, and the Mongo + S3 run store. One
image, two deployments (`server` + `runner`).

- Platform overview: [Iterion Cloud overview](../../docs/cloud-overview.md)
- Operator runbook: [docs/cloud-deployment.md](../../docs/cloud-deployment.md) and [Iterion Cloud admin guide](../../docs/cloud-admin-guide.md)

## Requirements

- Kubernetes ≥ 1.28, Helm ≥ 3.8 (OCI)
- MongoDB 6+ **replica set** (change streams + transactions), NATS 2.10+
  with JetStream, an S3-compatible bucket — the dev values bring up all
  three in-cluster (the nats sub-chart and the chart's own single-replica
  dev stores), external services recommended (prod)
- Optional: [KEDA](https://keda.sh) for queue-depth runner autoscaling,
  prometheus-operator for the PodMonitor / PrometheusRule, cert-manager
  or equivalent for ingress TLS

## Install

```bash
# Released chart (published to GHCR on every release)
helm upgrade --install iterion oci://ghcr.io/socialgouv/charts/iterion \
  --version <semver> -f my-values.yaml

# Dev stack — in-cluster Mongo/NATS/SeaweedFS, single replica, fixed dev keys
helm upgrade --install iterion ./charts/iterion -f charts/iterion/values-dev.yaml

# Prod shape — external Mongo/NATS/S3, HPA, NetworkPolicy, PodMonitor
helm upgrade --install iterion ./charts/iterion -f charts/iterion/values-prod.yaml -f my-overrides.yaml
```

Post-install, `NOTES.txt` prints the studio URL, where to find the
bootstrap super-admin temp password, and loud warnings when the dev stores
run or email is disabled.

## Values — top-level map

The full schema is `values.yaml` (every key commented inline); the
groups:

| Group | What it controls |
|---|---|
| `mode` | `local` (single-pod studio + PVC) vs `cloud` (the platform) |
| `image` | Repository/tag/**digest**/pullPolicy — one image for server + runner. `digest` wins over `tag`; prefer it over a moving tag (`edge`/`main`), which is resolved per pod and can leave one ReplicaSet running two builds — see [cloud-deployment.md](../../docs/cloud-deployment.md#pinning-the-image) |
| `server` | Replicas, full-surge `strategy`, resources, command/args, metrics port, `scheduling.*` (hostname anti-affinity + spread of its own replicas, soft by default) and `pdb.*` (the disruption budget, rendered only above one guaranteed replica) |
| `runner` | Pool enable/replicas/resources, full-surge `strategy`, `keda.*` (JetStream lag scaler), `sandbox.enabled` (+RBAC for per-run pods), the same `scheduling.*` / `pdb.*` knobs (a `runnerPools[]` entry overrides them key by key) |
| `config` | Non-secret env: `rollout.*` (monotonic runner epoch), `mongo.*`, `nats.*`, `s3.*`, `runner.*`, `auth.*` (public half: publicUrl, cookies, TTLs, signupMode, OIDC client ids), `smtp.*` (host/port/from — enables invitations + password-reset email), `orgDefaults.*` (platform-wide launch limits → `ITERION_ORG_DEFAULT_*`), `log.*` |
| `secrets` | Four bundles, each `create:` (chart-rendered, dev) or `existingSecret:` (sealed-secrets / external-secrets, prod): `llm` (fallback provider keys), `storage` (S3 creds), `auth` (`ITERION_JWT_SECRET`, `ITERION_SECRETS_KEY`, bootstrap admin, OIDC client secrets, `completionWebhookSecret`, `webpush` VAPID keypair), `smtp` (relay credentials, server-only) |
| `service` / `ingress` | Exposure; pair ingress TLS with your issuer |
| `probes` | `/healthz` liveness, `/readyz` readiness (Mongo/NATS/S3 pings) |
| `networkPolicy` | Opt-in deny-all egress + allowlist (see `examples/networkpolicy-egress.yaml`) |
| `metrics` | `podMonitor.*` scrape config + `prometheusRule.*` alert packs: the control-plane starter (DLQ depth, heartbeat errors, orphan recoveries, launch denials, webhook throttling), `prometheusRule.datastores.*` (a replica missing or restarting per store, valkey replication and AOF, SeaweedFS volumes and leader, MongoDB members — and an absent-signal rule per exporter family, so a signal nobody scrapes pages) and the backup rules when `backup.enabled` |
| `backup` | The off-cluster copy of the artifact bucket: one rclone CronJob per tier (daily `sync` keeping deletions, weekly and monthly `snapshot`s), image pinned by digest, destination credentials by `existingSecret` only, every run checked — [docs/cloud-backup.md](../../docs/cloud-backup.md#blob-backup) |
| `persistence` | local-mode PVC only |
| `nats` | The one sub-chart (the official NATS chart), dev only |
| `devStack` | The chart's own single-replica dev stores at the digests of docker-compose.cloud.yml — `mongodb` (a one-member replica set, **no auth: dev only**) and `seaweedfs` (the production object store in one process, a fixed identity, the bucket created by a pinned rclone hook); `values-dev.yaml` turns them on and `config.mongo.uri` / `config.s3.endpoint` resolve to them when empty |

## Upgrade notes

- **Generation-aware rollout** — first ship wire v12 with
  `config.rollout.runnerEpoch: 0`, then raise it to `1` in a separate release.
  Increment it for runner-code rollouts. Never `helm rollback` to a lower
  epoch: re-release the older image with a higher epoch. The value and its
  mismatch delay are literal PodTemplate envs so an old ReplicaSet scaled by
  HPA/KEDA cannot inherit the new generation through the shared ConfigMap.

- **SMTP** (`config.smtp.*` + `secrets.smtp`) — new; absent = email
  disabled (flows degrade gracefully, the UI hides forgot-password).
- **`secrets.auth.completionWebhookSecret`** — new; signs outbound
  run-completion callbacks (`X-Iterion-Signature`). Empty = unsigned
  (previous behaviour).
- **`config.orgDefaults.*`** — new platform-wide launch limits; all
  default to 0 (unlimited), so upgrades are behaviour-neutral until set.
- **Replicas are spread by default** — every server, runner and pool pod
  template now carries a soft hostname anti-affinity and a soft
  `topologySpreadConstraints`, both selecting the workload's OWN labels
  (`scheduling.*`); an upgrade rolls the pods once. Keep the previous spec
  with `scheduling.antiAffinity: none` + `scheduling.spread.enabled: false`.
- **PodDisruptionBudgets** render once a workload is *guaranteed* more than
  one replica (`replicas > 1`, or the HPA / KEDA floor above one) and never
  on a single replica, where `minAvailable: 1` refused every eviction and
  held node drains: a KEDA pool with `minReplicas: 1` loses the budget it
  used to get. `pdb.enabled: true|false` forces either way.
- **`metrics.prometheusRule.enabled`** — opt-in; requires the
  PrometheusRule CRD.
- **BREAKING, dev values only** — the Bitnami `mongodb` and `minio`
  sub-charts are gone with their `mongodb.*` / `minio.*` values (frozen
  `bitnamilegacy/*` images, no security updates): the dev stack is now the
  chart's own `devStack.*` — Mongo 8.3 and SeaweedFS 4.48, both pinned by
  digest, the bucket created by a pinned rclone hook. A values file still
  setting `mongodb.*` or `minio.*` is refused by the schema; point
  `config.mongo.uri` / `config.s3.endpoint` at your stores, or leave them
  empty with `devStack.*.enabled`. Production never enabled either sub-chart.
- **`backup.*`** — new, off by default; the render refuses an enabled
  backup without its destination Secret or with an unpinned image.
- **`metrics.prometheusRule.datastores.*`** — new, off by default; it names
  the stores of YOUR cluster (StatefulSet regexes, exporter selectors).

## Checks

```bash
task chart:check           # helm lint (dev, prod --strict) + the render tests
helm template iterion charts/iterion -f my-values.yaml | task chart:gate
```

`task chart:check` is part of `task check`. Its render tests confront every
anti-affinity, spread and PodDisruptionBudget selector of the dev, prod and
pool renders with the labels of its own pods and refuse a selector that
names none (`internal/rendergate`); `task chart:gate` judges any rendered
stream the same way — a GitOps repo's `helm template` included.

## Smoke test

```bash
helm test iterion          # runs the chart's test-connection pod
kubectl get pods -l app.kubernetes.io/instance=iterion
```

Then walk the first-webhook loop in
[Iterion Cloud admin guide](../../docs/cloud-admin-guide.md).
