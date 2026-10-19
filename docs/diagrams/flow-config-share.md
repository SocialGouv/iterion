# Flow — config-share: the mint, the two access paths, the fail-closed write

A config-share is one grant that lets a non-operator edit a declared allow-list of fields in one JSON file of one repository — through a capability URL or a signed-in least-privilege session — and nothing else in iterion.

> Drawn from the code at commit `f564846d3`. No gate compares this page's prose with the code: the commit above is the claim a redraw must re-verify by hand. [Conventions](README.md).

## The mint

An operator who passes `canManageTeam` mints the grant, and the bot's own `manifest.yaml` decides what the grant may contain: a declared `config_share:` block is authoritative, so the operator supplies only a bot and a category.

```mermaid
flowchart TD
    W{"configShares and genericSecrets and sealer and authSvc all wired"} -- "no" --> WX["no route is registered at all, and the studio card is hidden by config_shares_enabled"]
    W -- "yes" --> G{"canManageTeam, with synthetic principals refused first"}
    G -- "no" --> G403["403 admin or owner required"]
    G -- "yes" --> BODY{"a 64 KiB body cap, then bot_id"}
    BODY -- "over the cap, unparsable, or no bot_id" --> BB400["400 invalid body, or 400 bot_id required"]
    BODY -- "ok" --> V{"RepoSlug, then ValidateRepoRef"}
    V -- "reject" --> B400["400 carrying the validator message"]
    V -- "ok" --> S{"the bot declares a config_share block"}
    S -- "yes" --> D{"DeriveGrant expands the category placeholder and narrows to the selected editable fields"}
    S -- "no" --> K["keep the operator-supplied paths, visible defaulting to allowed"]
    D -- "no category, bad category, undeclared field, leftover brace, no editable path left" --> B400
    D -- "ok" --> P{"ValidateConfigPath, then ValidatePaths"}
    K --> P
    P -- "reject" --> B400
    P -- "ok" --> Q{"derived and category-scoped: probe the real file with ProjectedRead"}
    Q -- "projection empty" --> B400
    Q -- "non-empty, or the forge is unreachable" --> M["MintToken, Store.Create, auditTenant config_share.created"]
    M --> R201["201 with the plaintext token once, plus the capability URL"]
```

## The two access paths and the one write

Two structurally different principals — a synthetic `auth.KindShare` bearer on JWT-bypassed public routes, and a cookie session holding the orthogonal `config_editor` capability — converge on `applyShareEditAndRespond`, which answers and audits identically for both so the surfaces cannot drift.

