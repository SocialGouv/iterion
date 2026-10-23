# Data model — automation surfaces

The trigger spine that turns a board event into a bot launch, the two surfaces that fire a bot on a clock, and five things a multi-replica deployment claims or leases before it acts.

> Drawn from the code at commit `61a305262` — main at v3.266.1, after #2391, which carries the launch rung; how this page is kept true: [README](README.md).

## Block A — the trigger spine

```mermaid
erDiagram
    board_issues {
        string _id PK "native:<uuid>"
        string tenant_id FK "team id"
        string issue.state "e.g. inbox|backlog|ready|waiting_deps|in_progress|awaiting_input|review|done|blocked"
        string issue.bot "a bot SLUG, not a key"
        map issue.botargs
        string[] issue.labels
        string issue.claim
        string issue.statereason
        datetime issue.updatedat
    }
    board_events {
        ObjectId _id PK
        string tenant_id FK "team id"
        int64 event.seq
        string event.type "16 native.EventType values, issue_launch_refused included"
        string event.issueid FK "board_issues._id"
        map event.payload
        datetime event.timestamp
    }
    board_config {
        string _id PK "family: <team> (the tenant IS the id) | seq:<team> | trigger-cursor:<team>"
        object board "the schema document"
        int64 seq "the seq and trigger-cursor documents"
    }
    trigger_effects {
        string _id PK "<event id>|<sub id>, <event id>#projection"
        string tenant_id FK "team id"
        object event
        string kind "'' (absent) and 'launch' are the same thing; 'projection' is the other arm"
        string sub_id FK "trigger_subscriptions._id"
        string state "pending|claimed|done|failed"
        int attempts
        bool consume_marked
        string claim_id
        datetime not_before
        datetime updated_at
    }
    trigger_subscriptions {
        string _id PK
        string tenant_id FK "team id"
        string repo "a forge slug 'group/project'"
        string bot_id "a bot SLUG, never a key"
        string invocation "forge|command|schedule|board|keepalive"
        string mode "direct, board"
        object match
        bool consume_labels
        bool enabled
        string origin "e.g. operator | bot-home | manifest:<bot name>"
    }
    forge_board_bindings {
        string _id PK, FK "teams._id - the tenant IS the id"
        string connection_id FK "forge_connections._id"
        object status_mapping "an ARRAY of column and native-state pairs, not a map"
        map status_options "a native state to the board option id"
    }
    bot_sources {
        string _id PK
        string tenant_id FK "team id | 'platform:'"
        string slug "unique per tenant"
    }
    board_issues |o--o{ board_events : "event.issueid - empty on board_updated alone"
    board_config ||..o{ board_events : "event.seq - minted by an $inc on the seq:<team> document, never a key"
    board_events ||..o{ trigger_effects : "_id embeds board:cloud:<issue>:<seq>; the event itself is COPIED in"
    trigger_effects }o--o| trigger_subscriptions : "sub_id - empty on a projection row"
    trigger_effects }o--o| forge_board_bindings : "tenant_id, on a projection row - the binding _id IS the tenant"
    trigger_subscriptions }o..o{ board_issues : "issue.bot, stamped by board mode - no field points back"
    trigger_subscriptions }o..o| bot_sources : "bot_id (slug)"
    board_issues }o..o| bot_sources : "issue.bot (slug)"
    trigger_subscriptions }o..o| "FS: baked catalog" : "bot_id slug - the last tier, resolved at launch"
    board_issues }o..o| "FS: baked catalog" : "issue.bot slug - the last tier, resolved at launch"
```

- **`board_config` is one collection holding three document shapes.** The board schema (`_id` = the team), the event-seq allocator (`seq:<team>`) and the trigger cursor (`trigger-cursor:<team>`). None of the three carries a `tenant_id` field — the tenant is the `_id`.
- **A projection row names no subscription.** `kind: projection` is owed to the team's bound external board, so `sub_id` is empty. The binding is reached by key (`forge_board_bindings._id` *is* the team id), which is why that edge is solid, not dotted.
- **A bot id is a slug, resolved at launch, never a foreign key.** The walk is: the team's `bot_sources` row for `(tenant, slug)`, then the platform row (tenant `platform:`), then the baked catalog on the server's own filesystem — so a slug that resolves to nothing is a launch failure, not a dangling reference. The same holds for `issue.bot` and for `scheduled_bots.bot_id`.
- **The board field names are the lowercased Go names.** The inner documents are `native.Issue` and `native.Event` verbatim with no bson tags, so the stored fields are `issue.claimleaseuntil`, `issue.lastrunid`, `issue.statereason` and `event.issueid` — while the REST API renders the snake_case JSON names.

## Block B — the two scheduled surfaces, side by side

