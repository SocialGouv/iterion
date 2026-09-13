---
title: "Feature matrix"
description: "Compare 21 features across 10 solutions and examine 12 dimensions of agent workflows, with conditions and sources."
aside: false
pageClass: "comparison-page comparison-matrix"
---

<div lang="en">

# 🧭 Iterion and alternatives — feature matrix

**🏢 10 products · ✅ 21 features at a glance · 🧠 12 agent workflow dimensions · 🗂 4 detailed topics.**

Documentation reviewed on September 11, 2026.

📚 **Explore Iterion in depth:** [140 criteria across 16 categories, with sources, limitations and 35 bots](feature-inventory.md). The common matrix covers 21 criteria; the inventory details the broader capabilities of Iterion's agent-centered architecture.

### Quick navigation

[✅ Feature availability](#availability) · [🧠 Agent workflow depth](#agent-depth) · [🧩 Authoring & integrations](#authoring) · [🔄 Orchestration & budgets](#orchestration) · [🛠 Technical environment](#technical) · [☁️ Deployment & governance](#deployment) · [🏢 Team access](#team-access)

[🎯 Choosing an architecture](index.md) · [📚 Sources](#sources)

[👁️ Supervision](feature-inventory.md#supervision) · [🙋 Asynchronous interaction](feature-inventory.md#human-interaction) · [📚 Memory](feature-inventory.md#memory) · [⏪ Recovery and files](feature-inventory.md#recovery) · [🧩 Plugins](feature-inventory.md#extensions) · [⚡ Triggers](feature-inventory.md#triggers)

<a id="availability"></a>

## ✅ Feature availability

**One feature per row, one product per column.** Feature links open the technical explanations below.

**✅ Yes** · **❌ No within the reviewed scope** · **🟡 Conditional or partial** · **🛠 Integration to develop**

“Yes” means the product supplies the feature with normal configuration; it does not imply free availability or equivalent scope. Yellow marks a material condition explained below. 🛠 identifies an explicit integration path that needs implementation and testing. Edition restrictions remain visible in yellow.

<p class="comparison-scroll-hint">↔ Scroll to compare all ten products. Feature labels stay visible.</p>

<div class="comparison-table comparison-presence" role="region" aria-label="Feature matrix, twenty-one criteria and ten products" tabindex="0">

| Feature | <img class="comparison-logo" src="./assets/logos/iterion.png" alt="" width="24"><br>**Iterion** | <img class="comparison-logo" src="./assets/logos/n8n.ico" alt="" width="24"><br>**n8n** | <img class="comparison-logo" src="./assets/logos/make.ico" alt="" width="24"><br>**Make** | <img class="comparison-logo" src="./assets/logos/zapier.ico" alt="" width="24"><br>**Zapier** | <img class="comparison-logo" src="./assets/logos/activepieces.svg" alt="" width="24"><br>**Activepieces** | <img class="comparison-logo" src="./assets/logos/dify.svg" alt="" width="24"><br>**Dify** | <img class="comparison-logo" src="./assets/logos/flowise.svg" alt="" width="64"><br>**Flowise**<br>🗄 Archived | <img class="comparison-logo" src="./assets/logos/langgraph.svg" alt="" width="76"><br>**LangGraph** | <img class="comparison-logo" src="./assets/logos/crewai.ico" alt="" width="24"><br>**CrewAI** | <img class="comparison-logo" src="./assets/logos/windmill.svg" alt="" width="24"><br>**Windmill** |
|:---| :---: | :---: | :---: | :---: | :---: | :---: | :---: | :---: | :---: | :---: |
| 🎨 [Build visually without coding the graph](#authoring) | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | 🟡¹ | 🟡¹ | ✅ |
| 📄 [Workflow definition available as a file](#authoring) | ✅ | ✅ | ✅ | 🟡² | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| ⌨️ [Execute code within a workflow](#authoring) | ✅ | ✅ | 🟡⁴ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| 🤖 [AI agents that can call tools](#authoring) | ✅ | ✅ | ✅ | 🟡² | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| 🔌 [MCP integration, client or server](#authoring) | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | 🟡¹ | 🟡¹ | ✅ |
| 🙋 [Human intervention before continuing](#orchestration) | ✅ | ✅ | 🛠⁸ | 🟡² | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| 💾 [Persist and resume a human wait](#orchestration) | ✅ | ✅ | 🛠⁸ | 🟡² | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| 🔀 [Workflow branches and loops](#orchestration) | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| 💰 [Run-level token budget](#orchestration) | 🟡³ | 🛠⁹ | 🛠⁹ | 🛠⁹ | 🛠⁹ | 🛠⁹ | 🛠⁹ | 🛠⁹ | 🛠⁹ | 🛠⁹ |
| 💵 [Run-level estimated AI cost budget](#orchestration) | 🟡³ | 🛠⁹ | 🛠⁹ | 🛠⁹ | 🛠⁹ | 🛠⁹ | 🛠⁹ | 🛠⁹ | 🛠⁹ | 🛠⁹ |
| 🌿 [Create a Git worktree for a mission](#technical) | ✅ | 🛠¹⁰ | 🛠¹⁰ | 🛠¹⁰ | 🛠¹⁰ | 🛠¹⁰ | 🛠¹⁰ | 🛠¹⁰ | 🛠¹⁰ | 🛠¹⁰ |
| 🔀 [Finalize Git results with a merge policy](#technical) | ✅ | 🛠¹⁰ | 🛠¹⁰ | 🛠¹⁰ | 🛠¹⁰ | 🛠¹⁰ | 🛠¹⁰ | 🛠¹⁰ | 🛠¹⁰ | 🛠¹⁰ |
| 🛡 [Isolate code execution in a sandbox](#technical) | 🟡⁴ | 🟡⁴ | 🟡⁴ | 🟡⁴ | 🟡⁴ | ✅⁴ | 🟡⁴ | 🟡¹ | 🟡⁴ | 🟡⁴ |
| 📁 [Agent sandbox with shell and project files](#technical) | 🟡⁴ | 🛠¹⁰ | 🛠¹⁰ | 🛠¹⁰ | 🛠¹⁰ | 🟡¹³ | 🛠¹⁰ | 🟡¹ | 🟡⁴ | ✅⁴ |
| ⚙️ [Distribute execution across your own workers](#technical) | ✅ | ✅ | ❌⁵ | ❌⁵ | ✅ | 🟡¹¹ | ✅ | 🟡¹ | 🟡¹¹ | ✅ |
| 🏠 [Self-host the workflow engine](#deployment) | ✅ | ✅ | ❌⁵ | ❌⁵ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| ☁️ [Managed hosting: available or assessed on request](#deployment) | 🟡¹² | ✅ | ✅ | ✅ | ✅ | ✅ | 🟡⁶ | 🟡¹ | 🟡¹ | ✅ |
| 🏢 [Team/project access controls on your infrastructure](#team-access) | ✅ | 🟡¹⁴ | ❌⁵ | ❌⁵ | 🟡¹⁴ | 🟡¹⁴ | 🟡¹⁴ | 🟡¹⁴ | 🟡¹⁴ | 🟡¹⁴ |
| 👥 [SSO for team access](#deployment) | ✅ | 🟡² | 🟡² | 🟡² | 🟡² | 🟡² | 🟡² | 🟡¹ | 🟡² | 🟡² |
| 📖 [Publicly available engine source code](#deployment) | ✅ | ✅ | ❌⁵ | ❌⁵ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| 📜 [Engine core under unmodified MIT or Apache 2.0](#deployment) | ✅ | ❌⁷ | ❌⁵ | ❌⁵ | ✅⁷ | ❌⁷ | ✅⁷ | ✅ | ✅ | ❌⁷ |

</div>


<details class="comparison-conditions">
<summary>📋 Conditions, scope and limitations</summary>


1. **Frameworks and associated components.** LangGraph defines graphs in code; LangSmith Studio visualizes and debugs them. Associated LangSmith Fleet builds agents without code, giving this broader product scope a conditional authoring cell. Fleet does not establish visual editing of an arbitrary LangGraph graph. [L11] Deep Agents is an associated harness whose sandbox backends provide file tools and shell execution; a backend must be configured. [L8] CrewAI has a visual platform Studio, including a free Basic tier, separate from its Python framework. Managed services, authentication and MCP adapters rely on associated components. [L2] [L3] [L4] [L7] [C2] [C9]
2. **Plans and editions.** Zapier provides tool-using AI by Zapier steps from Professional, requiring an Advanced or Premium model even with BYOK. Human approval is available from Professional, with configurable expiration; JSON import/export requires Team/Enterprise. Dify and Flowise SSO require Enterprise, as do several controls in other products. Yellow cells reflect these restrictions. n8n Community supports user accounts but excludes shared projects, workflow/credential sharing, SSO and Git version control. [Z6] [Z8] [Z11] [Z5] [N3] [N12] [M1] [A8] [D7] [F6] [C9] [W9]
3. **Iterion budgets.** Limits apply to the current run without aggregating sub-bots; default finalization headroom is 10%. Cost is estimated. A limit per LLM call or agent is not automatically a cumulative run budget. For other products, see the proposed integration in note 9. [I1] [W2] [C5]
4. **Sandbox scope.** n8n: Code runners; Make: JS/Python sandbox on paid plans; Zapier: isolated scripts with time/memory limits; Dify: restricted Code node (the new Agent sandbox is covered in note 13); Flowise: associated E2B interpreter. These code interpreters alone do not establish an agent environment with shell and project files. Iterion depends on the backend, Activepieces on mode, CrewAI on associated services and Windmill on configuration. [I4] [N5] [M8] [Z7] [A5] [D3] [F7] [C6] [C13] [W5] [W6]
5. **Make and Zapier SaaS engines.** “No” refers to the reviewed commercial engines, not SDKs or connectors. Make's on-premise agent connects its SaaS to a local network; it is not a standalone workflow engine. The Make self-hosting conclusion is inferred from this documented scope. Zapier explicitly states that it does not offer an on-premise version. [M1] [M5] [Z4]
6. **Flowise end of life.** Features describe the documented software. The repository was archived on August 13, 2026, with the core team's official Discord/GitHub presence ending August 31, 2026. The site still lists Cloud: 🟡 records that historical offering and its major limitation, without confirming new contract availability. [F3] [F9]
7. **Licenses.** n8n uses the Sustainable Use License, Dify a modified Apache license, and Windmill a mixture of AGPL and Apache by file. Activepieces and Flowise checkmarks apply to community cores, excluding Enterprise components. Public source access and permissive licensing are separate criteria. [N7] [D6] [W8] [A7] [F4]

8. **Human waits in Make.** Proposed integration: store state in a Data Store, request a decision, then trigger a second scenario through a webhook. These building blocks are documented; this application-level integration does not natively suspend the same execution. [M9] [M10]
9. **Cumulative budget integration.** 🛠 means developing a budget controller: route AI calls through a custom API/tool, propagate a run ID, aggregate usage and reject subsequent calls. This may replace a built-in AI step. Sources establish extension points; accounting, enforcement, parallel calls and recovery require integration work and were not tested here. Zapier also pauses above 75 tasks per run, a task-based safeguard rather than a token/USD budget. [N10] [M12] [Z9] [Z10] [A9] [A10] [D9] [D10] [F8] [L5] [C10] [W1]
10. **Git and project sandbox integration.** 🛠 means Git scripts and/or an external execution service connected through commands, HTTP or custom tools. Implement the behavior marked 🛠: worktree creation and finalization policy, or a shell/file service where the selected product does not supply one. An existing sandbox still needs a defined Git delivery policy. A Git connector alone does not supply this lifecycle. Extension points appear in the details and [methodology](methodology.md). [N9] [N10] [M11] [M12] [Z10] [A9] [D9] [F8] [L5] [C1] [W1] [W5]
11. **Worker scope.** Dify documents Celery workers for streaming execution and resumes; non-streaming workflows remain in the API process in the described version. CrewAI documents an Enterprise private platform with replicated workers, separate from operating its Python library alone. [D8] [C11]
12. **Managed Iterion.** Managed service on request — subject to assessment. Self-hosting is available; feasibility, scope, service terms and support commitments are defined during that assessment.
13. **Dify new Agent.** The beta agent has a sandbox with commands, files and package installation. Its workflow node is available in Workflow apps; nodes do not automatically share sandbox state. This is separate from the restricted Code node. [D11] [D12]
14. **Self-hosted team access.** This criterion covers supplied team/project resource access controls, including paid platforms explicitly identified below. It does not equate project sharing with organization-level multi-tenancy or workload isolation. Windmill Community is limited to three workspaces. See [team access and multi-tenancy](#team-access) for every product's boundary and edition.

</details>

**Cell evidence:** the four detailed tables below describe each product's capabilities and official references. No “no” is inferred solely from an omitted mention. 🛠 paths are proposed architectures based on documented extensions. [Methodology and status evidence](methodology.md).

---

## 🧠 What agent support actually covers {#agent-depth}

Iterion's agent-centered design spans the workflow engine, execution backends and shared platform. Use these dimensions alongside the availability grid: two products can both support “agents” while assigning very different work to the application developer.

<div class="comparison-table comparison-agent-depth" role="region" aria-label="Agent workflow capabilities and comparison criteria" tabindex="0">

| Dimension | What Iterion provides | What to compare in your project |
|---|---|---|
| 🧠 **Execution backends and models** | In-process `claw` plus Claude Code, Codex, pi, Kimi and Grok backends; selection per node. [B01–B04](feature-inventory.md#b01) | Model API selection and delegation to a coding-agent harness are different capabilities. Compare tools, session support, fallback and authentication for the exact backend. |
| 🔀 **Custom and adaptive orchestration** | Reusable groups, sub-bots, parallel branches, data-dependent routing and loops; optional dynamic sub-agent orchestration inside supported nodes. [O04–O09](feature-inventory.md#o04), [B11](feature-inventory.md#b11) | Decide which boundaries the graph owns and which decisions agents make at runtime. Check resource contention and child-run behavior. |
| 🔁 **Iteration and termination** | Fixed or data-resolved loop bounds, plus explicit `unbounded` loops governed by fuel and a stagnation monitor. [O06](feature-inventory.md#o06) | Separate graph iteration limits from an agent's internal tool loop; test completion, exhausted fuel and lack of progress. |
| 💬 **Session and context control** | Fresh/inherited/forked sessions, node session persistence, artifact-only handoffs and compaction, subject to backend support. [B05–B08](feature-inventory.md#b05) | Check what is carried between steps and iterations, what gets discarded, and how much repeated context costs. |
| 👁️ **Supervision during execution** | Supervisor agents observe work and inject guidance; monitors have cadence and evaluation budgets. [S01–S04](feature-inventory.md#s01) | A post-run evaluator or parent delegating a task is different from a supervisor guiding a worker while it runs. Verify supported attachment and sub-bot paths. |
| 🙋 **Human collaboration** | Approval forms, asynchronous questions, answer synchronization, steering and review dialogue. [H01–H09](feature-inventory.md#h01) | Check whether the agent can continue while a person answers, where it must stop and how the wait is persisted. |
| 📚 **Memory and knowledge** | Document memory across runs with seven visibility scopes, selective context loading, import/export and optional auto-memory. [M01–M07](feature-inventory.md#m01) | Distinguish conversation history, workflow checkpoints, reusable knowledge and RAG ingestion/retrieval. Verify tenant boundaries and child-run access. |
| 🧩 **Reusable methods and integrations** | Versioned `.bot`/`.botz`, presets, skills, plugins, scripts, MCP client/server, local connector actions and API/SDK. [A01–A05](feature-inventory.md#a01), [X01–X09](feature-inventory.md#x01), [D04](feature-inventory.md#d04) | Check what a package includes, which dependencies it needs and how to connect existing tools. A ready-made application connector and a programmable API are different integration efforts. |
| ⏪ **Recovery and debugging** | Graph checkpoints, eligible-state resume, local file snapshots, rewind and verified actions. [R01–R10](feature-inventory.md#r01) | Test graph state, session state, files and external effects separately. Check which artifacts survive the selected deployment's failures. |
| 🌿 **Repository delivery** | Worktrees, result inspection, review gates and explicit Git finalization policy. Repository targeting can also be optional or disabled. [G01–G07](feature-inventory.md#g01) | Separate versioning the automation from isolating and integrating an agent's code changes. Use non-repository tasks when Git is irrelevant. |
| 🏢 **Shared operations** | Organizations, teams, roles, SSO, user/organization credentials, quotas, audit, triggers, scheduling and board dispatch. [E01–E10](feature-inventory.md#e01), [T01–T09](feature-inventory.md#t01), [K01–K07](feature-inventory.md#k01) | Compare multi-tenancy with multiple user accounts, and check what is included in the self-hosted edition. Match shared control-plane access with the actual execution isolation. |
| 🔐 **Controls and evidence** | Static graph diagnostics, typed `compute` outputs, backend-dependent permissions, budgets, secrets, run artifacts and evaluation recipes. [A08](feature-inventory.md#a08), [O03](feature-inventory.md#o03), [P01–P09](feature-inventory.md#p01), [U01–U11](feature-inventory.md#u01), [D05–D06](feature-inventory.md#d05) | Verify what the runtime enforces: general output validation is not enabled by product entry points in the reviewed build, permissions do not cover every shell path, and child-run budgets remain separate. |

</div>

**Compare implementations at the same level.** LangGraph is also an agent-oriented runtime; Deep Agents adds a harness with sandbox backends. Dify's new Agent includes a command/file sandbox in beta. Make and Windmill document conversational memory; Windmill also provides reusable and nested agents. These capabilities make session scope, supervision, deployment and the remaining integration work more informative than an “AI-native” label alone. [L1] [L8] [D11] [M15] [W2]

### Reading the product details

<div class="comparison-table" role="region" aria-label="Feature matrix — table 2" tabindex="0">

| ✅ Native | 🔌 Extension | 🛠 Integration | 🏷 Plan |
|---|---|---|---|
| Supplied by the product; configuration may be required. | Associated component. | Implementation using documented extension points. | Depends on edition or contract. |

</div>


In the detailed tables, products are rows and each column asks the same question of all ten solutions. A cell may combine several conditions.

> **Compare scope and effort.** A documented capability and a proposed integration can differ in coverage, effort and reliability. 🛠 describes the path supported by this study; it does not rule out another native solution.

<a id="authoring"></a>

## 🧩 1. Authoring, agents and integrations

Build workflows, customize agents and connect tools.

<div class="comparison-table" role="region" aria-label="Feature matrix — table 3" tabindex="0">

| Product | 🎨 Visual authoring | 📄 Workflow definition / portability | ⌨️ Deterministic code | 🤖 Agents and models | 🔌 Integrations and MCP |
|---|---|---|---|---|---|
| <img class="comparison-logo" src="./assets/logos/iterion.png" alt="" width="24"> **Iterion** | ✅ Native Studio | `.bot` sources, `.botz` bundles | Tool and `compute` nodes, commands, local connector `action:` nodes [I8] | Native agent/judge nodes, per-node backends/models, sessions, context and supported dynamic sub-agents | Forges, plugins, MCP client/server; local OpenAPI/Swagger connector packages, with broader catalog coverage and cloud transport still to extend [I1] [I2] [I8] |
| <img class="comparison-logo" src="./assets/logos/n8n.ico" alt="" width="24"> **n8n** | ✅ Native editor | Native JSON export; Git versioning 🏷 plan-dependent | JavaScript / Python | Agents, multi-agent and model selection | Application catalog; inbound and outbound MCP [N1] [N2] [N3] [N8] |
| <img class="comparison-logo" src="./assets/logos/make.ico" alt="" width="24"> **Make** | ✅ Scenario editor | JSON blueprint export | Make Code: JavaScript / Python | Make AI Agents, conversation history and knowledge files [M15] | Application catalog; MCP client for AI Agents and Make server [M1] [M2] [M3] [M6] [M7] |
| <img class="comparison-logo" src="./assets/logos/zapier.ico" alt="" width="24"> **Zapier** | ✅ Zap editor | Zap JSON export/import 🏷 Team/Enterprise [Z6] | JS/Python steps | AI by Zapier with tools 🏷 Professional+, Advanced/Premium model; Agents moving into Zaps [Z11] | Application catalog and MCP service [Z1] [Z7] [Z9] |
| <img class="comparison-logo" src="./assets/logos/activepieces.svg" alt="" width="24"> **Activepieces** | ✅ Flow editor | Flows and project releases | TypeScript steps | Run Agent steps and AI providers; standalone Agents/Chat are edition-scoped [A12] [A8] | Pieces, connections and MCP [A1] [A2] |
| <img class="comparison-logo" src="./assets/logos/dify.svg" alt="" width="24"> **Dify** | ✅ Native Studio | YAML DSL export | Python / JavaScript Code node | LLM/Agent nodes; new sandboxed Agent in beta [D11] [D12] | Tools, plugins; documented MCP publishing [D1] [D2] [D3] |
| <img class="comparison-logo" src="./assets/logos/flowise.svg" alt="" width="88"> **Flowise**<br>🗄 Archived | ✅ Assistant, Chatflow, Agentflow | JSON export/import; integration API/SDK [F5] | Custom JavaScript function | Agents, models and vector databases | Tools and MCP in AgentFlow V2 [F1] [F2] |
| <img class="comparison-logo" src="./assets/logos/langgraph.svg" alt="" width="105"> **LangGraph** | 🔌 Fleet for no-code agents; Studio for debugging code-defined graphs [L11] | Graph in application source code | Host-language functions | Native agent orchestration; models/tools through code or LangChain; associated Deep Agents harness [L8] | MCP adapters through the LangChain ecosystem [L1] [L2] [L3] |
| <img class="comparison-logo" src="./assets/logos/crewai.ico" alt="" width="24"> **CrewAI** | Code framework; platform visual Studio, including free Basic | Python and agent/task configuration | Python functions and tools | Specialized agents, Crews and Flows | Tools, MCP through `crewai-tools` [C1] [C2] [C9] |
| <img class="comparison-logo" src="./assets/logos/windmill.svg" alt="" width="24"> **Windmill** | ✅ Flow and app editor | Scripts, flows and YAML definitions | Multiple languages including Python, TS, Go and Bash | AI Agent steps, reusable/nested agents and conversation memory | Scripts/connectors; streamable HTTP MCP for agents [W1] [W2] |

</div>


> 📦 **Portability between engines.** An exported definition supports backup and review; running it in another engine requires adaptation. Exports may omit secrets, data or dependencies. [D2] [M2]

<a id="orchestration"></a>

## 🔄 2. Orchestration and execution control

Route steps, involve people, resume work and control consumption.

<div class="comparison-table" role="region" aria-label="Feature matrix — table 4" tabindex="0">

| Product | 🔀 Loops and routing | 🙋 Human intervention | 💾 Persistence and recovery | 📐 Structured inputs / outputs | 💰 AI budget controls |
|---|---|---|---|---|---|
| <img class="comparison-logo" src="./assets/logos/iterion.png" alt="" width="24"> **Iterion** | Rules/LLM routing, fan-out, data-driven loops and supported dynamic sub-agents | Human nodes, asynchronous questions, steering and live supervision | Graph checkpoints; eligible-state recovery; backend-dependent sessions | Static graph checks and typed `compute` outputs; general runtime output validation is not enabled [I7] | Tokens, estimated cost, duration, iterations; excludes sub-bots, with default finalization headroom [I1] [I3] |
| <img class="comparison-logo" src="./assets/logos/n8n.ico" alt="" width="24"> **n8n** | Flow control and agent composition | Tool-call approvals | Wait persists waits; not universal crash recovery | Structured inputs/outputs for AI steps | Max Iterations per agent; 🛠 run counter and enforcement in an HTTP AI service [N1] [N2] [N4] [N10] [N11] |
| <img class="comparison-logo" src="./assets/logos/make.ico" alt="" width="24"> **Make** | Routers, filters and iteration | 🛠 Data Store + decision + webhook | Business state across two scenarios; separate incomplete executions and retries | Named, typed scenario inputs/outputs | Platform credits; 🛠 run budget through an HTTP AI service [M1] [M4] [M9] [M10] [M12] [M13] |
| <img class="comparison-logo" src="./assets/logos/zapier.ico" alt="" width="24"> **Zapier** | Paths, filters and Looping | Human in the Loop 🏷 Professional and above | Wait until decision/expiration; separate error replay | Typed inputs and structured AI by Zapier outputs | Pause above 75 tasks/run; 🛠 token/USD budget through an external AI API [Z1] [Z2] [Z8] [Z9] [Z10] |
| <img class="comparison-logo" src="./assets/logos/activepieces.svg" alt="" width="24"> **Activepieces** | Branches, loops and sub-flows | Approvals and waitpoints | Durable waitpoints survive worker restarts | Action properties; 🛠 overall contract validation in a Code step | Supported AI gateways; 🛠 per-run accounting and enforcement to integrate [A1] [A3] [A4] [A9] [A10] |
| <img class="comparison-logo" src="./assets/logos/dify.svg" alt="" width="24"> **Dify** | If/Else, Iteration and Loop | Human Input node | Persisted human pause/resume within documented recovery mechanisms | Declared Code outputs; structured LLM outputs depend on model | Per-model settings; 🛠 token/cost counter and enforcement through AI plugin/API [D3] [D4] [D5] [D9] [D10] |
| <img class="comparison-logo" src="./assets/logos/flowise.svg" alt="" width="88"> **Flowise**<br>🗄 Archived | Branches, shared state and bounded Loop | Human Input node; tool approval | Human-wait checkpoints and recovery after restart | Flow state and schema-based LLM JSON; overall validation to compose | Loop/context limits; 🛠 cumulative budget through an AI tool/API [F2] [F8] |
| <img class="comparison-logo" src="./assets/logos/langgraph.svg" alt="" width="105"> **LangGraph** | Cycles, conditional routing and parallel branches | Interrupts and state editing | Checkpointers; persistent backend required | Graph state schema; business validation to build | 🛠 Shared state counter and checks before AI calls [L1] [L4] [L5] |
| <img class="comparison-logo" src="./assets/logos/crewai.ico" alt="" width="24"> **CrewAI** | Event-driven Flows, routing and tasks | Human input; async feedback provider with notification/callback transport to implement [C14] | `@persist`, checkpoints and automatic persistence of pending human feedback [C14] | State models and structured outputs by configuration | Per-agent `max_iter`, duration/rate; 🛠 shared counter and LLM hook enforcement [C1] [C3] [C4] [C5] [C10] |
| <img class="comparison-logo" src="./assets/logos/windmill.svg" alt="" width="24"> **Windmill** | Flows with branches, errors and agent steps | Suspend / Approval; forms 🏷 Cloud or Enterprise | Suspension and step retries; agent state can use a volume | AI Agent output JSON Schema | Per-agent iteration and completion limits; 🛠 cumulative budget in an AI script/API [W1] [W2] [W3] [W4] |

</div>


> 🔎 **Validate on your workflow:** parallel execution semantics, recovery during an interrupted call and the correctness of generated content beyond schema validation.

> 💰 **Iterion budget scope.** Budgets are native, but sub-bot totals are separate from the parent's budget. Default finalization headroom is 10% and can be disabled. Cost accounting depends on backend information. Include these settings when measuring total mission cost. [I1]

<a id="technical"></a>

## 🛠 3. Technical environment and repository work

Isolate execution, manage Git, prepare dependencies and inspect work.

<div class="comparison-table" role="region" aria-label="Feature matrix — table 5" tabindex="0">

| Product | 🛡 Code / agent isolation | 🌿 Git worktree and result finalization | 📦 Runtime dependencies | ⚙️ Distributed execution | 🔭 Technical observability |
|---|---|---|---|---|---|
| <img class="comparison-logo" src="./assets/logos/iterion.png" alt="" width="24"> **Iterion** | Docker/Podman/Kubernetes drivers; behavior depends on backend and launch | Native `worktree: auto`, branch and merge policy | Bot + repository Devbox; sandbox images | Platform: NATS, runners, KEDA, MongoDB/S3 | Events, artifacts, live console, session views, Prometheus and OTLP [I2] [I4] [I5] |
| <img class="comparison-logo" src="./assets/logos/n8n.ico" alt="" width="24"> **n8n** | External task runners for Code; 🛠 external service for per-mission shell/repository [N10] | 🛠 Git scripts through self-hosted Execute Command, or HTTP service [N9] [N10] | Extensible runner image; explicitly allowed packages | Queue mode: Redis, shared database and workers | Execution history, worker metrics; log streaming 🏷 plan-dependent [N3] [N5] [N6] |
| <img class="comparison-logo" src="./assets/logos/make.ico" alt="" width="24"> **Make** | Make Code JS/Python sandbox 🏷 paid; 🛠 external shell/repository service | 🛠 Remote Git scripts through SSH or HTTP [M11] [M12] | Standard libraries; custom libraries 🏷 Enterprise [M8] | SaaS engine; separate local connection agent | History, log search and audit 🏷 plan-dependent [M1] |
| <img class="comparison-logo" src="./assets/logos/zapier.ico" alt="" width="24"> **Zapier** | Isolated code, plan-dependent time/memory; 🛠 external shell/repository service | 🛠 Git executor API called through Webhooks [Z10] | Code runtime; Git toolchain in external service [Z7] | SaaS engine, without self-hosted engine workers | Zap History; observability 🏷 plan-dependent [Z1] [Z2] |
| <img class="comparison-logo" src="./assets/logos/activepieces.svg" alt="" width="24"> **Activepieces** | V8 and/or namespace modes; 🛠 external shell/repository service | 🛠 Git/sandbox executor through an HTTP-client piece [A9] | Piece versions and dependencies; npm depends on sandbox mode | App, Redis queue, workers and storage | Runs, analytics and audit 🏷 plan-dependent [A1] [A5] [A6] |
| <img class="comparison-logo" src="./assets/logos/dify.svg" alt="" width="24"> **Dify** | Restricted Code sandbox; new Agent command/file sandbox in beta [D11] | 🛠 Git tools and finalization policy; new Agent can execute commands, or use HTTP/plugin [D9] [D12] | Code: predefined libraries; new Agent: package installation [D11] | Celery for streaming and resumes; non-streaming in the API in the described version [D8] | Logs, dashboard and tracing integrations [D1] [D3] |
| <img class="comparison-logo" src="./assets/logos/flowise.svg" alt="" width="88"> **Flowise**<br>🗄 Archived | Associated E2B interpreter; 🛠 external shell/repository service [F7] | 🛠 Custom tool calling a Git/sandbox executor [F8] | Libraries available to JS runtime; hosting configuration | Documented message queue and workers | Traces, analytics and evaluations [F1] [F2] [F3] |
| <img class="comparison-logo" src="./assets/logos/langgraph.svg" alt="" width="105"> **LangGraph** | 🔌 Deep Agents sandbox backend for shell/files; separate from graph runtime [L8] | 🛠 Git functions/tools and finalization lifecycle to build | Application environment; sandbox backend/image when using Deep Agents | 🔌 Agent Server / LangSmith deployment, or own operations | State streaming; LangSmith Studio, tracing and evaluations [L1] [L2] [L4] |
| <img class="comparison-logo" src="./assets/logos/crewai.ico" alt="" width="24"> **CrewAI** | Associated E2B execution/file tools; legacy CodeInterpreterTool deprecated [C13] | 🛠 Python Git tools and finalization lifecycle to build | Python environment and selected tools | Team-operated framework; Enterprise private platform with replicated workers [C11] | Events and tracing integrations; console 🏷 plan-dependent [C1] [C6] [C7] [C9] |
| <img class="comparison-logo" src="./assets/logos/windmill.svg" alt="" width="24"> **Windmill** | NSJAIL and namespaces; AI Sandbox with persistent volumes | 🛠 Git scripts and merge policy in flows; AI Sandbox for files | Script dependencies; documented Python resolution and lockfile | Worker fleet, groups and access separation | Job logs, flow state and monitoring interfaces [W1] [W5] [W6] [W7] |

</div>


> 🛡 **Compare sandbox boundaries.** An isolated Python node, an agent sandbox with shell access and a platform pod protect different resources. n8n documents external Code runners; Dify separates restricted Code from its new Agent sandbox; Deep Agents supplies sandbox backends alongside LangGraph; Windmill has an AI Sandbox. Compare file lifetime, network access and recovery for the selected mechanism. [N5] [D3] [D11] [L8] [W5]

Iterion's `auto` mode can fall back to unsandboxed execution depending on the host; an explicit sandbox request behaves differently. The documented cloud path currently uses the runner pod as its isolation boundary. Codex delegation does not support Iterion's external sandbox. [I2] [I4]

<a id="deployment"></a>

## ☁️ 4. Deployment, operations and governance

Choose hosting, interfaces and team controls. Iterion's self-hosted platform includes multi-tenant organizations, teams and roles; evaluate these separately from simple user login support.

<div class="comparison-table" role="region" aria-label="Feature matrix — table 6" tabindex="0">

| Product | ☁️ Self-hosting / managed service | ⚡ API and triggers | 🌿 Git for workflow lifecycle | 👥 Team governance | 📜 License / commercial scope |
|---|---|---|---|---|---|
| <img class="comparison-logo" src="./assets/logos/iterion.png" alt="" width="24"> **Iterion** | Local and self-hostable platform; **managed service on request — subject to assessment** | CLI/SDK, API, cron, dispatcher, webhooks | `.bot` in Git; bundles and versions | Native multi-tenant organizations/teams, roles, SSO, bound secrets, quotas and audit | MIT; experimental status [I2] [I5] [I6] |
| <img class="comparison-logo" src="./assets/logos/n8n.ico" alt="" width="24"> **n8n** | Self-hosting and n8n Cloud | API, CLI, triggers and webhooks | Git/environments 🏷 plan-dependent | Community: user accounts, without shared projects or workflow/credential sharing; SSO and advanced governance 🏷 plan-dependent | Sustainable Use + separate Enterprise license [N1] [N3] [N7] [N12] [N13] |
| <img class="comparison-logo" src="./assets/logos/make.ico" alt="" width="24"> **Make** | SaaS; local connection agent, no standalone engine in the reviewed offering | API and scheduled/triggered scenarios | JSON blueprints; 🛠 Git/CI pipeline with Make CLI [M14] | Teams and Enterprise features 🏷 plan-dependent | Commercial service; credit quotas [M1] [M2] [M5] |
| <img class="comparison-logo" src="./assets/logos/zapier.ico" alt="" width="24"> **Zapier** | SaaS; no on-premise version | Triggers, webhooks and developer tools 🏷 plan-dependent | JSON export 🏷 Team/Enterprise; Git archiving and redeployment to arrange [Z6] | Workspaces, connections and controls 🏷 plan-dependent | Commercial service; task allocation [Z1] [Z4] |
| <img class="comparison-logo" src="./assets/logos/activepieces.svg" alt="" width="24"> **Activepieces** | Cloud and own infrastructure | Triggers, webhooks and MCP tools | Git and release promotion when Environments is enabled in the plan | Paid projects and roles; advanced audit/secrets 🏷 plan-dependent [A11] | MIT core; separate Enterprise components [A1] [A2] [A7] |
| <img class="comparison-logo" src="./assets/logos/dify.svg" alt="" width="24"> **Dify** | Cloud, VPC and self-hosting | Application API; MCP publishing | YAML DSL export; Git pipeline to arrange | Community: one workspace; multiple workspaces, SSO and advanced governance 🏷 Enterprise [D7] | Modified Apache 2.0 with additional conditions [D1] [D2] [D6] |
| <img class="comparison-logo" src="./assets/logos/flowise.svg" alt="" width="88"> **Flowise**<br>🗄 Archived | Self-hosting; Cloud still listed; repository archived, official community presence ended [F9] | API, SDK and embedded chat | Versionable JSON exports; 🛠 Git pipeline to arrange [F5] | Teams/workspaces; OIDC SSO 🏷 Enterprise [F6] | Apache 2.0 core; separate commercial components [F1] [F3] [F4] |
| <img class="comparison-logo" src="./assets/logos/langgraph.svg" alt="" width="105"> **LangGraph** | Self-operated library; associated LangSmith deployment | Runtime calls; API through Agent Server | Application code in the team's Git | Application-defined authorization; associated LangSmith Enterprise RBAC [L9] | MIT runtime; separate service terms [L1] [L2] [L4] [L6] |
| <img class="comparison-logo" src="./assets/logos/crewai.ico" alt="" width="24"> **CrewAI** | Self-operated framework; Basic/Enterprise cloud platform | Python calls; platform deployments and triggers | Code and configuration in Git | Platform console; Enterprise SSO and RBAC | MIT framework; separate platform [C1] [C7] [C8] [C9] |
| <img class="comparison-logo" src="./assets/logos/windmill.svg" alt="" width="24"> **Windmill** | Self-hostable; cloud offering | API, webhooks, scheduler and CLI | Git sync 🏷 Cloud/Enterprise; Community for workspaces up to two users [W12] | Workspace roles and item ACLs; Community limit of three workspaces [W10] [W11] | AGPL/Apache by file; distinguish Enterprise and distributed binaries [W1] [W6] [W8] |

</div>


### 🏢 Team access and multi-tenancy {#team-access}

**Iterion includes organizations, teams and their access controls in its self-hosted platform.** Compare this with the exact edition and resource boundary of each alternative. A shared login, a shared folder and an organization boundary cover different needs.

The availability row checks **team/project access controls on infrastructure you operate**. The table also records SaaS collaboration for context; Make and Zapier remain “no” for that self-hosted criterion.

<div class="comparison-table comparison-team-access" role="region" aria-label="Team access and tenancy by product" tabindex="0">

| Product | Team and resource boundary | Access conditions |
|---|---|---|
| **Iterion** | Organizations, teams and roles; tenant-scoped resources, credentials, quotas and audit. | Included in the self-hosted platform. Execution isolation still depends on runner and sandbox configuration. [I5] |
| **n8n** | Projects group workflows and credentials with project roles. | Paid projects and sharing; Community has user accounts but excludes these collaboration features. This does not establish independent organization tenants. [N12] [N14] |
| **Make** | Teams own scenarios, connections and data within an organization. | Multiple teams and team roles require Teams or above; hosted engine. [M16] |
| **Zapier** | Shared folders grant collaboration access; workspaces separate users, Zaps, connections and task allocations. | Folder sharing: Team/Enterprise. Multiple workspaces: Enterprise; hosted engine. [Z12] [Z13] |
| **Activepieces** | Projects scope flows, connections and tables to their members. | Projects are a paid feature; platform administrators and operators retain wider access. [A11] |
| **Dify** | Workspaces group applications, knowledge and members. | Self-hosted Community: one workspace. Multiple workspaces and enterprise management require Enterprise. [D7] |
| **Flowise** 🗄 | Workspaces partition resources with workspace roles. | Cloud/Enterprise feature; self-hosted use requires Enterprise. The end-of-life condition applies. [F10] [F9] |
| **LangGraph** | Associated LangSmith provides organizations, workspaces and RBAC. | RBAC and self-hosted LangSmith require Enterprise. These platform controls are separate from the graph library and application authorization. [L9] [L10] |
| **CrewAI** | Associated private platform provides organizations and permissions; optional namespaces separate organization workloads. | Enterprise platform, separate from the Python framework. Namespace isolation needs explicit setup. [C11] [C12] |
| **Windmill** | Workspaces scope users and resources; roles and item-level access control govern collaboration. | Community supports workspaces with a global limit of three; advanced controls and Git deployment depend on edition. [W10] [W11] [W12] |

</div>

**Check the boundary your application needs.** These are platform access controls for builders and operators. They do not automatically provide end-user authorization, isolated networks or a sandbox per customer. Administrators may retain cross-workspace access. For Iterion, validate [tenant configuration](feature-inventory.md#e01), [bound secrets](feature-inventory.md#p03) and [execution isolation](feature-inventory.md#p02) together.


> 🌿 **Two different Git lifecycles.** One versions and reviews the automation itself. The other isolates an agent's repository changes, preserves the result and determines how to integrate it. A GitHub connector or Git export alone does not establish the latter.

**LangGraph and CrewAI are compared as frameworks**, with associated platforms explicitly identified. Managed-service features are not automatically attributed to the open-source library; Enterprise features are not automatically attributed to community editions.

> 🗄 **Flowise end of life: August 31, 2026.** The repository was archived August 13. Software features remain documented; the Cloud listing does not guarantee current service or support. [Official announcement][F9].

## 🎯 Why Iterion's integrated approach matters

Iterion is **AI-centric by design**: agent and judge nodes, sessions, context, supervision and correction loops sit within the workflow engine. Developers can combine these primitives with custom scripts, plugins, MCP and APIs, then use the same platform for repository work, budgets, recovery and multi-tenant operations. The architectural choice is how much of that lifecycle you want supplied by the engine and how much you want to implement in application code.

Evaluate a complete mission: integration effort, execution behavior, accepted output and total cost, including models, infrastructure and human review. Feature counts cannot capture these tradeoffs; proposed 🛠 integrations need implementation and testing.

<div class="comparison-cta">

**Try the full lifecycle on a real mission.**

[Get started with Iterion](../quickstart.md) · [Explore the bot catalog](../examples.md) · [Plan your pilot](index.md#try-iterion)

</div>

<a id="sources"></a>

## 📚 Official sources

Product logos and icons: [visual asset provenance](assets/logos/README.md).

Iterion references are pinned to commit `bbc1ddc7844b81582b6dfc4d0b7f381680bb284b`, except the local connector foundation [I8], reviewed at `603d2a1e3314252dd2995e3bfaa4a7394e13093b`. Other sources were reviewed on September 11, 2026. CrewAI documentation displays version `v1.15.21`; its CodeInterpreterTool page announces deprecation while retaining historical examples, which are excluded from current capabilities here.

- **Iterion**: [I1 — DSL][I1]; [I2 — current state][I2]; [I3 — recovery][I3]; [I4 — sandbox][I4]; [I5 — platform][I5]; [I6 — license][I6]. Additional detail: [I7 — output validation scope][I7]; [I8 — local connector foundation][I8]. More: [worktrees and merge](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/adr/064-worktree-finalization-requires-delegated-authority.md), [observability](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/observability/README.md), [planned catalog](https://github.com/SocialGouv/iterion/issues/1072).
- **n8n**: [N1 — documentation][N1]; [N2 — agents and controls][N2]; [N3 — recorded plan conditions][N3]; [N4 — Wait][N4]; [N5 — task runners][N5]; [N6 — queue mode][N6]; [N7 — license][N7]; [N12 — Community scope][N12]; [N13 — user management][N13].
- **Make**: [M1 — recorded plan conditions][M1]; [M2 — blueprints][M2]; [M3 — AI Agents][M3]; [M15 — agent memory and knowledge][M15]; [M4 — incomplete executions][M4].
- **Zapier**: [Z1 — recorded plan conditions][Z1]; [Z2 — replay][Z2]; [Z3 — Human in the Loop][Z3]; [Z11 — AI tools and access conditions][Z11].
- **Activepieces**: [A1 — documentation][A1]; [A2 — releases and Git][A2]; [A3 — flow control][A3]; [A4 — MCP tools and flow structure][A4]; [A5 — sandboxing][A5]; [A6 — architecture][A6]; [A7 — license][A7].
- **Dify**: [D1 — documentation][D1]; [D2 — applications and DSL][D2]; [D3 — Code][D3]; [D4 — Human Input][D4]; [D5 — LLM][D5]; [D6 — license][D6]; [D11 — new Agent][D11]; [D12 — Agent node scope][D12].
- **Flowise**: [F1 — introduction][F1]; [F2 — AgentFlow V2][F2]; [F3 — Cloud record][F3]; [F4 — license][F4].
- **LangGraph**: [L1 — runtime][L1]; [L2 — LangSmith Studio][L2]; [L3 — MCP adapters][L3]; [L4 — persistence][L4]; [L10 — self-hosted platform][L10]; [L5 — Graph API][L5]; [L6 — license][L6]; [L8 — Deep Agents sandbox backends][L8]; [L11 — Fleet no-code authoring][L11].
- **CrewAI**: [C1 — concepts][C1]; [C2 — MCP][C2]; [C3 — checkpoints][C3]; [C4 — human control][C4]; [C5 — agents][C5]; [C6 — CodeInterpreterTool deprecation][C6]; [C7 — framework and platform][C7]; [C8 — license][C8].
- **Windmill**: [W1 — platform][W1]; [W2 — AI Agents][W2]; [W3 — approvals][W3]; [W4 — retries][W4]; [W5 — AI Sandbox][W5]; [W6 — isolation][W6]; [W7 — Python dependencies][W7]; [W8 — licenses][W8].

**Additional checks:** [n8n — JSON export][N8]; [Make — on-premise agent][M5]; [Zapier — support statement on on-premise][Z4]; [Zapier — SSO setup][Z5]; [Activepieces — SSO][A8]; [LangSmith — authentication][L7]; [CrewAI — Studio plan conditions][C9]; [Windmill — SSO][W9].

[I1]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/dsl.md
[I2]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/current-state.md
[I3]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/resume.md
[I4]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/sandbox.md
[I5]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/cloud-overview.md
[I6]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/LICENSE
[N1]: https://docs.n8n.io/
[N2]: https://docs.n8n.io/integrations/builtin/cluster-nodes/root-nodes/n8n-nodes-langchain.agent/tools-agent
[N3]: methodology.md#conditions-n8n
[N4]: https://raw.githubusercontent.com/n8n-io/n8n-docs/main/docs/integrations/builtin/core-nodes/n8n-nodes-base.wait.md
[N5]: https://docs.n8n.io/deploy/host-n8n/configure-n8n/set-up-task-runners
[N6]: https://docs.n8n.io/deploy/host-n8n/configure-n8n/scaling/enable-queue-mode
[N7]: https://raw.githubusercontent.com/n8n-io/n8n/master/LICENSE.md
[M1]: methodology.md#conditions-make
[M2]: https://help.make.com/blueprints
[M3]: https://help.make.com/make-ai-agent-new
[M4]: https://help.make.com/incomplete-executions
[Z1]: methodology.md#conditions-zapier
[Z2]: https://help.zapier.com/hc/en-us/articles/19220226086797-What-is-replay
[Z3]: https://help.zapier.com/hc/en-us/articles/38731463206029-Request-approval-to-keep-your-workflow-running-with-Human-in-the-Loop
[A1]: https://www.activepieces.com/docs/getting-started/introduction
[A2]: https://www.activepieces.com/docs/admin-guide/guides/project-releases
[A3]: https://www.activepieces.com/docs/build-pieces/piece-reference/flow-control
[A4]: https://www.activepieces.com/docs/mcp/tools
[A5]: https://www.activepieces.com/docs/install/architecture/sandboxing
[A6]: https://www.activepieces.com/docs/install/architecture/overview
[A7]: https://raw.githubusercontent.com/activepieces/activepieces/main/LICENSE
[D1]: https://docs.dify.ai/en/home
[D2]: https://docs.dify.ai/en/cloud/use-dify/workspace/app-management
[D3]: https://docs.dify.ai/en/cloud/use-dify/nodes/code
[D4]: https://docs.dify.ai/en/cloud/use-dify/nodes/human-input
[D5]: https://docs.dify.ai/en/cloud/use-dify/nodes/llm
[D6]: https://raw.githubusercontent.com/langgenius/dify/main/LICENSE
[F1]: https://docs.flowiseai.com/
[F2]: https://docs.flowiseai.com/using-flowise/agentflowv2
[F3]: methodology.md#conditions-flowise
[F4]: https://raw.githubusercontent.com/FlowiseAI/Flowise/main/LICENSE.md
[L1]: https://docs.langchain.com/oss/python/langgraph/overview
[L2]: https://docs.langchain.com/langsmith/studio
[L3]: https://docs.langchain.com/oss/python/langchain/mcp
[L4]: https://docs.langchain.com/oss/python/langgraph/persistence
[L5]: https://docs.langchain.com/oss/python/langgraph/graph-api
[L6]: https://raw.githubusercontent.com/langchain-ai/langgraph/main/LICENSE
[C1]: https://docs.crewai.com/core-concepts/Agents
[C2]: https://docs.crewai.com/v1.15.21/en/mcp/overview
[C3]: https://docs.crewai.com/v1.15.21/en/concepts/checkpointing
[C4]: https://docs.crewai.com/v1.15.21/en/learn/human-in-the-loop
[C5]: https://docs.crewai.com/v1.15.21/en/concepts/agents
[C6]: https://docs.crewai.com/v1.15.21/en/tools/ai-ml/codeinterpretertool
[C7]: https://docs.crewai.com/
[C8]: https://raw.githubusercontent.com/crewAIInc/crewAI/main/LICENSE
[W1]: https://www.windmill.dev/docs/intro
[W2]: https://www.windmill.dev/docs/core_concepts/ai_agents
[W3]: https://www.windmill.dev/docs/flows/flow_approval
[W4]: https://www.windmill.dev/docs/flows/retries
[W5]: https://www.windmill.dev/docs/core_concepts/ai_sandbox
[W6]: https://www.windmill.dev/docs/advanced/security_isolation
[W7]: https://www.windmill.dev/docs/advanced/dependencies_in_python
[W8]: https://raw.githubusercontent.com/windmill-labs/windmill/main/LICENSE

[N8]: https://docs.n8n.io/build/manage-workflows/export-and-import
[M5]: https://help.make.com/on-premise-agent
[Z4]: https://community.zapier.com/how-do-i-3/is-zapier-only-cloud-base-or-on-premise-too-18528
[Z5]: https://help.zapier.com/hc/en-us/articles/8496279747085-Set-up-single-sign-on-with-SAML
[A8]: methodology.md#conditions-activepieces
[L7]: https://docs.langchain.com/langsmith/authentication-methods
[C9]: methodology.md#conditions-crewai
[W9]: https://www.windmill.dev/docs/enterprise/onboarding

[N9]: https://docs.n8n.io/integrations/builtin/core-nodes/n8n-nodes-base.executecommand
[N10]: https://docs.n8n.io/integrations/builtin/core-nodes/n8n-nodes-base.httprequest
[N11]: https://docs.n8n.io/integrations/builtin/cluster-nodes/root-nodes/n8n-nodes-langchain.agent/tools-agent
[M6]: https://help.make.com/make-ai-agents-new-mcp-tools-are-now-available
[M7]: https://help.make.com/introduction-to-mcp
[M8]: https://help.make.com/the-make-code-app-is-available
[M9]: https://help.make.com/data-stores
[M10]: https://help.make.com/webhooks
[M11]: https://apps.make.com/ssh
[M12]: https://apps.make.com/http
[M13]: https://help.make.com/scenario-inputs-and-outputs/
[M14]: https://help.make.com/the-make-cli-is-now-live
[Z6]: https://help.zapier.com/hc/en-us/articles/8496308481933-Import-and-export-Zap-workflows-in-your-Team-or-Enterprise-account
[Z7]: https://help.zapier.com/hc/en-us/articles/8496310939021-Use-JavaScript-code-in-Zap-workflows
[Z8]: https://help.zapier.com/hc/en-us/articles/38731463206029-Request-approval-to-keep-your-workflow-running-with-Human-in-the-Loop
[Z9]: https://help.zapier.com/hc/en-us/articles/47402591569805-Migrating-from-Agents-to-AI-by-Zapier
[Z10]: https://help.zapier.com/hc/en-us/articles/8496326446989-Send-webhooks-in-Zap-workflows
[A9]: https://github.com/activepieces/activepieces/blob/main/.agents/skills/piece-builder/common-patterns.md
[A10]: https://www.activepieces.com/docs/admin-guide/guides/setup-ai-providers
[D7]: methodology.md#conditions-dify
[D8]: https://github.com/langgenius/dify/discussions/32245
[D9]: https://docs.dify.ai/en/cloud/use-dify/nodes/http-request
[D10]: https://docs.dify.ai/en/develop-plugin/features-and-specs/plugin-types/model-schema
[F5]: https://docs.flowiseai.com/migration-guide/cloud-migration
[F6]: https://docs.flowiseai.com/configuration/sso
[F7]: https://docs.flowiseai.com/integrations/langchain/tools/python-interpreter
[F8]: https://docs.flowiseai.com/integrations/langchain/tools/custom-tool
[F9]: https://github.com/FlowiseAI/Flowise/discussions/6727
[C10]: https://docs.crewai.com/v1.15.21/en/learn/llm-hooks
[C11]: https://enterprise-docs.crewai.com/reference/chart-values/worker

</div>

[N12]: https://github.com/n8n-io/n8n-docs/blob/main/docs/deploy/host-n8n/community-edition-features.md
[N13]: https://github.com/n8n-io/n8n-docs/blob/main/docs/administer/manage-users-and-access/README.md

[I7]: feature-inventory.md#a08
[L8]: https://docs.langchain.com/oss/python/deepagents/sandboxes
[D11]: https://docs.dify.ai/en/cloud/use-dify/build/new-agent/overview
[D12]: https://docs.dify.ai/en/cloud/use-dify/nodes/agent
[M15]: https://help.make.com/make-ai-agents-new-best-practices
[Z11]: https://help.zapier.com/hc/en-us/articles/45863491098893-Add-tools-to-your-AI-by-Zapier-step

[N14]: https://github.com/n8n-io/n8n-docs/blob/main/docs/administer/manage-users-and-access/set-permissions-and-roles-rbac/see-available-roles.md
[M16]: https://help.make.com/teams
[Z12]: https://help.zapier.com/hc/en-us/articles/22330977078157-Collaborate-with-members-of-your-Team-or-Enterprise-account
[Z13]: https://help.zapier.com/hc/en-us/articles/34713530114573-Zapier-account-organization-and-workspaces
[A11]: https://www.activepieces.com/docs/admin-guide/guides/structure-projects
[A12]: https://www.activepieces.com/docs/about/changelog
[F10]: https://docs.flowiseai.com/using-flowise/workspaces
[L9]: https://docs.langchain.com/langsmith/user-management
[L10]: https://docs.langchain.com/langsmith/self-hosted
[C12]: https://enterprise-docs.crewai.com/features/multi-org-namespaces
[W10]: https://www.windmill.dev/docs/core_concepts/roles_and_permissions
[W11]: https://www.windmill.dev/docs/advanced/dev_workspaces
[W12]: https://www.windmill.dev/docs/advanced/canonical_deployment_setups

[I8]: feature-inventory.md#x09
[L11]: https://docs.langchain.com/langsmith/fleet
[C13]: https://docs.crewai.com/v1.15.21/en/tools/ai-ml/e2bsandboxtools
[C14]: https://docs.crewai.com/v1.15.21/en/learn/human-feedback-in-flows#async-human-feedback-non-blocking
