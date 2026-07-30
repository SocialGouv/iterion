# 🤝 Backends and credential auto-detection

A backend is the executor iterion routes a node to — either the in-process
provider client and Iterion-native tool loop, or a coding-agent CLI with its
own tool loop — and one workflow can mix them per node. `model:` is an
independent wire-model pin, not a request for a particular backend. Iterion
ships six: `claw` (in-process LLM SDK),
`claude_code` (Claude Code CLI), `pi` (pi coding agent), `kimi` (Kimi Code
CLI), `grok` (Grok Build CLI), and the deprecated `codex` compatibility
delegate. It auto-detects whatever credentials you already have signed in: the
default preference considers `claude_code` and `claw`; the other four require
an explicit opt-in. This page documents the resolution chain, credentials,
support boundaries, and overrides.

```mermaid
flowchart LR
  MODEL["model: (optional wire-model pin)"] -.-> NODE{"🧠 Workflow node"}
  NODE --> RESOLVE{"backend: pin / workflow / env / credential detection"}
  RESOLVE -->|"claw"| DIRECT(["⚡ In-process provider client<br/>+ Iterion-native tools"])
  RESOLVE -->|"claude_code · pi · kimi · grok"| CLI[["🛠️ Delegated coding-agent CLI<br/>+ its own tool loop"]]
```

> **Cloud BYOK.** The auto-detection below is the *host/env* path. In
> cloud mode, provider API keys are owned per-org and sealed in Mongo,
> resolved per-run with a precedence chain (per-webhook override → user
> default → org default → env fallback). See [byok.md](byok.md).

## Backend status

| Backend | Status | Selection |
|---|---|---|
| `claw` | Recommended in-process backend for direct provider calls and native Iterion tools. | Automatic or explicit. |
| `claude_code` | Recommended CLI-agent backend for implementation work and Claude subscription/OAuth use. | Automatic when Claude Code OAuth is detected, or explicit. |
| `pi` | Supported, with iterion's permission gate. Reaches ~36 providers and reports a provider-computed cost. Runs a long-lived `--mode rpc` session by default — tool events, native steering, authoritative accounting, pre-flight handshake (`ITERION_PI_MODE=print` rolls back). Permission gate, ask_user, board capabilities and workflow-declared MCP servers (all three transports — streamable http, legacy sse, stdio) work via an embedded extension, which loads on the **rpc transport only**: a node declaring `permission:` is refused under `ITERION_PI_MODE=print` rather than run ungated. | Explicit only; detected and shown in Settings → Backends, and eligible for `ITERION_BACKEND_PREFERENCE`. |
| `kimi` | Supported through the generic CLI-agent protocol; session resume/fork is not wired. | Explicit only. |
| `grok` | Supported through the generic CLI-agent protocol; session resume/fork is not wired. | Explicit only. |
| `codex` | **Deprecated and frozen.** Compatibility/live-test path only; the compiler emits C030. | Per-node/workflow opt-in, or explicit addition to `ITERION_BACKEND_PREFERENCE`. |

## TL;DR

If you have **at least one** of:

- Claude Code signed in (OAuth in the macOS Keychain on Mac, or
  `~/.claude/.credentials.json` on Linux/WSL — "forfait")
- `ANTHROPIC_API_KEY` set in your environment
- `OPENAI_API_KEY` set in your environment
- `XAI_API_KEY` set in your environment (xAI Grok)
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

### Launch-time per-node/-group overrides

Above step 1 sits an explicit **launch-time override**: you can retarget
which model and/or backend specific nodes use for a single run, **without
editing the `.bot`**. Because the operator is deliberately re-pointing the
bot at launch, these win over the node's own DSL `backend:`/`model:`.

- **Studio** — the Launch form's "Model & backend per node" section lists
  the bot's LLM nodes (agents + judges) with a model input (suggesting
  detected providers' models) and a backend select; leave a field on
  *inherit* to keep the DSL default.
