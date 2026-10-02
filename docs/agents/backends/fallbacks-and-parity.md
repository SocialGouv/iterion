# Fallbacks, schema re-ask and system-prompt parity

Read it when you write `fallbacks:`, hit C173/C176, a node acts "dumber"
than its native harness, or a forfait route degrades. ADR-087 + ADR-091
are the decisions; [docs/backends.md](../../backends.md) the reference.

**Cross-backend fallback (`fallbacks:`).** A node may declare ordered,
named alternative **routes** (backend + model + credential hint) taken
when its primary fails — the case `provider:` cannot serve: a CLI
backend whose subscription forfait has shut, continuing on a metered API
through `claw`. `on:` filters which failure routes where (default
`[usage_window, unavailable]`; never `any`, never `auth` by default). A
fall-through is deliberately **loud**: a `model_fallback` event,
`_backend`/`_model` naming what actually *served*, and
`_fallback_used`/`_served_by` so a deterministic gate can fail closed on
a degraded input. THREE crossings are compile-time errors (C176): a route
that cannot enforce the node's `permission:` gate; a claw⇄CLI
crossing on a node with an empty `tools:` list (the list inverts meaning
across that boundary); and a **backend change on a node that keeps its
conversation** (`session: inherit` / `inherit_if_available` / `fork` —
`sessionContinuityCrossingReason`), since session continuity has no
cross-backend meaning. The third is why a node that must survive a
fall-through WITHOUT losing its thread builds its ladder from different
*providers* inside ONE backend rather than from different backends —
Copi's claw ladder is the shipped example. On any route, an answer that
fails the node's `output:` schema on a fixable shape (a missing required
field, text instead of JSON — the fallback model did not write the
primary's schema habits) gets ONE re-ask with the validation error, in the
conversation (claw) or session (claude_code/codex/pi) it came from, before
the node fails; `delegate_retry.reask` names the mode, and the re-ask's own
`delegate_*` events carry `attempt: 2`
([the schema re-ask](../../backends.md#a-schema-invalid-answer-gets-one-more-turn-the-schema-re-ask)).
Two route properties extend the chain (ADR-091):
`action: skip` is a TERMINAL degrade — the node completes with a
zero-value output stamped `_skipped` instead of failing the run (the
"continue and ignore" half of an optional-peer policy; "pause and
retry" = don't declare it, the failure stays resumable for the
usage-window retry) — and `when:` gates any route on an expr over vars,
so one node expresses both policies picked per run by a `--var`. C173
guards both. See
[ADR-087](../../adr/087-cross-backend-model-fallback-chain.md) +
[ADR-091](../../adr/091-fallback-skip-route-and-plan-peer-review.md) +
[docs/backends.md](../../backends.md).

**Auto-detection.** When neither the node (`backend:`) nor the workflow (`default_backend:`) names a backend, and `ITERION_DEFAULT_BACKEND` is unset, the resolver in [pkg/backend/model/executor.go:resolveBackendName](../../../pkg/backend/model/executor.go) probes the host for credentials (Claude Code OAuth, ANTHROPIC_API_KEY, OPENAI_API_KEY, AWS, GCP) and picks the first match in `ITERION_BACKEND_PREFERENCE` (default `claude_code,claw`; other CLI backends, including codex, are explicit opt-ins). When `model:` is also empty and the resolved backend is `claw`, the runtime substitutes a sensible model spec for the first available provider. The studio surfaces the live detection via the toolbar BackendStatusPill and disables Run when no credential is found. See [docs/backends.md](../../backends.md).

**System-prompt composition (adaptivity parity).** A node's `system:`
prompt is the *task*, never the whole operating posture. How it composes
with the agentic baseline differs by backend, and getting this wrong is
exactly what made iterion-via-Claude-Code feel dumber than native Claude
Code:
- **claude_code** — iterion passes the assembled prompt via
  `--append-system-prompt`, **never** `--system-prompt`. Replacing would
  strip Claude Code's native system prompt (TodoWrite/plan-before-act/
  read-before-edit/parallel-tool/`file:line`/refusal posture); appending
  keeps it as the base. iterion also emits `--setting-sources` on every
  spawn so the target repo's settings are honoured — the scopes are the
  node's `ambient_context:` policy (ADR-119, `workspace` by default: the
  repo's memory and settings, not the operator's personal setup), with
  `ITERION_CLAUDE_CODE_SETTING_SOURCES` as the raw override. MCP is the opposite —
  `--strict-mcp-config` makes the node's resolved MCP set (`mcp_server:`/
  `mcp:` blocks, repo `.mcp.json`, iterion's ask_user/board servers)
  authoritative: the operator's personal `~/.claude.json` servers never
  boot inside a bot node (`ITERION_CLAUDE_CODE_STRICT_MCP=0` restores
  inheritance). Tool restriction: under the
  always-on `--permission-mode bypassPermissions`, `--allowedTools` does
  **not** gate the toolset — claude_code nodes always have the full native
  toolset (a node's lowercase `tools:` list is a no-op here; the real
  hard-restrict flag is `--tools`, deliberately unused to preserve
  adaptivity). Async subagents / workflows: the session stays open until
  that work comes back (a background shell does not hold it), and a report
  written before it did is not the node's answer (save the one a spent
  budget's wrap-up asks for) — a node that "stops too
  early" or re-does its audit every pass is the thing to check against
  [backends.md](../../backends.md) ("Background work"), `delegate_background`
  events, the checkpoint's `session_ledger` and
  `ITERION_CLAUDE_CODE_BACKGROUND_*`.
