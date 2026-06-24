# Backends and credential auto-detection

iterion ships three execution backends — `claw` (in-process LLM SDK),
`claude_code` (Claude Code CLI subprocess), and `codex` (Codex CLI
subprocess) — and picks one automatically when neither the workflow
nor the node spell out which one to use. This page documents the
resolution chain, the credentials each backend understands, and how
to override the auto-selection.

> **Cloud BYOK.** The auto-detection below is the *host/env* path. In
> cloud mode, provider API keys are owned per-org and sealed in Mongo,
> resolved per-run with a precedence chain (per-webhook override → user
> default → org default → env fallback). See [byok.md](byok.md).

## TL;DR

If you have **at least one** of:

- Claude Code signed in (OAuth in the macOS Keychain on Mac, or
  `~/.claude/.credentials.json` on Linux/WSL — "forfait")
- `ANTHROPIC_API_KEY` set in your environment
- `OPENAI_API_KEY` set in your environment
- `AZURE_OPENAI_API_KEY` + `AZURE_OPENAI_ENDPOINT`
- AWS credentials (Bedrock) or `GOOGLE_CLOUD_PROJECT` (Vertex)

… then opening the studio, hitting **New**, and clicking **Run**
will work without any further configuration. The agent in the
default template has empty `backend:` and `model:` — both are
filled in at run time from what's available.

## Resolution chain

When a node, workflow, and env are all silent, the runtime resolves
a backend in this order (first non-empty wins):

1. `node.Backend` — the `backend:` line on the agent/judge/router
2. `workflow.default_backend` — the workflow-level `default_backend:`
3. `ITERION_DEFAULT_BACKEND` — environment override
4. **Auto** — the first backend in `ITERION_BACKEND_PREFERENCE` whose
   credentials are detected on the host
5. `claw` — last-resort fallback

The empty-template path lands on step 4. The pill in the studio
toolbar surfaces what the auto-resolver picked (and turns red when
no credential is available). Settings → Backends lists every detected
source and the resolved default in full:

![Studio settings — detected LLM backend credentials and resolved default](images/studio/settings-backends.png)

## Default preference order

```
claude_code → claw
```

`codex` is intentionally **not** in the default list. The codex SDK
has known limitations (see [codex C030](../pkg/dsl/ir/compile.go))
and we'd rather have authors opt in explicitly than auto-select it.
You can still set `backend: codex` per-node, or include it in
`ITERION_BACKEND_PREFERENCE` to make it eligible for auto-selection.

`claude_code` is preferred over `claw` when the user has the Claude
Code OAuth file — that path uses the user's "forfait" subscription
instead of metered API calls. Without OAuth, `claw` is preferred:
same auth (ANTHROPIC_API_KEY) but in-process and faster.

### Overriding the order

Set `ITERION_BACKEND_PREFERENCE` to a comma-separated list:

```bash
# Prefer claw even when Claude Code OAuth is present
export ITERION_BACKEND_PREFERENCE='claw,claude_code'

# Only use codex (must be explicitly listed)
export ITERION_BACKEND_PREFERENCE='codex'
```

Backends omitted from the list are never auto-selected, even if
their credentials exist.

## Per-node provider routing & fallback chain (`provider:`)

The `backend:` field chooses *which* execution stack runs a node; the
optional `provider:` field is a finer **credential-routing hint** within
that stack. It is resolved per node after `${VAR}` / `${VAR:-default}`
expansion.

Known hints:

| Hint | Effect |
|---|---|
| `anthropic` | Force Anthropic-direct (`ANTHROPIC_API_KEY` / Claude Code OAuth); skip z.ai even when `ZAI_API_KEY` is set. |
| `zai` | Force the z.ai Anthropic-compatible facade (`ANTHROPIC_BASE_URL`=z.ai + `ANTHROPIC_AUTH_TOKEN`=`$ZAI_API_KEY`). |
| `openai` | Force OpenAI-direct (`OPENAI_API_KEY`), skipping `OPENAI_BASE_URL` overrides. |
| `auto` / *(unset)* | Default process-env precedence. |

### Fallback chain