```mermaid
flowchart TD
    subgraph tok["Capability URL, Bearer header only, no cookie"]
        T1["extractInitialToken takes the token from the URL fragment, or the ?token= query fallback, or sessionStorage; keeps it only when it starts with iws_, strips both the hash and ?token= with replaceState, and caches it per share in sessionStorage"]
        T1 --> T2{"per-IP bucket, rate 1 burst 30, before any store lookup"}
        T2 -- "exhausted" --> E429["429 rate limited"]
        T2 -- "ok" --> T3{"an id is present and the Bearer value carries the iws_ prefix"}
        T3 -- "no" --> E401["401 invalid_share, the one uniform answer"]
        T3 -- "yes" --> T4{"GetByID, constant-time VerifyToken, Active at now"}
        T4 -- "unknown id, wrong token, disabled, revoked or expired" --> E401
        T4 -- "ok" --> T5{"per-share bucket"}
        T5 -- "exhausted" --> E429
        T5 -- "ok" --> T6["stamp the synthetic KindShare identity and the tenant, and touch last_used_at in the background"]
    end
    subgraph ses["Signed-in session, ADR-078"]
        S1{"canEditConfigShares: the config_editor capability on this team, or anyone who already manages it"}
        S1 -- "no" --> S403["403 forbidden"]
        S1 -- "yes" --> S2{"the share belongs to the path team and is Enabled"}
        S2 -- "no" --> S404["404 not found"]
        S2 -- "yes" --> S3["a real user principal - canEditConfigShares refuses a synthetic one up front"]
    end
    T6 --> RT{"route"}
    S3 --> RT
    RT -- "GET meta, token path only" --> MET["answered from the pinned record alone: no forge call and no delivery row"]
    RT -- "GET shares, session path only" --> LS["the Enabled rows of the team, reduced by editorShareView plus the bot manifest branding"]
    RT -- "GET runs, session path only" --> RN["editorRunRows: status and timestamps only, category-scoped, the first eight"]
    RT -- "GET config" --> RFC{"resolveShareFC, then ProjectedRead"}
    RFC -- "no forge token, or the read or the parse fails" --> R502["502 config source unavailable or read failed, and no delivery row"]
    RFC -- "ok" --> RD["a fresh object built from visible_paths, with sinks pruned at any depth"]
    RT -- "GET schedule, session path only" --> SG{"the schedule store is wired"}
    SG -- "no" --> SS404["404 schedules not available"]
    SG -- "yes" --> SGR["a pure read: 200 with exists, plus the cron, disabled and next_fire_at when a row matches - no write and no audit"]
    RT -- "PATCH schedule, session path only" --> PRESCH{"read_only, the schedule store, a 4 KiB body cap, an empty or invalid cron"}
    PRESCH -- "no store" --> SS404
    PRESCH -- "read_only" --> SF403["403 read_only"]
    PRESCH -- "bad body or cron" --> SB400["400 invalid body, cron required, or the ValidateCron message"]
    PRESCH -- "ok" --> SCH{"findCategorySchedule matches the bot and vars.category"}
    SCH -- "no match" --> SC404["404 no schedule for this category"]
    SCH -- "match" --> CR["NextFire, then the cron and the next fire updated alone, audit schedule.updated"]
    RT -- "PATCH config" --> PRE{"read_only, a body over 128 KiB, an empty patch, a missing sha"}
    PRE -- "refused" --> PX["403 read_only or 400, before any forge call and therefore before any delivery row"]
    PRE -- "ok" --> AP["applyShareEditAndRespond"]
    AP --> FC{"resolveShareFC: the team forge_token through ResolveGenericWithBindings"}
    FC -- "no sealer, or no forge_token bound" --> F502["502 config source unavailable, and no delivery row"]
    FC -- "ok" --> IM{"ApplyEdit re-reads the file: the caller sha equals the live blob sha"}
    IM -- "no" --> C409["409 carrying the fresh projection for a diff"]
    IM -- "yes" --> AL{"collectPatchLeaves walks the whole patch fail-closed"}
    AL -- "an off-list key, an object on a leaf grant, sinks or a prototype key at any depth" --> V400["400 field not editable, with no path echoed back"]
    AL -- "ok" --> LF{"ValidateLeaf on every changed leaf"}
    LF -- "reject" --> V400
    LF -- "ok" --> PUT{"PutFile with the previous sha, the pinned branch and the fixed share-editor author"}
    PUT -- "conflict" --> C409
    PUT -- "any other failure" --> W502["502 config write failed"]
    PUT -- "written" --> OK200["200 with the new sha and the changed paths"]
    C409 --> DEL["one Delivery row, written synchronously on a detached context"]
    V400 --> DEL
    W502 --> DEL
    OK200 --> DEL
```

## Steps

