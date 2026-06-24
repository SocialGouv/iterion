[← Documentation index](README.md) · [← BaaS overview](baas-overview.md)

# Inbound webhooks

**Audience.** Org admins wiring a forge or a custom caller to iterion,
and operators reviewing the auth + rate-limit + audit story before
opening `/api/webhooks/*` to public traffic.

This document covers **inbound** webhooks — external events arriving on
`/api/webhooks/<provider>/<id>` to launch a bot. The mirror feature
("call me back when this run finishes") is documented in
[outbound-callbacks.md](outbound-callbacks.md).

> **Prefer the Integrations tab for forge repos.** The manual lifecycle
> below (mint a token, paste the URL + token into the forge) is the
> low-level path. For a GitLab/GitHub/Forgejo repo, the studio's
> **Integrations** tab connects the forge once (OAuth or PAT) and
> **provisions the hook + token + this webhook config for you** when you
> enable a bot — see [forge-integrations.md](forge-integrations.md). Such
> configs carry a `provisioned_by` marker, render read-only here, and
> reject direct delete/rotate (409) — manage them from Integrations. The
> manual path remains for the Generic JSON trigger and for webhooks you
> want to own by hand.

Four providers are wired: GitLab (incl. `/revi` re-review command),
GitHub, Forgejo/Gitea, and a bot-agnostic Generic JSON endpoint
([pkg/server/webhooks_routes.go:supportedProviders](../pkg/server/webhooks_routes.go)).

## Lifecycle

1. An org admin creates a webhook through the studio (`Webhooks` tab on
   `/teams/<id>`) or the API. Iterion mints an `iwh_…` token (32 bytes
   of randomness behind a recognisable prefix) and returns it **exactly
   once** alongside the new `Config` document
   ([pkg/webhooks/token.go:MintToken](../pkg/webhooks/token.go)).
2. The admin pastes the inbound URL + token into the forge:
   - GitLab → Settings → Webhooks → URL `https://…/api/webhooks/gitlab/<id>` + Secret Token `iwh_…`
   - GitHub → Settings → Webhooks → Payload URL `https://…/api/webhooks/github/<id>` + Secret `iwh_…`
   - Forgejo/Gitea → Settings → Webhooks → Target URL + Secret `iwh_…`
   - Generic → any HTTP client, header `X-Iterion-Webhook-Token: iwh_…`
3. From then on, each delivery is admitted through the middleware,
   parsed by the provider, dispatched to a bot, and recorded as a
   `Delivery` row. The token plaintext is **not** kept at rest — only a
   salted hash, the last 4 chars, and a SHA-256 fingerprint
   ([pkg/webhooks/types.go:Config](../pkg/webhooks/types.go)).

Rotate or revoke at any time: `POST /api/teams/{id}/webhooks/{webhook_id}/rotate`
returns a fresh plaintext (also shown once) and updates the forge's
"secret" field is then a manual step.

## Auth modes — token vs HMAC

Iterion's middleware has two authentication modes, picked per provider
to match how the forge actually signs the request
([pkg/webhooks/types.go:SignatureMode](../pkg/webhooks/types.go),
[pkg/server/webhooks_routes.go:defaultSignMode](../pkg/server/webhooks_routes.go)).

| Provider | Default `sign_mode` | What proves authenticity | Header iterion reads |
|---|---|---|---|
| GitLab | `token` | The forge echoes the `iwh_` plaintext verbatim | `X-Gitlab-Token` (or `X-Iterion-Webhook-Token`) |
| GitHub | `hmac` (forced) | HMAC-SHA256 of the raw body, key = `iwh_` plaintext | `X-Hub-Signature-256` |
| Forgejo/Gitea | `hmac` (forced) | HMAC-SHA256 of the raw body | `X-Forgejo-Signature` (falls back to `X-Gitea-Signature`) |
| Generic | `token` (default; `hmac` opt-in) | Header bearer token, or HMAC of body | `X-Iterion-Webhook-Token` / `X-Iterion-Webhook-Signature` |

The same `iwh_…` plaintext that's shown at create time is used in both
modes — operators paste it once into the forge's "secret" field. For
HMAC providers, iterion seals that plaintext at rest under an AAD bound
to the webhook ID
([pkg/webhooks/token.go:SealHMACSecret](../pkg/webhooks/token.go)) so
the same value can be reused on every delivery to recompute the
signature without storing cleartext. Rotating the token reseals it.

**Why per-provider, not per-org?** GitHub and Forgejo's hooks **only**
sign the body — they don't echo any token header at all, so an operator
who picks token-mode for them would lock themselves out. GitLab's
"Secret Token" field is exactly the bearer model. The middleware skips
the header check entirely under `sign_mode: hmac` so the body bytes
stay intact for the provider handler's signature recomputation
([pkg/server/middleware_webhook.go:webhookAuth](../pkg/server/middleware_webhook.go)).

