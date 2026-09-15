# Automation surfaces — what launches a run without a human typing

The five trigger families and the spine they converge on: the dispatcher's
tracker poll, a bound roadmap board, inbound forge webhooks, the event-driven
`trigger` spine, and the board capabilities a bot declares to write back.

Full references: [../dispatcher.md](../dispatcher.md),
[../webhooks.md](../webhooks.md),
[../adr/046-event-driven-runs-trigger-spine.md](../adr/046-event-driven-runs-trigger-spine.md).

## Surfaces

### Dispatcher layer (`iterion dispatch`)

Iterion ships a long-running dispatcher on top of the runtime engine:
`iterion dispatch <config.yaml>` polls an issue tracker (native kanban,
GitHub Issues, or Forgejo/Gitea) and dispatches a workflow run per
eligible issue, with retry, stall detection, per-state concurrency,
and lifecycle hooks (`after_create`, `before_run`, `after_run`,
`before_remove`).

The dispatcher uses an **actor pattern** — a single goroutine owns all
mutable state; outside callers send typed commands on a channel. The
architecture is fully documented in [docs/dispatcher.md](../dispatcher.md);
the native tracker (the default, locally-owned kanban) is documented
in [docs/native-tracker.md](../native-tracker.md).

Key files: [pkg/dispatcher/dispatcher.go](../../pkg/dispatcher/dispatcher.go) (actor +
public API), [pkg/dispatcher/loop.go](../../pkg/dispatcher/loop.go) (polling + dispatch),
[pkg/dispatcher/tracker/tracker.go](../../pkg/dispatcher/tracker/tracker.go) (the
`Tracker` interface), [pkg/dispatcher/native/store.go](../../pkg/dispatcher/native/store.go)
(the JSON kanban store), [pkg/cli/dispatch.go](../../pkg/cli/dispatch.go) (daemon
wiring including the embedded SPA).

The studio's SPA exposes two new routes when the corresponding server
flags are set: `/board` (kanban CRUD with drag-and-drop, gated on
`server_info.native_tracker_enabled`) and `/dispatcher` (live dashboard
with running + retry tables, gated on `server_info.dispatcher_enabled`).

### Bound roadmap board (GitHub Projects v2 → cloud board dispatcher)

A team can bind a GitHub **Projects v2** board to its native board
(ADR-097). The default column map lands every *Planned* project item in
the native `ready` column — and `ready` is the column the **cloud board
dispatcher** (one per server replica, over the Mongo board) dispatches
from, so a human's drag on the roadmap is itself a launch path.

What keeps a bound roadmap from launching wholesale is the **bot**: cloud
has no default bot, so the dispatcher only claims a `ready` card that
names one (stamped by a triage bot's `set_bot`, by an operator, or by a
board trigger); a bot-less `ready` card stays roadmap content and is
neither claimed nor moved. A card claimed but unlaunchable after all
(`errCardUnlaunchable` — its bot was cleared or cannot be resolved in
between) is given back to its column under machine provenance, never
parked `blocked`. A launch the run service *refuses* is retried on the
launch-refusal backoff — 1m, 2m, 4m… doubling, capped at 30m — and filed
`blocked` after `ITERION_BOARD_LAUNCH_ATTEMPTS` consecutive refusals
(default 8). Key file:
[pkg/server/boarddispatch.go](../../pkg/server/boarddispatch.go).