`provider:` accepts a single value **or** an ordered, comma-separated
chain. The chain is the declarative generalisation of the
`RESCUE_PROVIDER` escape hatch:

```yaml
agent reviewer:
  backend: "claude_code"
  provider: "${RESCUE_PROVIDER:-zai},anthropic"   # z.ai first, Anthropic on hard failure
  model: "claude-opus-4-8"
```

Semantics:

- Each provider gets the node's **full retry budget** (transient errors
  are retried in place — see `RetryPolicy`).
- Only a **hard failure beyond the retry budget** — a non-retryable
  error, or a retryable one whose retries are exhausted — falls through
  to the *next* provider. The executor re-issues the same call with the
  next hint and emits **one** log note (and an `OnProviderFallback`
  observability event), so the operator sees a route change, not a
  failure.
- The node only fails if **every** provider in the chain is exhausted;
  the surfaced error names the chain that was attempted.
- A cancelled / timed-out run aborts the chain immediately rather than
  thrashing through every provider.
- Env expansion runs on the whole field **first**, then the result is
  split on commas — so an env var can supply the entire chain
  (`${PROVIDERS:-anthropic,zai}`) and a `:-default` may itself contain a
  comma.

### Which backends honour the chain

Only **`claude_code`** consumes the provider hint today, and it routes
within the **Anthropic-compatible family** (`anthropic` ↔ `zai` ↔ other
facades) — i.e. the same model id served by a different credential lane.
This is the validated path and the original `RESCUE_PROVIDER` use case.

`claw` derives its provider from the `model:` prefix
(`openai/…`, `anthropic/…`), and `codex` ignores the hint entirely. On
those backends a multi-element chain is a **no-op**: the runtime uses
only the first provider, and the compiler emits a **C088** warning. For
cross-provider failover under `claw` (e.g. Anthropic → OpenAI), vary the
`model:` per node instead — a credential hint alone cannot switch the
model that the API expects.

Unknown hint tokens (typos) are flagged at compile time with **C087**
(a warning) and ignored at run time (the node falls back to default
credential precedence). Fields containing a `${VAR}` env ref are left
for run-time resolution and not statically validated.

Single-value `provider:` (and unset) behaviour is unchanged — the chain
form is purely additive and fully back-compatible.

### Per-element model (`provider:model`)

A provider-specific model can be pinned per chain element with a
`provider:model` token, so a fall-through swaps **both** the credential
hint and the wire model:

```yaml
agent reviewer:
  backend: "claude_code"
  provider: "zai:glm-5.2,anthropic:claude-opus-4-8"   # glm-5.2 on z.ai, claude-opus-4-8 on Anthropic
```

This is the case where the chain's two providers serve **different model
ids over the same Anthropic-wire API** — `glm-5.2` is a z.ai model that
Anthropic would reject, so a hint-only swap would break on fall-through.
The token is split on the **first** colon (a model id that itself
contains a colon survives intact). An element **without** a model
(`anthropic` in `zai:glm-5.2,anthropic`) inherits the node's `model:`
baseline; an inheriting element after a model-bearing one restores the
baseline rather than carrying the previous override. A malformed element
— a colon with an empty provider (`:glm-5.2`) or empty model (`zai:`) —
warns **C172** at compile time. Env expansion still runs on the whole
field first, so the `:-` in `${VAR:-x}` is never mistaken for a
`provider:model` separator.

Like the hint chain itself, per-element models only take effect on
`claude_code` (the only backend that walks the chain today).

## Transient-error & network resilience

A brief internet/API outage should not abort a whole run. Every backend
call goes through a retry loop (`RetryPolicy`) with **capped exponential
backoff + jitter**, and the classifier treats connectivity failures as
retryable:

- **Detection** (`delegate.IsNetworkError`): `net.Error` timeouts, wrapped
  syscall errnos (`ECONNRESET`, `ECONNREFUSED`, `ETIMEDOUT`, …),
  `io.ErrUnexpectedEOF`, and a broad message-substring fallback for errors
  that cross the CLI/SDK boundary as text — `fetch failed`, `socket hang
  up`, `getaddrinfo ENOTFOUND`, `overloaded`, 5xx. The run's own
  `context.Canceled` / `DeadlineExceeded` are deliberately **excluded** so
  a cancelled or timed-out run never thrashes the retry budget.
