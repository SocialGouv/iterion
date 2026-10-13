# Data model — forges, repositories and bots

Enabling a bot on a repository binds it in two documents — the webhook config that enforces the binding first, the integration row that records the repo's own choices second, with no transaction over the pair — and the bots they name are slugs a launch resolves through three tiers, never stored keys.

> Drawn from the code at commit `61a305262` — main at v3.266.1, after #2391, which carries the launch rung; how this page is kept true: [README](README.md).

## Block 1 — Revi bound to a repository, inside a team

```mermaid
erDiagram
    forge_oauth_apps {
        string _id PK
        string tenant_id FK "teams._id"
        string provider "gitlab | github | forgejo"
        string forge_base_url
        string owner_login "the GitHub org or user that OWNS the App"
        bool security_read_only
        string client_id "in the clear; not a secret"
        bytes sealed_secret "AAD forge_oauth_app:<id>"
        bytes sealed_private_key
    }
    forge_connections {
        string _id PK
        string tenant_id FK "teams._id"
        string provider "gitlab | github | forgejo"
        string kind "oauth_app | github_app | pat"
        string purpose "empty is runtime | security_read"
        string status "active | needs_reauth | revoked | degraded"
        string forge_base_url
        string installation_account "the org the App is installed ON, not the App's own handle"
        string oauth_app_id FK "forge_oauth_apps._id"
        string managed_secret_id FK "generic_secrets._id"
        bool security_read_enabled
        string security_read_org_key "the dependabot_tokens map key"
        bytes sealed_payload "AAD forge_conn:<id>"
        datetime access_token_expires_at "in the clear"
    }
    repo_integrations {
        string _id PK
        string tenant_id FK "teams._id"
        string connection_id FK "forge_connections._id"
        string repo_full_name UK "owner/repo - unique with tenant_id and connection_id"
        string[] bot_ids "bot slugs - never a key"
        string[] events_normalized
        string webhook_id FK "webhook_configs._id"
        string hook_id "the forge-side hook id"
        string managed_secret_id FK "generic_secrets._id"
        map launch_vars
        string[] hold_labels
        string[] label_allowlist
        bool auto_fix_on_gate_failure
        string overlap "allow | skip | supersede | keepalive"
        bool sync_issues_enabled
        string min_author_role
    }
    webhook_configs {
        string _id PK
        string tenant_id FK "teams._id"
        string token_hash UK "unsalted SHA-256 of the iwh_ token"
        string provider "gitlab | github | forgejo | generic"
        string sign_mode "empty is token | hmac"
        bytes hmac_secret_sealed
        bool enabled
        string[] bot_ids
        object bot_rules "an ARRAY of bot rules"
        string[] label_allowlist
        map command_map "slash command to bot routes"
        map secret_overrides FK "workflow secret name to generic_secrets._id"
        map key_overrides FK "provider to api_keys._id"
        string provisioned_by "forge:<connection id>"
        bool review_on_sync
    }
    forge_oauth_apps |o--o{ forge_connections : "oauth_app_id"
    forge_connections ||--o{ repo_integrations : "connection_id"
    repo_integrations |o--|| webhook_configs : "webhook_id"
    webhook_configs |o..o{ runs : "owner_id as '<lane>:<config id>' - 'webhook:', 'autofix:' or 'gaterelaunch:' for example - on a run a lane of the webhook config launches"
    webhook_configs }o..o| forge_connections : "provisioned_by - forge:<connection id>; empty on a hand-created config"
    forge_connections |o--o| generic_secrets : "managed_secret_id"
    repo_integrations }o--|| generic_secrets : "managed_secret_id"
    webhook_configs }o--o{ generic_secrets : "secret_overrides - a workflow secret name per entry"
    forge_connections }o..o| generic_secrets : "generic_secrets.name - the team-wide row named dependabot_tokens the store lists first (created_at order on the Mongo store), shared by every github_app connection that opted in"
    teams ||--o{ bot_secret_bindings : "tenant_id - one row per (tenant, bot, secret name), TEAM-GLOBAL, repointed by the latest provision"
    bot_secret_bindings }o--|| generic_secrets : "secret_id"
    teams ||--o{ forge_oauth_apps : "tenant_id"
    teams ||--o{ forge_connections : "tenant_id"
    teams ||--o{ repo_integrations : "tenant_id"
    teams ||--o{ webhook_configs : "tenant_id"
    repo_integrations }o..o{ bot_sources : "bot_ids slugs - the team row, then the platform: row"
    repo_integrations }o..o{ "FS: baked catalog" : "bot_ids slugs - the last tier, resolved at launch"
    webhook_configs }o..o{ bot_sources : "bot_rules[].bot_id, resolved under tenant_id at every delivery"
    webhook_configs }o..o{ "FS: baked catalog" : "bot_rules[].bot_id - the last tier, resolved at every delivery"
```

