# Data model — memory, config shares and notifications

Three slices of a tenant's own content: the knowledge spaces bots and people share, the scoped config-edit grants a non-operator holds, and the per-user state that decides whose browser a finished run reaches.

> Drawn from the code at commit `61a305262` — main at v3.266.1, after #2391, which carries the launch rung; how this page is kept true: [README](README.md).

## Block A — Shared knowledge (memory)

```mermaid
erDiagram
    memory_spaces {
        string _id PK "v1:<visibility>:<tenant>:<project>:<bot>:<user>:<name>"
        string tenant_id FK "team id | empty"
        string visibility "private | bot | project | cross_project | user | org | global"
        string project_id "encoded project key, not a collection id"
        string bot_id "bot slug, not a collection id"
        string user_id FK "e.g. users._id"
        string name "one path segment, never slashed"
        int64 quota_bytes
        int64 used_bytes
        string mode "markdown"
        int version
        datetime updated_at
    }
    memory_docs {
        object _id PK "{space_id, path}"
        string space_id FK "memory_spaces._id"
        string tenant_id FK "team id | empty"
        string path "relative, canonical, no '..' segment"
        string title
        string description
        string[] tags
        int64 size
        string checksum
        int64 revision
        bytes content "inline markdown"
        string updated_by "a bare user id | 'bot:<bot id>' | 'import', or empty; never resolved as a key"
        datetime updated_at
    }
    memory_tenant_usage {
        string _id PK, FK "teams._id - the tenant IS the id"
        int64 used_bytes
        int64 quota_bytes
    }
    memory_docs }o--|| memory_spaces : "space_id"
    memory_spaces }o--o| memory_tenant_usage : "tenant_id"
    memory_tenant_usage |o--|| teams : "memory_tenant_usage._id"
    memory_spaces }o--o| teams : "tenant_id"
    memory_spaces }o..o| users : "user_id, when it is a plain user id"
    memory_docs }o..o| users : "updated_by, in its bare user-id form"
    runs }o..o| memory_spaces : "bot_id, with the project key, computes the bot-visibility 'auto-memory' space - only when auto-memory is on"
    memory_spaces |o..o| "FS: memory tree" : "no field - the same space on disk when Mongo is absent"
```

- `_id` is the whole identity. Every read is keyed on `_id` or `space_id`: the Mongo adapter adds no `tenant_id` clause.
- `quota_bytes = 0` means **no** sub-cap, never "deny". A `global` space has no tenant, so it skips the aggregate counter.
- The counter the code names the "org aggregate" is one row **per team**: an operator's org ceiling is written onto each team the org has at that moment, and a team created later starts at the 1 GiB default.
- `memory_spaces.bot_id` is, on a `bot` space a run writes, the launch's resolved bot identity (`runview.ResolveBotID`: an explicit bot id, else the bundle's declared name, else the bundle directory a path names; when that leaves it empty, the executor falls back to the workflow's name); the memory routes take it from `?bot=`. Neither is resolved against a collection.

## Block B — Config share: the `iws_` token path

```mermaid
erDiagram
    config_shares {
        string _id PK "uuid"
        string tenant_id FK "team id"
        string bot_id "bot slug, not a collection id"
        string repo_url
        string repo_ref
        string config_path
        string category
        string[] allowed_paths "dotted paths; no globs"
        string[] visible_paths "dotted paths"
        bool read_only
        string token_hash UK "hex SHA-256 of the plaintext, unsalted"
        string token_last4
        string fingerprint
        bool enabled
        string created_by FK "users._id"
        datetime expires_at
        datetime revoked_at "no code path writes it"
        datetime last_used_at
    }
    config_share_deliveries {
        string _id PK "uuid"
        string share_id FK "config_shares._id"
        string tenant_id FK "team id"
        datetime at
        string source_ip
        string user_agent
        string method "PATCH"
        string actor "share:<id> | user:<id>"
        int status "the HTTP status"
        string before_sha
        string after_sha
        string[] changed_paths
        string error
    }
    config_share_deliveries }o--|| config_shares : "share_id"
    config_shares }o--|| teams : "tenant_id"
    config_shares }o--|| users : "created_by"
    config_share_deliveries }o..o| users : "actor, in its user:<id> form"
    config_shares }o..o| generic_secrets : "tenant_id and bot_id - the forge_token they resolve, a bot_secret_bindings row first, else the team secret of that name"
    config_shares }o..o| scheduled_bots : "(tenant_id, bot_id) where vars.category equals category - the oldest match"
    config_shares }o..o{ runs : "config_shares.bot_id, matched case-insensitively against the run's bundle_name, else the basename of bundle_path less .botz, then, when the share names one, the launch var category"
    config_shares }o..|| "external: GitHub" : "repo_url + repo_ref + config_path"
    config_shares }o..o| "FS: baked catalog" : "bot_id slug at mint and on the config-editor share listing: team bot_sources, platform override, then the catalog"
```

