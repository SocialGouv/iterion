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

A server in cloud mode refuses to start without two values:

| Env var | Purpose | How to generate |
|---|---|---|
| `ITERION_JWT_SECRET` | HS256 signing key for access JWTs (≥32 bytes) | `openssl rand -base64 48` |
| `ITERION_SECRETS_KEY` | AES-256-GCM master key for sealing BYOK + OAuth blobs (exactly 32 bytes) | `openssl rand -base64 32` |

The server seals BYOK and OAuth blobs with `ITERION_SECRETS_KEY`, and
seals each per-run bundle under a fresh per-run key that travels in the
run message instead. Runner pods load their config with the sealer
optional ([cmd/iterion/runner.go](../cmd/iterion/runner.go)) and need no
key to open a current bundle, but their mid-run re-reads of at-rest
records want the server's key: with none, the 5-minute file-secret
refresh fails every tick and the git-credential and OAuth refreshers
never start.

The `ITERION_JWT_SECRET` is server-only. Rotating it invalidates every
issued access token (users get a fresh one via the next refresh
within 30 days; refresh tokens stored in Mongo are unaffected and can
be force-revoked by clearing the `sessions` collection).

`ITERION_REFRESH_REUSE_GRACE` (a Go duration, YAML
`auth.refresh_reuse_grace`, default `30s`) is the refresh-token reuse
grace window. A refresh rotates the token it is presented and hands out
its successor; the rotated token presented again within the window
receives that same successor instead of reading as reuse — what two
browser tabs, a desktop retrying an answer it lost, or two concurrent
refreshes of one token present. Outside the window, a rotated token
presented again is reuse: its lineage — every session rotated from the
same login, the legitimate holder's and a thief's alike — is revoked, and
the account's other logins keep theirs. The window is measured on the
database's clock (`$$NOW` stamps the rotation, and the presentation is
compared with the same clock, read from the primary whatever read
preference `ITERION_MONGO_URI` names), so the replicas' clocks do not move
it. `0` disables the window (every retired token presented again is
reuse). Keep it at the default or above: it covers the shipped clients'
retries of an answer they lost — the studio presents the token again 2, 7
and 17 s after its first attempt failed, the desktop at once, within its
30-second retry deadline — and a window shorter than those turns them into
reuse. A later presentation can still fall past it: when the desktop's own
retry also loses its answer after the server handed the successor out, its
next presentation comes from its background loop 15 s or more later, can
come more than 30 s after the rotation, and then reads as reuse, which
signs that device out. A value at or above `ITERION_REFRESH_TTL` is
refused at boot, and one above 5 minutes is logged as a warning. The price of the window: a stolen refresh token
replayed within it receives the same successor the legitimate holder
received — both then hold the same lineage, and the first rotated token
either of them presents outside its window revokes that lineage.

A successor counts as handed out once the refresh that writes it has
checked it and claimed it (`delivered_at`), the last step before it
answers; no refresh deletes a session it wrote. A rotation whose successor
was handed to nobody — never written, or written and never claimed,
whatever step failed after the rotation — leaves the retired token its
lineage's only credential: the token's next presentation writes or claims
that successor, if it comes within five minutes of the rotation (the
window instead, when that is longer) and before the token expires — an
outage that strikes after a rotation costs no session when the client
presents its token again within those five minutes, and signs that one
device out when it comes back later. A refresh is bounded at 20 seconds as
a whole and each of its session-store steps at 5 seconds: a store slower
than that answers the refresh with a `5xx`, without rotating the token
when the failure precedes the rotation, and leaving the rotation to the
token's next presentation within those five minutes when it follows it; a
claim whose answer is lost is a successor handed out, redelivered within
the window only. A refusal that revokes waits for its revocation's record,
at most 5 seconds — reuse detection's, or the retirement of the presented
session of an account that is disabled or pending a password change; the
pass over the other sessions runs after the answer.

A sign-out ends the lineage of the token it presents and never re-opens
its window; one whose session read or revocation record fails answers
`500` and leaves the cookies, for the client to present the token again. A
sign-out may name the session it means (`?lineage=`,
[REST API](cloud-rest-api.md)): a token of another session then signs
nothing out.

