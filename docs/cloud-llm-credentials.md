# Cloud LLM credentials — provisioning a cloud run's model access

How a **cloud** run (queue → runner pod) gets its LLM credential, and how to
provision one so a run stops failing with `401` / `429`. Written from a real
session that lost hours rediscovering this; if you touch cloud cred flow,
keep it current.

## The one-paragraph model

A cloud run resolves its LLM credential **per-run** from the launching
**team/org**, then injects it into the runner as env. If the team has nothing,
the run gets no key and claw fails with `401 x-api-key header is required`.
Three credential kinds exist, and **which backend you use decides which kinds
are legal**:

| Credential | Stored via | Injected as | `claw` backend | `claude_code` backend |
|---|---|---|---|---|
| **BYOK API key** (`sk-ant-api…`, `sk-…`) | `iterion remote api-keys create --provider <p> --from-file/-env` | `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` (x-api-key) | ✅ | ✅ (also works) |
| **Anthropic OAuth-forfait** (Claude sub, `sk-ant-oat…`) | `POST /api/me/oauth/claude_code/credentials` (paste `credentials.json`) | Bearer + oauth beta | ⚠️ allowed, warns (bills EXTRA USAGE) | ✅ (it *is* Claude Code) |
| **OpenAI ChatGPT-forfait** (Codex `auth.json`, `auth_mode: chatgpt`) | `POST /api/me/oauth/codex/credentials` (paste `auth.json`) | ChatGPT-backend OAuth | ✅ (allowed) | n/a |

## Decision shortcut

- **Sovereign `web_search` / any claw feature** → needs claw. An Anthropic
  OAuth-forfait now works there (it bills your EXTRA USAGE balance, not your
  plan — see below), but a **BYOK API key** or the **OpenAI ChatGPT-forfait**
  is the predictable-cost choice.
- **Only have a Claude subscription (OAuth)** → use the **`claude_code`
  backend** (native WebSearch/WebFetch + forwarded MCP). Legit: it *is*
  Claude Code, within Anthropic's Consumer Terms.
- **Have ChatGPT Plus/Pro + Codex signed in** → connect the codex `auth.json`
  and run **claw + an `openai/*` model** — sovereign features work.

## Subscription OAuth on a third-party backend: a spend question, not a ToS one

This used to be a hard refusal (`GuardThirdPartyOAuth` →
`ErrOAuthForfaitInThirdParty`), read as a Consumer-Terms boundary. Anthropic's
API settled it: the token IS accepted from a third-party app and billed
against the subscription's separate **extra-usage** balance rather than the
plan's limits. So the question is not "may I" but "which pot am I spending".

`secrets.GuardSubscriptionOAuth` ([pkg/secrets/credentials.go](../pkg/secrets/credentials.go),
called by `claw` and `pi`) therefore **warns once per node** instead of
refusing — the operator is spending a balance they may not expect — and
refuses only under **`ITERION_FORBID_SUBSCRIPTION_OAUTH=1`**.

**Set that flag on a shared or cloud instance.** There, consuming one
operator's extra-usage balance is a cost decision taken on behalf of everyone
using the instance. The flag closes the per-run credential path AND the env
path (it clears `ANTHROPIC_AUTH_TOKEN` when the value is a subscription token
— `sk-ant-oat…` — leaving a z.ai facade key or gateway bearer in that same
variable untouched), and it is forwarded into the sandbox so the refusal holds
inside the container too.

OpenAI's ChatGPT-forfait has never had an equivalent restriction.

## Gotchas that cost real time

- **A team BYOK `openai` key beats the forfait, and no env var overrides it.**
  `Registry.ResolveWithContext`
  ([pkg/backend/model/registry.go](../pkg/backend/model/registry.go)) hands a
  non-empty tenant key straight to the provider factory — the forfait path is
  never consulted on that branch. So a quota-dead team key yields
  `429 exceeded your current quota` while a valid forfait sits unused, and the
  **only** fix is removing the key (`iterion remote api-keys list` →
  `delete <id>`). `ITERION_OPENAI_USE_OAUTH=1` does **not** help here: that
  force-flag is read only by the local disk factory (the desktop/CLI path that
  reads `~/.codex`), not by the cloud per-run path.