- Six settings sit on both stores — hold labels, the label allowlist, overlap, the operator's launch vars (`operator_launch_vars` on the config), `bot_ids` and `min_author_role`. Which copy counts is a property of the lane, not of the store: every runtime reader of hold labels, the label allowlist and overlap takes the config's copy, while the zero-touch gate lane reads the INTEGRATION's `auto_fix_on_gate_failure` and its `launch_vars[gate_context]` pin. Nothing sets `min_author_role` on either store — the provision only carries the integration's existing value forward — so every lane reads it empty.
- `bot_ids` and `bot_rules[].bot_id` are slugs, not keys: nothing in Mongo resolves them. A launch tries the launching TEAM's `bot_sources` row, then the row under tenant `platform:`, then the baked catalog on disk — hence the dotted edges, and why the same slug can mean different code for two teams.
- The stored uniqueness is `(tenant_id, connection_id, repo_full_name)`; the reverse `(tenant_id, webhook_id)` index is NOT unique, so one webhook per integration is a provisioning invariant rather than a constraint.

## Block 2 — the inbound delivery lifecycle

```mermaid
erDiagram
    webhook_deliveries {
        string _id PK
        string tenant_id FK "teams._id"
        string webhook_id FK "webhook_configs._id"
        string idempotency_key UK "unique GLOBALLY, not per tenant"
        string event_kind
        string subject_id "prefixed, for example pr: | mr: | issue: | comment: | note:"
        string payload_hash "SHA-256"
        string status "accepted | invalid | filtered | launched | launch_error"
        string bot_id "the slug"
        string run_id FK "runs._id"
        int attempts
        datetime received_at
        datetime launched_at
    }
    webhook_deferred_launches {
        string _id PK "tenant|webhook|projectPath|subject"
        string tenant_id FK "teams._id"
        string webhook_id FK "webhook_configs._id - also the second _id segment"
        int64 generation
        datetime fire_at
        datetime claimed_until
        int attempts
        string order_key "the FORGE's own event timestamp, never arrival order"
        object targets "an ARRAY of deferred targets"
        string public_base
        datetime created_at
    }
    webhook_quotas {
        string _id PK "org|<team id>|<YYYY-MM> or wh|<team id>|<webhook id>|<YYYY-MM>"
        int count
    }
    webhook_configs ||--o{ webhook_deliveries : "webhook_id"
    webhook_configs ||--o{ webhook_deferred_launches : "webhook_id"
    webhook_configs |o..o{ webhook_quotas : "_id - its webhook segment, in the wh| key shape only"
    teams ||--o{ webhook_deliveries : "tenant_id"
    teams ||--o{ webhook_deferred_launches : "tenant_id"
    teams ||..o{ webhook_quotas : "_id - its team segment, in both key shapes"
    webhook_deliveries }o--o| runs : "run_id"
    webhook_deferred_launches }o..o{ runs : "no field: the sweep launches each target as the handler would have"
```