## Per-provider behaviour

### GitLab (`POST /api/webhooks/gitlab/{id}`)

Single URL, two event kinds dispatched on `X-Gitlab-Event`
([pkg/server/webhooks_gitlab.go](../pkg/server/webhooks_gitlab.go)):

- **`Merge Request Hook`** — auto-review on `open`/`reopen`. Pushes that
  produce action `update` deliberately do **not** re-trigger (auto-review
  on every push was found too noisy; cf.
  [pkg/webhooks/gitlab/parser.go:IsReviewable](../pkg/webhooks/gitlab/parser.go)).
- **`Note Hook`** — on-demand re-review. Only acts when the note hangs
  off an open MR **and** its first non-whitespace token is exactly
  `/revi`. Quoting "please run /revi" mid-text does not trigger
  (anti-oscillation guard;
  [pkg/webhooks/gitlab/note.go:IsReviewCommand](../pkg/webhooks/gitlab/note.go)).

Default event allowlist: `{merge_request, note}` — both kinds reach a
zero-config webhook
([pkg/webhooks/match.go:MatchEvent](../pkg/webhooks/match.go)).
Operators who want only the auto-review path list `["merge_request"]`
explicitly; that disables `/revi` while keeping open/reopen.

Vars stamped on the run: `pr_url`, `base_ref`, `scope_notes`,
`post_to_board=false`, `pr_review_mode=summary`, plus `re_review=true`
for the note path. The webhook's `LaunchVars` override these.

### GitHub (`POST /api/webhooks/github/{id}`)

HMAC over the body, header `X-Hub-Signature-256` (`sha256=<hex>`). Only
`pull_request` events with action `opened` or `reopened` trigger; ping
/ push / issue_comment are silently filtered (returns 200 — a 4xx makes
GitHub disable the webhook after repeated failures;
[pkg/server/webhooks_github.go](../pkg/server/webhooks_github.go)).

### Forgejo / Gitea (`POST /api/webhooks/forgejo/{id}`)

Same wire shape as GitHub-style PRs, two header spellings accepted:
`X-Forgejo-Signature` (current) or `X-Gitea-Signature` (older Gitea
deployments); same for `X-Forgejo-Event` / `X-Gitea-Event`. The
signature header is treated as a hex digest with or without the
`sha256=` prefix
([pkg/server/webhooks_forgejo.go:forgejoSignatureHeader](../pkg/server/webhooks_forgejo.go)).

### Generic (`POST /api/webhooks/generic/{id}`)

Bot-agnostic: the caller picks which bot to launch by name (or relies
on the webhook's `default_bot_id` / single-bot scope). Request shape
([pkg/webhooks/generic/generic.go:Request](../pkg/webhooks/generic/generic.go)):

```json
{
  "bot": "review-pr",
  "vars": { "pr_url": "https://gitlab.local/group/repo/-/merge_requests/7" },
  "idempotency_key": "ci-build-42",
  "repo_url": "https://gitlab.local/group/repo.git",
  "repo_ref": "feature/x",
  "project_path": "group/repo"
}
```

Field bounds: `vars` is capped at **256 keys**, each key must match
`[A-Za-z_][A-Za-z0-9_]{0,63}`, each value at **4 KiB**. Anything else
is a 400 (`generic: too many vars` / `bad var key` / `var value too
large`).

Curl example:

```bash
curl -X POST https://iterion.example.com/api/webhooks/generic/<id> \
  -H "X-Iterion-Webhook-Token: $IWH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "bot": "review-pr",
    "vars": { "pr_url": "https://gitlab.local/group/repo/-/merge_requests/7" },
    "idempotency_key": "ci-build-42"
  }'
```

**Var precedence.** Body vars merge in first; then the webhook's
configured `LaunchVars` **override** them. The operator is the
security-critical knob — a malicious caller cannot escalate by renaming
a key the org-admin has pinned (`handleGenericWebhook` in
[pkg/server/webhooks_generic.go](../pkg/server/webhooks_generic.go)).

## Matching: project + event + author allowlists, bot scope

Every webhook carries four filters
([pkg/webhooks/types.go:Config](../pkg/webhooks/types.go)):

- **`event_allowlist`** — provider-event names allowed; empty defaults
  to the provider's natural set (GitLab uses `{merge_request, note}`,
  the others use `{pull_request}`). A bare `*` matches everything.
- **`project_allowlist`** — `owner/repo` patterns. Empty = every project
  the forge fires for. Supports `*` (any), `owner/*`, or exact paths.