References: [docs/github-board-sync.md](../github-board-sync.md#dispatching-from-the-board),
[docs/dispatcher.md](../dispatcher.md#claim-selection-on-the-cloud-board--what-is-never-claimed),
[docs/adr/097-github-projects-v2-board-sync.md](../adr/097-github-projects-v2-board-sync.md).

### Inbound webhooks (cloud agent-workflow triggers)

Distinct from the dispatcher (which polls): cloud mode exposes
self-authenticating inbound webhook endpoints that launch a bot per
external event — GitLab MR open/reopen **and** `/revi` note re-review,
GitHub PR, Forgejo/Gitea PR, and a generic JSON trigger. Per-org
`iwh_` tokens (token or HMAC mode), rate limits, monthly quotas,
idempotent delivery audit, and the per-org launch gate (run quota /
cost cap / concurrency — `pkg/orgusage` + `pkg/server/launch_gate.go`)
all sit in front of the launch. Key files:
[pkg/webhooks/](../../pkg/webhooks/) (spine + per-provider parsers),
[pkg/server/webhooks_common.go](../../pkg/server/webhooks_common.go) (shared
admission→idempotency→launch tail). Reference: [docs/webhooks.md](../webhooks.md);
platform overview: [Iterion Cloud overview](../cloud-overview.md).

### Event-driven trigger spine (`pkg/trigger` + `pkg/eventbus`)

The unifying layer the trigger families above (schedule, dispatcher
poll, forge webhooks, `invocations:` DSL) are converging onto: one
canonical `trigger.Event` envelope, an internal `eventbus.Bus`
(`InProcBus` local, `NATSBus` on the **separate** `ITERION_EVENTS`
stream for cloud), and a `trigger.Subscription` registry binding
`(event filter) → (bot launch into a repo/workspace)` — queryable
**by repo / by bot** (`ListByRepo` / `ListByBot`), stored in-memory
(local) or Mongo (cloud) like `forge.RepoIntegrationStore`. The per-bot
`bundle.Invocation` stays the *capability* ("what can fire me");
`Subscription` is the *binding* (where/which repo), generated from
invocations — repo/tenant/cron never enter a manifest. **Four sources
ship on the spine** (each = a source adapter publishing a
`trigger.Event` + an effect: promote-card vs direct launch):
- **board events** — a `kind: board` invocation with a `board:` block
  (`on`/`to_states`/`all_labels`) fires a bot on a native-card
  transition. The board source tails the existing
  `native.Store.Subscribe` seam and **promotes the card** (stamps its
  bot) so the dispatcher's `Claim` — the **sole launch authority** —
  picks it up now (`Manager.Refresh()`) instead of at the 30s poll; the
  poll stays the reconciliation net, so fast-path + poll **cannot
  double-launch**. A board invocation may instead declare **`mode:
  direct`** — the evaluator then launches the bot ON the matching card
  (card id in `vars.issue_id`) instead of routing the card TO it; with
  **`consume_labels: true`** the matcher's `all_labels` set is stripped
  atomically pre-launch, making the label a one-shot re-armable trigger.
  This powers **issue auto-triage** (`bots/issue-triage`, persona
  Triagy): forge issues synced to the board are author-classified at
  ingest (fail-closed trust gate — `authorTrust` over
  `forge.PermissionClient`, threshold `MinAuthorRole`); trusted authors'
  cards land in inbox with `triage:auto` (fires Triagy, who stamps the
  handler bot + labels via `set_bot`), untrusted ones park with
  `needs:approval` + zero LLM until the operator's studio "Approve &
  triage" swaps the labels. The same author gate protects the webhook
  `AutoImplementOnOpen` zero-touch lane. **Cloud parity**: the mongo
  board has its own spine half
  ([pkg/server/trigger_cloud.go](../../pkg/server/trigger_cloud.go)) — a
  `board_events` poll-tail that normalizes and matches a whole batch,
  writes one row per matched `(event, subscription)` pair into the
  **durable effect outbox** (the `trigger_effects` collection), and only
  THEN CAS-advances the per-tenant cursor. Cloud board events do **not**
  ride the lossy NATS bus (ADR-094): the outbox IS the delivery. A
  `trigger.EffectWorker` on every replica claims due rows under a
  two-minute lease (`EffectLease`), runs them through the same evaluator,
  retries with exponential backoff up to `MaxEffectAttempts` (5), then
  parks the row as a queryable `failed` dead-letter. The label consume
  stays atomic (`boardmongo.ConsumeLabels`) and the row persists
  `ConsumeMarked` between the consume and the launch, so a
  `consume_labels` trigger is spent exactly once and a failed launch
  still retries. The cursor CAS dedups *materialization*; the leased
  claim dedups *execution* — debug a missed cloud board trigger from the
  outbox rows, not from the bus. The `/api/v1/triggers` CRUD is
  team-scoped in cloud (active-team JWT); the outbox contract is
  [ADR-094](../adr/094-trigger-effect-outbox.md).
- **run-completion** ("runned by iterion") — `runview.Service` emits
  `run.finished`/`failed`/`cancelled`/`paused` in-process, and cloud
  **runner pods publish the same events** onto the NATSBus
  (`Runner.fireOutcomeEvent`, sharing `trigger.BuildRunOutcome` with the
  runview emitter — the NATSBus is wired in cloud since the
  web-notifications work); a direct-mode subscription chains the next
  bot (`Actor` = upstream bot id), and the `usernotify` dispatcher
  consumes `run.paused`+terminals for browser push notifications
  ([docs/notifications.md](../notifications.md)).
- **scheduled** — `trigger.Scheduler` ticks schedule-kind subscriptions
  on their `Cron` (local tenant ""; cloud keeps cloudsched's CAS
  ticker).
- **git-forge** — the inbound-webhook launch tail emits a `SourceForge`
  event with a `launched_run_id` marker (observational; the evaluator
  never re-launches it, so the mature HMAC/idempotency/quota webhook path
  stays the sole authority).

Direct launches go through `serviceLauncher` over `runview.Service.Launch`.
Wired in [pkg/server/trigger_coordinator.go](../../pkg/server/trigger_coordinator.go)
(both `iterion studio` and `iterion dispatch`); REST CRUD at
`/api/v1/triggers` (gated by `server_info.triggers_enabled`). **Custom
ingress** ships: `POST /api/v1/triggers/emit` injects a `SourceCustom`
event onto the spine and answers `202` — the launch, if any, happens in
the evaluator, so no `run_id` comes back synchronously (use a direct
webhook when you need one). That endpoint is the extensibility point for
arbitrary external systems. The **studio Automations view** ships too, at
`/triggers` ([studio/src/views/Triggers/](../../studio/src/views/Triggers/)):
a Triggers tab listing and creating subscriptions by repo and by bot, plus
a Schedules tab. That route is deliberately **not** gated on
`triggers_enabled` — it degrades per tab, because a cloud server carries
Schedules even when the event-trigger spine is off. Still staged: the
forge *cutover* (spine becomes the forge launcher, inline retired),
forge-derived subscription provisioning, and dispatcher `EngineRunner`
convergence. Reference:
[docs/adr/046-event-driven-runs-trigger-spine.md](../adr/046-event-driven-runs-trigger-spine.md).

### Bot board access (capabilities)

Agent and judge nodes can write to the native board by declaring a
`capabilities:` list in the `.bot` DSL (e.g.
`capabilities: [board.create, board.move, board.read]`). The runtime
opens the matching tools transparently based on the backend:

- **claude_code (default)** — registers an internal `__mcp-board` stdio
  MCP server (subcommand of the iterion binary) and extends the
  AllowedTools list with the granted `mcp__iterion_board__*` FQNs.
- **claude_code (sandboxed)** — falls back to an HTTP transport at
  `/api/v1/mcp/board` on the iterion server, authenticated via an
  ephemeral `X-Iterion-Run` token registered by the runtime.
- **claw** — registers the operations as in-process tools under
  `mcp.iterion_board.*` via `pkg/backend/tool/claw_board_tools.go`.

All three paths route through the same
[pkg/dispatcher/native/boardops](../../pkg/dispatcher/native/boardops/ops.go)
package, so validation and event semantics are identical. Capability
diagnostics are `C080` (unknown cap, warning) and `C081` (malformed,
error). The bot catalog Nexie reads
([bots/whats-next/skills/iterion-bot-catalog.md](../../bots/whats-next/skills/iterion-bot-catalog.md))
is **generated** from each bot's `manifest.yaml` (persona table +
per-bot cards with description / triggers / vars / `when_to_use`,
enabled bots only) spliced into a hand-authored
`iterion-bot-catalog-static.md` preamble (the decision tree +
distinguishers + rituals you maintain by hand). To change Nexie's
routing, edit a bot's manifest (`display_name` / `description` /
`when_to_use` / `triggers` / `enabled`) or toggle it in the studio
Catalog manager — **don't hand-edit the generated region**. Regeneration
runs automatically before Nexie's run (engine) and on every studio
bot-metadata save (server); refresh the committed copy by hand with
`iterion bots regen-catalog`. A workspace overlay
(`.iterion/bot-overrides.yaml`, gitignored) can hide/show a bot
per-workspace without editing its manifest. See
[pkg/botregistry/catalog.go](../../pkg/botregistry/catalog.go).

