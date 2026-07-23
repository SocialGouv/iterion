[← Documentation index](README.md) · [← Iterion Cloud overview](cloud-overview.md)

# Iterion Cloud admin runbook

**Audience.** The platform operator who runs `iterion server` for
multiple teams **and** the org admin who manages their team inside it.
Two halves: §1 covers the platform-wide knobs only super-admins reach;
§2 covers the org self-serve flows on `/teams/<id>`. Each step pairs
the UI path with the equivalent curl so this page works for both the
studio user and the CI script.

For deployment + chart values, start with
[cloud-deployment.md](cloud-deployment.md). For the bigger conceptual
picture, see the [Iterion Cloud overview](cloud-overview.md). For the
precise REST shapes, see [cloud-rest-api.md](cloud-rest-api.md).

---

## Part 1 — Platform operator (super-admin)

### 1.1 Bootstrap the super-admin

On a fresh cluster, set `ITERION_BOOTSTRAP_ADMIN_EMAIL` and roll the
chart. The server creates the account on **first boot** if the `users`
collection is empty and prints a one-time password at WARN level
([cmd/iterion/server.go](../cmd/iterion/server.go)):

```
{"level":"warn","msg":"server: BOOTSTRAP super-admin created — email=ops@example.com temp_password=4xT0n… (rotate via POST /api/auth/password/change)"}
```

**Recovery case.** If the operator missed the log (pod restarted, log
aggregator misconfigured, …) and the bootstrap user is still in
`pending_password_change`, restart the server pod — the bootstrap
code path re-issues a fresh temp password for that one specific state.
An already-active account is **never** force-reset this way.

After login, post to `/api/auth/password/change` to rotate, then unset
`ITERION_BOOTSTRAP_ADMIN_EMAIL` on the next deploy (the guard is
`users.count()==0` so leaving it set is safe, but removing it is
cleaner).

### 1.2 Create an org and a first owner

```bash
# Create the org. owner_email defaults to the calling super-admin.
curl -X POST https://iterion.example.com/api/admin/orgs \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"Acme Corp","slug":"acme","owner_email":"alice@acme.example"}'
```

UI path: super-admin chip → Admin → Organisations → New.

The owner_email must already exist as a user. To pre-provision them,
either let them register through `/api/auth/register` (if
`config.auth.signupMode: open`) or send them an invitation token
through the org they end up owning.

### 1.3 Set quotas + caps

```bash
curl -X PATCH https://iterion.example.com/api/admin/orgs/$ORG_ID \
  -H "Authorization: Bearer $TOKEN" \
  -d '{
    "monthly_run_quota":     1000,
    "monthly_cost_cap_usd":   80.0,
    "max_concurrent_runs":      5,
    "launch_rate_per_min":     30,
    "memory_quota_bytes":     1073741824
  }'
```

UI path: Admin → Organisations → Acme → Limits → Save.

Every field is optional; `0` means "inherit the platform default"
(which the operator sets via `ITERION_ORG_DEFAULT_*` env vars — see
[cloud-deployment.md](cloud-deployment.md)). Negative values are 400s.
The handler also propagates a `memory_quota_bytes` change into the
enforced memory counter via `SetTenantQuota`
([pkg/server/admin_orgs_routes.go](../pkg/server/admin_orgs_routes.go)) —
without that step the change wouldn't take effect, only the displayed
ceiling would.

See [quotas-and-limits.md](quotas-and-limits.md) for what each limit
actually does at run launch and how to debug a denial.

### 1.4 Suspend or set an org to read-only

```bash
curl -X POST https://iterion.example.com/api/admin/orgs/$ORG_ID/status \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"status":"suspended","reason":"non-payment 2026-06"}'
```

`status` is one of `active` / `suspended` / `read_only`
([pkg/identity/types.go:TeamStatus](../pkg/identity/types.go)).
Suspended and read-only orgs deny every run launch with reason
`org_suspended` and HTTP 403; reads (the studio, the API, the run
console) still work so the org's members can see what they previously
ran. The reason is recorded on the audit row.

UI path: Admin → Organisations → Acme → toggle status.

### 1.5 Audit log

Every control-plane mutation is recorded
([pkg/audit/audit.go](../pkg/audit/audit.go)):

```bash
# All platform-level actions (super-admin and org status changes).
curl "https://iterion.example.com/api/admin/audit?limit=100"

# Filter by action token or actor.
curl "https://iterion.example.com/api/admin/audit?action=org.status_changed"
curl "https://iterion.example.com/api/admin/audit?actor=$USER_ID&from=2026-06-01T00:00:00Z"
```

The stable action tokens (grep `auditTenant`/`auditPlatform` call sites
for the full list):

