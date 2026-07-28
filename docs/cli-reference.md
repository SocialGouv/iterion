# CLI reference

This page maps every public top-level command in the current binary and documents the common operational flags. `iterion <command> --help` is the canonical, build-specific leaf reference. The global `--json` flag is inherited by commands; commands that produce structured records use it for machine-readable output.

## Command map

| Command | Purpose |
|---|---|
| `bench asymptote` | Build a workflow-quality stabilisation report from persisted runs. |
| `bots` | Create bots, install published ones, and emit the catalogue. |
| `bundle` | Pack a bundle source directory into a deterministic `.botz`. |
| `completion` | Generate Bash, Zsh, Fish, or PowerShell completion. |
| `diagram` | Render a workflow as Mermaid. |
| `dispatch` | Poll a tracker and launch an eligible bot per issue. |
| `fork` | Fork a run at a prior LLM turn. |
| `import` | Convert a Claude Code workflow script into a draft `.bot`. |
| `inspect` | Inspect local runs, executions, events, traces, tools, artifacts, and logs. |
| `issue` | Manage the native kanban tracker and import forge issues. |
| `marketplace` | Browse, submit, install, and uninstall local-registry bots/plugins. |
| `memory` | Export, import, and size local shared-knowledge spaces. |
| `models` | Inspect resolved model capabilities and their source. |
| `openapi` | Generate this build's OpenAPI 3.1 document offline. |
| `plugin` | Install/configure/enable/run runtime plugins. |
| `remote` | Authenticate to and drive a remote/cloud Iterion server. |
| `report` | Generate a chronological run report. |
| `resume` | Resume a paused, cancelled, or resumable failed run. |
| `run` | Execute a `.bot`, `.botz`, or bundle directory. |
| `runner` | Run the cloud NATS worker process. |
| `runs` | Apply local run-store lifecycle operations. |
| `sandbox` | Diagnose and strictly validate sandbox configuration. |
| `schedule` | Manage host-cron and keepalive schedules. |
| `secret` | Manage the local sealed secret store. |
| `server` | Serve studio/run APIs locally or the cloud control plane. |
| `skill` | Manage the project/global skill library. |
| `studio` | Launch the local visual editor and run console. |
| `supervise` | Attach a watcher/steering agent or install its Claude hook. |
| `validate` | Parse, compile, and validate without executing. |
| `version` | Print version and commit information. |

`help` is Cobra's generated help command.

## Project and workflow commands

