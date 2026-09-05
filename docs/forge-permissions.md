# Forge permissions & identity — who a bot acts as

This is the model people most often get wrong: **the iterion user who launches
a bot is NOT who the bot acts as on the forge.** Two completely separate planes
are at play. This page documents both and how they meet (they barely do).

## TL;DR

- **iterion identity** (your role `viewer | member | admin | owner` in an
  org/team) governs what you can do **inside iterion** — log in, launch runs,
  manage settings/integrations/keys. GitHub team-gating (SSO grants) feeds
  *this* plane: being in `org/team` gives you an iterion seat.
- **forge credential** (the token of the forge *connection*, scoped to the
  **team**, not to you) is what actually **acts on the repo** when a bot opens a
  PR / pushes / comments.
- A bot's repo rights = **the connection identity's rights**, the *same* for
  every user of the team, regardless of their iterion role **or their own GitHub
  rights**. Your personal GitHub team/repo permissions do **not** flow through.

So: *“users in a GitHub team that has repo rights → do they inherit those rights
to make PRs?”* → **No.** Their GitHub membership only gated their **login** to
the iterion org. The PR is opened with the **connection's** token.

## The two planes

### Plane 1 — iterion RBAC (what you can do in iterion)

Per-team role, ordered `viewer(1) < member(2) < admin(3) < owner(4)`
([pkg/identity/types.go](../pkg/identity/types.go)). Gates iterion operations:
launch runs (member+), manage integrations / keys / members / SSO
(`canManageTeam` = admin+, [pkg/server/auth_routes.go](../pkg/server/auth_routes.go)).
It does **not** choose or change any forge credential.

GitHub SSO team-gating populates this plane only: a verified+enabled grant
`(github_org, team_slug) → member` lets matching users **log in and join** the
iterion org. The GitHub OAuth **login** token is used once to read the user's
email + org/team memberships and is then **discarded — never stored, never
reused** for forge writes ([pkg/auth/oidc/github.go](../pkg/auth/oidc/github.go),
[pkg/auth/oidc_service.go](../pkg/auth/oidc_service.go)).

### Plane 2 — the forge connection (what a bot does on the repo)

A `forge.Connection` ([pkg/forge/types.go](../pkg/forge/types.go)) is a
**team-scoped service credential**. Its sealed token is opened
(`AdminTokenFor`, [pkg/forge/connection_sealer.go](../pkg/forge/connection_sealer.go))
and re-packaged once as a managed `forge_token` generic secret
(`ensureManagedSecret`, [pkg/forge/orchestrator.go](../pkg/forge/orchestrator.go)),
injected into every bot run at `/run/iterion/secrets/forge_token`. The bot's
`gh`/`glab`/git calls authenticate with **that token** — i.e. the **connection's
identity**. (The managed secret holds the *same token value* as the connection;
the re-seal only changes the encryption envelope, not the identity.)

## Who the connection acts as — by kind

| Connection kind | Token | Acts on the forge as | Repo rights = |
|---|---|---|---|
| `oauth_app` | OAuth user access token (refreshable) | **the user who authorized** the OAuth app | that user's repo permissions |
| `github_app` | Installation token (minted ~1h, GitHub only) | **the GitHub App** (a bot identity) | the App's manifest permissions (narrow, least-privilege) |
| `pat` | Personal access token (static) | **the PAT owner** (a person or a service account) | the PAT's scopes |

The launching user's iterion role is **irrelevant** to this: a `member` and an
`admin` launching the same bot both write via the same connection token.
`Provision` records the launching user only as `ActorID` / `created_by` for the
audit trail ([pkg/forge/orchestrator.go](../pkg/forge/orchestrator.go)) — never
for authentication.

## What this means for your setup (and the super-admin question)

