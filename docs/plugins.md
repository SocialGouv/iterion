# Plugins

Iterion's plugin ecosystem lets you extend the runtime with **declarative,
out-of-process** packages. A plugin never injects Go code (iterion ships static
`CGO_ENABLED=0` binaries that are bind-mounted into sandbox containers — Go's
`plugin` package is a non-starter). Instead a `plugin.yaml` manifest declares
**what** to contribute, and the runtime wires it into iterion's existing seams.

Plugins are installable-by-default, uninstallable, replaceable, and composable —
`rtk` (the command-output compressor) ships as a plugin enabled by default; the
knowledge-graph explorers `graphify` and `repo-falcon` ship disabled.

## Contribution kinds (v1)

A manifest's `contributes:` block lists one or more typed extension points:

| kind          | what it adds                                              | wired into |
|---------------|-----------------------------------------------------------|------------|
| `rewriters`   | command-output compressors (the rtk generalization)       | the rewrite chain on all three shell surfaces (claude_code Bash hook, claw bash builtin, tool nodes) |
| `mcp_servers` | MCP servers (e.g. a knowledge-graph explorer)             | the workflow MCP catalog — ambient, workflow-wide, like a project `.mcp.json` entry |
| `skills`      | markdown skills                                           | mirrored into `<workspace>/.claude/skills/` at run start |
| `commands`    | markdown slash commands                                  | mirrored into `<workspace>/.claude/commands/` (claude_code discovers via `--setting-sources project`) |
| `agents`      | markdown subagents                                       | mirrored into `<workspace>/.claude/agents/` (claude_code discovers via `--setting-sources project`) |
| `hooks`       | JSON settings fragments (`{"hooks": {...}}`)             | idempotently merged into `<workspace>/.claude/settings.json` (claude_code fires them via `--setting-sources project`) |
| `lifecycle`   | `index` / `refresh` shell commands                        | `iterion plugin run <name> index|refresh` (+ optional `auto_index`) |

`skills` / `commands` / `agents` share one mirror mechanism + the bundle
collision policy (copy / no-op / refresh / shadow) — a same-named
bundle/workspace file shadows the plugin's. Each is a `[]string` of paths
relative to the plugin root.

`hooks` is a `[]string` of JSON settings-fragment paths. At run start iterion
**idempotently merges** every enabled plugin's hooks into
`<workspace>/.claude/settings.json` under `.hooks`: a sidecar
(`.claude/.iterion-managed/plugin-hooks.json`) records the last injection, so a
re-run/resume removes the prior set before re-adding the current one — user
hooks already in `settings.json` are preserved, and disabling a plugin removes
its hooks. A fragment is either a full settings shape (`{"hooks": {...}}`) or a
bare `{<Event>: [...]}` map. **Security:** a `command`-type hook runs arbitrary
shell on tool events — installed plugins are opt-in (disabled by default), so
enabling one with hooks is the operator's deliberate choice, like installing any
tool.

A single plugin may contribute several kinds (repo-falcon ships `mcp_servers` +
`lifecycle` + `skills`).

## Where plugins live

- **Builtins** are embedded in the binary under `pkg/plugin/builtin/<name>/`
  (`rtk`, `graphify`, `repo-falcon`).
- **Installed** plugins live under `~/.iterion/plugins/<name>/` (a directory
  with a `plugin.yaml`). `iterion plugin install <path|git-url>` puts them there.
- **Enable/disable state** is persisted in `~/.iterion/plugins.yaml`; the
  default for a plugin with no recorded preference is its manifest
  `default_enabled`. (`$ITERION_HOME` overrides the home dir.)

## Manifest reference (`plugin.yaml`)

