# Cloud REST API reference

**Audience.** Anyone calling iterion programmatically — a CI job, an
SDK author, an operator writing curl runbooks. Every endpoint listed
here exists in [pkg/server/](../pkg/server/), grouped by domain.

This page is **curated, not exhaustive**: it covers the cloud- and
team-facing surface, and leaves out the local-studio-only routes
(`/api/local/*`, `/api/files`, `/api/projects`, `/api/examples`,
`/api/filesystem/*`) along with much of the run-console read surface.
A few team-facing families have their own reference instead of a table
here: `/api/admin/llm/*` ([cloud-llm-credentials.md](cloud-llm-credentials.md)),
`/api/admin/settings/usage-caps` ([usage-caps.md](usage-caps.md)),
the notification routes ([notifications.md](notifications.md)), the
config-share routes ([config-share.md](config-share.md)), and
`/api/teams/{id}/plugin-sources*` ([plugins.md](plugins.md)).
For the complete inventory of the build you are actually running, use
the generated spec — every route is recorded by the server's
`recordingMux`, so it cannot drift:

```bash
iterion openapi                 # OpenAPI 3.1 for this binary, offline, to stdout
curl .../api/openapi.json       # the same, from a live instance
curl .../api/routes             # just the method+pattern inventory
```

(The `/api/v1/native`, `/api/v1/dispatcher` and `/api/v1/mcp/board` CRUD
sub-trees register on the *same* mux but bypass the route recorder — they
are handed the bare `ServeMux` rather than the recording wrapper — so
they are deliberately absent from that spec. The exception worth knowing:
the forge-facing `/api/v1/native/issues/*` routes and the dependency
graph go through the recorded path and therefore **do** appear.)

Authentication. Most routes accept any of:

- **Cookie**: `iterion_auth` (access JWT) + `iterion_refresh` (rotation).
- **Bearer JWT**: `Authorization: Bearer <access-jwt>` issued by login
  / refresh.
- **Bearer PAT**: `Authorization: Bearer iap_…` — long-lived personal
  access token; authenticates **as** the issuing user with that user's
  role + super-admin flag
  ([pkg/server/pat_routes.go:identityFromPAT](../pkg/server/pat_routes.go)).
- **WS query**: `?t=<access-jwt>` for WebSocket clients that can't set
  headers.

Where a route says "team member", "team admin" or "super-admin", the
guard maps to `canViewTeam` / `canManageTeam` / `requireSuperAdmin`.
Webhook delivery URLs (`POST /api/webhooks/<provider>/<id>`) use their
own auth (token bearer or HMAC body signature) and are public to the
JWT layer.

## Authentication + identity

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `POST` | `/api/auth/login` | public | Email + password login |
| `POST` | `/api/auth/refresh` | refresh cookie | Rotate access JWT |
| `POST` | `/api/auth/logout` | public | Drop refresh session + cookies |
| `POST` | `/api/auth/register` | public (when `signup_mode=open` or with invite) | Create account |
| `POST` | `/api/auth/password/change` | public (legacy) | First-login password rotation for `pending_password_change` users |
| `POST` | `/api/auth/password/reset/request` | public | Mint + email a reset token (always 200, no enumeration) |
| `POST` | `/api/auth/password/reset/confirm` | public | Redeem `iar_…`, set new password, issue fresh session |
| `GET` | `/api/auth/providers` | public | List configured OIDC connectors + `signup_mode` |
| `GET` | `/api/auth/oidc/{provider}/start` | public | Start OIDC dance |
| `GET` | `/api/auth/oidc/{provider}/callback` | public | OIDC redirect target |
| `GET` | `/api/auth/invitations/lookup` | public | Resolve invitation token → email + team |
| `POST` | `/api/auth/invitations/accept` | member | Accept an invitation while logged in |
| `GET` | `/api/auth/me` | member | Current user + active team identity |
| `POST` | `/api/auth/me/team/{team_id}` | member | Switch active team |
| `POST` | `/api/auth/me/org/{org_id}` | member | Switch active org (re-issues the JWT; validates org then team) |
| `POST` | `/api/me/password` | member | Self-service password change |
| `POST` | `/api/me/sessions/revoke-all` | member | Sign out every device |

