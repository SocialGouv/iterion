# Events, leases, queues and schedules — package invariants

One entry per package, verbatim from the engine map this tree replaces.
The generated [map-packages.md](../../references/map-packages.md) lists
every package with its purpose and interfaces; these leaves carry the
invariants. `iterion map find|impact|path` answer the exact questions.

- `pkg/lease/` — **Singleton-net election** ([ADR-117](../../adr/117-server-side-leader-lease-for-idempotent-nets.md)): a named, sticky lease one replica holds while it runs a net that is idempotent but costs N times as much on N replicas — the merge-gate sweep first, whose offers spend the forge's hourly request budget. `Store` (Mongo `leases` collection + memory twin, one conformance suite) and `Run` (campaign → serve the term, renewing every TTL/3 → fail-closed step-down → release on exit, so a rollout hands over within one retry rather than a TTL). Load reduction, not mutual exclusion: work that must never run twice keeps its per-item claim. Lease names live in `pkg/server/leases.go`

- `pkg/trigger/` — The unifying spine for **event-driven runs**: one canonical `Event` envelope every source (forge webhook, schedule tick, native-board transition, run completion, custom ingest) maps onto, a `Subscription` registry binding (event filter) → (bot launch into a repo/workspace), and the `Evaluator` that consumes events from `pkg/eventbus`, matches subscriptions, and launches/promotes. See [docs/adr/046-event-driven-runs-trigger-spine.md](../../adr/046-event-driven-runs-trigger-spine.md)

- `pkg/eventbus/` — Internal publish/subscribe spine carrying `trigger.Event` values from producers to the trigger `Evaluator`; two interchangeable implementations selected at wiring time — `InProcBus` (local CLI/studio) and `NATSBus` (cloud, the separate `ITERION_EVENTS` stream)

- `pkg/schedgate/` — Overlap policy + pre-launch guard evaluation shared by the three scheduled-launch surfaces (host crontab, `pkg/trigger.Scheduler`, `pkg/cloudsched`); no I/O beyond running the guard subprocess

- `pkg/cloudsched/` — Cloud-mode recurring-bot scheduler: per-org store of cron-scheduled bots + a multi-replica-safe CAS ticker firing each due schedule exactly once (cloud counterpart of `iterion schedule`)

- `pkg/retrypolicy/` — Retry policy (`usage_window` resume|off, `max_attempts`, `max_wait`, `jitter`) for `failed_resumable` runs that should wait out a provider quota/usage window instead of re-burning pods; resolved field-by-field in precedence order (per-run override → launching surface → bot manifest → `ITERION_RETRY_*` machine default → package default), with the `ITERION_CLOUD_RETRY_*` platform ceiling applied last and only able to *lower* a policy. Consumed by the runner's usage-retry path, `--auto-resume`, and `iterion schedule`/`cloudsched`. See [docs/scheduling.md](../../scheduling.md) §Retry

- `pkg/queue/` — NATS-backed work queue used by cloud-mode dispatcher → runner pods

- `pkg/dispatcher/` — Long-running dispatcher: native kanban store, polling actor, tracker adapters (native, github, forgejo)

  - `tracker/` — `Tracker` interface + normalized `Issue` type + GitHub/Forgejo adapters

  - `native/` — Filesystem-backed kanban (board.json, issues/, events.jsonl) + REST + adapter

  - `native/boardops/` — capability-gated board operations shared by the `__mcp-board` stdio server, the `/api/v1/mcp/board` HTTP handler, and the claw in-process tools (`mcp.iterion_board.*`)

- `pkg/usernotify/` — User-addressed notifications for run lifecycle moments (run paused on a human form, finished/failed/cancelled): `Dispatcher` consumes run-outcome `trigger.Event`s from the eventbus spine (queue group `usernotify` on NATS ⇒ one replica per event), resolves recipients (run owner + per-user team-wide opt-in prefs), dedups per episode via the `sent_notifications` first-writer-wins claim, and fans out to `Sink`s — `webpush/` (VAPID Web Push to per-browser subscriptions, cloud) ships; desktop (Wails OS notification) and email are future sinks on the same interface. A 2-min reconciliation sweep replays episodes the lossy bus dropped. Shared event authority: `trigger.BuildRunOutcome` (used by both `runview.emitRunOutcome` and the runner's `fireOutcomeEvent`). Enabled iff `ITERION_WEBPUSH_VAPID_{PUBLIC,PRIVATE}_KEY` are set (`iterion server webpush-keys` mints a pair). See [docs/notifications.md](../../notifications.md)

- **Review scope** — a human gate anchors the workspace at `refs/iterion/runs/<run>/gate/<seq>` and shows the operator everything changed **since the previous gate**, grouped by the node that changed it (`/api/runs/{id}/review/scope|diff`, [pkg/server/runs_review_scope.go](../../../pkg/server/runs_review_scope.go)). Deliberately a RANGE, not a declared node list: per-node boundaries only exist for main-path nodes, so a declared list would silently miss subbot / fan-out / compute work — the range is a workspace before/after, and what cannot be attributed shows under *Other changes* rather than being dropped. See [docs/review-scope.md](../../review-scope.md)

- `pkg/notify/` — Delivers run-completion webhooks to operator-supplied URLs behind an SSRF guard (http/https only; fails closed on loopback/link-local/RFC-1918/metadata hosts unless opted in)

- `pkg/webhooks/` — Inbound-webhook spine: long-lived per-org `iwh_` tokens (token or HMAC mode) authenticating an external caller (forge/CI/script) to launch a configured set of bots, with per-provider parsers (GitLab/GitHub/Forgejo/generic). See [docs/webhooks.md](../../webhooks.md)

- `pkg/alert/` — Run-observer detecting stall/budget/failure conditions off runtime events (`budget_warning`/`budget_exceeded`/`run_failed`) + a per-run liveness heartbeat, fanning alerts to webhooks and in-process sinks

- `pkg/clock/` — Minimal `Clock` abstraction (real + fake) for deterministic testing of time-dependent logic (e.g. daily spend-cap resets)