```yaml
name: rtk                       # unique id (kebab-case) = install dir + enable key
version: 1.0.0
description: …                  # shown in `plugin list`
author: …
schema_version: 1               # default 1; a newer version than the binary supports is rejected
default_enabled: true           # enable state when the operator has expressed none
auto_index: false               # run lifecycle.index before a run if enabled
contributes:
  rewriters:
    - id: rtk
      locate:                   # env → PATH (bin) → conventional paths (~ expanded)
        env: ITERION_RTK_BIN
        bin: rtk
        paths: [~/.local/bin/rtk, /usr/local/bin/rtk, /usr/bin/rtk]
      invoke:
        argv: ["rewrite", "{{command}}"]   # exactly one arg holds {{command}} (the full shell line)
        env: { RTK_TELEMETRY_DISABLED: "1" }
        timeout_ms: 5000
        apply_exit_codes: [0, 3]            # exit codes whose stdout is taken as the rewrite (default [0])
        modes:
          on: {}
          ultra: { inject_flag: "--ultra-compact" }  # inserted after the binary name
      sandbox_mount: /usr/local/bin/rtk     # bind-mount the host binary here in sandboxed runs
  mcp_servers:
    - { name: falcon, transport: stdio, command: falcon,
        args: ["mcp","serve","--snapshot","{{workspace}}/.falcon/artifacts","--repo","{{workspace}}"] }
  skills:
    - skills/code-knowledge-graph.md
  lifecycle:
    index:   "falcon index --repo {{workspace}}"
    refresh: "falcon refresh --repo {{workspace}}"
```

Activation-time placeholders in `mcp_servers` and `lifecycle`:
`{{workspace}}`, `{{plugin.dir}}`, `{{plugin.cache}}`
(`~/.iterion/plugins/<name>/cache`). The rewriter `{{command}}` placeholder is
substituted at rewrite time with the full shell command line.

## Command-output compression (rtk, generalized)

Compression is the `rewriter` kind. The DSL field is **`compress:`**
(`on|ultra|off`) on the `workflow` block and on `agent`/`judge`/`tool` nodes;
the CLI flag is **`--compress`**; the env default is **`ITERION_COMPRESS`**.

Precedence (mirrors the old rtk chain): CLI `--compress` → node `compress:` →
workflow `compress:` → `ITERION_COMPRESS` → off. Tool nodes are node-opt-in
**only** (a run override can force-off as a kill switch but never force-on), so
a review loop's `git diff` stays full-fidelity unless the author opts in.

The **active rewriter chain** is every enabled rewriter plugin, applied in
stable name order — so you can *replace* rtk (disable it, enable another) or
*complement* it (enable several; each rewrites the previous one's output). rtk
ships enabled, so behaviour is unchanged out of the box; `compress:` is off
per-node by default.

Diagnostic `C102` flags an invalid `compress:` value.

## Knowledge-graph explorers

Two are shipped as disabled builtins; enable either to give agents code-graph
context:

- **repo-falcon** — a deterministic Go MCP server. Enabling it injects the
  `falcon` MCP server into every run's catalog (so agent/judge nodes get
  `falcon_*` tools: symbol lookup, file context, architecture, call paths) and
  mirrors a skill telling agents to query the graph before reading code. Its
  `lifecycle` builds/refreshes the `.falcon/artifacts` snapshot.
- **graphify** — a CLI + skill that builds a queryable graph (`graphify-out/`)
  spanning code and docs; the skill guides agents to `graphify query` / read the
  report. Its `lifecycle` builds/updates the graph.

```sh
iterion plugin enable repo-falcon
iterion plugin run repo-falcon index   # build the snapshot for this workspace
# now any bot run exposes falcon_* tools to its agents
```

## CLI

```
iterion plugin list                      # all plugins + enable state + kinds
iterion plugin info <name>               # manifest details
iterion plugin enable <name>             # turn on (persists to plugins.yaml)
iterion plugin disable <name>            # turn off (use this for builtins; they can't be uninstalled)
iterion plugin run <name> index|refresh  # run a lifecycle command in the cwd
iterion plugin install <path|git-url>    # install a third-party plugin under ~/.iterion/plugins/
iterion plugin uninstall <name>          # remove an installed plugin
```

## Marketplace

