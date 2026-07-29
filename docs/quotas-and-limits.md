# Quotas and limits

**Audience.** Anyone choosing platform-default values, deciding what to
set on a paying org, or debugging "why did this run get denied". Both
the operator-set platform defaults and the per-org overrides documented
here come from real fields on real records — not aspirational settings.

Iterion enforces five distinct limits at run launch and one at the
webhook intake. They live behind a single decision function
([pkg/server/launch_gate.go:gateLaunch](../pkg/server/launch_gate.go))
called by every code path that creates a run: launch / resume /
inbound webhook.

## The launch-admission order

`gateLaunch` returns the **first** failing check, in this exact order:

1. **Org status** — team `EffectiveStatus()` ∈ {`active`}. Suspended
   and read-only orgs short-circuit here.
2. **Concurrency** — `count(active runs for tenant) < MaxConcurrentRuns`
   ([CountActiveRunsByTenant](../pkg/server/launch_gate.go)). Active =
   `queued` or `running`.
3. **Launch rate** — token-bucket `LaunchRatePerMin` per org, rate =
   `perMin/60` per second, burst = `perMin`.
4. **Monthly cost cap** — `MonthlyUsage.CostUSD < MonthlyCostCapUSD`,
   read from the Mongo `org_usage` counter.
5. **Monthly run quota** — `AllowRun()` atomically increments the
   counter and reports `ok=false` if the new total would exceed
   `MonthlyRunQuota`. This is also the **metering** step — a successful
   run consumes one slot at this point.

Super-admins bypass the whole gate (they explicitly opt out of org
scoping). Local mode (no identity store) has no gate. The gate
**fail-opens** on a Mongo / store error so a transient blip doesn't
wedge every launch — quotas are an operator policy, not a hard security
boundary. The one nuance: when `AllowRun` errors at step 5 the launch
still proceeds **unmetered** (logged WARN) instead of being denied; the
denial path is only the deliberate "this would exceed the cap" case.

## Limits, fields and platform defaults

Every limit has three knobs: an **override field** (on the Org or the
Team document — see the table), a **platform env var** (the default
applied when the override field is zero), and a public **denial reason
token**. Zero means "no limit" everywhere — the safe default for
existing deployments.

| Limit | Override field | Platform env var | Denial reason | HTTP |
|---|---|---|---|---|
| Org suspended / read-only | `Status` | n/a — admin action | `org_suspended` | 403 |
| Concurrent active runs | `MaxConcurrentRuns` | `ITERION_ORG_DEFAULT_MAX_CONCURRENT_RUNS` | `concurrency_cap_exceeded` | 429 (`Retry-After: 30`) |
| Launches per minute | `LaunchRatePerMin` | `ITERION_ORG_DEFAULT_LAUNCH_RATE_PER_MIN` | `launch_rate_limited` | 429 |
| Monthly LLM cost cap (USD) | `MonthlyCostCapUSD` | `ITERION_ORG_DEFAULT_MONTHLY_COST_CAP_USD` | `monthly_cost_cap_exceeded` | 402 |
| Monthly run quota | `MonthlyRunQuota` | `ITERION_ORG_DEFAULT_MONTHLY_RUN_QUOTA` | `monthly_run_quota_exceeded` | 402 |

`Status`, `MonthlyCostCapUSD`, and `MonthlyRunQuota` are **Org**-document
fields (org-wide, super-admin managed — `pkg/identity.Org`); the org
run/cost counters sum every team in the org. `MaxConcurrentRuns` and
`LaunchRatePerMin` are **Team**-document fields (per-workspace executor
caps — `pkg/identity.Team`).

The override-field semantics are pinned in
[pkg/server/launch_gate.go:orValue](../pkg/server/launch_gate.go) (the
tenant override wins when > 0; else platform default; zero = unlimited).
The denial reason tokens are stable strings — clients (the studio, SDKs,
CI scripts) switch on them. The HTTP status codes follow the standard
"402 = paying issue (resets next month), 429 = retry later" convention.