Every revocation — a sign-out, reuse detection, "sign out everywhere", a
password change, a password reset (by email or by an administrator), the
rotation an administrator forces, an administrator's disable — first
records itself in `session_revocations`: a revoked lineage, or the
account's session generation raised. That record is the revocation's
commit point, the one write it waits for and answers from: from then on
every session it covers is refused at its next refresh, and every access
token minted for one of them at its next derivation (below). A pass then
marks the covered sessions that are still live as revoked; it runs after
the answer, retried, and its cost follows the live sessions, not the
account's refresh history — a pass that fails, or that a restart cuts,
changes no answer, and the next revocation of the same lineage or account
runs it again. The records are kept for good (one small row per
signed-out lineage and per account signed out everywhere, never reaped): a
record is what covers a session its pass did not reach, for as long as
that session lives — and every successor is checked against the records
before it is delivered and again once it is claimed, so a refresh racing
the revocation cannot outlive it; a restore of `sessions` brings
`session_revocations` with it.

A password change, a password reset (by email or by an administrator) and
the rotation an administrator forces run one at a time per account (a
second waits up to 20 seconds for the first, then answers `409`
`credential_change_busy`), end every session of the account before the new
credential lands, and again once it landed: a sign-in reads the account's
session generation before it records itself under the password it
verified, and a password replaced since refuses it, so a sign-in with the
old password that raced the change is covered too. A failure before the
new password is committed answers `500` and leaves the password — and an
emailed reset's link — as it was; the sessions may already have ended,
since a record can land even when its answer is lost. Once the new
password is committed, a failure answers `409` `password_changed_sign_in`:
the password is changed, the link spent, the account's earlier sessions
ended, and the user signs in with the new password — and so does a change
an administrator's disable lands under: the change opens its session only
for an account still active once its last revocation is recorded, so a
re-enable later revives no session opened while the account was disabled.
A commit whose answer is lost (a request cut, a connection dropped) may
have landed: the revocation that follows a commit runs anyway, the request
answers `500`, and an emailed reset's link stays spent. An account an administrator sets pending a
password change refreshes nothing and mints nothing; a password sign-in is
sent to the forced change, and an SSO sign-in is refused until the
password is changed — by the emailed reset for an account that signs in
through SSO only. An administrator's disable lands the status first, then
ends every session, revokes every personal access token of the account and
removes every SSO identity a signed-in session connected from the account
settings; a revocation that fails there answers `500` with the account
disabled, and the console's retry revokes. "Sign out everywhere" does the
same two things too — revokes every personal access token of the account
and removes every SSO identity a signed-in session connected — the
compromise response, and the one way to end a credential minted from a
session before its revocation; a step that fails after the record answers
`503` `signout_cleanup_failed`, and the user signs in again and repeats the
request — every step is idempotent
([cloud-user §7](cloud-user.md)). The SSO identity the account was created
with, or first signed in with through SSO, stays; and the removal never
leaves an account without a sign-in method it had: an account with no
password whose every remaining identity was session-written keeps its
oldest one, so the cleanup cannot lock
its owner out of an account that signs in through SSO only (an
administrator sees the kept identity in the account's file, and the
account owner disconnects it once a password is set). Disconnecting the LAST sign-in
method of an account with no password is refused outright (`409`
`last_sign_in_method`): another provider or a password comes first. A
password change and a reset keep the
tokens and the identities, so a CI token survives a password change; a
reuse detection removes the SSO identities the reused lineage connected;
a sign-out keeps them.

An access token names the session it was minted for, and stays a
stateless credential: it authenticates every request until its own expiry
(the access TTL, 15 minutes by default), whatever happened to its session
since. Only the routes that mint a credential, grant or exercise platform
authority, or change or remove a credential first check the credential
presented to them (`auth.Service.CheckDerivation`):

- the team and org switches, and the creation of a personal access token
  (asked again once the token exists);