- **CLI** — repeatable `--model` / `--backend`, each a `selector=value` (or
  a bare `value` for every LLM node). A selector matches by exact node id
  (`reviewer_claude`), id glob (`reviewer_*`, `fix_*`), or node kind
  (`agent`|`judge`). Most specific match wins; resolution is per-field so
  `--model` and `--backend` compose:

  ```bash
  # cheap model for reviewers, stronger for fixers, all on claw
  iterion run bots/whole-improve-loop/main.bot \
    --model 'reviewer_*=anthropic/claude-fable-5' \
    --model 'fix_*=anthropic/claude-sonnet-5' \
    --backend '*=claw'
  ```

- **HTTP** — `POST /api/runs` accepts `model_overrides: [{selector, model,
  backend}]`.

This composes with the mono/dual `--review-mode` topology (ADR-052): the
review mode chooses *which family* runs (one or two), the override chooses
*which model/backend* each running node uses. Overrides are not yet
re-applied on `iterion resume` (same limitation as `--backend`/`--compress`).

## Default preference order

```
claude_code → claw
```

`codex` is intentionally **not** in the default list. The codex SDK
has known limitations (see [codex C030](../pkg/dsl/ir/compile.go)) and the
delegate is frozen: new workflows and backend work should use `claude_code`,
or `claw` with an OpenAI model. It remains available for compatibility and live
test coverage.
You can still set `backend: codex` per-node, or include it in
`ITERION_BACKEND_PREFERENCE` to make it eligible for auto-selection.

`claude_code` is preferred over `claw` when the user has the Claude
Code OAuth file — that path uses the user's "forfait" subscription
instead of metered API calls. Without OAuth, `claw` is preferred:
same auth (ANTHROPIC_API_KEY) but in-process and faster.