Source: [pkg/server/auth_routes.go](../pkg/server/auth_routes.go) +
[pkg/server/password_routes.go](../pkg/server/password_routes.go).

## Teams + members + invitations

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/teams` | member | List the caller's teams |
| `POST` | `/api/teams` | member | Create a team |
| `GET` | `/api/teams/{id}/members` | team member | List members |
| `PATCH` | `/api/teams/{id}/members/{user_id}` | team admin | Change role |
| `DELETE` | `/api/teams/{id}/members/{user_id}` | team admin | Remove a member |
| `GET` | `/api/teams/{id}/invitations` | team admin | List pending invitations |
| `POST` | `/api/teams/{id}/invitations` | team admin | Mint a token (shown once) |
| `DELETE` | `/api/teams/{id}/invitations/{invite_id}` | team admin | Revoke |
| `GET` | `/api/orgs/{id}/usage` | org member | Org-member mirror of the admin usage view (see below) |
| `GET` | `/api/teams/{id}/audit` | team admin | Tenant audit log |

## Organisations — self-serve (org members, teams, SSO)

The org-admin self-serve mirror of the super-admin org views (two-level
tenancy, ADR-048). All routes are `requireAuth`; the org role is checked
in-handler — read routes need org **membership** (`canViewOrg`), mutations
need org **admin/owner** (`canManageOrg`). Sources:
[pkg/server/orgs_routes.go](../pkg/server/orgs_routes.go),
[pkg/server/org_sso_routes.go](../pkg/server/org_sso_routes.go),
[pkg/server/org_sso_domain_routes.go](../pkg/server/org_sso_domain_routes.go).

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/orgs/{id}/members` | org member | List org members + roles |
| `PATCH` | `/api/orgs/{id}/members/{user_id}` | org admin | Change a member's org role (`member\|admin\|owner`) |
| `DELETE` | `/api/orgs/{id}/members/{user_id}` | org admin | Remove a member |
| `GET` | `/api/orgs/{id}/invitations` | org admin | List pending org invitations |
| `POST` | `/api/orgs/{id}/invitations` | org admin | Mint an org invitation token |
| `DELETE` | `/api/orgs/{id}/invitations/{invite_id}` | org admin | Revoke |
| `GET` | `/api/orgs/{id}/teams` | org member | List the org's teams |
| `POST` | `/api/orgs/{id}/teams` | org admin | Create a team in the org |
| `GET` | `/api/orgs/{id}/audit` | org admin | Org audit log |
| `GET` | `/api/orgs/{id}/sso/providers` | org member | List SSO providers |
| `POST` | `/api/orgs/{id}/sso/providers` | org admin | Add an SSO provider (OIDC) |
| `PATCH` | `/api/orgs/{id}/sso/providers/{provider_id}` | org admin | Update a provider |
| `DELETE` | `/api/orgs/{id}/sso/providers/{provider_id}` | org admin | Remove a provider |
| `POST` | `/api/orgs/{id}/sso/providers/{provider_id}/test` | org admin | Test a provider's config |
| `GET` | `/api/orgs/{id}/sso/domains` | org member | List claimed SSO domains |
| `POST` | `/api/orgs/{id}/sso/domains` | org admin | Claim a domain |
| `POST` | `/api/orgs/{id}/sso/domains/{domain_id}/verify` | org admin | Verify a claimed domain |
| `DELETE` | `/api/orgs/{id}/sso/domains/{domain_id}` | org admin | Release a domain |

## BYOK LLM keys + generic secrets + bindings

