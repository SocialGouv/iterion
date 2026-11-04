# OAuth forfait (consumer subscription) for cloud runs

Iterion can drive bot runs on a **consumer subscription** — a Claude
Pro/Max "forfait" (the OAuth token `claude login` stores in
`~/.claude/.credentials.json`) or a ChatGPT Plus/Pro forfait (the Codex
`auth.json` in ChatGPT mode) — instead of metered API keys. This is meant
for **developing and testing bots**; what that means contractually, and
where the operator's own deployment stands, is below.

> [!WARNING]
> **A forfait is an individual licence, for personal and development use.**
> Both subscriptions are sold under the vendor's *consumer* terms, to one
> person. This tree paraphrases Anthropic's position as "not for fully
> automated production": that wording is **iterion's paraphrase, not a
> quotation** — check it against the Consumer Terms current at the time you
> read this before relying on it ([ADR-085](adr/085-pi-as-execution-backend.md)
> records one earlier reading of those terms that was later reversed).
> Running a whole org's automated production workload on one shared
> subscription is outside the licence as this tree reads it; use **API
> keys** (BYOK, platform tier, or a gateway) for production automation.
> The team-, org- and platform-scoped credentials below are operator
> conveniences, not production credentials, and so is a member's forfait
> lent through the [credential pool](credential-pool.md). The studio's team
> and platform OAuth pages carry a standing warning banner (plus a
> persistent toast at connect time); the org tier is connected from the CLI
> (`iterion remote orgs oauth set <kind>`) and carries no warning.

## What a forfait is, and is not

- **Who holds it.** A forfait is bound to one person's account with the
  vendor. Connecting it to iterion lends that person's licence to the runs
  it serves; iterion seals and refreshes the blob but does not change whose
  licence it is. Users developing their own bots on their own subscription
  are the use case this page describes.
- **What the code refuses, and what it does not.** iterion refuses nothing
  on `claude_code` and `codex`: they spawn the vendor's own CLI, which
  draws on the plan exactly as it would on a laptop. On `claw` and `pi` the
  Claude subscription bills Anthropic's separate extra-usage balance and
  iterion warns per node; `ITERION_FORBID_SUBSCRIPTION_OAUTH=1` turns that
  warning into a refusal — **for those two backends only**
  ([pkg/secrets/credentials.go](../pkg/secrets/credentials.go),
  [pkg/backend/detect](../pkg/backend/detect/detect.go)). No runtime
  check exists that distinguishes a dev run from a production run; the
  distinction is the operator's.
- **The audit trail.** The publisher logs one line per run,
  `cloudpublisher: credentials GRANTED for run=… — <tier>(oauth:<kind> fp=…)`,
  whose tier is `oauth-forfait` (user or team), `org`, `pool` or
  `platform`; the run records the same `credential_tiers`. The grep
  `grep -E 'credentials GRANTED for run=.*\(oauth:(claude_code|codex) '`
  catches every forfait-funded run whatever tier served it
  ([cloud-admin.md §9](cloud-admin.md#9-audit-log)) — that is the evidence
  of which runs a subscription actually paid for. A token in the runner's
  own env (`iterion-forfait`, or a `CLAUDE_CODE_OAUTH_TOKEN` key in
  `iterion-llm`) never reaches that log: it is proved absent by the
  Secrets' key names, not by a grep.

### Where the SocialGouv deployment stands

The instance the SocialGouv team operates is the deployment this tree is
written against, so its posture is stated here rather than implied:

- **Claude**: production runs on **API keys** — the platform tier or a
  tenant's own key. The pod-level Claude forfait (`secrets.forfait.enabled`,
  the `iterion-forfait` Secret) is retired there, as it is in the chart's
  own `values-prod.yaml`. That flag does not cover every channel: the
  `iterion-llm` Secret, mounted on the runner independently of it, is to
  carry API keys and no `CLAUDE_CODE_OAUTH_TOKEN`, and a platform-tier
  Claude forfait blob sealed in the database is outside the chart
  altogether — `iterion remote admin llm oauth` lists it and `delete
  claude_code` removes it. The posture is checked on those channels, not
  inferred from the flag.
- **Codex / ChatGPT**: the consumer OAuth forfait **stays configured**
  (`ITERION_OPENAI_USE_OAUTH=1`, `ITERION_OAUTH_FORFAIT_CODEX_CLIENT_ID`),
  under review by the team's legal counsel against OpenAI's consumer terms.
  That choice is to be declared in the deployment's architecture file and
  privacy policy (OpenAI as a provider under its consumer terms, not an
  Art. 28 GDPR processor) — those texts are the operator's, outside this
  tree. Nothing in iterion asserts
  that OpenAI's terms permit it: the code applies no refusal to the
  ChatGPT forfait on `claw` or `codex`, and that absence is not a reading
  of the terms.
- **Users' personal forfaits** remain allowed for developing and testing
  their bots, which is the licence's own use.

### Keeping a deployment on the strict BYOK path

There is **no global off switch** for forfaits — the client id is
defaulted, the connect routes are always served. The levers an operator
actually has, each covering one delivery channel, are tabulated in
[cloud-admin.md §7](cloud-admin.md#7-byok--oauth-forfait--operator-perspective):
`secrets.forfait.enabled: false` (no `iterion-forfait` Secret — and no
`CLAUDE_CODE_OAUTH_TOKEN` key in `iterion-llm`, which the runner mounts
regardless), no platform-tier blob (`iterion remote admin llm oauth`),
`ITERION_FORBID_SUBSCRIPTION_OAUTH=1`
(claw/pi only), `ITERION_OPENAI_USE_OAUTH=0` (claw's ChatGPT path), the
credential pool off (`iterion remote pool policy --enabled=false`) or kept
to a policy for what members lend to other people's runs. What users
connect for themselves is a policy, not a flag.

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
| `ITERION_OAUTH_FORFAIT_CODEX_CLIENT_ID` | Codex OAuth client id — optional override; the refresh worker defaults to the id carried by the uploaded `auth.json`. |
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
