# Plugins

Iterion's plugin ecosystem lets you extend the runtime with **declarative,
out-of-process** packages. A plugin never injects Go code (iterion ships static
`CGO_ENABLED=0` binaries that are bind-mounted into sandbox containers — Go's
`plugin` package is a non-starter). Instead a `plugin.yaml` manifest declares
**what** to contribute, and the runtime wires it into iterion's existing seams.

Plugins are installable-by-default, uninstallable, replaceable, and composable —
`rtk` (the command-output compressor) ships as a plugin enabled by default; the
knowledge-graph explorers `graphify`, `repo-falcon` and `codeindex` and the web
toolkit `firecrawl` (Firecrawl search/scrape/crawl MCP — see
[web-search.md](web-search.md)) ship disabled.

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
  (`rtk`, `graphify`, `repo-falcon`, `codeindex`, `firecrawl`).
- **Installed** plugins live under `~/.iterion/plugins/<name>/` (a directory
  with a `plugin.yaml`). `iterion plugin install <path|git-url>` puts them there.
- **Enable/disable state** is persisted in `~/.iterion/plugins.yaml`; the
  default for a plugin with no recorded preference is its manifest
  `default_enabled`. (`$ITERION_HOME` overrides the home dir.)

### Env-based enablement & config (cloud / headless)

