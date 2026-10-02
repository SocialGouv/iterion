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
(improve claw in `.works/claw-code-go`): [docs/plugins.md](../../plugins.md).

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

