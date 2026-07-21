# ADR-046 — Event-driven runs: a unified trigger spine (board events first)

Status: **accepted (phases 1–4)** (2026-06-25) — shipped end-to-end, local +
cloud-ready: the spine (`pkg/trigger` + `pkg/eventbus`); **board events**
(board-mode promote); a **direct-mode launcher** over `runview.Service`;
**run-completion chaining** (`run.finished`/`failed`/`cancelled`);
**scheduled** (in-process `Scheduler` over schedule-kind subscriptions, the
local twin of cloudsched); **git-forge events on the bus** (the shared
webhook launch tail emits a `SourceForge` event, observational via the
`launched_run_id` marker so it can't double-launch); and **custom ingress**
(`POST /api/v1/triggers/emit` injects a `SourceCustom` event onto the spine —
the first-class extensibility point for arbitrary external systems). The
**studio Automations view** (`/triggers`, gated on `triggers_enabled`) lists
and manages every subscription by repo/by bot with a create form per source.
Staged follow-ons: the forge *cutover* (spine becomes the forge launcher,
inline path retired behind a parity flag), forge-derived subscription
provisioning, and dispatcher `EngineRunner` convergence.

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

- **Launch-path convergence** — DONE for the direct path: `serviceLauncher`
  (`pkg/server/trigger_launcher.go`) wraps `runview.Service.Launch`, resolving
  the bot via `botregistry.ResolveBotPath`. DONE for the dispatcher's
  *execution step* behind a default-off flag — see the 2026-07-16 update
  below. STAGED: routing `RunRun`, and flipping the dispatcher flag on by
  default (the production cutover, which reconciles the pipeline-concurrency
  gate / broker fan-out the full Service layers on).
- **Run-completion chaining** — DONE: `runview.Service.emitRunCompletion`
  publishes `run.finished`/`run.failed`/`run.cancelled` onto the shared bus
  (wired via `SetEventPublisher(coord.Bus())`); a direct-mode subscription
  matching `Source: run` fires the downstream bot. The `Actor` carries the
  upstream bot id so a chain can key on "after feature-dev finishes".
  *2026-07-21 update (web-notifications work):* the event construction is
  now the shared `trigger.BuildRunOutcome` (kind derivation, tenant+owner
  enrichment, per-episode ID), **cloud runner pods publish the same
  outcome events** onto the NATSBus (`Runner.fireOutcomeEvent` — closing
  the "runner-pod runs are invisible to the spine" gap), and the NATSBus
  is wired for real in `iterion server`/`runner` (`cfg.EventsBus`,
  injected into the trigger coordinator so every consumer rides ONE bus).
  First cloud consumer: the `usernotify` dispatcher (browser web push on
  `run.paused` + terminals, queue group `usernotify` —
  [docs/notifications.md](../notifications.md)). The evaluator itself
  still subscribes on the coordinator's bus (local); pointing a cloud
  evaluator at the NATSBus stays a staged follow-on.
- **Scheduled** — DONE: `trigger.Scheduler` ticks schedule-kind subscriptions
  on their `Cron` and fires them via the launcher, scoped to the local tenant
  "" (no-op in cloud, where cloudsched's CAS ticker stays authoritative for
  real tenants). `FromScheduleInvocation` seeds a schedule subscription from a
  bot's `suggested_cron`. STAGED: folding `cloudsched.ScheduledBot` into
  `Subscription{Invocation: schedule}` so cloud + local share one table.
- **Git-forge** — DONE (on the bus): the shared launch tail
  (`insertAndLaunchWebhook`) emits a `SourceForge` event with the
  `launched_run_id` marker, so forge is a unified, observable source and the
  evaluator never re-launches it. STAGED (the cutover): when a forge
  *subscription* matches, skip the inline launch and let the spine launch
  (stop setting the marker); parity-gate via the `webhooks.Delivery` audit,
  then retire the inline path. The per-event `payload["vars"]` carrier the
  evaluator now merges is the vehicle for forge's dynamic launch vars.
- **Custom ingress** — DONE: `POST /api/v1/triggers/emit` (requireAuth) forces
  `Source: custom` (cannot spoof a board/forge event) and publishes onto the
  bus; matching custom subscriptions fire asynchronously. STAGED: a cloud
  signed-token variant alongside the local auth gate.
- **Studio Automations view** + cloud team-scoped REST + a file-backed local
  subscription store (the memory store is rebuilt from manifests each start).

## Update 2026-07-16 — Launch-path convergence primitive (LaunchSpec fields + RunLauncher seam)

The dispatcher's `EngineRunner.Dispatch` can now route its fresh-dispatch
execution step through `runview.Service.Launch` — the single launch
authority — instead of building a private engine. Shipped behind a
default-off flag; the direct-engine path is unchanged unless opted in.

**The four per-launch `LaunchSpec` additions** (`pkg/runview/service.go`),
each a per-launch override of the service-level default:

- `WorkDir` — the per-issue worktree, so `${PROJECT_DIR}` resolves there
  (runtime.WithWorkDir), not the daemon cwd.
- `DailyCap` — the dispatcher's singleton-SpendStore cost guard
  (runtime.WithDailyCap), so every concurrent run writes the one ledger.
- `SourceRef` — the originating kanban issue stamped onto the run record
  (runtime.WithSource).
- `ExtraObservers` — event observers fired on **every run event**, not just
  engine-level ones. Delivered through **two disjoint observer seams**:
  `runtime.WithEventObserver` for engine emits, and
  `ExecutorSpec.EventObservers` (attached to the backend hooks' redacting
  emitter) for the high-frequency tool events that flow straight to the store
  through the backend hook layer. The two event sets are disjoint — the two
  are the only `AppendEvent` chokepoints during a run — so each event reaches
  the observer exactly once, keeping the dispatcher's stall watermark alive on
  long agent nodes. **No store wrapper is interposed** (see the dated update
  below).

**A fifth field, `OnOutcome func(error)`** — the return-path completion of
the four data fields. A blocking caller (the dispatcher routing through
Launch) needs the run's terminal **Go error**, not just its persisted
status: `scheduleRetry` text-matches the error for sandbox-setup backoff
(`isSandboxSetupError`) and doomed-resume detection (`isResumeSourceChanged`),
so a status-reconstructed error would silently change the retry cadence.
`OnOutcome` fires once in the run goroutine with `bodyErr` just before
`LaunchResult.Done` closes (the channel-close is the happens-before edge),
so the caller reads the exact error `engine.Run` returned. Fire-and-forget
launches (CLI / studio / webhook) leave it nil.

**The seam** (`pkg/dispatcher/engine_runner.go`): a `RunLauncher` interface
(`LaunchAndWait(ctx, LaunchSpec) error`) with a `ServiceRunLauncher` adapter
over `*runview.Service`, wired via `NewEngineRunner`'s new `WithRunLauncher`
option and gated on `ITERION_DISPATCH_VIA_SERVICE`. `dispatchViaService`
translates the DispatchSpec invariants into the LaunchSpec fields and blocks
on the launcher.

### Decisions / trade-offs

- **Interface seam, not a hard dependency.** `EngineRunner` calls the
  `RunLauncher` interface; the concrete `runview.Service` binding is injected
  by the wiring layer. Keeps the routing testable in isolation and the
  default path allocation-free.
- **Resume and bundle dispatches stay on the direct engine path.** Resume
  reuses the checkpoint + worktree the runner already owns, and a `.botz`
  runner shares one opened bundle handle across dispatches — neither maps
  cleanly onto a stateless `Service.Launch` yet. The gate excludes both, so
  the convergence covers exactly the fresh plain-`.bot` dispatch.
- **Default-off flag over a hard cutover.** The full `Service` layers a
  pipeline-concurrency gate, broker fan-out, supervisors, a session board
  and a completion notifier onto every launch. For the dispatcher-observable
  invariants (stall / caps / retry / source) the two paths are proven
  byte-identical by a diff-run test
  (`engine_runner_via_service_test.go`), but the extra machinery is why the
  production cutover (flag-on by default, Service wired into the daemon
  Manager) is deliberately staged rather than shipped here.
- **`map[string]any` → `map[string]string` var stringification.** `LaunchSpec.Vars`
  is stringly-typed; dispatcher bot-args are strings in practice, and a
  non-string value is rendered with `%v` so it still reaches the bot.

## Key files

New: `pkg/trigger/{event,subscription,store,memstore,mongostore,evaluator,
launcher,board_source}.go`, `pkg/eventbus/{bus,inproc}.go`,
`pkg/server/{trigger_coordinator,triggers_routes}.go`, `pkg/cli/trigger.go`.
Modified: `pkg/bundle/manifest.go` (`InvocationBoard`), `pkg/queue/nats/nats.go`
(`ITERION_EVENTS`), `pkg/dispatcher/manager.go` (`Refresh` nudger),
`pkg/server/{server,server_info}.go`, `pkg/cli/{studio,dispatch}.go`.

Launch-path convergence (2026-07-16): `pkg/runview/service.go` +
`pkg/runview/service_launch.go` (LaunchSpec
`WorkDir`/`ExtraObservers`/`DailyCap`/`SourceRef`/`OnOutcome` + `launchExtras`
wiring), `pkg/dispatcher/engine_runner.go` (`RunLauncher` /
`ServiceRunLauncher` / `WithRunLauncher` / `dispatchViaService`).

## Update 2026-07-16 — ExtraObservers via observer seams, not a store wrapper

The first cut delivered `ExtraObservers` by wrapping the `store.RunStore`
(an `observerStore`, mirroring the dispatcher's `heartbeatStore`) so every
`AppendEvent` fanned out to the observers. That wrapper **shadowed the
concrete `*FilesystemRunStore` against the executor's and sandbox's optional-
capability type-probes**: `PlanWriter` (todo/plan snapshots),
`RunFilesStore` (`EnsureRunFilesDir`/`ListRunFiles`/`OpenRunFile` — the
run-files host/sandbox bind-mount parity), and `QueuedInboxVersioner` all
resolved to `nil` on the dispatcher-via-service path (`ITERION_DISPATCH_VIA_SERVICE=1`)
because Go interface embedding forwards only the base `RunStore` methods, not
the optional interfaces satisfied concretely. The convergence route therefore
*silently degraded* the very launch it claimed to reuse — a plain studio
launch keeps those capabilities.

**Decision.** Drop the store wrapper. The launch hands the executor + engine
the **raw** `s.store`, and `ExtraObservers` ride two disjoint seams:

- **engine events** → `runtime.WithEventObserver` (wired in `engineOptions`
  from `launchExtras.observers`);
- **backend-hook events** (the high-frequency tool stream) →
  `ExecutorSpec.EventObservers`, threaded into `NewStoreEventHooks` and fired
  by the `redactingEmitter` — already the single `AppendEvent` chokepoint for
  hook events, where capability detection happens on the *original* emitter
  before wrapping, so nothing is shadowed.

A grep proof underpins completeness: during a run there are exactly two
`AppendEvent` chokepoints — `runtime` `emitBranch` (fires `e.onEvent`) and the
`redactingEmitter` (fires the executor observers) — so the union covers every
event with no double-fire. `engine_runner_via_service_test.go` still proves the
dispatcher-observable event **set** is byte-identical to the direct path, and
`TestLaunchStore_KeepsOptionalCapabilities` +
`TestLaunch_HonoursDispatcherConvergenceFields` (asserting the `artifact_files`
area is created under an observed launch) pin the capability-preservation
invariant.

**Trade-off.** The two-seam delivery leans on the invariant that engine and
hook emits are disjoint; a future third `AppendEvent` call site would need a
matching seam. That is a cheaper, more honest cost than a wrapper that must
hand-forward every present *and future* optional capability to avoid silent
degradation — the failure mode a `RunStore` decorator structurally invites.
Files: removed `pkg/runview/observer_store.go`; `pkg/backend/model/hooks.go`
(`redactingEmitter.observers` + `NewStoreEventHooks` variadic),
`pkg/runview/executor.go` (`ExecutorSpec.EventObservers`),
`pkg/runview/service_launch.go` (raw store + `launchExtras.observers`).