> `org.created` · `org.updated` · `org.status_changed` · `user.password_changed`
> · `user.password_reset` · `user.sessions_revoked` · `member.removed`
> · `member.role_changed` · `invitation.created` · `invitation.accepted`
> · `invitation.deleted` · `byok.created` · `byok.updated` · `byok.deleted`
> · `secret.created` · `secret.updated` · `secret.deleted`
> · `binding.created` · `binding.updated` · `binding.deleted`
> · `webhook.created` · `webhook.updated` · `webhook.rotated` · `webhook.deleted`
> · `pat.created` · `pat.revoked`
> · `dlq.replayed` · `dlq.discarded`

Two scopes: tenant rows (visible to the org's admins at
`GET /api/teams/{id}/audit`) and platform rows (super-admin only at
`/api/admin/audit`). Each carries the actor's IP + user-agent + a small
`meta` blob (never secret material). Mongo TTL: **400 days**
([pkg/audit/audit.go:RetentionDays](../pkg/audit/audit.go)).

### 1.6 User admin — force a password rotation

```bash
# Find the user.
curl "https://iterion.example.com/api/admin/users?email=alice@acme.example"

# Force a password change on next login.
curl -X PATCH https://iterion.example.com/api/admin/users/$USER_ID \
  -d '{"status":"pending_password_change"}'
```

Setting the status to `pending_password_change` blocks login until the
user rotates via `POST /api/auth/password/change` (the standard
auth-rotation endpoint also covers the bootstrap and post-reset flows).
Disabling the user (`status:"disabled"`) refuses every login attempt
while keeping their data.

### 1.7 DLQ triage

When a run exhausts its NATS redelivery budget (default 8) the runner
**parks** a copy on the DLQ stream and flips the run to
`failed_resumable` ([pkg/runner/loop.go](../pkg/runner/loop.go), look
for `parking on DLQ`). Triage:

```bash
# List parked messages. cursor advances; next_cursor=0 means exhausted.
curl "https://iterion.example.com/api/admin/dlq?limit=20"

# Peek (full RunMessage payload).
curl "https://iterion.example.com/api/admin/dlq/$SEQ"

# Replay (re-publish onto iterion.queue.runs, then delete from DLQ).
curl -X POST "https://iterion.example.com/api/admin/dlq/$SEQ/replay"

# Discard permanently.
curl -X DELETE "https://iterion.example.com/api/admin/dlq/$SEQ"
```

Behind the scenes
([pkg/queue/nats/dlq.go](../pkg/queue/nats/dlq.go)): each DLQ message
carries `Iterion-DLQ-Reason`, `Iterion-Run-Id`, `Iterion-Tenant-Id`,
`Iterion-Num-Delivered` headers so the list view explains *why* it
parked without re-decoding the body. Replay salts the `Nats-Msg-Id`
with the DLQ sequence so JetStream's dedup window can't silently swallow
the second attempt; the new in-flight run is admitted through the
launch gate like any other publish.

### 1.8 Orphan sweeper

A runner pod that dies between claiming a run and writing its terminal
status strands the run row in `running` (or `queued`) forever — UI
shows an eternal spinner, `iterion resume` rejects ("not a resumable
status"). The orphan sweeper closes that gap
([pkg/server/queue_sweeper.go](../pkg/server/queue_sweeper.go)):

- Scans every 60s for `queued` past the redelivery window + margin
  (~90 min with the defaults) or `running > 10 min` AND no current
  NATS-KV lease.
- CAS-flips matched rows to `failed_resumable` so `iterion resume` (or
  the studio Retry button) lights up.
- Bumps `iterion_runs_orphan_recovered_total`. Set the
  `IterionOrphanRunsRecovered` alert (default in the starter pack) to
  catch pod churn early.

The PrometheusRule pack ships the alert at
`increase(iterion_runs_orphan_recovered_total[30m]) > 0`.

### 1.9 SMTP configuration

Transactional email (invitations + self-service password reset) is opt
in. Without SMTP, iterion falls back to a `LogMailer` that prints
would-be messages at WARN level — fine for dev, useless for production
([pkg/mail/log.go](../pkg/mail/log.go)). Configure via env on the
server pod:

| Env var | Required | Effect |
|---|---|---|
| `ITERION_SMTP_HOST` | yes (enables real mailer) | Relay hostname |
| `ITERION_SMTP_PORT` | usually 587 | TCP port |
| `ITERION_SMTP_USERNAME` | yes (typically) | SMTP AUTH user |
| `ITERION_SMTP_PASSWORD` | yes (typically) | SMTP AUTH password |
| `ITERION_SMTP_FROM` | yes | Envelope + From header, e.g. `iterion <no-reply@example.org>` |
| `ITERION_SMTP_STARTTLS` | default `true` | Upgrade before AUTH (only disable against `localhost`) |

The chart's `config.smtp.*` block fills the first four; the
chart's `secrets.smtp.{username,password}` (or
`secrets.smtp.existingSecret`) carries the credentials
([charts/iterion/templates/secret-smtp.yaml](../charts/iterion/templates/secret-smtp.yaml)).
When `ITERION_SMTP_HOST` is set, the boot log shows
`server: SMTP mailer enabled (host=…)`. `/api/server/info` exposes
`email_enabled` so the SPA hides the forgot-password link when it's
off.

