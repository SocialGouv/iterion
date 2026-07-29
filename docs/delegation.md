# Delegation

Iterion splits agents into thinkers and doers. A `model:` node makes a direct
LLM call for reasoning; delegating a node to a `backend:` CLI agent gives it
full tool access — editing files, running shell, driving git — and you wire
both into the same graph:

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
| `claw` (default) | recommended for read-only / judges | In-process multi-provider LLM client (Anthropic, OpenAI, …) — use with `model: "openai/gpt-5.4-mini"` etc. |
| `codex` | **discouraged** | Runs the `codex` CLI as a subprocess. Cannot configure its tool set, tends to fill its own context window, and has weaker iterion integration. The compiler emits a `C030` warning per node. Kept for compatibility — prefer `claude_code` or `claw`+OpenAI in new workflows. |
| `pi` | opt-in | Runs the [pi](https://pi.dev) multi-provider agent harness — the backend to reach for when you need **a model the other backends cannot run** (~36 providers: anthropic, openai, google, vertex, bedrock, azure, github-copilot, xai, zai, groq, cerebras, …). Reports a provider-computed cost (not an estimate), carries its own credential store, and from v3.7.6 supports the permission gate, `ask_user`/async questions, board `capabilities:`, and workflow `mcp_server:` blocks via the iterion pi extension (RPC transport only). Small tool set, no subagents/web. See [Backends → Third-party agent CLIs](backends.md#third-party-agent-clis-pi-kimi-grok-and-the-cli-agent-seam) and [ADR-085](adr/085-pi-as-execution-backend.md). |
| `kimi` | opt-in | Runs Moonshot's `kimi-code` CLI, whose argv is disjoint from claude-code's (`kimi -p <prompt> --output-format stream-json [-m <alias>]`). A concrete instance of iterion's generic CLI-agent backend. See [Backends → Third-party agent CLIs](backends.md#third-party-agent-clis-pi-kimi-grok-and-the-cli-agent-seam) and [ADR-065](adr/065-dedicated-cli-agent-backend.md). |
| `grok` | opt-in | Runs xAI's `grok` Build CLI through the same CLI-agent seam (`grok -p <prompt> --output-format json [-m <model>]`); `system:` appends via `--rules`, `reasoning_effort` maps to `--reasoning-effort`, headless approval forced with `--permission-mode bypassPermissions --always-approve`. Credentials come from the CLI's own Grok Build login (`~/.grok`). Distinct from the metered xAI HTTP path (`backend: claw` + `model: "xai/…"`). See [Backends → Third-party agent CLIs](backends.md#third-party-agent-clis-pi-kimi-grok-and-the-cli-agent-seam) and [ADR-065](adr/065-dedicated-cli-agent-backend.md). |

> 💡 `claude_code` works with your Claude subscription (Pro/Max/Team/Enterprise) — no separate API key required. `claw` calls provider APIs directly and needs the corresponding API key (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, …).

Delegation is useful for agents that need to *act* on the codebase (write files, run tests, execute commands). For agents that only need to *think* (review, judge, plan), use `model:` directly — it's lighter weight and faster.

You can mix both in the same workflow. A common pattern is using `model:` for reviewers and judges, and `backend:` for implementers:

```iter
agent reviewer:
  model: "claude-sonnet-4-20250514"    # Direct API call — fast, read-only
  readonly: true

agent implementer:
  backend: "claude_code"              # Full agent — can edit files
  tools: [read_file, write_file, patch, run_command]
```
