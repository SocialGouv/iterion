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

Print mode (`pi --mode json`) ships first; the RPC transport lands later under
the **same** backend name, selected by `PiBackend`, with `ITERION_PI_MODE` as
the operator's pin.

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

### Refuse the Anthropic subscription forfait

`piGuardForfait` fails a pi node whose only Anthropic credential is the stored
Claude Pro/Max OAuth forfait, and `piResolveEnv` strips
`ANTHROPIC_OAUTH_TOKEN` / `CLAUDE_CODE_OAUTH_TOKEN` / `CLAUDE_CONFIG_DIR`
unconditionally so an inherited value cannot reach pi on any provider path.

Anthropic's Consumer Terms scope the subscription to the official Claude Code
CLI surface. `claude_code` and `codex` are exempt because they spawn that CLI,
which remains the authorised consumer; pi speaks the Messages API directly, so
the exemption does not transfer — and the subprocess-vs-in-process distinction
is irrelevant, since the guard is about *which client the credential
authorises*.

**This is a legal determination, not an engineering one, and it has not been
made.** The conservative default is deliberate: a later ruling that this is
permitted relaxes the guard, whereas the opposite would be a retrofit after
runs have already happened. The same open question applies to pi's
`openai-codex` (ChatGPT plan) and `github-copilot` OAuth providers; iterion
takes no position and injects nothing for either. A user's own `pi` login in
`~/.pi/agent/auth.json` is their relationship with the vendor — iterion reads
and injects nothing there.

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

**Not yet validated: a real model call.** No provider credential was available
on the validating host (`~/.pi/agent/auth.json` absent, no provider env keys,
and the one Anthropic credential present is the OAuth forfait this backend
refuses by design). Everything above exercises the real binary, the real event
stream and the whole Go path; what remains unproven is a genuine provider
round-trip — model resolution against a live catalogue, real usage figures, and
the fuzzy-model-match warning firing on a typo.

## Follow-ups

- The RPC transport, and with it: tool events into the studio timeline,
  `steer`-based inbox drain, session resume/fork, `get_session_stats`
  accounting, and the `extension_ui_request` channel — which is the first
  chance for a CLI backend to suspend *inside* a tool call, matching claw.
- A pi extension (TypeScript, in this repo, loaded with `-e`) restoring the
  permission gate, `ask_user`, board tools and MCP.
- Diagnostics for the two silent gaps: `mcp_server` on a pi node, and
  placeholder secrets on a pi node.
- **File the legal question** covering Anthropic, OpenAI Codex and GitHub
  Copilot subscription credentials, once, together.