- **`author_allowlist`** — PR/MR author logins allowed to trigger a
  launch. Empty = any author. Case-insensitive; entries may be bot
  logins like `dependabot[bot]` / `renovate[bot]`, so a webhook can
  react ONLY to a dependency bot's PRs and ignore human PRs on the same
  repo. A bare `*` opts back into allow-all. The matched author login is
  also stamped onto every review run as the `pr_author` var. (Applies to
  the PR/MR *open* path; comment/`/revi` triggers use `min_replier_role`
  / `authorized_repliers` instead.) When auto-provisioning, a bot sets
  this from its manifest `forge.webhook.author_allowlist`; co-enabling a
  bot that reviews all authors re-opens the shared webhook.
- **`bot_ids` + `wildcard_bots`** — the only bots a delivery may
  launch. A wildcard (`["*"]` with `wildcard_bots=true`) must be
  declared **explicitly** so the studio + audit can flag it; the create
  endpoint logs a `webhooks: wildcard-bot webhook` warning at WARN level
  and the audit row carries `wildcard_bots: true`
  ([pkg/server/webhooks_routes.go:normalizeBotScope](../pkg/server/webhooks_routes.go)).

For zero-config forge webhooks (no explicit `default_bot_id`, no single
bot in scope) iterion auto-selects the **`review-pr`** bot — the same
default that ships with the Revi catalog
([pkg/server/webhooks_common.go:defaultWebhookBotReviewPR](../pkg/server/webhooks_common.go)).
The generic webhook deliberately does **not** apply this default — a
bot-agnostic endpoint must pick deterministically, so missing-bot is a
400.

## Idempotency

Iterion durably dedupes deliveries via a unique index on
`(tenant_id, idempotency_key)` — a duplicate insert returns
`ErrDuplicate` and the handler replies 200 with `{status:"duplicate",
run_id, delivery_id}` ([pkg/server/webhooks_common.go:insertAndLaunchWebhook](../pkg/server/webhooks_common.go)).
The key space is **provider-prefixed** so the same event id can't
collide across paths:

| Key prefix | Identifying tuple | Bumps on |
|---|---|---|
| `mr\|` | `(tenant, webhook, project_id, mr_iid, head_sha)` | a new push (new head SHA) → fresh launch |
| `note\|` | `(tenant, webhook, project_id, mr_iid, note_id)` | a new `/revi` comment → fresh launch |
| `gh\|` | `(tenant, webhook, project_path, pr_number, head_sha)` | a new push → fresh launch |
| `fj\|` | `(tenant, webhook, project_path, pr_number, head_sha)` | a new push → fresh launch |
| `generic\|` | `(tenant, webhook, request.idempotency_key OR sha256(body))` | any change in dedup token or body → fresh launch |

Terminal (non-launched) rows — `invalid`, `filtered`, `quota_exceeded`,
`rate_limited`, `launch_error` — get a **random UUID** as their
idempotency key so they never collide with the real dedup key
([pkg/server/webhooks_common.go:recordTerminalWebhookDelivery](../pkg/server/webhooks_common.go)).
A retry of the same upstream event after a transient failure can
therefore launch successfully.

## Limits + admission

A delivery passes through these gates in order, each enforced by the
middleware before the provider handler runs
([pkg/server/middleware_webhook.go:webhookAuth](../pkg/server/middleware_webhook.go)):

| Step | Outcome on fail | HTTP |
|---|---|---|
| Resolve `Config` by URL id | not found / provider mismatch | 401 `invalid webhook` |
| Verify token (token-mode only) | bad token | 401 `invalid webhook token` |
| Config not disabled | `enabled=false` | 410 `webhook disabled` |
| Per-webhook token-bucket rate (default `rate=1.0, burst=10`) | bucket empty | 429 with `Retry-After` |
| Per-org monthly call quota (default **10 000**, override `monthly_call_limit`) | quota exhausted | 429 `monthly call quota exceeded` |
| Org status `active` | suspended / read-only | 403 `org suspended` |

Then the provider handler verifies the body (HMAC for github/forgejo,
optional HMAC for generic), parses, applies the event/project/bot
filters above, and finally hands off to `insertAndLaunchWebhook`
which runs the **launch-admission gate** (org-level quotas / cost cap
/ concurrency / launch rate — see
[quotas-and-limits.md](quotas-and-limits.md)) before publishing the
run.

A denial at the launch-admission step writes a `launch_error` delivery
row carrying the stable denial reason (`monthly_run_quota_exceeded`,
`monthly_cost_cap_exceeded`, …) and returns the standard launch-denial
envelope to the forge — so the forge sees a 402/429 it can decide what
to do with, not a synthetic 200.

