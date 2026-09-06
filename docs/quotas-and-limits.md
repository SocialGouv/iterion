# Quotas and limits

**Audience.** Anyone choosing platform-default values, deciding what to
set on a paying org, or debugging "why did this run get denied". Both
the operator-set platform defaults and the per-org overrides documented
here come from real fields on real records — not aspirational settings.

Iterion enforces five distinct limits at run launch and one at the
webhook intake. They live behind a single decision function
([pkg/server/launch_gate.go:gateLaunch](../pkg/server/launch_gate.go))
called by every code path that creates a run on a cloud instance: the
HTTP launch and resume, the inbound webhooks, the retry sweeper's
automatic resumes and the board dispatcher's launches — the table in
[Which surfaces are gated](#which-surfaces-are-gated) is the exhaustive
list, with the two paths that still launch outside it.

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

## Which surfaces are gated

Every launch a cloud instance performs passes `gateLaunch` with the
identity of whoever is launching, meters one monthly run at step 5, and
hands the slot back when the run service then refuses the launch (a
sealing failure, a queue outage, a bot that does not compile — no run
exists, so nothing was consumed):

| Surface | Identity on the ctx | Gated | Metered |
|---|---|---|---|
| `POST /api/runs`, the studio, `iterion remote runs launch`, the MCP `remote_runs_launch` | the caller's | yes | yes, rolled back on a refused launch |
| `POST /api/runs/{id}/resume`, the WS answer that resumes a run | the caller's | yes | yes |
| Inbound webhooks, direct launch (`insertAndLaunchWebhook`) — including the merge-gate auto-fix and relaunch lanes, which reuse that tail | the token's synthetic `webhook` identity | yes | yes, rolled back on a refused launch and for the idempotency loser |
| Inbound webhooks, **board mode** (the command creates a card; the dispatcher launches it) | the token's | pre-check only, at card creation | **no** — a card is not a run; the pre-check's slot is handed back at once and the dispatcher meters the launch when it claims the card |
| **Board dispatcher** (`processBoardCard`) | `board-dispatcher` on the card's team | yes | yes, rolled back on a refused launch |
| Retry sweeper (automatic resume of a `failed_resumable` run) | the run's owner | yes | yes, rolled back on a failed resume |
| `POST /api/v1/triggers/emit` (custom event) | the caller's | once per request | once per request — the launches it fans out to go through the spine launcher below |
| Trigger spine direct launches (`serviceLauncher`: `mode: direct` board triggers, run-completion chains, the emit fan-out) | store tenant only, no auth identity | **no** | no |
| `cloudsched` scheduled launches (`launchScheduledBot`) | store tenant only, no auth identity | **no** | no |
| Local mode (`iterion studio` / `iterion dispatch` with no identity store) | — | no gate exists | — |

On the board dispatcher a denial is a **launch refusal** of the
dispatcher's transient class, not a verdict on the card
([dispatcher.md](dispatcher.md#claim-selection-on-the-cloud-board--what-is-never-claimed)):
the card returns to its column under the machine provenance
`launch_refused`, its ledger reads the rule that refused it — `launch
gate: concurrency_cap_exceeded: org has 3 active runs (cap 3) …` — and
the next attempt waits out the backoff, so an org at its cap retries its
ready cards on the backoff schedule, not on every 5s tick. A cap that
does not free within the attempt cap files the card `blocked` under
`launch_given_up` with the rule on it, and the pipeline board shows it
in its *Needs attention* lane.

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
| `org_usage.cost_usd` + tokens | At the end of each runner execution attempt, from that attempt's accumulated LLM events | [pkg/runner/loop_spend.go:recordOrgSpend](../pkg/runner/loop_spend.go) calls `orgusage.AddSpend` |
| `webhook_deliveries.count` | At webhook admission (after auth + rate) | [pkg/webhooks/store.go:Counter](../pkg/webhooks/store.go) |

The run counter includes **every** launch: REST `POST /api/runs`,
`POST /api/runs/{id}/resume` (a resume re-enters the engine and spends
like a launch), and inbound webhook deliveries. A re-published DLQ
message does **not** double-count — it picks up the existing run row.

Cost metering is "floor, not invoice":

- **`claw`** (in-process LLM) is priced through `pkg/backend/cost` and
  reports `cost_usd` per call.
- **Every CLI delegate** (`claude_code`, Codex, `pi`, Kimi, and Grok)
  contributes its aggregate token total when the CLI reports usage. The cloud
  runner's delegate event has no input/output split, so that total is currently
  booked to `input_tokens`.
- A CLI delegate's `cost_usd` **is** added to `org_usage.cost_usd` — the
  `delegate_finished` figure flows through `metricsEmitter.RunTotals` into
  `recordOrgSpend`. `claw` is the one exclusion, and deliberately: being
  in-process it emits *both* a priced `llm_step_finished` per step and a
  delegation total, so counting both would charge every claw run twice and trip
  an org's monthly cap at half its budget
  ([pkg/runner/loop_metrics.go:240-260](../pkg/runner/loop_metrics.go),
  [loop_spend.go](../pkg/runner/loop_spend.go)).
- It is still a floor, not an invoice: a delegate that reports no cost
  contributes none. Treat the monthly USD cap as a trend signal rather than a
  billing ledger.
- **A forfait run does NOT report `$0`** — and reading `cost_usd` as money
  spent is the misreading this bullet exists to prevent. `claude_code` prints
  `total_cost_usd` on every call whatever pays for it: on a **subscription**
  it is the price those same calls WOULD have cost metered, cache creation
  billed at 1.25× and cache reads at 0.1× included. Measured 2026-09-03 on a
  cloud runner holding a forfait: `claude -p "reply pong"` — three input
  tokens, five output — reported **$0.0402**, because it created 5 751 cache
  tokens and read 17 120. Nothing was charged; the plan is flat. So an org
  showing `cost_usd_this_month: 1991` on forfait-served runs has spent that in
  *equivalent API price*, not in money: the only real money on a subscription
  is the **extra-usage** overage, which the provider's own console is the sole
  authority on.
- And the bucket is the **ORG**, not the credential: `recordOrgSpend` charges
  `msg.OrgID` whatever tier served the run (team forfait, credential pool,
  platform keys, BYOK). The figure answers "what did this org consume", never
  "what did this key cost" — which is what the **per-credential ledger**
  below answers.

## Per-credential usage — what did THIS key cost

[`pkg/credusage`](../pkg/credusage/credusage.go) is the second bucket, fed
from the same attempt beside `recordOrgSpend`. It keys on
`{fingerprint, provider, tier, tenant} × month` and answers the question the
org counter structurally cannot.

Two properties carry it:

- **Split by backend.** A run can spend a `claude_code` forfait on its
  implementer and a platform codex key on its plan review, while
  `RunTotals()` is one number that belongs to neither. The spend is taken per
  `(backend, model)` ROUTE, and the MODEL is what names the provider (a
  `claw` node can be pointed anywhere). A route iterion cannot attribute —
  a bare model id on a multi-provider backend, a provider the run holds no
  credential for — is charged to **nobody**: no figure beats a wrong one.
- **Nature, in the API.** Every amount is typed `metered` (real money on an
  invoice: a BYOK or lent API key) or `estimate` (a subscription — see the
  `total_cost_usd` bullet above). The same line
  `credpool.CredentialSource.Metered()` draws, asserted equal by
  `TestCredentialNature_AgreesWithCredpoolMetered`. The list responses keep
  `metered_usd` and `estimated_usd` **apart** for that reason: summing them
  reproduces exactly the misreading the ledger exists to remove.

The `tier` (`team` | `pool` | `platform`) is part of the meter identity, not
a label: the same key lent through the pool and used by its owner are two
different economic facts.

**Where a route's model comes from.** The runner's metrics emitter names each
node's route from three events, in order: `delegate_started.declared_model`
(the node's spec, provider included — every backend emits it), then each
`llm_request` (the id the call actually went to), then
`delegate_finished.effective_model` when the backend reports one. A backend
reports the id it CALLED, and claw strips the provider before the request —
so a claw step reports `gpt-5.6-sol`, not `openai/gpt-5.6-sol`. A bare id
names no provider and would fall to the backend's default wire (anthropic for
claw), charging an OpenAI model's tokens to the Claude forfait; when the
reported id is the declared model without its prefix, the route keeps the
declared, provider-qualified name. A different id (a fallback element) is kept
as reported.

**claw inside a sandbox.** The LLM loop runs in `iterion __claw-runner` in the
container, and the runner relays its per-step `llm_request` /
`llm_step_finished` to the launcher over the IPC ([sandbox.md](sandbox.md#claw-backend-in-sandbox)),
so a sandboxed claw node is metered from its steps exactly like an in-process
one — and the `delegate_finished` total, a summary of those steps, is not
counted again (cost or tokens). When a run's container carries an older
runner that relays nothing, that total is the only observation: it is booked
on the route and, when the event carries no `cost_usd`, priced from the table
at the model's **input** rate — a floor, since one aggregate count cannot be
split into input and output — or left unpriced when no source knows the
model. Zero is unknown, never free.

A route iterion cannot attribute is logged at **warn** by the runner, once per
route per attempt (`no credential iterion can name`): the decline is
definitive for that attempt, so it has to be visible.

```sh
# This team's credentials, this month
iterion remote usage --by-credential
# GET /api/teams/{id}/credentials/usage

# The platform tier across every tenant it served (super-admin)
iterion remote api GET /api/admin/credentials/usage
# ?tier=team|pool|platform — or ?fingerprint=<fp> for one credential,
# whose rows live under each tenant that drew on it.
```

Metering is best effort throughout, like the org bucket: a missing counter,
an unattributable route or a store failure leave the observation on the
floor rather than turn a finished run into a failed one.

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
