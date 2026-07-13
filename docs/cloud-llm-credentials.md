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
| **Anthropic OAuth-forfait** (Claude sub, `sk-ant-oat…`) | `POST /api/me/oauth/claude_code/credentials` (paste `credentials.json`) | Bearer + oauth beta | ❌ **guarded** (CGU) | ✅ (it *is* Claude Code) |
| **OpenAI ChatGPT-forfait** (Codex `auth.json`, `auth_mode: chatgpt`) | `POST /api/me/oauth/codex/credentials` (paste `auth.json`) | ChatGPT-backend OAuth | ✅ (allowed) | n/a |

## Decision shortcut

- **Sovereign `web_search` / any claw feature** → needs claw → **NOT** an
  Anthropic OAuth token (guarded). Use a **BYOK API key** or the **OpenAI
  ChatGPT-forfait**.
- **Only have a Claude subscription (OAuth)** → use the **`claude_code`
  backend** (native WebSearch/WebFetch + forwarded MCP). Legit: it *is*
  Claude Code, within Anthropic's Consumer Terms.
- **Have ChatGPT Plus/Pro + Codex signed in** → connect the codex `auth.json`
  and run **claw + an `openai/*` model** — sovereign features work.

## Why the guard exists (do not bypass)

`secrets.GuardThirdPartyOAuth` ([pkg/secrets/credentials.go](../pkg/secrets/credentials.go),
called at [pkg/backend/model/claw_backend.go](../pkg/backend/model/claw_backend.go))
returns `ErrOAuthForfaitInThirdParty` when a run would drive **Anthropic**
with a **Claude Code OAuth-forfait** through **claw** (a third-party SDK).
Anthropic's Consumer Terms scope the subscription OAuth to Claude Code only —
so this is a **ToS boundary, not a bug**. It guards the *SDK*, not the user:
even the sole operator on a dev instance must not route the Claude
subscription through claw. OpenAI's ChatGPT-forfait has no such restriction,
so claw + ChatGPT-forfait is fine.

## Gotchas that cost real time

- **BYOK precedence over OAuth.** `OPENAI_API_KEY` (or a pod-level platform
  key from the `iterion-llm` secret) **wins** over a connected ChatGPT-forfait.
  If the platform key is quota-dead you get `429 exceeded your current quota`
  while a valid forfait sits unused. Force the forfait with
  **`ITERION_OPENAI_USE_OAUTH=1`** on the runtime
  (`iterion.config.extraEnv` in the Helm values). Same idea:
  `ITERION_OPENAI_USE_OAUTH=0` / any `OPENAI_BASE_URL` disables OAuth.
- **An Anthropic OAuth token stored as a BYOK `anthropic` key fails as
  `invalid x-api-key`** — it's sent as `x-api-key`, but an OAuth token needs
  `Authorization: Bearer`. BYOK is for real API keys only.
- **A keyless team → `401 x-api-key header is required`** (nothing injected),
  distinct from `invalid x-api-key` (something injected, wrong shape).
- **Codex `auth.json` carries both a metered `OPENAI_API_KEY` and the
  chatgpt `tokens`.** Uploading it as-is lets the metered key win → `429`.
  Strip `OPENAI_API_KEY` from the blob before upload (or set
  `ITERION_OPENAI_USE_OAUTH=1`) so only the forfait remains.

## Provisioning cookbook (via `iterion remote`, authenticated)

```sh
# BYOK — value read from a file, never printed:
iterion remote api-keys create --provider anthropic --name mykey --from-file ~/anthropic.key
iterion remote api-keys create --provider openai   --name mykey --from-file ~/openai.key

# Anthropic OAuth-forfait (claude_code backend only) — paste credentials.json:
iterion remote api POST /api/me/oauth/claude_code/credentials \
  --data '{"claudeAiOauth":{"accessToken":"<token>"}}'

# OpenAI ChatGPT-forfait (claw + openai/* model) — paste Codex auth.json:
iterion remote api POST /api/me/oauth/codex/credentials --data @~/.codex/auth.json
# then, if a metered OPENAI_API_KEY shadows it: set ITERION_OPENAI_USE_OAUTH=1 on the runtime

iterion remote api GET /api/me/oauth/connections     # verify
```

Team scope: creds resolve from the **launching team/org**. A run launched
under a keyless team won't see keys on another team — provision on the team
you launch under (or switch active team).

## Related

- Backends + provider routing + the OpenAI-via-ChatGPT-forfait section:
  [backends.md](backends.md).
- The sovereign `web_search` backend ladder (a claw feature — hence needs a
  claw-legal credential): [web-search.md](web-search.md).