If the GitHub connection is `oauth_app` authorized by an **org-admin** (e.g. the
super-admin's own account), then **every** bot run that any team user launches
opens PRs / pushes **as that org-admin**, with that account's full GitHub
powers — a shared, broad service credential. Convenient, but high blast radius:
a plain iterion `member` who can launch a bot effectively wields the connecting
admin's GitHub rights on the repos that account can reach.

Being a GitHub **org-admin yourself does not** grant your *bots* anything extra
beyond what the connection token can do; it only mattered because the org-admin
is typically who authorized the connection.

### Least privilege

Prefer narrowing the *connection*, not the user:

- **GitHub App** (`github_app`) — the bot acts as the App with exactly the
  permissions in its manifest (`contents:write`, `pull_requests:write`,
  `issues:write` for posting the PR/MR back-link on the source issue,
  `metadata:read`, `repository_hooks:write` for the per-repo inbound webhook),
  scoped to the repos the App is installed on. It deliberately does **not**
  request `administration` (repo deletion/settings/teams/branch-protection) —
  that is over-privileged, and per GitHub docs webhooks require
  `repository_hooks`, not `administration`. The right answer for production: bots get only
  what they need, and PRs are authored by a clearly-non-human bot identity.
  **Self-service** (no platform App, no manual registration): Integrations →
  "+ Register an OAuth app" → github → **"Create a GitHub App"** (iterion builds
  the scoped App via manifest and captures its private key), then the **"Install"**
  button on that app → install on the org/repos → a `github_app` connection. The
  legacy platform App (`ITERION_FORGE_GITHUB_APP_*`) still works as a fallback.
- **A dedicated service-account PAT** (`pat`) with a minimal scope, instead of a
  human org-admin's `oauth_app`.

## GitHub vs GitLab vs Forgejo

The identity model is **identical** across providers — connection token = acting
identity — via the common `forge.Admin` abstraction (ADR-049,
[docs/adr/049-forge-as-interchangeable-substrate.md](adr/049-forge-as-interchangeable-substrate.md)).

| | GitHub | GitLab | Forgejo/Gitea |
|---|---|---|---|
| `oauth_app` (acts as the authorizing user) | ✅ | ✅ | ✅ |
| `pat` (acts as the token owner) | ✅ | ✅ | ✅ |
| `github_app` (acts as a scoped App/bot) | ✅ | ✅ (best path on a self-hosted instance: a project/group access token used as a `pat`) | — (no App concept; same — use a bot-account PAT) |
| auth header | `Bearer` | `Bearer` | `token` (Gitea scheme) |

So the **only** real difference is that the narrow “bot identity” path is a
first-class **GitHub App** on GitHub, whereas on GitLab/Forgejo you approximate
least-privilege with a **scoped service-account / project token** connected as a
`pat`. Clients: [pkg/forge/gitlab/client.go](../pkg/forge/gitlab/client.go),
[pkg/forge/forgejo/client.go](../pkg/forge/forgejo/client.go).

## The face on the connection — the iterion-bot avatar

Whatever the kind, the operator sees the connection's account on every comment
it posts. iterion gives that account the mascot of the official `iterion-bot`
GitHub account wherever the forge lets it: **automatically** for a GitLab
account the forge flags as a bot (a group/project access token's bot user, a
service account — `PUT /user/avatar`, GitLab ≥ 17.0), **on request** for a
dedicated account it cannot flag (Forgejo; a hand-made `iterion-bot` user),
**never** for an OAuth connection (it authenticates as the person who
authorized it), and **by hand** for a GitHub App (no logo API — the studio
hands over the file and the settings page). Runbook, endpoint and escape
hatch: [brand.md](brand.md).

## Audit — correlating a forge action back to a person

The forge action is authored by the connection identity, but **who triggered
it** is recorded: `RepoIntegration.CreatedBy` / the run's `ActorID` and the
tenant audit log ([pkg/audit](../pkg/audit)). So “a PR opened by the connection
bot” is traceable to the iterion user (and run) that launched it, even though
the GitHub author is the connection.

## Shared branded App vs per-org App

iterion resolves a GitHub App **per connection** ([`Connection.OAuthAppID`](../pkg/forge/types.go)),
so an instance can offer both shapes at once. Which one an operator wants is a
product decision, not a technical constraint:

| | Branding | Least privilege | Install friction |
|---|---|---|---|
| **Shared branded App** (one public "iterion" App, its own name + logo) | one-click, recognisable | every adopter grants the App's full permission set | lowest |
| **Per-org App** (manifest-created, today's default) | none — a generated `iterion-forge-<hash>` name | each org grants only what it wants | an extra creation step |

The tradeoff is narrower than it looks, and one common worry is unfounded:

- **A public App can still be installed on selected repositories.** Repository
  scope is chosen by whoever *installs*, not by the App. "Public" only means
  anyone may install it.
- **Permissions, however, are per-App, not per-installation.** An adopter takes
  the whole requested set or declines. So a single branded App carrying
  `administration` + `workflows` + `packages` forces a team that only wants PR
  review to grant repo-creation and CI-rewrite rights too. Splitting into
  capability tiers (a baseline App and a delivery App) is the honest way to
  keep one-click onboarding without over-granting.

### Enabling the shared App — the non-negotiable part

Configure `ITERION_FORGE_GITHUB_APP_ID` / `_PRIVATE_KEY` / `_SLUG`, **and**
`_CLIENT_ID` / `_CLIENT_SECRET`, and enable *"Request user authorization
(OAuth) during installation"* on the App.

The client credentials are not optional hardening. A shared App's private key
can mint a token for **any** installation of that App, and `installation_id`
arrives as an enumerable integer on a public callback URL — so without
ownership verification an attacker substitutes a victim org's id and obtains a
connection that mints tokens against their repositories. The install callback
therefore **refuses** a shared-App install it cannot verify
([`handleForgeGitHubAppCallback`](../pkg/server/forge_connect_routes.go)); a
per-org App is key-scoped and cannot reach another tenant's installation, which
is why it skips the check.

## Possible evolutions (not built today)

- **Per-user forge identity pass-through** — open each user's bot PRs as *their
  own* GitHub identity (store + refresh a per-user forge token, distinct from
  the login token). Makes authorship match the human, at the cost of holding a
  forge token per user and reconciling it with the connection model. Not
  currently done by design (login tokens are auth-only).
- **Per-bot connection binding** — bind specific bots to specific connections so
  a low-trust bot uses a narrow App/PAT while a release bot uses a broader one.
- **Connection-level role mapping** — gate *which* iterion roles may launch bots
  that use a given (powerful) connection.

When any of these ship, update this page.
