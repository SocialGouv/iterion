# Iterion cloud — user guide

You signed up for an iterion workspace (or got an invite). This
guide covers the user-facing flows: signing in, switching teams,
registering API keys, connecting your Claude Code / Codex
subscriptions, and inviting teammates.

For the operator-facing flows (chart, secrets, SSO config), see
[cloud-admin.md](cloud-admin.md).

## 1. Signing in

The login page surfaces every auth method your operator has
enabled. The basic two:

- **Email + password.** Use the credentials your team admin sent
  you. Argon2id at rest; the access token lasts 15 minutes and is
  silently rotated via the refresh token when it expires. Each
  rotation mints a fresh 30-day refresh session, so continuous use
  keeps you signed in while an idle session lapses 30 days after its
  last rotation.
- **Single sign-on.** One button per configured provider
  (Google, GitHub, your company's SSO). Clicking it redirects to
  the IdP, then back to iterion with a session cookie set.

If you have an invitation token (in your email or chat), paste it
into the "Create account" form along with your email + password.
The token binds your account to the inviting team automatically.

## 2. Teams (tenants)

Every workspace inside iterion is a "team" — a tenant boundary.
Runs, API keys, OAuth blobs, audit entries are partitioned by team:
team A simply cannot see team B's data, even if you're an admin in
both.

The chip in the top-right of the studio surfaces the active team;
clicking it lets you:

- Switch teams. The server re-bakes a fresh access JWT bound to the
  new `team_id` (the resource-tenant claim). All studio surfaces (run
  list, files panel, settings) follow.
- Open the team admin page (`/teams/<id>`) where you can:
  - invite teammates with a role (viewer / member / admin / owner)
    and copy the one-time invitation token to send via email or
    chat.
  - change a member's role.
  - manage **team-scoped API keys** (see §3).
- Open your account settings (`/account`).

You always have a "personal team" auto-created at sign-up; no
collaborators land there unless you invite them. Move shared work
to a real team.

## 3. API keys (BYOK — bring your own key)

iterion runs your workflows against the LLM providers you choose,
billed to **your** API account. There are two scopes:

- **Team-scoped keys** (`/teams/<id>` → API keys tab). Visible to
  every team member; admins/owners manage them. Use these for
  shared infrastructure: a team OpenAI key everyone uses by
  default.
- **User-scoped keys** (`/account` → API keys). Visible only to
  you. Use these for personal experimentation that should not
  charge a teammate's credit card.

When you launch a workflow, iterion picks a key for each provider
the workflow calls in this order:

1. an explicit `key_overrides[provider]` you pin at launch time;
2. your user-scoped default for that provider;
3. any user-scoped key (first match);
4. the team-scoped default;
5. any team-scoped key (first match);
6. the operator's env-var fallback (deployment-wide).

Mark a key `is_default` on creation to skip steps 3 and 5.

The key value itself is **write-only** — once submitted, iterion
seals it with the deployment master key and never returns the
plaintext. The UI only shows `last4` + a fingerprint so you can
distinguish two keys for the same provider.

Supported providers: Anthropic, OpenAI, AWS Bedrock, GCP Vertex,
Azure (Foundry), OpenRouter, xAI, z.ai.

## 4. OAuth subscriptions (Claude Pro/Max + ChatGPT)

If you already have a paid Claude Pro/Max or ChatGPT subscription,
you can let iterion drive the official CLIs (Claude Code / Codex)
on your behalf — they bill against your subscription, not your
team's API key.

> **Important — Terms of Service.** This path is **only** valid for
> the official CLI surface. iterion's in-process Anthropic SDK
> (`claw` backend) refuses to consume the forfait blob and returns
> a clear error if a workflow tries — see operator guide §7. If
> your workflows use the `claw` backend for an Anthropic model,
> you need a real API key (BYOK), not the forfait.

To connect your forfait:

1. On a machine where the official CLI works, sign in once
   (`claude login`, `codex login`).
2. Locate the credentials file the CLI writes:
   - Claude Code: `~/.claude/.credentials.json`
   - Codex: `~/.codex/auth.json`
3. Open `/account` → OAuth subscriptions → Connect.
4. Paste the file contents into the textarea and submit.

iterion seals the blob at rest. When you launch a workflow that
uses the `claude_code` or `codex` backend, the runner materialises
the file in a per-run `tmpfs` directory (mode 0700, file 0600),
sets `CLAUDE_CONFIG_DIR` / `CODEX_HOME` on the spawned CLI, and
removes the directory the moment the run ends.

Refresh: the **Refresh tokens** button rotates your stored
`access_token` against the provider's OAuth endpoint without
re-pasting. iterion runs this automatically in the background for
records that expire within 24 hours, so day-to-day you should
never need to click it.

If iterion's deployment doesn't have the corresponding OAuth
client configured (see operator guide §7), refresh fails — paste
a fresh `credentials.json` from your local CLI to recover.

## 5. Invitations

To invite someone to your team:

1. Open `/teams/<id>` → Members + invitations.
2. Enter their email + role.
3. Copy the **invitation token** the server returns (it appears
   ONCE — iterion stores only its hash).
4. Send it to them however you want (email / chat / SMS).

They paste the token into the "Create account" form (or, if they
already have an iterion account, into `/invitations/accept?token=<token>`
on a logged-in session). When the deployment has SMTP configured, the
invitation is also emailed automatically with that accept link.

Invitations expire after 7 days.

## 6. Personal access tokens (PATs)

For CI jobs, SDKs and curl — anywhere the 15-minute browser session is
impractical. `/account` → Tokens → create: the `iap_…` value is shown
**once**. Use it as `Authorization: Bearer iap_…`. A PAT authenticates
as *you* (your role, your team — optionally pinned to one team at
creation) and dies instantly on revoke, on account disable, or when
you leave the pinned team. The operator may cap token lifetimes
(`ITERION_PAT_MAX_TTL`). Details: [secrets-reference.md](secrets-reference.md).

## 7. Password & account security

- **Forgot password** — when the deployment has email enabled, the
  login page offers a reset link (valid 60 minutes, single use; every
  other session is revoked on completion). Without email, ask an
  admin to force a password rotation on your account.
- **Change password** — `/account` → Profile: rotating your password
  signs out every *other* session; yours continues seamlessly.
- **Sign out everywhere** — same page; kills every refresh session
  (active browser tabs lose access within ≤15 minutes).

## 8. My org's usage

`/teams/<id>` → Usage shows the month's consumption against your org's
limits: runs vs quota, metered LLM cost vs cap, live concurrency,
webhook calls, memory bytes, key/secret/webhook counts. The same data
is at `GET /api/orgs/{id}/usage`. Denial semantics when a cap is hit:
[quotas-and-limits.md](quotas-and-limits.md).

## 9. Common errors

| What you see | What's going on |
|---|---|
| "no API key configured for provider X" at run launch | Neither you nor your team has registered a key for that provider, and the operator hasn't set the env-var fallback. Add a key in `/account` → API keys or ask a team admin to add one. |
| "refusing to spend a subscription OAuth token outside the vendor's own CLI" | The deployment set `ITERION_FORBID_SUBSCRIPTION_OAUTH=1` and the run resolved subscription credentials for a `claw` or `pi` node. Add an Anthropic API key (BYOK), switch to `backend: claude_code`, or ask the operator to allow direct use; Anthropic then bills a separate **extra-usage** balance. Note that cloud-uploaded Claude credentials currently authenticate claw/Claude Code, not pi. |
| "Anthropic subscription extra-usage balance is empty" | A `claw` call (or pi with ambient/local subscription auth) reached Anthropic after that separate balance was exhausted. Enable/replenish extra usage at `claude.ai/settings/usage`, add an API key, or use `backend: claude_code` to spend the plan itself. |
| "invitation expired" / "invitation already accepted" | Ask the inviter to issue a new one. |
| Login redirects you back to `/login` after the OIDC bounce | The IdP and iterion disagree on the redirect URI; ping your operator with the URL bar contents at the moment of the bounce. |

## 10. Where the data lives

- **Your runs**: visible to every member of the team that owns them.
  Switching teams hides them; super-admins (your operator) can
  see across teams when they need to.
- **Your API keys**: user-scoped keys are visible only to you;
  team-scoped keys are visible to every member of that team.
- **Your OAuth subscriptions**: visible only to you. Sealed with
  the deployment master key — the operator can read which
  connections exist + their expiry, but never the plaintext.
- **Audit log**: every OAuth-forfait use is logged with your user
  id, run id and kind. Operators can review it for cost attribution
  and CGU defence-in-depth.

## 11. Editing and creating bots

The bot **catalog** shipped with the instance is read-only (baked into
the runner image). To customise a bot or write your own, the studio's
bot editor saves into a **team-authored bot store** — the bundle lives
with your team, not on a pod's ephemeral filesystem, so it survives
restarts and every team member can launch it.

- **Fork a catalog bot** to get an editable copy of its whole bundle
  (`main.bot` + `manifest.yaml` + `skills/`…), then edit it.
- **Author a new bot** from scratch in the multi-file editor.
- Every save **compiles the bundle first** — a bot that doesn't compile
  is rejected with its diagnostics rather than failing later at launch.
- Concurrent edits are guarded: if a teammate saved between your load
  and your save, you get a conflict and re-load.

Editing is gated: you need the **bot-editor capability**, or to be a
team admin or owner — a bot runs in every member's context, so authoring
one is team automation policy, not a personal setting. (If your operator
hasn't wired a bot store, the editor reports that bot editing is not
enabled.) The underlying API is documented in
[cloud-rest-api.md → Team bot sources](cloud-rest-api.md#team-bot-sources-cloud-bot-editing).
