# Delegation

Iterion can mix lightweight in-process calls and full coding-agent CLIs in one
graph. `backend:` chooses the executor and its tool surface; `model:` pins the
wire model independently. When `backend:` is absent, the normal workflow,
environment, and credential resolution chain chooses one — `model:` alone does **not** force a
direct API call. Delegated coding-agent backends are the richest choice for
editing files, running shell, and driving git:

```iter
agent implementer:
  backend: "claude_code"          # recommended (codex is supported but discouraged)
  input: plan_schema
  output: result_schema
  system: implementation_prompt
  tools: [read_file, write_file, run_command, git_diff]
```

| Backend | Status | What it does |
|---------|--------|-------------|
| `claude_code` | recommended | Runs the `claude` CLI as a subprocess with full tool access |
| `claw` | recommended for direct provider calls | In-process multi-provider LLM client (Anthropic, OpenAI, …) with Iterion's native declared tools — select it explicitly with `backend: "claw"` and pair it with `model: "openai/gpt-5.4-mini"`, etc. It is also the last-resort backend when credential detection finds nothing. |
| `codex` | **discouraged** | Runs the `codex` CLI as a subprocess. Cannot configure its tool set, tends to fill its own context window, and has weaker iterion integration. The compiler emits a `C030` warning per node. Kept for compatibility — prefer `claude_code` or `claw`+OpenAI in new workflows. |
| `pi` | opt-in | Runs the [pi](https://pi.dev) multi-provider agent harness — the backend to reach for when you need **a model the other backends cannot run** (~36 providers: anthropic, openai, google, vertex, bedrock, azure, github-copilot, xai, zai, groq, cerebras, …). Reports a provider-computed cost (not an estimate), carries its own credential store, and from v3.7.6 supports the permission gate, `ask_user`/async questions, board `capabilities:`, and workflow `mcp_server:` blocks via the iterion pi extension (RPC transport only). Small tool set, no subagents/web. See [Backends → Third-party agent CLIs](backends.md#third-party-agent-clis-pi-kimi-grok-and-the-cli-agent-seam) and [ADR-085](adr/085-pi-as-execution-backend.md). |
| `kimi` | opt-in | Runs Moonshot's `kimi-code` CLI, whose argv is disjoint from claude-code's (`kimi -p <prompt> --output-format stream-json [-m <alias>]`). A concrete instance of iterion's generic CLI-agent backend. See [Backends → Third-party agent CLIs](backends.md#third-party-agent-clis-pi-kimi-grok-and-the-cli-agent-seam) and [ADR-065](adr/065-dedicated-cli-agent-backend.md). |
| `grok` | opt-in | Runs xAI's `grok` Build CLI through the same CLI-agent seam (`grok -p <prompt> --output-format json [-m <model>]`); `system:` appends via `--rules`, `reasoning_effort` maps to `--reasoning-effort`, headless approval forced with `--permission-mode bypassPermissions --always-approve`. Credentials come from the CLI's own Grok Build login (`~/.grok`). Distinct from the metered xAI HTTP path (`backend: claw` + `model: "xai/…"`). See [Backends → Third-party agent CLIs](backends.md#third-party-agent-clis-pi-kimi-grok-and-the-cli-agent-seam) and [ADR-065](adr/065-dedicated-cli-agent-backend.md). |

> 💡 `claude_code` works with your Claude subscription (Pro/Max/Team/Enterprise) — no separate API key required. `claw` calls provider APIs directly and needs the corresponding API key (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, …).

Delegated coding agents are useful for autonomous implementation work. For a
review, judge, or plan that should call a provider API in-process, pin both the
`claw` backend and the model; `readonly: true` constrains the node independently
of that selection:

```iter
agent reviewer:
  backend: "claw"                      # Force an in-process provider call
  model: "anthropic/claude-sonnet-5"
  readonly: true

agent implementer:
  backend: "claude_code"               # Full coding agent — can edit files
  tools: [read_file, write_file, patch, run_command]
```

Leaving `backend:` out instead opts into automatic credential detection, which
may select either `claude_code` or `claw` under the default preference.