- The `org|` prefix of a quota row names a key shape, not an `orgs` document: the segment is the webhook config's `tenant_id`, a TEAM.
- The debounce row's `_id` IS the subject key, so one push volley costs one review.
- Only `accepted`, `launched` and a post-insert `launch_error` occupy the lane's key: the refusal statuses — `invalid`, `filtered`, and a `launch_error` the launch gate denied before the insert — are filed under a random uuid.

## Block 3 — bot-definition tiers and team extensions

```mermaid
erDiagram
    bot_sources {
        string _id PK
        string tenant_id FK "a team id, or the platform: sentinel"
        string slug UK "unique per tenant, never globally"
        map files "bundle path to content"
        string origin "tenant | forked:<catalog id>"
        int version
        datetime updated_at
    }
    bot_source_versions {
        string _id PK "<row id>:<version>"
        string tenant_id FK "a team id, or platform:"
        int version
        string slug
        map files
        datetime created_at "the WRITE time, not the row's"
    }
    plugin_sources {
        string _id PK
        string tenant_id FK "teams._id"
        string name UK "unique per tenant"
        string git_url
        string ref
        string secret_id FK "generic_secrets._id"
        bool enabled
        string degraded_reason
    }
    platform_settings {
        string _id PK "family: bot_roles|runner_pools|sandbox|bot_vars|platform_credentials|budget_floor|usage_caps"
        string reviewer "bot_roles"
        string revi_converse "bot_roles"
        string brancher "bot_roles"
        string implementer "bot_roles"
        map vars "bot_vars"
        object pools "an ARRAY of pool rows - runner_pools"
    }
    marketplace {
        string _id PK "slug, the normalised manifest name - or <slug>@<team id>"
        string kind "bot | plugin"
        string repo_url
        string[] tags
        string scope
        string org_id FK "teams._id - the TEAM despite the name"
        string status
        int installs
    }
    bot_sources |o..o{ bot_source_versions : "_id - <row id>:<version>, kept after the row is deleted"
    teams |o--o{ bot_sources : "tenant_id - platform: on a deployment-wide row"
    teams ||--o{ plugin_sources : "tenant_id"
    plugin_sources }o--o| generic_secrets : "secret_id"
    teams |o--o{ marketplace : "org_id - stamped in cloud mode only; empty on a local or legacy entry"
    platform_settings |o..o{ bot_sources : "platform_settings.reviewer and the three other role slugs of the bot_roles family"
    bot_sources }o..o| "FS: baked catalog" : "origin forked:<catalog id>"
    plugin_sources }o..o{ "external: git repository" : "git_url plus ref"
    marketplace }o..o{ "external: git repository" : "repo_url plus ref plus subpath"
```

- `platform:` is an ordinary row in the same collection under a sentinel tenant, not a second collection.
- The baked tier is a FILESYSTEM: the tiered resolver reads the configured bot roots — by default `bots/`, `examples/` and `.botz/` under the work dir.
- `platform_settings` holds one document per family and carries no tenant at all. Sovereign runner pools are a FAMILY, not a collection.
- `marketplace` carries no `tenant_id`. `org_id` is where the tenant hides, and it holds the submitting TEAM: the viewer's reach is their own team id, so an `org`-scoped entry is visible to ONE team, never org-wide.

## Block 4 — the board binding and the provisioning gate