The env vars are read at boot by
[cmd/iterion/server.go:orgLimitDefaultsFromEnv](../cmd/iterion/server.go).
Invalid / negative / unset values fold back to zero (unlimited).

## The denial envelope

Every denial returns the same JSON shape
([pkg/server/launch_gate.go:writeLaunchDenial](../pkg/server/launch_gate.go)):

```jsonc
{
  "error":    "monthly_cost_cap_exceeded",         // stable token
  "detail":   "monthly LLM cost cap ($80.00) reached",
  "reset_at": "2026-07-01T00:00:00Z"               // monthly quotas only
}
```

Plus a header on rate denials:

```
Retry-After: 31
```

Forge webhooks see the **same** envelope when the launch-admission gate
fires — the inbound handler writes a `launch_error` delivery row and
calls `writeLaunchDenial` so a forge integration can react identically
to a UI-driven launch.

## What gets metered

| Counter | When it bumps | Where |
|---|---|---|
| `org_usage.runs` | At launch admission (step 5 above) | [pkg/orgusage/orgusage.go:AllowRun](../pkg/orgusage/orgusage.go) |
| `org_usage.cost_usd` + tokens | After each LLM call on the runner | [pkg/runner/loop.go](../pkg/runner/loop.go) calls `orgusage.AddSpend` |
| `webhook_deliveries.count` | At webhook admission (after auth + rate) | [pkg/webhooks/store.go:Counter](../pkg/webhooks/store.go) |

The run counter includes **every** launch: REST `POST /api/runs`,
`POST /api/runs/{id}/resume` (a resume re-enters the engine and spends
like a launch), and inbound webhook deliveries. A re-published DLQ
message does **not** double-count — it picks up the existing run row.

Cost metering is "floor, not invoice":

- **`claw`** (in-process LLM) is priced through `pkg/backend/cost` and
  reports `cost_usd` per call.
- **Every CLI delegate** (`claude_code`, `pi`, Kimi, Grok, and legacy Codex)
  contributes its aggregate token total when the CLI reports usage. The cloud
  runner's delegate event has no input/output split, so that total is currently
  booked to `input_tokens`.
- Delegate USD is **not** added to `org_usage.cost_usd`: this applies even to pi,
  whose backend result carries a provider-computed cost for run/benchmark
  telemetry. Consequently the monthly USD cap understates delegate-heavy bots;
  use it as a trend signal, not a billing ledger.

## Reading usage

Both views share the same JSON shape
([pkg/server/admin_orgs_routes.go:orgUsageView](../pkg/server/admin_orgs_routes.go)):

```jsonc
{
  "org": { "id": "…", "name": "…", "status": "active", … },
  "members": 12,
  "effective_memory_quota_bytes": 1073741824,
  "monthly_run_quota":            1000,
  "runs_this_month":              347,
  "cost_usd_this_month":          18.91,
  "input_tokens_this_month":      4123890,
  "output_tokens_this_month":      921334,
  "monthly_cost_cap_usd":         80.0,
  "max_concurrent_runs":          5,
  "active_runs":                  2,
  "webhook_calls_this_month":     410,
  "memory_used_bytes":            73801234,
  "api_key_count":                3,
  "generic_secret_count":         2,
  "bot_binding_count":            4,
  "webhook_count":                3
}
```

Two routes serve it:

- `GET /api/admin/orgs/{id}/usage` — super-admin only, any org.
- `GET /api/orgs/{id}/usage` — any member of the org (self-serve
  mirror).

The "effective" values resolve the team override against the platform
default before returning, so the UI shows the **real** ceiling the gate
would apply.

## Webhook call quota — the separate axis

Inbound webhook deliveries have their **own** quota separate from the
run launch counter
([pkg/webhooks/store.go:Counter](../pkg/webhooks/store.go)). It rejects
the request before the launch gate fires — so a flood of "filtered"
deliveries (label edits on a noisy MR) still counts toward the org's
webhook budget, but never against the cost cap or run quota.