## Delivery audit + statuses

Every accepted request lands in `webhook_deliveries` with one of these
statuses ([pkg/webhooks/types.go status constants](../pkg/webhooks/types.go)):

| Status | Meaning |
|---|---|
| `accepted` | Auth/quota passed, awaiting launch result (intermediate state) |
| `launched` | Run published to the queue; `run_id` set, `launched_at` stamped |
| `duplicate` | Same idempotency key replayed — `run_id` of the original launch is returned |
| `filtered` | The event didn't match `event_allowlist` / `project_allowlist` / `author_allowlist` / `IsReviewable` |
| `invalid` | Bad payload, missing token, bot not permitted by scope |
| `rate_limited` | Per-webhook bucket empty |
| `quota_exceeded` | Per-org or per-webhook monthly call quota exhausted |
| `launch_error` | The launch-admission gate refused (cost cap / run quota / concurrency / org suspended) OR the runner publisher failed |

Delivery rows never carry the raw payload — only a SHA-256 hash, the
selected fields (`event_kind`, `event_action`, `project_path`,
`subject_id`, `subject_sha`), the source IP, and (for launched rows)
the resulting `run_id`. Read them at
`GET /api/teams/{id}/webhooks/{webhook_id}/deliveries` (last 100 by
default).

## Webhook CRUD API

All routes are mounted under `/api/teams/{id}/webhooks/…` and require
team admin (`canManageTeam`) for mutations; team membership
(`canViewTeam`) for reads
([pkg/server/webhooks_routes.go:registerWebhookRoutes](../pkg/server/webhooks_routes.go)).

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/teams/{id}/webhooks` | team member | List webhooks for the team |
| `POST` | `/api/teams/{id}/webhooks` | team admin | Create + mint token (returned once) |
| `GET` | `/api/teams/{id}/webhooks/{webhook_id}` | team member | Read a single webhook |
| `PATCH` | `/api/teams/{id}/webhooks/{webhook_id}` | team admin | Update name/enabled/scope/rate/quota/vars/key_overrides |
| `DELETE` | `/api/teams/{id}/webhooks/{webhook_id}` | team admin | Remove (deliveries kept for audit) |
| `POST` | `/api/teams/{id}/webhooks/{webhook_id}/rotate` | team admin | Rotate token + re-seal HMAC secret |
| `GET` | `/api/teams/{id}/webhooks/{webhook_id}/deliveries` | team member | List recent deliveries (default 100) |
| `POST` | `/api/webhooks/{provider}/{id}` | webhook token / HMAC | Public delivery endpoint, one per provider |

The `POST` create response shape:

```json
{
  "config": { "id": "…", "tenant_id": "…", "provider": "gitlab", "token_last4": "Vp3a", … },
  "token": "iwh_…"
}
```

The `token` field is the **only** way to recover the plaintext — once
the response is closed, only the salted hash remains. The studio shows
it inside a "copy now, you won't see it again" affordance.

### `key_overrides` — pin a BYOK key per webhook

`key_overrides` maps a provider name (`"anthropic"`, `"openai"`, …) to a
BYOK API-key id owned by the same team. Runs launched through this
webhook then use that exact key for the named provider, overriding the
org/user default in
[pkg/secrets/byok.go:Resolve](../pkg/secrets/byok.go). Use it to bill
several webhooks for the same bot against different keys (e.g. one
"production" webhook on the org's primary key, one "internal-CI"
webhook on a sandbox key). Mismatched provider, missing key, or a key
that belongs to another org are 400s
([pkg/server/webhooks_routes.go:validateKeyOverrides](../pkg/server/webhooks_routes.go)).

### `launch_vars` — pin run vars from the org config

Anything in `launch_vars` is merged into the run's variable map **after**
the handler-derived vars, so the operator's keys always win. Useful for:
e.g. forcing `severity_threshold=high` on a security webhook, or pinning
`pr_review_mode=detailed` regardless of what the forge said.

## Observability

Every delivery bumps a label set on `iterion_webhook_deliveries_total`
(`provider`, `status`) and pre-handler throttles bump
`iterion_webhook_throttled_total` (`provider`, `reason`)
([pkg/cloud/metrics/metrics.go](../pkg/cloud/metrics/metrics.go)). There
are **deliberately no tenant labels** — cardinality discipline — so
per-org accounting lives in Mongo (`org_usage` + `webhook_deliveries`),
not Prometheus.

The starter PrometheusRule pack ships an alert on
`increase(iterion_webhook_throttled_total[1h]) > 50` that surfaces a
noisy forge integration or an abusive caller. See
[charts/iterion/README.md](../charts/iterion/README.md) for the full
alert pack.
