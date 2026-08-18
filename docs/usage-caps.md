# Usage caps — stop below the provider's wall

An LLM subscription ("forfait") meters two rolling windows, five hours and
seven days, and refuses every call once one is exhausted. iterion already
survives that refusal: the run parks and a durable retry resumes it when the
window reopens ([scheduling.md](scheduling.md#retry)). What it could not do
was stop *before* the wall — and the wall is rarely where an operator wants
to be, because the same subscription usually pays for their own interactive
work. A fleet of bots that drives it to 100% takes the human down with it.

A **usage cap** is a percentage the operator chooses, enforced from the
provider's own telemetry.

```sh
# The recommended posture. Two variables is the whole configuration.
export ITERION_USAGE_CAP_5H_PCT=85
export ITERION_USAGE_CAP_WEEK_PCT=75
```

Nothing is capped by default: an unset cap leaves runs bounded only by the
provider, which is the historical behaviour.

## The two postures

The windows fail differently, so they default to different postures.

| Window | Default mode | What it does |
|---|---|---|
| five-hour | `soft` | Never interrupts work in flight; no NEW run starts |
| weekly | `hard` | Stops the run where it stands, and starts nothing new |

A five-hour window refills soon, so killing a half-finished run to save
minutes of quota trades a lot for a little. A weekly window that runs out on
a Tuesday is a dead week, and the run that would have finished is worth less
than the four days of headroom it would have eaten.

Either posture ends a capped run the same way: `failed_resumable`, with a
durable retry armed for the instant the window reopens. **A capped run is not
a lost run; it is a run that waits.** The cap reuses the provider-refusal
path wholesale rather than inventing a recovery of its own.

## Configuration

| Variable | Values | Default |
|---|---|---|
| `ITERION_USAGE_CAP_5H_PCT` | `0`–`100` (`0`/unset = no cap) | unset |
| `ITERION_USAGE_CAP_5H_MODE` | `off` \| `soft` \| `hard` | `soft` |
| `ITERION_USAGE_CAP_WEEK_PCT` | `0`–`100` (`0`/unset = no cap) | unset |
| `ITERION_USAGE_CAP_WEEK_MODE` | `off` \| `soft` \| `hard` | `hard` |
| `ITERION_USAGE_CAP` | `off` disarms both caps | unset |

A malformed value **refuses to start** rather than falling back to no cap:
every wrong answer here fails open, and a guard silently disabled by a typo
is the failure the feature exists to prevent.

There is deliberately **no per-run flag and no DSL field**. The cap protects
a credential and the deployment that owns it, not a run — a bot able to lift
the guard would not be a guard. `ITERION_USAGE_CAP=off` is the escape hatch,
and it belongs to whoever runs the deployment.

### Which windows a cap governs

The provider reports six windows. `five_hour` is the 5h cap; `seven_day`,
`seven_day_opus`, `seven_day_sonnet` and `seven_day_overage_included` are all
governed by the weekly cap — a run refused on the per-model weekly sub-limit
is refused, whatever the all-models number says. `overage` is **not** capped
here: it is metered money, not subscription quota, and `--max-cost-usd` is
what bounds money.

## Where the numbers come from

Claude Code emits a `rate_limit_event` on its stream-json output whenever the
provider's usage numbers move, carrying `{status, rateLimitType, utilization,
resetsAt}`. `utilization` is a fraction (0..1); `resetsAt` is Unix seconds.
That is the only place a subscription's remaining headroom is observable
from outside the provider — the metered API returns `anthropic-ratelimit-*`
headers, but a CLI-driven session hides them, and `GET /api/oauth/usage`
requires a `user:profile` scope that a `claude setup-token` credential does
not carry.

Consequences worth knowing:

- The cap is **claude_code-shaped today**. Other backends have no equivalent
  telemetry surface; a run on `claw` or `pi` is not capped.
- A reading **expires at its own reset instant**. Past it the window has
  rolled over and the number describes a window that no longer exists, so a
  stale reading stops blocking by itself — no sweeper, no TTL to tune.

## What it looks like when it fires

- a `usage_cap` event on the run's timeline (`window`, `percent`, `cap`,
  `mode`, `stopped`, `resets_at`) — the only thing that distinguishes "the
  provider refused us" from "we stopped ourselves";
- a warn line in `run.log`: `usage cap: seven_day window at 76% ≥ 75%
  (week, hard), resets 2026-08-18T21:00:00Z`;
- the run's own error, carrying the same sentence;
- then the ordinary wait: `run_retry_scheduled` with `retry_after` at the
  window's reopening, and `run_auto_resumed` when it fires.

## What a cap does NOT stop

**A run is only refused in advance when it could not possibly avoid
spending.** The cap governs a model subscription, and it blocks at launch
only if EVERY path from the workflow's entry to a terminal passes through
something that can call a model — an agent, a judge, an `llm` router, a
model-answered human node, an agent recovery rung, a subbot, a supervisor.

If any model-free path exists, the run starts and the **mid-run** guard stops
it at the actual call. That costs a pod and a clone in the worst case, and it
is the price of not refusing work that would never have been billed.

The distinction is not cosmetic. A zero-LLM run is often the half of a bot
that *gathers* — and gathered material is not recoverable by retrying later.
Vigie's `collect` mode polls feeds into a queue; a feed serves a short window
and does not remember what nobody fetched, so every refused collect is
material permanently gone, while the `digest` half it feeds waits on a queue
that stays empty. Between 2026-08-17 and 2026-08-18 that is exactly what
happened.

**Why "every path" and not "contains a model node".** A two-mode bot carries
both halves in ONE `.bot`: Vigie's `collect` polls feeds with tool nodes,
`digest` synthesises with an agent, and a router picks between them from a
field the `plan` node produces at RUNTIME — not from a var, so no launch-time
analysis can predict it. A predicate asking merely "does this graph contain
an agent?" answers yes for both halves and refuses the collect half too. That
is exactly the defect that silenced the production veille, and shipping the
weaker predicate first did not fix it.

The predicate is [`ir.Workflow.AlwaysReachesLLM`](../pkg/dsl/ir/uses_llm.go),
walking forward from the entry and treating a model-calling node as a wall:
reaching a terminal without hitting one proves a model-free path exists. It
stays conservative in the direction that matters — an unwalkable graph, a
missing entry, a dangling edge or a supervisor all answer "true", keeping
today's refusal rather than opening the gate. (`UsesLLM` still exists for the
plain "does this graph contain one?" question; the two deliberately disagree
on a two-mode bot, which is the whole point.)

Both pre-flights apply it: the cloud runner's (which has the compiled
workflow in hand) and the local launch path's (which compiles only when the
cap is blocking, so the common case pays nothing). The mid-run guard stays
armed in both cases, so a workflow that turns out to spend anyway is still
stopped at the call.

## Cloud

Every pod sees only its own session, so readings are shared through the
`usage_windows` collection (one document per credential and window, newest
wins). A claimed run consults it **before** cloning a repo or starting a
container and parks for free when there is no headroom — otherwise each pod
would rediscover the ceiling by spending against it.

The ledger is keyed per credential: a tenant that brought its own
subscription is never blocked by what another tenant spent, and runs falling
back to the deployment's own credential share one meter, which is correct —
they really are one subscription.

The pre-flight **fails open** on every uncertainty (no ledger, an unreadable
ledger, nothing measured yet, a rolled-over reading). A cap exists to protect
a subscription from a fleet, not to strand the fleet on a bookkeeping outage;
the in-run guard still stands behind it, so failing open costs one call.

A local CLI or studio run keeps the same policy on a process-local ledger,
and a second run in the same process starts already knowing what the first
one measured. There the launch is **refused outright** (`429`, with the
reason and the reopening instant) rather than parked: the operator is
present, so an immediate answer beats a queued one.

## Emergency brake

The cap governs iterion. To stop **everything** on a cloud deployment,
including runs already claimed, freeze the runner instead — queued work
piles up in NATS and resumes intact on unfreeze:

```sh
kubectl -n iterion annotate scaledobject iterion-runner \
  autoscaling.keda.sh/paused-replicas="0" --overwrite   # freeze
kubectl -n iterion annotate scaledobject iterion-runner \
  autoscaling.keda.sh/paused-replicas-                  # thaw
```

Cancel in-flight runs *before* freezing: a pod killed mid-run ends
`failed` (not resumable), where a cancel checkpoints it.
