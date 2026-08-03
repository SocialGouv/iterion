# Credential pool — mutualising contributors' unused LLM capacity

How developers lend spare LLM capacity to an iterion deployment — a Claude
Pro/Max or ChatGPT **subscription**, or a personal **API key** of any
provider — and how a run with no credential of its own draws on it.

Read [cloud-llm-credentials.md](cloud-llm-credentials.md) first: the pool is
the **fourth and last** tier of the credential resolution described there.

## The one-paragraph model

A contributor connects a subscription through the ordinary personal OAuth
flow (or already holds a personal BYOK key), then makes a **pledge**: a standing offer of that credential
bounded by ceilings *they* choose (spend per day and per week, runs per day,
runs at once, an optional sharing window, an optional bot allow-list). At
launch, a run that has neither a BYOK key nor a personal/org forfait asks
the **broker** for a donor; it picks the least-consumed eligible pledge,
records a **lease** against the run, and hands the credential to the
ordinary sealing path — the runner cannot tell it apart from a personal
forfait. When the attempt ends, the runner reports what it spent, which
charges the donor's ledger and frees their concurrency slot.

## Two sources, and why the difference matters

| | `oauth` — a subscription | `api_key` — a metered key |
|---|---|---|
| Billed | against a plan the lender already pays for | **per token, on the lender's own invoice** |
| The dollar figures are | ESTIMATES derived from tokens (`pkg/backend/cost`) — say "≈$1.80" | actual charges |
| A spend ceiling is | optional | **required** (lending one without a ceiling is an open invoice) |
| Real hard limit | the provider's usage window | the ceiling itself |
| Asked for | first | last — only when no already-paid-for plan can serve |

`CredentialSource.Metered()` is the single predicate every surface reads to
decide whether to hedge a figure or state it.

## What this is not

- **Not an invoice, for a subscription.** The CLI bills nothing per call, so
  those figures are estimates. They are the right unit for sharing fairly
  between donors; they are not what anyone is charged. A lent API key is the
  opposite case — there the figures are the charge.
- **Not a way around a provider's quota.** The hard limit remains the
  provider's own usage window. When one is hit, the donor is put to rest
  until its reset — that cooldown, not the dollar ceiling, is what actually
  protects a lender from being drained.
- **Not neutral on licensing.** A Claude or ChatGPT subscription is an
  **individual licence**. Pooling one across people is a deployment owner's
  decision. The mechanism is built so it stays an informed one — explicit
  pledge, ceilings set by the lender, kill switch effective at the next
  launch, and a per-donor log of every run served — but the decision itself
  is yours, and the same "dev/test, not production automation" caveat that
  the org-shared forfait already carries applies with more force here.

## Resolution order (where the pool sits)

`resolveAndSealCredentials`
([pkg/server/cloudpublisher/publisher.go](../pkg/server/cloudpublisher/publisher.go)):

1. **BYOK API keys** — team-scoped, personal-first.
2. **Personal OAuth forfait** — the run owner's own.
3. **Org OAuth forfait** — `secrets.OrgOwnerKey(tenant)`, the fallback for
   automated runs whose owner is a synthetic identity.
4. **Pool** — only when steps 1–3 produced **nothing at all**. Spending a
   contributor's lent credential while the tenant holds a usable key of its
   own would take a donation nobody needed.

Order within the pool (`poolWantOrder`): subscriptions first — `claude_code`
(a lent Claude forfait runs natively there), then `codex` — and only then
metered keys, provider by provider. Spending someone's real money is the
last resort even among donations.

A pledge only ever answers a request for its OWN source and ref: a
subscription never stands in for a metered key, because they are billed to
different places.

## Enforcement: the run's own budget is the ceiling

A grant carries the donor's **remaining allowance**, and the launch clamps
the run's `max_cost_usd` to it (`clampBudgetToGrant`). The engine then stops
the run on its own budget. Post-hoc ledger accounting is the final truth,
but it arrives too late to protect anyone — the clamp is what does.

The clamp only ever **lowers**: the tightest of (launch override, the bot's
declared budget, the allowance) wins. A donor who set no spend cap grants an
allowance of `0`, which means *no ceiling*, not "nothing left" (an exhausted
donor is never selected in the first place).

## Availability rules

A pledge is skipped when it is paused, unhealthy, inside a cooldown, outside
its sharing window, or when the requested bot is not in its allow-list.
Among the rest, selection ranks by the **fraction of what each donor
offered** that has been consumed today — so a modest pledge is not drained
before a generous one — with least-recently-served as the tie-break.

Two states are set from a run's outcome:

| Outcome | Effect on the donor |
|---|---|
| `ErrRateLimited{usage_window}` | Rest until the provider's reset (+1 min), or a bounded 1 h blind wait when no reset parsed. |
| `ErrAuthFailed`, twice in a row | Held out of the pool (`health: auth_failed`) until they reconnect. One blip never evicts anyone. |
| Anything else (including a plain workflow failure) | Nothing — a bot that failed on its own logic says nothing about the credential. |

