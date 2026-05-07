# Cloud deployment

This document is the operator runbook for the cloud-mode topology
(`iterion server` + `iterion runner` + Mongo + NATS JetStream + S3).
It covers prerequisites, secret + token lifecycle, NetworkPolicy
egress, observability, resume, and migration from a filesystem store.

The Helm chart at [helm/iterion/](../helm/iterion/) renders the full
stack; values-dev.yaml bundles in-cluster Mongo / NATS / MinIO for
smoke tests, values-prod.yaml expects external dependencies.

## Topology

```
+----------+       POST /api/runs          +----------+
| client   | ----------------------------> | server   |
+----------+                               | (cloud)  |
                                           +----+-----+
                                                | publish (NATS JetStream)
                                                v
+----------+   change-stream events       +----------+
|  WS      | <--------------------------- |  Mongo   |
|  client  |                              |          |
+----------+                              +----+-----+
                                                ^ AppendEvent
                                                |
                                           +----+-----+
                                           | runner   |
                                           |  (pool)  |
                                           +----+-----+
                                                | NATS-KV lease + S3 artifacts
```

- **server** publishes RunMessages onto JetStream and serves the
  editor SPA + run console (REST + WebSocket).
- **runner** pulls RunMessages, claims a NATS-KV lease, executes the
  workflow, and writes events + artifacts to Mongo + S3.

## Prerequisites

| Component | Requirement |
|---|---|
| Kubernetes | 1.28+ for `context.WithoutCancel` semantics + native `Probe.gRPC` (optional) |
| CNI | NetworkPolicy enforcement enabled (Calico, Cilium, Antrea) when `networkPolicy.enabled=true` |
| MongoDB | 6.0+ with **replica set** (change-streams require an oplog) |
| NATS | 2.10+ with JetStream enabled |
| S3-compatible | bucket pre-created with `s3:ListBucket`, `s3:GetObject`, `s3:PutObject`, `s3:DeleteObject` for the IAM principal |
| KEDA (optional) | 2.13+ if `runner.keda.enabled=true` |
| Prometheus Operator (optional) | for `metrics.podMonitor.enabled=true` |

## Session token

Every cloud server requires `ITERION_SESSION_TOKEN` to gate `/api/*`.
Without it, boot aborts with an explicit error (override only via
`ITERION_DISABLE_AUTH=true` for local smoke tests).

Generate + apply:

```bash
kubectl create secret generic iterion-session \
  --from-literal=ITERION_SESSION_TOKEN=$(openssl rand -hex 32) \
  --namespace iterion
```

Reference from values-prod.yaml:

```yaml
secrets:
  session:
    existingSecret: iterion-session
```

Rotation = create a new secret + `helm upgrade`. Active WS clients are
disconnected on the next rolling-restart of the server pods.

## NetworkPolicy egress

`values-prod.yaml` ships with `networkPolicy.enabled=true` + an empty
`extraAllow` so the cluster default-denies egress except DNS. Add
explicit rules for Mongo, NATS, S3, and the LLM provider:

```yaml
networkPolicy:
  enabled: true
  extraAllow:
    # In-cluster Mongo (same namespace)
    - to:
        - podSelector:
            matchLabels:
              app.kubernetes.io/name: mongodb
      ports:
        - protocol: TCP
          port: 27017
    # External LLM provider (Anthropic)
    - to:
        - ipBlock:
            cidr: 0.0.0.0/0
      ports:
        - protocol: TCP
          port: 443
```

The chart synthesises a single egress block from the union of
defaults + `extraAllow`. There is no auto-detection of bundled
sub-charts; if you also bundle Mongo via `mongodb.enabled`, add the
matching `extraAllow` entry.

## NATS monitoring endpoint (KEDA)

KEDA's NATS JetStream scaler scrapes `/jsz` on the **monitoring**
port (8222 by default), not the client URL. The chart helper
`iterion.nats.monitoringEndpoint` resolves to:

1. `.Values.config.nats.monitoringEndpoint` if set, else
2. `<release>-nats:8222` for bundled NATS, else fails.

For external NATS:

```yaml
config:
  nats:
    url: nats://nats.shared:4222          # JetStream client port
    monitoringEndpoint: nats.shared:8222  # /jsz scrape
```