```mermaid
erDiagram
    scheduled_bots {
        string _id PK
        string tenant_id FK "team id"
        string repo_integration_id FK "repo_integrations._id"
        string bot_id "a bot slug"
        string cron "5-field standard cron"
        int interval_seconds
        datetime next_fire_at
        datetime last_fire_at
        string overlap
        string retry_usage_window
        bool disabled
        string last_error
        string last_run_id FK "runs._id"
    }
    schedule_launch_intents {
        string _id PK "schedule id plus nominal slot time"
        object schedule "ScheduledBot snapshot, including tenant_id and next_fire_at"
        datetime not_before "retry deadline or claim lease"
        string claim_id
        bool done
        bool prepared
    }
    trigger_subscriptions {
        string _id PK
        string tenant_id FK "team id"
        string invocation "e.g. schedule, keepalive"
        string cron
        int interval_seconds
        string overlap
        string retry_usage_window
        bool enabled
        string last_error
    }
    repo_integrations {
        string _id PK
        string tenant_id FK "team id"
        string repo_full_name "'group/project', unique per (tenant, connection, repo)"
    }
    runs {
        string _id PK
        string tenant_id FK "team id"
        string source.kind "free-form; today dispatcher|schedule|studio_chat"
        string source.schedule_id FK "e.g. scheduled_bots._id, trigger_subscriptions._id"
    }
    audit_events {
        string _id PK
        string scope "tenant or platform - NOT what kind of id tenant_id holds"
        string tenant_id "POLYMORPHIC: e.g. the schedule TEAM id; an ORG id"
        string action "e.g. schedule.tick.fired|skipped_overlap|guard_blocked|guard_error|launch_failed"
        string target_id
        map meta
        datetime created_at
    }
    scheduled_bots }o--o| repo_integrations : "repo_integration_id - the provisioned id on the forge path, resolved from repo_url on CREATE in the manual path, never re-resolved by a PATCH; empty, for example, with no repo_url"
    trigger_subscriptions }o..o{ repo_integrations : "repo (slug) - compared to the EVENT repo, never joined to an integration; one slug may match several"
    scheduled_bots |o--o{ runs : "source.schedule_id - empty on a run no schedule launched"
    scheduled_bots }o--o| runs : "last_run_id"
    scheduled_bots |o--o{ schedule_launch_intents : "schedule._id - inserted before advancing next_fire_at"
    trigger_subscriptions |o--o{ runs : "source.schedule_id"
    scheduled_bots |o..o{ audit_events : "target_id on the schedule.* rows, never joined"
```

- **Both rows carry the same policy; their clocks live separately.** A schedule-kind `trigger_subscriptions` row has **no** next-fire field — the resident `trigger.Scheduler` persists its calendar and pending intentions in `schedule-intents.json`. Cloud slots persist in `schedule_launch_intents` before `scheduled_bots.next_fire_at` advances, so a maintenance hold survives a restart.
- **The two surfaces do not overlap — and together they do not cover every row.** `cloudsched` fires `scheduled_bots` rows only; the resident `trigger.Scheduler` fires subscriptions of tenant `""` only, the local single-host scope. So a schedule- or keepalive-kind `trigger_subscriptions` row carrying a real team id is fired by neither.
- **The gate persists nothing of its own.** `pkg/schedgate` returns a `TickRecord`; that record is the tick's trace. In cloud the record lands in `audit_events` (`action` `schedule.tick.<decision>`, `tenant_id` the schedule's **team**).

## Block C — what is leased, and by whom

```mermaid
erDiagram
    board_issues {
        string _id PK "native:<uuid>"
        string tenant_id FK "team id"
        string issue.claim "e.g. board-dispatcher:<uuid> | reaper:board-dispatcher:<uuid>"
        int64 issue.claimepoch
        datetime issue.claimleaseuntil
        object issue.launchrefusal
    }
    trigger_effects {
        string _id PK "<event id>|<sub id> or <event id>#projection"
        string tenant_id FK "team id"
        string state "pending|claimed|done|failed"
        string claim_id
        datetime not_before
        int attempts
    }
    forge_board_bindings {
        string _id PK, FK "teams._id - the tenant IS the id"
        datetime last_synced_at
        datetime sync_lease_until
        string sync_lease_owner
    }
    leases {
        string _id PK "the lease NAME - never a tenant"
        string owner "the host plus a random suffix"
        datetime expires_at
        datetime renewed_at
    }
```

