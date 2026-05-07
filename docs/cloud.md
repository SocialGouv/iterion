[← Documentation index](README.md) · [← Iterion](../README.md)

# Cloud Mode

A long-running server deployment that targets multi-tenant teams. Same Go core as the CLI, but exposes the editor + run engine through HTTP/WS to a shared instance, persists runs to a Mongo + S3-compatible blob store, and dispatches jobs to a runner pool via NATS JetStream.

## Architecture at a glance

| Component | Implementation | Role |
|---|---|---|
| **Server** | `iterion server` (`pkg/server/`) | HTTP/WS API + embedded editor SPA + dispatch of runs to the queue |
| **Runner pod** | `iterion runner` (`pkg/runner/`) | Consumes the NATS queue, executes workflows, can launch a per-run sandbox pod via Kubernetes |
| **Queue** | NATS JetStream (`pkg/queue/`) | At-least-once delivery, distributed lease coordination |
| **Run store** | MongoDB + S3-compatible blob (`pkg/store/`) | Replaces the local `.iterion/` filesystem store |
| **Config** | `pkg/config/` | Reads env vars + YAML for Mongo/NATS/S3/Sandbox/Runner sections |
| **Metrics** | `pkg/cloud/metrics/` | Prometheus registry exposed on `/metrics` |

```yaml
# values.yaml — minimal example (see charts/iterion/values.yaml for the full schema)
mongo:
  uri: "mongodb://mongo:27017/iterion"
blob:
  endpoint: "https://s3.example.com"
  bucket: "iterion-runs"
queue:
  nats: "nats://nats:4222"
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
- **Auth**: `ITERION_SESSION_TOKEN` and `ITERION_AUTH_TOKEN` env vars gate the API; health endpoints are auth-exempt

---

👉 **For deployment, secrets, NetworkPolicy egress, observability, resume and migration from a filesystem store, see the full operator runbook: [cloud-deployment.md](cloud-deployment.md).**
