[← Documentation index](README.md) · [← Iterion Cloud overview](cloud-overview.md)

# Secrets reference

**Audience.** Org admins choosing where to store a credential (their
own account, the team, a bot binding) and operators reviewing the
sealing model before opening the platform to multiple tenants.

This page is the **single map** of every secret kind iterion knows. The
engine-side protection layers (placeholders, sink redaction,
TLS-MITM egress) live in [secrets.md](secrets.md); this page is about
**which record stores which value and which run picks it up**.

## The six kinds at a glance

| Kind | What it carries | Scope | Resolved at | Source of truth |
|---|---|---|---|---|
| **BYOK API key** | LLM provider key (Anthropic, OpenAI, …) | team or user | run launch | [pkg/secrets/byok.go](../pkg/secrets/byok.go) |
| **Generic secret** | arbitrary string (forge PAT, deploy key) | team or user | run launch | [pkg/secrets/generic.go](../pkg/secrets/generic.go) |
| **Bot-secret binding** | policy that names a generic secret for a bot | team only | run launch | [pkg/secrets/bindings.go](../pkg/secrets/bindings.go) + ADR [018](adr/018-bot-secret-binding-egress-enforcement.md) |
| **File secret** (`as: file`) | secret materialised on disk in the sandbox | engine | per-node exec | [pkg/secrets/files.go](../pkg/secrets/files.go) ([engine layers](secrets.md)) |
| **OAuth-forfait** | Claude Pro / ChatGPT subscription blob | user only | run launch | [pkg/secrets/oauth.go](../pkg/secrets/oauth.go) |
| **Tokens** | bearer credentials (`iwh_` / `iap_` / `iar_`) | varies | every request | [pkg/webhooks/token.go](../pkg/webhooks/token.go) · [pkg/pat/pat.go](../pkg/pat/pat.go) · [pkg/auth/password_reset.go](../pkg/auth/password_reset.go) |

## BYOK LLM keys

`ApiKey` records carry a sealed provider key plus the resolution
metadata. Supported providers
([pkg/secrets/byok.go:Provider](../pkg/secrets/byok.go)):
`anthropic` · `openai` · `bedrock` · `vertex` · `azure` · `openrouter` ·
`xai` · `zai`.

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
6. (no hit) — the operator's env-var fallback (deployment-wide), or a
   400 if neither side has a key.

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

1. **user-scoped secret** of that name (if the launching user has one
   — interactive personal opt-in still wins);
2. **bot binding** mapping `<name>` to a stored team-scoped secret
   (the canonical route for unattended / webhook runs);
3. **team-scoped secret** of that name (existing fallback).

Source scope appears on `GenericResolution.SourceScope` so the audit
trail says exactly which tier won.

### `allowed_hosts` — egress enforcement

A binding's `allowed_hosts` **narrows** the workflow's declared
`secrets.<name>.hosts` (it never broadens it). The publisher carries it
on `RunBundle.GenericSecretHosts`; the runner intersects with the
workflow's hosts in `model.effectiveSecretHosts`; the egress proxy
applies the intersection as the live allowlist.

The intersection rule
([pkg/secrets/bindings.go:IntersectHosts](../pkg/secrets/bindings.go)):

- both empty → empty (unrestricted)
- one set, one empty → the set (restriction wins)
- both set → their intersection

Example: a `gitlab_pat` secret declared with `hosts:
["gitlab.example.com"]` and bound with `allowed_hosts:
["gitlab.example.com/api"]` ends up with `["gitlab.example.com/api"]`
as its effective egress allowlist for that bot.

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

Per-user, per-kind (`claude_code` / `codex`) — the verbatim
`credentials.json` / `auth.json` the official CLI wrote on a local
machine
([pkg/secrets/oauth.go](../pkg/secrets/oauth.go)). The publisher
materialises it just-in-time in a per-run `tmpfs` mount on the runner;
the user-facing flow is documented in
[cloud-user.md → OAuth subscriptions](cloud-user.md#4-oauth-subscriptions-claude-promax--chatgpt).

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
| (no prefix) | Refresh JWT | `sessions` (hashed) | the `iterion_refresh` cookie |
| (no prefix) | Access JWT (HS256) | not stored — signed at issue | the `iterion_auth` cookie / `Authorization: Bearer` |

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
| `oauth_credentials.sealed_blob` | `oauth:<user>:<kind>` |
| `webhook_configs.hmac_secret_sealed` | `webhook_hmac_secret:<webhook_id>` |
| `run_secrets.sealed_bundle` | `run_secrets:<run_id>` |

`ITERION_SECRETS_KEY` is required at boot in cloud mode (`openssl rand
-base64 32` → exactly 32 raw bytes). Server pods AND runner pods must
agree on it, because the runner is the only thing that decrypts the
per-run bundle the publisher sealed.

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
