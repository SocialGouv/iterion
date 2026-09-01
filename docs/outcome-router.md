# The outcome router — `ITERION_OUTCOME_ROUTER`

The instance-side answer to "a run produced work and nobody routed it": a
terminal run that carries a launch-frozen `RoutingPolicy` is **decided by its
own contract** — merge, relaunch, or escalate — once per terminal episode,
with a durable, auditable decision registry as the idempotence.

## Switch and activation

- **`ITERION_OUTCOME_ROUTER=on`** on the **server** deployment activates the
  router (any other value = off). It rides the Deployment manifest so every
  replica reads the same value: turning it ON is an explicit rollout.
- **Turning it OFF needs no rollout**: the sweep re-reads the env every tick
  and the offer path checks it per run, so `kubectl set env` (or an emergency
  edit) stops new decisions within a minute. A merge already in flight
  finishes.
- **Activation watermark.** The first replica that starts with the switch on
  persists the activation instant (first-writer-wins,
  `EnsureRouterWatermark`). The sweep never reaches behind it, so flipping
  the switch on does **not** retro-route up to a lookback's worth of
  historical terminals (without it: up to a full 200-run batch of merges
  pushed to the forge in the first minute). Terminals that predate activation
  stay the operator's. The watermark survives restarts and is shared by all
  replicas; on the local store it is `<store>/router_watermark.json`, on
  Mongo the `router:watermark` sentinel row in `run_route_decisions`.
- The router requires the store's decision-registry capability
  (`RouteDecisionStore`); without it the router refuses to start — never run
  unclaimed.

## What it decides, and what happens

Two independent offer paths — the run-outcome event bus (fast, lossy) and a
60s sweep over the store (the source of truth; six terminal paths never
publish an event). Both converge on one decision per `(run, outcome_seq)`
episode via the registry claim.

| Decision | Action |
|---|---|
| `merge` | Only for a `finished` run with a banked branch; goes through the ordinary merge-claim machine (`PerformMergeCtx`), re-reading the run under the claim first. Failure → registry row `failed` + `route_action_failed` ops alert; the bounded re-claim retries. |
| `relaunch` | **Not wired yet**: the row records `failed` ("execution not enabled") and a `route_action_failed` ops alert asks the operator to act. |
| `escalate` (the default) | The alert **is** the action: a `route_escalated` ops alert (webhook + errtrack, deduped per episode). If delivery fails on every channel, the row finishes `failed` so the bounded re-claim re-delivers. |

Exclusions the offer enforces: cancelled runs (an operator's stop is never
auto-routed), paused runs, runs owned by a platform continuation
(`redelivery_pending` / `retry_armed`), pre-episode runs (`outcome_seq == 0`),
already-merged runs, and a `merge` verdict on a non-`finished` run (a stale
checkpoint is not a verdict — downgraded to escalate).

## The registry (audit + idempotence)

- One row per episode, unique on `(run_id, outcome_seq)`; states
  `claimed → succeeded | failed`.
- A `claimed` row older than the 15-min lease is **stolen** (the claimant
  died); a `failed` row is retried — both bounded by the 3-attempt cap. A
  poison episode that kept killing its claimants ends with an exhausted row
  and a `route_action_failed` alert instead of re-arming forever.
- The sweep anti-joins settled episodes (succeeded, or failed at cap) so
  decided terminals never clog the 200-run batch head.
- **Read it back**: `GET /api/runs/{id}/route-decisions` (newest episode
  first — decision, reason, policy hash, attempts, outcome), or on the local
  store `<store>/runs/<id>/route_decisions.json`.

## Alerting

Router alerts ride the operator-alert dispatcher
([docs/observability.md](observability.md), `ITERION_ALERTS_WEBHOOK_URL` +
episode-claim store): kinds `route_escalated` and `route_action_failed`,
deduplicated per episode, released-for-retry when every channel fails. A
server without ops alerts configured (local studio) keeps the registry row
and the Warn log as its whole surface.

## Rollout procedure

1. Deploy the release carrying the router **with the switch off** (default).
2. Verify ops alerts are live ("operator alerts enabled" in the server log).
3. Set `ITERION_OUTCOME_ROUTER=on` on the server Deployment and roll it. The
   watermark pins to this instant; only runs that terminate afterwards are
   routed.
4. Watch the first decisions: `route_escalated` alerts on the ops channel,
   rows under `/api/runs/{id}/route-decisions`, and
   `server: outcome router attached` in the log.
5. Emergency stop: set the env to `off` (no rollout needed — see above).
