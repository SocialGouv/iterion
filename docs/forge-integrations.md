# Forge integrations (connect a repo, auto-provision)

**Audience.** Org admins who want to wire a GitLab/GitHub/Forgejo repo to a
bot (e.g. Revi on merge-request open) without the manual
PAT→secret→binding→webhook→forge-hook chain.

This is the **outbound** complement of [inbound webhooks](webhooks.md):
inbound (`pkg/webhooks`) authenticates deliveries the forge sends *to*
iterion; forge integrations (`pkg/forge`) hold the admin credential that
lets iterion call *out* to the forge to register that delivery — and the
bot-secret binding — in one action.

## What it does

In the studio, **`/teams/<id>` → Integrations**:

1. **Connect a forge** once — OAuth (when an OAuth app is configured) or
   paste a personal access token (the fallback, and the only path for
   self-hosted instances with no registrable OAuth app). iterion validates
   the token (`GET /user`), reads the identity, and stores the credential
   **sealed**.
2. **Enable a repo** — pick a repo the credential can administer, check the
   forge-capable bots (each shows the events it subscribes to + its manifest
   rationale), and click Enable. In one server action iterion:
   - derives a team-scoped **managed `forge_token`** secret from the
     connection (created once per connection);
   - creates (or extends) the iterion **webhook config** for that repo, with
     a fresh `iwh_` secret it holds on both ends, the events the bots need,
     and a per-webhook `SecretOverrides` pin to the managed token;
   - calls the forge API to **create the webhook** on the repo pointing at
     iterion's inbound URL with that `iwh_` secret;
   - records a **`RepoIntegration`** join row.