`~/.iterion/plugins.yaml` is per-machine and ephemeral in a cloud runner pod,
so enablement can also be driven by **immutable env** — set once on the
runtime (e.g. a Helm chart's `config.extraEnv`), no persistent file needed.
Env wins over both stored state and `default_enabled`:

| Env var | Effect |
|---------|--------|
| `ITERION_PLUGINS_ENABLE=a,b` | force-enable these plugins (comma/space list) |
| `ITERION_PLUGINS_DISABLE=c`  | force-disable (wins over enable for the same name) |
| `ITERION_PLUGIN_<NAME>_<KEY>=v` | set config value `<key>` for `<name>` (highest precedence over defaults + stored values) |

`<NAME>`/`<KEY>` are upper-cased with `-` → `_`. Example — enable the
`firecrawl` MCP plugin and point it at a self-hosted instance, entirely from
env:

```sh
ITERION_PLUGINS_ENABLE=firecrawl
ITERION_PLUGIN_FIRECRAWL_API_URL=http://iterion-firecrawl:3002
```

### Markdown contributions reach cloud runs (ADR-079)

Env-based enablement above is what a runner pod needs for **process-local**
kinds (`rewriters`, `mcp_servers`, `lifecycle`): those run *in* the pod, so the
pod's own registry must know about them.

The **markdown** kinds (`skills`, `commands`, `agents`) work differently, and
used not to work on cloud at all. A runner pod's iterion home is ephemeral and
empty, so an *installed* plugin — one living in the studio/server instance's
`~/.iterion/plugins/`, which no env var can conjure onto a pod — mirrored
nothing there, silently (mirroring is best-effort so no error surfaced).

Since **ADR-079** the launching instance resolves the enabled plugins' markdown
files and ships them on the queue message (`RunMessage.Contributions`, schema
v5); the runner mirrors that payload with the same collision policy and
precedence. So an org-private plugin installed + enabled on a cloud studio now
reaches its runs. Two consequences worth knowing:

- **`hooks` are not carried yet** — they merge into `.claude/settings.json`
  rather than mirroring markdown, so they still need the pod's own registry
  (env-based enablement).
- Enablement of *installed* plugins stays **global per instance**. For a
  **team-scoped, private** plugin see the next section.

### Org-private plugins from a git repo (ADR-080)

Installing a plugin into a cloud pod's iterion home is **not durable** — the pod
is ephemeral, so the plugin silently disappears on the next restart and runs
proceed without it. And a single `plugins.yaml` per instance cannot give one
team a private plugin.

A **plugin source** fixes both: a team-scoped record naming the git repository
that holds the plugin, persisted in Mongo (the durable cloud substrate). The
checkout is only a re-derivable cache, so a restart rebuilds it. The fetch is
built in a staging directory and moved into place with a single rename, so a
launch is never handed a half-populated tree; and concurrent cold launches on
the same source share one clone (a per-key lock serialises them) rather than
racing N fetches against the same ref.

```sh
# team admin/owner
POST /api/teams/{id}/plugin-sources
{ "name": "deploy-k8s-acme", "git_url": "https://github.com/acme/iterion-deploy.git",
  "ref": "v1.0.0", "secret_id": "<generic secret: PAT or deploy key>", "enabled": true }
```

The repo may carry a full `plugin.yaml` or be a bare `skills/` library (a
skills-only manifest is synthesized, as with `iterion plugin install`). The
credential is consumed **by reference** — passed to git through an askpass
helper, never argv, never the URL, and redacted from output.

**Pin `ref` to a tag or sha.** A pinned ref makes the checkout immutable: every
launch after the first costs no network, *and* updating the plugin becomes an
explicit, auditable bump instead of a skill that changes under a running bot. A
moving branch is allowed but warns, and `pinned_ref` is exposed on the API so
the UI can flag it.

Unlike an installed plugin (best-effort, skipped on error), a source the
operator **enabled** that fails to fetch **fails the launch** — a run missing
its platform skill would otherwise succeed while doing the wrong thing.

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
(`~/.iterion/plugins/<name>/cache`), and `{{config.<key>}}` for any declared
config field (see below). The rewriter `{{command}}` placeholder is substituted
at rewrite time with the full shell command line; a rewriter's `invoke.env` and
`invoke.argv` also resolve `{{config.<key>}}`.

## Configuration (`config:`)

A plugin can declare user-configurable settings — like a Firefox add-on's
preferences. The operator sets values in the studio (the **Configure** pane on
the Plugins view) or via the CLI; the values are stored in `~/.iterion/plugins.yaml`
(`0600`, alongside enable state) and substituted into the plugin's
`mcp_servers`/`rewriters`/`lifecycle` commands through `{{config.<key>}}`.

```yaml
config:
  - key: max_depth          # referenced as {{config.max_depth}}
    label: Max depth        # studio form label (defaults to key)
    type: int               # string | bool | int | float | enum | secret
    default: "3"            # used until the operator sets a value
    description: How deep to traverse.
  - key: mode
    type: enum
    options: [on, ultra]    # required for type: enum
    default: on
  - key: api_key
    type: secret            # password field; value never sent back to the studio
    required: true          # advisory; surfaced in the UI/CLI
contributes:
  lifecycle:
    index: "graphify {{workspace}} --depth {{config.max_depth}}"
  mcp_servers:
    - { name: g, command: graphify, args: ["mcp"], env: { GRAPH_TOKEN: "{{config.api_key}}" } }
```

Values are stored and substituted as strings (a `bool` is `"true"`/`"false"`,
an `int` is `"30"`). A `secret` is write-only over the API: the list/info
responses report only whether it is *set*, never its value, and a blank
submission keeps the prior value (the studio shows "leave blank to keep").
Secrets are stored in cleartext in the `0600` `plugins.yaml`, so this is for
instance-level configuration, not a substitute for tenant secret bindings.

Manage config from the CLI (parity with the studio):

```bash
iterion plugin config <name>                                   # show schema + values
iterion plugin config <name> --set max_depth=5 --set mode=ultra  # set values
iterion plugin info <name>                                     # includes the config block
```

Endpoints: the plugin list/info DTO carries `config_schema` + `config_values`
(+ `config_secret_set`); `PUT /api/v1/plugins/{name}/config` (super-admin)
persists values.

## Command-output compression (rtk, generalized)

Compression is the `rewriter` kind. The DSL field is **`compress:`**
(`on|ultra|off`) on the `workflow` block and on `agent`/`judge`/`tool` nodes;
the CLI flag is **`--compress`**; the env default is **`ITERION_COMPRESS`**.

Precedence: CLI `--compress` → node `compress:` → workflow `compress:` →
`ITERION_COMPRESS` → **default**.

- **Agent/judge nodes are opt-OUT**: when a rewriter plugin is enabled and its
  binary is present (the chain is available), the default is **on** — so rtk
  compresses agent shell output out of the box. An explicit `off` at any level
  wins: per-run via `--compress off` / the studio toggle, or globally via
  `iterion plugin disable rtk` (chain empty → off) or `ITERION_COMPRESS=off`.
- **Tool nodes are opt-IN only** (a run override can force-off as a kill switch
  but never force-on), so a review loop's `git diff` stays full-fidelity unless
  the node sets `compress:`.

The **active rewriter chain** is every enabled rewriter plugin, applied in
stable name order — so you can *replace* rtk (disable it, enable another) or
*complement* it (enable several; each rewrites the previous one's output). rtk
ships installed + enabled, so it is used on runs by default (agent/judge),
disableable per-run and globally as above.

Diagnostic `C102` flags an invalid `compress:` value.

## Knowledge-graph explorers

Three are shipped as disabled builtins; enable any to give agents code-graph
context:

- **repo-falcon** — a deterministic Go MCP server. Enabling it injects the
  `falcon` MCP server into every run's catalog (so agent/judge nodes get
  `falcon_*` tools: symbol lookup, file context, architecture, call paths) and
  mirrors a skill telling agents to query the graph before reading code. Its
  `lifecycle` builds/refreshes the `.falcon/artifacts` snapshot.
- **graphify** — a CLI + skill that builds a queryable graph (`graphify-out/`)
  spanning code and docs; the skill guides agents to `graphify query` / read the
  report. Its `lifecycle` builds/updates the graph.
- **codeindex** — a deterministic, zero-dependency repo-indexing engine
  distributed on npm (`@maxgfr/codeindex`, version-pinned `npx` like
  `firecrawl`). It is the **broadest** contributor shipped: `mcp_servers`
  (26 tools — link-graph, symbols, callers, references, BM25/semantic search,
  hotspots, change coupling, complexity, dead code, architecture rules, agent
  memories and symbolic edits), `lifecycle` (incremental `.codeindex/`
  artifacts), `skills`, `commands` (`/codeindex-map`, `/codeindex-impact`,
  `/codeindex-risk`), `agents` (`codebase-cartographer`) **and** a `rewriter`.
  Its MCP server is pinned to the workspace (`mcp --repo {{workspace}}`), so
  agents never pass an absolute path. Its rewriter maps a tree-wide `grep -r` /
  `rg` onto the indexed equivalent — it locates a real `codeindex` binary
  (Homebrew, `npm -g`, or the runner image's pre-installed global) and degrades
  to passthrough when none is present, so it never pays `npx` startup on a
  per-command hook. Priming the index first turns MCP activation into a load
  rather than a rebuild, which is what `auto_index: true` does before a run.

```sh
iterion plugin enable repo-falcon
iterion plugin run repo-falcon index   # build the snapshot for this workspace
# now any bot run exposes falcon_* tools to its agents

iterion plugin enable codeindex
iterion plugin run codeindex index     # writes <workspace>/.codeindex/
iterion plugin config codeindex --set max_files=50000   # large monorepo
# optional: RRF-fused semantic search instead of lexical-only
iterion plugin config codeindex --set embed_endpoint=http://iterion-codeindex-embed:8756
```

## CLI

```
iterion plugin list                      # all plugins + enable state + kinds
iterion plugin info <name>               # manifest details (+ config schema/values)
iterion plugin config <name> [--set k=v] # show or set a plugin's configuration
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

## Contribution parity status

The `contributes:` design covers the Claude Code plugin taxonomy from the UI,
CLI, and marketplace. Skills and MCP servers reach both `claude_code` and
`claw`. Pi also consumes the resolved plugin skills through an explicit
`--skill` directory in both transports, and the MCP catalog through its embedded
RPC extension. The remaining work is claw-side discovery/execution for commands,
named agents, and hooks. Kimi, Grok, and Codex do not consume these
plugin contribution surfaces:

| Claude plugin type | iterion kind | parity note |
|--------------------|--------------|-------------|
| skills             | `skills` ✅ shipped      | claude_code native lookup, claw's `skill` tool, and pi's explicit `--skill` path consume `.claude/skills/` |
| MCP servers        | `mcp_servers` ✅ shipped | `claude_code`, claw, and pi RPC consume the resolved MCP catalog |
| slash commands     | `commands` ✅ shipped (claude_code) | mirrored to `.claude/commands/`; claude_code discovers via `--setting-sources project`. claw reads commands only from CLAUDE.md today → a `.claude/commands/` loader is staged in `.works/claw-code-go` (`internal/commands/`), lands on the next claw release + `go.mod` bump |
| subagents          | `agents` ✅ shipped (claude_code) | mirrored to `.claude/agents/`; claude_code discovers via `--setting-sources project`. claw has the `agent` tool + SubagentRunner but no named-agent file loader → claw-side follow-on |
| hooks              | `hooks` ✅ shipped (claude_code) | plugin hooks idempotently merged into `.claude/settings.json`; claude_code fires them via `--setting-sources project`. claw has shell + Go hook runners but no settings discovery → claw-side follow-on |

The principle: where claude_code has a native surface and claw does not (or they
diverge), the gap is closed in **`.works/claw-code-go`** (the vendored claw
source) so a plugin behaves identically on either backend, rather than papered
over with a claude_code-only adapter. Adaptation bridges are acceptable as an
interim only when native parity is impractical.

The `commands`, `agents`, and `hooks` manifest kinds are shipped today. Their
claude_code wiring is live; only the claw parity work called out in the table is
follow-on.

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

> **Skill pack (this) vs. skill library.** A skills-only plugin is a **shared,
> versioned, enable/disable-able unit** — install a whole pack from git, toggle
> it on/off. The complementary [**skill library**](skills-library.md) is your
> **editable, per-skill** store (`~/.iterion/skills/`, `iterion skill add|rm`)
> that workflows reference by name via the DSL `skills:` field. `iterion skill
> import <git-url>` installs a pack through *this* plugin path — the bridge
> between the two halves of the hybride model (ADR-059).

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