### 1.10 PrometheusRule + PodMonitor

The chart's `metrics.podMonitor.enabled=true` deploys a PodMonitor that
scrapes both server and runner `/metrics`; `metrics.prometheusRule.enabled=true`
ships the starter alert pack
([charts/iterion/templates/prometheus-rule.yaml](../charts/iterion/templates/prometheus-rule.yaml)).
Both depend on the prometheus-operator CRDs being installed in the
cluster — without them, helm will skip the resource.

`/metrics` is **ClusterIP-only** by design; never expose it through an
Ingress. See [quotas-and-limits.md](quotas-and-limits.md) for the
exhaustive metric list and
[charts/iterion/README.md](../charts/iterion/README.md) for the chart
values.

---

## Part 2 — Org admin (team owner / admin)

You're a member of one or more orgs and your role in at least one is
`admin` or `owner`. Everything here is self-serve at
`https://iterion.example.com/teams/<id>`.

### 2.1 Create an inbound webhook from the studio

UI: `/teams/<id>` → Webhooks → "Create webhook".

1. Pick a **provider**: GitLab, GitHub, Forgejo/Gitea, or Generic.
2. Pick the **bot scope**: one bot, several bots, or "wildcard" (any
   bot the catalog ships — needs an explicit checkbox; flagged in
   audit).