```mermaid
erDiagram
    forge_board_bindings {
        string _id PK, FK "teams._id - the tenant IS the id"
        string provider
        string owner
        string connection_id FK "forge_connections._id"
        string project_id
        string status_field_id "the board's status single-select"
        map status_options "native state to board option id"
        object status_mapping "an ARRAY of column and native-state pairs, not a map"
        string[] missing_statuses
        string[] unresolved_at_bind
        int64 sync_every_seconds
        datetime last_synced_at
        datetime sync_lease_until
        string degraded_reason
    }
    forge_provision_approvals {
        string _id PK
        string org_id FK "orgs._id"
        string tenant_id FK "teams._id"
        string connection_id FK "forge_connections._id"
        string repo_full_name UK "unique with tenant_id and connection_id"
        string[] bot_ids "the slugs"
        string integration_id FK "repo_integrations._id"
        string[] base_bot_ids
        bool replace
        map schedule_crons
        string requested_by
        datetime created_at
    }
    teams ||--o| forge_board_bindings : "forge_board_bindings._id"
    forge_board_bindings }o--|| forge_connections : "connection_id"
    orgs ||--o{ forge_provision_approvals : "org_id"
    teams ||--o{ forge_provision_approvals : "tenant_id"
    forge_provision_approvals }o--|| forge_connections : "connection_id"
    forge_provision_approvals }o--o| repo_integrations : "integration_id"
    teams }o--o| orgs : "org_id - empty on a team with no parent org"
```

- The binding's `_id` IS the team id, so one board per team needs no unique index.
- An approval row exists while the decision is pending.

## Collections

| Collection | Package | Document struct | Tenant field (values) | Unique / TTL indexes | Twin |
|---|---|---|---|---|---|
| `forge_oauth_apps` | `pkg/forge` | `ForgeOAuthApp` | `tenant_id` — a team id | unique `(tenant_id, provider, forge_base_url, owner_login, security_read_only)`; the two legacy indexes are dropped by name first | `none (cloud-only)` |
| `forge_connections` | `pkg/forge` | `Connection` | `tenant_id` — a team id | none unique; `tenant_provider`, `token_expiry` | `none (cloud-only)` |
| `repo_integrations` | `pkg/forge` | `RepoIntegration` | `tenant_id` — a team id | `tenant_conn_repo_unique` on `(tenant_id, connection_id, repo_full_name)`; `tenant_webhook` | `none (cloud-only)` |
| `webhook_configs` | `pkg/webhooks` | `Config` | `tenant_id` — a team id | `token_hash_unique`; `tenant_created`; no TTL | `none (cloud-only)` |
| `webhook_deliveries` | `pkg/webhooks` | `Delivery` | `tenant_id` — a team id | `idempotency_unique` on the key ALONE; TTL `deliveries_ttl` on `received_at`, 90 days | `none (cloud-only)` |
| `webhook_deferred_launches` | `pkg/webhooks` | `DeferredLaunch` | `tenant_id`, and the first `_id` segment — a team id | none unique; `deferred_fire_at`; TTL `deferred_ttl` on `created_at`, 7 days | `none (cloud-only)` |
| `webhook_quotas` | `pkg/webhooks` | none — `MongoCounter` writes `{_id, count}` | none — the team is an `_id` segment | none declared, no TTL | `none (cloud-only)` |
| `bot_sources` | `pkg/botsource` | `BotSource` | `tenant_id` — a team id, or `platform:` | `tenant_slug_unique` on `(tenant_id, slug)` | `memory-only` |
| `bot_source_versions` | `pkg/botsource` | `BotSource`, `_id` rewritten to `<row id>:<version>` | `tenant_id` — a team id, or `platform:` | none unique; TTL `created_at_ttl`, 30 days | `memory-only` |
| `plugin_sources` | `pkg/pluginsource` | `PluginSource` | `tenant_id` — a team id | `tenant_name_unique` on `(tenant_id, name)`; `tenant_enabled` | `none (cloud-only)` |
| `marketplace` | `pkg/marketplace` | `Entry` | `org_id` — the submitting TEAM id, stamped on every cloud submission (empty on a local or legacy entry); the owner when `scope` is `org` | none unique; seven query indexes including `text_search` and `moderation_queue` | `file marketplace.json` |
| `platform_settings` | `pkg/platformcfg`, `pkg/usagecap` | one struct per family — `BotRoles`, `RunnerPools`, `Sandbox`, `BotVars`, `PlatformCredentials`, `budgetfloor.Policy` (the one family `pkg/platformcfg` stores whose type lives outside it), the usage caps | none — deployment-wide; the `_id` is the family | none declared, no TTL | `none (cloud-only)` |
| `forge_board_bindings` | `pkg/forge` | `BoardBinding` | the `_id` **is** the team id | none unique (the `_id` is the key); `board_binding_due` | `none (cloud-only)` |
| `forge_provision_approvals` | `pkg/forge` | `ProvisionApproval` | `tenant_id` — the requesting team; `org_id` — the approving org | `tenant_conn_repo_unique`; `org`; no TTL | `none (cloud-only)` |
| `teams` | `pkg/identity` | `Team` | the `_id` **is** the tenant | stub here | see [data-model-identity.md](data-model-identity.md) |
| `orgs` | `pkg/identity` | `Org` | the `_id` **is** the org | stub here | see [data-model-identity.md](data-model-identity.md) |
| `runs` | `pkg/store/mongo` | `store.Run` | `tenant_id` — a team id | stub here | see [data-model-runs.md](data-model-runs.md) |
| `generic_secrets` | `pkg/secrets` | `GenericSecret` | `tenant_id` — a team id | stub here | see [data-model-credentials.md](data-model-credentials.md) |
| `bot_secret_bindings` | `pkg/secrets` | `BotSecretBinding` | `tenant_id` — a team id | stub here | see [data-model-credentials.md](data-model-credentials.md) |
| `"FS: baked catalog"` | `pkg/botregistry` | not a collection | — | not a collection | the bundles shipped in the image, read from `bots/`, `examples/` and `.botz/` under the work dir; `//go:embed` carries four paths only |
| `"external: git repository"` | not a collection | — | — | not a collection | the repository a plugin source or a listing names |