- **`claude_code` silent exits**: a connectivity drop surfaces as an opaque
  `session ended without result message (cli_exit_code=N)`, with the real
  cause only on the CLI's stderr. The delegate re-types it as
  `ErrTransient` when stderr shows a network marker (one explicit
  `network connectivity issue detected` warn), so the loop retries instead
  of failing the node.
- **Adaptive budget**: network/transient errors get a larger attempt
  budget (`MaxAttemptsTransient`, default **6**) than ordinary retryable
  errors (`MaxAttempts`, default **3**), so the backoff spans roughly a
  minute of outage. An explicitly pinned `MaxAttempts` is always respected
  (the inflated default only applies when neither is set). Each retry logs
  `network connectivity issue — delegate retry k/n … (backoff …)`.

Only after a provider exhausts this budget does the run fall through to the
next entry in the `provider:` chain (above).

## Per-backend detection rules

A backend reports `Available: true` only when **both** a binary/runtime
and a credential are present. Just having the CLI in `PATH` is not
enough — the runtime still needs an API key or an OAuth file to
actually make calls.

### `claude_code`

| Credential | Source |
|---|---|
| OAuth (forfait) | **macOS:** the `Claude Code-credentials` item in the Keychain (where Claude Code 2.x stores it by default — no file is written). **Linux/WSL:** `$CLAUDE_CONFIG_DIR/.credentials.json` (default `~/.claude/.credentials.json`; non-hidden `credentials.json` also accepted) |
| Binary | `claude` in `$PATH`, or `~/.claude/local/claude` |

On macOS the Keychain is probed for *existence only* (via
`/usr/bin/security find-generic-password` — the token is never read), so a
machine logged into Claude Code is detected even though it has no
`.credentials.json` file. For auto-resolution, **OAuth is required**. If you
only have `ANTHROPIC_API_KEY` and the binary, `claw` is preferred (same auth,
no subprocess fork). To use `claude_code` with API-key auth, set
`backend: claude_code` explicitly on the node.

### `codex`

| Credential | Source |
|---|---|
| OAuth | `$CODEX_HOME/auth.json` (default `~/.codex/auth.json`) |
| Binary | `codex` in `$PATH`, `~/.volta/bin/codex`, `~/.local/bin/codex`, `/usr/local/bin/codex`, `/usr/bin/codex` |

Same logic as claude_code: only OAuth flips it to "available" for
auto-resolution. `OPENAI_API_KEY` alone routes to `claw`.

### `claw`

`claw` is in-process and pluralised across providers. It reports
`Available: true` when **any** of these is set:

| Provider | Detection |
|---|---|
| `anthropic` | `ANTHROPIC_API_KEY` or `ANTHROPIC_AUTH_TOKEN` |
| `openai` | `OPENAI_API_KEY`, **or** Codex CLI signed in via "Sign in with ChatGPT" (see `OpenAI via ChatGPT forfait` below) |
| `foundry` (Azure) | `AZURE_OPENAI_API_KEY` + `AZURE_OPENAI_ENDPOINT` |
| `bedrock` | `AWS_REGION` or `AWS_DEFAULT_REGION` (full chain handled by AWS SDK) |
| `vertex` | `GOOGLE_CLOUD_PROJECT` |

When `model:` on the agent is also empty, the runtime substitutes a
sensible default for the first available provider — currently
`anthropic/claude-opus-4-8` for Anthropic and
`openai/gpt-5.4-mini` for OpenAI.

### OpenAI via ChatGPT forfait (Codex CLI OAuth)

When Codex CLI is signed in via *Sign in with ChatGPT* (rather than the
default API-key mode), iterion's `claw` provider can reuse that OAuth
token to drive OpenAI calls through the ChatGPT-Codex backend
(`chatgpt.com/backend-api/codex/responses`) — billed against the user's
ChatGPT Plus / Pro / Team subscription instead of the metered
api.openai.com endpoint.

**Setup:**