- Every field that shapes a write is pinned at mint: `repo_url`, `repo_ref`, `config_path`, `allowed_paths`.
- `token_hash_unique` is the only unique index. Nothing enforces one share per `(bot_id, category)`: an operator may hold several live grants on the same category.
- Rotation rewrites `token_hash`, `token_last4` and `fingerprint` in place; there is no `rotated_at` stamp, so only the `config_share.rotated` audit event dates it.
- Only `PATCH …/config` writes delivery rows. The pre-flight refusals record nothing, so no delivery row does not mean no mutating attempt.

## Block C — User notifications

```mermaid
erDiagram
    notification_prefs {
        ObjectId _id PK
        string tenant_id FK "team id, empty"
        string user_id FK "users._id"
        string scope "own | team"
    }
    push_subscriptions {
        string _id PK "the endpoint"
        string endpoint UK "the push service's capability URL"
        string user_id FK "users._id"
        string tenant_id FK "team id, empty"
        string p256dh
        string auth
        string user_agent
        datetime created_at
        datetime last_used_at
    }
    sent_notifications {
        string _id PK "run:<id>:<interaction or status>:<unix updated_at>, the same key prefixed ops|, ops|run:<id>:parked:<attempts>:<class>, ops|route:<id>:<seq>:<exhausted|action_failed|escalated>"
        datetime sent_at
        bool delivered
    }
    notification_prefs }o--|| users : "user_id"
    notification_prefs }o--o| teams : "tenant_id - empty under DisableAuth"
    push_subscriptions }o--|| users : "user_id"
    push_subscriptions }o--o| teams : "tenant_id"
    runs ||..o{ sent_notifications : "_id embeds the run id; never resolved as a key"
    runs }o..o{ notification_prefs : "notification_prefs.tenant_id against the run's, for the rows whose scope is team"
    runs }o..o{ push_subscriptions : "runs.owner_id and the opt-in user ids, to user_id"
    push_subscriptions }o..|| "external: browser push service" : "endpoint"
```

- Absence is the default. A `notification_prefs` row exists only once a user saved a scope, so no row means `own`; the unique `(tenant_id, user_id)` index is the real identity.
- `push_subscriptions` stores the endpoint twice — as `_id` on insert and as a separately unique-indexed field.
- `sent_notifications` is keyed purely by episode, and four key shapes share it: the user dispatcher's bare run-outcome key, that same key under `ops|`, the operator dispatcher's parked-run key carrying (attempts, failure class), and the outcome router's `ops|route:` alert key.

## Collections

