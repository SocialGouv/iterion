# Iterion observability stack

> **STATUS — dashboard is a schema contract, not a working live view.**
> The Grafana panels reference metrics (`iterion_node_cost_usd_total`,
> `iterion_node_tokens_total`, `iterion_llm_retry_total`,
> `iterion_llm_request_total`, `iterion_tool_call_total`,
> `iterion_node_duration_ms`, `iterion_parallel_branches`) that **no
> code in this repo currently emits**. The OTLP exporter translation
> layer that converts `TelemetryEvent` records into these metrics is
> deferred — see `docs/roadmap_progress.md` track on observability.
>
> Until that translation lands, `docker compose up -d` will produce an
> empty dashboard. The contents below describe the target shape, which
> is also the source-of-truth for the metric names the emitter needs to
> implement.

A self-contained docker-compose stack that gives you Grafana dashboards
for cost, tokens, retries, and node duration without any external
SaaS dependency.

## What's included

| Component | Purpose | Port |
|---|---|---|
| `otel-collector` | Receives OTLP traces / metrics / logs from iterion, fans them out | 4318 (HTTP), 4317 (gRPC) |
| `tempo` | Trace storage + query | 3200 |
| `prometheus` | Metric storage + query | 9090 |
| `grafana` | Dashboard UI | 3000 |

The Grafana dashboard in `grafana/iterion-workflow.json` is auto-loaded
from the running container; pre-provisioned datasources point at
Prometheus and Tempo.

## Two-command setup

```bash
cd docs/observability
docker compose up -d
```

Then open <http://localhost:3000>. The dashboard "Iterion Workflow"
appears under General. Login with `admin` / `admin` (or browse
anonymously — anonymous Viewer access is enabled).

To export iterion telemetry, point the claw-code-go OTLP exporter at
the local collector:

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
iterion run examples/your_workflow.iter
```

Tear down:

```bash
docker compose down -v
```

## Panels

| Panel | Metric | What it tells you |
|---|---|---|
| Cost per node | `iterion_node_cost_usd_total{node_id}` | Where the money goes per workflow node |
| Tokens per model | `iterion_node_tokens_total{model}` | Which provider/model dominates token spend |
| Retry rate | `iterion_llm_retry_total / iterion_llm_request_total` | How often LLM calls retry (rate limits, transients) |
| Node duration | `iterion_node_duration_ms_bucket` | p50 / p95 / p99 latency by node |
| Parallel branches | `iterion_parallel_branches` | Concurrency over time |
| Top-10 cost runs | `iterion_node_cost_usd_total{run_id}` | Most expensive runs |
| Tool calls | `iterion_tool_call_total{tool}` | Tool usage frequency |

## Required telemetry fields

The dashboard expects the OTLP exporter to set these attributes /
metrics on each event:

- `node_id` (string) — workflow node ID
- `model` (string) — full model spec (e.g. `anthropic/claude-sonnet-4-6`)
- `run_id` (string) — iterion run identifier
- `tool` (string) — tool name on `tool_call` events
- Counter metrics: `iterion_node_cost_usd_total`, `iterion_node_tokens_total`,
  `iterion_llm_retry_total`, `iterion_llm_request_total`, `iterion_tool_call_total`
- Histogram metric: `iterion_node_duration_ms`
- Gauge metric: `iterion_parallel_branches`

The claw-code-go telemetry exporter ships `TelemetryEvent` records over
OTLP; the iterion side is responsible for translating those into the
named metrics above. Until that translation lands, this dashboard is
the schema contract — emit metrics with these names and the dashboard
will render.
