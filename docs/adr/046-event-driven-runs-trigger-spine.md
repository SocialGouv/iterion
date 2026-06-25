# ADR-046 — Event-driven runs: a unified trigger spine (board events first)

Status: **accepted (phase 1)** (2026-06-25) — the spine (`pkg/trigger` +
`pkg/eventbus`) and the first source (**iterion board events**) ship
end-to-end, local + cloud-ready. Launch-path convergence, the studio
Automations view, forge-derived provisioning, and the remaining sources
(run-completion, scheduled/cloudsched, git-forge refactor, custom ingress)
are designed here and staged as follow-ons.

## Context

Iterion could already launch a bot run from several triggers, but each was a
**siloed implementation** with its own config model, storage, and launch path:

- **Host-crontab schedules** — `pkg/cli/schedule.go` → `RunRun()` directly.
- **Dispatcher tracker-polling** — `pkg/dispatcher/` polls native/github/forgejo
  every 30s → `Runner.Dispatch(DispatchSpec)`.
- **Inbound forge webhooks** — `pkg/webhooks/` + `pkg/server/webhooks_*.go` →
  `invocation_dispatch.go` → `runview.Launch`.
- **Per-bot `invocations:` DSL** — `bundle.Invocation{Kind: forge|command|
  schedule|board}` — the closest existing unifying contract, but only the
  webhook/forge path consumed it.

The costs: **three non-converged launch paths** (`RunRun`, `runview.Launch`,
`Runner.Dispatch`); an iterion **board event** could only fire a bot via the
30s poll; no **run-completion → run** chaining; `pkg/cloudsched` existed but
never fired; and no first-class **custom-integration** trigger beyond the
single generic webhook.

## Decision

Two composing layers, plus one canonical event channel.

1. **Capability stays where it is.** `bundle.Invocation` remains the
   bot-authored "what surfaces can fire me" — no repo/tenant/cron knowledge
   enters a manifest. `InvocationKindBoard` gains an optional `board:` block
   (`on` / `to_states` / `all_labels`) with fail-fast parse validation.

2. **A new binding layer — `pkg/trigger`.** `trigger.Subscription` binds
   `(event filter) → (bot launch into a target)`: one row per
   `(tenant, repo, bot, invocation-kind)`. The pure `trigger.Matcher` is the
   union of every legacy family's allowlists (sources/kinds/actions/repos/
   authors/labels/subject-states), so each old config maps on without losing
   fidelity. `SubscriptionStore` mirrors `forge.RepoIntegrationStore`
   (in-memory for local single-host, Mongo for cloud), with `ListByRepo` /
   `ListByBot` powering the "by repo / by bot" surfaces and `ListCandidates`
   the evaluator hot path.

3. **A new internal event bus — `pkg/eventbus`.** `Bus{Publish, Subscribe}`
   with two impls: `InProcBus` (local, lossy fan-out à la
   `runview.EventBroker`) and `NATSBus` (cloud, a **new** `ITERION_EVENTS`
   stream / `iterion.events.*` subject — deliberately separate from the
   `iterion.queue.runs` work queue, because events are at-least-once fan-out
   notifications, not exactly-once locked work). The same `trigger.Evaluator`
   consumes the bus identically in both modes; only the `Bus` impl is injected
   differently.

The packages avoid an import cycle by layering: `eventbus` imports `trigger`
(for `Event`/`Matcher`); `trigger` imports neither `eventbus` nor `runview`.
The evaluator's `Handle(ctx, Event) error` matches `eventbus.Handler`, so the
wiring layer (`pkg/server`) connects them; the board source publishes through a
tiny local `trigger.Publisher` interface that `Bus` satisfies structurally.

### First source — iterion board events (the proof)

A native-board card created/moved/labeled fires a bot **without** the 30s wait:

- **Ingress reuses the existing tail.** `pkg/trigger/board_source.go` subscribes
  to `native.Store.Subscribe` (the writer-agnostic events.jsonl tailer) — no new
  store append point — normalizes `native.Event` → `trigger.Event{Source:
  board, Kind: card.*}`, and publishes to the bus. Lifecycle copied from
  `pkg/server/watch_coordinator.go`.
- **Effect is "promote the card," never launch directly.**
  `NativeBoardEffect.Promote` stamps the matched card's `Bot`/`BotArgs` (and,
  if the subscription names one, an eligible state). The dispatcher's existing
  `tracker.Claim` stays the **sole launch authority** — so the event fast-path
  and the poll safety-net are **structurally unable to double-launch**, and the
  promote is idempotent (a card already pinned to the bot is left untouched, so
  an event storm converges). After a real change it nudges the dispatcher
  (`Manager.Refresh()`) so the card dispatches now instead of at the next poll.
- **The poll is supplemented, not replaced.** fsnotify drops under back-pressure
  and the tail starts at EOF with no replay, so the 30s poll remains the
  reconciliation net; the dispatcher becomes one (privileged) bus consumer.

Wired in `pkg/server` (`StartTriggerCoordinator`, reached by both `iterion
studio` and `iterion dispatch`); discovery-driven via
`cli.buildLocalTriggerStore` (a bot opts into event-driven promotion purely by
adding a `board:` block — zero engine/CLI edit). REST CRUD at
`/api/v1/triggers`, gated by `server_info.triggers_enabled`.

## Why not …

- **Reuse `runview.Launch` from `pkg/trigger` directly?** It would cycle once
  run-completion emits events back into the bus from `runview`. The `Launcher`
  interface lives in `trigger`; its `runview`-wrapping impl lives in the wiring
  layer.
- **Reuse the runs queue for events?** Different delivery semantics; reusing it
  would force runner pods to filter events and corrupt queue-position math.
- **Put repo/tenant/cron on the manifest?** That conflates a bot capability with
  a deployment binding; the orchestrator generates subscriptions *from*
  invocations instead.
- **Replace the dispatcher poll?** The CAS-claim is the repo's hardest-won
  correctness; the spine feeds it events and consumes its launches, it does not
  absorb its scheduler.

## Consequences / staged follow-ons (same spine)

Each later source = "a source adapter that publishes a `trigger.Event` + an
effect choice (promote-card vs direct launch)":

- **Launch-path convergence** — `serviceLauncher` over `runview.Service` + four
  per-launch `LaunchSpec` fields (`WorkDir`, `ExtraObservers`, `DailyCap`,
  `SourceRef`) to preserve the dispatcher's workspace/stall/cap/retry
  invariants; converge only the *execution step* of `EngineRunner.Dispatch`.
- **Run-completion chaining** — emit `run.finished`/`run.failed` beside
  `completionNotifier.FireForRun`; direct-launch effect.
- **Scheduled** — wire `cloudsched`/host-crontab as a timer source emitting
  `SourceSchedule`; fold `ScheduledBot` into `Subscription{Invocation: schedule}`.
- **Git-forge** — refactor `webhooks_*.go` to publish `SourceForge` after
  auth/HMAC; parity-gate via the `webhooks.Delivery` audit, then retire the
  inline launch.
- **Custom ingress** — a signed `POST /api/.../triggers/emit`.
- **Studio Automations view** + cloud team-scoped REST + a file-backed local
  subscription store (the memory store is rebuilt from manifests each start).

## Key files

New: `pkg/trigger/{event,subscription,store,memstore,mongostore,evaluator,
launcher,board_source}.go`, `pkg/eventbus/{bus,inproc}.go`,
`pkg/server/{trigger_coordinator,triggers_routes}.go`, `pkg/cli/trigger.go`.
Modified: `pkg/bundle/manifest.go` (`InvocationBoard`), `pkg/queue/nats/nats.go`
(`ITERION_EVENTS`), `pkg/dispatcher/manager.go` (`Refresh` nudger),
`pkg/server/{server,server_info}.go`, `pkg/cli/{studio,dispatch}.go`.
