# Iterion cloud — operator guide

This guide covers everything you need to **run** an iterion cloud
deployment for a team or an organisation: bootstrapping the first
super-admin, configuring SSO, managing tenants, and rotating the
secrets that gate the multitenant data plane.

For the user-facing flows (login, BYOK, OAuth-forfait), see
[cloud-user.md](cloud-user.md).

## 1. Architecture in one paragraph

`iterion server` (HTTP) and `iterion runner` (workflow executor) are
two binaries built from the same image. The server persists run
metadata + events in **MongoDB**, artifact bytes in **S3**, and
publishes work onto a **NATS JetStream** subject the runner pool
drains. Auth, multitenancy, BYOK and OAuth-forfait sit entirely on
the server side: every request is gated by a JWT that carries the
caller's `tenant_id` (active team), and the server seals tenant-
scoped credentials per-run before the runner unseals + injects them
into the engine ctx.

## 2. Required secrets at boot

Cloud mode refuses to start without two values:

| Env var | Purpose | How to generate |
|---|---|---|
| `ITERION_JWT_SECRET` | HS256 signing key for access JWTs (≥32 bytes) | `openssl rand -base64 48` |
| `ITERION_SECRETS_KEY` | AES-256-GCM master key for sealing BYOK + OAuth blobs (exactly 32 bytes) | `openssl rand -base64 32` |

Both **server pods AND runner pods** must agree on `ITERION_SECRETS_KEY`
— without it the runner can't unseal the per-run bundle the publisher
wrote, and every workflow fails at "fetch run_secrets".

The `ITERION_JWT_SECRET` is server-only. Rotating it invalidates every
issued access token (users get a fresh one via the next refresh
within 30 days; refresh tokens stored in Mongo are unaffected and can
be force-revoked by clearing the `sessions` collection).

## 3. Bootstrap the first super-admin

Set `ITERION_BOOTSTRAP_ADMIN_EMAIL=ops@example.com` on a fresh
deployment. On the first boot of an empty `users` collection the
server creates the account with a one-time random password printed
at `WARN` level in the structured log:

```
{"level":"warn","msg":"server: BOOTSTRAP super-admin created — email=ops@example.com temp_password=4xT0n... (change on first login)"}
```

Capture the password from your log aggregator, sign in, change it,
and unset `ITERION_BOOTSTRAP_ADMIN_EMAIL` on the next deploy (the
guard is `users.count() == 0`, but removing the env var is cleaner).

## 4. Helm chart — `charts/iterion`

```bash
helm install iterion ./charts/iterion \
  -f ./charts/iterion/values-prod.yaml \
  --set secrets.auth.create=false \
  --set secrets.auth.existingSecret=iterion-auth
```

Production rolls the auth bundle out-of-band (sealed-secrets,
external-secrets, manual `kubectl apply` of a Secret with the same
env-var names). The chart's `secrets.auth.create=true` path bakes
values into the release record — convenient for kind/dev, never
appropriate for prod.

The auth Secret expected by `secrets.auth.existingSecret` must hold:

```yaml
stringData:
  ITERION_JWT_SECRET: "..."
  ITERION_SECRETS_KEY: "..."
  ITERION_BOOTSTRAP_ADMIN_EMAIL: "ops@example.com"  # optional
  # Per-provider secrets — only needed when the matching OIDC is
  # enabled in the chart's config.auth.oidc block:
  ITERION_OIDC_GOOGLE_CLIENT_SECRET: "..."
  ITERION_OIDC_GITHUB_CLIENT_SECRET: "..."
  ITERION_OIDC_GENERIC_CLIENT_SECRET: "..."
```

Public OIDC info (issuer URL, client IDs, scopes, public URL) lives
in the **ConfigMap** through `config.auth` in `values.yaml` — no
need to land it in the Secret.

## 5. SSO providers

| Provider | Required values | Notes |
|---|---|---|
| Email + password | nothing — built-in | Argon2id, no MFA in V1 |
| Google | `clientId` + `clientSecret`, redirect URI `${PUBLIC_URL}/api/auth/oidc/google/callback` | Standard Google Cloud OAuth client, type "Web application" |
| GitHub | `clientId` + `clientSecret`, callback URL `${PUBLIC_URL}/api/auth/oidc/github/callback` | OAuth App (NOT GitHub App), scopes `read:user user:email` |
| Generic OIDC | `issuerUrl` + `clientId` + `clientSecret` + `displayName`, `scopes` defaulting to `openid email profile` | Discovery-based; works with Keycloak, Auth0, Azure AD, Okta, … |

First-time login behaviour depends on `ITERION_SIGNUP_MODE`:

- `invite_only` (default): the user must hold an invitation token
  matching their email; first login without one returns 403.
- `open`: the server auto-provisions a personal team and lets them
  in.

