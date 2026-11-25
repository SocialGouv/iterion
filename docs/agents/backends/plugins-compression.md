# Plugins — rewriters, MCP, skills, lifecycle; and `compress:`

Read it when command output is compressed (rtk) or a plugin changes what a
node sees; the product reference is [docs/plugins.md](../../plugins.md).

### Plugins (rewriters, MCP, skills, lifecycle) + command-output compression

Iterion has a **plugin ecosystem**: declarative, out-of-process packages
(`plugin.yaml`) with typed `contributes:` kinds — `rewriters` (command-output
compressors), `mcp_servers` (e.g. knowledge-graph explorers), `skills` /
`commands` / `agents` (markdown mirrored into `.claude/{skills,commands,agents}/`,
all three read by claude_code, skills and commands also by claw — see the
[capability matrix](../../backends.md#workspace-slash-commands)), `hooks` (JSON
fragments idempotently merged into `.claude/settings.json`), and
`lifecycle` (index/refresh). Builtins are embedded
([pkg/plugin/builtin/](../../../pkg/plugin/builtin)); `rtk` ships **enabled**,
`graphify` + `repo-falcon` + `firecrawl` (web search/scrape MCP —
[docs/web-search.md](../../web-search.md)) ship **disabled**. Installed plugins live under
`~/.iterion/plugins/<name>/`, enable state in `~/.iterion/plugins.yaml`. Manage
with `iterion plugin list|info|enable|disable|run|install|uninstall`. The plugin
system never injects Go code (static `CGO_ENABLED=0` binaries rule out Go
`plugin`); it wires manifests into existing seams (rewrite chain, MCP catalog,
skill mirroring). Marketplace entries carry a `kind` (`bot`|`plugin`) so both
share one registry. Public skill libraries install ergonomically: `iterion
plugin install <git-url>` of a bare `skills/` repo (no `plugin.yaml`)
synthesizes a skills-only manifest. Full reference + the roadmap toward the full
Claude plugin taxonomy (commands/agents/hooks) with claude_code⇄claw parity
(improve claw in `pkg/claw`): [docs/plugins.md](../../plugins.md).

**Command-output compression** is the `rewriter` kind, generalized from the old
hardcoded rtk integration. `rtk` ("Rust Token Killer",
[github](https://github.com/rtk-ai/rtk)) is the default-enabled rewriter: it
rewrites an agent's shell command to its token-compressed equivalent (`git
status` → `rtk git status`), saving 60–90% of command-output tokens, on all
three shell surfaces — the **claude_code** Bash PreToolUse hook, the **claw**
bash builtin, and **tool nodes** (node-level opt-in ONLY, so a review loop's
`git diff` stays full-fidelity). The DSL field is **`compress:`**
(`on|ultra|off`) on the `workflow` block and `agent`/`judge`/`tool` nodes; CLI
flag **`--compress`**; env **`ITERION_COMPRESS`**. Precedence: CLI → node →
workflow → env → **default**. The default is opt-OUT for agent/judge nodes
(**on** when a rewriter plugin is enabled + its binary present, so rtk is used
out of the box) and opt-IN for tool nodes (off unless the node sets
`compress:`). Disable per-run (`--compress off` / studio toggle) or globally
(`iterion plugin disable rtk` → chain empty → off; or `ITERION_COMPRESS=off`).
Enabled rewriter plugins form an ordered **chain** so you can replace rtk or
stack several compressors. iterion uses rewriters strictly as
compressors, never permission gates (failures fall back to the original
command). Sandboxed runs bind-mount each rewriter's host binary at its declared
`sandbox_mount` (rtk → `/usr/local/bin/rtk`). Diagnostic `C102` flags an invalid
`compress:` value.

**Secret substitution is table-driven.** The launcher's
`BuildSecretGuard` registers only declared secrets for materialization;
`secretMaterializer` binds that guard into the delegate task. The Bash
`rewriteCommandHandler` skips compression when that substitution changes
the input, then `materializeSecretsHandler` supplies the value. Neither
pass treats a bare `ITERION_*` literal as a secret. `ResolveSecretRef` also
requires the exact declared name; sanitizing an unknown name must not alias
a declared one. Diagnose command corruption before output restoration:
`TestToolRewritePreservesUndeclaredLiterals` checks the command bytes, a
pipeline, repeated Go-template literals, and the declared-secret round trip.

**An enabled MCP plugin widens every claw node's tool list.** Its servers'
tools join each node that has tools — measured on iterion.cloud with
firecrawl enabled, a node declaring 8 tools sent 34. Their schemas reach a
gateway's grammar engine whenever a turn forces a tool call (claw's first
agentic turn sends `tool_choice: "required"`): vLLM's xgrammar refuses
`propertyNames` (mistral-small-3.2: `features not supported by xgrammar`;
qwen3.5: `Unimplemented keys: ["propertyNames"]`), which zod v4 emits for
every `z.record`. The shared schema cleaner (`cleanSchemaJSON`) drops a
`propertyNames` that admits every key (`true`, `{}`, a string type with
annotations only) — an object key is always a string, so the meaning holds
for every provider; one that constrains keys stays and still breaks those
engines (#3645).

Other constructs break other engines: qwen3.6's grammar refuses a hostname
`pattern` that also carries length bounds (`Unable to determine if regex is
empty`, #3679). The loop does not chase them schema by schema. A grammar
refusal (`APIError.IsGrammarRefusal`: `xgrammar` or `Grammar error` in the
gateway's message, even served as a 500) is never retried as transient.
When it refuses the forced first-turn choice, that turn retries with
`tool_choice` auto plus an instruction to use a tool, and the generation
fails if none runs — the contract always-thinking models already follow.
The switch shows as an `llm_retry` event naming the refusal.