```bash
# 1. Install or update Codex CLI (>= 0.130.0 for gpt-5.5 access).
# 2. Sign in via ChatGPT (NOT API-key):
codex logout                     # if previously logged in via API key
codex login                      # follow prompts → "Sign in with ChatGPT"
# 3. Verify auth.json carries chatgpt mode:
jq '.auth_mode' ~/.codex/auth.json   # → "chatgpt"
```

Iterion auto-detects this on the next `iterion run`. The status pill
shows the OpenAI provider as available even without `OPENAI_API_KEY`
set.

**Precedence:**

`OPENAI_API_KEY` wins when both are present. The reasoning: an explicit
env var was a deliberate user action — typically a project-scoped BYOK
key, a CI secret, or a shared workspace credential — and silently
spending someone else's ChatGPT subscription would be a surprising
default. ChatGPT-OAuth activates when `OPENAI_API_KEY` is unset.

```bash
# Force OAuth even with OPENAI_API_KEY set:
export ITERION_OPENAI_USE_OAUTH=1

# Force API-key only (refuse to use OAuth even if no key is set):
export ITERION_OPENAI_USE_OAUTH=0

# Setting OPENAI_BASE_URL (for OpenRouter/Ollama/vLLM) automatically
# disables OAuth so masquerading codex_cli_rs headers don't reach an
# unintended backend.
```

The studio status pill renders both detected sources, with the
inactive one struck-through and labelled `(overridden by …)`.

**Model-version gating.** OpenAI's backend gates model access on the
HTTP `version:` header iterion sends with each call. By default iterion
derives this from `codex --version`; override with
`ITERION_CODEX_VERSION=X.Y.Z` if you need to claim a different version
without reinstalling the binary. Concretely: `gpt-5.5` requires
codex-cli >= 0.130; `gpt-5.4` works on older versions.

**Refresh.** iterion does **not** implement OAuth refresh — it reads
whatever `access_token` is currently on disk. Codex CLI maintains the
file as a side effect of normal use; if your token expires mid-run,
just `codex --version` (or any other Codex command) to trigger a
refresh, then re-run iterion.