- the WebSocket ticket — at its mint and again at its redemption;
- the SSO link from the account settings — at its start and in its
  callback, before and again after the link is written;
- the removal of an SSO identity, the revocation of a personal access
  token, the in-app password change and "sign out everywhere";
- how an org's accounts sign in: its SSO providers and its verified email
  domains (create, update, delete, verify);
- every platform-admin route (`/api/admin/…`, the plugin routes — enable,
  disable, install, uninstall, and a marketplace plugin's install and
  uninstall, and the marketplace moderation of a public or instance-wide
  entry), which reads the super-admin flag from the account as it stands,
  not from the token: an administrator's demotion takes effect there at
  once;
- the platform acts outside the admin prefix: placing an existing account
  in an organisation (`PUT /api/orgs/{id}/members/{user_id}`), writing or
  deleting the instance-wide (`global`) memory space, and revoking another
  account's personal access token;
- the desktop SSO exchange, which hands out no session revoked since the
  IdP callback;
- the mint of a team-scoped token that outlives the credential it is
  minted from and is bound to nothing of it: a webhook token (the create
  and the rotate), a config share-link token (the create and the rotate),
  and a team invitation token (the team-scoped and the org-scoped mint).
  None of them authenticates as the account, but each opens a door that
  stays open after the minter's session is gone, so the mint is gated.

The super-admin claim itself is re-derived where every request's identity
is established, before any route runs: a token that claims the flag keeps
it only while the credential stands and the account still is a
super-admin, so a demotion — or a revocation — takes the platform
authority away at once, everywhere: the admin routes, and the
super-admin's reach over every team's and org's routes
(`auth_authz`'s escape hatches), and every route above that reads the
flag. A read of the account that fails answers an explicit `5xx` — the
request is never judged on an unchecked claim. Everything else the token
of a revoked or demoted super-admin does in its remaining minutes is
ordinary access and persists, as below.

There, it refuses the access token of a session signed out or
reuse-revoked, of an account signed out everywhere, whose password
changed, disabled or pending a password change, and an access token that
names no session asking for an SSO link; it refuses a personal access token
revoked, expired or gone, and any personal access token asking for a
credential of its own account — a switch, another token, an SSO link, a
temporary password — or for a team token that would outlive it — a webhook,
share-link or invitation token (`403`: a token is pinned to its team and
its own expiry); and what a route mints for the same use never outlives the
presented credential — a switched access token expires when the presented
one does, and a WebSocket ticket opens no WebSocket past it.
Everything else the access token of a revoked session does in its
remaining minutes is done and persists: it reads what its account may
read, launches, steers and cancels runs, edits bots and configurations, and
creates team and org state — a membership or a role, a team secret, an API
key, a forge or OAuth connection, a webhook's non-token settings — that
outlives it, listed and revocable in the team's, the org's or the
account's settings. What it no longer mints is the tokens of that state:
the webhook and share-link tokens and the invitation are gated above, so
the door that outlives the session is opened by a standing session only. A WebSocket opened before the
revocation is asked again: every envelope that changes a run (cancel,
pause, answer, queue a message or cancel a queued one, bump a loop, raise a
budget) checks the standing of the credential the socket was opened with —
its account, its session, a personal access token's row (whose expiry and
revocation both close the socket at the next tick), the access token's own
expiry left out — AND re-derives the scope the upgrade resolved once: the run's team,
the caller's standing there, the super-admin reach — read from the account
as it stands now. A refused credential answers an error and closes the
socket (close code `1008`, a reason starting `credential_refused`); a
lost scope — a
membership removed, a demotion — answers `forbidden` on that envelope and
leaves the socket's reads alone; a read that does not complete refuses
that envelope (`standing_unavailable`, `scope_unavailable`) and keeps the
socket. A run socket that only reads stops at
its absolute lifetime — 2 hours, `ITERION_RUN_WS_MAX_LIFETIME` — and its
client opens another, presenting a credential again (close code `1000`, a
reason starting `lifetime_reached`). The post-mortem shell and the Browser-Live CDP
bridge ask the same question every 30 seconds while they are open
(`ITERION_RUN_SHELL_WATCH_PERIOD`, `ITERION_RUN_BROWSER_STANDING_PERIOD`):
the standing of the credential AND the scope the upgrade resolved, read
from the account as it stands now. A refused credential closes either
surface (close code `1008`, a reason starting `credential_refused`); a
scope the
caller no longer holds — a membership removed, a demotion, a run whose
team is gone — closes it too (close code `1008`, a reason starting
`scope_lost`): every such reason is its token followed by the cause,
never the bare token, and neither surface has an envelope to refuse, so a
lost scope closes it, the shell saying so on an error frame before the
close. A standing read that does not complete is no verdict: it keeps the
surface open. The bridge counts its incomplete reads and closes after
three consecutive ones (close code `1008`, a reason starting
`standing_unavailable`)
instead of driving the run's Chromium on a credential nobody could
re-check; the shell counts nothing and has no envelope to refuse — it
stays open until its idle or lifetime cap ends it. The duration knobs honour a stated
override, and one they cannot parse or stand behind a non-positive value
is named in the boot log — never silently replaced
([environment variables](environment-variables.md)). A session written before
lineages were recorded is its own root, and a retired one presented again
names no family: its reuse revokes the whole account, as reuse detection
did before.

