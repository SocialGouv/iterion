# Ultracode

`reasoning_effort: ultracode` is the highest setting on the effort dial. It is
**not** an additional API effort value — Anthropic's Messages API only accepts
up to `xhigh`/`max` on the wire. Ultracode is a **mode**, mirroring Claude
Code's "Ultracode" level:

> **xhigh reasoning + a standing prerogative to orchestrate multi-agent workflows.**

It is delivered through prompt engineering (a standing-consent system
instruction) rather than a new wire parameter, and it is **reliable only on
`claude-opus-4-8` and the Claude 5 family (`claude-opus-5`,
`claude-fable-5-1`)** — the orchestration half is backed by Anthropic's
mid-conversation system messages, which ship on those models. On any other
model ultracode degrades gracefully to plain `xhigh`.

See Anthropic's reference: [Effort](https://platform.claude.com/docs/en/build-with-claude/effort)
and [Build an orchestration mode](https://platform.claude.com/docs/en/build-with-claude/mid-conversation-effort-example).

## What the runtime does

When a node declares `reasoning_effort: ultracode`, iterion:

1. **Sends `xhigh` on the wire.** The value is remapped via `model.wireEffort`
   before it reaches the provider, then coerced to what the model accepts
   (`xhigh` on Opus 4.8/4.7, `high` on OpenAI, etc.). The literal `ultracode`
   never reaches an LLM API.
2. **Grants the orchestration prerogative.** A `## Workflow Orchestration`
   section is appended to the system prompt giving standing consent to
   decompose substantial work across parallel subagents (via the `agent`
   tool) and to verify findings adversarially — without asking first.
3. **Makes the subagent tool available.** On the `claw` backend, the `agent`
   subagent tool is added to the node's allowlist when the node restricts its
   tools (an unrestricted set already exposes the claw builtins). The
   `claude_code` backend orchestrates through its native subagent mechanism.
4. **Warns off Opus 4.8.** Compiling `ultracode` on a model that isn't
   `claude-opus-4-8` emits diagnostic **C089** (a warning, not an error): the
   orchestration half won't be reliable and the node runs as plain `xhigh`.

Adaptive thinking is enabled automatically for Opus 4.8 by the claw backend
(`thinking: {type: "adaptive"}`), so ultracode gets extended thinking without
extra configuration.

## Usage

```iter
agent implementer:
  backend: "claude_code"
  model: "anthropic/claude-opus-4-8"
  reasoning_effort: ultracode
  system: build_system
  user: build_task
```

Ultracode fits **implementer/agent nodes** that benefit from spawning helpers
for independent sub-parts. It is wasteful on judges, routers, and simple
read-only nodes — use `high` or `xhigh` there, and keep cheap LLM routers on a
small model such as `anthropic/claude-sonnet-4-6`.

The value is also settable dynamically from an upstream node:

```iter
router -> implementer with {_reasoning_effort: "ultracode"}
```

and via env substitution, which is resolved (and re-validated) at runtime:

```iter
  reasoning_effort: "${ITERION_EFFORT:-ultracode}"
```

## Relationship to other dials

- **Effort vs. cursors.** Effort (including ultracode) controls compute and the
  orchestration prerogative. [Cursors](cursors.md) are qualitative framing
  dials (ambition/depth/rigor/autonomy) appended under `## Calibration`. They
  are orthogonal and compose freely.
- **Ultracode vs. routers.** Routers (`fan_out_all`, `llm`) orchestrate at the
  *workflow* graph level — the author wires the branches. Ultracode lets a
  single agent node orchestrate *dynamically* at run time by spawning
  subagents it decides it needs. Use both together for deep fan-out.

## Studio

The effort selector offers **ultracode** only when the node's model is
`claude-opus-4-8` or a Claude 5 model (the `/api/effort-capabilities` endpoint gates it server-side,
complementing the C089 compile warning). The `EffortBar` renders it full-bar in
a distinct accent tone, reading as "beyond max".