> 💳 **On `claw` (and `pi`), the Claude Code subscription bills to *extra
> usage*, not to your plan.** `claw` authenticates with the subscription OAuth
> token (set `ANTHROPIC_AUTH_TOKEN` to the `claudeAiOauth.accessToken`
> from `~/.claude/.credentials.json`, no `ANTHROPIC_API_KEY`; claw then
> sends `Authorization: Bearer` + the `anthropic-beta: oauth-2025-04-20`
> header), and it **works** — verified 2026-07-28 with a real
> `claude-haiku-4-5` call. It bills against the subscription's separate
> **extra-usage** balance rather than the plan's limits, which is what
> Anthropic says when that balance empties ("Third-party apps now draw from
> your extra usage, not your plan limits"). iterion emits a one-time stderr
> warning naming the billing, and `ITERION_FORBID_SUBSCRIPTION_OAUTH=1`
> refuses the path outright.
>
> This replaces an earlier note that called the path *effectively unusable*:
> non-Claude-Code clients used to be throttled to ~zero (immediate `429`,
> no `Retry-After`) while the official CLI on the same token succeeded.
> Anthropic has since served third-party clients through extra usage
> instead. For predictable spend on an `anthropic/*` model, `ANTHROPIC_API_KEY`
> or z.ai (`ZAI_API_KEY`) remain the metered options. (The OpenAI/ChatGPT
> forfait via `claw` also works — see below — because there is no equivalent
> client-identity gate on that path today.)

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

> **Legacy compatibility only.** This backend is deprecated and frozen. The
> details below document existing workflows; they are not a recommendation for
> new authoring.

| Credential | Source |
|---|---|
| OAuth | `$CODEX_HOME/auth.json` (default `~/.codex/auth.json`) |
| Binary | `codex` in `$PATH`, `~/.volta/bin/codex`, `~/.local/bin/codex`, `/usr/local/bin/codex`, `/usr/bin/codex` |

Same logic as claude_code: only OAuth flips it to "available" for
auto-resolution. `OPENAI_API_KEY` alone routes to `claw`.

#### Sandbox and `full_access`

A codex node runs under codex's own sandbox. `readonly: true` always selects
`read-only` (and takes precedence over a conflicting `full_access: true`).
Otherwise, an omitted/empty `tools:` list keeps Iterion's normal
"native tools unrestricted" contract and selects `workspace-write`; a non-empty
list uses `read-only` only when all declared tools are known readers, and
`workspace-write` when it contains a writer/shell tool (both Iterion snake_case
names and Claude-style names are recognised). Unknown/custom names conservatively
select `workspace-write`; use `readonly: true` for an explicit lock-down.

With Codex's default configuration neither `read-only` nor `workspace-write`
allows **network egress** — so a codex agent cannot reach an external API (e.g.
codex's built-in `imagegen`, or any HTTP call). A user-level Codex config can
override workspace-write network policy; Iterion does not currently rewrite that
file, so operators who require a hard network boundary should use an Iterion
Docker/Kubernetes sandbox with a backend that supports it.

To grant network access, the pipeline author sets `full_access: true` on the
node. It lifts the sandbox to `danger-full-access` (unrestricted network +
out-of-workspace writes) — the same posture as `codex exec -s
danger-full-access`. It is **off by default and opt-in per node** in the workflow:

```
agent make_cover:
  backend: "codex"
  full_access: true    # codex sandbox -> danger-full-access (needed for imagegen / network)
  user: cover_prompt
```

Other backends ignore `full_access` (they do not impose the codex sandbox).

> **Outer-sandbox limitation:** the pinned Codex Agent SDK cannot yet route its
> subprocess through Iterion's Docker/Kubernetes command builder. Iterion fails
> such a node explicitly instead of silently launching the CLI on the host. Set
> `sandbox: none` to rely on Codex's own sandbox, or use `claude_code`/`claw`
> when the outer container boundary is required.

#### Image inputs (`images:`)

A codex node can receive **input images** for image-to-image via the node-level
`images:` list. Each entry is a templated path, resolved per run against the
node's `input`/`vars`/etc. and forwarded to the codex CLI as `-i`. Empty results
(e.g. an optional reference that doesn't apply this run) are dropped.

```
agent keyframe:
  backend: "codex"
  full_access: true
  images: ["{{input.prev_frame}}", "{{input.identity_anchor}}"]
  user: keyframe_prompt
```

Use it to seed a generation from a prior image (e.g. reusing the previous
keyframe for visual continuity, or a character-identity anchor). Non-codex
backends ignore `images:` (all CLI delegates instead receive launch-time
`attachments:` by path plus the `read_image` tool; native multimodal forwarding
is claw-only).

### `claw`

`claw` is in-process and pluralised across providers. It reports
`Available: true` when **any** of these is set:

| Provider | Detection |
|---|---|
| `anthropic` | `ANTHROPIC_API_KEY` or `ANTHROPIC_AUTH_TOKEN` |
| `openai` | `OPENAI_API_KEY`, **or** Codex CLI signed in via "Sign in with ChatGPT" (see `OpenAI via ChatGPT forfait` below) |
| `xai` | `XAI_API_KEY` (xAI Grok — OpenAI-compatible chat completions at `api.x.ai`) |
| `foundry` (Azure) | `AZURE_OPENAI_API_KEY` + `AZURE_OPENAI_ENDPOINT` |
| `bedrock` | `AWS_REGION` or `AWS_DEFAULT_REGION` (full chain handled by AWS SDK) |
| `vertex` | `GOOGLE_CLOUD_PROJECT` |

When `model:` on the agent is also empty, the runtime substitutes a
sensible default for the first available provider (the detector's
`SuggestedModel` for the first available provider, in this priority
order) — currently
`anthropic/claude-opus-5` for Anthropic,
`anthropic/glm-5.2` for z.ai,
`openai/gpt-5.4-mini` for OpenAI, and
`xai/grok-3` for xAI.

### xAI Grok (`model: "xai/…"`)

xAI is a first-class claw provider. Set `XAI_API_KEY` (or store a BYOK
key under provider `xai` in cloud mode) and point a node at a Grok
model:

```yaml
agent planner:
  backend: "claw"
  model: "xai/grok-3"
  # optional: model: "xai/grok-3-mini"  # reasoning variant
```

Optional `XAI_BASE_URL` overrides the host (default `https://api.x.ai`).
A trailing `/v1` is stripped automatically so pasting the public
OpenAI-SDK base URL (`https://api.x.ai/v1`) still works — claw's
OpenAI-compatible client always appends `/v1/chat/completions`.

In cloud mode the same key can be stored as a per-org BYOK record with
`provider: xai` (see [byok.md](byok.md)). Sandboxed runs with
`network: allowlist` already include `api.x.ai` in the `iterion-default`
preset.

For web search & fetch on claw (the `web_search`/`web_fetch` tools, the
SearXNG → Brave → DuckDuckGo backend ladder, `ITERION_WEB_SEARCH`, and the
Firecrawl MCP tier), see [web-search.md](web-search.md).

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

**ToS posture.** ChatGPT subscriptions don't carve out Codex CLI as the only
legitimate surface. (Anthropic Pro/Max was long read as doing exactly that for
Claude Code; it now serves third-party clients and bills them to extra usage —
see the note under
[z.ai integration](#using-a-non-anthropic-provider-via-the-anthropic-wire-format-zai--glm).)
Reproducing Codex CLI's OAuth flow from a third-party tool is
gray-area but has no explicit prohibition today. We treat this as
pragmatic — if OpenAI changes the terms or tightens enforcement, set
`ITERION_OPENAI_USE_OAUTH=0` and fall back to `OPENAI_API_KEY`.

## Third-party agent CLIs (`pi`, `kimi`, `grok`, and the CLI-agent seam)

Some agent CLIs have an argument protocol **disjoint from claude-code's**
Session mode (`--print`, prompt on stdin, `--append-system-prompt`, …), so
neither `backend: claude_code` nor the per-node `command:` override (which
only swaps the *binary* while keeping claude-code's argv) can drive them.
iterion ships dedicated backends for these as instances of a generic
**CLI-agent** seam (`delegate.CLIAgentBackend` + `CLIAgentProtocol`, see
[ADR-065](adr/065-dedicated-cli-agent-backend.md)): build the target CLI's
*own* argv, run with a wall-clock timeout (inside the run's sandbox when
active), parse stdout into a structured result, retry on no-output /
network transient. Adding another such CLI is a new protocol *value*, not
new plumbing.

### `pi` (pi coding agent)

[pi](https://pi.dev) ([earendil-works/pi](https://github.com/earendil-works/pi))
is a multi-provider agent harness. It is the backend to reach for when you
need **a model the other backends cannot run**.

```yaml
agent review:
  backend: "pi"
  model: "openai/gpt-5.5"   # or cerebras/…, groq/…, zai/…, github-copilot/…
  system: "…the task…"
```

**Install:** `npm install -g @earendil-works/pi-coding-agent` (Node ≥ 22.19)
or `curl -fsSL https://pi.dev/install.sh | sh`. Pin a specific binary with
`ITERION_PI_BIN`, or per node with `command:`.

#### What it brings

- **~36 first-class providers** behind one agent loop, with an
  auto-refreshing model catalogue: anthropic, openai, openai-codex, google,
  vertex, bedrock, azure, github-copilot, xai, zai, moonshot, deepseek,
  groq, cerebras, mistral, minimax, openrouter, together, fireworks, and
  more. `pi --list-models` is the authoritative list for your install.
- **A provider-computed cost**, not an estimate. pi reports
  `usage.cost.total` per message against its own pricing catalogue; iterion
  records it verbatim (`cost.AnnotateWithUSD`) instead of guessing from the
  static table. It also reports the real input/output split, where the other
  CLI-agent backends book every token at the output rate. This makes
  `max_cost_usd` and the spend cap quantitatively correct on a CLI backend
  for the first time.
- **Its own credential store** (`~/.pi/agent/auth.json`) with OAuth flows
  for Anthropic, OpenAI (ChatGPT/Codex) and GitHub Copilot — an
  authentication path independent of iterion's.

#### What it does NOT bring

pi deliberately ships a small tool set: `read, bash, edit, write, grep,
find, ls`, and **no MCP client at all** — plus no subagent/`Task`, no todo,
no web fetch/search, no notebook, no background bash. The iterion pi
extension supplies the MCP half; the rest stands. Consequences for a
`backend: "pi"` node:

- **The permission gate DOES work** (from v3.15.0), supplied by the iterion pi
  extension that the backend loads automatically. `permission: ask|deny` and
  its `allow:`/`ask:`/`deny:` rule lists resolve through the same
  `permission.Policy` as `claude_code` and `claw`, so all three reach identical
  verdicts. RPC transport only — a print-mode node has no channel for it.
- **board `capabilities:` DO work** (from v3.15.0, RPC transport only): the
  extension bridges iterion's board MCP endpoint onto pi and registers each
  tool it advertises.
- **`ask_user` DOES work** (from v3.15.0, RPC transport only): the agent can
  put a question to the operator, which pauses the run and resumes with their
  answer.
- **Async questions DO work** (`interaction: async`, ADR-081 — from v3.15.0,
  RPC transport only): `ask_user_async` posts without stopping and
  `await_answers` is the sync point, with the same semantics as claw and
  claude_code. Answers are delivered mid-run through pi's native `steer`.
- **workflow `mcp_server:` blocks DO work** (from v3.15.0, RPC transport
  only), on all three transports: `http` (streamable HTTP), `sse` (the
  legacy binding) and `stdio` (a child process). The extension carries its
  own MCP client — pi has none — and discovers each server's tools through
  `tools/list`, so schemas and capability gating stay with the server.
  Two things to know:
  - **A sandboxed stdio server runs inside the container**, so its
    `command:` must resolve there. Same caveat as `claude_code`.
  - **Connecting is bounded** by `ITERION_PI_MCP_CONNECT_TIMEOUT_MS`
    (default 10000). Servers connect in parallel during pi's session
    start — which iterion's own 30s handshake is waiting on — so one
    unreachable server costs its own tools, not the run. Failures are
    logged, not fatal.
- **`__ITERION_SECRET_*__` placeholders are not materialised.** Use file
  secrets instead ([secrets.md](secrets.md)) — they are real mounted files
  and work unchanged.
- **node `tools:` lists are advisory**, as for every CLI-agent backend. The
  one exception iterion enforces is a `readonly:` node, which pins pi to
  `--tools read,grep,find,ls`.

A node needing any of the above should stay on `claude_code` or `claw`.
**pi's value is the models those cannot reach, not replacing them on a
workflow that already works.**

#### Behaviour worth knowing

- **`AGENTS.md` is read alongside `CLAUDE.md`, and it is the dominant
  per-call cost.** pi walks up from the working directory and injects both.
  Two consequences:
  - If your repo carries an `AGENTS.md` meant for a different agent, it
    reaches pi nodes too.
  - **Measure it before you budget.** On iterion's own tree (a 103 KB
    `CLAUDE.md`) a one-word prompt costs **26,933 input tokens with context
    files against 448 without** — sixty times the input, on every call,
    before the node does any work. It stays on by default for parity with
    `claude_code`; set `ITERION_PI_NO_CONTEXT_FILES=1` to turn it off when a
    node does not need the repo's instructions.
- **The target repo's `.pi/` directory is refused.** pi executes
  project-local extensions as TypeScript *inside the agent process* — the
  process holding the run's credentials — so trusting a checked-out
  repository turns prompt injection into code execution. iterion passes
  `--no-approve`. Opt in per node with `ITERION_PI_TRUST_PROJECT=1`, and
  only for a repository you control.
- **Skills are passed explicitly, one `--skill` per skill.** iterion mirrors
  bundle/plugin/library skills into `<workspace>/.claude/skills/`, which is
  not one of pi's own lookup roots. It names each mirrored skill individually
  rather than handing over the directory, because under `worktree: auto` that
  directory is a checkout of the *target* repository and CLI `--skill` paths
  bypass the project-trust gate `--no-approve` exists to close — so a repo
  that ships its own `.claude/skills/` would get attacker-authored prompt text
  loaded as trusted. The list comes from the engine, which is the only party
  that knows what it wrote: provenance recovered from the workspace would sit
  inside the very checkout it is meant to vouch against. Under
  `ITERION_PI_TRUST_PROJECT=1` the whole directory is passed, repo skills
  included — that is the opt-in.
- **Sessions live with the run**, under the store dir (or the workspace when
  sandboxed) — never `~/.pi/agent/sessions`, so concurrent nodes cannot
  collide and a pruned run takes its sessions with it.
- **pi retries upstream failures itself** — 3 attempts, 2s/4s/8s backoff by
  default — *inside* iterion's own retry loop. Two consequences, both
  observed live:
  - Those attempts are invisible to iterion's rate-limit classifier, which
    only ever sees the outcome of the last one.
  - **The reported cost is short by the discarded attempts.** Only the final
    attempt's transcript survives in pi's `agent_end`, so a node that made
    four billed upstream calls reports one call's tokens and cost. iterion
    logs a WARN naming the retry count when this happens — an unexplained
    slow node with a suspiciously low cost is the symptom.

  Print mode has no lever to disable this. Pin `ITERION_PI_AGENT_DIR` to an
  agent dir whose `settings.json` sets `retry.enabled: false` (or a small
  `retry.baseDelayMs`) if it matters for your quota accounting.
- **Model patterns are fuzzy-matched, and a near miss is dangerous.** pi
  resolves an unknown pattern against its catalogue rather than failing.
  Measured: **`zai/glm-5` resolves to `glm-5v-turbo`** — a *vision* model, not
  GLM 5.x. iterion logs a warning whenever the model pi actually used differs
  from the one requested; watch for it on first use. (A *far* miss like
  `zai/glm5` is safer: it passes through literally and the provider rejects it
  with a clean 400, typed as deterministic so no retries are burnt.)

- **`session: inherit` needs the edge to carry `_session_id`.** Session resume
  works — but the id travels on the upstream node's *output* map, and a bare
  `a -> b` edge does not forward it, so the node silently runs fresh and the
  agent "forgets" the previous conversation. iterion now warns; the fix is
  `a -> b with { _session_id: "{{outputs.a._session_id}}" }`. This is not
  pi-specific: it applies to every backend.

#### 💳 Subscription credentials bill to *extra usage*

A Claude Pro/Max OAuth subscription **works** on pi, and on `claw`. Anthropic
does not reject a third-party app using it — it answers:

> Third-party apps now draw from your extra usage, not your plan limits. Add
> more at claude.ai/settings/usage and keep going.

So the path is supported, but billed against a **separate extra-usage
balance** rather than your plan's limits. That is the one surprising part, so
iterion logs a warning naming it on every node that spends a subscription
token outside the vendor's own CLI. When the balance is empty the API returns
a `400 invalid_request_error` whose text mentions nothing about credentials;
iterion translates it into a message naming the cause and the ways out, so it
does not read like a broken token.

**To refuse it instead**, set `ITERION_FORBID_SUBSCRIPTION_OAUTH=1`. Worth
doing on a shared or cloud instance, where spending an operator's extra-usage
balance is a cost decision taken on behalf of everyone using it. The refusal
applies to pi and `claw` alike; `claude_code` and `codex` are unaffected —
they spawn the vendor's own CLI, which draws on the plan normally.

For production Anthropic work, a metered `ANTHROPIC_API_KEY` or a
`claude_code` node remains the predictable choice.

Your *own* `pi` login in `~/.pi/agent/auth.json` is your relationship with
the vendor — nothing is read or injected. `github-copilot` is the same: iterion
injects nothing for it.

`openai-codex` is the exception, because it is the one provider iterion does
bridge. It is OAuth-only — pi reads it from an agent directory and there is no
environment variable to pass it by, unlike the ~30 API-key providers — so
iterion seeds a throwaway agent dir from your Codex credential for the node's
lifetime and deletes it afterwards.

**That means a live access *and refresh* token is on a filesystem the agent
process can read**, for as long as the node runs. It is structural, not a choice
of directory: driving pi's `openai-codex` provider at all requires the file, and
an agent under prompt injection has shell access. Two mitigations, both
deliberate choices you make per node:

- point the node at an `OPENAI_API_KEY` model instead of `openai-codex/…` when
  it runs against a repository you do not control;
- set `ITERION_FORBID_SUBSCRIPTION_OAUTH=1` to refuse the bridge outright.

iterion writes the seed `0600` inside a `0700` directory and keeps it
unstageable by git, so it is not committed or diffed by accident — but those
guard against mistakes, not against an agent that goes looking.

#### Environment variables

| Variable | Effect |
|---|---|
| `ITERION_PI_BIN` | Absolute path to the `pi` binary (e.g. a `bun --compile` single-file build on a host with no Node). |
| `ITERION_PI_MODE` | `print` rolls back to the one-shot transport. The default is the long-lived `--mode rpc` session (tool events, native steering, authoritative accounting, pre-flight handshake). |
| `ITERION_PI_AGENT_DIR` | Pins `PI_CODING_AGENT_DIR`. Reproducible pi config, but hides the operator's own `auth.json` — so the OAuth breadth above goes with it. |
| `ITERION_PI_OFFLINE` | `0` re-enables pi's catalogue refresh inside a sandbox (off by default there: an egress policy would stall startup). |
| `ITERION_PI_TRUST_PROJECT` | `1` trusts the target repo's `.pi/` resources. See the warning above. |

### `kimi` (Moonshot kimi-code)

Moonshot's **`kimi-code`** takes the prompt as `-p <prompt>`
(`kimi --print …` errors with `unknown option '--print'`):

```
kimi -p <prompt> --output-format {text,stream-json} [-m <alias>]
```

```yaml
agent implement:
  backend: "kimi"
  model: "kimi-code/kimi-for-coding" # complete kimi-code alias is preserved
  system: "…the task…"
```

### `grok` (xAI Grok Build CLI)

xAI's **Grok Build** coding agent (`grok` on `PATH`, typically installed
under `~/.grok/bin/grok`) is headless via `-p` / `--single`, not
claude-code Session mode:

```
grok -p <prompt> --output-format json \
     --permission-mode bypassPermissions --always-approve \
     [-m <model>] [--rules <system>] [--reasoning-effort <level>]
```

```yaml
agent implement:
  backend: "grok"
  model: "grok-4.5"          # or grok-4.5-build, grok-3, …
  # model: "xai/grok-4.5"    # also fine — the xai/ prefix is stripped
  system: "…the task…"       # delivered as --rules (appended to Grok's native prompt)
  reasoning_effort: high     # optional; mapped to --reasoning-effort
```

The node's `system:` is passed as **`--rules`** (append), never as
`--system-prompt-override`, so Grok keeps its native agentic baseline
(tools, plan, posture). Credentials come from the CLI itself (Grok Build
login / `~/.grok` config) — iterion does not inject `XAI_API_KEY` for this
backend. That is **distinct** from calling the xAI HTTP API via
`backend: claw` + `model: "xai/…"`.

### Behavioural notes (all CLI-agent backends)

- **Explicit opt-in.** `kimi` / `grok` are never auto-detected; set
  `backend: "kimi"` or `backend: "grok"`. No host credential silently
  re-targets a run away from `claude_code` / `claw`.
- **Credentials** are resolved by the CLI itself from its own env/config
  (e.g. `$MOONSHOT_API_KEY` for kimi, Grok Build OAuth for grok) — iterion
  inherits the host environment and does not fight the CLI's native
  resolution.
- **Tools are not host-gated.** Like `codex`, these CLIs run with their
  *own* built-in toolset; a node's `tools:` list is advisory. For a hard
  tool-permission boundary use `claude_code`, `claw`, or pi in RPC mode; all
  three enforce the shared permission gate.
- **Effort:** kimi has no dial (ignored); grok maps `reasoning_effort` to
  `--reasoning-effort` (`ultracode` degrades to `high`).
- **Sessions** are captured for observability (`sessionId`) but resume/fork
  is not yet wired for CLI-agent backends.

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

## Client identity (User-Agent + custom headers) — claw backend

Every claw provider (anthropic, openai, vertex, foundry) sends an
honest **`User-Agent: claw-code-go/<version>`** by default. Before
2026-07 no UA was set at all, so Go's `Go-http-client/2.0` leaked onto
the wire — the worst possible fingerprint against endpoints that gate
service on the calling tool (z.ai's Coding Plan risk-control flags
"SDK-based access" traffic; repeated violations can suspend the
account). Two exceptions: the **ChatGPT-OAuth** path keeps its
protocol-required `codex_cli_rs/<codex version>` identity, and
**bedrock** requests carry the aws-sdk UA (SigV4-signed).

Override precedence (first non-empty wins), identical on the
in-process claw path and the sandboxed `__claw-runner` (the three env
vars below are forwarded into the container):

1. `ITERION_LLM_USER_AGENT` — iterion surface, injected into every claw
   provider factory ([pkg/backend/model/registry.go](../pkg/backend/model/registry.go)
   `withClientIdentity`).
2. `CLAW_USER_AGENT` — claw's own env override (works for any
   claw-code-go embedder).
3. The per-path default (`claw-code-go/<version>`, or
   `codex_cli_rs/<version>` in ChatGPT-OAuth mode).

For arbitrary headers, claw honours **`ANTHROPIC_CUSTOM_HEADERS`** with
Claude Code's exact semantics: newline-separated `Name: Value` pairs,
applied **last** on every request so they can override any default
header — including the User-Agent:

```bash
# ~/.iterion/env — identify as a specific tool against a gated endpoint
ITERION_LLM_USER_AGENT="my-tool/1.0"
# or arbitrary headers, Claude Code style (overrides UA too):
ANTHROPIC_CUSTOM_HEADERS="User-Agent: my-tool/1.0
X-My-Header: value"
```

A malformed `ANTHROPIC_CUSTOM_HEADERS` line (no `Name:` part) fails the
request with an explicit parse error rather than being silently dropped.

**ToS caveat.** Presenting as another tool is a decision between you
and the endpoint you target (e.g. your z.ai subscription's supported-
tools policy) — iterion/claw default to the honest identity and never
spoof on their own. It changes nothing about the Anthropic subscription
path above: that works on its own merits (billed to extra usage), and no
User-Agent you configure affects how it is billed.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| Pill is red | No credential detected | Set `ANTHROPIC_API_KEY` or sign in to Claude Code, then click Refresh |
| Pill is green but Run errors out with "no provider" | Workflow uses `model: openai/...` but only `ANTHROPIC_API_KEY` is set | Switch model to an Anthropic spec, or add `OPENAI_API_KEY` |
| Pill says "claude_code" but you wanted "claw" | OAuth is found and ranked first | `export ITERION_BACKEND_PREFERENCE='claw,claude_code'` |
| Pill says "claw" but a legacy workflow needs Codex | Codex is deprecated and absent from the default order | Prefer `claw` + an OpenAI model; for compatibility, select `backend: codex` explicitly and ensure `$CODEX_HOME/auth.json` exists |
| Editor pill stale after fixing env | Server cache (30s) | Click the pill → **Refresh** |