3. (Optional) project allowlist (e.g. `acme/repo`, `acme/*`, or `*`).
4. (Optional) event allowlist (defaults are sensible per provider).
5. (Optional) `key_overrides` mapping a provider → BYOK key id so this
   webhook bills against a specific key (see
   [webhooks.md](webhooks.md#key_overrides--pin-a-byok-key-per-webhook)).
6. (Optional) `launch_vars` — operator-pinned vars stamped on every run.

Iterion mints an `iwh_…` token shown **exactly once**. Copy it and the
URL `/api/webhooks/<provider>/<id>` and paste them into the forge:

- **GitLab** — Settings → Webhooks → URL + Secret Token. Tick "Merge
  request events" and "Comments" (for `/revi`).
- **GitHub** — Settings → Webhooks → Payload URL + Secret. Content-type
  `application/json`. Subscribe to "Pull requests" only.
- **Forgejo/Gitea** — Settings → Webhooks → Target URL + Secret. Pick
  "Pull Request" events.
- **Generic** — your own client. Send `X-Iterion-Webhook-Token: iwh_…`
  in the request header.

CLI equivalent:

```bash
curl -X POST https://iterion.example.com/api/teams/$TEAM_ID/webhooks \
  -H "Authorization: Bearer $TOKEN" \
  -d '{
    "name":     "GitLab MR review",
    "provider": "gitlab",
    "bot_ids":  ["review-pr"],
    "project_allowlist": ["acme/*"]
  }'
# Response: { "config": { ... }, "token": "iwh_…" }
```

Rotate later via `POST /api/teams/{id}/webhooks/{webhook_id}/rotate` —
a fresh `iwh_` is returned and the HMAC seal is refreshed in lockstep.

The full reference (auth modes, per-provider filters, idempotency, the
`/revi` command) lives in [webhooks.md](webhooks.md).

### 2.2 Add generic secrets and bind them to a bot

A **generic secret** is a string value the org wants its bots to use —
a forge personal-access-token, a deploy key, etc. A **bot-secret
binding** ties a stored secret to one bot under the name the workflow
declares in its `secrets:` block, optionally narrowing the egress
allowlist further than the workflow does (ADR
[docs/adr/018](adr/018-bot-secret-binding-egress-enforcement.md)).

```bash
# 1. Store the secret (team-scoped). Returns metadata only; the
#    plaintext is sealed at rest.
curl -X POST https://iterion.example.com/api/teams/$TEAM_ID/secrets \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"name":"gitlab_pat","value":"glpat-xxx","allowed_hosts":["gitlab.example.com"]}'
# Response: { "id": "sec_…", "name": "gitlab_pat", "last4": "…", … }

# 2. Bind it to the review-pr bot under the workflow name "forge_token"
#    with hosts intersected to gitlab.example.com only.
curl -X POST https://iterion.example.com/api/teams/$TEAM_ID/bots/review-pr/bindings \
  -d '{
    "secret_id": "sec_…",
    "secret_name_for_workflow": "forge_token",
    "allowed_hosts": ["gitlab.example.com"]
  }'
```

UI path: `/teams/<id>` → Secrets → "New secret"; then Bots → review-pr
→ "Bind a secret" → pick the stored one.

The full binding resolution chain (user > binding > team) and what
`allowed_hosts` enforces is in
[secrets-reference.md](secrets-reference.md).

### 2.3 Watch usage

UI path: `/teams/<id>` → Usage. The run/cost counters are **org-scoped**
(they sum every team in the org), so the API is keyed on the org:

```bash
curl https://iterion.example.com/api/orgs/$ORG_ID/usage
```

The view fields are documented in
[quotas-and-limits.md](quotas-and-limits.md#reading-usage). Members can
read it; only admins can change the underlying limits (that's the
super-admin's
`/api/admin/orgs/{id}` endpoint above).

### 2.4 Read the team audit log

```bash
curl "https://iterion.example.com/api/teams/$TEAM_ID/audit?limit=100"
# Filter:
curl "https://iterion.example.com/api/teams/$TEAM_ID/audit?action=byok.created"
curl "https://iterion.example.com/api/teams/$TEAM_ID/audit?actor=$USER_ID"
```

UI path: `/teams/<id>` → Audit.

**Requires team admin** (not just membership) because rows expose
member emails and IPs. Tokens are stable and listed in §1.5.

### 2.5 Invite members

```bash
# Mint an invitation token. The token is returned exactly once.
curl -X POST https://iterion.example.com/api/teams/$TEAM_ID/invitations \
  -d '{"email":"bob@acme.example","role":"member"}'
# Response: { "invitation": { … }, "token": "…" }
```

UI path: `/teams/<id>` → Members → "Invite".

When SMTP is configured, the new member receives an email with a link
to `/auth/invitations/lookup?token=…` and creates an account from
there. Without SMTP, the in-band response token must be sent to them
out-of-band (email / chat / SMS).

Invitations expire in 7 days; revoke with
`DELETE /api/teams/{id}/invitations/{invite_id}`.

### 2.6 Personal access tokens for CI

PATs (`iap_…`) are long-lived bearer credentials for programmatic API
access where the 15-minute JWT + refresh dance is impractical
([pkg/pat/pat.go](../pkg/pat/pat.go)). They authenticate **as the
issuing user** with that user's role (including super-admin if
applicable); v1 has no scope axis.

```bash
# Mint a PAT pinned to one team, expiring in 90 days.
curl -X POST https://iterion.example.com/api/me/tokens \
  -d '{"name":"github-actions","team_id":"team_…","expires_in_days":90}'
# Response: { "pat": { … }, "token": "iap_…" }

# Use it on any /api/* request as the bearer:
curl https://iterion.example.com/api/runs \
  -H "Authorization: Bearer iap_…"

# List + revoke.
curl https://iterion.example.com/api/me/tokens
curl -X DELETE https://iterion.example.com/api/me/tokens/<token_id>
```

UI path: `/account` → Personal access tokens.

The platform operator can cap every PAT's lifetime via
`ITERION_PAT_MAX_TTL` (Go duration, e.g. `2160h` = 90 days). A
missing or longer expiry is clamped to the ceiling at mint time
([pkg/server/pat_routes.go:handleCreatePAT](../pkg/server/pat_routes.go)).

Mitigations against the unscoped-bearer risk: the optional team pin
(scoped membership re-checked on every use), the optional expiry, the
platform `ITERION_PAT_MAX_TTL`, instant revocation, and audit rows on
create / revoke. Member removal kills the PAT immediately — the
membership re-check at every use returns "token team unavailable".

---

## Where things live

| Topic | File |
|---|---|
| Full chart values + secret table | [charts/iterion/README.md](../charts/iterion/README.md) |
| Inbound webhook reference (auth modes, per-provider behaviour) | [webhooks.md](webhooks.md) |
| Quota / metering / denial reasons / metrics | [quotas-and-limits.md](quotas-and-limits.md) |
| Every kind of secret + where it's resolved | [secrets-reference.md](secrets-reference.md) |
| Every REST endpoint + auth class | [cloud-rest-api.md](cloud-rest-api.md) |
| Control plane vs data plane + queue internals | [cloud-architecture.md](cloud-architecture.md) |
| The operator runbook (chart install, secrets, NetworkPolicy) | [cloud-deployment.md](cloud-deployment.md) |
| The end-user-side flows (login, BYOK, OAuth-forfait) | [cloud-user.md](cloud-user.md) |