Marketplace entries carry a `kind` (`bot` | `plugin`, defaulting to `bot` for
legacy entries) so bots and plugins share one hosted registry. `iterion
marketplace list` can filter by kind; installing a plugin entry resolves its
repo coordinates and runs the same `plugin install` path.

## Roadmap — full Claude plugin taxonomy & parity

The `contributes:` design is deliberately open so iterion can grow to configure
**every Claude Code plugin type** from the UI and the marketplace. Planned kinds
map onto Claude Code's plugin model:

| Claude plugin type | iterion kind | parity note |
|--------------------|--------------|-------------|
| skills             | `skills` ✅ shipped      | claude_code native lookup + claw `skill` tool both read `.claude/skills/` |
| MCP servers        | `mcp_servers` ✅ shipped | both backends consume the MCP catalog |
| slash commands     | `commands` ✅ shipped (claude_code) | mirrored to `.claude/commands/`; claude_code discovers via `--setting-sources project`. claw reads commands only from CLAUDE.md today → a `.claude/commands/` loader is staged in `.works/claw-code-go` (`internal/commands/`), lands on the next claw release + `go.mod` bump |
| subagents          | `agents` ✅ shipped (claude_code) | mirrored to `.claude/agents/`; claude_code discovers via `--setting-sources project`. claw has the `agent` tool + SubagentRunner but no named-agent file loader → claw-side follow-on |
| hooks              | `hooks` ✅ shipped (claude_code) | plugin hooks idempotently merged into `.claude/settings.json`; claude_code fires them via `--setting-sources project`. claw has shell + Go hook runners but no settings discovery → claw-side follow-on |

The principle: where claude_code has a native surface and claw does not (or they
diverge), the gap is closed in **`.works/claw-code-go`** (the vendored claw
source) so a plugin behaves identically on either backend, rather than papered
over with a claude_code-only adapter. Adaptation bridges are acceptable as an
interim only when native parity is impractical.

The `commands`/`agents`/`hooks` kinds are tracked as a follow-on; the manifest
schema and registry already accommodate new kinds without a breaking change.

## Public skill libraries (shipped)

`iterion plugin install <path|git-url>` installs any repo carrying a
`plugin.yaml`. When the source has **no** `plugin.yaml` but ships bare skills,
install **synthesizes a skills-only manifest**, so a popular community skill
pack becomes a first-class, enable/disable-able iterion plugin with no
authoring step:

```sh
iterion plugin install https://github.com/acme/awesome-claude-skills
iterion plugin enable awesome-claude-skills   # disabled by default (opt-in)
```

Skill discovery: every `*.md` under `skills/` (recursively); if there is no
`skills/` directory, top-level `*.md` (excluding `README.md`). The plugin name
is derived from the repo/dir basename (kebab-cased). The synthesized
`plugin.yaml` is written into `~/.iterion/plugins/<name>/`, and the skills
mirror into `<workspace>/.claude/skills/` once enabled — same path as any other
skill-contributing plugin.

## Implementation pointers

- Manifest + registry: [pkg/plugin/manifest.go](../pkg/plugin/manifest.go),
  [pkg/plugin/registry.go](../pkg/plugin/registry.go); builtins under
  [pkg/plugin/builtin/](../pkg/plugin/builtin/).
- Rewriter chain: [pkg/backend/rewrite/rewrite.go](../pkg/backend/rewrite/rewrite.go).
  Callsites: claude_code hook (`installRewriteHook`), claw bash builtin
  (`RewriteCommandFieldCtx`), tool node (`executor_tool.go`).
- MCP-server contribution merge: [pkg/backend/mcp/plugin_servers.go](../pkg/backend/mcp/plugin_servers.go).
- Skill mirroring: [pkg/runtime/plugin_skills.go](../pkg/runtime/plugin_skills.go).
- Sandbox bind-mounts: `addRewriterMounts` in
  [pkg/runtime/sandbox_mounts.go](../pkg/runtime/sandbox_mounts.go).
- CLI: [cmd/iterion/plugin.go](../cmd/iterion/plugin.go) +
  [pkg/cli/plugin.go](../pkg/cli/plugin.go).
