# ADR-174 — The org is the envelope of budget and capacity; a team carries an optional share; a plan supplies the defaults

- **Status**: Proposed — drafted on 2026-11-26 from the inventory on [#3748](https://github.com/SocialGouv/iterion/issues/3748) and revised after an adversarial review. It awaits the operator's decision, and nothing below is built.
- **Date**: 2026-11-26
- **Deciders**: jo (the decision, pending); Claude (the inventory, verified against the code; this draft; two adversarial review passes, one on facts and one on design)
- **Amends**: [ADR-048](048-org-team-hierarchy.md). ADR-048 decision 2 said "Teams can optionally carry a sub-cap", and its P3 listed "per-team sub-caps". Its as-built section kept `MaxConcurrentRuns` and `LaunchRatePerMin` on `Team`, and no sub-cap was ever built.
- **Relates to**:
  - [ADR-032](032-quota-optimistic-inc-with-detached-context-rollback.md) — the atomic monthly counter.
  - [ADR-033](033-launch-gate-fails-open-on-metering-error.md) — the gate fails open on a store error.
  - [ADR-155](155-reserved-capacity-is-a-durable-allocation.md) — reserved capacity. ADR-155's own text has not been amended; the operator's arbitration of 2026-11-24, in [a comment on #2968](https://github.com/SocialGouv/iterion/issues/2968), drops its `capacityledger` and turns its slot axes (team cap, per-bot reserve) into `store.CredSlotStore` subjects.
  - [ADR-162](162-the-fork-review-lane-is-a-gesture-on-floors-that-land-first.md) — the fork-author bound, which fails closed in the launch gate.
  - [ADR-167](167-a-shared-gateway-bounds-its-alive-runs-through-the-run-admission-ledger.md) — `gw:<host>`, the first non-key subject of `CredSlotStore`. It is designed, with only the store slice merged; [#3254](https://github.com/SocialGouv/iterion/issues/3254) is open.
- **Serves**: [#3748](https://github.com/SocialGouv/iterion/issues/3748)

## Context

ADR-048 put "plan + quotas/cost-cap" on the org, and built the monthly half of it: `Org.MonthlyRunQuota` and `Org.MonthlyCostCapUSD` are charged on the `orgusage.OrgSubject` counter, summed across the org's teams. The inventory of 2026-11-26 ([#3748](https://github.com/SocialGouv/iterion/issues/3748), every point traced to `file:line`) found that everything else that bounds a tenant is keyed on the **team**:

- **Capacity.** `Team.MaxConcurrentRuns` and `Team.LaunchRatePerMin` ([pkg/identity/types.go](../../pkg/identity/types.go)) are enforced in [`gateLaunch`](../../pkg/server/launch_gate.go), and the org has no such field.
- **Webhook calls.** The monthly counter's key is `org|<TenantID>`, and the webhook config's `TenantID` is the team id ([pkg/server/webhooks_routes.go](../../pkg/server/webhooks_routes.go)), so every team gets its own 10 000 calls a month. That 10 000 is a constant, with no field and no env override.
- **Memory.** Setting the org's `MemoryQuotaBytes` copies it onto the bucket of each team that exists at that moment, so every team carries the whole ceiling. A team created later starts on the platform default instead ([pkg/server/admin_orgs_routes.go](../../pkg/server/admin_orgs_routes.go), [pkg/store/mongo/memory.go](../../pkg/store/mongo/memory.go)).
- **Team count.** Nothing bounds how many teams an org has. An org admin therefore multiplies every team-keyed ceiling by creating teams.

The monthly budget has no per-team share: a team that loops spends the org's month, and every other team of the org is refused with a 402 until the first. The concurrency cap counts active runs and then compares the count, so two replicas can both admit into the last slot. Defaults come from `ITERION_ORG_DEFAULT_*`, which the chart ships as 0 (unlimited). There is no layer between a tenant's field and the deployment's env, and nothing ties limits to an offer.

On requests, only auth, launches (when a launch rate is configured, and the chart ships 0), webhooks, connector ingress, legal documents and config-share are rate-limited. Authenticated API reads and writes, WebSocket connections, invitations and web-push have no bound per user, token or team.

The operator's posture bounds this record in two ways. First, nothing is built without a trigger: on #2968 (2026-11-24) the operator found no live reservation and no live cap in production, since the chart ships 0. Second, tenants are controlled: sign-up stays `invite_only` in the short term. This record therefore fixes the **model**, and names the trigger that builds each piece.

## Decision

### D0 — One convention and one resolver for every limit

- **One value convention.** Every new field (on the org, on the team, in a plan) and every D6 shape reads its value the same way:
  - **0** inherits the next layer;
  - **−1** means unbounded at this layer, explicitly;
  - a **positive** value is the limit.

  This is the convention the fork-author org fields already ship (ADR-162), extended instead of adding a third family. A plan can never make a tenant unbounded by accident, and a tenant can be made unbounded on purpose, in a way that can be grepped.
- **One resolver.** A single function, `Resolve(ctx, org, team, field)`, folds the layers in order: team, then org, then plan (D3), then `ITERION_ORG_DEFAULT_*`, then the built-in default. It is the only site behind:
  - `gateLaunch`;
  - the org-admin clamp on team caps, which clamps against the resolved org or plan value rather than the env;
  - the effective values the usage views show.

  `orValue` and every direct read of `orgDefaults` go through it; a `grep` proves it.
- **Who may raise a value.** The super-admin may raise any value. That includes a team cap above the platform default, which the org-admin route refuses today for every caller, super-admin included ([#3753](https://github.com/SocialGouv/iterion/issues/3753)).

### D1 — The org is the envelope of every countable limit, capacity included

- **Capacity on the org.** `Org.MaxConcurrentRuns` and `Org.LaunchRatePerMin` join the org's monthly fields. A launch needs room in the team **and** in the org.
- **Webhook calls on the org.** `Org.MonthlyWebhookCalls` replaces the constant, defaulting to 10 000 through D0. The counter is then keyed on the org. Re-keying shrinks an N-team org from N × 10 000 calls to its own value, so the field and its override land **before** the re-key.
- **Memory on the org.** The memory quota is resolved from the org when a team's counter is created, instead of being copied at PATCH time. The target is one org counter, of which a team's value is a share (D2).
- **Team count.** `Org.MaxTeams` bounds team creation, so creating teams cannot multiply the envelope.

### D2 — A team carries an optional share of each org limit

- **What a share is.** `Team.MonthlyRunQuota` and `Team.MonthlyCostCapUSD` define the team's share, charged on a new `orgusage.TeamSubject(orgID, teamID)`.
- **Spelling.** The subject is `team|<orgID>|<teamID>`, escaped like `ForkAuthorSubject`, so the org purge reaches it. `orgsweep`'s alternation gains `team`, and team deletion purges the row.
- **Gate charge.** The gate charges the team subject, then the org subject. The admission handle carries the **ordered list** of subjects it charged, and one release helper gives all of them back. That helper is called by `rollback`, every deny branch (fork-author included) and every refusal that proves nothing started (`RunMayHaveStarted`, #1725). An org-metering error after a granted team charge returns a handle that still holds the team charge. A team-metering error refuses an untrusted subject, as ADR-162 decision 5 does for its own counter.
- **Runner charge.** Dollars are charged after the fact by the runner, so a gate-only change would leave the team's cost document at 0 forever. The gate therefore stamps the team subject on the queue message, as it stamps `ForkAuthorSubject` (a wire-version bump). `recordOrgSpend` then makes a third `AddSpend`, on the team subject, beside the org's and the fork author's. A runner older than the bump does not charge the team, so the team's cost under-counts until the fleet rolls.
- **What a share bounds.** A share stops **new** launches once the **reported** spend crosses it. It does not stop a run in flight. The overshoot is bounded by the runs in flight times their per-run spend, which `max_cost_usd` and `ITERION_CLOUD_MAX_*` bound.
- **Oversubscription.** A share is a **ceiling, not an allocation**: the shares of one org may sum past the org's limit. That is the same reading as a budget floor ("a preference, not a guarantee"); a guaranteed share would need the durable allocation the 2026-11-24 arbitration declined to build.
- **Denial tokens.** A refused share answers `team_monthly_run_quota_exceeded` or `team_monthly_cost_cap_exceeded` (402), so the operator knows to raise the team's share, not the org's.
- **Budget floors.** A floor subtracts from the **org-level** axis only: dollars today, and slots once D1 builds the org cap. A team share is never lowered by a floor. Otherwise every team whose share sits at or below a reserve would be refused all unreserved work for the month, even a team that never runs the reserved bot. The denial detail names the ceiling that refused.

### D3 — A plan is a named set of defaults, between the tenant and the deployment

- **What a plan is.** A record in the platform settings, runtime-mutable, super-admin only and audited (like `budget-floor` and `usage-caps`). It names defaults for every D1 and D2 field and for the D6 shapes.
- **Assignment.** `Org.PlanID` names the org's plan. D0's resolver reads it as the layer between the org and the env.
- **Reading a plan at launch.** The resolver reads it with a ctx, behind a TTL cache like `usagecap.Resolver`. A read that fails after a first success keeps the last known value. A plan never read since the replica started falls through to the env, and that fall-through is counted (D5).
- **What a plan is not.** It carries no price, no billing and no entitlement. Tying a plan to an offer is a later decision, and nothing here presumes it.

### D4 — The concurrency cap: the gate chooses, the publisher enforces

This follows the per-key cap's own split, and ADR-167's: one ledger, one acquirer, one refusal contract.

- **The gate chooses.** `gateLaunch` keeps `CountActiveRunsByTenant`, extended to the org's count. It stays **advisory**: it answers 429 `concurrency_cap_exceeded` with `Retry-After`, and it fails open (counted, D5). Most refusals happen there, cheaply.
- **The publisher enforces.** The publisher adds `team:<id>` and `org:<id>` to its all-or-nothing `CredSlotStore` reservation loop, ordered from the narrowest row to the widest: the run's keys, then the team, then the org, then the gateway. A reservation given back on a narrow refusal therefore never touches a row shared more widely.
  - A refused run is parked `failed_resumable` with its retry armed in the same write, under `TENANT_SLOT_WAIT`, which the retry sweeper re-asks. This is the rare loser of a race between replicas.
  - A ledger error refuses the launch, as the ledger's contract says for a key: a hard bound that opens on an incident is only advisory. This is the one arm of the tenant cap that does **not** fail open, and the exception to ADR-033 is deliberate.
- **Stamping.** The tenant subjects are composed from `run.TenantID` and the run's org at stamp time, by **one** helper shared by the launch stamp and both `SetRunCredStamp` twins. They never come from the sealed bundle, because a re-resolution replaces `CapacitySubjects` wholesale.
  - **Blast radius.** ADR-167 makes a re-stamp fail closed whenever a stamp carries a subject. With a tenant subject on every run, every resume's re-stamp becomes fail-closed: a ledger outage then refuses resumes as well as launches.
- **Sub-bots.** A tree is one place (ADR-167 §5): a sub-bot child runs under its live ancestor's place and asks for none. That is a change of meaning — the cap counts trees, where `CountActiveRunsByTenant` counts children too — and [quotas-and-limits.md](../quotas-and-limits.md) says so in the same change. The resumes a run's own machinery drives (answer-human, host-event, missions) are seen through adoption of the stamp; each gets a conformance row.
- **Dependencies.** Enforcement is only atomic once ADR-167's stamping (T2) and its transit guard (T4a) exist. Without them, a NATS redelivery of a `failed_resumable` run executes with no admission. So D4 ships after them, and like ADR-167 it **observes first, then enforces behind the activation guard**. That guard refuses enforcement while alive runs are unstamped, because adoption cannot see runs that predate the deploy.

### D5 — A fail-open is counted

ADR-033's fail-open on a store error stays, but every arm that admits on an error increments `iterion_launch_gate_fail_open_total{arm}`, and the starter alert pack gains a rule on it. The arms:

| Arm | What it is | What changes |
|---|---|---|
| `team_read` | the team cannot be read | counted |
| `org_read` | the org cannot be read | `orgForTeam` folds the error into a zero org today, without logging. That skips the org's suspend check **and** meters the run on the team key, with the platform defaults. It must surface its error, log it, and say what it charged. |
| `active_count` | the tenant's active runs cannot be counted | counted |
| `allow_run` | the monthly counter errors | the launch proceeds unmetered; counted |
| `repo_usage` | the per-repository quota read fails | counted |
| `team_share` | the D2 team counter errors | counted |
| `plan_read` | the plan is unreadable, or never read since the replica started | the resolver falls through to the env; counted |
| `rate_limiter` | the Valkey limiter errors | it swallows the error inside `allow()` today, with no log (a code comment in `valkey_stores.go`, not a decision of ADR-033). `authRateLimiterBackend.allow` must return the error — an interface change — so that the arm can be logged and counted. |

The 429s the auth limiter answers get their own counter, `iterion_ratelimit_denied_total{bucket}`. Tracked on [#3752](https://github.com/SocialGouv/iterion/issues/3752).

### D6 — Authenticated requests are rate-limited per principal, then per team

- **One middleware** on the authenticated API, reusing the Valkey-backed `authLimiter`. It keys a bucket on the principal (user id, or PAT id), then one on the team. Machine principals (daemons, webhook-kind identities) get their **own** team bucket, so a team's bots never throttle its humans.
- **Shapes.** They come from D0's resolver, with plan and env layers. A new denial token, `api_rate_limited`, answers 429 with `Retry-After`.
- **WebSocket.** The number of concurrent connections per user is capped.
- **Side-effecting routes.** Invitations (which send email), WebSocket tickets and `/push/test` (see [#3750](https://github.com/SocialGouv/iterion/issues/3750)) get `limitRoute` buckets.
- **Without Valkey.** The in-process twin is per pod, so N replicas allow N times the shape. The server warns at boot when D6 is enabled on a cloud store without Valkey, as it already warns for the publish grant.

## Triggers — what builds when

| Piece | Built | Trigger |
|---|---|---|
| D5 — fail-open counted | **now** ([#3752](https://github.com/SocialGouv/iterion/issues/3752)) | Operator priority (2026-11-26). Every other guard is observable only through it. The cost is an interface change on the limiter and a surfaced `orgForTeam` error. |
| D0 — the hatch on team caps | **now** ([#3753](https://github.com/SocialGouv/iterion/issues/3753)) | A bound with no override is a defect (philosophy #1). |
| D2 — the team share of the monthly budget | **now** | Operator priority (2026-11-26): one team must not be able to spend its org's month. The cost: the gate charge and the release helper, a queue wire bump, the runner's third charge, an `orgsweep` row. It bounds new launches, not a run in flight. |
| D4 — the publisher enforces the concurrency cap | after #3254 T2 and T4a, on trigger | The first team or org with a concurrency cap above 0 in production. Until then the chart ships 0, and the gate's count is the cap. |
| D1 — org capacity, `MaxTeams`, org-keyed webhook and memory counters | on trigger | An org whose teams compete for one runner fleet, or any tenant the operator does not control. |
| D3 — plans and the full resolver | on trigger | A second class of tenant that needs other defaults than the deployment's. D0's resolver lands with D2, without the plan layer. |
| D6 — API rate limiting | on trigger, at the latest before `ITERION_SIGNUP_MODE=open` | A tenant the operator does not control, or a public exposure. |

The work items are tracked under [#3748](https://github.com/SocialGouv/iterion/issues/3748). Each is opened when this record is accepted.

## Consequences

**No migration for the new fields.** Each one reads 0 and inherits, so nothing changes until someone sets them.

**Re-keying is not free.** Moving the webhook counter to the org restarts the current month and shrinks a multi-team org's envelope. The new field must therefore land first. Moving memory to one org counter replaces the per-team copies. Both are acceptable while tenants are controlled; neither is once a tenant pays for its quota.

**Admission does more work.** Per launch:

- **D2** adds one conditional `findOneAndUpdate` on the team's document, plus one rollback write when the org refuses.
- **D4** adds, per tenant subject, an upsert and up to `credSlotReserveRounds` conditional writes, with adoption reads. The `org:<id>` row is written by every team of the org: under a cross-team burst, that contention can exhaust the bounded rounds. The store then returns an error, which D4's enforcer refuses, by its fail-closed rule.

**The cap's meaning changes once D4 enforces.** It counts trees instead of runs, and resumes become fail-closed when the ledger is down. Both are written into [quotas-and-limits.md](../quotas-and-limits.md) by the change that ships them.

**Docs.** Each piece lands its rows, tokens and resolution order in [quotas-and-limits.md](../quotas-and-limits.md) in the same change.

**Cloud-native.** Mongo counters, `cred_slots` and Valkey buckets are shared across replicas. D6 without Valkey is per pod and says so at boot. Local mode has no gate, as before.

## Alternatives considered

- **Keep capacity on the team, and only cap the team count.** This closes the multiplication with the least work, but an org admin still cannot share one envelope between teams, and a team's monthly spend stays unbounded within the org.
- **Move everything to the org and drop the team's limits.** Simpler, but it loses what ADR-048 deliberately kept: a team's executor protection, and a team suspended without its org.
- **Reserve the tenant slot in `gateLaunch`.** One acquirer before the run exists and a second in the publisher, with two refusal contracts (429 vs park). ADR-167 rejected that shape for the gateway.
- **The `capacityledger` of ADR-155.** Declined on 2026-11-24: `CredSlotStore` already holds admissions in production, and a second ledger would duplicate it.
- **Commercial plans with billing (Stripe or similar).** Out of scope: nothing in the code or on the board asks for it today. D3 leaves room for it without presuming it.

## Open questions for the operator

1. **D2: may team shares oversubscribe?** As proposed they may. The alternative is that their sum must stay within the org's limit, refused at write.
2. **D3: where do plans live?** As proposed, in the database (runtime-mutable). The alternative is Helm values, so that a deploy witnesses every change.
3. **D1: how is the webhook counter re-keyed?** Either accept the reset of the current month, or carry the counts over at cutover.
4. **D4: the sequencing.** You set atomic concurrency as a high priority on 2026-11-26. This record puts it behind #3254 T2 and T4a and behind a live cap, because before them it is not atomic. Should #3254 be brought forward, or do the gate's count and D5's counting suffice until a cap is live?
