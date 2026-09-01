# The outcome router — `ITERION_OUTCOME_ROUTER`

The instance-side answer to "a run produced work and nobody routed it": a
terminal run that carries a launch-frozen `RoutingPolicy` is **decided by its
own contract** — merge, relaunch, or escalate — once per terminal episode,
with a durable, auditable decision registry as the idempotence.

## Switch and activation

- **`ITERION_OUTCOME_ROUTER=on`** on the **server** deployment activates the
  router (any other value = off). It rides the Deployment manifest so every
  replica reads the same value: turning it ON is an explicit rollout.
- **Emergency stop**: `kubectl set env deploy/<server> ITERION_OUTCOME_ROUTER=off`.
  On Kubernetes this mutates the Deployment and therefore triggers a rolling
  restart — a process env is immutable, so the per-tick env re-read only
  matters outside k8s (local `iterion server`, tests). The stop is still
  fast (no image build, pods recycle in seconds) and a merge already in
  flight finishes.
- **Re-enabling catches up the OFF window.** The watermark records the FIRST
  activation and never moves, so after an off/on cycle the sweep processes
  terminals that landed while the router was off (bounded by the 24h
  lookback). That is deliberate — every rolling deploy is a short off/on
  cycle, and skipping the gap would drop the runs that terminated
  mid-restart. But after a long DELIBERATE stop, the operator may not want
  the catch-up (runs they consciously left unrouted): before re-enabling,
  advance the watermark to "now" — Mongo:
  `db.run_route_decisions.updateOne({_id: "router:watermark"}, {$set: {claimed_at: new Date()}})`;
  local store: rewrite `<store>/router_watermark.json`'s `activated_at`.
  Cancelled runs are never routed in any case.
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
| `merge` | Only for a `finished` run with a banked branch; goes through the ordinary merge-claim machine (`PerformMergeCtx`), re-reading the run under the claim first. A transient failure → registry row `failed` + `route_action_failed` ops alert, and the bounded re-claim retries. A **content conflict** → row `requires_action`: `PerformMergeCtx` leaves `merge_status=conflicted` for the studio's conflict resolver, and retrying would run git against the conflicted index, fail on a non-conflict error and overwrite that status — so the router stops and the human continues. |
| `relaunch` | **Not wired yet**: the row records `requires_action` ("execution not enabled") and a `route_action_failed` ops alert asks the operator to act. |
| `escalate` (the default) | The alert **is** the action: a `route_escalated` ops alert (webhook + errtrack, deduped per episode). A delivery failure does **not** burn the attempt cap: the row stays `claimed`, the 15-min lease steal re-delivers, and once the steal cap is spent the exhausted-claim path re-offers the alert every sweep until a channel takes it — then settles the row. A webhook outage delays an escalation; it can no longer silence it. |

Exclusions the offer enforces: cancelled runs (an operator's stop is never
auto-routed), paused runs, runs owned by a platform continuation
(`redelivery_pending` / `retry_armed`), pre-episode runs (`outcome_seq == 0`),
already-merged runs, and a `merge` verdict on a non-`finished` run (a stale
checkpoint is not a verdict — downgraded to escalate).

**The bank deadline.** A terminal status lands *before* the worktree's bank
push (measured 10+ minutes on large repos), so a `merge` verdict on a run
with no `final_branch`/`final_commit` is refused **silently** — deciding
there would burn the episode while the branch is still on its way. That
wait is bounded at **30 minutes** past the terminal (`finished_at`, else
`updated_at`): after it the empty bank *is* the answer — the run committed
nothing, or its push never landed — and the router escalates, naming the
missing bank. Without the bound such a run was re-offered every 60s with
no row and no log line until it aged out of the lookback, which is the
router's own motivating incident; it also sat at the head of the ascending
sweep batch for the whole lookback.

## The registry (audit + idempotence)

- One row per episode, unique on `(run_id, outcome_seq)`; states
  `claimed → succeeded | failed | requires_action`.
- `failed` is the **retryable** terminal; `requires_action` is the one no
  retry can move and a human must (a merge conflict, an unwired decision).
  It is not re-claimable, on either backend.
- A `claimed` row older than the 15-min lease is **stolen** (the claimant
  died); a `failed` row is retried — both bounded by the 3-attempt cap. A
  poison episode that kept killing its claimants ends with an exhausted row
  and a `route_action_failed` alert instead of re-arming forever.
- A re-claimed row is in flight again: it carries none of the previous
  attempt's outcome (`error` and `finished_at` are cleared).
- The sweep anti-joins settled episodes (succeeded, requires_action, or
  failed at cap) so decided terminals never clog the 200-run batch head.
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
