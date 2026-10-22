# Secrets reference

**Audience.** Org admins choosing where to store a credential (their
own account, the team, a bot binding) and operators reviewing the
sealing model before opening the platform to multiple tenants.

This page is the **single map** of every secret kind iterion knows. The
engine-side protection layers (placeholders, sink redaction,
TLS-MITM egress) live in [secrets.md](secrets.md); this page is about
**which record stores which value and which run picks it up**.

Diagram: [diagrams/data-model-credentials.md](diagrams/data-model-credentials.md) — the credentials data model (secrets at rest, the sealed bundle, the meters).

## The six kinds at a glance

| Kind | What it carries | Scope | Resolved at | Source of truth |
|---|---|---|---|---|
| **BYOK API key** | LLM provider key (Anthropic, OpenAI, …) | team or user | run launch | [pkg/secrets/byok.go](../pkg/secrets/byok.go) |
| **Generic secret** | arbitrary string (forge PAT, deploy key) | team or user | run launch | [pkg/secrets/generic.go](../pkg/secrets/generic.go) |
| **Bot-secret binding** | policy that names a generic secret for a bot | team only | run launch | [pkg/secrets/bindings.go](../pkg/secrets/bindings.go) + ADR [018](adr/018-bot-secret-binding-egress-enforcement.md) |
| **File secret** (`as: file`) | secret materialised on disk in the sandbox, or in the runner pod when the run is unsandboxed | engine | run launch | [pkg/secrets/files.go](../pkg/secrets/files.go) ([engine layers](secrets.md)) |
| **OAuth-forfait** | Claude Pro / ChatGPT subscription blob | user, team, org tier or platform | run launch | [pkg/secrets/oauth.go](../pkg/secrets/oauth.go) |
| **Tokens** | bearer credentials (`iwh_` / `iap_` / `iar_`) | varies | every request | [pkg/webhooks/token.go](../pkg/webhooks/token.go) · [pkg/pat/pat.go](../pkg/pat/pat.go) · [pkg/auth/password_reset.go](../pkg/auth/password_reset.go) |

## BYOK LLM keys