- **With no team key, the forfait already wins over the pod key** — no flag
  needed. `ResolveWithContext` tries the per-run codex forfait *before* falling
  back to the env-var resolver that reads the pod's `OPENAI_API_KEY` (from the
  `iterion-llm` secret). Only the disable knobs are global:
  `ITERION_OPENAI_USE_OAUTH=0` or any `OPENAI_BASE_URL` turns the forfait path
  off (set them under the chart's top-level `config.extraEnv`).
- **An Anthropic OAuth token stored as a BYOK `anthropic` key fails as
  `invalid x-api-key`** — it's sent as `x-api-key`, but an OAuth token needs
  `Authorization: Bearer`. BYOK is for real API keys only.
- **A keyless team → `401 x-api-key header is required`** (nothing injected),
  distinct from `invalid x-api-key` (something injected, wrong shape).
- **Codex `auth.json` must be in ChatGPT mode — there is nothing to strip.**
  The upload is parsed into `CodexCredentialsView`
  ([pkg/secrets/oauth.go](../pkg/secrets/oauth.go)), which declares only
  `auth_mode` + `tokens.*` + `last_refresh`; an `OPENAI_API_KEY` sitting in the
  file is dropped by the JSON decoder and can never shadow anything. The real
  failure is a blob that isn't ChatGPT-mode: `IsChatGPTMode()` requires
  `auth_mode: "chatgpt"` **and** `tokens.access_token` **and**
  `tokens.account_id`, and when it is false the forfait is silently skipped.
  Re-run `codex login` with "Sign in with ChatGPT" and upload the file
  unedited.

## Provisioning cookbook (via `iterion remote`, authenticated)

```sh
# BYOK — value read from a file, never printed:
iterion remote api-keys create --provider anthropic --name mykey --from-file ~/anthropic.key
iterion remote api-keys create --provider openai   --name mykey --from-file ~/openai.key

# Anthropic OAuth-forfait (claude_code backend only) — send the WHOLE
# credentials.json. A body carrying only accessToken is accepted but stored
# NON-refreshable (`sealOAuthRecord` sets NotRefreshable when refreshToken is
# absent), so the connection dies silently at the next token expiry:
iterion remote api POST /api/me/oauth/claude_code/credentials \
  --data "@$HOME/.claude/.credentials.json"
# (Linux/WSL path. On macOS Claude Code keeps these in the Keychain, so there
#  may be no file to read until you export it.)

# OpenAI ChatGPT-forfait (claw + openai/* model) — the Codex auth.json.
# Note `"@$HOME/…"`, not `@~/…`: the shell only expands a tilde at the START
# of a word, and ReadDataArg does no expansion of its own, so `@~/...` is read
# literally and fails with "no such file or directory".
iterion remote api POST /api/me/oauth/codex/credentials --data "@$HOME/.codex/auth.json"
# then, if a team BYOK openai key shadows it: delete that key (see the gotchas)

iterion remote api GET /api/me/oauth/connections     # verify (check refreshable)
```

Scope — the two credential kinds resolve **differently**:

- **BYOK keys** are team-scoped, personal-first: `(team, you, provider)` then
  `(team, "", provider)` ([pkg/secrets/byok.go](../pkg/secrets/byok.go)). A run
  under a keyless team won't see another team's keys.
- **OAuth forfaits** resolve **user-first with an org fallback**
  ([pkg/server/cloudpublisher/publisher.go](../pkg/server/cloudpublisher/publisher.go): `addOAuth(ownerID,
  "user")` then `addOAuth(OrgOwnerKey(tenantID), "org")`), so a personal
  `/api/me/oauth/…` connection follows *you* across teams — switching the
  active team neither gains nor loses it.

**Automated runs need the org-scoped upload.** A webhook / dispatcher / cron
run is owned by a synthetic identity with no personal forfait, so it only ever
sees the org record. Provision those on the team:

```sh
iterion remote api POST /api/teams/<team-id>/oauth/codex/credentials \
  --data "@$HOME/.codex/auth.json"
iterion remote api GET /api/teams/<team-id>/oauth/connections
```

## Related

- Backends + provider routing + the OpenAI-via-ChatGPT-forfait section:
  [backends.md](backends.md).
- The sovereign `web_search` backend ladder (a claw feature — hence needs a
  claw-legal credential): [web-search.md](web-search.md).
