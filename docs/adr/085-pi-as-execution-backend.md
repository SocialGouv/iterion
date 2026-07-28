# ADR-085: pi as an execution backend

- **Status**: Accepted
- **Date**: 2026-07-27
- **Code**: [pkg/backend/delegate/pi.go](../../pkg/backend/delegate/pi.go) (`BackendPi`, `piProtocol`, `PiBackend`), [pkg/backend/delegate/pisdk/](../../pkg/backend/delegate/pisdk/) (ported client surface), [pkg/backend/delegate/cliagent.go](../../pkg/backend/delegate/cliagent.go) (`ExtraArgsFor`, `ParseOutputRich`, `SystemPromptViaFile`)
- **Supersedes nothing; extends** [ADR-065](065-dedicated-cli-agent-backend.md) (the CLI-agent seam) and [ADR-061](061-per-backend-system-prompt-composition-mode.md) (system-prompt composition mode)

## Context

iterion's backend fleet has a shape problem at the edges. `claw` is the
in-process path and reaches ~7 providers through a hand-maintained model
registry; claw-code-go's own `docs/ROADMAP-2026H2.md` describes its scope as
"functionally finished (parity 32/33 COMPLETE) but nothing proves it", and its
README self-qualifies as *"Experimental — most features not manually
validated"*. `claude_code` is the full-capability path but is Anthropic-shaped.
`kimi` and `grok` reach exactly one vendor each. There is no backend that
answers "run this node on whatever model is cheapest / sovereign / available
today".

