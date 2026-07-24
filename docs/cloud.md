# Cloud Mode

Cloud mode runs iterion as a multi-tenant **control plane for AI agents** — the
same Go core as the CLI, but with orgs, quotas, bound credentials, inbound
webhooks, and an audit trail, serving a shared always-on run engine over
HTTP/WS. Runs persist to a Mongo + S3-compatible blob store, and jobs dispatch
to a runner pool via NATS JetStream. Reading map:

| You want | Read |
|---|---|
| The platform overview + governed execution loop | [cloud-overview.md](cloud-overview.md) |
| Control plane / data plane internals | [cloud-architecture.md](cloud-architecture.md) |
| Deploy it (Helm, secrets, probes) | [cloud-deployment.md](cloud-deployment.md) + [chart README](../charts/iterion/README.md) |
| Operate it (orgs, quotas, DLQ, SMTP) | [cloud-admin-guide.md](cloud-admin-guide.md) |
| Trigger agent workflows from a forge / any system | [webhooks.md](webhooks.md) |
| Limits, metering, denial semantics | [quotas-and-limits.md](quotas-and-limits.md) |
| Every secret kind in one map | [secrets-reference.md](secrets-reference.md) |
| The REST surface | [cloud-rest-api.md](cloud-rest-api.md) |

## Architecture at a glance

| Component | Implementation | Role |
|---|---|---|
| **Server** | `iterion server` (`pkg/server/`) | HTTP/WS API + embedded studio + dispatch of runs to the queue |
| **Runner pod** | `iterion runner` (`pkg/runner/`) | Consumes the NATS queue, executes workflows, can launch a per-run sandbox pod via Kubernetes |
| **Queue** | NATS JetStream (`pkg/queue/`) | At-least-once delivery, distributed lease coordination |
| **Run store** | MongoDB + S3-compatible blob (`pkg/store/`) | Replaces the local `.iterion/` filesystem store |
| **Config** | `pkg/config/` | Reads env vars + YAML for Mongo/NATS/S3/Sandbox/Runner sections |
| **Metrics** | `pkg/cloud/metrics/` | Prometheus registry exposed on `/metrics` |

```yaml
# values.yaml — minimal example (see charts/iterion/values.yaml for the full schema)
config:
  mongo:
    uri: "mongodb://mongo:27017/iterion"
    database: "iterion"
  nats:
    url: "nats://nats:4222"
  s3:
    endpoint: "https://s3.example.com"
    bucket: "iterion-runs"
    region: "us-east-1"
    usePathStyle: true
```

## Deploy

- **Helm (OCI registry)**:

  ```bash
  helm upgrade --install iterion \
    oci://ghcr.io/socialgouv/charts/iterion \
    --version <semver> \
    -f values.yaml
  ```

  The chart is published to GHCR on every release (job `publish-chart` in `.github/workflows/release.yml`); pick a `--version` from the [iterion releases](https://github.com/SocialGouv/iterion/releases). It bundles server + runner Deployments, KEDA-based runner autoscaling on queue depth, and optional sandbox RBAC for per-run pods. To install from a local checkout instead (chart hacking, unreleased fixes), use `helm upgrade --install iterion ./charts/iterion -f values.yaml`.
- **Local stack** for testing cloud mode end-to-end: `docker compose -f docker-compose.cloud.yml up` brings up Mongo + NATS + MinIO + iterion server + runner — see [`docker/`](../docker/) for init scripts
- **Container image**: `ghcr.io/socialgouv/iterion:latest` (built by `.github/workflows/image.yml` on every main push and tag; scanned by `.github/workflows/trivy.yml` post-build and weekly — non-blocking, findings land in the repo Security tab)
- **Health probes**: `GET /healthz` (liveness, always 200) and `GET /readyz` (200 when Mongo/NATS/S3 are reachable in cloud mode)
- **Auth**: JWT/cookie based — `ITERION_JWT_SECRET` signs access tokens issued by login/refresh, `ITERION_SECRETS_KEY` seals credentials, API clients present the access JWT as `Authorization: Bearer`, the `iterion_auth` cookie, or WS `?t=`, and health/auth bootstrap endpoints are auth-exempt

---

👉 **For deployment, secrets, NetworkPolicy egress, observability, resume and migration from a filesystem store, see the full operator runbook: [cloud-deployment.md](cloud-deployment.md).**