There is no project-initialisation step: iterion works against any directory.
To create a bot, see [`iterion bots create`](#iterion-bots); to attach a repo
or a cloud instance, see [repo scope](repo-scope.md) and [cloud CLI](cloud-cli.md).

### `iterion validate`

```bash
iterion validate workflow.bot
iterion validate bundle.botz --json
```

Accepted inputs are `.bot`, `.botz`, and bundle directories. Validation reports sparse DSL diagnostics in C001–C199 and bundle checks in C200–C230; the [diagnostic catalogue](references/diagnostics.md) is authoritative.

### `iterion diagram`

```bash
iterion diagram workflow.bot
iterion diagram workflow.bot --view detailed
iterion diagram workflow.bot --view full
```

`--detailed` and `--full` are aliases for the corresponding `--view` values.

### `iterion bundle`

```bash
iterion bundle pack my-bot
iterion bundle pack my-bot --output dist/my-bot.botz --force
```

`bundle` only packages. Create the source directory with `iterion bots create`.
See [bundles](bundles.md) for the archive contract.

### `iterion import`

```bash
iterion import .claude/workflows/review.js
iterion import review.js --name review --out review.bot
iterion import review.js --dry-run
```

Import never executes JavaScript. Recognised `agent`/`phase`/loop/routing shapes become DSL; unknown constructs become `## IMPORT` markers. The result is a compile-checked draft and the conversion is intentionally lossy. See [import](import.md).

## Run lifecycle

### `iterion run`

```bash
iterion run <file.bot|file.botz|bundle-dir> [flags]
```

Inputs and execution:

| Flag | Meaning |
|---|---|
| `--var key=value` | Set a variable; repeatable. |
| `--preset <name>` | Apply an in-source preset before `--var`. |
| `--recipe <file>` | Apply a recipe JSON overlay. |
| `--run-id <id>` | Supply the run id. |
| `--store-dir <dir>` | Local store override. Without it, reuse a managed project `.iterion` or use the deterministic project slot under `$ITERION_HOME/projects/` (normally `~/.iterion/projects/`). |
| `--timeout <duration>` | Outer run deadline. |
| `--log-level error\|warn\|info\|debug\|trace` | Logging verbosity. |
| `--no-interactive` | Return at a human pause instead of prompting on the TTY. |
| `--skip-mcp-health` | Warn instead of aborting when a declared MCP server fails startup health. |
| `--auto-resume <n>` | Retry eligible `failed_resumable` causes with capped backoff. |

Launch-time graph overrides:

| Flag | Meaning |
|---|---|
| `--model selector=model` | Override by node id, id glob, or kind (`agent`/`judge`); repeatable. A bare model targets all LLM nodes. |
| `--backend selector=backend` | Same selector rules for a supported backend; repeatable. `claw`/`claude_code` are recommended, Kimi/Grok are explicit opt-ins, and Codex is legacy. |
| `--max-cost-usd`, `--max-duration`, `--max-tokens`, `--max-iterations`, `--max-parallel-branches` | Override non-zero workflow budget fields. |
| `--review-mode mono\|dual\|auto` | Legacy/third-party topology override for workflows that declare a `review_mode` var; current catalogue campaigns do not. |

Access/isolation:

| Flag | Meaning |
|---|---|
| `--permission off\|ask\|deny` | Override the tool-permission gate. |
| `--permission-allow`, `--permission-ask`, `--permission-deny` | Add repeatable Claude-Code-style rules. |
| `--sandbox none\|auto` | Force sandbox mode or inherit when omitted. |
| `--sandbox-default-image <ref>` | Fallback image for `auto`. |
| `--sandbox-host-state auto\|none` | Bind or exclude host `~/.iterion`/`~/.claude`; use `none` on multitenant runners. |
| `--compress off\|on\|ultra` | Override command-output rewriting/compression. |

Worktree finalization:

| Flag | Meaning |
|---|---|
| `--branch-name <name>` | Override the run branch name. |
| `--merge-into current\|none\|<branch>` | Select final target or keep only the run branch. |
| `--merge-strategy squash\|merge` | Collapse run commits or preserve fast-forward history. |
| `--auto-merge=<bool>` | Apply the finalization automatically; CLI default is true, studio launches defer it. |

See [permissions](permissions.md), [sandbox](sandbox.md), [merge policy](merge-policy.md), and [settings precedence](settings-precedence.md).

### `iterion inspect`

```bash
iterion inspect
iterion inspect --run-id RUN --events
iterion inspect --run-id RUN --list-nodes
iterion inspect --run-id RUN --node analyze --section trace
iterion inspect --run-id RUN --exec exec:main:analyze:0
```

Node selection supports `--branch`, `--iteration` (`-1` latest), `--section summary|events|trace|tools|artifacts|interactions|log|all`, and `--log-tail`.

### `iterion report`

```bash
iterion report --run-id RUN
iterion report --run-id RUN --output report.md
```

The report reconstructs summary, artifacts, timeline, routing, branch lifecycle, interactions, and budget events.

### `iterion resume`

```bash
iterion resume --run-id RUN --answer approved=true
iterion resume --run-id RUN --answers-file answers.json
```

`--file` defaults to the persisted source path. `--force` ignores source drift; `--force-stale` takes over a `running` run whose event stream has been silent for at least 60 seconds. Resume also accepts `--auto-resume`, model/backend overrides, all `--max-*` budget overrides, and permission mode/rules. Model/backend launch overrides are not persisted, so repeat them when continuity matters. See [resume](resume.md).

### `iterion fork`

```bash
iterion fork --run-id PARENT --node implement
iterion fork --run-id PARENT --node implement --turn 0 --new-inputs inputs.json
```

The new run is created in `cancelled` state at the selected conversation turn; resume it to execute. `--rewind-code` additionally requests the captured code snapshot where available. `--name` controls the friendly name.

### `iterion runs prune`

```bash
iterion runs prune --dry-run
iterion runs prune --older-than 168h --keep-last 100
```

Default retention deletes `finished`, `failed`, and `cancelled` runs older than 720 hours. `failed_resumable` requires explicit inclusion via `--status`. Only `<store-dir>/runs/` is touched; worktrees are not removed.

### `iterion runs questions` / `iterion runs answer`

```bash
iterion runs questions <run-id>
iterion runs answer <run-id> <interaction-id> "<answer>"
```

Inspect and answer the **non-blocking** questions an agent posts with the
`ask_user_async` tool (`interaction: async`, ADR-081 — see
[async-interaction.md](async-interaction.md)). `runs questions` lists the
still-unanswered questions of a run; `runs answer` records one answer and
queues it for delivery to the asking node's inbox — the running agent picks
it up at its next turn boundary and the run never has to pause. Both take
`--store-dir` (default `.iterion`). For a run **paused** on a blocking
question, use `iterion resume --answer` instead.

## Bot creation, discovery, and extension distribution

### `iterion bots`

```bash
iterion bots create <slug> [--template <id>] [--workdir <dir>] [--dest bots]
iterion bots templates
iterion bots list
iterion bots list --paths bots --paths examples --format markdown
iterion bots install <git-url|path> [--path <bundle>] [--dest bots]
iterion bots regen-catalog
```

`bots create` scaffolds a bot bundle under `bots/<slug>` — `main.bot`, `manifest.yaml`, `README.md`, `.gitignore`, and the `skills/ prompts/ attachments/ presets/` layout — then refreshes the generated catalogue. It is the CLI half of the studio builder at `/bots/new`: both render through `pkg/botscaffold`, so a bot created either way is identical. The generated workflow is parsed **and** compiled before anything is written.

The name must be free **everywhere discovery looks** (`bots/`, `examples/`, `.botz/`), not merely under `--dest`: a duplicate name makes catalogue routing ambiguous. A collision exits 2 and names the conflicting bot's path.

| Flag | Meaning |
|---|---|
| `--template <id>` | Start from a gallery template (default `blank`); `iterion bots templates` lists them. |
| `--workdir <dir>` | Workspace root anchoring `--dest` and the catalogue refresh (default: cwd). |
| `--dest <dir>` | Parent directory for the bundle, resolved against `--workdir` (default `bots`). |
| `--display-name`, `--description`, `--instructions` | Pre-fill catalogue metadata and the agent's mission. |
| `--model`, `--backend` | Pin instead of auto-detection. |
| `--worktree`, `--sandbox` | Isolation dials; only override the template when passed explicitly. |

`bots list` scans `bots` and `examples` by default and emits `json`, `markdown`, or a generated `skill`. Installs default to workspace `.botz/` and never run the bot. `regen-catalog` rebuilds Nexie's generated bot catalogue from manifests and `.iterion/bot-overrides.yaml`.

### `iterion marketplace`

```bash
iterion marketplace list [--kind bot|plugin] [--query text] [--tag tag]
iterion marketplace submit <git-url|path> [--path subdir] [--ref ref]
iterion marketplace install <slug> [--force]
iterion marketplace uninstall <slug>
```

The registry lives under `<store-dir>/marketplace/`. Submission validates/indexes metadata but does not install. Bot installs land in workspace `.botz/`; plugin installs land under `~/.iterion/plugins/`; neither auto-runs.

### `iterion plugin`

Subcommands are `list`, `info`, `install`, `uninstall`, `enable`, `disable`, `config`, and `run`.

```bash
iterion plugin list
iterion plugin enable repo-falcon
iterion plugin config firecrawl --set api_url=http://localhost:3002
iterion plugin run repo-falcon index
iterion plugin install <directory|git-url>
```

Built-ins are `rtk` (enabled by default), `graphify`, `repo-falcon`, and `firecrawl` (disabled by default). Third-party installs are disabled until enabled. A bare public skill library can be installed through the same path. See [plugins](plugins.md).

### `iterion skill`

Subcommands are `add`, `export`, `import`, `list`, `rm`, and `show`. `--project` targets the project store; otherwise the global store is used.

```bash
iterion skill add changelog-writer --from skill.md
iterion skill import https://github.com/acme/skills
iterion skill list
```

See [skills library](skills-library.md).

### `iterion models` and `iterion openapi`

```bash
iterion models
iterion models openai/gpt-5.5 --json
iterion models --refresh
iterion openapi --output openapi.json
```

Model data reports the online-cache or curated-fallback source. `openapi` is offline and code-generated; use `iterion remote openapi` for a server's live spec.

## Local services and automation

### `iterion studio`

```bash
iterion studio --dir . --port 4891
iterion studio --bots-path ./bots --no-browser
```

The listener defaults to loopback. `--bind 0.0.0.0` exposes unauthenticated local file/run APIs, so use it only on trusted networks. Upload limits are controlled by `--max-upload-size`, `--max-total-upload-size`, `--max-uploads-per-run`, and `--allow-upload-mime`; `--max-concurrent-pipelines` defaults to 3. `--no-browser-pane` disables preview/CDP support. See [visual editor](visual-editor.md).

### `iterion dispatch`

```bash
iterion dispatch
iterion dispatch iterion.dispatcher.yaml --port 4892
iterion dispatch iterion.dispatcher.yaml --no-server
```

No argument selects the native tracker, embedded bot catalogue, and HTTP `:4892` defaults. See [dispatcher](dispatcher.md).

### `iterion schedule`

The manifest defaults to `$ITERION_SCHEDULES_FILE` or `~/.iterion/schedules.yaml`; every subcommand accepts `--manifest`.

```bash
iterion schedule add weekly --cron "0 2 * * 1" \
  --bot bots/sec-audit-source/main.bot --workdir "$PWD"
iterion schedule install
iterion schedule audit --name weekly --since 24h
```

| Subcommand | Notable flags |
|---|---|
| `add <name>` | Required `--cron` and `--bot`; plus `--workdir`, `--store-dir`, `--sandbox`, `--timeout`, repeatable `--var`, `--description`, `--disabled`. |
| `add <name>` guards | `--guard`, `--guard-timeout`, `--guard-var`. Exit 0 fires; stdout becomes a workflow var. |
| `add <name>` overlap | `--overlap skip\|allow\|keepalive`, `--max-concurrent`, `--stale-after`. |
| `list`, `remove` | Inspect or delete manifest entries. |
| `run <name>` | Execute now; `--dry-run` prints the resolved command. |
| `audit` | Filter tick decisions with `--name`, `--since`, `--surface`, `--tail`. |
| `install`, `uninstall` | Synchronize/remove the managed crontab block; install also accepts `--print` and `--tz`. |

See [scheduling](scheduling.md), including sub-minute keepalive behavior.

### `iterion issue`

Subcommands are `create`, `list`, `show`, `move`, `update`, `close`, `board`, and `import`.

```bash
iterion issue create --title "Fix auth" --label backend --priority 10
iterion issue list --state todo --unclaimed
iterion issue move ISSUE --to doing
iterion issue board show
FORGE_TOKEN=... iterion issue import --forge forgejo \
  --repo owner/name --base-url https://forge.example --token-env FORGE_TOKEN
```

Forge import is one-way/idempotent, skips pull requests, and reads the token only from the named environment variable. See [native tracker](native-tracker.md).

### `iterion sandbox doctor`

```bash
iterion sandbox doctor
iterion sandbox doctor --strict workflow.bot --target local
iterion sandbox doctor --strict workflow.bot --target cloud
```

Strict mode resolves workflow/CLI sandbox settings and exits non-zero for driver, image, Kubernetes compatibility, host-state, or network-policy failures. It accepts the run-equivalent `--sandbox`, `--sandbox-default-image`, and `--sandbox-host-state` overrides. See [sandbox](sandbox.md).

### `iterion server` and `iterion runner`

```bash
iterion server --config cloud.yaml --bind 0.0.0.0 --port 4891
iterion runner --config cloud.yaml
iterion server webpush-keys        # mint a VAPID keypair for Web Push
```

`server` uses local in-process mode by default and cloud control-plane mode under `ITERION_MODE=cloud`. `runner` consumes NATS run messages and persists through MongoDB/S3. The `server webpush-keys` subcommand prints a fresh VAPID public/private pair for the `ITERION_WEBPUSH_VAPID_{PUBLIC,PRIVATE}_KEY` env vars that enable user notifications ([notifications](notifications.md)). See [cloud deployment](cloud-deployment.md).

## State, knowledge, and supervision

### `iterion secret`

Subcommands are `set`, `list`, and `rm`; `--project` selects the per-project store. Values are never printed.

```bash
iterion secret set GITHUB_TOKEN
iterion secret set DB_URL --project --hosts db.internal
iterion secret list
```

See [secrets](secrets.md).

### `iterion memory`

Subcommands are `du`, `export`, and `import`. A space is selected by visibility (`bot`, `project`, `cross_project`, `user`, `org`, `global`), name, and the applicable project/user/tenant selector.

```bash
iterion memory du --visibility bot --name campaign
iterion memory export --visibility project --name shared --out shared.tar.gz
```

See [memory and knowledge](memory-and-knowledge.md).

### `iterion supervise`

```bash
iterion supervise --run-id RUN --node implement \
  --system @policies/watchdog.md --monitor event_type=tool_error,tool_name=Bash
iterion supervise install-hook
iterion supervise uninstall-hook
```

The watcher evaluates on turn boundaries/monitor matches and injects node-scoped steering for the next turn. Main flags are `--model`, `--system`, repeatable `--node`/`--monitor`, `--cooldown`, `--max-evals`, and `--claude-session` for a raw Claude Code session. A DSL `supervisor` declaration starts the same coordinator automatically. See [supervisors](supervisors.md).

## Remote, benchmarks, and utility commands

`iterion remote` exposes typed cloud domains for runs, bots, marketplace, issues/boards, dispatcher, triggers, orgs/teams/users, tokens, secrets/keys/bindings, webhooks/forge, audit/usage/limits, memory, plugins, SSO/admin, routes/OpenAPI, and raw API access. CI can use `ITERION_REMOTE_URL`, `ITERION_REMOTE_TOKEN`, and optional team/org selectors without a config file. The complete reference is [cloud CLI](cloud-cli.md).

`iterion bench asymptote` accepts primary `--runs`, optional `--variant-runs`, a required `--judge-node`, judge field/threshold, loop selector, labels, title, per-run detail, and output path. See [asymptote bench](asymptote-bench.md).

`iterion completion <bash|zsh|fish|powershell>` emits shell completion. `iterion version` prints build version and commit; `--commit` prints only the SHA, truncated to the same 12 characters the default output embeds, and exits non-zero when the build carries none (no `-ldflags` injection, no VCS build info, or the Dockerfile's `unknown` default) rather than handing a script an empty or bogus value.
