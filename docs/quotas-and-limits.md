# Quotas and limits

**Audience.** Anyone choosing platform-default values, deciding what to
set on a paying org, or debugging "why did this run get denied". Both
the operator-set platform defaults and the per-org overrides documented
here come from real fields on real records — not aspirational settings.

Iterion enforces six distinct limits at run launch and one at the
webhook intake — and, since #950, **reserves** capacity for named
workloads, which is the one thing on this page that is not a ceiling
(see [Budget floors](#budget-floors--capacity-reserved-for-a-workload)). They live behind a single decision function
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
2. **Per-repository quota** — when the launch names a repository and a
   quota covers it, its month-to-date consumption (read off
   `pkg/credusage`'s repository dimension) must be under the ceiling. See
   [Budget floors](#budget-floors--capacity-reserved-for-a-workload).
3. **Concurrency** — `count(active runs for tenant) < MaxConcurrentRuns`
   ([CountActiveRunsByTenant](../pkg/server/launch_gate.go)). Active =
   `queued` or `running`.
4. **Launch rate** — token-bucket `LaunchRatePerMin` per org, rate =
   `perMin/60` per second, burst = `perMin`.
5. **Monthly cost cap** — `MonthlyUsage.CostUSD < MonthlyCostCapUSD`,
   read from the Mongo `org_usage` counter.
6. **Monthly run quota** — `AllowRun()` atomically increments the
   counter and reports `ok=false` if the new total would exceed
   `MonthlyRunQuota`. This is also the **metering** step — a successful
   run consumes one slot at this point.

Steps 3 and 5 are additionally **lowered by any capacity reservation that
does not name this launch's bot** — the floor described below. A
reservation never creates a limit that is not configured, so a deployment
with no concurrency or cost cap is unaffected by one.

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
| `POST /api/v1/triggers/emit` (custom event) | the caller's | pre-check only, per request | **no** — an emit is one EVENT and fans out to 0..N launches; the pre-check's slot is handed back at once and the spine meters each launch it performs |
| Trigger spine direct launches (`serviceLauncher`: `mode: direct` board triggers, run-completion chains, the emit fan-out, the local schedule source) | `trigger-spine` on the subscription's team | yes | yes, rolled back on a refused launch |
| `cloudsched` scheduled launches (`launchScheduledBot`) | `cloud-scheduler` on the schedule's team | yes | yes, rolled back on a refused launch |
| Local mode (`iterion studio` / `iterion dispatch` with no identity store, the pipelines admission) | — | no gate exists | — |

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

On the two surfaces that have no request to answer, a denial is recorded
where an operator reads it, never skipped in silence:

- a **trigger subscription** carries `last_error` + `last_error_at`
  (`GET /api/v1/triggers`, `iterion remote triggers list`), raised by the
  refusal and cleared by the next launch that goes through;
- a **schedule** carries the same two fields
  (`GET /api/teams/{id}/schedules`, `iterion remote schedules list`), plus
  the tick-audit row the ticker already wrote.

Both are targeted field writes on both store twins, so an operator editing
the row between the match and the record does not lose the edit.

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
| Per-repository quota | `budget_floor` platform settings (`repo_quotas`) | n/a — runtime-mutable, no env default | `repo_quota_exceeded` | 402 |

`Status`, `MonthlyCostCapUSD`, and `MonthlyRunQuota` are **Org**-document
fields (org-wide, super-admin managed — `pkg/identity.Org`); the org
run/cost counters sum every team in the org. `MaxConcurrentRuns` and
`LaunchRatePerMin` are **Team**-document fields (per-workspace executor
caps — `pkg/identity.Team`), and `Team.Status` is now writable through
`iterion remote teams status` (org admin) — it was read by the gate below
and settable by nothing.

A limit this table does NOT carry: **whose credential funds the run**. That
is a separate question with its own two gates — the org's
`CredentialAudience` (which teams may spend the org's shared keys) and the
platform tier's `platform_credentials` audience (which tenants may draw on
the deployment's). Both are documented in
[cloud-llm-credentials.md](cloud-llm-credentials.md); neither denies a
launch, they decide what the run is handed.

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
  runner's delegate event carries no input/output split, so that total is
  reported as **`aggregate_tokens`** — its own field, leaving `input_tokens`
  and `output_tokens` for splits that were actually measured. **A true total
  is the sum of the three**, and a per-direction ratio is only meaningful on a
  row whose aggregate is zero. Zero in all three means *not observed*, never
  *nothing spent*.
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

# A past month, on either route
iterion remote usage --by-credential --month 2026-08
iterion remote api GET "/api/admin/credentials/usage?month=2026-08"

# The platform tier across every tenant it served (super-admin)
iterion remote api GET /api/admin/credentials/usage
# ?tier=team|pool|platform — or ?fingerprint=<fp> for one credential,
# whose rows live under each tenant that drew on it.
```

**The admin route answers for ONE tier, and the default is `platform`.** A
team forfait's spend is metered on a `team` row and is therefore absent from
the unfiltered listing — by design, since no tenant view can show the
platform tier and that is the question this route exists for. Every response
now carries a `scope` object naming what was applied (`tier`, `fingerprint`,
`repo`, `team_id`), because a listing that does not say what it left out
reads as "everything", and a credential missing from "everything" reads as a
credential that spent nothing:

```json
{ "month": "2026-09", "scope": { "tier": "platform" }, "credentials": [ … ] }
```

Both routes take `?month=YYYY-MM`, and **refuse a value they cannot parse**
(400) rather than fall back to the current month — the response is labelled
with a month, so serving another one under that label is a wrong answer, not
a partial one. Two readings of this endpoint made without either property
were what opened #1087 against a counter that was recording normally: a
`?month=` the server ignored returned the current month twice, and the
platform-tier default hid the team row the run had actually charged.

Metering is best effort throughout, like the org bucket: a missing counter,
an unattributable route or a store failure leave the observation on the
floor rather than turn a finished run into a failed one.

### The repository dimension

The meter also keys on the **repository** a run targeted — `repo_id`, the
forge slug (`owner/repo`, `group/sub/project`) the launch surfaces already
stamp on the run as `ProjectPath` and the studio already groups runs by, not
a second identity derived from a clone URL. It is what makes "one busy
repository is eating the shared subscription" a question with an answer.

```sh
# What this team spent on one repository this month
iterion remote usage --by-credential --repo SocialGouv/iterion

# The same repository across every tenant and credential (super-admin)
iterion remote api GET "/api/admin/credentials/usage?repo=SocialGouv/iterion"
# ?fingerprint= and ?repo= are REFUSED together (400): one credential across
# repositories and one repository across credentials are different questions,
# and answering whichever the code checked first returns a figure nobody
# asked for.
```

Four properties are worth knowing, because each is a way to misread the
numbers:

- **Every listing that predates the dimension sums the repositories back
  together**, so `--by-credential` without `--repo` reports exactly what it
  always did. The alternative — one row per repository — would have made a
  credential appear several times with no total anywhere.
- **A row with no repository is not "all repositories".** Local runs, CLI
  runs and non-webhook cloud launches target none, and neither does any row
  written before this existed. `--repo` therefore never reaches them: a
  repository's bill must not quietly include the deployment's unattributed
  runs. The document id omits an empty repo entirely, which is what lets
  those rows keep accumulating instead of restarting from zero at the deploy.
- **A repository's spend spans tenants.** Two teams can serve one repo with
  their own credentials, so the admin view sums both; the team view returns
  only that team's share.
- **A run whose repository cannot be read meters without one.** Same rule as
  the route attribution: charged to the credential, attributed to nobody —
  never guessed.

This is the accounting subject the per-repo quota is enforced against — see
*Budget floors* below.

## Budget floors — capacity RESERVED for a workload

Everything above this line is a **ceiling**: it answers *"how far may this
go?"*. A floor answers the other question — *"how much is held for this,
whatever else runs?"* — and until
[#950](https://github.com/SocialGouv/iterion/issues/950) iterion could not
express it.

The difference is not academic. On 2026-09-08 campaign bots and the PR
reviewer shared one Anthropic subscription; when its five-hour window closed
at 06:14Z, eight runs parked in six minutes and fourteen within the hour. **No
cap had been exceeded and no quota breached** — every individual run stayed
under its own ceiling all the way down. Review simply had nothing held for it.

```sh
iterion remote admin budget-floor                                     # show
iterion remote admin budget-floor reserve --bot review-pr --five-hour 20
iterion remote admin budget-floor quota --repo owner/repo --monthly-usd 50
iterion remote admin budget-floor rm --bot review-pr
```

Each of those subcommands is a GET → edit one entry → `PUT` of the WHOLE
policy, so the write is **conditional**: the record's `updated_at` travels back
as a compare token and a lost race is a `409`, not one admin's reservation
silently deleted by the other's stale document. The CLI answers it by
re-reading and replaying the same edit; a raw
`iterion remote api PUT /api/admin/settings/budget-floor` must send the
`updated_at` it read (omit it only for a deployment's very first write).

### The workload is a bot id

`review-pr` — the thing that actually spends, already on every run and already
the vocabulary a credential-pool pledge uses for its allow-list. No
indirection, no new concept. The cost, stated because it is silent: **renaming
or replacing the bot leaves the reservation pointing at an id nothing
launches**, and it then protects nothing. Re-point it by hand.

### The reserve holds a share of the provider's WINDOW by default

On a subscription the provider bills nothing per call — which is exactly why
`credusage` types those dollars `estimate`. Reserving "$X for the reviewer" on
a forfait reserves a fiction: the run that dies does so because the five-hour
window is spent, not because a figure was reached. So the default axis is the
window, and enforcement needed no new gate — the credential walk already skips
a forfait whose window is closed and falls through to the next tier; the
reserve simply lowers the ceiling that skip is judged against, per bot.

```
5h window utilisation
0%                     50%       60%        70%   80%       100%
|-----------------------|---------|----------|-----|---------|
      everyone      unreserved  feature-dev  review-pr   provider
                     stops       stops        stops       wall
```

Two other axes are available where they are the honest answer, each enforced
at the gate that already caps it: `--monthly-usd` (real money on a metered
key) comes off the org's cost cap, and `--concurrent-runs` off the team's
concurrency cap — the dial that keeps the reviewer answering a PR while a
campaign runs. Any subset may be set.

### Composition: protected from the others, never from itself

A workload's ceiling is the deployment cap **minus the reserves of every
OTHER workload**. With `review-pr` at 20 and `feature-dev` at 10 under an 80%
cap: ordinary work stops at 50, `review-pr` may reach 70, `feature-dev` 60.

Summing *every* reservation instead would refuse a workload on its own band —
the reservation would make its holder stop **earlier** than before it existed,
the exact opposite of a floor, and green under any test that only checks
unreserved work.

### Two things a reservation can never do

- **Create a cap.** A window, cost cap or concurrency cap that is not
  configured is returned untouched. Subtracting a reserve from zero yields a
  positive limit out of nothing, and a deployment that never set a cap would
  ACQUIRE one — every run refused because somebody wrote a reservation. A
  floor may hold work back; it may not invent a ceiling.
- **Let its holder overspend.** The reserved workload never passes the
  deployment's own caps. A reservation holds capacity back from others; it is
  not a way around the wall.

### When the reserves swallow the whole cap

Reserving 50% of the window under a 50% deployment cap — or lowering the cap
after the reservations were written — leaves unreserved work *nothing*, and
that is a refusal, never a lowered ceiling. It has to be said out loud because
the two are one keystroke apart in code and opposite in effect: `MaxPercent 0`
means **"this window is not enforced"** to `pkg/usagecap`, so a ceiling
clamped at zero would hand every unreserved bot an *uncapped* credential —
amplifying the starvation the reserve exists to prevent. So the walk **refuses
the credential** for the unreserved bot and falls through to the next tier,
saying so in the skip log (*"the five_hour window is entirely reserved for
other workloads"*); the org's monthly cost cap does the same on its own axis,
denying with `monthly_cost_cap_exceeded`. `Policy.Validate` cannot catch this
at write time — it knows the 100% window, never the deployment's own cap.

### The per-repo quota

A ceiling inside the shared budget, refused with its own reason
(`repo_quota_exceeded`) because the operator's next move differs: raise *that
repository's* quota, not the org's. Three forms — `--monthly-usd` (default),
`--runs-per-month`, and `--reserve-share N --share-of-bot <bot>`, which slices
a reservation so that raising the reservation raises every repository taking a
share of it.

A share of a **window-only** reserve is refused at configuration time, not
resolved to zero: slicing a live five-hour window between repositories would
need real-time arbitration across replicas, and a locally computed share is a
number two pods disagree about. The refusal names the way out.

It is read off the repository dimension above — the meter the runs actually
write — so the figure `usage --by-credential --repo X` shows **is** the figure
the gate refuses on. A view that disagreed with the gate could not exist.

### What is not covered

The gate is told what a launch is by an explicit subject, and two surfaces
legitimately do not know: the REST `POST /runs` gates before its body is
parsed (an operator-initiated launch, not the automated fan-out these quotas
bound), and a trigger `emit` is one event that fans out to many launches —
each of which is gated with its own subject. Both pass the empty subject and
are judged as ordinary, uncapped work.

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
  "aggregate_tokens_this_month":  2210544,
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