- **Five mechanisms, five grains.** A card is held by a *fenced CAS claim* (marker plus epoch plus a 15-minute lease); an effect row is held by a *claim id with a 2-minute horizon*; one team's board binding is held, for the length of a reconciliation pass, by a *watermark CAS plus a 5-minute lease*; a named periodic net is held by a *sticky lease* in `leases`; and a schedule's next fire is held by a CAS on `next_fire_at` (`cloudsched`'s `ClaimTick`) — no lease, exactly once per slot. Only the sticky lease is an election.
- **There is no dispatcher-coordinator lease.** `leases` holds two names in this tree, `merge-gate-sweeper` (the merge-gate sweep) and `board-issue-sync` (the forge→board issue sync of the repo integrations).
- **`pkg/lease` is load reduction, not mutual exclusion.** A holder that stalls past its TTL can overlap its successor, so the package is for idempotent reconciliation nets whose *cost* scales with replicas.
- **A lease running out does nothing by itself.** The expiry is a predicate the sweeps query, never a timer.

## Collections

| Collection | Package | Document struct | Tenant field (values) | Unique / TTL indexes | Twin |
|---|---|---|---|---|---|
| `trigger_subscriptions` | `pkg/trigger` | `Subscription` | `tenant_id` — a team; `""` is the local scope | none unique, none TTL | memory (local, manifest-seeded each start) / Mongo (cloud) |
| `trigger_effects` | `pkg/trigger` (type), `pkg/dispatcher/boardmongo` (store) | `EffectRow` | `tenant_id` — a team | TTL `done_ttl`, 7 days, partial on `state: "done"` | file `trigger-effects.json` beside the native board |
| `board_issues` | `pkg/dispatcher/boardmongo` | `issueDoc` wrapping `native.Issue` | `tenant_id` — a team | none unique, none TTL | file `issues/<id>.json` |
| `board_events` | `pkg/dispatcher/boardmongo` | `eventDoc` wrapping `native.Event` | `tenant_id` — a team | none unique, none TTL | file `events.jsonl` |
| `board_config` | `pkg/dispatcher/boardmongo` | `configDoc` wrapping `native.Board` | the `_id` is the tenant: `<team>`, `seq:<team>`, `trigger-cursor:<team>` | the `_id` index only | file `board.json` (the seq is recovered from `events.jsonl`; the cursor has no twin) |
| `scheduled_bots` | `pkg/cloudsched` | `ScheduledBot` | `tenant_id` — a team | none unique, none TTL | none (cloud-only) — the in-process store is test-only; `~/.iterion/schedules.yaml` is the host counterpart |
| `schedule_launch_intents` | `pkg/cloudsched` | `LaunchIntent` | `schedule.tenant_id` — a team | unique slot `_id`; no TTL | `schedule-intents.json` beside the native board (resident trigger scheduler) |
| `leases` | `pkg/lease` | none — `{_id, owner, expires_at, renewed_at}` | none — deployment-wide names | the `_id` index only | memory-only (which elects every process, so it is for one) |
| `forge_board_bindings` | `pkg/forge` | `BoardBinding` | the `_id` is the team | none unique, none TTL | none (cloud-only) — the in-process store answers the OpenAPI spec generator and tests, no runtime mode |
| `bot_sources` | `pkg/botsource` | `BotSource` | `tenant_id` — a team, or `platform:` | `tenant_slug_unique` | `memory-only` (local mode and tests); the baked catalog is the filesystem tier |
| `"FS: baked catalog"` | `pkg/botregistry` | not a collection | — | not a collection | the bundles shipped in the image, read from `bots/`, `examples/` and `.botz/` under the work dir (or `Bots.Paths`) |
| `repo_integrations` | `pkg/forge` | `RepoIntegration` | `tenant_id` — a team | `tenant_conn_repo_unique` | none (cloud-only) — same as the binding above: spec generator and tests only |
| `runs` | `pkg/store/mongo` | `store.Run` | `tenant_id` — a team; absent on a local or resident-surface run | none unique, none TTL | file `runs/<id>/run.json` |
| `audit_events` | `pkg/audit` | `audit.Event` | `tenant_id` — a **team** on the `schedule.*` rows drawn here, an org on `auditOrg` rows, empty or either on `scope: platform` rows ([identity](data-model-identity.md)) | TTL `audit_ttl`, 400 days | none (cloud-only); the tick records go to the local JSONL |

The three board collections share one filesystem twin, the native tracker's `<store-dir>/dispatcher/` directory (`board.json`, `issues/<id>.json`, `events.jsonl`): the same `native.Issue` and `native.Event` values, stored on both sides — under their json tags on disk, the lowercased Go field names in Mongo.

## Related

- [dispatcher.md](../dispatcher.md) — the dispatcher actor, the card state machine, claim selection, the claim lease and its watchdog; [native-tracker.md](../native-tracker.md) — the filesystem twin in full: layout, board schema, the `iterion issue` CLI, the ingest contract.
- [scheduling.md](../scheduling.md) — the host manifest, overlap policy, keepalive, the guard command, retry precedence.
- [bot-invocations.md](../bot-invocations.md) — the manifest capability a subscription is derived from; [cloud-rest-api.md](../cloud-rest-api.md) — the schedule and trigger routes.
- [github-board-sync.md](../github-board-sync.md) — the projection effect and the reconciliation pass that share the outbox; [agents/engine/automation-surfaces.md](../agents/engine/automation-surfaces.md) — the domain index for the four trigger families.
- [adr/046-event-driven-runs-trigger-spine.md](../adr/046-event-driven-runs-trigger-spine.md), [adr/094-trigger-effect-outbox.md](../adr/094-trigger-effect-outbox.md), [adr/096-board-claim-lease-and-watchdog.md](../adr/096-board-claim-lease-and-watchdog.md), [adr/097-github-projects-v2-board-sync.md](../adr/097-github-projects-v2-board-sync.md).