Disable is the inverse, one click: delete the forge hook, the webhook
config, and the join row (the managed secret survives — it is shared by the
connection's other repos; it is removed when the connection is deleted).

## What a bot declares (`forge:` in its manifest)

A bot opts into auto-provisioning with a `forge:` block in its
`manifest.yaml` — advisory, discovery-time metadata (like `dispatch_vars`),
read by the orchestrator, not the runtime:

```yaml
forge:
  events: [pull_request, pull_request_comment]  # normalized; mapped per provider
  token_scopes:
    pull_requests: write
    repository: read
  secret: forge_token            # the workflow-secret name the bot consumes
  webhook:
    launch_vars: { pr_review_mode: summary }
    min_replier_role: developer
    author_allowlist: [dependabot[bot], "*dependabot[bot]"]  # react only to these authors
    author_scope: exclusive   # ...and keep them off every other co-enabled bot
  rationale: |
    Shown in the enable dialog so the operator sees why each scope is asked.
```

Normalized event vocabulary (mapped to the forge's native names by
[pkg/forge/event_map.go](../pkg/forge/event_map.go)):

| normalized | GitLab | GitHub | Forgejo |
|---|---|---|---|
| `pull_request` | `merge_request` | `pull_request` | `pull_request` |
| `pull_request_comment` | `note` | `issue_comment` | `issue_comment` |

(GitLab's native names `merge_request` / `note` are translated a second
time — to the boolean request-body fields `merge_requests_events` /
`note_events` — inside the GitLab admin client when the hook is created.)

Unknown events / scope keys / levels fail manifest parsing
([pkg/bundle/manifest.go:validateForgeRequirements](../pkg/bundle/manifest.go)).

Enabling **several** bots on one repo (a reviewer + a dependency guard)
is the common case: the orchestrator writes one `BotRule` per
co-enabled bot, and an inbound delivery fans out to every rule that
claims the event and admits the author. A bot that sets
`author_scope: exclusive` claims its `author_allowlist` as MINE —
provisioning adds those logins to every other co-enabled bot's denylist,
so a general reviewer stops double-reviewing the dependency PRs the
guard owns. See [Inbound webhooks § Per-bot routing](webhooks.md#per-bot-routing-—-co-enabling-several-bots-on-one-repo)
for the runtime fan-out, idempotency, and the suffix-wildcard author
match (`*renovate[bot]`).

## The managed-token design (why the downstream is unchanged)

The connection's admin token (OAuth user token / installation token / PAT)
is used **only** to manage iterion's footprint on the forge — create hooks,
list repos. It is **never** what a bot posts with. Instead the orchestrator
derives a managed, team-scoped `forge_token` generic secret from it and pins
it on the webhook via `SecretOverrides` (Tier-0 in
`ResolveGenericWithBindings`). So the entire existing run path —
`forge_token` → `RunBundle` → `/run/iterion/secrets/forge_token` →
`glab`/`gh` — is **unchanged**. OAuth/App tokens are kept fresh by a
background worker ([pkg/forge/refresh.go](../pkg/forge/refresh.go)) that
re-seals the connection blob, then rewrites the managed secret's plaintext;
PAT connections never refresh.

> Note on *identity* (vs envelope): "never what a bot posts with" means the
> **sealing envelope** differs (`forge_conn:<id>` → `generic_secret:<id>`), not
> the credential — the managed secret holds the **same token value**, so a bot
> acts as the **connection's identity** on the forge. Who that is, how it relates
> to the iterion user who launched the bot (it doesn't — different planes),
> least-privilege, and GitHub vs GitLab vs Forgejo are documented in
> **[forge-permissions.md](forge-permissions.md)**.

## Configuration (cloud)

The Mongo stores (`forge_connections`, `repo_integrations`,
`forge_oauth_apps`) and the routes are wired automatically in `iterion
server` cloud mode. OAuth apps are registered **per team, per provider,
per instance** at runtime via the `/api/teams/{id}/forge/oauth-apps`
store — the legacy process-global `ITERION_FORGE_*_OAUTH_*` env map has
been replaced and is no longer read. A `(tenant, provider, base URL)`
with no registered app offers only the PAT path; register one (or use the
GitHub App-manifest flow) to enable OAuth, with no redeploy.

`PublicURL` must be set (the OAuth redirect + the forge hook URL are built
from it).

## Security envelopes

- Connection tokens are sealed with AAD `forge_conn:<id>`; the managed
  secret keeps the existing `generic_secret:<id>` AAD; a **self-service
  manifest-created** GitHub App's private key is sealed in the
  `forge_oauth_apps` store (AAD `forge_oauth_app_key:<id>`), while the
  legacy **platform-global** GitHub-App key lives in deployment config. No
  token is ever logged or placed in a URL.
- `Connection.ForgeBaseURL` is threaded onto `webhooks.Config.ForgeBaseURL`
  so the existing inbound SSRF host-pin keeps applying; the global
  `ITERION_WEBHOOK_FORGE_HOSTS` allowlist still wins.
- The `iwh_` webhook secret is minted server-side and **never shown to the
  operator** — iterion holds both ends. A fresh one is minted on every
  mutating provision so the forge hook secret and the iterion config hash
  stay in lockstep without needing the prior plaintext.
- Insufficient scope (the token can't create a hook) surfaces as a
  structured `insufficient_scope` 403 so the studio can prompt to reconnect
  with broader scope or paste a PAT.

## API

All under `/api/teams/{id}/forge/`. Connection/repo mutations require a team
admin or owner; connection health and GitHub-App grant refresh are available to
a team member. The OAuth callback is a public IdP redirect target authenticated
by signed state + an agent-binding cookie:

| Method | Path | Purpose |
|---|---|---|
| GET | `/connections` | list connections |
| POST | `/connections` | connect (`mode: pat` \| `oauth` \| `app`) → `{connection}`, `{authorize_url}`, or `{install_url}` (GitHub-only `app`) |
| DELETE | `/connections/{conn_id}` | disconnect (deprovisions every repo first) |
| GET | `/connections/{conn_id}/health` | stored + live connection and token health |
| POST | `/connections/{conn_id}/refresh` | GitHub App only: re-probe installation grants, persist them, and mint a fresh token |
| GET | `/connections/{conn_id}/repos?search=&page=` | repo picker (admin-capable only) |
| GET | `/api/forge/oauth/callback` | **public** OAuth redirect target |
| GET | `/repo-bots` | list active integrations |
| GET | `/repo-bots/preview?connection_id=&bots=` | events + scopes + conflicts, no writes |
| POST | `/repo-bots` | enable `{connection_id, repo, bot_ids}` → `ProvisionResult` |
| PATCH | `/repo-bots/{integration_id}` | update an integration's bot set |
| DELETE | `/repo-bots/{integration_id}` | disable |

Both write routes also carry the repo's **operator-owned** settings —
`launch_vars`, `overlap`, `hold_labels`, `label_allowlist`,
`auto_fix_on_gate_failure`. They live on the integration because provisioning
rebuilds the whole webhook config from the manifests: anything set only on that
config is wiped by the next enable/update. Omitting a field keeps the stored
value; an explicit empty list clears it. `label_allowlist` is the one that
decides which freshly-applied issue label dispatches the implementer (empty =
any label does).

## Provider support

| Provider | Mode(s) | Status |
|---|---|---|
| GitLab | OAuth · PAT | live ([pkg/forge/gitlab](../pkg/forge/gitlab)) |
| GitHub | OAuth App · PAT · **GitHub App** | live ([pkg/forge/github](../pkg/forge/github)) |
| Forgejo / Gitea | OAuth · PAT¹ | live ([pkg/forge/forgejo](../pkg/forge/forgejo)) |

¹ Forgejo OAuth-token API auth uses the Gitea `token` scheme like the PAT
path; validate against a live instance before relying on the OAuth (vs PAT)
mode there.

The **GitHub App** mode (a third connect option for GitHub) authenticates
with a per-installation token minted on demand from the App private key
(RS256 JWT → `POST /app/installations/{id}/access_tokens`, cached in the App
client and re-minted ≈60 s before its ~1 h expiry — distinct from the
`RefreshWorker`, whose 5-minute lead rewrites the managed `forge_token` for
OAuth/PAT refresh-token connections). The operator installs
the App via `github.com/apps/<slug>/installations/new?state=…`; GitHub's
"Setup URL" must point at `<PublicURL>/api/forge/github/app/callback`. The
App posts as itself (`<slug>[bot]`), needs no user seat, and uses
fine-grained per-repo permissions. Config:

```
ITERION_FORGE_GITHUB_APP_ID
ITERION_FORGE_GITHUB_APP_PRIVATE_KEY_FILE   (PEM; or _PRIVATE_KEY inline)
ITERION_FORGE_GITHUB_APP_SLUG
```

After changing a GitHub App installation's permissions, run
`iterion remote forge refresh <connection-id>` (or call the refresh endpoint)
to pick them up immediately. The response separates the installation's live
grants from the permissions carried by the newly minted token; those can differ,
and the token is what acts.

Adding a provider = implement the `forge.Admin` interface (+ an
`OAuthExchanger`/`TokenRefresher` for OAuth) and register it in the server's
provider dispatch; the orchestrator + studio are provider-agnostic.