| Collection | Package | Document struct | Tenant field (values) | Unique / TTL indexes | Twin |
|---|---|---|---|---|---|
| `memory_spaces` | `pkg/store/mongo` | `memSpaceDoc` | `tenant_id` — a team id, empty for `visibility: global` | none unique, no TTL (`tenant_vis_recent` has no reader in the adapter, which only ever queries `_id`) | `file .quota/spaces/<checksum of the ref>.json` and the space tree |
| `memory_docs` | `pkg/store/mongo` | `memDoc` (`memDocID`) | `tenant_id` — a team id, omitted on a global space's docs | the composite `_id` is the only uniqueness; no TTL | `file <space root>/<path>` |
| `memory_tenant_usage` | `pkg/store/mongo` | none — written as `bson.M` | the `_id` **is** the team id | no declared index; the implicit `_id` index only | `file .quota/aggregate.json` |
| `config_shares` | `pkg/configshare` | `Share` | `tenant_id` — a team id | `token_hash_unique`; no TTL | `memory-only` |
| `config_share_deliveries` | `pkg/configshare` | `Delivery` | `tenant_id` — a team id, copied from the share | none unique; TTL `configshare_deliveries_ttl` on `at`, 90 days | `memory-only` |
| `notification_prefs` | `pkg/usernotify` | `Prefs` | `tenant_id` — a team id (the identity's active team), empty under DisableAuth | `tenant_user` unique on `(tenant_id, user_id)`; no TTL | `none (cloud-only)` |
| `push_subscriptions` | `pkg/usernotify/webpush` | `Subscription` | `tenant_id` — a team id, provenance only, empty under DisableAuth | `endpoint` unique; no TTL | `none (cloud-only)` |
| `sent_notifications` | `pkg/usernotify` | `SentRecord` | none — keyed by episode | the `_id` insert is the claim; TTL `sent_at_ttl`, 30 days | `none (cloud-only)` |
| `teams` | `pkg/identity` | `Team` | the `_id` **is** the tenant | stub here | see [data-model-identity.md](data-model-identity.md) |
| `users` | `pkg/identity` | `User` | none — a user spans teams | stub here | see [data-model-identity.md](data-model-identity.md) |
| `runs` | `pkg/store/mongo` | `store.Run` | `tenant_id` — a team id | stub here | see [data-model-runs.md](data-model-runs.md) |
| `generic_secrets` | `pkg/secrets` | `GenericSecret` | `tenant_id` | stub here | see [data-model-credentials.md](data-model-credentials.md) |
| `scheduled_bots` | `pkg/cloudsched` | `ScheduledBot` | `tenant_id` | stub here | see [data-model-automation.md](data-model-automation.md) |
| `"FS: memory tree"` | `pkg/memory` | `FSStore` | a path segment for `user`/`org`/`cross_project` only (`local` when empty); `bot` and `project` key on the project, `global` on neither | not a collection | the local twin of every space but `private` |
| `"FS: baked catalog"` | not a collection | — | — | not a collection | the bot bundles shipped in the image |
| `"external: GitHub"` | not a collection | — | — | not a collection | the forge a share commits through |
| `"external: browser push service"` | not a collection | — | — | not a collection | the endpoint a subscription names |

`connection.Connection` is not a collection, here or anywhere. Its only durable store is a `FileStore` over `<store dir>/connections.json`. No ER page draws a `connections` entity and the guard asks for none.

## Related

- [memory-and-knowledge.md](../memory-and-knowledge.md) — the visibilities, the two quota levels, the REST surface and how bots consume memory.
- [config-share.md](../config-share.md) — the grant, the security model and the three route groups; [adr/076-scoped-config-share-editor.md](../adr/076-scoped-config-share-editor.md), [adr/077-declarative-per-bot-config-share-surface.md](../adr/077-declarative-per-bot-config-share-surface.md) and [adr/078-config-editor-role-sso-team-scoped-access.md](../adr/078-config-editor-role-sso-team-scoped-access.md) for why it has this shape.
- [notifications.md](../notifications.md) — which run events notify, recipient resolution, the sweep and the VAPID switch.
- [cloud-rest-api.md](../cloud-rest-api.md) — the memory and notification routes with their auth level.
- [data-model-forge-bots.md](data-model-forge-bots.md) — `plugin_sources`, the durable twin of a team's private plugin and its skills, and the bot tiers a `bot_id` slug resolves through.

## Sources

- [pkg/store/mongo/memory.go](../../pkg/store/mongo/memory.go) — `colMemorySpaces`, `colMemoryDocs`, `colMemoryTenantUsage`, `memSpaceDoc`, `memDoc`, `memDocID`, `validateCloudTenant`, `ensureSpace`, `ensureTenant`, `SetTenantQuota`, `bumpCounter`.
- [pkg/store/mongo/tenant.go](../../pkg/store/mongo/tenant.go) — `withTenantFilter`, which this adapter never calls.
- [pkg/knowledge/scope.go](../../pkg/knowledge/scope.go) — `Visibility`, `knownVisibilities`, `SpaceRef`, `SpaceRef.ID`, `ValidateDocPath`.
- [pkg/knowledge/quota.go](../../pkg/knowledge/quota.go) — `DefaultOrgAggregateQuota`, `DefaultQuotaFor`, `DefaultMaxDocumentSize`.
- [pkg/memory/fsstore.go](../../pkg/memory/fsstore.go) and [pkg/memory/quota_fs.go](../../pkg/memory/quota_fs.go) — `FSStore.scopeFor`, `LegacyBotRef`, the space tree, `ErrUnsupportedVisibility` and the `.quota` sidecars.
- [pkg/server/memory_routes.go](../../pkg/server/memory_routes.go) — the ref stamping and `requireMemoryWriteAuth`.
- [pkg/server/admin_orgs_routes.go](../../pkg/server/admin_orgs_routes.go) — the operator override that reaches the aggregate counter.
- [pkg/backend/model/executor_auto_memory.go](../../pkg/backend/model/executor_auto_memory.go) and [pkg/backend/automemory/automemory.go](../../pkg/backend/automemory/automemory.go) — `autoMemorySpaceRef`, `autoMemoryAttribution`, `SpaceName`, `Mode`, `ResolveSourced`.
- [pkg/backend/model/claw_backend.go](../../pkg/backend/model/claw_backend.go) and [pkg/backend/model/memory_tools.go](../../pkg/backend/model/memory_tools.go) — the `memory:` block's two paths, `ResolveSpaceRef` when it declares a `visibility:` and `LegacyBotRef` when it carries `scope:` alone, and `installWorkspaceMemory`, whose first call is `knowledge.MemoryStore.Root`.
- [pkg/runview/workflow_path.go](../../pkg/runview/workflow_path.go) and [pkg/runview/service_launch.go](../../pkg/runview/service_launch.go) — `ResolveBotID`, `BotIDForPath`, `BotIDForRun`: the identity every launch and resume stamps as `bot_id`.
- [pkg/configshare/types.go](../../pkg/configshare/types.go) — `Share`, `Share.Active`, `Delivery`, `Store`.
- [pkg/configshare/mongo.go](../../pkg/configshare/mongo.go) and [pkg/configshare/memory.go](../../pkg/configshare/memory.go) — `colShares`, `colShareDeliveries`, `EnsureSchema`, `MemoryStore`.
- [pkg/server/config_shares_routes.go](../../pkg/server/config_shares_routes.go) — `handleCreateConfigShare`, `handleRotateConfigShare`, `handleDeleteConfigShare`, `handleConfigShareDeliveries`, `loadTeamShare`.
- [pkg/server/config_share_routes.go](../../pkg/server/config_share_routes.go) — `applyShareEditAndRespond`, `recordShareDelivery`, `shareFileClient`.
- [pkg/server/middleware_configshare.go](../../pkg/server/middleware_configshare.go) — `configShareAuth`, the per-share rate bucket and the `last_used_at` touch.
- [pkg/secrets/bindings.go](../../pkg/secrets/bindings.go) — `ResolveGenericWithBindings` and the tier order a share's `forge_token` walks.
- [pkg/server/bot_resolver.go](../../pkg/server/bot_resolver.go) — `teamBotManifest` and `teamBotRow`, the team tier a `bot_id` slug resolves through.
- [pkg/server/config_editor_routes.go](../../pkg/server/config_editor_routes.go) — `findCategorySchedule`, `handleConfigEditorRuns`.
- [pkg/usernotify/prefs.go](../../pkg/usernotify/prefs.go) and [pkg/usernotify/mongostore.go](../../pkg/usernotify/mongostore.go) — `Prefs`, `Scope`, `PrefsCollectionName`, `SentCollectionName`, `MongoSentStore.TryMark`.
- [pkg/usernotify/sent.go](../../pkg/usernotify/sent.go) — `SentRecord`, `ClaimGrace`.
- [pkg/server/notifications_routes.go](../../pkg/server/notifications_routes.go) — the enrolment and prefs routes, and their 404 without web push.
- [pkg/usernotify/dispatcher.go](../../pkg/usernotify/dispatcher.go) — the episode claim, `pauseSuppressor` and `ListTeamWide`.
- [pkg/usernotify/webpush/store.go](../../pkg/usernotify/webpush/store.go) and [pkg/usernotify/webpush/sink.go](../../pkg/usernotify/webpush/sink.go) — `Subscription`, `SubscriptionsCollectionName`, the 404/410 prune.
- [pkg/trigger/runoutcome.go](../../pkg/trigger/runoutcome.go), [pkg/alert/opsdispatch.go](../../pkg/alert/opsdispatch.go) and [pkg/server/outcome_router.go](../../pkg/server/outcome_router.go) — `RunOutcomeEventID`, `opsEpisodePrefix`, `opsEpisodeKey`, `NotifyOperator`, `notifyRouteDecisionErr`: every `sent_notifications` key is minted forward from a run, and nothing parses one back.
- [pkg/cloud/orgsweep/sweep.go](../../pkg/cloud/orgsweep/sweep.go) — `teamScopedCollections` and the purge filter.
- [cmd/iterion/server.go](../../cmd/iterion/server.go) — the store wiring and which `EnsureSchema` calls the VAPID switch gates.
- [pkg/connection/file_store.go](../../pkg/connection/file_store.go) and [pkg/connection/local.go](../../pkg/connection/local.go) — `FileStore`, `DefaultPath`, `LocalTenant`, `LocalResolver`: the connector connection model, filesystem-only.