## Related

- [forge-integrations.md](../forge-integrations.md) — connect a forge, enable a repo, the manifest `forge:` block, the normalized-to-native event table, the managed token, the operator-owned settings.
- [forge-permissions.md](../forge-permissions.md) and [forge-security-read.md](../forge-security-read.md) — who a bot acts as per connection kind, least privilege per call family, and the watch-only connection.
- [webhooks.md](../webhooks.md) — the inbound side end to end: token versus HMAC, the lanes, the matching filters, per-bot routing, the idempotency keys, limits and admission, the delivery statuses.
- [platform-bots.md](../platform-bots.md) and [plugins.md](../plugins.md) — the `platform:` override and its precedence, the git-hosted plugin sources and the quarantine contract.
- [github-board-sync.md](../github-board-sync.md) and [adr/097-github-projects-v2-board-sync.md](../adr/097-github-projects-v2-board-sync.md) — binding by address, the status reconciliation, the cadence and the lease.
- [adr/039-per-tenant-sealed-oauth-app-store-replaces-global-env.md](../adr/039-per-tenant-sealed-oauth-app-store-replaces-global-env.md), [secrets.md](../secrets.md), [cloud-rest-api.md](../cloud-rest-api.md) and [agents/engine/tenancy-credentials-spend.md](../agents/engine/tenancy-credentials-spend.md) — the sealed app store, the credential tiers, the route tables and the settings families.

## Sources