| Actor | Action | Code |
|---|---|---|
| team admin | mints the grant: the gate, a 64 KiB body, `bot_id`, the target validators, the bot's declared surface, the category probe, then one row, one plaintext token and one audit event | `handleCreateConfigShare`, `botConfigShareSpec`, `DeriveGrant`, `MintToken` |
| editor, token path | opens the capability URL; the client takes the token from the fragment, the `?token=` fallback or `sessionStorage`, and keeps it out of the URL bar | `extractInitialToken`, `shareFetch` |
| middleware | per-IP bucket, the `iws_` prefix, `GetByID`, a constant-time verify and `Active`, then the per-share bucket, the synthetic principal and a detached `last_used_at` touch | `configShareAuth`, `bearerConfigShareToken`, `VerifyToken`, `Share.Active` |
| editor, session path | signs in and lists the team's enabled shares; every other session route re-loads the share through the same gate and the same team check | `handleConfigEditorList`, `loadEditableShare`, `canEditConfigShares` |
| server | reads: a fresh projection over `visible_paths`, plus the whole-file sha that becomes the if-match token | `handleConfigShareGet`, `handleConfigEditorGet`, `Service.ProjectedRead` |
| server | pre-flight on a write: `read_only`, the 128 KiB body cap, an empty patch, a missing sha — all before any forge call | `handleConfigSharePatch`, `handleConfigEditorPatch` |
| server | the one write both paths take: resolve the team `forge_token`, re-read the file, walk the patch fail-closed, validate every changed leaf, commit against the previous sha | `applyShareEditAndRespond`, `resolveShareFC`, `Service.ApplyEdit`, `collectPatchLeaves`, `ValidateLeaf`, `AdminClient.PutFile` |
| server | records all four write outcomes — conflict, rejection, forge failure, success — as one row, synchronously, on a detached context | `recordShareDelivery` |
| editor, session path | retunes the cadence of its own category, and reads the reduced run view to see the effect | `handleConfigEditorPatchSchedule`, `findCategorySchedule`, `ValidateCron`, `editorRunRows` |

## The grant's lifecycle

```mermaid
stateDiagram-v2
%% vocabulary: configshare.Share
    [*] --> active: mint sets Enabled true and ExpiresAt to now plus expires_days, 14 by default
    [*] --> never_expiring: mint with never_expires leaves ExpiresAt at the zero time, which Active reads as unexpiring
    active --> active: rotate re-mints the token hash, so every link handed out earlier dies at once
    never_expiring --> never_expiring: rotate
    active --> expired: now reaches ExpiresAt
    expired --> expired: the session path still reads and writes it - loadEditableShare gates on Enabled alone
    active --> [*]: DELETE removes the row - its delivery rows stay until the 90-day TTL reaps them
    never_expiring --> [*]: DELETE removes the row
    expired --> [*]: DELETE removes the row
    unreachable: disabled and revoked - read by Active, by loadEditableShare, by the editor listing and by the operator shareView, written by no handler
```

| State | Predicate | Entered by, left by |
|---|---|---|
| `active` | `Enabled`, `RevokedAt == nil`, `now` before `ExpiresAt` | the mint; left by the clock, or by `DELETE` |
| never-expiring | a zero `ExpiresAt`, which `Share.Active` treats as unexpiring | `never_expires` at mint; left only by `DELETE` — rotate cuts the links handed out earlier and keeps the share unexpiring |
| expired | `now` at or after `ExpiresAt` | the clock; the token path collapses to the uniform 401, while the session path keeps editing it |
| disabled, revoked | `!Enabled`, `RevokedAt != nil` | unreachable: four sites read them — `Active`, `loadEditableShare`, the editor listing and the operator `shareView`, the only one that exposes both to a client — and no handler writes either: `Enabled: true` at mint is the only assignment |
| `Delivery.Status` | not a lifecycle | the HTTP code of the one call that wrote the row; append-only, never transitioned |

## Guards that refuse

