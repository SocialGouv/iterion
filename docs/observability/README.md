# Iterion observability stack

A self-contained docker-compose stack that gives you Grafana dashboards
for cost, tokens, retries, and node duration without any external
SaaS dependency.

iterion exposes a Prometheus `/metrics` endpoint directly (no OTLP hop
required) when started with `ITERION_PROMETHEUS_ADDR=:9464`. The
docker-compose stack scrapes this endpoint and renders the dashboards
listed below.

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

To make the dashboard render data, run iterion with the Prometheus
endpoint enabled. Prometheus is preconfigured to scrape
`host.docker.internal:9464` every 5 s (see
`configs/prometheus.yaml`).

```bash
ITERION_PROMETHEUS_ADDR=:9464 iterion run examples/your_workflow.iter
# In another shell, sanity-check the metrics:
curl -s localhost:9464/metrics | grep iterion_
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

## Backend coverage

iterion attributes metrics from each backend on a best-effort basis:

| Metric                          | claw | claude_code | codex |
|---------------------------------|:----:|:-----------:|:-----:|
| `iterion_llm_request_total`     | ✅    | ✅           | ✅     |
| `iterion_llm_retry_total`       | ✅    | ✅           | ✅     |
| `iterion_node_duration_ms`      | ✅    | ✅           | ✅     |
| `iterion_tool_call_total`       | ✅    | ✅           | ✅     |
| `iterion_node_tokens_total`     | ✅    | ✅\*\*       | ✅\*\* |
| `iterion_node_cost_usd_total`   | ✅\*  | ✅\*         | ✅\*   |
| `iterion_parallel_branches`     | ✅    | ✅           | ✅     |

\* Cost is computed from a small per-model pricing table embedded in
`cost/cost.go`. Models not in the table emit no `_cost_usd` field.
Add models there if you want them tracked.

\*\* Token counts come from the SDK's `ResultMessage.Usage` for
claude_code (Claude Agent SDK) and codex (Codex Agent SDK). Both
backends now annotate the node output with `_tokens` / `_model` /
`_cost_usd` exactly like the in-process claw backend, so all three
backends feed the same Prometheus counters.

If a particular SDK version omits the usage block (e.g. early codex
betas), the tokens counter simply does not increment for that node — no
zero-fill is emitted, which keeps the dashboard's "no data" state
distinguishable from a real zero.