- **claw** — claw-code-go is a bare API client with **no** native system
  prompt, so iterion prepends an authored `agenticOperatingPosture` base
  (the parity substrate) before the node's `system:` text. A node's
  `tools:` list **does** restrict claw (lowercase names are claw-native)
  — and is RESOLVED against the registry, so a name claw does not have
  fails the node at dispatch. `iterion validate` refuses it first (C135,
  claw only — on a CLI backend the list is inert, so an unknown name
  there is dead config, not a failure); the catalog of accepted names is
  [pkg/backend/toolcatalog](../../../pkg/backend/toolcatalog/toolcatalog.go), kept
  honest by a conformance test against the real registry. `list_files` /
  `run_command` / `git_diff` / `search_codebase` circulate in older
  examples and have never been registered — use `glob` / `bash` / `grep`.

The `bypassPermissions` note above describes the default (`permission:
off`). The opt-in **permission gate** (`permission: ask|deny`, see the
DSL section + [docs/permissions.md](../../permissions.md)) adds a
deterministic allow/deny/ask boundary on top — without changing
`--permission-mode` (under bypass, PreToolUse hooks still run and a hook
`deny` still blocks the tool, so the gate rides the existing hook
surface). It is the anti-prompt-injection counterpart that keeps a
hypnotized/injected agent from silently performing off-policy actions.

The mechanism is `delegate.SystemPromptMode` (Standalone | AppendToNative
| AuthoredBase), set per-backend by `SystemPromptModeForBackend`
([pkg/backend/delegate/delegate.go](../../../pkg/backend/delegate/delegate.go)).
This restores adaptivity **without** touching the convergence machinery —
the `agenticOperatingPosture` "converge and stop / don't re-litigate"
clause reinforces the asymptote, it does not gate it.

**OpenAI ChatGPT-forfait via claw.** When Codex CLI is signed in via "Sign in with ChatGPT" (`auth_mode: "chatgpt"` in `~/.codex/auth.json`), `claw` can reuse that OAuth token + account_id to drive OpenAI calls through `chatgpt.com/backend-api/codex` — billing against the user's ChatGPT Plus/Pro subscription instead of metered API calls. Precedence: `OPENAI_API_KEY` wins when both are present (explicit env var = deliberate); ChatGPT-OAuth activates when no API key is set, or when `ITERION_OPENAI_USE_OAUTH=1` forces it. `ITERION_OPENAI_USE_OAUTH=0` or any `OPENAI_BASE_URL` disables OAuth. The `version:` header (which OpenAI uses to gate model availability — e.g. gpt-5.5 requires codex-cli ≥ 0.130) is sourced from `ITERION_CODEX_VERSION` or `codex --version`. See the "OpenAI via ChatGPT forfait" section in [docs/backends.md](../../backends.md). The **Anthropic** subscription equivalent also works on `claw` (and `pi`) — set `ANTHROPIC_AUTH_TOKEN` with no `ANTHROPIC_API_KEY` — but Anthropic bills third-party clients against the subscription's separate **extra-usage** balance, not the plan's limits (verified 2026-07-28; supersedes an older note claiming the path was throttled to zero). iterion warns per node and `ITERION_FORBID_SUBSCRIPTION_OAUTH=1` refuses it — worth setting on a shared/cloud instance, where spending an operator's extra-usage balance is a decision taken for everyone. See **ADR-085**.