User-scoped + team-scoped flavours share the same payload shape. Both
return metadata only — the plaintext is **write-only**.

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/teams/{id}/api-keys` | team member | List team's BYOK keys |
| `POST` | `/api/teams/{id}/api-keys` | team admin | Create |
| `PATCH` | `/api/teams/{id}/api-keys/{key_id}` | team admin | Toggle default / rename |
| `DELETE` | `/api/teams/{id}/api-keys/{key_id}` | team admin | Delete |
| `GET` | `/api/me/api-keys` | member | List own user-scoped keys |
| `POST` | `/api/me/api-keys` | member | Create personal key |
| `PATCH` | `/api/me/api-keys/{key_id}` | member | Update |
| `DELETE` | `/api/me/api-keys/{key_id}` | member | Delete |
| `GET` | `/api/teams/{id}/secrets` | team member | List team's generic secrets |
| `POST` | `/api/teams/{id}/secrets` | team admin | Create |
| `PATCH` | `/api/teams/{id}/secrets/{secret_id}` | team admin | Update |
| `DELETE` | `/api/teams/{id}/secrets/{secret_id}` | team admin | Delete |
| `GET` | `/api/me/secrets` | member | Personal secrets |
| `POST` | `/api/me/secrets` | member | Create |
| `PATCH` | `/api/me/secrets/{secret_id}` | member | Update |
| `DELETE` | `/api/me/secrets/{secret_id}` | member | Delete |
| `GET` | `/api/teams/{id}/bots/{bot_id}/bindings` | team member | List bot bindings |
| `POST` | `/api/teams/{id}/bots/{bot_id}/bindings` | team admin | Create binding |
| `PATCH` | `/api/teams/{id}/bots/{bot_id}/bindings/{binding_id}` | team admin | Update |
| `DELETE` | `/api/teams/{id}/bots/{bot_id}/bindings/{binding_id}` | team admin | Delete |

Sources:
[pkg/server/byok_routes.go](../pkg/server/byok_routes.go),
[pkg/server/generic_secrets_routes.go](../pkg/server/generic_secrets_routes.go),
[pkg/server/bot_bindings_routes.go](../pkg/server/bot_bindings_routes.go).
Full semantics in [secrets-reference.md](secrets-reference.md).

## Team bot sources (cloud bot editing)

Team-authored bot bundles — the writable, tenant-scoped store the studio
editor saves into (cloud pods bake the catalog read-only). Each source is a
multi-file bundle (`main.bot` + `manifest.yaml` + `skills/`…). Edit rights =
the `config_editor` capability, team admin, or owner (`canEditBots`); if the
server has no bot-source store wired every route returns `501 bot editing is
not enabled on this server`.

| Method | Path | Access | Purpose |
|---|---|---|---|
| `GET` | `/api/teams/{id}/bot-sources` | bot editor | List the team's bots (metadata only — file bodies omitted) |
| `GET` | `/api/teams/{id}/bot-sources/{slug}` | bot editor | One bot with its full file map |
| `PUT` | `/api/teams/{id}/bot-sources/{slug}` | bot editor | Create or replace the whole bundle (`{files, version?}`) |
| `PUT` | `/api/teams/{id}/bot-sources/{slug}/files/{path...}` | bot editor | Per-file save (`{content, version?}`) |
| `DELETE` | `/api/teams/{id}/bot-sources/{slug}/files/{path...}` | bot editor | Delete one file (never `main.bot`) |
| `DELETE` | `/api/teams/{id}/bot-sources/{slug}` | bot editor | Delete the bot |
| `POST` | `/api/teams/{id}/bot-sources/{slug}/fork` | bot editor | Fork a baked catalog bot (`{from}`) into an editable copy |

Every write **compiles the bundle before it persists** — a bot that fails to
parse/compile is rejected `400 bot does not compile: <diagnostics>`, never
left to fail at launch. A non-zero `version` is an optimistic if-match token
(`409` if a concurrent editor advanced it); slug collisions are `409`. Source:
[pkg/server/bot_sources_routes.go](../pkg/server/bot_sources_routes.go),
store [pkg/botsource/](../pkg/botsource/botsource.go).

## Forge integrations (connections, OAuth apps, repo-bots)

The team-scoped, **outbound** forge layer behind the studio's repo-first shell
([docs/repo-scope.md](repo-scope.md)): connect a forge, hold an OAuth app /
GitHub App credential, and provision a set of bots onto a repo (webhook + hook
+ managed secret + bindings). All routes are `requireAuth`; the team role is
checked in-handler — read routes need team **membership** (`canViewTeam`) and
most mutations need team **admin/owner** (`canManageTeam`). The GitHub-App
connection refresh is the deliberate exception: like connection health, it
requires membership. Sources:
[pkg/server/forge_routes.go](../pkg/server/forge_routes.go),
[forge_refresh_route.go](../pkg/server/forge_refresh_route.go),
[forge_connect_routes.go](../pkg/server/forge_connect_routes.go),
[forge_oauth_app_routes.go](../pkg/server/forge_oauth_app_routes.go),
[forge_github_manifest_routes.go](../pkg/server/forge_github_manifest_routes.go),
[forge_provisioning_routes.go](../pkg/server/forge_provisioning_routes.go),
[board_forge.go](../pkg/server/board_forge.go).

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/teams/{id}/forge/connections` | team member | List forge connections |
| `POST` | `/api/teams/{id}/forge/connections` | team admin | Connect a forge (PAT / OAuth / GitHub-App install) |
| `DELETE` | `/api/teams/{id}/forge/connections/{conn_id}` | team admin | Remove a connection |
| `GET` | `/api/teams/{id}/forge/connections/{conn_id}/health` | team member | Connection health / token probe |
| `POST` | `/api/teams/{id}/forge/connections/{conn_id}/refresh` | team member | GitHub App only: re-probe live installation grants, persist them, and force a fresh token mint |
| `GET` | `/api/teams/{id}/forge/connections/{conn_id}/repos` | team member | Repos visible to the connection |
| `GET` | `/api/teams/{id}/forge/repos` | team member | Team's forge-linked repos |
| `POST` | `/api/teams/{id}/forge/repos` | team admin | Create a repo (opt-in `RepoCreator` capability) |
| `GET` | `/api/teams/{id}/forge/oauth-apps` | team member | List per-tenant OAuth apps |
| `POST` | `/api/teams/{id}/forge/oauth-apps` | team admin | Register an OAuth app (`manual` / `auto` / `auto_from_connection`) |
| `DELETE` | `/api/teams/{id}/forge/oauth-apps/{app_id}` | team admin | Remove an OAuth app |
| `POST` | `/api/teams/{id}/forge/oauth-apps/github-manifest` | team admin | Start the GitHub App-manifest auto-create flow |
| `GET` | `/api/teams/{id}/forge/repo-bots` | team member | List repo→bot provisionings (integrations) |
| `GET` | `/api/teams/{id}/forge/repo-bots/preview` | team member | Preview what enabling a bot set subscribes to (no forge writes) |
| `POST` | `/api/teams/{id}/forge/repo-bots` | team admin | Enable bots on a repo (provision webhook + hook + secret + bindings) |
| `PATCH` | `/api/teams/{id}/forge/repo-bots/{integration_id}` | team admin | Set the exact bot set (replace semantics) |
| `DELETE` | `/api/teams/{id}/forge/repo-bots/{integration_id}` | team admin | Disable / deprovision (tears down webhook + hook) |
| `PATCH` | `/api/teams/{id}/forge/integrations/{iid}` | team admin | Update an integration (incl. `sync_issues_enabled`) |
| `POST` | `/api/teams/{id}/forge/integrations/{iid}/sync` | team admin | Run the forge→board issue sync now (one-way, forge is source) |
| `GET` | `/api/teams/{id}/forge/integrations/{iid}/hooks` | team member | List the webhooks on an integration |