`sessions` stores no refresh token: the successor is re-derived from the
presented token under a key derived from `ITERION_JWT_SECRET`, so rotating
that secret also ends the window for the tokens rotated just before. The
desktop SSO ticket store (`desktop_sso_tickets`) rests no credential either
([#2635](https://github.com/SocialGouv/iterion/issues/2635)): the IdP
callback DEFERS a desktop login — the ticket stores the account, the lineage
the session will be born under and the landing team, keyed by the ticket's
SHA-256, never the ticket — and the session, its refresh token included, is
minted when the desktop redeems the ticket. No refresh token exists while a
ticket waits, so none rests in the collection, in the oplog, or in a backup
([cloud-backup.md](cloud-backup.md)), and an unredeemed ticket leaves no
live session behind — there is none to revoke at its expiry. A REFUSED
desktop sign-in (an account pending a password change, an IdP error) never
leaves the desktop waiting out its timeout either: the callback redirects
the refusal's `sso_error` code to the desktop's own loopback listener
(validated at the flow's start, `safeDesktopRedirect`), so the app reports
the reason at once. A document
written before this shape (the ticket in plaintext as `_id`, a whole login
with its refresh token as `result`) answers no redemption — its `_id` is no
hash a redemption computes — and the TTL index reaps it.

Upgrading from a release without session lineages (before #1668) is a
rollout constraint: pods of both releases serve the same `sessions` while
a rolling update overlaps them (the chart's default, `server.strategy`
RollingUpdate with `maxSurge: 100%`). For the sessions an old pod touches
during the overlap, guarantees above do not hold:

- a refresh served by an old pod writes a successor outside any lineage,
  so a reuse a new pod detects later revokes the lineage without that
  successor, which stays live — it and every successor rotated from it,
  each refresh extending it by `ITERION_REFRESH_TTL` — for as long as it
  keeps being refreshed, until its account is signed out everywhere;
- a sign-out served by an old pod moves the retired token's `revoked_at`
  and keeps its rotation record, so a new pod still completes that
  token's successor afterwards;
- "sign out everywhere", a password change or a reset served by an old
  pod revokes the sessions it finds and records nothing, so it reaches
  neither a successor a new pod writes afterwards nor a refresh of a new
  pod racing it;
- after a "sign out everywhere" served by a new pod, a session an old pod
  writes is born under generation zero, refused at its first refresh on a
  new pod;
- a credential an old pod mints skips the derivation checks above — a
  personal access token presented there still switches team and mints
  another token, a revoked session's access token still mints one — an old
  pod's platform-admin and credential-removal routes check nothing but the
  token, and a "sign out everywhere" an old pod serves revokes no personal
  access token and removes no SSO identity;
- an access token an old pod mints names no session: a new pod checks it
  for its account alone, and refuses it an SSO link (`403`);
- an SSO identity an old pod links records no session, and neither does one
  linked before this release: "sign out everywhere" and a disable keep it,
  like the identity an account was created with. Review the identities
  connected to the accounts used during the overlap (the account's file in
  the console lists them): only the ACCOUNT OWNER can remove one — Account
  → Connected SSO, with a live session — except an account's last sign-in
  method, which the server refuses to remove until another provider or a
  password is in place; there is no admin route that removes another
  account's identity. For an identity nobody can account for, disable the
  account and reset its password: the owner then signs in with the
  temporary password and disconnects the identity, and an account with a
  password loses every session-written identity at the next "sign out
  everywhere".

A rollback to a release before #1668 runs the old code over the new rows:
they keep working as plain sessions, and every guarantee of this section
that rests on the records ends with it. Upgrade with
`server.strategy: {type: Recreate, rollingUpdate: null}` — Helm merges the
chart's default `rollingUpdate` block into a `type: Recreate` given alone,
and Kubernetes refuses a Recreate strategy that carries one — so no old
pod serves once a new one does, at the cost of the rollout's downtime; or
accept those effects for the overlap and sign the accounts used during it
out everywhere afterwards: those with a `sessions` row the overlap wrote
without a `lineage`
(`db.sessions.distinct("user_id", {lineage: {$exists: false}, issued_at: {$gte: <overlap start>}})`),
and those an old pod revoked during it — on a small deployment, every
account.

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

- `invite_only` (default): a first SSO login by an unknown user is
  refused — the OIDC paths read no invitation token — and the callback
  redirects to `/login?sso_error=login_failed`; a GitHub login whose
  teams match a configured allow-list is admitted whatever the mode.
  Whatever the mode, an SSO login of an account an administrator set
  pending a password change is refused with
  `/login?sso_error=password_change_required`, and the login page names
  the exits.
  Invitations gate `POST /api/auth/register`, where the token must
  match the email.
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

1. **API keys** (BYOK): per-team or per-user, optionally flagged
   `is_default`. Resolution order at run launch: per-run override →
   user-default → user-other → team-default → team-other → env.
2. **OAuth-forfait**: per-user only, one record per kind (Claude Code,
   Codex). The blob is the verbatim `credentials.json` / `auth.json`
   the official CLI writes locally; iterion never reads its plaintext
   except to refresh and to materialise it just-in-time in a per-run
   `tmpfs` mount on the runner.

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
| Server boots fine, every workflow fails at `unseal run_secrets` | The sealed record does not open under the per-run key the run message carries — the seal binds the tenant, the runner pool and the run id | Relaunch the run: each bundle is sealed for one run and expires after 24h |
| `/api/auth/login` returns 401 with no logs | DB connection healthy but `users` collection empty | Set `ITERION_BOOTSTRAP_ADMIN_EMAIL`, restart the server, capture the temp password from logs |
| OIDC redirect lands on the SPA but immediately bounces back to `/login` | `ITERION_PUBLIC_URL` doesn't match the redirect URI registered with the IdP | Update either side; the URI must equal `${PUBLIC_URL}/api/auth/oidc/<name>/callback` |
| Anthropic calls fail with `refusing to spend a subscription OAuth token outside the vendor's own CLI` | The runner has `ITERION_FORBID_SUBSCRIPTION_OAUTH=1` and resolved subscription credentials for a `claw` or `pi` node (the pi check can see an uploaded credential even though that upload is not yet a pi auth source) | Keep the guard and configure tenant BYOK / switch to `claude_code`, or deliberately remove the guard after accepting Anthropic's separate extra-usage billing |
| Anthropic reports that the subscription extra-usage balance is empty | A permitted `claw` call, or pi with ambient/local subscription auth, exhausted the separate balance | Replenish/enable extra usage, configure BYOK, or route the node through `claude_code` to use plan limits |
| Users can't see other team members' runs | Working as intended — tenant scoping. Super-admins see everything via `/admin` | n/a |
