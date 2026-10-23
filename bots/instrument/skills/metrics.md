---
name: metrics
description: The metrics family — what "done" means for a /metrics surface (HTTP request count and latency by route and status, process metrics, scrape wiring), and the discipline that keeps it honest. Read when scope asks for metrics.
---

# metrics — the family

Your job: give the repository a **`/metrics` surface a Prometheus can
scrape**, carrying at minimum the three signal groups below, wired the way
the repo already serves HTTP and configured the way it already reads
config. The stack SDK lives in the `lang-<stack>` skill — read the one(s)
matching the repo.

## Definition of done (all five, or say which fails and why)

1. **A `/metrics` endpoint on the production surface** — the same server/
   app object that serves real traffic (or the SDK's default server when
   the stack separates it, e.g. a second port), reachable on the address
   the deployment exposes. A metrics registry nothing scrapes is not done.
2. **HTTP request count and latency, by route and status CLASS** — the framework
   instrumentator/middleware when the SDK ships one (FastAPI, Gin, Echo,
   Express, Chi…); hand-rolled counters only when no instrumentator exists,
   and then with a route TEMPLATE ((`/users/{id}`), never a raw path with
   ids in it — cardinality is the failure mode.
3. **Process metrics** — the SDK's default collector (CPU, RSS, FDs, GC,
   event-loop lag where applicable). This is nearly free; omitting it is
   a finding.
4. **Scrape wiring where the repo carries its deployment manifests** — a
   `ServiceMonitor` (prometheus-operator CRD) or scrape annotations
   (`prometheus.io/scrape: "true"`, port, path) on the deployment/service,
   matching whichever convention the repo's manifests already use. If the
   repo carries no manifests, say so in the report instead of inventing
   infrastructure.
5. **Env-gated like the DSN** — the SAME semantics as the Sentry DSN
   toggle (instrumentation §4): absent or false = off, true = on — strict
   opt-in, never on-by-default — following the house config convention for
   the name, with the toggle named in the report.

## Discipline

- **Count and duration, by route template and status code** — a metric
  that cannot answer "which endpoint got slow" is decoration.
- **Do not double-instrument** — if the repo already exposes `/metrics`
  (actuator, prometheus_client already wired), SURVEY it, fill the gaps
  against the definition of done, and report what was already there.
- **Histograms over summaries** for latency when the SDK makes you choose
  (summaries do not aggregate across replicas).
- **Cardinality budget**: no label whose values are unbounded (user ids,
  full URLs, trace ids). Route templates and status classes only.
- **Verify like everything else** (instrumentation §verification): the
  endpoint answers 200 with the documented metric names — a curl in the
  verify step, or the fixture test when the repo has tests.

## Report

The campaign report names: the endpoint and port, the three signal groups
with their metric names, the scrape wiring added (or why not), the env
toggle, and the cardinality decisions taken.