The OAuth handshake completes on public callbacks the SPA is redirected to:
`GET /api/forge/oauth/callback`, `GET /api/forge/github/app/callback`, and
`GET /api/forge/github/app-manifest/callback`.

## Inbound webhooks

CRUD (operator-side) plus per-provider delivery URLs.

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/teams/{id}/webhooks` | team member | List |
| `POST` | `/api/teams/{id}/webhooks` | team admin | Create + mint `iwh_` token (shown once) |
| `GET` | `/api/teams/{id}/webhooks/{webhook_id}` | team member | Get one |
| `PATCH` | `/api/teams/{id}/webhooks/{webhook_id}` | team admin | Update |
| `DELETE` | `/api/teams/{id}/webhooks/{webhook_id}` | team admin | Delete |
| `POST` | `/api/teams/{id}/webhooks/{webhook_id}/rotate` | team admin | Rotate token + re-seal HMAC |
| `GET` | `/api/teams/{id}/webhooks/{webhook_id}/deliveries` | team member | Last ~100 deliveries |
| `POST` | `/api/webhooks/gitlab/{id}` | webhook token | Inbound delivery (MR + `/revi`) |
| `POST` | `/api/webhooks/github/{id}` | webhook HMAC | Inbound PR delivery |
| `POST` | `/api/webhooks/forgejo/{id}` | webhook HMAC | Inbound PR delivery (also Gitea headers) |
| `POST` | `/api/webhooks/generic/{id}` | webhook token (or HMAC opt-in) | Bot-agnostic JSON delivery |

Source: [pkg/server/webhooks_routes.go](../pkg/server/webhooks_routes.go).
Full reference: [webhooks.md](webhooks.md).

## OAuth-forfait (Claude / Codex)

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/me/oauth/connections` | member | List configured forfait kinds + expiry |
| `POST` | `/api/me/oauth/{kind}/authorize/start` | member | Begin the browser authorize handshake — the primary way a user connects |
| `POST` | `/api/me/oauth/{kind}/authorize/complete` | member | Finish that handshake |
| `POST` | `/api/me/oauth/{kind}/credentials` | member | Upload pasted `credentials.json` / `auth.json` |
| `POST` | `/api/me/oauth/{kind}/refresh` | member | Refresh stored access token against the IdP |
| `PATCH` | `/api/me/oauth/{kind}` | member | Name the account behind the credential (`{"account_label": "…"}`, `""` clears) — metadata only, the sealed credential is untouched |
| `DELETE` | `/api/me/oauth/{kind}` | member | Disconnect |