## What a donor's admission costs them, and when it comes back

An acquisition consumes three things at once: a unit of the daily run
quota, a concurrency slot, and — until the run reports — the allowance it
was granted. That last one is what stops ten runs launched together from
each being handed the same "remaining" and spending it ten times over: a
live lease's `granted_cost_usd` counts against the next admission.

Which is why a spend cap is **shared across the slots still free** rather
than handed whole to the first run: promising it all away would deny every
sibling on cost, and the `max_concurrent_runs` a donor set could never
bind. A `$9/day` + `3 runs at a time` pledge grants `$3` per slot, summing
to exactly what was offered. A donor who allowed a single run at a time
has nothing to share, so that run still receives the whole allowance.

While every allowed slot is busy the donor reads **`serving`**, not
`exhausted` — the launches are refused, but nothing was given yet beyond
what those runs spend, and the state clears as they end. `exhausted` is
reserved for what the ledger really recorded.

They come back on three paths, and the distinction matters:

| Path | Slot + committed allowance | Daily run unit |
|---|---|---|
| `Report` — the run finished (however it ended) | freed | **kept** (it ran) |
| `Release` — the launch failed after the grant, so no run ever existed | freed | **returned** |
| sweeper — the pod died without reporting | freed | **kept** (it ran) |

`Report` closes the lease with a compare-and-set and charges the donor only
if it wins, so a redelivered report cannot debit twice. A resumed run that
**reported** *renews* its admission instead of taking a second one: it is
the same run, and charging it again would let one flaky run that resumes a
few times consume a contributor's whole day. Reporting is what earns the
renewal — the runner's spend hand-off is deferred and fires on every
outcome, `paused_waiting_human` included, so an ordinary pause/resume
always qualifies.

**One lease document per attempt**, never reused. A run that resumes —
onto the same donor or another — leaves every finished attempt's record
intact, because that record is the donor's only evidence for a charge
already on their ledger. Acquiring *supersedes* whatever was still marked
as serving that run (a pod that died without reporting leaves its lease
open), so a run never holds two open leases and "who is serving this run",
hence who gets charged, is never ambiguous. That supersede happens **before**
the admission is judged, so an attempt killed mid-flight is seen for what it
is — an attempt that never said what it spent — rather than as a prior
admission to renew against for free.

An attempt records whether it consumed a run unit, so releasing a **resume**
gives nothing back: it renewed rather than consumed, and refunding it would
mint quota out of a failed launch. An **abandoned** or **superseded**
attempt does not count as a prior admission either — nothing ever learned
what it spent, so the next attempt is admitted as new rather than renewing
indefinitely against a record that means nothing. When accounting is lost,
the daily run ceiling is the only guard left standing, and it must hold.

Concurrency and committed spend are **derived** from live leases, never
accumulated in a counter, so an abandoned run cannot permanently consume
either. The abandoned-lease sweeper
([pkg/server/credpool_sweeper.go](../pkg/server/credpool_sweeper.go)) closes
leases past their TTL every 2 minutes; no spend is charged for a run that
never reported, because inventing a figure would misreport a donation in
the direction that costs the donor.

**Two residual behaviours, stated plainly rather than papered over:**

- Two acquisitions that read a donor's state at the same instant can both
  be admitted, so a cap can be exceeded by one in-flight run under
  simultaneous launches. The committed-allowance accounting bounds the
  damage to a single overshoot rather than N; closing it entirely needs a
  distributed lock the deployment shape does not justify.
- The spend caps are per **calendar day / ISO week**, not per run. A run
  that starts at 23:00 and resumes after midnight can spend up to the daily
  cap on each of the two days. Each day honours the ceiling the donor set;
  a single long run is not separately bounded.

## Audience — who may draw on a pool

A pool belongs to an org. `Audience` is a **union of independent
predicates**, each separately togglable, with the strictest default:

| Field | Admits |
|---|---|
| *(nothing set)* | Only teams of the owning org. **The default.** |
| `teams: [...]` | Those team ids, wherever they live. |
| `orgs: [...]` | Every team under those orgs. |
| `contributors: true` | Any user with an **active** pledge, wherever they launch from — the reciprocity dial ("lend to borrow"). Pausing your own sharing stops your borrowing too. |
| `all_teams: true` | Every team on the instance. |

Selection scans the enabled pools and applies each audience, so a pool that
opens itself to another org is genuinely reachable from there. Every pool
that admits the requester is tried, own-org first: running a pool of your
own must not exclude you from a community one when your own donors are all
cooling, exhausted or out-of-hours.

## Operator cookbook

Admission is handled by the **existing GitHub team-gating**, not by anything
in this package: create an orgsso GitHub row mapping the contributors'
GitHub team to the iterion team you want them to land in
([pkg/auth/oidc_service.go](../pkg/auth/oidc_service.go) →
`FindGitHubGrantingOrgs`). An allow-listed contributor then signs in with
GitHub and is already a member — with the default audience they can both
lend and borrow, with no extra setup.