- `pkg/forge` — [types.go](../../pkg/forge/types.go) (`Connection`, `Purpose`), [connection_store.go](../../pkg/forge/connection_store.go) (`ConnectionStore`, `ExpiringBefore`), [repo_integration_store.go](../../pkg/forge/repo_integration_store.go) (`RepoIntegration`), [oauth_app_store.go](../../pkg/forge/oauth_app_store.go) (`ForgeOAuthApp`, `EnsureSchema`), [board_binding_store.go](../../pkg/forge/board_binding_store.go) (`BoardBinding`, `ClaimSync`), [provision_approval_store.go](../../pkg/forge/provision_approval_store.go) (`ProvisionApproval`), [orchestrator.go](../../pkg/forge/orchestrator.go) (`Provision`, `ensureManagedSecret`, `ensureBotBinding`, `narrowGitHubAppSecret`, `managedSecretName`, `syncSchedules`).
- `pkg/webhooks` — [types.go](../../pkg/webhooks/types.go) (`Config`, `BotRule`, `CommandRoute`, `Delivery`, `DeferredLaunch`, `DeferredTarget`), [mongo.go](../../pkg/webhooks/mongo.go) (`EnsureSchema`, `GetByIdempotencyKey`, `ClaimFailedRetry`, `Upsert`, `ClaimDue`, `MongoCounter`).
- bots and extensions — [botsource/botsource.go](../../pkg/botsource/botsource.go) (`BotSource`, `PlatformTenantID`, `MaxBundleBytes`), [botsource/mongo.go](../../pkg/botsource/mongo.go) (`snapshotVersion`, `GetVersionAtOrBefore`, `BotSourceVersionTTLDays`), [pluginsource/pluginsource.go](../../pkg/pluginsource/pluginsource.go) (`PluginSource`, `MarkDegraded`), [pluginsource/mongo.go](../../pkg/pluginsource/mongo.go) (`withTenantFilter`), [marketplace/types.go](../../pkg/marketplace/types.go) (`Entry`), [marketplace/mongostore.go](../../pkg/marketplace/mongostore.go), [marketplace/jsonstore.go](../../pkg/marketplace/jsonstore.go), [platformcfg/store.go](../../pkg/platformcfg/store.go) (`FamilyBotRoles`, `FamilyBudgetFloor`, `ColRoutingPolicies`), [platformcfg/platformcfg.go](../../pkg/platformcfg/platformcfg.go) (`BotRoles`, `RunnerPools`), [usagecap/settings_mongo.go](../../pkg/usagecap/settings_mongo.go), [budgetfloor/budgetfloor.go](../../pkg/budgetfloor/budgetfloor.go) (`Policy`).
- `pkg/server` — [bot_resolver.go](../../pkg/server/bot_resolver.go) (`resolveBotTieredRaw`, `resolveBotSource`, `teamBotRow`, `launchBot.Tier`), [webhooks_common.go](../../pkg/server/webhooks_common.go) (`resolveForgeEventBots`, `webhookForgeConnection`), [webhooks_debounce.go](../../pkg/server/webhooks_debounce.go) (`deferSubjectKey`, `dropReplayedTargets`), [platform_settings.go](../../pkg/server/platform_settings.go) (`roleBots`), [forge_publish.go](../../pkg/server/forge_publish.go) (`forgeConnectionForPR`), [middleware_webhook.go](../../pkg/server/middleware_webhook.go), [webhooks_routes.go](../../pkg/server/webhooks_routes.go), [forge_provisioning_routes.go](../../pkg/server/forge_provisioning_routes.go), [forge_approval_routes.go](../../pkg/server/forge_approval_routes.go).
- neighbours — [secrets/generic.go](../../pkg/secrets/generic.go) (`GenericSecret`, `ErrGenericSecretTenantMissing`, `withGenericSecretTenantFilter`), [secrets/bindings.go](../../pkg/secrets/bindings.go) (`BotSecretBinding`, `ErrBindingTenantMissing`), [secrets/local.go](../../pkg/secrets/local.go) (`LocalSecretsFileName`), [store/mongo/tenant.go](../../pkg/store/mongo/tenant.go) (`withTenantFilter`), [connection/store.go](../../pkg/connection/store.go) (`Store`, `MemoryStore`), [connection/file_store.go](../../pkg/connection/file_store.go) (`FileStore`), [identity/types.go](../../pkg/identity/types.go) (`Team`, `Org.RequireProvisionApproval`), [store/run.go](../../pkg/store/run.go) (`Run.BotSourceTier`, `Run.BotSourceTenant`), [botregistry/registry.go](../../pkg/botregistry/registry.go) (`DefaultPaths`, `NormalizeName`), [bots/embed.go](../../bots/embed.go), [bundle/manifest.go](../../pkg/bundle/manifest.go) (`DefaultForgeSecretName`).