`ApiKey` records carry a sealed provider key plus the resolution
metadata. Supported providers
([pkg/secrets/byok.go:Provider](../pkg/secrets/byok.go)):
`anthropic` · `openai` · `bedrock` · `vertex` · `azure` · `openrouter` ·
`xai` · `zai` · `moonshot` · `openai_compatible` (a tenant-owned gateway:
the record carries `base_url`, see [ADR-131](adr/131-tenant-gateway-credential.md);
storable and listed, but no run spends it until #2005 PR-C').

Scope semantics:

- `ScopeUserID == ""` → **team-scoped**: every team member picks it up.
- `ScopeUserID != ""` → **user-only**: the API list endpoint hides it
  from other team members even though it lives under the same tenant.

Within a `(team, user, provider)` tuple at most one record may carry
`is_default = true`; resolution prefers it. Mark a key default at
create or update; `ClearDefault` is called transparently so the rule
holds.

### Resolution precedence (`pkg/secrets/byok.go:Resolve`)

For each LLM provider the run needs, iterion picks the first match:

1. `key_overrides[provider]` — caller-pinned (the launch payload, or
   the webhook's `KeyOverrides` field).
2. `(team, userID, provider, is_default=true)` — the user's flagged
   default.
3. `(team, userID, provider)` — first match (creation order).
4. `(team, "", provider, is_default=true)` — the team's flagged default.
5. `(team, "", provider)` — first team-scoped match.
6. (no hit) — the publisher's walk goes on to its later tiers
   ([data-model-credentials.md](diagrams/data-model-credentials.md) draws all
   seven) and its fallback door (`acquireDoorGrant`); past them, the runner pod's ambient env: the run is
   queued, not refused. A publish-time refusal is opt-in —
   `ITERION_CLOUD_REQUIRE_LLM_CREDENTIAL`, or a tenant running under
   `llm_fallback: none` — and refuses the launch with `NO_LLM_CREDENTIAL`
   ([cloud-llm-credentials.md](cloud-llm-credentials.md)).

Webhook `key_overrides` is validated up front so a misconfiguration
fails at create time, not at the first inbound delivery (see
[webhooks.md](webhooks.md#key_overrides--pin-a-byok-key-per-webhook)).

REST surface: `/api/teams/{id}/api-keys` for team keys,
`/api/me/api-keys` for user keys
([pkg/server/byok_routes.go](../pkg/server/byok_routes.go)). Plaintext
is **write-only** — the listing returns metadata only (`name`,
`provider`, `last4`, `fingerprint`, `is_default`).

## Generic secrets

Same scope semantics as BYOK keys (team or user), no concept of
`is_default` or per-provider uniqueness — operators name them and the
workflow refers to them by name. Stored at `/api/teams/{id}/secrets`
(team) and `/api/me/secrets` (user)
([pkg/server/generic_secrets_routes.go](../pkg/server/generic_secrets_routes.go)).

A workflow declares the names it expects in its `secrets:` block; the
runtime injects placeholders at parse time
([engine layers in secrets.md](secrets.md)), and the publisher
materialises the real value just before the workflow runs. Plaintext is
never persisted on the run; only the sealed bundle is.

## Bot-secret bindings (the policy layer)

A **binding** is metadata, not a secret value: it ties a stored generic
secret to one bot under the name that bot's workflow declares in
`secrets:` ([ADR 018](adr/018-bot-secret-binding-egress-enforcement.md),
[pkg/secrets/bindings.go:BotSecretBinding](../pkg/secrets/bindings.go)).

Why exist: a synthetic webhook actor has **no user identity** of its
own, so it can't carry user-scoped secrets. A binding gives the
unattended run a deterministic mapping from "the bot needs a
`forge_token`" to "use the team-scoped secret `sec_…`". Personal
(`scope_user_id` set) secrets are deliberately excluded from binding
dereference — a binding is shared org automation policy
([pkg/secrets/bindings.go:bindableGenericSecretForBotBinding](../pkg/secrets/bindings.go)).

### Resolution precedence with bindings (`ResolveGenericWithBindings`)

When the runtime asks for secret `<name>`, iterion picks:

1. **per-run override** — the launch's `SecretOverrides` (a webhook's
   `secret_overrides`, for instance) pinning `<name>` to a stored secret
   id; its `SourceScope` reads `webhook-override`;
2. **user-scoped secret** of that name (if the launching user has one
   — interactive personal opt-in still wins);
3. **bot binding** mapping `<name>` to a stored team-scoped secret
   (the canonical route for unattended / webhook runs);
4. **team-scoped secret** of that name (existing fallback).

Source scope appears on `GenericResolution.SourceScope` so the audit
trail says exactly which tier won.

### `allowed_hosts` — egress enforcement

A binding's `allowed_hosts` **narrows** the workflow's declared
`secrets.<name>.hosts` (it never broadens it). The publisher carries it
on `RunBundle.GenericSecretHosts`; the runner intersects with the
workflow's hosts in `model.effectiveSecretHosts`; the egress proxy
applies the intersection as the live allowlist.

The effective allowlist
([pkg/backend/model:effectiveSecretHosts](../pkg/backend/model/secretguard.go),
over [`IntersectHosts`](../pkg/secrets/bindings.go)):

- both empty → empty (unrestricted)
- one set, one empty → the set (restriction wins)
- both set, overlapping → their intersection
- both set, disjoint → `[""]`, deny-all (an empty intersection would
  otherwise read as allow-any)

The intersection is literal — byte-identical strings, case and `:port`
included: a binding host the workflow does not list verbatim — a subdomain
of one included, and anything carrying a path — drops out of the
intersection, and a binding left with none is deny-all. A `gitlab_pat`
declared with `hosts: ["gitlab.example.com"]` therefore binds usefully
with `allowed_hosts: ["gitlab.example.com"]` and not with
`["gitlab.example.com/api"]`.

REST surface:
`/api/teams/{id}/bots/{bot_id}/bindings`
([pkg/server/bot_bindings_routes.go](../pkg/server/bot_bindings_routes.go)).

## File secrets (`as: file`)

For credentials whose natural form is a file on disk (`kubeconfig`,
cloud SDK config, a deploy certificate), declare the secret with
`as: file` in the workflow. The engine writes the plaintext into a
read-only file inside the sandbox, exposes the path via
`{{secrets.<name>.path}}` (and optionally an env var), and the agent
passes the *path* to its commands — never the bytes.

The mechanism, materialisation paths, `optional: true` semantics, and
driver behaviour (Docker bind-mount vs Kubernetes per-run Secret) live
in [secrets.md → File secrets](secrets.md#file-secrets). This page is
the cloud-side cross-reference: file secrets are sourced through the
**generic secret → binding → workflow secrets:** chain above, so any
file secret declared `optional: true` will pick up a team-scoped
binding seamlessly.

## OAuth-forfait

Per-owner, per-kind (`claude_code` / `codex`), ranked when an owner holds
several — a `credentials.json` / `auth.json` payload
([pkg/secrets/oauth.go](../pkg/secrets/oauth.go)). The publisher
materialises it just-in-time in a per-run `tmpfs` mount on the runner;
the user-facing flow is documented in
[cloud-user.md → OAuth subscriptions](cloud-user.md#4-oauth-subscriptions-claude-promax--chatgpt).

The owner is a user id or one of three reserved keys in the same
namespace: `org:<team id>` — a team-shared forfait, mutated through
`/api/teams/{id}/oauth/…` by a team admin
([pkg/server/oauth_team_routes.go](../pkg/server/oauth_team_routes.go)) —
`orgtier:<org id>` for the org tier (`/api/orgs/{id}/oauth/…`) and
`platform:` for the deployment's own (super-admin,
`/api/admin/llm/oauth/…`). Each is an ordinary record reusing the whole
store / seal / refresh machinery, and the publisher falls back through
them in that order after the run owner's personal one, the mutualised pool
among them
([pkg/server/cloudpublisher/credential_plan.go](../pkg/server/cloudpublisher/credential_plan.go)). A team mapped to a
runner pool, or holding `llm_fallback: none`, walks neither the org tier,
the pool nor the platform.

ToS guard: iterion's in-process `claw` backend refuses to consume the
Claude Code OAuth-forfait blob ([cloud-admin.md
§7](cloud-admin.md)) — the forfait is scoped to the official CLI
only. The `codex` OAuth flow has no equivalent restriction.

## Tokens — what each prefix means

| Prefix | What | Where it lives | Visibility |
|---|---|---|---|
| `iwh_…` | Inbound webhook bearer | `webhook_configs.token_hash` | shown once at create / rotate |
| `iap_…` | Personal access token | `pat.token_hash` | shown once at mint |
| `iar_…` | Password-reset link | `password_resets.token_hash` | sent by email; 60-minute TTL |
| (no prefix) | Refresh JWT | `sessions` (hashed) | the `__Host-iterion_refresh` cookie |
| (no prefix) | Access JWT (HS256) | not stored — signed at issue | the `__Host-iterion_auth` cookie / `Authorization: Bearer` |

All four `i…_` tokens use the same primitive
([pkg/auth/password.go:GenerateRandomToken](../pkg/auth/password.go) +
`HashRefreshToken`): 32 random bytes URL-safe-encoded, the prefix is
recognisability for humans. The hash on disk is salted; verification is
constant-time.

The reset and PAT TTLs are platform-level: reset is hard-pinned at
60 minutes
([pkg/auth/password_reset.go:ResetTokenTTL](../pkg/auth/password_reset.go));
PATs cap at `ITERION_PAT_MAX_TTL` (unset = no cap) and clamp every
mint to the ceiling
([pkg/server/pat_routes.go:handleCreatePAT](../pkg/server/pat_routes.go)).

## The sealing model

Everything that lives at rest as "sealed" goes through
`secrets.Sealer` — an AES-256-GCM AEAD wired from `ITERION_SECRETS_KEY`
at boot ([pkg/secrets/sealer.go](../pkg/secrets/sealer.go)).

Wire format (single-byte versioned for forward compatibility):

```
v1: 0x01 | nonce(12) | ciphertext+tag
```

The AAD ("authenticated additional data") binds each sealed blob to its
intended record so a sealed bundle cannot be silently transplanted:

| Record | AAD |
|---|---|
| `api_keys.sealed_secret` | `api_key:<id>` |
| `generic_secrets.sealed_secret` | `generic_secret:<id>` |
| `oauth_credentials.sealed_payload` | `oauth:<user_id>:<kind>` |
| `webhook_configs.hmac_secret_sealed` | `webhook_hmac_secret:<webhook_id>` |
| `run_secrets.sealed_bundle` | `run_secrets:<tenant_id>/<runner_pool>/<run_id>` (records with a `key_id`; the pool segment is empty for shared-fleet runs) — legacy id-less records keep `run_secrets:<run_id>` |

`ITERION_SECRETS_KEY` — or the `ITERION_SECRETS_KEYS` ring — is required
at boot by the cloud SERVER, which refuses to start without key material
(`openssl rand -base64 32` → exactly 32 raw bytes). It seals and opens the at-rest records; a runner pod needs none
to open a current (`dek`) bundle but still wants it for the pre-DEK cohorts
and for its mid-run re-reads (see [Run bundles: no key to
rotate](#run-bundles-no-key-to-rotate)), and where a deployment mounts it,
both pods must agree on it.

Deploy ordering: roll the RUNNERS before or atomically with the servers.
The wire schema pins it — a consumer refuses a `RunMessage` outside
`queue.MinSchemaVersion`–`queue.SchemaVersion` at the schema check, so a
server-first deploy that raises `queue.SchemaVersion` makes every new
publish bounce.

### Rotating `ITERION_SECRETS_KEY` — current state

There is **no rotation tooling yet**. The honest manual procedure
documented in [cloud-admin.md §8](cloud-admin.md):

1. Generate the new key.
2. Have all users re-paste their API keys + OAuth blobs through the UI.
3. Roll the new key into the server + runner Secret simultaneously.
4. Drop the `api_keys`, `generic_secrets`, `oauth_credentials`,
   `bot_secret_bindings`, `run_secrets`, `webhook_configs.hmac_secret_sealed`
   (or wait for users / admins to overwrite their entries).

Phase G in the public roadmap will add envelope encryption (master key
in KMS, per-tenant DEKs) so rotation becomes a single update; until
then, the step-by-step above is the operator path. **Be deliberate**:
this is a destructive change, every sealed blob becomes unreadable the
moment the new key takes over.

### Run bundles: no key to rotate

Since ADR-123 a run bundle seals under a fresh 32-byte per-run DEK
that travels in the queue message (`BundleDEK`) — on its pool's stream,
or on the shared `iterion.queue.runs` for a run with no pool. A parked
message keeps it on the dead-letter stream (seven days by default), and the
super-admin dead-letter peek returns that payload with only its vars
redacted. No platform key material is needed to open a bundle. There is nothing to
rotate: every bundle already has its own key, and the ring is out of
the run path. Runner pods need no auth secret to open a current bundle;
a deployment may keep one during the transition window (it opens bundles
published before the DEK rollout, until the 24 h TTL clears them) — and
the runner's mid-run re-reads of at-rest records still want one: with no
sealer the 5-minute file-secret refresh fails every tick and the
git-credential rotation never starts, so a long run pushes with the
token it was launched with.

### Rotating the at-rest key — the ring

The ring (`ITERION_SECRETS_KEYS` + `ITERION_SECRETS_KEY_ID`) covers
`api_keys`, `oauth_credentials` and `generic_secrets` — the records
that carry no key id and seal under the ring's current key:

1. Set `ITERION_SECRETS_KEYS="<old_id>=<b64>,<new_id>=<b64>"` and
   `ITERION_SECRETS_KEY_ID="<new_id>"` on the server (the runner does
   not need it any more). Restart. Opens fall back across the whole
   ring, so records sealed under `<old_id>` keep working while it
   stays; they re-seal under `<new_id>` only when rewritten (a
   re-paste, a refresh-worker rewrite, a publisher grant).
2. Retire `<old_id>` from the ring once every at-rest record has been
   rewritten or re-pasted. A record whose key is gone refuses with
   authentication failure.

The per-run AAD (`tenant/pool/run`) also makes a bundle
undecryptable outside its own identity context — a record ref served
across tenants yields authentication failure, not credentials.
