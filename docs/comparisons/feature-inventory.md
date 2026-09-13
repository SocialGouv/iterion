---
title: "Iterion feature inventory"
description: "140 criteria across 16 categories, with Iterion capabilities, sources and limitations."
aside: false
pageClass: "comparison-page comparison-inventory"
---

<div lang="en">

# 🧭 Iterion feature inventory

<img class="comparison-logo" src="./assets/logos/iterion.png" alt="Iterion logo" width="32"> **140 criteria · 16 categories · 35 bots in the repository**

September 11, 2026 · Capabilities, use cases and limitations of the reviewed version.

**Iterion combines agent orchestration and Git workflows with active supervision, memory across missions, asynchronous interaction, file recovery, plugins, scheduling and team coordination.** Use this inventory to identify the capabilities your developers and operators need, and check their scope before adoption.

← [Comparison guide](index.md) · [Feature availability — 10 products](feature-matrix.md#availability)

> **Status key.** ✅ Documented in the reviewed version. 🟡 Available with a material restriction on backend, deployment, scope or activation. 🧩 A method supplied as a bot using engine capabilities. Deferred work is listed separately and excluded from the total.
>
> **Evidence.** Documentation review and targeted code inspection: 139 criteria at commit `bbc1ddc7844b81582b6dfc4d0b7f381680bb284b`, plus connector criterion X09 at `603d2a1e3314252dd2995e3bfaa4a7394e13093b`; no execution tests were performed for this inventory. A checkmark does not imply parity across backends, managed availability or certified reliability. The project describes itself as experimental. [current-state]

## ⭐ Capabilities to explore

<div class="comparison-table" role="region" aria-label="Iterion feature inventory — table 1" tabindex="0">

| Capability | What it enables in your workflow |
|---|---|
| 👁️ [Active supervision](#supervision) | An agent can monitor a mission and send guidance while work is in progress. |
| 🙋 [Asynchronous questions](#human-interaction) | An agent can keep working while you prepare an answer, then wait at a chosen point. |
| 📚 [Memory and knowledge](#memory) | Selected knowledge can be reused across runs, bots and projects. |
| ⏪ [Recovery and rewind](#recovery) | Resume the graph, investigate errors and restore supported file states. |
| 🧩 [Skills and plugins](#extensions) | Distribute team methods, tools and technical knowledge. |
| ⚡ [Triggers and scheduling](#triggers) | Start a mission from a forge event, a schedule, a card or another result. |
| 📋 [Board and shared configuration](#board) | Track work and delegate specific configuration fields to business users. |
| 🏢 [Native multi-tenant platform](#administration) | Operate agent workflows across organizations and teams with scoped resources, credentials and roles. |
| 🔐 [Permissions and data protection](#security) | Make access, secrets and data transformations explicit workflow settings. |

</div>


These categories help you assess fit. Their presence in Iterion does not establish their absence elsewhere.

## 🗺️ Browse the inventory

[📝 Authoring and sharing](#authoring) · [🔀 Orchestration](#orchestration) · [🧠 Models, sessions and context](#models) · [🙋 Human interaction](#human-interaction) · [👁️ Supervision](#supervision) · [📚 Memory and knowledge](#memory) · [🧩 Skills, plugins and integrations](#extensions) · [⏪ Recovery and rewind](#recovery) · [🌿 Git and delivery](#git) · [⚡ Triggers and scheduling](#triggers) · [📋 Team coordination](#board) · [🔐 Permissions, secrets and data](#security) · [🏢 Organizations, access and usage](#administration) · [🔎 Observability](#observability) · [⚙️ Deployment and evaluation](#operations) · [🤖 Bot methods](#bots)

<a id="authoring"></a>

## 📝 Authoring and sharing workflows

<div class="comparison-table" role="region" aria-label="Iterion feature inventory — table 2" tabindex="0">

| ID | Feature | Status | Usage and scope |
|---|---|:---:|---|
| <a id="a01"></a>A01 | **Declarative `.bot` workflows** | ✅ | Define agents, tools, data and transitions in a versionable file. A deterministic graph does not make LLM responses identical. [dsl] |
| <a id="a02"></a>A02 | **Synchronized visual and source editors** | ✅ | Canvas, node library, inspector, live validation, undo and redo. [visual-editor] |
| <a id="a03"></a>A03 | **Guided bot creation** | ✅ | Starter templates, mission form, parameters and a first run; equivalent creation through the CLI. [visual-editor] |
| <a id="a04"></a>A04 | **Portable `.botz` bundles** | ✅ | Deterministic ZIP archive containing the workflow, prompts, skills, attachments and manifest; supports declaring a minimum engine version. [bundles] |
| <a id="a05"></a>A05 | **Reusable variables, presets and prompts** | ✅ | Parameterize a method at launch and distribute specializations without copying the entire workflow. [bundles] [dsl] |
| <a id="a06"></a>A06 | **Team bot editing** | ✅ | Create, duplicate and edit bundle files in cloud Studio, with conflict checks on save. [visual-editor] |
| <a id="a07"></a>A07 | **Importing an existing Claude workflow** | 🟡 | Static converter from Claude JavaScript files to a `.bot` draft, with a report of unsupported elements. Does not import n8n workflows. [import] |
| <a id="a08"></a>A08 | **Schemas and static diagnostics** | ✅ | Validate references, properties and graph/data-flow constraints before execution. General node-output schema validation is an engine option, not enabled by product entry points in this build; `compute` outputs are checked separately. [dsl] [output-validation-code] [compute-types-code] |
| <a id="a09"></a>A09 | **Mermaid diagram export** | ✅ | Generate a compact, detailed or full workflow diagram for documentation and review. [cli-reference] |

</div>


<a id="orchestration"></a>

## 🔀 Orchestrating agents, code and events

<div class="comparison-table" role="region" aria-label="Iterion feature inventory — table 3" tabindex="0">

| ID | Feature | Status | Usage and scope |
|---|---|:---:|---|
| <a id="o01"></a>O01 | **Explicit agents and judges** | ✅ | Separate production from evaluation in the graph; a judge is a configurable LLM evaluation, not proof of correctness. [dsl] |
| <a id="o02"></a>O02 | **Deterministic tools and scripts** | ✅ | Command nodes or JavaScript, Python and shell/Bash scripts, with stdout capture and error handling. [dsl] |
| <a id="o03"></a>O03 | **Computation without a model or shell** | ✅ | The `compute` node provides bounded expressions, transformations, map/filter/reduce and typed outputs. [dsl] |
| <a id="o04"></a>O04 | **Conditional, LLM and round-robin routing** | ✅ | Select a branch using rules, a model decision or a declared rotation. [routers] |
| <a id="o05"></a>O05 | **Parallel branches and result collection** | 🟡 | `fan_out_all`, `fan_out_each`, with `wait_all` or `best_effort` collection; concurrent workspace writes are restricted. [routers] [groups-iteration-subbots] |
| <a id="o06"></a>O06 | **Bounded, data-driven loops and ordered iteration** | ✅ | Fixed or data-resolved bounds, `foreach`, and explicit `unbounded` loops governed by fuel and a stagnation monitor. Empty `foreach` input requires a guard if its body must never run. [groups-iteration-subbots] [loop-fuel-code] [loop-edges-code] |
| <a id="o07"></a>O07 | **Reusable parameterized groups** | ✅ | Declare a subgraph and instantiate it with distinct parameters and prefixes. [groups-iteration-subbots] |
| <a id="o08"></a>O08 | **Sub-bots with their own runs** | 🟡 | Call another bot, retrieve its output and isolate its work; budgets, memory and supervisors have parent/child limitations. [groups-iteration-subbots] [memory-and-knowledge] [supervisors] |
| <a id="o09"></a>O09 | **Resource pools and concurrency limits** | ✅ | Declare resources and reserve a slot with `needs`, for example to limit simultaneous worktrees. [groups-iteration-subbots] [dsl] |
| <a id="o10"></a>O10 | **Emitting and waiting for internal events** | 🟡 | In-run `emit`/`wait`, retained events for late waiters and mandatory timeouts. Durable parking on external events is deferred. [event-primitives] |

</div>


<a id="models"></a>

## 🧠 Models, sessions and context

<div class="comparison-table" role="region" aria-label="Iterion feature inventory — table 4" tabindex="0">

| ID | Feature | Status | Usage and scope |
|---|---|:---:|---|
| <a id="b01"></a>B01 | **Six execution backends** | 🟡 | `claw`, `claude_code`, `pi`, `codex`, `kimi`, `grok`, with different capabilities and support levels. [backends] [delegation] |
| <a id="b02"></a>B02 | **Model and backend selection per node** | ✅ | Mix executors in one workflow, select the model independently of the backend and apply launch overrides. [backends] |
| <a id="b03"></a>B03 | **Provider or credential fallback** | 🟡 | Configurable fallback chains when the initial access is unsuitable; changes should remain observable. [backends] |
| <a id="b04"></a>B04 | **Cross-backend fallback and skip routes** | 🟡 | Declare a fallback executor or a workflow-defined `skip` route; prerequisites are validated. Codex is excluded from the first cross-backend fallback version. [backends] |
| <a id="b05"></a>B05 | **Fresh, inherited or forked sessions** | 🟡 | Choose conversational continuity between steps; session resume and fork are not wired for Kimi/Grok. [dsl] [delegation] |
| <a id="b06"></a>B06 | **Persistent sessions on node re-entry** | 🟡 | `session: persist` reuses a node's conversation in a loop; supported on Claude Code, pi and Codex along the main workflow path. [session-persist] |
| <a id="b07"></a>B07 | **Artifact-only handoff** | ✅ | `artifacts_only` passes the useful result without carrying the previous step's conversation history. [dsl] |
| <a id="b08"></a>B08 | **Context compaction** | 🟡 | Configurable threshold and retention of recent exchanges; depends on the executor and can support error recovery. [dsl] [run-recovery] |
| <a id="b09"></a>B09 | **Behavior dials** | ✅ | Qualitative or numeric prompt settings for depth, caution, style and more. They guide the model without enforcing permissions or guaranteeing quality. [cursors] |
| <a id="b10"></a>B10 | **Reasoning effort settings** | 🟡 | Choose `reasoning_effort` per node or through input data; levels adapt to model and backend support, independently of behavior dials. [ultracode] [effort-code] |
| <a id="b11"></a>B11 | **Sub-agents and workflows created within a node** | 🟡 | `ultracode` enables dynamic orchestration on claw and Claude Code depending on model, harness and tools; separate from declared fan-out and sub-bots. Pi does not provide this capability. [ultracode] [subagents-code] [pi-code] |

</div>


<a id="human-interaction"></a>

## 🙋 Human interaction and guidance

<div class="comparison-table" role="region" aria-label="Iterion feature inventory — table 5" tabindex="0">

| ID | Feature | Status | Usage and scope |
|---|---|:---:|---|
| <a id="h01"></a>H01 | **Typed approval forms** | ✅ | `human` nodes support text, choices, booleans, numbers and lists; review context is separate from expected answers. [human-in-the-loop] |
| <a id="h02"></a>H02 | **Agent questions during execution** | 🟡 | `ask_user` with answers through Studio, CLI or API; support depends on backend and transport. [human-in-the-loop] [delegation] |
| <a id="h03"></a>H03 | **Questions without blocking the agent** | 🟡 | `ask_user_async` lets the agent continue before receiving answers; asynchronous interaction runs on the main path, outside fan-out branches. [async-interaction] |
| <a id="h04"></a>H04 | **Explicit answer synchronization** | 🟡 | `await_answers` synchronizes pending questions; the node requires a timeout, and the run remains active while waiting. [async-interaction] |
| <a id="h05"></a>H05 | **Automatic answers or human escalation** | 🟡 | `llm` and `llm_or_human` modes follow workflow policy; an automated answer is not human approval. [human-in-the-loop] |
| <a id="h06"></a>H06 | **Guidance added to an active run** | 🟡 | Inbox and steering redirect work at backend-defined intake points; they do not instantly cancel an ongoing action. [backends] [supervisors] |
| <a id="h07"></a>H07 | **Files and images at launch or in an answer** | 🟡 | Persisted attachments with MIME type, size and hash; vision depends on the model. Review gates do not accept the same uploads as ordinary human nodes. [attachments] [human-in-the-loop] |
| <a id="h08"></a>H08 | **Review dialogue before integration** | 🟡 | Review companion, correction requests and an optional review environment; explicitly choose `human_required` or an agent verdict. [review-merge-gate] |
| <a id="h09"></a>H09 | **Operator pause with checkpoint** | 🟡 | Request a pause at the next safe boundary, then resume; local in-process path only, with remote cloud pause unimplemented. [pause-code] |

</div>


<a id="supervision"></a>

## 👁️ Supervising an active mission

<div class="comparison-table" role="region" aria-label="Iterion feature inventory — table 6" tabindex="0">

| ID | Feature | Status | Usage and scope |
|---|---|:---:|---|
| <a id="s01"></a>S01 | **Agent supervising another agent** | 🟡 | Observe an agent/judge node and inject guidance during execution; supervisors declared inside sub-bots are not currently wired. [supervisors] |
| <a id="s02"></a>S02 | **Supervision monitors, cadence and budget** | ✅ | React to events, errors or cost signals with a cooldown and evaluation limit; account for supervision costs. [supervisors] |
| <a id="s03"></a>S03 | **Attaching a supervisor to a session** | 🟡 | Attach through the CLI to a local run, or to a Claude Code session with prepared transcript and hooks; remote cloud attachment is deferred. [supervisors] |
| <a id="s04"></a>S04 | **Changing budgets and iterations during a run** | 🟡 | Raise limits or grant extra iterations with acknowledgment and a recorded event; changes apply at safe boundaries. [dsl] [steering-code] |

</div>


<a id="memory"></a>

## 📚 Reusable memory and knowledge

<div class="comparison-table" role="region" aria-label="Iterion feature inventory — table 7" tabindex="0">

| ID | Feature | Status | Usage and scope |
|---|---|:---:|---|
| <a id="m01"></a>M01 | **Document memory across runs** | ✅ | Retain knowledge as Markdown documents beyond a single run's artifacts. [memory-and-knowledge] |
| <a id="m02"></a>M02 | **Seven visibility scopes** | 🟡 | Private run, bot, project, multiple projects, user, organization and global catalog; storage and context determine access. Global scope is read-only for organizations. [memory-and-knowledge] |
| <a id="m03"></a>M03 | **Agent memory read, write and list tools** | 🟡 | `memory_read`, `memory_write`, `memory_list`, with configurable permissions; check the adapter and backend in use. [memory-and-knowledge] [memory-code] |
| <a id="m04"></a>M04 | **Index and selective context loading** | ✅ | Inject an index, preload selected documents and reinject memory before compaction according to configuration. [memory-and-knowledge] |
| <a id="m05"></a>M05 | **Automatic bot memory** | 🟡 | `auto_memory` maintains a `MEMORY.md` per bot and repository; opt-in on claw/Claude Code/pi, with restrictions for cloud sandboxes without read-back. [memory-and-knowledge] |
| <a id="m06"></a>M06 | **Memory quotas and secret detection** | 🟡 | Aggregate, space and document quotas; structured secret detection does not cover all confidential data. [memory-and-knowledge] |
| <a id="m07"></a>M07 | **Memory import and export** | ✅ | tar.gz archives through CLI/API, with skip, overwrite or rename strategies to retain or transfer knowledge. [memory-and-knowledge] |

</div>


<a id="extensions"></a>

## 🧩 Skills, plugins and technical integrations

<div class="comparison-table" role="region" aria-label="Iterion feature inventory — table 8" tabindex="0">

| ID | Feature | Status | Usage and scope |
|---|---|:---:|---|
| <a id="x01"></a>X01 | **Editable skills library** | 🟡 | Global, project or bundled skills; management CLI and local editor, with selected skills transported to cloud runners. [skills-library] |
| <a id="x02"></a>X02 | **Skills selected per workflow or node** | ✅ | Name and description enter the context; content loads on demand. A missing skill produces a warning. [skills-library] |
| <a id="x03"></a>X03 | **Plugins with declarative contributions** | 🟡 | MCP, skills, output rewriting, commands, agents, hooks and lifecycle features; backend support varies by contribution. [plugins] |
| <a id="x04"></a>X04 | **Private organization plugins from Git** | ✅ | Resolve private and public sources, then transport them for local or cloud runs. [plugins] |
| <a id="x05"></a>X05 | **Command output compression** | 🟡 | RTK plugin and `compress` modes reduce text sent to the model; plugin availability and activation depend on the interface in use. [plugins] |
| <a id="x06"></a>X06 | **MCP client in workflows** | 🟡 | Declare MCP servers, inherit them or disable them per node; transports and support depend on the backend. [dsl] [delegation] |
| <a id="x07"></a>X07 | **MCP server for controlling Iterion** | ✅ | `iterion mcp` exposes launch, monitoring, human answers and board operations, locally or against a configured remote instance. [mcp-server] |
| <a id="x08"></a>X08 | **Web search and page reading** | 🟡 | Native tools or configured providers/MCP, including DuckDuckGo, Brave, SearXNG and Firecrawl; coverage varies by backend. [web-search] |
| <a id="x09"></a>X09 | **Deterministic connector actions and package generation** | 🟡 | Local in-process `action:` nodes use typed parameters, bound connections and structured results with pagination/completion indicators. Generate packages from OpenAPI/Swagger with an authored overlay. Project catalogs require opt-in; cloud transport and the catalog MCP facade are outside this delivered scope. [connector-dsl] [connector-local-code] [connector-cli-code] |

</div>


<a id="recovery"></a>

## ⏪ Recovery, repair and rewind

<div class="comparison-table" role="region" aria-label="Iterion feature inventory — table 9" tabindex="0">

| ID | Feature | Status | Usage and scope |
|---|---|:---:|---|
| <a id="r01"></a>R01 | **Workflow state checkpoints** | 🟡 | Resume eligible states with outputs, counters and position; interrupted steps may execute again. [resume] |
| <a id="r02"></a>R02 | **Sub-bot recovery after restart** | 🟡 | Reattach the parent to its recorded child run instead of always launching a new child, when the state is recoverable. [subbot-reattach] |
| <a id="r03"></a>R03 | **Rewinding to an earlier node** | 🟡 | `rewind` invalidates dependent outputs; `--auto` locates a changed node. Branch and starting-state restrictions apply. [resume] |
| <a id="r04"></a>R04 | **File snapshots outside Git history** | 🟡 | Filesystem tracker wired to local CLI/Studio, with deduplicated states at execution boundaries; cloud runners do not wire this tracker. Separate from pushed Git workspace checkpoints. [workspace-versioning] [tracker-wiring-code] [runner-code] |
| <a id="r05"></a>R05 | **Targeted restoration and pre-rewind backup** | 🟡 | Restore run-produced files from available local snapshots while protecting operator changes; ignored or oversized paths are excluded. [workspace-versioning] [resume] |
| <a id="r06"></a>R06 | **Actions with verifiable postconditions** | ✅ | Check a condition before and after a command, skip an already-satisfied action and select required/recover/best_effort policy. [verified-actions] [verified-code] |
| <a id="r07"></a>R07 | **Bounded action repair** | 🟡 | Propose a corrected command, then optionally delegate recovery to an agent; opt-in, bounded attempts, with success determined by the postcondition. [verified-actions] [verified-code] |
| <a id="r08"></a>R08 | **Controlled retries and automatic recovery** | 🟡 | Error classification, backoff, compaction or human pause; optional CLI auto-resume excludes terminal errors. [run-recovery] [recovery-code] |
| <a id="r09"></a>R09 | **Recovery hint after pod loss** | 🟡 | Expose the last successfully pushed workspace checkpoint and a recovery command; this does not automatically restore files or validate a deliverable. [resume] |
| <a id="r10"></a>R10 | **Forking a run from an earlier exchange** | 🟡 | Create a resumable run with modified inputs and restore the code snapshot when available; conversational precision depends on the backend. [cli-reference] [fork-code] |

</div>


<a id="git"></a>

## 🌿 Working and delivering in a repository

<div class="comparison-table" role="region" aria-label="Iterion feature inventory — table 10" tabindex="0">

| ID | Feature | Status | Usage and scope |
|---|---|:---:|---|
| <a id="g01"></a>G01 | **Separate worktree per run** | 🟡 | Isolate a run's Git changes; behavior depends on mode and repository, particularly for empty repositories. [dsl] [repo-scope] [worktree-finalization] |
| <a id="g02"></a>G02 | **Git finalization policy** | 🟡 | Retain or integrate the result according to the selected policy; finalization authority must be granted. [cli-reference] [worktree-finalization] |
| <a id="g03"></a>G03 | **Merge gate with result review** | 🟡 | Review, corrections and merge/squash decision within the supported flow; configure acceptance criteria and the target branch. [review-merge-gate] |
| <a id="g04"></a>G04 | **Changes and commits view** | ✅ | Inspect modified files, diffs and commits from a run; metadata is persisted for documented cloud paths. [visual-editor] [cloud-git] |
| <a id="g05"></a>G05 | **Conservative worktree pool cleanup** | 🟡 | Bounded collection of eligible old worktrees preserves dirty, active or recoverable work; it is not a strict disk cap. [worktree-pool] |
| <a id="g06"></a>G06 | **Devbox tooling environment** | 🟡 | Prepare tooling declared by the bot and repository; requires a compatible installation and execution environment. [devbox] |
| <a id="g07"></a>G07 | **Repository targeting, connection and creation** | 🟡 | Guided forge connection, target selection and authorized creation in cloud Studio. Manifests also support optional or no repository requirements; creation needs forge permissions. [repo-scope] |

</div>


<a id="triggers"></a>

## ⚡ Triggers, scheduling and chaining

<div class="comparison-table" role="region" aria-label="Iterion feature inventory — table 11" tabindex="0">

| ID | Feature | Status | Usage and scope |
|---|---|:---:|---|
| <a id="t01"></a>T01 | **Manifest-defined invocations** | ✅ | Declare forge, command, schedule, board and keepalive inputs, with direct launch or card creation. [bot-invocations] |
| <a id="t02"></a>T02 | **Multi-forge and JSON inbound webhooks** | ✅ | GitHub, GitLab, Forgejo/Gitea or generic payloads; configurable event/project/author filters and authentication. [webhooks] |
| <a id="t03"></a>T03 | **Forge commands and conversations** | 🟡 | Slash commands and bot replies in authorized GitHub/GitLab threads; replies still rely on a skill, while native `forge.reply` is deferred. [forge-conversations] [bot-invocations] |
| <a id="t04"></a>T04 | **Local and cloud scheduling** | ✅ | Cron, guards and overlap policy; runs through host cron locally or the platform scheduler. [scheduling] |
| <a id="t05"></a>T05 | **Keepalive and availability waits** | 🟡 | Relaunch recurring missions using guards and availability windows; configure behavior per bot and backend. [scheduling] [bot-invocations] |
| <a id="t06"></a>T06 | **Triggers on events and run outcomes** | ✅ | Subscribe to board events, explicit events or run completion to chain missions. [trigger-spine] |
| <a id="t07"></a>T07 | **Durable outbox for cloud board effects** | 🟡 | Materialization, claim, retry and permanent failure for trigger effects; at-least-once execution with documented residual failure windows. [trigger-outbox] |
| <a id="t08"></a>T08 | **Automatic mission outcome routing** | 🟡 | Policy frozen at launch for merge, relaunch or escalation; server option and decision log. [outcome-router] |
| <a id="t09"></a>T09 | **HTTP callbacks and web notifications** | 🟡 | HMAC-signed completion callbacks, operator alerts and Web Push require configuration; native desktop notifications are deferred. [outbound-callbacks] [notifications] [usage-caps] |

</div>


<a id="board"></a>

## 📋 Coordinating team work

<div class="comparison-table" role="region" aria-label="Iterion feature inventory — table 12" tabindex="0">

| ID | Feature | Status | Usage and scope |
|---|---|:---:|---|
| <a id="k01"></a>K01 | **Native kanban independent of a forge** | ✅ | Cards, columns, labels, priorities, assignments and comments through Studio or CLI. [native-tracker] |
| <a id="k02"></a>K02 | **Typed custom fields and dependencies** | ✅ | Validated card schema, hard blockers and `waiting_deps` state prevent premature launches. [native-tracker] |
| <a id="k03"></a>K03 | **Autonomous issue dispatcher** | 🟡 | Selection, claim, launch, retries, stall detection and concurrency limits; native tracker and configurable adapters. [dispatcher] |
| <a id="k04"></a>K04 | **Pipeline board and mission control** | ✅ | View planned tasks and active runs, launch by priority and limit concurrent pipelines. [visual-editor] [native-tracker] |
| <a id="k05"></a>K05 | **GitHub Projects V2 synchronization** | 🟡 | Bidirectional statuses; issue fields synchronize according to a specific mapping, not arbitrary field replication. [github-board-sync] |
| <a id="k06"></a>K06 | **Links between cards, runs and results** | ✅ | Latest run, worktree, wait states and event history establish work provenance. [native-tracker] |
| <a id="k07"></a>K07 | **Configuration editor for non-operators** | 🟡 | Share revocable access to selected repository file fields; forge commits, conflict checks and audit, without operator Studio access. [config-share] |

</div>


<a id="security"></a>

## 🔐 Permissions, secrets and data

<div class="comparison-table" role="region" aria-label="Iterion feature inventory — table 13" tabindex="0">

| ID | Feature | Status | Usage and scope |
|---|---|:---:|---|
| <a id="p01"></a>P01 | **Tool permissions and read-only mode** | 🟡 | Ask/deny gates on agent calls: claw, Claude Code and pi RPC; Kimi/Grok deny only, Codex without this gate. `permission:` does not apply to shell `tool` nodes. [permissions] [delegation] |
| <a id="p02"></a>P02 | **Execution sandbox** | 🟡 | Configurable isolation by backend, driver and deployment; `auto` can degrade, while Codex uses its own sandbox. [sandbox] |
| <a id="p03"></a>P03 | **Bot-bound secrets and host controls** | 🟡 | Bindings limit supplied secrets; `allowed_hosts` covers supported egress paths. This guarantee does not extend to every shell call. [secrets-reference] |
| <a id="p04"></a>P04 | **Secrets sealed at rest** | ✅ | AES-256-GCM with record binding; authorized cloud components share the key. Automatic master-key rotation is not currently available. [secrets-reference] |
| <a id="p05"></a>P05 | **File-based secret injection** | 🟡 | Materialize a secret with `as: file`, with in-run refresh on supported paths. [secrets-reference] [file-secret-refresh] |
| <a id="p06"></a>P06 | **Local sensitive-data detection and masking** | 🟡 | Go-based `privacy_filter` covers accounts, emails, phone numbers, URLs and secrets. Person names, postal addresses and dates are excluded. [privacy_filter] |
| <a id="p07"></a>P07 | **Restoring masked values** | 🟡 | `privacy_unfilter` restores original values if the vault file is retained; the vault is not replicated to Mongo/S3. Trace masking targets `privacy_filter` inputs and `privacy_unfilter` outputs, not data flowing elsewhere. [privacy_filter] [vault-code] [runner-code] |
| <a id="p08"></a>P08 | **Upload and browser preview controls** | 🟡 | MIME, size, path and SSRF controls on covered routes; active content is restricted, without a general guarantee that files are harmless. [attachments] [browser-pane] |
| <a id="p09"></a>P09 | **System credential vault for desktop** | 🟡 | macOS Keychain, Windows Credential Manager or Linux Secret Service integration requires the system service. Separate from cloud secret sealing. [desktop] [keychain-code] |

</div>


<a id="administration"></a>

## 🏢 Organizations, access and usage

<div class="comparison-table" role="region" aria-label="Iterion feature inventory — table 14" tabindex="0">

| ID | Feature | Status | Usage and scope |
|---|---|:---:|---|
| <a id="e01"></a>E01 | **Multi-tenant organizations, teams and roles** | ✅ | Separate platform resources by tenant and assign operator/administrator permissions and editing capabilities. [cloud-overview] [config-share] |
| <a id="e02"></a>E02 | **Organization SSO** | ✅ | Generic OIDC and dedicated connectors, with organization-specific configuration and access policies. [sso] |
| <a id="e03"></a>E03 | **User or organization LLM keys** | ✅ | BYOK, defaults and launch/integration overrides follow the resolution chain; providers bill separately. [byok] [secrets-reference] |
| <a id="e04"></a>E04 | **Subscription/OAuth authentication** | 🟡 | Reuse authentication supported by CLI backends within the limits of those accounts and their providers. [oauth-forfait] [backends] |
| <a id="e05"></a>E05 | **Opt-in contributor credential pool** | 🟡 | Credential lending with audience, availability, limits and per-run leases; does not increase provider quotas. [credential-pool] |
| <a id="e06"></a>E06 | **Run budgets: cost, tokens, duration and iterations** | 🟡 | Configurable limits, backend-dependent estimates, finalization headroom and separate sub-bot budgets; not a universal billing cap. [dsl] |
| <a id="e07"></a>E07 | **Platform quotas and launch admission** | 🟡 | Concurrency, rate, monthly and cost quotas; quota reads can allow a launch through on storage errors. [quotas-and-limits] |
| <a id="e08"></a>E08 | **Subscription usage-window protection** | 🟡 | Adjustable soft/hard thresholds and policies; measurement is currently specific to Claude Code and separate from monetary budgets. [usage-caps] |
| <a id="e09"></a>E09 | **Personal tokens and administration audit** | ✅ | Attributed API access and administrative actions; change history covers supported operations. [cloud-rest-api] [secrets-reference] |
| <a id="e10"></a>E10 | **Effective settings and their origins** | ✅ | Launch displays backend, permission, compression and auto-memory values and provenance, plus nodes with their own settings. [settings-precedence] |

</div>


<a id="observability"></a>

## 🔎 Observing and inspecting results

<div class="comparison-table" role="region" aria-label="Iterion feature inventory — table 15" tabindex="0">

| ID | Feature | Status | Usage and scope |
|---|---|:---:|---|
| <a id="u01"></a>U01 | **Live run console** | ✅ | Graph, events, logs and counters for monitoring execution in Studio. [visual-editor] |
| <a id="u02"></a>U02 | **Artifacts and history per step/iteration** | ✅ | Versioned outputs and persisted events for inspecting completed runs. [persisted-formats] |
| <a id="u03"></a>U03 | **Cost reports and usage statistics** | ✅ | Breakdown by model/provider, run counts, failures and P50/P95 durations; precision depends on available accounting. [visual-editor] |
| <a id="u04"></a>U04 | **Session task list** | 🟡 | Deterministic TodoWrite/todo_write history reconstructed from events; currently populated by Claude Code and claw. [session-board] |
| <a id="u05"></a>U05 | **AI-generated monitoring widgets** | 🟡 | Notes, metrics, checklists, progress and charts; local opt-in with an evaluation budget, not currently wired to cloud. [session-board] |
| <a id="u06"></a>U06 | **Web previews and timestamped captures** | 🟡 | Result URLs, run-linked captures and live browser through manual local attachment; cloud live browsing and Playwright auto-attach are deferred. [browser-pane] |
| <a id="u07"></a>U07 | **Browsable file deliverables** | ✅ | A tool can publish a generated file as a run attachment using a stdout directive; publication failures are reported without failing the tool. [attachments] |
| <a id="u08"></a>U08 | **Post-mortem worktree terminal** | 🟡 | Inspect an idle run in its retained environment; local Unix only, without a cloud host terminal. [post-mortem-shell] |
| <a id="u09"></a>U09 | **Structured logs, errors and tracing** | 🟡 | Process JSON, alerts and Sentry/GlitchTip integration; a DSN is required and tracing is enabled separately. [observability] |
| <a id="u10"></a>U10 | **Prometheus, OTLP and Grafana dashboards** | 🟡 | Metrics endpoint and supplied observability stack; costs, tokens, retries and durations depend on data exposed by each backend. [observability-stack] |
| <a id="u11"></a>U11 | **Direct run file editing** | 🟡 | Edit a text file in a retained local worktree; 4 MiB limit, without Git staging, locking or concurrent conflict checks. Separate from the bot editor. [run-file-editor] |

</div>


<a id="operations"></a>

## ⚙️ Deploying and evaluating the platform

<div class="comparison-table" role="region" aria-label="Iterion feature inventory — table 16" tabindex="0">

| ID | Feature | Status | Usage and scope |
|---|---|:---:|---|
| <a id="d01"></a>D01 | **CLI, Studio, desktop app and self-hosted cloud** | ✅ | Multiple interfaces around the same engine; feature parity is incomplete. [current-state] [desktop] |
| <a id="d02"></a>D02 | **Distributed runners and platform storage** | ✅ | NATS queue, runners, MongoDB and S3 storage; Kubernetes architecture and scaling require operation. [cloud-architecture] |
| <a id="d03"></a>D03 | **Bot sources frozen at cloud launch** | ✅ | Hashed snapshot of parent, sub-bots and resources; runners do not silently substitute their own catalog version. [bot-bundle-snapshots] |
| <a id="d04"></a>D04 | **API and TypeScript SDK** | ✅ | Control runs, resume and consume events; documented SDK support for Node, Deno and Bun. [cloud-rest-api] [readme] |
| <a id="d05"></a>D05 | **Recipes and variant comparison** | ✅ | Combine workflow, parameters, prompts, budgets and evaluation policy into benchmark campaigns. [recipes] |
| <a id="d06"></a>D06 | **Convergence analysis on recorded runs** | 🟡 | `bench asymptote` aggregates verdicts by iteration without replaying LLMs; it neither guarantees convergence nor constitutes a completed competitive benchmark. [asymptote-bench] |
| <a id="d07"></a>D07 | **MIT-licensed source code** | ✅ | Modifiable, self-hostable engine; third-party components and services retain their own terms. [license] |
| <a id="d08"></a>D08 | **Desktop multi-project switching** | ✅ | Select and return to multiple projects in the native app; onboarding prepares a first project. [desktop] [desktop-projects-code] |
| <a id="d09"></a>D09 | **Desktop updates with signature verification** | 🟡 | Verify the manifest and artifact using Ed25519, then offer installation according to package and platform. This does not imply macOS notarization or Windows signing. [desktop] [updater-code] |

</div>


<a id="bots"></a>

## 🤖 Methods supplied as bots

<div class="comparison-table" role="region" aria-label="Iterion feature inventory — table 17" tabindex="0">

| ID | Feature | Status | Usage and scope |
|---|---|:---:|---|
| <a id="c01"></a>C01 | **Application and feature development** | 🧩 | `app-dev`, `feature-dev`, `feature-gap-fill`, `bmady` bundles; workflows adaptable to the repository and acceptance criteria. [bot-app-dev] [bot-feature-dev] [bot-feature-gap-fill] [bot-bmady] |
| <a id="c02"></a>C02 | **Code review and review dialogue** | 🧩 | `review-pr`, `revi-converse` and review helpers; verify forge coverage and permissions. [bot-review-pr] [bot-revi-converse] |
| <a id="c03"></a>C03 | **Continuous improvement and modernization** | 🧩 | `whole-improve-loop`, `branch-improve-loop`, `modernize`, `evolve`; each bot defines its checks and approval steps. [bot-whole-improve-loop] [bot-branch-improve-loop] [bot-modernize] [bot-evolve] |
| <a id="c04"></a>C04 | **Tests, coverage and instrumentation** | 🧩 | `test-coverage`, `e2e-coverage`, `golden-master`, `instrument` build or inspect evidence suited to the project. [bot-test-coverage] [bot-e2e-coverage] [bot-golden-master] [bot-instrument] |
| <a id="c05"></a>C05 | **Security audits and dependency maintenance** | 🧩 | `sec-audit-source`, `sec-audit-deps`, `dep-update-guard`, `secured-renovacy`, `supply-shield` and variants; an automated audit cannot certify the absence of vulnerabilities. [bot-sec-audit-source] [bot-sec-audit-deps] [bot-dep-update-guard] [bot-supply-shield] |
| <a id="c06"></a>C06 | **Vulnerability monitoring** | 🧩 | `vuln-watch` detects issues and routes remediation according to configuration. [bot-vuln-watch] |
| <a id="c07"></a>C07 | **Accessibility** | 🧩 | `rgaa-audit`, `ultra11y`; assess audits and fixes against the tested scope, without assuming automatic compliance. [bot-rgaa-audit] [bot-ultra11y] |
| <a id="c08"></a>C08 | **Documentation and architecture** | 🧩 | `docs-refresh`, `product-docs`, `wiki-gen`, `adr-cartograph`, `adr-rechallenge` produce and update documents for review. [bot-docs-refresh] [bot-product-docs] [bot-wiki-gen] [bot-adr-cartograph] [bot-adr-rechallenge] |
| <a id="c09"></a>C09 | **Content monitoring, triage and planning** | 🧩 | `feed-watch`, `issue-triage`, `whats-next`, `campaign`; collection, synthesis and coordination using the supplied workflows. [bot-feed-watch] [bot-issue-triage] [bot-whats-next] [bot-campaign] |

</div>


<a id="limites"></a>
## 🚧 Deferred work and remaining limitations

<div class="comparison-table" role="region" aria-label="Iterion feature inventory — table 18" tabindex="0">

| Area | Status in this inventory |
|---|---|
| `dsl: 2` versioning, explicit file imports and migration | Described in ADR-098; no implementation evidence found in the parser/CLI at the reviewed commit. These commands and imports are not listed as shipped. [dsl-roadmap] |
| Durable run parking on external events | Deferred; distinct from triggers that launch runs and active internal waits. [event-primitives] [async-interaction] |
| Supervisors declared inside sub-bots | Declarations are not wired; parent support does not extend to children. [supervisors] |
| Cloud supervisor attachment and Studio toggle | Deferred; workflow-declared supervision exists, while CLI attachment targets local execution. [supervisors] |
| Memory parity across execution paths | The UI incorrectly lists some bot/project spaces; cloud sub-bots do not receive the parent's MemoryStore; driver read-back limits auto-memory. [memory-and-knowledge] |
| Filesystem snapshots and PII vault on another pod | The file tracker is local; the PII vault is a file in the executor's storeDir. Remote Git checkpoints and Mongo/S3 storage do not transfer them. [tracker-wiring-code] [vault-code] [runner-code] |
| Cloud live browser and Playwright auto-attach | Documented as future work. Existing previews and captures do not establish remote live browsing. [browser-pane] |
| AI-generated session widgets in cloud | Dedicated cloud storage and DSL declaration are deferred; current curation is local and opt-in. [session-board] |
| PII detection for names, postal addresses and dates | Planned for a later version; the five current categories provide limited coverage. [privacy_filter] |
| General node-output validation in product launches | The engine exposes opt-in validation and bounded correction, but CLI/Studio/cloud entry points do not enable it in this build. `compute` output conformance is active independently. [output-validation-code] [output-options-code] [compute-types-code] |
| Automatic master-key rotation and per-tenant KMS | Unavailable; current sealed storage does not provide this capability. [secrets-reference] |
| Native desktop notifications | Deferred; Web Push is separate from native application notifications. [notifications] |
| Automatic remediation bot triggered by Sentry | Described as an intention; `error-watch` does not yet exist. Error reporting and Sentry MCP access are separate capabilities. [sentry-feedback-loop] |
| Broader business connector catalog | The local deterministic foundation is available in [X09](#x09). Broader packaged coverage, the catalog MCP facade and cloud transport remain outside this delivered scope; future connectors are not counted as installable. Tracking: [connector catalog, issue #1072](https://github.com/SocialGouv/iterion/issues/1072). |

</div>


**Cross-cutting limitations.** Sub-bot budgets are not aggregated into the parent's budget, and launch quotas are not a strict financial barrier during storage errors. Resuming a run does not guarantee exactly-once execution of external effects. [dsl] [quotas-and-limits] [resume]

**Improvement methods.** Bots can incrementally retain validated work and measure iteration verdicts. Outcomes depend on the configured checks; improvement on every iteration and freedom from regressions are not guaranteed. [improvement-ratchet]

<a id="evidence"></a>
## 🔬 Inventory evidence

Stable criterion IDs support future comparisons. The total spans primitives, interfaces, operations and methods; it is not a competitive score.

Row references point to a pinned repository version. Targeted code inspection complemented the documentation for the following mechanisms:

<div class="comparison-table" role="region" aria-label="Iterion feature inventory — table 19" tabindex="0">

| Category | Evidence in the inspected code |
|---|---|
| Local connector actions | Typed action recipe and result contract, in-process resolver wiring, OpenAPI/Swagger generation: [DSL][connector-dsl], [local launch wiring][connector-local-code], [CLI][connector-cli-code]. Reviewed at the separate connector commit recorded in X09. |
| Loop control | Fixed/data-resolved bounds, finite fuel and loop stagnation checks: [bounds and monitor][loop-fuel-code], [back-edge handling][loop-edges-code]. |
| Output schemas | Opt-in general validation versus active `compute` conformance: [option][output-options-code], [validator][output-validation-code], [compute path][compute-types-code]. |
| Asynchronous interaction | `StoreAsyncAskBinder`, `InteractionKindAsync` persistence, `human_input_requested` event: [async code][async-code]. |
| Supervision | `Coordinator`, cooldown, `MaxEvals` limit and injection: [supervision code][supervise-code]. |
| Memory | Seven `Visibility` values, `MemoryStore` contract and tool/index wiring: [scopes][scope-code], [contract][knowledge-code], [tools][memory-code]. |
| Verified actions | Skip precondition, recipe, postcondition and bounded repair stages: [implementation][verified-code]. |
| Recovery | Error-class dispatch, attempt counter, retry/compaction/pause: [implementation][recovery-code]. |
| Files and rewind | `Capture`, `Restore`, `RestoreOnly` and protected paths: [tracker contract][tracker-code]. |
| Plugins | Manifest and typed contributions: [implementation][plugin-code]. |
| Shared editor | Readable field projection, patch validation and SHA conflict rejection: [service][config-code]. |
| Credential pool | Acquisition, contributor ranking, leases and release: [broker][credpool-code]. |
| Board triggers | Claim, retry and effect materialization: [worker][outbox-code]. |
| Limit adjustments | `bump_loop`/`raise_budget`, deduplication and typed replies: [transport][steering-code]. |
| PII masking | Registration of both tools, validation and per-run vault: [implementation][privacy-code]. |

</div>


Documentation and targeted implementation checks underpin this inventory. Capabilities remain tied to the cited commit and should be reassessed when the version changes.

### 📦 The 35 source bundles

Counted from `bots/*/main.bot` at the reviewed commit. These are source bundles; not every binary embeds all 35, and some support other workflows.

[adr-cartograph](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/adr-cartograph/main.bot) · [adr-rechallenge](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/adr-rechallenge/main.bot) · [app-dev](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/app-dev/main.bot) · [arbitrate](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/arbitrate/main.bot) · [bmady](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/bmady/main.bot) · [branch-improve-loop](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/branch-improve-loop/main.bot) · [campaign](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/campaign/main.bot) · [dep-update-guard](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/dep-update-guard/main.bot) · [devbox-setup](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/devbox-setup/main.bot) · [docs-refresh](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/docs-refresh/main.bot) · [e2e-coverage](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/e2e-coverage/main.bot) · [evolve](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/evolve/main.bot) · [feature-dev](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/feature-dev/main.bot) · [feature-gap-fill](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/feature-gap-fill/main.bot) · [feed-watch](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/feed-watch/main.bot) · [golden-master](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/golden-master/main.bot) · [instrument](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/instrument/main.bot) · [issue-triage](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/issue-triage/main.bot) · [modernize](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/modernize/main.bot) · [product-docs](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/product-docs/main.bot) · [revi-converse](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/revi-converse/main.bot) · [review-env](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/review-env/main.bot) · [review-pr](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/review-pr/main.bot) · [rgaa-audit](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/rgaa-audit/main.bot) · [sec-audit-deps](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/sec-audit-deps/main.bot) · [sec-audit-source](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/sec-audit-source/main.bot) · [secured-renovacy](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/secured-renovacy/main.bot) · [supply-shield](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/supply-shield/main.bot) · [supply-shield-cve](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/supply-shield-cve/main.bot) · [test-coverage](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/test-coverage/main.bot) · [ultra11y](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/ultra11y/main.bot) · [vuln-watch](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/vuln-watch/main.bot) · [whats-next](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/whats-next/main.bot) · [whole-improve-loop](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/whole-improve-loop/main.bot) · [wiki-gen](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/wiki-gen/main.bot)

The [CSV inventory](/comparisons/feature-inventory.csv) contains the same criteria, statuses, limitations and references for further comparisons.

### 📎 Pinned sources

The short references above resolve to the following documents and files.

- [asymptote-bench] : `docs/asymptote-bench.md`.
- [async-code] : `pkg/backend/model/async_ask.go`.
- [async-interaction] : `docs/async-interaction.md`.
- [attachments] : `docs/attachments.md`.
- [backends] : `docs/backends.md`.
- [bot-adr-cartograph] : `bots/adr-cartograph/main.bot`.
- [bot-adr-rechallenge] : `bots/adr-rechallenge/main.bot`.
- [bot-app-dev] : `bots/app-dev/main.bot`.
- [bot-bmady] : `bots/bmady/main.bot`.
- [bot-branch-improve-loop] : `bots/branch-improve-loop/main.bot`.
- [bot-bundle-snapshots] : `docs/bot-bundle-snapshots.md`.
- [bot-campaign] : `bots/campaign/main.bot`.
- [bot-dep-update-guard] : `bots/dep-update-guard/main.bot`.
- [bot-docs-refresh] : `bots/docs-refresh/main.bot`.
- [bot-e2e-coverage] : `bots/e2e-coverage/main.bot`.
- [bot-evolve] : `bots/evolve/main.bot`.
- [bot-feature-dev] : `bots/feature-dev/main.bot`.
- [bot-feature-gap-fill] : `bots/feature-gap-fill/main.bot`.
- [bot-feed-watch] : `bots/feed-watch/main.bot`.
- [bot-golden-master] : `bots/golden-master/main.bot`.
- [bot-instrument] : `bots/instrument/main.bot`.
- [bot-invocations] : `docs/bot-invocations.md`.
- [bot-issue-triage] : `bots/issue-triage/main.bot`.
- [bot-modernize] : `bots/modernize/main.bot`.
- [bot-product-docs] : `bots/product-docs/main.bot`.
- [bot-revi-converse] : `bots/revi-converse/main.bot`.
- [bot-review-pr] : `bots/review-pr/main.bot`.
- [bot-rgaa-audit] : `bots/rgaa-audit/main.bot`.
- [bot-sec-audit-deps] : `bots/sec-audit-deps/main.bot`.
- [bot-sec-audit-source] : `bots/sec-audit-source/main.bot`.
- [bot-supply-shield] : `bots/supply-shield/main.bot`.
- [bot-test-coverage] : `bots/test-coverage/main.bot`.
- [bot-ultra11y] : `bots/ultra11y/main.bot`.
- [bot-vuln-watch] : `bots/vuln-watch/main.bot`.
- [bot-whats-next] : `bots/whats-next/main.bot`.
- [bot-whole-improve-loop] : `bots/whole-improve-loop/main.bot`.
- [bot-wiki-gen] : `bots/wiki-gen/main.bot`.
- [browser-pane] : `docs/browser-pane.md`.
- [bundles] : `docs/bundles.md`.
- [byok] : `docs/byok.md`.
- [cli-reference] : `docs/cli-reference.md`.
- [connector-dsl] : `docs/dsl.md` at the connector commit recorded in X09.
- [connector-local-code] : `pkg/runview/localconnectors.go` at that connector commit.
- [connector-cli-code] : `cmd/iterion/connectors.go` at that connector commit.
- [cloud-architecture] : `docs/cloud-architecture.md`.
- [cloud-git] : `docs/adr/068-persist-run-diff-content-for-cloud-panels.md`.
- [cloud-overview] : `docs/cloud-overview.md`.
- [cloud-rest-api] : `docs/cloud-rest-api.md`.
- [config-code] : `pkg/configshare/service.go`.
- [config-share] : `docs/config-share.md`.
- [credential-pool] : `docs/credential-pool.md`.
- [credpool-code] : `pkg/credpool/broker.go`.
- [current-state] : `docs/current-state.md`.
- [cursors] : `docs/cursors.md`.
- [delegation] : `docs/delegation.md`.
- [desktop] : `docs/desktop.md`.
- [desktop-projects-code] : `cmd/iterion-desktop/bindings.go`.
- [devbox] : `docs/adr/017-devbox-first-bot-toolchain.md`.
- [dispatcher] : `docs/dispatcher.md`.
- [dsl] : `docs/dsl.md`.
- [dsl-roadmap] : `docs/adr/098-dsl-versioning-and-authoring-surface.md`.
- [effort-code] : `pkg/backend/model/effort.go`.
- [event-primitives] : `docs/adr/051-in-bot-event-driven-primitives.md`.
- [file-secret-refresh] : `docs/adr/069-mid-run-file-secret-refresh-for-sandboxed-runs.md`.
- [forge-conversations] : `docs/forge-conversations.md`.
- [fork-code] : `pkg/runview/fork.go`.
- [github-board-sync] : `docs/github-board-sync.md`.
- [groups-iteration-subbots] : `docs/groups-iteration-subbots.md`.
- [human-in-the-loop] : `docs/human-in-the-loop.md`.
- [import] : `docs/import.md`.
- [improvement-ratchet] : `docs/improvement-ratchet.md`.
- [keychain-code] : `cmd/iterion-desktop/keychain.go`.
- [knowledge-code] : `pkg/knowledge/iface.go`.
- [license] : `LICENSE`.
- [mcp-server] : `docs/mcp-server.md`.
- [memory-and-knowledge] : `docs/memory-and-knowledge.md`.
- [memory-code] : `pkg/backend/model/memory_tools.go`.
- [native-tracker] : `docs/native-tracker.md`.
- [notifications] : `docs/notifications.md`.
- [oauth-forfait] : `docs/oauth-forfait.md`.
- [observability] : `docs/observability.md`.
- [observability-stack] : `docs/observability/README.md`.
- [outbound-callbacks] : `docs/outbound-callbacks.md`.
- [outbox-code] : `pkg/trigger/effects_worker.go`.
- [outcome-router] : `docs/outcome-router.md`.
- [pause-code] : `pkg/runview/service_control.go`.
- [permissions] : `docs/permissions.md`.
- [persisted-formats] : `docs/persisted-formats.md`.
- [pi-code] : `pkg/backend/delegate/pi.go`.
- [plugin-code] : `pkg/plugin/manifest.go`.
- [plugins] : `docs/plugins.md`.
- [post-mortem-shell] : `docs/post-mortem-shell.md`.
- [privacy-code] : `pkg/backend/tool/privacy/register.go`.
- [privacy_filter] : `docs/privacy_filter.md`.
- [quotas-and-limits] : `docs/quotas-and-limits.md`.
- [readme] : `README.md`.
- [recipes] : `docs/recipes.md`.
- [recovery-code] : `pkg/runtime/recovery_dispatch.go`.
- [repo-scope] : `docs/repo-scope.md`.
- [resume] : `docs/resume.md`.
- [review-merge-gate] : `docs/review-merge-gate.md`.
- [routers] : `docs/routers.md`.
- [run-file-editor] : `docs/adr/016-in-run-worktree-file-editor.md`.
- [run-recovery] : `docs/adr/056-adaptive-run-level-recovery.md`.
- [runner-code] : `pkg/runner/loop.go`.
- [sandbox] : `docs/sandbox.md`.
- [scheduling] : `docs/scheduling.md`.
- [scope-code] : `pkg/knowledge/scope.go`.
- [secrets-reference] : `docs/secrets-reference.md`.
- [sentry-feedback-loop] : `docs/sentry-feedback-loop.md`.
- [session-board] : `docs/session-board.md`.
- [session-persist] : `docs/adr/089-session-persist.md`.
- [settings-precedence] : `docs/settings-precedence.md`.
- [skills-library] : `docs/skills-library.md`.
- [sso] : `docs/adr/035-per-org-sso-generic-oidc-plus-dedicated-connectors.md`.
- [steering-code] : `pkg/runview/steer.go`.
- [subagents-code] : `pkg/backend/model/executor_build_task.go`.
- [subbot-reattach] : `docs/adr/084-subbot-reattach-across-restarts.md`.
- [supervise-code] : `pkg/supervise/coordinator.go`.
- [supervisors] : `docs/supervisors.md`.
- [tracker-code] : `pkg/workspacetrack/tracker.go`.
- [tracker-wiring-code] : `pkg/runview/service_launch.go`.
- [trigger-outbox] : `docs/adr/094-trigger-effect-outbox.md`.
- [trigger-spine] : `docs/adr/046-event-driven-runs-trigger-spine.md`.
- [ultracode] : `docs/ultracode.md`.
- [updater-code] : `cmd/iterion-desktop/updater.go`.
- [usage-caps] : `docs/usage-caps.md`.
- [vault-code] : `pkg/backend/tool/privacy/vault.go`.
- [verified-actions] : `docs/adr/044-adaptive-recovery-for-deterministic-action-nodes.md`.
- [verified-code] : `pkg/backend/model/executor_verified_action.go`.
- [visual-editor] : `docs/visual-editor.md`.
- [web-search] : `docs/web-search.md`.
- [webhooks] : `docs/webhooks.md`.
- [workspace-versioning] : `docs/workspace-versioning.md`.
- [worktree-finalization] : `docs/adr/064-worktree-finalization-requires-delegated-authority.md`.
- [worktree-pool] : `docs/worktree-pool.md`.


[asymptote-bench]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/asymptote-bench.md
[async-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/backend/model/async_ask.go
[async-interaction]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/async-interaction.md
[attachments]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/attachments.md
[backends]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/backends.md
[bot-adr-cartograph]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/adr-cartograph/main.bot
[bot-adr-rechallenge]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/adr-rechallenge/main.bot
[bot-app-dev]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/app-dev/main.bot
[bot-bmady]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/bmady/main.bot
[bot-branch-improve-loop]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/branch-improve-loop/main.bot
[bot-bundle-snapshots]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/bot-bundle-snapshots.md
[bot-campaign]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/campaign/main.bot
[bot-dep-update-guard]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/dep-update-guard/main.bot
[bot-docs-refresh]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/docs-refresh/main.bot
[bot-e2e-coverage]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/e2e-coverage/main.bot
[bot-evolve]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/evolve/main.bot
[bot-feature-dev]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/feature-dev/main.bot
[bot-feature-gap-fill]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/feature-gap-fill/main.bot
[bot-feed-watch]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/feed-watch/main.bot
[bot-golden-master]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/golden-master/main.bot
[bot-instrument]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/instrument/main.bot
[bot-invocations]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/bot-invocations.md
[bot-issue-triage]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/issue-triage/main.bot
[bot-modernize]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/modernize/main.bot
[bot-product-docs]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/product-docs/main.bot
[bot-revi-converse]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/revi-converse/main.bot
[bot-review-pr]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/review-pr/main.bot
[bot-rgaa-audit]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/rgaa-audit/main.bot
[bot-sec-audit-deps]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/sec-audit-deps/main.bot
[bot-sec-audit-source]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/sec-audit-source/main.bot
[bot-supply-shield]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/supply-shield/main.bot
[bot-test-coverage]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/test-coverage/main.bot
[bot-ultra11y]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/ultra11y/main.bot
[bot-vuln-watch]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/vuln-watch/main.bot
[bot-whats-next]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/whats-next/main.bot
[bot-whole-improve-loop]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/whole-improve-loop/main.bot
[bot-wiki-gen]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/wiki-gen/main.bot
[browser-pane]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/browser-pane.md
[bundles]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/bundles.md
[byok]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/byok.md
[cli-reference]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/cli-reference.md
[cloud-architecture]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/cloud-architecture.md
[cloud-git]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/adr/068-persist-run-diff-content-for-cloud-panels.md
[cloud-overview]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/cloud-overview.md
[cloud-rest-api]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/cloud-rest-api.md
[config-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/configshare/service.go
[config-share]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/config-share.md
[credential-pool]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/credential-pool.md
[credpool-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/credpool/broker.go
[current-state]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/current-state.md
[cursors]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/cursors.md
[delegation]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/delegation.md
[desktop]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/desktop.md
[desktop-projects-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/cmd/iterion-desktop/bindings.go
[devbox]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/adr/017-devbox-first-bot-toolchain.md
[dispatcher]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/dispatcher.md
[dsl]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/dsl.md
[dsl-roadmap]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/adr/098-dsl-versioning-and-authoring-surface.md
[effort-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/backend/model/effort.go
[event-primitives]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/adr/051-in-bot-event-driven-primitives.md
[file-secret-refresh]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/adr/069-mid-run-file-secret-refresh-for-sandboxed-runs.md
[forge-conversations]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/forge-conversations.md
[fork-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/runview/fork.go
[github-board-sync]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/github-board-sync.md
[groups-iteration-subbots]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/groups-iteration-subbots.md
[human-in-the-loop]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/human-in-the-loop.md
[import]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/import.md
[improvement-ratchet]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/improvement-ratchet.md
[keychain-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/cmd/iterion-desktop/keychain.go
[knowledge-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/knowledge/iface.go
[license]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/LICENSE
[mcp-server]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/mcp-server.md
[memory-and-knowledge]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/memory-and-knowledge.md
[memory-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/backend/model/memory_tools.go
[native-tracker]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/native-tracker.md
[notifications]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/notifications.md
[oauth-forfait]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/oauth-forfait.md
[observability]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/observability.md
[observability-stack]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/observability/README.md
[outbound-callbacks]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/outbound-callbacks.md
[outbox-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/trigger/effects_worker.go
[outcome-router]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/outcome-router.md
[pause-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/runview/service_control.go
[permissions]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/permissions.md
[persisted-formats]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/persisted-formats.md
[pi-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/backend/delegate/pi.go
[plugin-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/plugin/manifest.go
[plugins]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/plugins.md
[post-mortem-shell]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/post-mortem-shell.md
[privacy-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/backend/tool/privacy/register.go
[privacy_filter]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/privacy_filter.md
[quotas-and-limits]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/quotas-and-limits.md
[readme]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/README.md
[recipes]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/recipes.md
[recovery-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/runtime/recovery_dispatch.go
[repo-scope]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/repo-scope.md
[resume]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/resume.md
[review-merge-gate]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/review-merge-gate.md
[routers]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/routers.md
[run-file-editor]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/adr/016-in-run-worktree-file-editor.md
[run-recovery]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/adr/056-adaptive-run-level-recovery.md
[runner-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/runner/loop.go
[sandbox]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/sandbox.md
[scheduling]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/scheduling.md
[scope-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/knowledge/scope.go
[secrets-reference]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/secrets-reference.md
[sentry-feedback-loop]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/sentry-feedback-loop.md
[session-board]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/session-board.md
[session-persist]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/adr/089-session-persist.md
[settings-precedence]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/settings-precedence.md
[skills-library]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/skills-library.md
[sso]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/adr/035-per-org-sso-generic-oidc-plus-dedicated-connectors.md
[steering-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/runview/steer.go
[subagents-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/backend/model/executor_build_task.go
[subbot-reattach]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/adr/084-subbot-reattach-across-restarts.md
[supervise-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/supervise/coordinator.go
[supervisors]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/supervisors.md
[tracker-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/workspacetrack/tracker.go
[tracker-wiring-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/runview/service_launch.go
[trigger-outbox]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/adr/094-trigger-effect-outbox.md
[trigger-spine]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/adr/046-event-driven-runs-trigger-spine.md
[ultracode]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/ultracode.md
[updater-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/cmd/iterion-desktop/updater.go
[usage-caps]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/usage-caps.md
[vault-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/backend/tool/privacy/vault.go
[verified-actions]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/adr/044-adaptive-recovery-for-deterministic-action-nodes.md
[verified-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/backend/model/executor_verified_action.go
[visual-editor]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/visual-editor.md
[web-search]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/web-search.md
[webhooks]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/webhooks.md
[workspace-versioning]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/workspace-versioning.md
[worktree-finalization]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/adr/064-worktree-finalization-requires-delegated-authority.md
[worktree-pool]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/worktree-pool.md

</div>

[output-validation-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/runtime/node_output.go
[output-options-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/runtime/engine_options.go
[compute-types-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/runtime/special_node.go
[loop-fuel-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/runtime/engine_resolve.go
[loop-edges-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/runtime/edges.go

[connector-dsl]: https://github.com/SocialGouv/iterion/blob/603d2a1e3314252dd2995e3bfaa4a7394e13093b/docs/dsl.md#tool
[connector-local-code]: https://github.com/SocialGouv/iterion/blob/603d2a1e3314252dd2995e3bfaa4a7394e13093b/pkg/runview/localconnectors.go
[connector-cli-code]: https://github.com/SocialGouv/iterion/blob/603d2a1e3314252dd2995e3bfaa4a7394e13093b/cmd/iterion/connectors.go
