# CLI reference

This page maps every public top-level command in the current binary and documents the common operational flags. `iterion <command> --help` is the canonical, build-specific leaf reference. The global `--json` flag is inherited by commands; commands that produce structured records use it for machine-readable output.

## Command map

| Command | Purpose |
|---|---|
| `bench asymptote` | Build a workflow-quality stabilisation report from persisted runs. |
| `bots` | Create bots, install published ones, and emit the catalogue. |
| `bundle` | Pack a bundle source directory into a deterministic `.botz`. |
| `clean` | Reclaim disk by deleting run worktrees whose work has landed. |
| `completion` | Generate Bash, Zsh, Fish, or PowerShell completion. |
| `diagram` | Render a workflow as Mermaid. |
| `dispatch` | Poll a tracker and launch an eligible bot per issue. |
| `fork` | Fork a run at a prior LLM turn. |
| `import` | Convert a Claude Code workflow script into a draft `.bot`. |
| `inspect` | Inspect local runs, executions, events, traces, tools, artifacts, and logs. |
| `issue` | Manage the native kanban tracker and import forge issues. |
| `marketplace` | Browse, submit, install, and uninstall local-registry bots/plugins. |
| `mcp` | Serve the operator MCP server on stdio (local + remote tool families). |
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

Accepted inputs are `.bot`, `.botz`, and bundle directories. Validation reports sparse DSL diagnostics in C001–C199 plus the async-interaction band C240–C242, and bundle checks in C200–C234; the [diagnostic catalogue](references/diagnostics.md) is authoritative.

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
| `--backend selector=backend` | Same selector rules for a supported backend; repeatable. `claw`/`claude_code` are in the default auto-selection order; Codex, `pi`, Kimi and Grok are explicit opt-ins. |
| `--max-cost-usd`, `--max-duration`, `--max-tokens`, `--max-iterations`, `--max-parallel-branches` | Override non-zero workflow budget fields. |
| `--review-mode mono\|dual\|auto` | Select the reviewer topology for workflows that declare a `review_mode` var (currently `review-pr` and `evolve`). `mono` runs one family, `dual` runs both, and `auto` resolves to mono on the preferred detected family. No-op for other workflows. |

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
iterion resume --run-id RUN --answer music=@./theme.mp3   # file field → staged as an attachment
```

`--file` defaults to the persisted source path. `--force` ignores source drift; `--force-stale` takes over a `running` run whose event stream has been silent for at least 60 seconds. Resume also accepts `--auto-resume`, model/backend overrides, all `--max-*` budget overrides, and permission mode/rules. Model/backend launch overrides are not persisted, so repeat them when continuity matters. See [resume](resume.md).

### `iterion fork`

```bash
iterion fork --run-id PARENT --node implement
iterion fork --run-id PARENT --node implement --turn 0 --new-inputs inputs.json
```

The new run is created in `cancelled` state at the selected conversation turn; resume it to execute. `--rewind-code` additionally requests the captured code snapshot where available. `--name` controls the friendly name.

### `iterion rewind`

```bash
iterion rewind --run-id RUN --auto           # locate the edit itself
iterion rewind --run-id RUN --node implement # or name the pivot
iterion resume --run-id RUN --force          # after editing the .bot
```

Moves an existing run's checkpoint back onto an already-executed node and
invalidates the outputs downstream of it, so the next resume replays from
there. Same run id — use `fork` when you want the original left intact.
`--auto` diffs your edited `.bot` against the source the run executed and
rewinds to the earliest affected node — the bot-development loop in one step.
`--node` accepts any node with a recorded output (including `tool` and
`compute`, unlike fork's turn anchor); `--file` overrides the source the graph
is read from. Budget accounting, loop counters, and `events.jsonl` are
preserved; artifacts the dropped nodes published get a superseding `rewound`
marker version.

**The workspace is restored too, on BOTH run shapes** — a `worktree: auto` run
through git, an in-place run through
[workspace versioning](workspace-versioning.md). On an in-place run that
workspace is your live checkout, so `--restore-scope` bounds what comes back:

| value      | restores                                                     | default for |
|------------|--------------------------------------------------------------|-------------|
| `produced` | only paths this run recorded changing after the pivot started | in-place runs |
| `full`     | every versioned path in the snapshot                          | `worktree: auto` runs |
| `none`     | nothing; the node replays against the tree as it stands       | — (`--keep-files` is the old spelling) |

`produced` is refused on a `worktree: auto` run — git reverts the whole tree or
none of it — rather than silently widened. It is the in-place default because a rewind is launched right after you
edit files — `--auto` derives the pivot from that edit — so putting the whole
tree back would revert your own work along with the run's. What iterion cannot
attribute it reports rather than guesses: paths it overwrote that had changed
since the run last recorded its workspace, and paths it left in place for the
same reason (which may be a failed node's partial output, or your editor).
The pre-rewind state is banked first either way — `--list-snapshots` /
`--restore-snapshot` is the way back, and it is deliberately full-tree. See
[resume](resume.md#rewind-resume-from-an-earlier-node).

### `iterion runs prune`

```bash
iterion runs prune --dry-run
iterion runs prune --older-than 168h --keep-last 100
```

Default retention deletes `finished`, `failed`, and `cancelled` runs older than 720 hours. `failed_resumable` requires explicit inclusion via `--status`. Only `<store-dir>/runs/` is touched; worktrees are not removed — use [`iterion clean`](#iterion-clean) for those.

### `iterion clean`

```bash
iterion clean                             # dry run, conservative, this project
iterion clean --apply
iterion clean --all-projects --older-than 720h --apply
iterion clean --level moderate --keep-last 10 --apply
```

Reclaims the per-run worktrees under `<store-dir>/worktrees/`. A
`worktree: auto` run that succeeds promotes its commits onto a branch and
removes its checkout; a failed or interrupted one deliberately leaves the
checkout behind for inspection and never comes back for it. On a
long-lived store that leftover pool is where the disk goes, and
`runs prune` cannot reach it.

What decides safety is not age and not run status alone, but what git can
**prove** about the commits:

| landing | meaning | taken at |
| --- | --- | --- |
| `merged` | a ref whose tip is **not** this HEAD contains this HEAD — another line of work was built on top | conservative (clean tree), moderate (dirty) |
| `own-branch` | refs contain HEAD but every one points exactly **at** it: labels keeping the commits alive, nothing built upon them | aggressive |
| `orphan` | git cannot account for the directory at all | aggressive |
| `unlanded` | no ref contains HEAD, or git could not answer | never |
| `nested-repo` | the tree holds a repository of its own | never |

Classifying by *what the refs were built upon* rather than by ref name
matters in practice: iterion creates run worktrees detached
(`git worktree add <path> <sha>`), so there is no symbolic ref to compare
a branch name against, and a run whose commits were merely promoted to
`iterion/run/<x>` has not been adopted by anything yet.

Refs under `refs/iterion/` — the per-run checkpoints iterion writes itself
— are consulted, because they do hold a run's commits alive; but they are
that run's own bookkeeping and are reaped with it, so containment by one
of them can never mean `merged`. Annotated tags are compared on their
peeled commit: `%(objectname)` is the tag object's id and never equals the
commit, so a release tag sitting on a worktree's HEAD would otherwise read
as work built on top of it.

Three guards no level lifts:

1. **A run that is not terminal keeps its worktree.** Checked against run
   status, never mtime — a run can spend hours inside one agent turn
   without touching its checkout, so age alone would call it abandoned. A
   run whose `run.json` cannot be read is treated as active, not as absent.
   The sweep takes the same per-run lock `iterion run` and `iterion resume`
   hold for a run's lifetime and keeps it across the deletion: the window a
   status re-read alone leaves open is not an instant but the whole
   removal, which on a real worktree runs for seconds.
2. **An `unlanded` worktree is never deleted.** Its commits would survive
   only in the reflog, which expires; recovering or discarding that work is
   a decision for the operator and for git, not for a sweep.
3. **A worktree holding a repository of its own is never deleted** — an
   initialised submodule, or a plain clone dropped inside it (a vendored
   checkout, a dependency's source kept beside the code that uses it). Its
   objects live under the directory, so containment in the outer repository
   proves nothing about them, and being gitignored the tree still reads
   clean. A submodule merely *declared* and never initialised does not
   trigger it: `git worktree add` never populates submodules, so that is
   their normal state and there is nothing there to lose.

Git itself must be usable before any verdict is formed: a git missing from
a cron `PATH`, or a malformed config, would otherwise make every directory
unclassifiable at once. A git that answers but fails on a particular
directory yields `unlanded`, never `orphan` — only git's own "not a git
repository" is read as a directory that is not a worktree.

What iterion mirrors into a run worktree at run start does not count as
uncommitted work — it is written by iterion, not produced by the run.
That is `.claude/skills/`, `.claude/commands/`, `.claude/agents/`,
`.claude/.iterion-managed/` and `.claude/settings.json`, and nothing else:
anything a run puts elsewhere under `.claude/` is the run's, and reads as
work.

Immediately before a deletion the whole verdict is derived again, because
the classification is a photograph and a sweep runs for tens of seconds. A
worktree whose HEAD moved, whose tree turned dirty, or which gained a
repository of its own in the meantime is spared. Re-asking only about the
working tree would not do: a commit leaves a clean tree.

Every git answer is refused unless git is talking about **that** directory.
Asked about a directory merely nested inside a repository — and the
project-local `<repo>/.iterion/` store puts the whole worktree pool inside
the operator's checkout — git walks up and answers for the enclosing
repository, whose clean status and refs would read as a landed worktree.

Dot-prefixed entries such as `worktrees/.state` are left alone: they hold
gate state shared across runs, not one run's checkout. Gitignored content,
by contrast, is deleted at every level — in a run worktree it is the build
output the command exists to reclaim — and the count of gitignored paths is
reported per worktree so it is visible before `--apply`.

The command is a dry run until `--apply`, and reports what it spared and
why, so "nothing was eligible" is never confused with "everything was
guarded". Each spared entry carries a `skip_reason`: `run-active`,
`unlanded`, `nested-repo`, `too-recent`, `keep-last`, or
`needs-higher-level`. Sizes are measured for the candidates and for what
only the level ladder holds back — so the yield of the next level up is
visible before choosing it — and not for the rest, which report `0`:
measuring costs a walk of every file, and the live checkout of a running
campaign is never walked at all.

`--older-than` defaults to `168h` (7 days) and is compared against the
mtime of the worktree's **root directory**, which changes when an entry is
created, removed or renamed directly in it — not when a file deep inside
is edited.

A failed deletion does not abort the sweep: the rest is still processed and
the report still printed, since an aborted sweep strands what it already
deleted with no record of it. Failures are listed under `failed` in
`--json`, apart from `deleted`, so `deleted_count` counts deletions rather
than attempts — in a dry run it counts what *would* be deleted, which
`dry_run: true` and each entry's `"deleted": false` make explicit.

After each successful deletion the worktree's own registration is dropped
from the parent repository. `git worktree prune` is deliberately not used —
it sweeps the whole repository and would also drop the registration of a
worktree merely absent at that instant, such as an operator's checkout on
an unmounted volume, discarding its index and staged work.

`--keep-last` applies per store, so under `--all-projects` it keeps N of
each project's worktrees rather than N across the whole machine. Run records
are kept unless `--with-runs` is passed — they are the journal of what the
agent did, and they are small.

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

`iterion models pricing` audits the committed cost table in `pkg/backend/cost/cost.go` against the prices published by the spec aggregator (models.dev) and reports every disagreement — it never rewrites the table (prices feed budget decisions, so a change is a human judgement call). Three verdicts: **DISAGREES** (committed vs published rate differ), **IGNORED** (a price is published but the estimator still reports none), and **table only** (the aggregator has no price — expected for brand-new models, which is why the committed table exists). `--refresh` refetches published prices first; `--check` exits non-zero on drift, for CI:

```bash
iterion models pricing                  # audit against the cached specs
iterion models pricing --refresh        # refetch first, then audit
iterion models pricing --check          # non-zero exit on drift, for CI
```

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

### `iterion mcp`

```bash
iterion mcp                        # stdio MCP server: local_* + remote_* tools
iterion mcp --read-only            # read tools only; remote_api limited to GET
iterion mcp --only remote          # no local-store access at all
claude mcp add iterion -- iterion mcp   # register in Claude Code
```

Serves the operator-facing MCP server on stdio so any MCP client
(Claude Code, Claude desktop, Cursor) can drive iterion: `local_*`
tools operate this machine's store/engine (validate, launch detached
runs that survive the session, follow events/logs/reports, answer
questions, native board), `remote_*` tools drive the `iterion remote`
instance (typed core + the `remote_api` escape hatch + routes/OpenAPI
discovery). Flags: `--store-dir`, `--read-only`, `--only local|remote`.
See [MCP server](mcp-server.md).

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