- **Feature wiring** — the public, operator and config-editor routes are registered together or not at all, on the same predicate `server_info` reports as `config_shares_enabled`; without it the studio card is hidden and every path is a 404.
- **`canManageTeam`** — mint, rotate and delete answer `403`; rotate and delete then answer `404` when the share's `TenantID` is not the path team. The list and the deliveries route take `canViewTeam` instead, and neither is audited.
- **`Identity.IsSynthetic`** — `canViewTeam`, `canManageTeam`, `canEditConfigShares` (hence `canEditBots`), `canViewOrg` and `canManageOrg` all refuse a `KindShare` principal up front, so an `iws_` token reaches nothing beyond its own three routes — not even the session editor built on the same service.
- **Target shape** — `RepoSlug`, `ValidateRepoRef`, `ValidateConfigPath` and `ValidatePaths` answer `400`: a clean `owner/name`, an explicit branch with no leading dash and no `..`, a normalized relative path outside `.git/`, `.github/`, `.gitlab-ci`, `dockerfile` and `.env`, and literal leaf grants with writable inside readable and none a prefix of another.
- **The mint's first two refusals** — a 64 KiB body cap answers `400 invalid body` and an absent bot answers `400 bot_id required`, both before any validator runs.
- **`DeriveGrant`** — `400` for a missing or malformed category, a field the bot never declared editable, a template whose placeholder stayed unresolved, or a declared surface that expands to no editable path at all.
- **The category probe** — `400 category … has no editable fields`, so a typo never hands out a link to an empty form; a forge outage does not block the mint.
- **Rate** — a per-IP bucket answers `429` before any store lookup, a per-share bucket answers `429` once the token is proven.
- **The uniform `401 invalid_share`** — an absent store, an empty id, a non-`iws_` bearer, an unknown id, a hash mismatch, `!Enabled`, a set `RevokedAt` and an expiry all collapse to it, so neither the id space nor the lifecycle is probeable.
- **Write pre-flight** — `403 read_only`, a 128 KiB body cap, `400 patch has no editable field` and `400 sha required` all answer before the forge is called, and therefore before any `Delivery` row exists.
- **The write itself** — a stale sha answers `409` with the fresh projection; `collectPatchLeaves` and `ValidateLeaf` answer `400 field not editable` with no path echoed; the forge answers `502 config write failed`. Each of the four outcomes writes one `Delivery` row — but a missing `forge_token` answers `502 config source unavailable` before `ApplyEdit` and leaves no row at all.
- **`sinks` and the prototype keys** — refused inside a patch value by `rejectForbiddenIn` and stripped from a projection by `pruneForbidden`, at any depth, whatever the grant's shape.
- **The read** — both `GET config` handlers answer `502 config source unavailable` when the forge client cannot be resolved and `502 read failed` when the read or the JSON parse fails; neither writes a `Delivery` row.
- **The cadence** — `403 read_only` and `404 schedules not available` answer first, then a 4 KiB body cap, `400 cron required` and `ValidateCron`; only then may a `config_editor` change the cron of the schedule whose `vars.category` equals its share's category, and no match is `404 no schedule for this category`, never an auto-creation. The `GET` is a pure read: `200` carrying `exists`, no `404` on a miss, no write and no audit.

## Collections touched

`config_shares` (written at mint, rotate, delete and the `last_used_at` touch; a unique `token_hash` index) · `config_share_deliveries` (one append-only row per write outcome, under a 90-day TTL) · `audit_events` (`config_share.created`, `.rotated`, `.deleted`, and `schedule.updated` from the editor) · `memberships`, `org_memberships` and `teams` (read by `canManageTeam`, `canViewTeam`, `canEditConfigShares` and the org-admin fallback `orgAdminOfTeam` → `canManageOrg`) · `generic_secrets` and `bot_secret_bindings` (read on every forge call, to resolve the team's `forge_token`) · `scheduled_bots` (read by `findCategorySchedule`, cron and next fire written by the editor) · `runs` (read only, reduced to status and timestamps, capped at eight).

The file itself is not a collection: it is a real commit on the pinned branch of the pinned repository, authored as `iterion-share-editor[bot]`.

## Related

- [config-share.md](../config-share.md) — the feature's home page: the share shape, the security model and the three API tables.
- [adr/076-scoped-config-share-editor.md](../adr/076-scoped-config-share-editor.md), [adr/077-declarative-per-bot-config-share-surface.md](../adr/077-declarative-per-bot-config-share-surface.md), [adr/078-config-editor-role-sso-team-scoped-access.md](../adr/078-config-editor-role-sso-team-scoped-access.md) — the capability URL, the bot-declared surface, the signed-in capability.
- [data-model-knowledge.md](data-model-knowledge.md) — `config_shares` and `config_share_deliveries` and their edges.
- [access-control.md](access-control.md) — the principal kinds and the gate matrix these routes sit in.
- [data-model-identity.md](data-model-identity.md) (`memberships`, `teams`, `audit_events`), [data-model-credentials.md](data-model-credentials.md) (`generic_secrets`, `bot_secret_bindings`), [data-model-automation.md](data-model-automation.md) (`scheduled_bots`), [data-model-runs.md](data-model-runs.md) (`runs`).
- [bot-runs/feed-watch.md](../bot-runs/feed-watch.md) — the bot hardening that makes an edited `editorial` safe to run.

