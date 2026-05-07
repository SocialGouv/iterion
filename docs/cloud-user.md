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
  you. Argon2id at rest, refresh tokens auto-rotate every 15
  minutes, full session expires after 30 days of inactivity.
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

The chip in the top-right of the editor surfaces the active team;
clicking it lets you:

- Switch teams. The server re-bakes a fresh access JWT bound to the
  new `tenant_id`. All editor surfaces (run list, files panel,
  settings) follow.
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
Azure (Foundry), OpenRouter, xAI.

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
already have an iterion account, into `/invite/<token>` on a
logged-in session).

Invitations expire after 7 days.

## 6. Common errors

| What you see | What's going on |
|---|---|
| "no API key configured for provider X" at run launch | Neither you nor your team has registered a key for that provider, and the operator hasn't set the env-var fallback. Add a key in `/account` → API keys or ask a team admin to add one. |
| "refusing to use Claude Code OAuth-forfait via third-party SDK" | A workflow uses `backend: claw` against an Anthropic model and you only have a forfait connected. Either add an Anthropic API key (BYOK) or switch the workflow to `backend: claude_code`. |
| "invitation expired" / "invitation already accepted" | Ask the inviter to issue a new one. |
| Login redirects you back to `/login` after the OIDC bounce | The IdP and iterion disagree on the redirect URI; ping your operator with the URL bar contents at the moment of the bounce. |

## 7. Where the data lives

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