- Default per-org cap: **10 000 / month**
  ([pkg/server/webhooks_routes.go:defaultOrgMonthlyWebhookCalls](../pkg/server/webhooks_routes.go)).
- Per-webhook tighter override: `Config.MonthlyCallLimit` (0 = inherit).
- Atomic CAS Mongo counter (`org_usage` reuses the same pattern); a
  denied call does **not** consume quota.

Reset semantics, audit and denial format match the run quota — only the
quota dimension differs.

## Memory quota — pointer

Memory + knowledge spaces have their own per-org aggregate quota
(`MemoryQuotaBytes` on the Org document) plus per-visibility sub-caps.
The launch gate does **not** evaluate it — memory writes go through a
separate CAS check inside the memory store. See
[memory-and-knowledge.md](memory-and-knowledge.md) for the full
contract.

Changing the org override via
`PATCH /api/admin/orgs/{id} { "memory_quota_bytes": … }` propagates
into the enforced counter via `SetTenantQuota` on the cloud Mongo
memory store
([pkg/server/admin_orgs_routes.go:tenantMemoryQuotaSetter](../pkg/server/admin_orgs_routes.go)) —
the field on `Team` alone is not enough, the counter has to be told.

## Prometheus metrics

Every denial / throttle event bumps a counter on the shared registry
([pkg/cloud/metrics/metrics.go](../pkg/cloud/metrics/metrics.go)). No
tenant label is ever attached — cardinality discipline; per-org
accounting lives in the Mongo counters above.

| Metric | Labels | Meaning |
|---|---|---|
| `iterion_launch_denied_total` | `reason` (denial token) | Run launches denied by the admission gate |
| `iterion_webhook_throttled_total` | `provider`, `reason` (`rate_limited` / `quota_exceeded`) | Inbound deliveries throttled before processing |
| `iterion_webhook_deliveries_total` | `provider`, `status` | Every inbound delivery's terminal status |
| `iterion_auth_logins_total` | `result` (`success` / `invalid` / `locked` / `password_change_required` / `error`) | Login attempts |
| `iterion_auth_password_resets_total` | `step` (`requested` / `confirmed`) | Self-service reset flow |
| `iterion_dlq_depth` | — | Runs parked on the DLQ (the orphan / max-deliver bridge) |
| `iterion_runs_orphan_recovered_total` | — | The orphan sweeper's flips to `failed_resumable` |
| `iterion_runs_usage_window_blocked_total` | — | Runs stopped by an exhausted provider quota window |
| `iterion_runs_retry_scheduled_total` | — | Durable automatic retries armed for a provider reset |
| `iterion_runs_retry_resumed_total` | `result` (`enqueued` / `abandoned` / `failed`) | Retry-sweeper outcomes for due runs |
| `iterion_runs_retry_pending` | — | Due-retry rows observed in the latest bounded sweep (sampled gauge) |
| `iterion_runs_retry_sweeps_total` | — | Retry-sweeper passes; flat at zero in cloud means the sweeper is not running, not merely idle |

The starter alert pack
([charts/iterion/templates/prometheus-rule.yaml](../charts/iterion/templates/prometheus-rule.yaml))
fires:

- **IterionLaunchDeniesSpiking** at `sum(rate(iterion_launch_denied_total[10m])) > 0.5`.
- **IterionWebhookThrottling** at `increase(iterion_webhook_throttled_total[1h]) > 50`.
- **IterionDLQNotEmpty** when `iterion_dlq_depth > 0` for 10 minutes.
- **IterionRunnerHeartbeatErrors** on `increase(iterion_runner_heartbeat_errors_total[5m]) > 3`.
- **IterionOrphanRunsRecovered** on `increase(iterion_runs_orphan_recovered_total[30m]) > 0`.

The thresholds are deliberately conservative starting points — tune
them per deployment.