Recommended for most deployments: `invite_only` + a super-admin
inviting initial team owners.

## 6. Tenant management

Teams = tenants. Every Run, Event, Interaction and run-scoped
credential bundle is partitioned by `tenant_id` at the Mongo
level (compound indexes on `(tenant_id, status, created_at)` and
`(tenant_id, owner_id, created_at)` on `runs`; `(tenant_id, run_id, seq)`
on `events`). Tenant scoping is enforced via context: the server
auth middleware stamps `tenant_id` into the request ctx after JWT
decode, and `pkg/store/mongo` augments every query with that filter
unless the ctx is privileged (super-admin, runner bootstrap, the
`migrate` tool).

Roles inside a team:

| Role | Can read runs | Can launch | Can manage members | Can manage team API keys |
|---|---|---|---|---|
| `viewer` | yes | no | no | no |
| `member` | yes | yes | no | no |
| `admin` | yes | yes | yes | yes |
| `owner` | yes | yes | yes | yes |

Plus the global `is_super_admin` flag, which bypasses every team
check and surfaces the `/admin` admin pages.

## 7. BYOK + OAuth-forfait — operator perspective

Users register their own credentials through the admin UI; iterion
seals them at rest with `ITERION_SECRETS_KEY`. There are two
storage tracks:

1. **API keys** (BYOK): per-team or per-user, optionally flagged
   `is_default`. Resolution order at run launch: per-run override →
   user-default → user-other → team-default → team-other → env.
2. **OAuth-forfait**: per-user only, one record per kind (Claude Code,
   Codex). The blob is the verbatim `credentials.json` / `auth.json`
   the official CLI writes locally; iterion never reads its plaintext
   except to refresh and to materialise it just-in-time in a per-run
   `tmpfs` mount on the runner.

**CGU (terms-of-service) guard.** Anthropic scopes the Claude Pro/
Max OAuth-forfait to the official Claude Code CLI; iterion's
in-process LLM client (`claw`) is therefore forbidden from consuming
it. The code enforces this via `secrets.GuardThirdPartyOAuth(...)`,
called from `claw_backend.Execute` for every Anthropic model.
A unit test (`pkg/secrets/claw_guard_test.go`) pins the rule.

If you want to disable OAuth-forfait entirely (e.g. you're operating
in a jurisdiction where the legal team prefers the strict BYOK path),
leave `oauthForfait.{anthropic,codex}.enabled=false` in your values
file and don't set `ITERION_OAUTH_FORFAIT_*_CLIENT_ID`. The
`/api/me/oauth/*` endpoints stay reachable but token refresh
fails with `not configured` — users will re-paste blobs on expiry.

## 8. Rotating the master key

Rotating `ITERION_SECRETS_KEY` invalidates every sealed BYOK + OAuth
record. The clean path:

1. Generate the new key.
2. Have all users re-paste their API keys + OAuth blobs.
3. Roll the new key into the server + runner Secret simultaneously.
4. Drop the `api_keys`, `oauth_credentials` and `run_secrets`
   collections (or wait for users to overwrite their entries).

Phase G in the public roadmap will add envelope encryption (master
key in KMS, per-tenant DEKs) so rotation is a single MongoDB
update; until then, the step-by-step above is the operator path.

## 9. Audit log

OAuth-forfait usage is logged at `INFO` on the publisher side:

```
"cloudpublisher: oauth-forfait used run=<id> user=<id> kind=claude_code"
```

Plumb it into your log aggregator (Loki, ELK, Datadog) and add a
dashboard panel — useful both for cost attribution and as your
defence-in-depth for the CGU guard discussed in §7.

## 10. Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| Server boots fine, every workflow fails at `unseal run_secrets` | Server and runner have different `ITERION_SECRETS_KEY` | Make the secret bundle identical (same envFrom Secret, no per-pod override) |
| `/api/auth/login` returns 401 with no logs | DB connection healthy but `users` collection empty | Set `ITERION_BOOTSTRAP_ADMIN_EMAIL`, restart the server, capture the temp password from logs |
| OIDC redirect lands on the SPA but immediately bounces back to `/login` | `ITERION_PUBLIC_URL` doesn't match the redirect URI registered with the IdP | Update either side; the URI must equal `${PUBLIC_URL}/api/auth/oidc/<name>/callback` |
| Anthropic calls fail with `refusing to use Claude Code OAuth-forfait via third-party SDK` | A workflow targets `claw` for an Anthropic model, the user has only an OAuth-forfait connection (no API key) | Either (a) configure a tenant-scoped `ANTHROPIC_API_KEY` BYOK, or (b) switch the workflow to `backend: claude_code` so the official CLI handles the call |
| Users can't see other team members' runs | Working as intended — tenant scoping. Super-admins see everything via `/admin` | n/a |