`authorize/complete` and `credentials` take an optional `?account_label=`
query parameter to name the account at connect time. The listing exposes
`account_label` beside `fingerprint`, the same string the publisher logs
when it picks a credential (see
[cloud-llm-credentials.md](cloud-llm-credentials.md#name-the-account-behind-every-credential)).

Every route above has a team-scoped mirror at
`/api/teams/{id}/oauth/…` (`connections`, `{kind}/authorize/start`,
`{kind}/authorize/complete`, `{kind}/credentials`, `{kind}/refresh`,
`PATCH {kind}`, and `DELETE {kind}`) for a forfait the whole team draws
on rather than one operator, and a platform mirror at
`/api/admin/llm/oauth/…` (super-admin).

Source: [pkg/server/oauth_routes.go](../pkg/server/oauth_routes.go),
[pkg/server/oauth_team_routes.go](../pkg/server/oauth_team_routes.go).

## Credential pool

Lending your own subscription or personal metered key to the shared
pool — full model in [credential-pool.md](credential-pool.md).

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/me/pool` | member | Your pledges and what they have served |
| `PUT` | `/api/me/pool/{source}/{ref}` | member | Create or update a pledge (ceilings, sharing window, bot allow-list) |
| `DELETE` | `/api/me/pool/{source}/{ref}` | member | Withdraw a pledge |
| `GET` | `/api/me/pool/history` | member | The runs your quota served |
| `GET` | `/api/teams/{id}/pool` | team member | The pool's policy and its donors |
| `PUT` | `/api/teams/{id}/pool` | team admin | Set the audience policy deciding who may draw |

Source: [pkg/server/credpool_routes.go](../pkg/server/credpool_routes.go).

## Cloud schedules

Recurring bots, the cloud counterpart of `iterion schedule` —
[scheduling.md](scheduling.md).

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/teams/{id}/schedules` | team member | List the team's cron-scheduled bots |
| `POST` | `/api/teams/{id}/schedules` | team admin | Create a schedule |
| `PATCH` | `/api/teams/{id}/schedules/{sid}` | team admin | Update one |
| `DELETE` | `/api/teams/{id}/schedules/{sid}` | team admin | Remove one |

Source: [pkg/server/schedules_routes.go](../pkg/server/schedules_routes.go).

## Triggers (event-driven runs)

The subscription registry binding (event filter) → (bot launch), gated
on `server_info.triggers_enabled` —
[ADR-046](adr/046-event-driven-runs-trigger-spine.md).

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/v1/triggers` | member | List subscriptions (team-scoped in cloud) |
| `POST` | `/api/v1/triggers` | member | Create one |
| `GET` | `/api/v1/triggers/{id}` | member | Read one |
| `PUT` | `/api/v1/triggers/{id}` | member | Replace one |
| `DELETE` | `/api/v1/triggers/{id}` | member | Remove one |
| `POST` | `/api/v1/triggers/emit` | member | Publish a custom event onto the bus |
| `GET` | `/api/v1/triggers/health` | member | Evaluator + bus health |
| `POST` | `/api/v1/bots/{name}/triggers/from-invocation` | member | Generate a subscription from a bot manifest's `invocations:` block |

Source: [pkg/server/triggers_routes.go](../pkg/server/triggers_routes.go).

## Personal access tokens (PATs)

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/me/tokens` | member | List own PATs (no plaintext) |
| `POST` | `/api/me/tokens` | member | Mint a PAT (`iap_…` shown once) |
| `DELETE` | `/api/me/tokens/{token_id}` | member (owner) or super-admin | Revoke |

Source: [pkg/server/pat_routes.go](../pkg/server/pat_routes.go).

## Memory + knowledge

Spaces are addressed by query params (`?name=`, `?visibility=`, plus
`?bot=` for `visibility=bot` and `?project=` for project/bot scopes).

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/memory/usage` | member | `{used_bytes, quota_bytes}` for one space |
| `GET` | `/api/memory/docs` | member | List documents in a space (optional `?dir=`) |
| `GET` | `/api/memory/doc` | member | Read a document (`?path=`) |
| `PUT` | `/api/memory/doc` | member (super-admin for `visibility=global`) | Write |
| `DELETE` | `/api/memory/doc` | member (super-admin for global) | Delete |
| `GET` | `/api/memory/export` | member | Tarball export of the space |
| `POST` | `/api/memory/import` | member (super-admin for global) | Import a tarball |

Source: [pkg/server/memory_routes.go](../pkg/server/memory_routes.go).
Full reference: [memory-and-knowledge.md](memory-and-knowledge.md).

## Runs surface

Read-only views plus the launch / resume mutations the studio drives.

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/runs` | member (tenant-scoped) | List runs |
| `GET` | `/api/runs/global-active` | member | Active runs across the **local** stores on this machine — a desktop-daemon affordance walking `$HOME/.iterion/**`. In cloud mode it returns `{"runs":[]}` unconditionally: the pod's `$HOME` is shared infrastructure, so walking it could leak across tenants. |
| `POST` | `/api/runs` | member | Launch a workflow |
| `POST` | `/api/runs/preview-cost` | member | Estimate cost before launch |
| `POST` | `/api/runs/uploads` | member | Upload an attachment |
| `GET` | `/api/runs/{id}` | member (run tenant) | Run state |
| `GET` | `/api/runs/{id}/events` | member | Event log |
| `GET` | `/api/runs/{id}/workflow` | member | Workflow source attached to the run |
| `GET` | `/api/runs/{id}/artifacts/{node}` | member | Artifact versions for a node |
| `GET` | `/api/runs/{id}/artifacts/{node}/{version}` | member | One artifact |
| `GET` | `/api/runs/{id}/files` / `…/files/content` / `…/files/diff` | member | Working-tree views |
| `GET` | `/api/runs/{id}/commits` etc. | member | Worktree commit history |
| `GET` | `/api/runs/{id}/attachments/{name}` | member | Download an attachment |
| `GET` | `/api/runs/{id}/attachments/{name}/url` | member | Pre-signed S3 URL |
| `POST` | `/api/runs/{id}/cancel` | member | Cancel a running run |
| `POST` | `/api/runs/{id}/pause` | member | Pause |
| `POST` | `/api/runs/{id}/resume` | member | Resume (re-publishes through the queue) |
| `POST` | `/api/runs/{id}/fork` | member | Fork at a prior turn |
| `POST` | `/api/runs/{id}/merge` | member | Land the run's storage branch on a target branch. For a **repo-targeted** run — whose workspace is gone by the time it returns — the server materialises its own clone of the launch ref, runs the same merge pipeline, and pushes the advanced target back to the forge (never force). The merge is persisted as merged only once the forge has it; a refused push records `merge_status=failed`. |
| `GET` | `/api/runs/{id}/merge/conflicts` | member | Conflicting paths left by a merge attempt |
| `POST` | `/api/runs/{id}/merge/conflicts/resolve` | member | Resolve one conflicting path with supplied content |
| `POST` | `/api/runs/{id}/merge/conflicts/resolve-with-agent` | member | Delegate one conflict to the resolver agent (`ITERION_CONFLICT_RESOLVER_MODEL`) |
| `POST` | `/api/runs/{id}/merge/conflicts/finalize` | member | Commit the resolved merge and continue the pipeline |
| `POST` | `/api/runs/{id}/merge/conflicts/abort` | member | Abandon the merge; the storage branch is preserved |
| `POST` | `/api/runs/{id}/commit-and-finalize` | member | Commit pending work and finalise |
| `POST` | `/api/runs/{id}/rename` | member | Rename a run |
| `GET` | `/api/runs/{id}/children` | member | Child (subbot) runs |
| `GET` | `/api/runs/{id}/review/scope` / `…/review/diff` | member | Human-gate review scope and its diff ([review-scope.md](review-scope.md)) |
| `GET` | `/api/runs/{id}/session-board` | member | Session-board widgets ([session-board.md](session-board.md)) |
| `GET` | `/api/runs/{id}/interactions/pending` | member | Unanswered `ask_user_async` questions |
| `GET`/`DELETE` | `/api/runs/{id}/queue-messages`, `…/queue-message/{msgID}` | member | Pending steering messages for the run |
| `GET` | `/api/runs/{id}/log` | member | Streamed run log |
| `GET` | `/api/runs/{id}/preview` | member | Preview proxy (SSRF-guarded) |
| `GET` | `/api/ws/runs/{id}/shell` | member (via `?t=`) | Post-mortem PTY in a preserved worktree ([post-mortem-shell.md](post-mortem-shell.md)) |
| `GET` | `/api/ws/runs/{id}` | member (via `?t=`) | Live run-console WebSocket |
| `GET` | `/api/v1/runs/stats` | member | Rolling stats (for the studio) |
| `GET` | `/api/v1/limits/cost` | member | Cost-cap status |
| `POST` | `/api/v1/limits/cost/override` | member | Grant or revoke the day's cost-cap override. **Any authenticated member** — the handler performs no role check and records the audit actor as `operator`. |

Source: [pkg/server/runs.go](../pkg/server/runs.go).

## Super-admin (organisations + users + DLQ + audit)

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/admin/orgs` | super-admin | List every org |
| `POST` | `/api/admin/orgs` | super-admin | Create org |
| `GET` | `/api/admin/orgs/{id}` | super-admin | Read |
| `PATCH` | `/api/admin/orgs/{id}` | super-admin | Update name / slug / quotas |
| `DELETE` | `/api/admin/orgs/{id}` | super-admin | Schedule org deletion (reversible until it runs) |
| `POST` | `/api/admin/orgs/{id}/restore` | super-admin | Cancel a scheduled deletion |
| `POST` | `/api/admin/orgs/{id}/status` | super-admin | Suspend / read-only / activate |
| `GET` | `/api/admin/orgs/{id}/usage` | super-admin | Usage snapshot |
| `GET` | `/api/admin/orgs/{id}/teams` | super-admin | List the org's teams |
| `GET` | `/api/admin/users` | super-admin | List users (`?offset=&limit=` pagination; limit default 50, max 200) |
| `PATCH` | `/api/admin/users/{id}` | super-admin | Status / super-admin flag |
| `POST` | `/api/admin/users/{id}/reset-password` | super-admin | Force a user's password reset |
| `GET` | `/api/admin/audit` | super-admin | Platform audit log (filters: `action`, `actor`, `from`, `to`, `offset`, `limit`) |
| `GET` | `/api/admin/dlq` | super-admin | List parked messages |
| `GET` | `/api/admin/dlq/{seq}` | super-admin | Peek payload |
| `POST` | `/api/admin/dlq/{seq}/replay` | super-admin | Re-publish onto the live subject |
| `DELETE` | `/api/admin/dlq/{seq}` | super-admin | Discard |
| `GET` | `/api/admin/bots` | super-admin | List platform bot overrides (metadata + content digest) |
| `GET` | `/api/admin/bots/{slug}` | super-admin | One override incl. its file map |
| `PUT` | `/api/admin/bots/{slug}` | super-admin | Push/replace an override (`{files, version?}`; compiled before persisting; 413 over the body cap) |
| `DELETE` | `/api/admin/bots/{slug}` | super-admin | Remove the override (baked catalog serves again) |
| `PUT/DELETE` | `/api/admin/bots/{slug}/files/{path}` | super-admin | Single-file edit of an override |
| `POST` | `/api/admin/bots/{slug}/fork` | super-admin | Seed the override from the baked bundle (`{from}`) |
| `GET/PUT` | `/api/admin/settings/bot-roles` | super-admin | Webhook role→bot bindings (merge semantics; `null` clears a field) |
| `GET/PUT` | `/api/admin/settings/sandbox` | super-admin | `sandbox: auto` fallback image override |

Sources: [pkg/server/admin_orgs_routes.go](../pkg/server/admin_orgs_routes.go),
[pkg/server/admin_bots_routes.go](../pkg/server/admin_bots_routes.go),
[pkg/server/platform_settings.go](../pkg/server/platform_settings.go),
[pkg/server/queue_sweeper.go](../pkg/server/queue_sweeper.go).

## Server info + health

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/server/info` | public | Mode, version, `auth_required`, `email_enabled`, per-feature enablement flags, upload limits |
| `GET` | `/healthz` | public | Liveness — HTTP listener up |
| `GET` | `/readyz` | public | Readiness — Mongo + NATS + S3 reachable under 1s deadline |
| `GET` | `/metrics` | public on the metrics port (ClusterIP-only by design) | Prometheus scrape |

## Non-obvious JSON shapes

### `POST /api/teams/{id}/webhooks` — create response (token-once)

```json
{
  "config": {
    "id": "8e2…",
    "tenant_id": "team_acme",
    "name": "GitLab MR review",
    "provider": "gitlab",
    "sign_mode": "",
    "enabled": true,
    "token_last4": "Vp3a",
    "fingerprint": "sha256:…",
    "bot_ids": ["review-pr"],
    "wildcard_bots": false,
    "project_allowlist": ["acme/*"],
    "event_allowlist": [],
    "rate_limit": { "rate": 1.0, "burst": 10 },
    "monthly_call_limit": 0,
    "launch_vars": {},
    "key_overrides": {},
    "created_by": "user_…",
    "created_at": "2026-06-11T10:11:12Z",
    "updated_at": "2026-06-11T10:11:12Z"
  },
  "token": "iwh_…"
}
```

The `token` field is the **only** way to recover the plaintext. The
same shape comes back from the rotate endpoint.

### Launch-denial envelope

Every gate refusal — REST launch, resume, webhook publication — uses
the same shape ([pkg/server/launch_gate.go](../pkg/server/launch_gate.go)):

```jsonc
{
  "error":    "monthly_run_quota_exceeded",   // stable token
  "detail":   "monthly run quota (1000) exhausted",
  "reset_at": "2026-07-01T00:00:00Z"          // monthly quotas
}
```

Plus the header `Retry-After: <seconds>` on `concurrency_cap_exceeded`
and `launch_rate_limited`. Token list and HTTP semantics in
[quotas-and-limits.md](quotas-and-limits.md).

### `GET /api/orgs/{id}/usage` (also `/api/admin/orgs/{id}/usage`)

See [quotas-and-limits.md → Reading usage](quotas-and-limits.md#reading-usage)
for the full `orgUsageView` schema. Same shape on both routes — the
admin endpoint is super-admin only, the member endpoint is any org member.

### `POST /api/me/tokens` — create PAT

Request:

```json
{ "name": "github-actions", "team_id": "team_…", "expires_in_days": 90 }
```

Response (plaintext shown once):

```json
{
  "pat":   { "id": "…", "name": "github-actions", "token_last4": "Q9k2", "expires_at": "…", … },
  "token": "iap_…"
}
```

`expires_in_days` is clamped down to `ITERION_PAT_MAX_TTL` when the
platform sets one. `team_id` is optional; without it the PAT inherits
the user's default team and re-checks membership at every use.