```sh
# A contributor lending a metered key instead of a subscription:
iterion remote api-keys create --provider anthropic --name mine --from-file ~/key
iterion remote pool share --source api_key --ref anthropic   --key-id <id from `iterion remote api-keys list`> --max-usd-day 3

# 1. Operator: create/enable the pool for the org (default audience:
#    the org's own teams only).
iterion remote api PUT /api/teams/<team-id>/pool --data '{"enabled":true}'

# Widen it only if you mean to — this lets more runs spend contributors'
# personal subscriptions:
iterion remote api PUT /api/teams/<team-id>/pool \
  --data '{"enabled":true,"audience":{"contributors":true}}'

# 2. Contributor: connect the subscription (once), then pledge it.
iterion remote api POST /api/me/oauth/claude_code/authorize/start
iterion remote pool share --source oauth --ref claude_code \
  --max-usd-day 5 --max-usd-week 20 --max-runs-day 10 --max-concurrent 1 \
  --from-hour 19 --to-hour 8          # optional: lend it overnight

# 3. Contributor: watch, pause, withdraw.
iterion remote pool status
iterion remote pool history           # every run your quota served
iterion remote pool pause             # keeps your terms for later
iterion remote pool withdraw

# 4. Operator: who is lending, and how they are doing.
iterion remote pool donors
```

Studio equivalents: **Account settings → Share my quota** for a contributor,
**Team → Credential pool** for an operator.

## REST surface

| Endpoint | Who | Purpose |
|---|---|---|
| `GET /api/me/pool` | donor | Own pledges + today/this-week consumption |
| `PUT /api/me/pool/{source}/{ref}` | donor | Create/update terms. Clears a parked health state ONLY when the credential was genuinely re-connected since — the signal is the record's `created_at` (stamped by connect/paste), never `updated_at`, which the token-refresh worker bumps hourly for a subscription the provider may have revoked |
| `DELETE /api/me/pool/{source}/{ref}` | donor | Withdraw |
| `GET /api/me/pool/history` | donor | The runs this quota served |
| `GET /api/teams/{id}/pool` | member | Pool policy. The donor roster is included only for org admins — who lends, and how much, is the contributors' business |
| `PUT /api/teams/{id}/pool` | **org** admin | Enable/disable + audience. The pool document is org-keyed, so this is an org-level decision and is audited on the org |

A donor's credential is never returned by any of these — it stays sealed in
`pkg/secrets` and is only ever unsealed into a run bundle.

## Gotchas that will cost time

- **A pledge for a credential you do not hold is refused, not stored** — so
  nobody believes they are contributing when they are not. 409 for a missing
  subscription, 404/403 for a key that is absent or not yours.
- **Only a PERSONAL key can be pooled.** A team-scoped key is the team's to
  spend; letting one member lend it would hand the whole team's credential
  to the pool. Checked at pledge time AND again at every acquisition, since
  a key can be re-scoped afterwards.
- **A metered pledge must carry a spend ceiling.** Refused otherwise: it
  would be an open invoice on the lender's own account.
- **`ITERION_FORBID_SUBSCRIPTION_OAUTH=1` does not disable the pool.** That
  guard only covers `claw`/`pi` (`secrets.GuardSubscriptionOAuth`); a lent
  Claude forfait still works on `claude_code`, which is its native path.
  Correct, and worth knowing before you conclude the pool is off.
- **The spend signal must be live.** Delegate backends only report cost
  because `DelegateInfo.CostUSD` rides the `delegate_finished` event into
  the runner's per-run totals. If a future change drops that field, every
  donor silently reads "0 consumed" and no ceiling ever trips — the
  regression tests for this are in
  [pkg/runner/metrics_test.go](../pkg/runner/metrics_test.go).
- **Spend is charged to the day that ADMITTED the run**, not the day it
  finished, or a run crossing midnight would leak past its donor's daily cap.
- **A contribution joins the donor's OWN org's pool, or none.** There is no
  "the instance runs one pool, use that" fallback: on a multi-tenant
  instance it would enrol a personal subscription into a stranger's org.
  A user whose org has no pool is told so.
- **Disconnecting a subscription withdraws the pledge with it.** Consent is
  given for a specific connected credential; leaving the terms behind would
  let them rebind to whatever is connected next under the same key. The
  cascade is best-effort — a degraded pool store never blocks a
  disconnection, and a pledge left behind is parked at the next acquisition.
- **The estimate is a floor, and one case makes it lower than you'd think.**
  A delegate call that is retried for a schema mismatch reports only the
  first attempt's cost (the `delegate_finished` hook fires once) — the same
  under-count the token figures have always had. Donors are never
  over-charged; they may be slightly under-charged.
- **Cloud only.** The pool lives on the launch path of the cloud publisher;
  a local `iterion run` resolves credentials from the local store and never
  consults it.

## Related

- [cloud-llm-credentials.md](cloud-llm-credentials.md) — the other three tiers.
- [quotas-and-limits.md](quotas-and-limits.md) — the per-org run/cost caps,
  which are unrelated and stack on top.
- [backends.md](backends.md) — which backend can use which credential kind.
