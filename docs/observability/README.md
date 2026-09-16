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
ITERION_PROMETHEUS_ADDR=:9464 iterion run bots/whats-next/main.bot
# In another shell, sanity-check the metrics:
curl -s localhost:9464/metrics | grep iterion_
```

Binding is **fail-soft** by default: if the address is taken or malformed the
run logs one `prometheus: serve …` error line and carries on with no
`/metrics` endpoint. Set `ITERION_PROMETHEUS_REQUIRED=1` (`true` / `yes` /
`on` also count) to bind the address up front instead, so a misconfiguration
fails the command rather than leaving the dashboard silently empty.

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

Every panel above reads a **Prometheus** series registered by the run's own
exporter (`NewPrometheusExporter`, `pkg/benchmark/prometheus.go`) and served
on `ITERION_PROMETHEUS_ADDR`'s `/metrics`. The OTLP exporter is the other
plane — per-event traces and log aggregation, not metrics — so a collector
pointed at iterion **without** `ITERION_PROMETHEUS_ADDR` leaves the dashboard
empty.

| Metric | Type | Labels |
|---|---|---|
| `iterion_node_cost_usd_total` | counter | `node_id`, `run_id` |
| `iterion_node_tokens_total` | counter | `node_id`, `run_id`, `model` |
| `iterion_llm_request_total` | counter | `node_id`, `model` |
| `iterion_llm_retry_total` | counter | `node_id`, `model` |
| `iterion_tool_call_total` | counter | `node_id`, `tool` |
| `iterion_node_duration_ms` | histogram (buckets 50 ms → 300 s) | `node_id` |
| `iterion_parallel_branches` | gauge | *(none)* |

`node_id` is the workflow node ID, `run_id` the iterion run identifier, `model`
the full model spec (e.g. `anthropic/claude-sonnet-4-6`), and `tool` the tool
name on a tool-call event.

## Backend coverage

iterion attributes metrics from each backend on a best-effort basis:

| Metric                          | claw | claude_code | pi | kimi | grok | codex |
|---------------------------------|:----:|:-----------:|:--:|:----:|:----:|:-----:|
| `iterion_llm_request_total`     | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| `iterion_llm_retry_total`       | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| `iterion_node_duration_ms`      | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| `iterion_tool_call_total`       | ✅ | ✅ | ✅ | — | — | ✅ |
| `iterion_node_tokens_total`     | ✅ | ✅ | ✅ | ✅\* | ✅\* | ✅ |
| `iterion_node_cost_usd_total`   | ✅\*\* | ✅\*\* | ✅† | — | — | ✅\*\* |
| `iterion_parallel_branches`     | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |

\* Kimi/Grok token counts are present only when the CLI's JSON output includes
usage. Their legacy protocol adapter exposes a total rather than an
input/output split.

\*\* Cost is computed from the per-model pricing table in
`pkg/backend/cost/cost.go`; an unknown model emits no `_cost_usd` field.

† Pi supplies its own provider-computed input/output cost in both RPC and print
modes, so it does not depend on Iterion's pricing table.

Every backend result is stamped with the common `_tokens` / `_model` /
`_cost_usd` fields that are available. Tool-call counters are narrower: claw
emits them from its native loop, Claude Code from SDK stream blocks, pi from
RPC events, and Codex from app-server item lifecycles (`WebSearch`, `Bash`,
etc.). The current Kimi/Grok adapters expose no per-tool callback. Codex Web
search exposes a call count but not a per-call billed amount; `_cost_usd`
therefore remains the model/token estimate rather than pretending to include
an unknown search surcharge.

If a particular SDK version omits the usage block (e.g. early codex
betas), the tokens counter simply does not increment for that node — no
zero-fill is emitted, which keeps the dashboard's "no data" state
distinguishable from a real zero.
