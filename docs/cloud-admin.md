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
caller's active `org_id` + `team_id` (the store layer partitions on
that team id under a `tenant_id` field), and the server seals tenant-
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

### Why a moving tag shows up in the browser first

Pin `image.digest` rather than `image.tag` — the reference, the trade-off and
the `--set image.tag` no-op it creates are in
[cloud-deployment.md](cloud-deployment.md#pinning-the-image). What that page does
not say is how the hazard *presents*, and it presents in the studio before it
presents anywhere else.

The SPA's assets are content-hashed per build. With several builds in one
ReplicaSet — an HPA scale-up pulls whatever the moving tag means at that pod's
start time — the browser takes `index.html` from one replica and asks for chunk
hashes only that build carries. The misses land on another replica. Nothing in
the API misbehaves; the studio simply does not boot. Sampling ONE asset URL
repeatedly is what identifies it (see the troubleshooting table); reading the
manifest is not, because each replica's manifest is self-consistent.

Two consequences:

- **`kubectl rollout restart` is not a repair** under a GitOps controller with
  self-heal. It writes an annotation the controller reverts, restoring a
  template that still matches the running ReplicaSet — so no pod is recreated,
  nothing changes, and `rollout status` still reports success. Deploy by
  changing the digest in git.
- **Re-resolve the digest immediately before writing it.** The publisher can
  build the same commit more than once; a digest resolved from a tag five
  minutes earlier is already a guess.

### Serving under several hostnames

A deployment reachable under more than one name has one browser origin's worth
of cookies, `sessionStorage`, CSP and WebSocket-origin allowance *per name* —
four silently divergent copies. `config.auth.canonicalRedirect: true`
(`ITERION_CANONICAL_REDIRECT=1`) sends browser **document navigations** arriving
on any other host to `ITERION_PUBLIC_URL`, with a 302.

It never redirects anything under `/api/`, and never a non-navigation. Inbound
webhooks keep answering on whichever host the forge was configured with, as do
the CLI, the SDK and the health probes — which is the point: consolidating the
browser must not break the integrations that were handed the other name.

Off by default. `ITERION_PUBLIC_URL` is also set on deployments legitimately
reached by another name — a port-forward to localhost, an in-cluster Service
DNS, a preview host — and redirecting those moves the operator off the instance
they asked for. Turning it on without a `ITERION_PUBLIC_URL` carrying both a
scheme and a host is refused at startup, in every mode.

`ITERION_PUBLIC_URL` must be a **bare origin** while this is on. A path prefix
is meaningful elsewhere in that value — the OIDC redirect URI is built as
`${PUBLIC_URL}/api/auth/oidc/<name>/callback` — but the redirect targets the
origin alone, so a navigation would land on the right host at the wrong path.
That pairing is refused at startup rather than half-honoured.

**Check the proxy before flipping it.** The comparison needs the host the
*client* addressed, which it takes from `X-Forwarded-Host`, falling back to
`Host`. A proxy that rewrites `Host` to an internal Service name *and* omits
`X-Forwarded-Host` leaves neither, so every navigation is redirected to a
target whose host can never match — an unbounded loop taking the whole browser
surface down. ingress-nginx satisfies this by default (it preserves the client
`Host` and sets `X-Forwarded-Host`); a chart or mesh that sets
`nginx.ingress.kubernetes.io/upstream-vhost`, or an equivalent rewrite, does
not. Verify on the deployment, not on the class of proxy.

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

### Organisations & quotas

The full org admin runbook (create org, set quotas, suspend/read-only,
invite members, watch usage, mint PATs, triage the DLQ) lives in
[Iterion Cloud admin guide](cloud-admin-guide.md). The exact denial reasons +
HTTP semantics + Prometheus metrics every quota emits are in
[quotas-and-limits.md](quotas-and-limits.md).

Also wired and admin-readable:

- **Audit log** — `/api/admin/audit` (platform) +
  `/api/teams/{id}/audit` (tenant), 400-day retention. Action token
  list and filter params in
  [Iterion Cloud admin guide §1.5](cloud-admin-guide.md#15-audit-log).
- **Personal access tokens** — `/api/me/tokens` (mint / revoke);
  platform ceiling via `ITERION_PAT_MAX_TTL`. See
  [Iterion Cloud admin guide §2.6](cloud-admin-guide.md#26-personal-access-tokens-for-ci).
- **SMTP** — `ITERION_SMTP_*` env (or the chart's `config.smtp` +
  `secrets.smtp`); without it the `LogMailer` falls back to the log
  fallback and `/api/server/info` advertises `email_enabled=false`.
  See [Iterion Cloud admin guide §1.9](cloud-admin-guide.md#19-smtp-configuration).

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

1. **API keys** (BYOK): per-user, per-team or per-**org**, optionally
   flagged `is_default`. Inside that tier a launch walks per-run
   override → user-default → user-other → team-default → team-other
   (`secrets.OrderedAPIKeyCandidates` in `pkg/secrets/byok.go`) — and
   the tier is only the first step of an ordered plan
   (`pkg/server/cloudpublisher/credential_plan.go`): BYOK → generic
   secrets → OAuth-forfait (user, then team) → **org tier** (the org's
   own keys and forfaits, when its credential audience admits the team)
   → **credential pool** (a contributor's lent subscription, considered
   only when the run resolved nothing at all) → **platform tier** (the
   deployment's own DB-backed credentials, skipped when a pool grant was
   taken) → restore of a key a provider had just refused. The runner
   pod's ambient env is what is left when every tier comes up empty.
   Tier by tier:
   [cloud-llm-credentials.md](cloud-llm-credentials.md#the-one-paragraph-model);
   `iterion remote credentials preview` prints the effective order for a
   given launch, reserving nothing and verifying nothing.
2. **OAuth-forfait** (kinds `claude_code`, `codex`): stored at four
   scopes — per user (`/api/me/oauth/…`), per team
   (`/api/teams/{id}/oauth/…`), per org (`/api/orgs/{id}/oauth/…`) and
   per platform (`/api/admin/llm/oauth/…`, super-admin). Each (owner,
   kind) pair holds a **chain**, not a single record: every credential
   carries a `rank`, `0` the primary and `1`+ the fallbacks tried in
   order when the link before them cannot serve
   (`pkg/secrets/oauth.go`). The blob is the verbatim
   `credentials.json` / `auth.json` the official CLI writes locally;
   iterion never reads its plaintext except to refresh and to
   materialise it just-in-time in a per-run `tmpfs` mount on the runner.
   Chain mechanics and the per-scope CLI:
   [cloud-llm-credentials.md](cloud-llm-credentials.md#the-fallback-chain--several-forfaits-behind-one-tier).

**Subscription-OAuth billing guard.** A Claude Pro/Max OAuth subscription
*works* on API-direct backends when the token reaches them, but Anthropic bills
third-party clients against the subscription's separate **extra-usage** balance
rather than the plan's limits. The cloud-uploaded Claude credential is bridged
to `claw` and `claude_code`; it is **not yet** mapped into pi's agent dir/env
(pi can use an ambient/local subscription token). iterion warns on each direct
use (`secrets.SubscriptionOAuthNotice`) and permits it by default.

**On a shared instance you probably want to refuse it**: spending an
operator's extra-usage balance is a cost decision taken on behalf of
everyone using the deployment. Set
`ITERION_FORBID_SUBSCRIPTION_OAUTH=1` on the runner — it closes both
the per-run credential path (`secrets.GuardSubscriptionOAuth`, called
from `claw_backend.Execute` and the pi backend) and the env path (the
provider constructor in `pkg/backend/model/registry.go`). `claude_code`
and `codex` are unaffected: they spawn the vendor's own CLI, which
draws on the plan normally. Pinned by
`pkg/secrets/subscription_oauth_test.go`; rationale in
[ADR-085](adr/085-pi-as-execution-backend.md).

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

## 10. Backup & restore

The data plane's durable state lives in Mongo (runs, events,
identity, sealed secrets) + the blob bucket (artifact bodies). The
backup + restore drill — including the
"Mongo and blob snapshot must overlap in time" invariant — lives in
[docs/cloud-backup.md](cloud-backup.md). Run the restore drill
quarterly against a sacrificial namespace; an unverified backup is no
backup.

## 11. Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| Studio blank; console shows `/assets/*.js` blocked — as a disallowed MIME type (`text/html`) on replicas without this fix, as a 404 on replicas carrying it | Replicas are serving different builds — `image.tag` is a moving tag and a scale-up pulled a newer one. A mixed fleet shows both symptoms at once | Pin `image.digest` (§4). Confirm first: `kubectl -n <ns> get pods -l app.kubernetes.io/component=server -o custom-columns=NAME:.metadata.name,IMAGEID:.status.containerStatuses[0].imageID` — more than one digest across those pods means this. (`<none>` is a pod still pulling, not a second build) |
| Server boots fine, every workflow fails at `unseal run_secrets` | Server and runner have different `ITERION_SECRETS_KEY` | Make the secret bundle identical (same envFrom Secret, no per-pod override) |
| `/api/auth/login` returns 401 with no logs | DB connection healthy but `users` collection empty | Set `ITERION_BOOTSTRAP_ADMIN_EMAIL`, restart the server, capture the temp password from logs |
| OIDC redirect lands on the SPA but immediately bounces back to `/login` | `ITERION_PUBLIC_URL` doesn't match the redirect URI registered with the IdP | Update either side; the URI must equal `${PUBLIC_URL}/api/auth/oidc/<name>/callback` |
| Anthropic calls fail with `refusing to spend a subscription OAuth token outside the vendor's own CLI` | The runner has `ITERION_FORBID_SUBSCRIPTION_OAUTH=1` and resolved subscription credentials for a `claw` or `pi` node (the pi check can see an uploaded credential even though that upload is not yet a pi auth source) | Keep the guard and configure tenant BYOK / switch to `claude_code`, or deliberately remove the guard after accepting Anthropic's separate extra-usage billing |
| Anthropic reports that the subscription extra-usage balance is empty | A permitted `claw` call, or pi with ambient/local subscription auth, exhausted the separate balance | Replenish/enable extra usage, configure BYOK, or route the node through `claude_code` to use plan limits |
| Users can't see other team members' runs | Working as intended — tenant scoping. Super-admins see everything via `/admin` | n/a |