## Metrics & dashboards

The server + runner expose `/metrics` on `:9090` (configurable via
`config.metrics.port`). Counters/gauges are documented at
[pkg/cloud/metrics/metrics.go](../pkg/cloud/metrics/metrics.go) and
populated at runtime:

| Metric | Pod | Meaning |
|---|---|---|
| `iterion_runs_created_total{status}` | server | Every Launch/Resume publish |
| `iterion_runs_active{status="running"}` | runner | Sum across pods = in-flight runs |
| `iterion_run_duration_seconds{status}` | runner | Histogram, terminal status |
| `iterion_ws_connections` | server | Live run-console subscribers |
| `iterion_mongo_change_stream_lag_seconds` | server | Set on each delivered event |
| `iterion_nats_pending_messages` | runner | Polled every 15s from JetStream consumer |
| `iterion_llm_tokens_total{backend,model,direction}` | runner | input/output/cache_read/cache_write |
| `iterion_llm_cost_usd_total{backend,model}` | runner | Reserved (not yet emitted by hooks) |
| `iterion_runner_heartbeat_errors_total` | runner | Each KV lease refresh failure |

Wire a Prometheus PodMonitor:

```yaml
metrics:
  podMonitor:
    enabled: true
    interval: 30s
```

`/metrics` is **ClusterIP-only** by design — no ingress should expose
it publicly.

## Tracing

The server + runner emit OpenTelemetry spans:

- `iterion.api.launch_run`, `iterion.api.resume_run` (server)
- `iterion.runner.process_one` (runner, root span per run)
- `iterion.node.execute` (engine, child span per node)

Trace context propagates through the W3C `traceparent` header on the
NATS RunMessage so a single trace covers `client → server → queue →
runner → node graph`.

Configure the OTLP exporter via standard env vars:

```yaml
config:
  env:
    OTEL_EXPORTER_OTLP_ENDPOINT: "http://tempo.observability:4318"
    OTEL_SERVICE_NAMESPACE: "iterion"
    OTEL_RESOURCE_ATTRIBUTES: "deployment.environment=prod"
```

When `OTEL_EXPORTER_OTLP_ENDPOINT` is unset, spans are dropped and the
W3C propagator-only path is installed (inbound trace context still
respected, but no export).

## Resume from a paused / failed run

Cloud-mode resume goes through the same NATS path as launch. The
client passes the inline `source` of the workflow because the server
pod has no operator filesystem:

```bash
curl -X POST https://iterion.example.com/api/runs/$RUN_ID/resume \
  -H "Authorization: Bearer $ITERION_SESSION_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "source": "'"$(jq -Rs . workflow.iter)"'",
    "answers": {"approved": true},
    "force": false
  }'
```

`force=true` bypasses the workflow-hash mismatch guard (useful after a
local fix). The runner reads the flag from the RunMessage and applies
it to `runtime.New(WithForceResume)`.

## Migration from filesystem store

`iterion migrate to-cloud` uploads runs from a local `.iterion/`
directory into Mongo + S3. Idempotent (Mongo upserts + S3 PUT
overwrites):

```bash
ITERION_MONGO_URI=mongodb://...?replicaSet=rs0 \
ITERION_MONGO_DB=iterion \
ITERION_S3_ENDPOINT=https://s3.amazonaws.com \
ITERION_S3_BUCKET=iterion-prod \
ITERION_S3_REGION=eu-west-3 \
  iterion migrate to-cloud --store-dir ./.iterion --concurrency 4
```

Re-run safely if interrupted; runs already in Mongo are no-ops.

## Smoke test (`task chart:kind`)

```bash
devbox run -- task chart:kind
```

Renders + lints the chart, checks `appVersion` matches `package.json`.
For a real install + workflow exec, see the `cloud-e2e` CI job in
[.github/workflows/tests.yml](../.github/workflows/tests.yml).

## Health endpoints

| Path | Behaviour |
|---|---|
| `/healthz` | 200 if the HTTP listener is up — covers liveness probe |
| `/readyz` | Pings Mongo + NATS + S3 with 1s sub-deadline each, 503 on any failure — covers readiness probe |

The `/readyz` JSON response details which dependency is failing so the
operator can debug from `kubectl describe pod`.
