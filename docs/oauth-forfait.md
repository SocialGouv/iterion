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

At launch the cloud publisher resolves OAuth credentials **user-primary
with an org fallback**, per kind:

1. **The run owner's personal forfait** wins. An interactive run launched
   from the studio/CLI by an authenticated developer carries that
   developer's `OwnerID`, so their connected subscription is used.
2. **The org/team forfait** is used as a fallback for any kind the owner
   hasn't connected. Automated runs (webhook / dispatcher / scheduler)
   carry a *synthetic* owner (`webhook:<id>`, …) with no personal
   forfait, so they fall back to the org credential when one is set.
3. Otherwise the resolution continues down the tier chain: the
   **credential pool** (a contributor's lent subscription), then the
   DB-backed **platform tier**, then the pod env.

Two corrections to the mental model that "the forfait is used unless
nothing else is there":

- **BYOK is resolved FIRST, not last.** It is step 1 of the chain
  ([publisher.go:359](../pkg/server/cloudpublisher/publisher.go#L359)), and
  on the same wire it also outranks the forfait at consumption time:
  `anthropicCredEnvForCLI` tests `ANTHROPIC_API_KEY` *before* the forfait's
  `CLAUDE_CONFIG_DIR`
  ([claude_code_creds.go:331-337](../pkg/backend/delegate/claude_code_creds.go)).
  So a team Anthropic key **shadows** a connected forfait, and deleting the
  key is the only way to prefer the subscription — see the gotchas in
  [cloud-llm-credentials.md](cloud-llm-credentials.md).
- **A forfait the provider is currently refusing is skipped**, so steps 1–2
  can produce nothing for a tenant that *holds* one, and a later tier
  serves instead
  ([the fallback chain](cloud-llm-credentials.md#the-tiers-are-a-fallback-chain-not-a-fixed-first-choice)).
  If no later tier fills that wire the skipped forfait is restored, so the
  run parks with a durable usage-window retry rather than failing
  unauthenticated.

Only the **`claude_code`** backend consumes the forfait (it *is* the
Claude Code CLI — the ToS-clean path; the runner materialises the sealed
blob into a temp dir and points the CLI at it via `CLAUDE_CONFIG_DIR`).
`claw` nodes (judges/reviewers, in-process) are **not** wired for
Anthropic OAuth and keep using API keys.

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
