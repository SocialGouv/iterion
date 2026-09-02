# The outcome router — `ITERION_OUTCOME_ROUTER`

The instance-side answer to "a run produced work and nobody routed it": a
terminal run that carries a launch-frozen `RoutingPolicy` is **decided by its
own contract** — merge, relaunch, or escalate — once per terminal episode,
with a durable, auditable decision registry as the idempotence.

A run with **no** contract is never routed. The switch below only decides
whether the reactor runs at all; what it decides is the contract's business.

## The contract — `routing_policy`

The contract says what success means for THIS run, in the run's own
vocabulary. It is supplied at launch as the `routing_policy` object on the
run-launch body (`POST /api/runs`, [pkg/server/runs_launch.go](../pkg/server/runs_launch.go)),
validated and hashed there, and persisted on the run document. There is no
CLI flag and no `.bot` block: the contract belongs to whoever *launched* the
run, not to the workflow.

```json
{
  "routing_policy": {
    "version": 1,
    "success_when": "outputs.gate.converged && !outputs.gate.needs_human",
    "block_when": ["outputs.campaign.is_code_bug"],
    "allowed_actions": ["merge"],
    "merge_into": "main",
    "merge_strategy": "squash",
    "max_relaunches": 0
  }
}
```

| Field | Meaning |
|---|---|
| `version` | Schema version of the contract itself. Must be `1` at launch; a run carrying a version newer than the reader understands **escalates** rather than honouring half a contract (`routing.CurrentPolicyVersion`). |
| `success_when` | **Required.** Must evaluate to boolean `true` on the terminal checkpoint for the run to be an auto-merge candidate. |
| `block_when` | Any expression that evaluates `true` — or fails to evaluate — blocks the merge whatever `success_when` says. This is where a bot's explicit blockers are quoted (a pending re-baseline, a re-anchor demand, `is_code_bug`). |
| `allowed_actions` | Bounds what a consumer may do automatically: `merge`, `relaunch`, `resume`. **An empty or absent set allows nothing** — fail-closed. Anything not listed escalates. |
| `merge_into` | Branch a success lands on (`""` = the run's `RepoRef` default). Validated through the repo's one canonical branch-name rule (`git.ValidateBranchName`), because the value arrives in an HTTP body and feeds a git operation. |
| `merge_strategy` | `squash` (the default) or `merge`. |
| `max_relaunches` | Caps automatic fresh relaunches. **`0` — the omitempty default — means "never relaunch automatically"**: a contract that lists the `relaunch` action but leaves the cap at zero has granted nothing, and escalates. |
| `hash` | Not supplied by the caller: the launch computes sha256 over the canonical JSON of every field above (the action set is sorted, so `[merge, relaunch]` and `[relaunch, merge]` are the same contract). Every decision records it, so an audit can prove WHICH contract decided. |

### The expression grammar is deliberately tiny

`success_when` / `block_when` are boolean algebra over **output refs only**:
`outputs.<node>.<key>…` combined with `!`, `&&`, `||`. Nothing else — no
comparison, no literal, no `vars.` / `input.` / `artifacts.` / `loop.`
namespace. A contract reads the gates the bot *published* on its terminal
checkpoint, and only those.

Strictness is the point ([pkg/routing/policy.go](../pkg/routing/policy.go)):

- **Every ref is resolved and type-checked individually before evaluation.**
  An absent path, a typo'd node, a renamed key or a non-bool value is a
  contract violation, not a `false`. Checking only the final result would let
  `!outputs.gone.flag` read as `true` through `!`'s truthy coercion.
- **Escalation is the default.** No contract, an unreadable expression, a
  blocker that held, a policy that allows nothing, a hash that does not match
  its own stamp — all land on `escalate`, never on `merge`.
- `success_when` true → `merge` if allowed, else escalate.
  `success_when` false → `relaunch` if allowed *and* funded by
  `max_relaunches`, else escalate.
- **Only a `finished` run may land its work.** A `cancelled` or
  `failed_resumable` run can carry a checkpoint whose outputs still satisfy
  `success_when` — the gate spoke on an *earlier* pass — so the reading
  downgrades it to escalate. Same for a run that is not terminal at all: the
  precondition lives in the evaluator, not only in the reactor that calls it.

### Refused at launch, not discovered at the terminal

`Service.Launch` is the one choke point every surface funnels through
([pkg/runview/service_launch.go](../pkg/runview/service_launch.go)), and it
refuses a contract that could not work:

- a malformed expression, a namespace outside the grammar, an unknown action,
  a negative `max_relaunches`, an unknown merge strategy, an unusable
  `merge_into` → `400`;
- a ref naming a **node the workflow does not have**, or a **field that node
  never publishes** (checked against the node's declared output schema; nodes
  with a dynamic output shape are not statically checkable and pass).

That last one is the interesting refusal. A blocker on a field the bot never
publishes passes the grammar, survives the launch, and then reads
"unreadable → escalate, forever" at the terminal — silently disabling the
automation the contract exists to allow. It is a launch error instead.

### Frozen means frozen

The contract is resolved, validated and hashed at launch and **never re-read
from a mutable source afterwards** — re-reading a team or repo setting at the
terminal would let the contract of already-produced work change
retroactively. `SaveRun` enforces it: the persisted contract wins over
whatever a later save carries, and the first-write window closes as soon as
the run has produced ([pkg/store/store_run.go](../pkg/store/store_run.go)).
Resume replays it from the run document, like the model pins.

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
| `merge` | Only for a `finished` run with a banked branch; goes through the ordinary merge-claim machine (`PerformMergeCtx`), re-reading the run under the claim first. Failure → registry row `failed` + `route_action_failed` ops alert; the bounded re-claim retries. |
| `relaunch` | **Not wired yet**: the row records `failed` ("execution not enabled") and a `route_action_failed` ops alert asks the operator to act. |
| `escalate` (the default) | The alert **is** the action: a `route_escalated` ops alert (webhook + errtrack, deduped per episode). A delivery failure does **not** burn the attempt cap: the row stays `claimed`, the 15-min lease steal re-delivers, and once the steal cap is spent the exhausted-claim path re-offers the alert every sweep until a channel takes it — then settles the row. A webhook outage delays an escalation; it can no longer silence it. |

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