**ToS posture.** Unlike Anthropic Pro/Max (whose Consumer Terms scope
the subscription to *Claude Code only* — see the warning under
[z.ai integration](#using-a-non-anthropic-provider-via-the-anthropic-wire-format-zai--glm)),
ChatGPT subscriptions don't carve out Codex CLI as the only legitimate
surface. Reproducing Codex CLI's OAuth flow from a third-party tool is
gray-area but has no explicit prohibition today. We treat this as
pragmatic — if OpenAI changes the terms or tightens enforcement, set
`ITERION_OPENAI_USE_OAUTH=0` and fall back to `OPENAI_API_KEY`.

## Editor UX

The studio calls `GET /api/backends/detect` at mount time. The
**status pill** in the top-left of the toolbar shows:

- 🟢 **Green** + auto-resolved backend name when at least one
  credential is detected.
- 🔴 **Red** "no creds" when nothing is detected. The Run button is
  disabled in this state.
- Click the pill for a per-backend breakdown, sources, hints, and a
  link back to this page.

The detection result is cached server-side for 30 seconds. Click
**Refresh** in the popover to re-probe after fixing your env.

## Workflow-level pinning

To pin a backend across an entire workflow (e.g. force `claude_code`
even when OAuth is missing, expecting CI to inject it later):

```iter
default_backend: claude_code

agent reviewer:
  # inherits backend: claude_code
  ...
```

Per-node overrides take precedence:

```iter
agent reviewer:
  backend: claw
  model: anthropic/claude-haiku-4-5-20251001
```

## Using a non-Anthropic provider via the Anthropic wire format (z.ai / GLM)

Some providers ship an Anthropic-compatible HTTP endpoint so existing
Claude Code clients can talk to them with zero code change. The most
common case today is **z.ai's Coding Plan**, which serves GLM-4.5 /
GLM-4.6 through `https://api.z.ai/api/anthropic` (or whatever endpoint
your z.ai dashboard lists — confirm there). Anthropic itself encourages
this kind of integration for partner providers; z.ai's own docs
describe the Claude Code wiring.

Iterion-desktop sources `~/.iterion/env` at startup (commit `84a7fc2`)
and `claudesdk/process.go` forwards the entire host env to the spawned
Claude Code subprocess. There are two equivalent ways to wire it.

### Shortcut: `ZAI_API_KEY` alone

Recommended path — drop a single line in `~/.iterion/env`:

```bash
# ~/.iterion/env
ZAI_API_KEY=<bearer token from your z.ai dashboard>
```

When iterion sees `ZAI_API_KEY` set AND no `ANTHROPIC_API_KEY` /
`ANTHROPIC_AUTH_TOKEN` set, it automatically configures
`ANTHROPIC_BASE_URL=https://api.z.ai/api/anthropic` and
`ANTHROPIC_AUTH_TOKEN=$ZAI_API_KEY` for both the spawned Claude Code
subprocess (`backend: claude_code`) and the in-process claw provider
factory (`backend: claw`). Restart iterion-desktop after editing the
file so the launcher re-sources it.

If `ANTHROPIC_API_KEY` (or `ANTHROPIC_AUTH_TOKEN`) is also set, that
takes precedence — the shortcut is intentionally "auto-route only
when no Anthropic auth is configured". This lets a user keep a
fallback Anthropic key for some workflows without losing the z.ai
default.

### Explicit form: `ANTHROPIC_BASE_URL` + `ANTHROPIC_AUTH_TOKEN`

For full control, set the two env vars directly:

```bash
# ~/.iterion/env
ANTHROPIC_BASE_URL=https://api.z.ai/api/anthropic
ANTHROPIC_AUTH_TOKEN=<bearer token from your z.ai dashboard>
# Leave ANTHROPIC_API_KEY UNSET — if both are present, Claude Code
# prefers the API key and routes back to Anthropic, defeating the
# purpose.
```

Workflows then run unchanged: `backend: claude_code` still selects
the same delegate, but the network destination is z.ai and the
underlying model is GLM. Model strings stay Anthropic-shaped
(`claude-opus-4-7`, …); z.ai's gateway maps them to its own GLM
families internally.

**Important caveats**

- This only works when Anthropic's wire-format aliasing exists at the
  provider side. If you're pointing at OpenRouter, Ollama, or another
  OpenAI-shaped endpoint, use `backend: claw` with `model: openai/…`
  + `OPENAI_BASE_URL` instead.
- **API keys only — no forfait via iterion.** Both Anthropic's Consumer
  Terms (Pro/Max plans) and z.ai's Coding Plan terms restrict
  subscription benefits to *officially supported tools*. Driving either
  provider's subscription/OAuth forfait through iterion (or any other
  third-party orchestrator) is a ToS violation. Always use a BYOK API
  key path: `ANTHROPIC_API_KEY`, `ZAI_API_KEY`, or the BYOK panel in the
  cloud UI. The legacy in-cloud OAuth-forfait wiring
  (`pkg/server/oauth_routes.go::OAuthKindClaudeCode`) is scheduled for
  removal — see `.plans/zai-glm-byok.md`.
- Cost: iterion's token-usage panels currently price against an
  Anthropic rate card. When you route to z.ai the wire shape is
  unchanged so token counts are still reported, but the dollar
  estimates are not accurate until a per-provider rate card lands.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| Pill is red | No credential detected | Set `ANTHROPIC_API_KEY` or sign in to Claude Code, then click Refresh |
| Pill is green but Run errors out with "no provider" | Workflow uses `model: openai/...` but only `ANTHROPIC_API_KEY` is set | Switch model to an Anthropic spec, or add `OPENAI_API_KEY` |
| Pill says "claude_code" but you wanted "claw" | OAuth is found and ranked first | `export ITERION_BACKEND_PREFERENCE='claw,claude_code'` |
| Pill says "claw" but you wanted Codex | Codex isn't in the default order | `export ITERION_BACKEND_PREFERENCE='codex,claude_code,claw'` and ensure `$CODEX_HOME/auth.json` exists |
| Editor pill stale after fixing env | Server cache (30s) | Click the pill → **Refresh** |