[pi](https://pi.dev) ([earendil-works/pi](https://github.com/earendil-works/pi))
is a released multi-provider agent harness — npm plus native single-file
binaries, CI, pinned dependencies, shrinkwrap, a scheduled `npm audit` — whose
`packages/ai` registers ~36 first-class providers behind one agent loop, with an
auto-refreshing model catalogue and its own OAuth flows (Anthropic, OpenAI
Codex, GitHub Copilot). It also exposes something no CLI iterion drives today: a
bidirectional JSONL control plane (`pi --mode rpc`) with steer/abort/compact/
fork/session-stats and an extension UI channel.

Four motivations, in the order they matter:

1. **Provider breadth and cost.** One backend that reaches models no other one
   can, and that reports what the call actually cost.
2. **A maintained alternative to claw**, for the paths where claw's
   unvalidated status is a risk.
3. **Superior runtime control**, via the RPC transport.
4. **Vendor diversity** — an agent harness not owned by a model vendor.

## Decision

Ship pi as `backend: "pi"`, staged.

### One backend name, transport upgraded transparently

Both transports ship under the **same** backend name, selected by `PiBackend`.
The RPC session is the default; `ITERION_PI_MODE=print` is the operator's
rollback.

Print vs RPC is a *transport*, not a contract: every observable — `Result`
fields, hook firing, error types, `SystemPromptMode`, model/effort mapping,
sandbox routing — is identical by construction because both reuse
`piResolveModel` / `piMapEffort` / `piExtraArgsFor` / `piResolveEnv`. Only
fidelity differs, and strictly upward. A second backend name (`pi_rpc`) would
cost ~12 string-site edits — registry, queue enum, effort endpoint, two CLI
help strings, four studio files, detection, docs, diagnostics — for zero
authoring value, since a workflow author would have nothing to choose between.

### Port pi's official client surface rather than decode ad hoc

`pkg/backend/delegate/pisdk/` is a faithful Go port of the client surface pi
publishes (`RpcClient` and the RPC types are exported from its package entry
point; the protocol is specified in `packages/coding-agent/docs/rpc.md`),
pinned to a named upstream commit. This mirrors the existing
[`claudesdk/`](../../pkg/backend/delegate/claudesdk/) precedent.

The alternative — walking `map[string]any` from the outside — was rejected
because the expensive part of this integration is not the wire format but the
*semantics*, and those are written down upstream: that the `prompt` response
fires at preflight while `agent_settled` is the real completion boundary; that
commands are dispatched unserialised so responses must be correlated by id;
that pi gates stdout on reader backpressure, so a slow consumer stalls the
agent; that closing stdin is the graceful-shutdown signal; that framing is
LF-only because U+2028/9 are legal inside JSON strings. Rediscovering those by
experiment is exactly the cost the port avoids, and the print-mode parser
needs half of the same types anyway.

### Composition: append, never replace

`SystemPromptModeForBackend("pi") → SystemPromptAppendToNative`. pi assembles a
native agentic prompt **from the active tool set** and loads `AGENTS.md` +
`CLAUDE.md` and skills on its own; `--system-prompt` would replace all of it.
The node's task goes to `--append-system-prompt`, as a **file** — a composed
prompt (posture + cursors + skills + preset) can exceed `MAX_ARG_STRLEN`, and a
real path removes the text-vs-path ambiguity of a flag that accepts both.

iterion's `agenticOperatingPosture` is deliberately **not** prepended. It is
the `SystemPromptAuthoredBase` substrate for `claw` precisely because
claw-code-go is a bare API client with no prompt of its own. pi is not, and
stacking the two would duplicate read-before-edit / plan-then-act /
converge-and-stop in two different wordings.

### Refuse the target repository's `.pi/` directory

iterion passes `--no-approve`. pi executes project-local extensions as
TypeScript **inside the agent process** — the process holding the run's
credentials — so trusting a checked-out repository turns prompt injection into
code execution. This is a sharper boundary than the equivalent for
`claude_code`, which does not execute repository TypeScript at startup.
`ITERION_PI_TRUST_PROJECT=1` opts back in, per node.

### ⚠️ Anthropic subscription credentials — the original reasoning was wrong

**This section's original decision (a blanket refusal) is superseded by
evidence from Anthropic itself, and the code now permits by default.** The
original reasoning is kept below, marked, because it is instructive about how a
plausible reading of vendor terms can be wrong.

Driving `backend: "pi"` with a Claude subscription OAuth token against
`anthropic/claude-haiku-4-5` produced, from Anthropic's own API:

```
400 invalid_request_error
"Third-party apps now draw from your extra usage, not your plan limits.
 Add more at claude.ai/settings/usage and keep going."
```

That is not a policy refusal. Anthropic **accepts** the token from a
third-party app and bills it against a *separate extra-usage balance* instead
of the plan's limits — a productised, supported path. The 400 only means that
balance is empty. So the premise of the refusal below ("reusing the
subscription outside the official CLI is out of policy") does not hold in the
strong form it was written in; the vendor has since drawn the line at
*billing*, not at *which client*.

**Decision taken on that evidence: permit by default, warn, and let the
operator refuse.**

- `secrets.SubscriptionOAuthOnly` detects the condition;
  `secrets.SubscriptionOAuthNotice` is the shared warning text; both `pi` and
  `claw` log it per node. The old `GuardThirdPartyOAuth` /
  `ErrOAuthForfaitInThirdParty` pair is gone.
- **`ITERION_FORBID_SUBSCRIPTION_OAUTH=1`** restores the refusal, for both
  backends at once. The opt-out exists because on a shared or cloud instance,
  spending an operator's extra-usage balance is a cost decision taken on
  behalf of everyone using it — that should be closable.
- pi no longer strips `ANTHROPIC_OAUTH_TOKEN` / `CLAUDE_CODE_OAUTH_TOKEN` /
  `CLAUDE_CONFIG_DIR` from its environment. With the refusal gone the strip
  would only break a working credential path.
- The empty-balance condition is a **legible** error naming the cause and the
  three ways out, instead of an opaque 400 that reads like a bad token.
- `claw` gained a logger (`model.WithClawLogger`) because its `EventHooks`
  surface had no channel for an operator-facing warning.
- `claude_code` and `codex` are untouched: they spawn the vendor's CLI, which
  draws on the plan normally.

### Original decision (superseded): refuse the Anthropic subscription forfait

*(Historical. The code described here no longer exists — `piGuardForfait` and
`secrets.GuardThirdPartyOAuth` were removed.)*

A guard failed any pi node whose only Anthropic credential was the stored
Claude Pro/Max OAuth forfait, and `piResolveEnv` stripped
`ANTHROPIC_OAUTH_TOKEN` / `CLAUDE_CODE_OAUTH_TOKEN` / `CLAUDE_CONFIG_DIR`
unconditionally so an inherited value could not reach pi on any provider path.

Anthropic's Consumer Terms scope the subscription to the official Claude Code
CLI surface. `claude_code` and `codex` are exempt because they spawn that CLI,
which remains the authorised consumer; pi speaks the Messages API directly, so
the exemption does not transfer — and the subprocess-vs-in-process distinction
is irrelevant, since the guard is about *which client the credential
authorises*.

This was written as a legal determination pending confirmation, on the
reasoning that a conservative default is cheap to relax and expensive to
retrofit. **The confirmation arrived and went the other way** — see the
superseding section above. The remaining open questions are pi's
`openai-codex` (ChatGPT plan) and `github-copilot` OAuth providers, where
iterion still takes no position and injects nothing. A user's own `pi` login
in `~/.pi/agent/auth.json` is their relationship with the vendor — iterion
reads and injects nothing there.

### Three additions to the CLI-agent seam

ADR-065's protocol could not express pi. Three fields, all of which retro-benefit
`kimi` and `grok`:

- **`ExtraArgsFor(task)`** — per-task argv (session ids, skill paths,
  sandbox-dependent switches) without needing a bespoke `Backend`.
- **`ParseOutputRich`** returning `CLIAgentParse` — a real input/output token
  split, a provider-computed USD cost, effective model, context window, and a
  **typed `Err`** for a CLI that renders a failed run on a *zero* exit code.
- **`SystemPromptViaFile`** — see above.

`cost.AnnotateWithUSD` accompanies them: when the CLI reports an authoritative
cost it wins over iterion's estimate table. Until now
`cliagent.go` booked *every* CLI-agent token at the output rate with a zero
input count, so `max_cost_usd` and the spend cap were systematically wrong on
those backends.

## Trade-offs / Consequences

**Accepted, and documented in [backends.md](../backends.md):**

- **pi's tool set is a strict subset of claude_code's** (`read, bash, edit,
  write, grep, find, ls`). Moving a workflow that already runs on
  `claude_code` to pi is more work for less capability. pi's value is the
  models claude_code cannot reach — stated plainly so the backend is not
  adopted for the wrong reason.
- **No MCP client at all.** Board `capabilities:`, `ask_user`/async
  interaction, and workflow `mcp_server` blocks are all served over MCP and
  therefore do not reach a pi node. This is the single largest gap and the
  one most likely to be underestimated.
- **`__ITERION_SECRET_*__` placeholders are not materialised.** File secrets
  work unchanged. A future diagnostic should flag the combination.
- **Tools are not host-gated** (ADR-065's standing consequence). The one
  exception iterion enforces is `readonly:` → `--tools read,grep,find,ls`.
  Mapping iterion's `tools:` names onto pi's built-ins was rejected: the name
  spaces are disjoint, and a partial mapping would silently disable `bash` on
  any node listing iterion names.
- **`ultracode` is inexpressible** — pi has no subagent tool; it degrades to
  `xhigh`.
- **pi auto-retries inside iterion's retry loop** (3 attempts by default),
  invisibly to the rate-limit classifier **and to the cost accounting** — only
  the last attempt's transcript survives, so a node that made four billed
  calls reports one. Mitigated by a WARN naming the retry count; print mode
  has no lever to disable it, and the RPC transport will send
  `set_auto_retry:false`.
- **`AGENTS.md` is read alongside `CLAUDE.md`** — a behavioural difference
  from every other backend.
- **Auto-detection excludes pi.** `DefaultPreferenceOrder` stays
  `{claude_code, claw}`; adding pi would silently change every existing
  empty-backend workflow. Opt in via `ITERION_BACKEND_PREFERENCE`.
- **Model patterns are fuzzy-matched by pi**, so a typo runs a different model
  rather than failing. Mitigated by surfacing `Result.EffectiveModel` and
  warning when it differs from the request — a mitigation, not a fix.
- **A weekly 0.x upstream.** The port is pinned to a commit and the provenance
  list in `pisdk/doc.go` is the maintenance contract.

**Rejected alternatives:**

- *A bespoke `PiBackend` not reusing `CLIAgentBackend`* — would duplicate the
  sandbox pidfile-kill plumbing, the retry loop, stderr line-logging, and the
  schema-aware `parseSDKOutput` fallback.
- *`-p <prompt>`* — pi's argv parser silently drops a message beginning with
  `-` or `@`, and an iterion prompt routinely starts with a markdown bullet.
  Prompt-on-stdin is mandatory, and it also bounds ARG_MAX and makes pi's
  read-stdin-to-EOF behaviour benign.
- *Trusting `.pi/` by default*, *pinning `PI_CODING_AGENT_DIR` by default*
  (it would hide the operator's `auth.json` — the credential breadth that
  motivates the backend), and *`~/.pi/agent/sessions` for run sessions*
  (concurrent nodes would collide and pruning a run would not reclaim them).

## Validation against the real binary

`pi_smoke_test.go` drives the **real `pi`** end to end with no credentials, no
network and no cost: a test-only pi extension
([pisdk/testdata/mock-provider.ts](../../pkg/backend/delegate/pisdk/testdata/mock-provider.ts))
uses `pi.registerProvider` to register a model that replays a scripted
response. It skips when `pi` is absent, so it is free for anyone without it and
automatic drift detection for anyone with it.

This exists because `pisdk/` is a *port*, and only a stream pi actually
produced can tell you the port still matches — a hand-written fixture cannot
notice a renamed field. Four things it found that reading the source had not:

1. **`{"type":"message_start","message":{}}`** — pi emits an **empty** message
   object at the start of an assistant turn. Decoded as a zero `Message` and
   filtered by role; harmless, but no hand-authored fixture would have
   contained it.
2. **Exit code 0 on a fully failed run** — confirmed live, including through
   pi's exhausted retry loop. `stopReason` really is the only failure signal.
3. **pi's internal retry loop, and its cost consequence.** A scripted 429 made
   pi retry 3 times over ~14s of backoff. Only the last attempt's transcript
   survives in `agent_end`, so the accounting derived from it is short by the
   discarded attempts. This produced `CLIAgentParse.Notices` (logged at WARN),
   because otherwise the operator sees an unexplained slow node with a
   suspiciously low cost.
4. **A real classification bug.** The first implementation reused
   `isRateLimitMessage`, and a plain `rate_limit_error: 429 too many requests`
   did not match it — so a throttle was typed as a permanent failure,
   skipping both retry and provider fallback. The detector is *deliberately*
   narrow because for `claude_code` it scans untrusted assistant **prose**
   (its own comment records dropping `rate_limit_error` because
   security-audit agents write about rate limits). But pi's `errorMessage` is
   structured metadata only the runtime writes, so the narrow list was the
   wrong tool. Classification now prefers the upstream HTTP status pi records
   in `diagnostics[].error.code` (`Message.HTTPStatus()`), with a broader
   text fallback that is safe precisely because the input is structured.

### Real model round-trip (z.ai / GLM-5.2)

Completed through the whole chain — DSL → IR → runtime → `PiBackend` →
`CLIAgentBackend` → pi → `pisdk` → `Result` — against `zai/glm-5.2`. It
validated the provider-computed cost path and `piResolveModel`'s z.ai
handling, and surfaced three things:

1. **Context files are the dominant per-call cost, by a factor of sixty.**
   A one-word prompt on iterion's own tree costs **26,933 input tokens with
   context files against 448 without** — pi injects the repo's 103 KB
   `CLAUDE.md` on *every* call. It stays on for `claude_code` parity;
   `ITERION_PI_NO_CONTEXT_FILES=1` is the new off switch. Any budget
   estimate for a pi node that ignores this is wrong by an order of
   magnitude.
2. **A node with no `user:` prompt silently does nothing.** pi with an empty
   prompt emits its session header and exits without a turn — 0 tokens, no
   error. Where `claude_code` would still run, pi no-ops, and the failure
   presents as a structured-output validation error two steps later.
3. **The reported node cost is the LAST attempt's, not the sum.** A run whose
   first attempt spent 26,997 tokens and whose retry spent 106 reported 106.
   This is iterion's general retry accounting, not pi-specific, but it
   compounds the pi-internal-retry under-reporting above — flagged, not
   fixed here.

### The RPC transport (`ITERION_PI_MODE=rpc`)

Landed and validated live against `zai/glm-5.2` through the full chain, plus
credential-free tests against the real `pi --mode rpc` (handshake, settle,
hooks, failure typing, cancellation, equivalence with print). What it adds over
print mode — none of it available on any other iterion CLI backend:

- **Tool events** (`tool_execution_start/end` → `TaskHooks`), so the studio
  timeline and files-touched panel finally populate for a CLI backend.
- **Native steering**: `task.InboxDrain()` → pi's `steer`, where `claude_code`
  fakes the same thing with a PostToolUse `AdditionalContext` plus a
  Stop-blocking hook.
- **Authoritative accounting** from `get_session_stats`, incl. context usage.
- **A pre-flight handshake**: `get_state` resolves the model and session id in
  ~200 ms, before a token is spent.
- **Abort on cancellation** instead of killing a process mid-call, so a
  partial transcript still lands.

**RPC is the default.** `ITERION_PI_MODE=print` is the rollback.

Getting there required fixing a real accounting bug the first RPC version
shipped with. It folded `get_session_stats`'s cache reads and writes into the
input figure, so the same trivial turn reported 550 tokens on print and 2078 on
RPC. An earlier draft of this ADR called RPC's number "the more complete one" —
that was wrong. `claude_code` sums `Usage.InputTokens + OutputTokens` and routes
`input + cache_creation + cache_read` to the context gauge instead, so
**excluding cache from the billed count is the convention**, and RPC was simply
inconsistent with it. A workflow's `max_tokens` budget has to mean the same
thing whichever backend ran the node.

RPC now matches, cache load goes to `PeakInputTokens` where it belongs, and
`TestPiRPCLiveEquivalence` asserts token and cost equality across transports —
that assertion is what gates the default staying on RPC.

### The sandboxed path

Validated in a real Docker container (`zai/glm-5.2`, correct answer, no leaked
container or host process afterwards). It required two fixes and one image
change:

- **`pi` is now in the baked CLI set** (`docker/llm-clis/package.json` +
  a symlink in `sandbox/finalize/Dockerfile`), alongside `claude` and `codex`.
  Without it a sandboxed pi node cannot start at all.
- **Host provider credentials were invisible inside the container.** A
  container inherits nothing, and the sandbox `ExecOpts.Env` carried only the
  per-run override map — so a node failed with `No API key found for zai`
  while the host had the key. `CLIAgentProtocol.SandboxEnv` now forwards pi's
  credential variables **by name** (an explicit allowlist, not a blanket
  `os.Environ()` push that would leak every unrelated host secret).
- **`task.ExtraEnv` was dropped on the sandboxed path** for every CLI-agent
  backend, not just pi: a sandboxed agent could not see tools the run had just
  provisioned for it via devbox. Fixed in the shared helper, so `kimi` and
  `grok` benefit too.

**Still not validated:**
- **The fuzzy-model-match warning.** `Result.EffectiveModel` differing from
  the request is logged, but no run has yet resolved a typo'd model, so the
  warning has not fired in anger.
- **Session resume/fork.** `--session-id` / `--fork` are emitted and
  argv-tested; no run has actually resumed a pi session.
- **Anthropic through pi.** Reached the API and was refused for an empty
  extra-usage balance (see above), never for a policy reason. A successful
  Anthropic round-trip through pi remains unproven.

### The iterion pi extension

`pi-extension/` is a TypeScript package bundled by esbuild into a single file
with no runtime imports, committed as `pkg/backend/delegate/piext/asset/`,
embedded in the binary, and loaded as `pi -e <path>`. It closes the gap between
pi's deliberately small surface and what iterion workflows declare.

Why each of those choices:

- **Embedded, not installed.** The extension's version is structurally locked
  to the engine that drives it — same commit, same binary — so the
  Go↔extension contract cannot skew. `pi install` would mutate the operator's
  own pi configuration and re-resolve npm at every start; `-e npm:…` needs
  network at run start, fatal under a sandbox egress policy.
- **`-e`, not `.pi/extensions/`.** pi's project-trust gate silently ignores
  project extensions in non-interactive modes, so that vector would never load
  and never say so.
- **A single bundle with externals.** `@earendil-works/*` and `typebox` are
  marked external because pi's loader aliases those specifiers to its own
  bundled copies; a second typebox would make structurally-identical but
  instance-distinct schemas. Everything else is inlined, because there is no
  `node_modules` inside a sandbox.
- **A contract version on both sides.** On a mismatch the extension registers
  **nothing** and notifies loudly. Half a permission gate is worse than none,
  because the operator would believe they had one.
- **`task pi-ext:check` fails on a stale asset.** The asset is committed, so
  without that check a forgotten rebuild would silently ship yesterday's
  extension on every run.

**Shipped: the permission gate, `ask_user`, and MCP-over-HTTP bridging.** pi has no permission system at all, so a
workflow's `permission: ask|deny` block was silently inert on a pi node. The
`tool_call` hook forwards each call over the control channel; the decision is
made in Go by the **same `permission.Policy`** that drives claude_code's
PreToolUse hook and claw's gate, because a second implementation of the rule
parser and glob matcher in TypeScript would drift the first time either
changed. Fail-closed on no answer: a gate that fails open is worse than a
failed tool call. `ask` is reported as an escalation (the extension has no
operator to ask) and blocks meanwhile.

Verified against the real binary: pi calls a tool, the gate fires, and the
model receives `bash is denied by this workflow's permission policy`.

**`ask_user`** gives a pi node a way to reach a human, which it had none of —
pi is a headless process with no operator attached, so `interaction: human` was
inert. The tool does not answer the question: it hands it to iterion, which
suspends the RUN, persists an interaction the studio renders, and resumes with
the operator's reply. That is the only shape that works, because the answer may
arrive minutes later from a different process, so the tool cannot block on it.

Two details that matter more than they look:

- **What the model is told.** Not "error", but "the run is now paused, the
  question is with a human, the conversation resumes with their reply". An
  agent handed a bare error re-asks in a loop.
- **The tool is registered only when the node can actually reach someone**
  (`ITERION_PI_INTERACTION`). Offering it otherwise would let the agent call it
  and stall against a pause nobody will resolve.

A permission `ask` uses the same suspension path, carrying a
`permission.Marker` so the studio renders an approval card rather than a
question.

**MCP over HTTP, which is what makes board capabilities reachable.** pi has no
MCP client, so every MCP surface iterion built was invisible to it. The
extension carries a small JSON-RPC-over-HTTP client (hand-rolled rather than
pulled from the MCP SDK: the bundle must stay dependency-free to load inside a
sandbox with no `node_modules`), discovers tools via `tools/list`, and
registers each one on pi.

Nothing about the board is hardcoded — the server stays the source of truth for
what exists and what it accepts, so a new board operation needs no change in
the extension. Tool names keep iterion's `mcp__<server>__<tool>` shape, which
is load-bearing: the permission layer treats that namespace as infrastructure
and exempts it, so renaming would make `permission: ask` pause the run on the
very tools used to talk to the board.

The board is offered **only** when the run has capabilities *and* the endpoint
and token are wired. Registering it otherwise would hand the agent tools that
fail on every call — worse than absent, because the model burns turns
discovering they do not work. Bridging happens on `session_start` and a server
that is unreachable costs its own tools, not the session.

Verified against a real HTTP MCP server: handshake, `tools/list`, `tools/call`
with the `X-Iterion-Run` header, and the result reaching the model.

**The control channel.** pi's extension-UI protocol is a *closed* union, so the
channel tunnels through two of its members — `ctx.ui.input` for
request/response, `ctx.ui.notify` for one-way. No listener, no port, no token,
and it works identically inside a sandbox where a network callback would have
to cross the container boundary. Every payload carries `__iterion`, because the
UI channel is **shared** with any other extension the operator installed: without
the marker a hostile or buggy extension could fabricate a permission verdict. An
unmarked request is cancelled — its documented safe default — with a warning.

## Follow-ups

- **Extension, remaining**: async questions (`ask_user_async` /
  `await_answers`), stdio and SSE MCP transports for third-party servers (the
  HTTP half is done and is the reusable core), and Claude-Code tool aliases.
  A workflow declaring a non-HTTP `mcp_server:` should still use
  `backend: "claw"`.
- Diagnostics for the two silent gaps: `mcp_server` on a pi node, and
  placeholder secrets on a pi node.
- A successful Anthropic completion through pi (blocked only on an extra-usage
  balance, never on policy).
