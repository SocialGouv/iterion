# OAuth forfait (Claude subscription) for cloud runs

Iterion can drive bot runs on a **Claude subscription** (the OAuth
"forfait" token, like the one `claude login` stores in
`~/.claude/.credentials.json`) instead of metered API keys. This is meant
for **developing and testing bots** — see the ToS note below.

> [!WARNING]
> **For developing and testing bots only — not for fully automated
> production.** A Claude subscription is an **individual licence**
> (Anthropic Consumer Terms). Running a whole org's automated production
> workload on one shared subscription is outside that licence; use **API
> keys** (BYOK) for production automation. The org-scoped credential
> below is an operator convenience, surfaced with a warning at connect
> time, not a production credential.

## How a run resolves a forfait

The publisher resolves BYOK keys and OAuth subscriptions per provider wire,
with personal, team, organization, pool and platform rules. A connected API
key can take precedence over a subscription. See the maintained
[resolution guide](cloud-llm-credentials.md) and
[pool conditions](credential-pool.md#resolution-order-where-the-pool-sits).

Within an OAuth owner/kind, rank `0` is the primary and higher ranks are tried
in order when a previous provider window is closed. Interactive runs use the
authenticated launcher's personal connections; automated runs carry their
own synthetic owner and do not inherit the operator's personal subscription.
The Studio selector addresses the chosen rank for connect, rename, refresh
and disconnect, and can add another fallback without replacing the primary.

Both the Claude Code CLI and Claw's Anthropic path can consume a Claude
subscription. The runner supplies the resolved credential to the selected
backend. The presence of an OAuth record alone is not proof it paid for a
particular run: inspect the run's credential tiers and publisher grant.

When profile scope is present, connecting also verifies the provider account
and displays its email. Multiple connections of that verified account share
one meter; Studio flags duplicate accounts within the visible owner. See
[verified accounts](cloud-llm-credentials.md#verified-accounts-share-one-provider-meter)
for lookup failures and rollout requirements.

## Connecting — browser flow (no `claude login`, no file paste)

Connecting is a 100%-in-browser PKCE authorization-code flow, so a cloud
operator never has to run `claude login` in a pod or paste a
credentials.json file:

1. Click **Connect Claude** (personal: *Settings → OAuth*; org: *Team →
   Integrations*, admins only).
2. A new tab opens on `claude.ai`. Sign in and authorize.
3. Anthropic's callback page shows a `code#state` string — copy it and
   paste it back into the studio. The server exchanges it for tokens and
   seals them.

The single copy/paste step is unavoidable: the public Claude Code OAuth
client only permits Anthropic's own callback page or a `localhost`
loopback as redirect targets — a remote studio is neither, so it can't
receive a silent redirect. A **raw paste of credentials.json / auth.json**
remains available under *Advanced* (and is the only path for Codex).

## Token refresh

Access tokens expire (hours). A background **refresh worker** rotates
every connected forfait (personal *and* org) ~30 min before expiry, so
long-running and automated runs never read a stale token. A manual
**Refresh tokens** button is also available per connection.

Claude refreshes verify the identity of the returned bearer before associating
it with a shared account meter. If the profile is unavailable, the refreshed
tokens remain usable but the UI reports that account identity is unverified;
a later successful lookup restores the correlation. A reconnect replacing a
credential also invalidates refresh work based on the previous snapshot.

## Configuration

Nothing needs to be provisioned for Anthropic: the client id defaults to
the **public** Claude Code OAuth client (`9d1c250a-…`, a PKCE public
client with no secret), so the browser flow works out of the box. All
values are overridable per deployment:

| Env var | Purpose |
| --- | --- |
| `ITERION_OAUTH_FORFAIT_ANTHROPIC_CLIENT_ID` | Claude Code OAuth client id. **Defaults** to the public client; override only if Anthropic rotates it. |
| `ITERION_OAUTH_FORFAIT_CODEX_CLIENT_ID` | Codex OAuth client id (refresh). |
| `ITERION_OAUTH_FORFAIT_ANTHROPIC_AUTHORIZE_URL` | Override the authorize endpoint (default `https://claude.ai/oauth/authorize`). |
| `ITERION_OAUTH_FORFAIT_ANTHROPIC_REDIRECT_URI` | Override the headless redirect (default `https://platform.claude.com/oauth/code/callback`). |
| `ITERION_OAUTH_FORFAIT_ANTHROPIC_SCOPES` | Override the requested scopes. |
| `ITERION_OAUTH_FORFAIT_ANTHROPIC_TOKEN_URL` | Override the token endpoint (default `https://console.anthropic.com/v1/oauth/token`). Used by BOTH the auth-code exchange and the server-side refresh. |
| `ITERION_OAUTH_FORFAIT_ANTHROPIC_PROFILE_URL` | Override the account-lookup endpoint (default `https://api.anthropic.com/api/oauth/profile`). **Move it with the token endpoint**: this leg sends the bearer outbound on every connect and every refresh, so a deployment that re-points the token URL and leaves this one ships tokens its own gateway minted to Anthropic. |
| `ITERION_OAUTH_FORFAIT_CODEX_TOKEN_URL` | Override the Codex token endpoint (default `https://auth.openai.com/oauth/token`). |

Re-pointing an OEM-repackaged CLI takes the whole family: the authorize
URL, the redirect URI and the scopes drive the browser connect, and the
token URL drives what happens after it — a deployment that moves the
first three and not the fourth connects against one host and refreshes
against another.

The browser connect flow is available whenever the OAuth store is wired
(cloud mode) — the client id is defaulted, so no extra config is needed.

### Storage & isolation

A forfait is an `OAuthRecord` sealed at rest (AES-GCM, AAD-bound to its
owner+kind); the plaintext is only ever materialised inside the runner.
The org credential is stored under a synthetic owner key
(`org:<tenantID>`) — the same machinery as a personal record, so sealing,
refresh and expiry tracking are identical. Org endpoints
(`/api/teams/{id}/oauth/...`) are **team-admin only**; listing is
viewer-visible.

> **Maintenance note.** This rides the *public, undocumented* Claude Code
> OAuth client. If Anthropic rotates the client id, endpoints or scopes,
> re-capture the values from a fresh `claude login` and set them via the
> env overrides above.
