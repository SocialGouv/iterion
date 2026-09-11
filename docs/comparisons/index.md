---
title: "Iterion and alternatives"
description: "Compare AI workflow architectures, developer experience and deployment options across Iterion and nine alternatives."
aside: false
pageClass: "comparison-page"
---

<div lang="en">

# 🧭 Iterion and alternatives: choose your agent workflow architecture

Updated September 11, 2026.

🔍 **Compare the details:** [10 products, 21 product and technical features](feature-matrix.md).

✅ [See which features each product provides](feature-matrix.md#availability).

📚 [Explore Iterion: 140 criteria, 16 categories and 35 bots](feature-inventory.md).

**Iterion is AI-centric by design: agents, their context and their execution lifecycle are at the heart of the workflow engine.** It turns your way of working with AI into versioned, executable workflows with native agents and judges, session controls, controlled correction loops, supervision, budgets and recovery. Developers can use these building blocks for custom agent applications, research, document production, business operations and repository work. [Iterion overview](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/README.md).

Iterion combines a programmable workflow language with native agent execution and a shared control plane. Compare how each product handles state, context, tools, supervision and delivery, and which parts your team will configure or implement. The graph can define the outer process while agents adapt their work inside it.

## 🎯 Start with the outcome you need

<div class="comparison-table comparison-choice" role="region" aria-label="Choosing an agent workflow architecture — table 1" tabindex="0">

| Your main goal | How Iterion addresses it | Main fit check |
|---|---|---|
| 🧠 Build a highly customized agent application | Compose `.bot` graphs, scripts, tools and sub-bots; let supported agents orchestrate dynamically inside a node. | Graph and state requirements, backend capabilities, application embedding and API integration. |
| 🔎 Research, analyze documents or produce reports | Combine web/MCP tools, document memory, specialist agents, human input and file deliverables. A Git repository can be optional. | Source access, retrieval quality, document formats and the interface your users need. |
| ⚡ Automate a business process | Use schedules or webhooks, deterministic code, agents and human decisions in one workflow. | Required application actions and credentials; broad connector coverage still requires integration work. |
| 🌿 Develop, review or maintain a repository | Start from catalog bots, isolate changes in worktrees, run checks and apply a review/finalization policy. | Repository conventions, available tooling and acceptance criteria. |
| ⌨️ Combine scripts and agent reasoning | Use tool nodes and typed `compute` steps alongside agents; pass structured data through the graph. | Dependency setup, execution isolation and explicit validation of model-produced data. |
| 🏢 Run workflows for several teams or organizations | Use the self-hosted multi-tenant platform with roles, credentials, quotas and audit. | Tenant boundaries, operational ownership and required service commitments. |

</div>


Use the product comparison below to assess the same mission against alternative architectures. The decision depends on implementation effort, operational control and the accepted result.

### 💡 When Iterion belongs on your shortlist

**Evaluate Iterion when your workflow needs:**

- **Agents as core building blocks:** model and backend selection, session continuity, context management, tool access and structured handoffs.
- **Custom application logic:** combine the `.bot` DSL with scripts, reusable groups, sub-bots, plugins, MCP and API/SDK integration. Explicit graph boundaries can coexist with dynamic agent decisions.
- **Controlled iteration:** define review and correction loops, human decisions, supervision, budgets and recovery within the engine.
- **A path from individual development to a shared platform:** start locally, then use organizations, teams and roles in the self-hosted multi-tenant platform.

The catalog includes development and review methods as well as documentation, monitoring, triage and planning. Use these as starting points for your own workflows. [Explore the methods](feature-inventory.md#bots).

## ⚖️ How the approaches compare

<div class="comparison-table" role="region" aria-label="Choosing an agent workflow architecture — table 2" tabindex="0">

| Product | Documented approach | What Iterion brings to this use case |
|---|---|---|
| <img class="comparison-logo" src="./assets/logos/iterion.png" alt="" width="24"> **Iterion** | Agent-centered workflow engine: declarative graphs, custom code, session controls, dynamic orchestration on supported backends and a multi-tenant platform. | Native controls for agent execution, supervision, knowledge reuse, human collaboration and recovery; Git delivery is one supported workflow. [Execution capabilities](feature-matrix.md#agent-depth). |
| <img class="comparison-logo" src="./assets/logos/n8n.ico" alt="" width="24"> **n8n** | Visual application automation with tool-using agents, agent composition, memory integrations, MCP and human controls. | Per-node execution backends, explicit session handoffs, supervision and mission controls. Iterion's self-hosted multi-tenant platform includes organizations and teams; n8n Community excludes shared projects and workflow/credential sharing. [Edition scope](methodology.md#conditions-n8n). |
| <img class="comparison-logo" src="./assets/logos/make.ico" alt="" width="24"> **Make** | Application scenarios with routing, deterministic modules and AI Agents using tools, knowledge and conversation IDs. | An extensible engine to own and operate agent workflows, with backend selection, supervision, recovery and reusable method packages. Compare the exact application integrations your mission needs. |
| <img class="comparison-logo" src="./assets/logos/zapier.ico" alt="" width="24"> **Zapier** | Application automation with autonomous tool use, knowledge access and optional tool approvals inside AI by Zapier steps. | Versioned agent workflows, execution-backend choice and native mission controls that can run on infrastructure you operate. Compare tool coverage and workflow ownership. |
| <img class="comparison-logo" src="./assets/logos/activepieces.svg" alt="" width="24"> **Activepieces** | Flows, agent steps, MCP, approvals and self-hosting; standalone Agents/Chat and team governance have distinct edition requirements. | An integrated agent execution lifecycle: sessions, supervision, document memory and review/correction, with optional repository delivery. Compare these controls on the selected backend and edition. [Team access](feature-matrix.md#team-access). |
| <img class="comparison-logo" src="./assets/logos/dify.svg" alt="" width="24"> **Dify** | Visual AI workflows, knowledge retrieval and agents. Its new Agent adds skills and a command/file sandbox in beta. | Multiple execution backends, active supervision, scoped document memory and explicit mission controls. Compare knowledge ingestion, agent runtime, workflow packaging and deployment needs. [Agent scope](feature-matrix.md#technical). |
| <img class="comparison-logo" src="./assets/logos/flowise.svg" alt="" width="88"> **Flowise** | Visual chatflows and agentflows with state, loops, human input and checkpoints; the project has reached end of life. | A candidate engine for maintaining and extending those agent workflows, with native execution and team operations. Reproduce the required behavior and migrate tools/data explicitly. [Lifecycle status](methodology.md#conditions-flowise). |
| <img class="comparison-logo" src="./assets/logos/langgraph.svg" alt="" width="105"> **LangGraph** | Agent-oriented runtime for stateful graphs combining deterministic and model-driven steps; associated Deep Agents and LangSmith components extend the stack. | A declarative, extensible engine with native sessions, supervision and shared operations. Compare the runtime contract and extension model with application-code orchestration; assess associated components separately. [Architecture details](feature-matrix.md#authoring). |
| <img class="comparison-logo" src="./assets/logos/crewai.ico" alt="" width="24"> **CrewAI** | Agent-oriented Python framework with specialized crews, explicit flows and persistence; a separate platform provides additional operating interfaces. | A workflow language and integrated agent controls for custom applications, with reusable methods and multi-tenant operation. Compare composition, state handling and the deployment components you need. [Architecture details](feature-matrix.md#authoring). |
| <img class="comparison-logo" src="./assets/logos/windmill.svg" alt="" width="24"> **Windmill** | Scripts, flows and apps with reusable/nested agents, conversational memory and coding-agent sandboxes. | Execution-backend selection, supervisor guidance, session handoffs and mission recovery, plus native worktree/finalization policies for repository tasks. Compare the full agent lifecycle. [Technical details](feature-matrix.md#technical). |

</div>


[Explore detailed capabilities and their sources](feature-matrix.md#sources).

> 🗄 **Flowise end of life: August 31, 2026; repository archived August 13.** Its documented features remain listed for existing users. The Cloud listing does not guarantee current service or support. [Official announcement](https://github.com/FlowiseAI/Flowise/discussions/6727).

## 🧩 What developers can build on

**A programmable workflow you can own and review.** Version the `.bot` source alongside your project, review changes and reuse the method across your team. Native agent and judge nodes describe who produces work, who evaluates it and which conditions advance the graph. Use the synchronized visual editor when useful. [Language and orchestration](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/dsl.md).

**Adaptive work inside an explicit process.** Define reusable groups, sub-bots, conditional routes and data-driven loops. On supported backends, an agent can create sub-agents dynamically inside its node. The workflow retains the outer checkpoints and control boundaries; dynamic orchestration has model and backend requirements. [Loops and composition](feature-inventory.md#o06), [dynamic orchestration](feature-inventory.md#b11).

**Knowledge work with or without a repository.** Use memory, web search, MCP tools, scripts and attachments for research, analysis and document production. Repository targeting can be optional or disabled. Validate your ingestion, retrieval and delivery requirements for the specific application. [Memory](feature-inventory.md#memory), [web tools](feature-inventory.md#x08), [file outputs](feature-inventory.md#u07).

**A repository delivery lifecycle.** Start from development, review, documentation or maintenance bots. Worktrees isolate Git changes, and a finalization policy defines how to retain or integrate the result. Configure your tests and review criteria as part of the workflow. [Bot catalog](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/examples.md), [Git finalization](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/adr/064-worktree-finalization-requires-delegated-authority.md).

**Execution controls built into the engine.** Follow the mission, answer human requests and resume eligible runs while preserving completed steps. Interrupted or failed steps may execute again. Configure budgets and iteration limits to bound the work. [Recovery](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/resume.md), [budgets](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/dsl.md#budget-fields).

**Collaboration while agents work.** An agent can ask a question asynchronously and wait at a defined point. A supervisor can observe another agent and send guidance during execution. Backend and composition limits are detailed in the inventory. [Asynchronous questions](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/async-interaction.md), [supervisors](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/supervisors.md).

**Custom tools and reusable knowledge.** Combine document memory, skills and plugins with your scripts and MCP tools. Selected knowledge can span a private run, project or organization, depending on storage and configuration. Automatic memory is optional and backend-dependent. [Memory](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/memory-and-knowledge.md), [skills](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/skills-library.md), [plugins](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/plugins.md).

**Integration with existing delivery processes.** Forge webhooks, scheduling, the native board and dispatcher connect events to missions. Use the CLI or API/SDK from your application or CI pipeline, and map terminal states and artifacts to your checks. Restricted configuration sharing lets non-operators edit selected fields. [Invocations](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/bot-invocations.md), [kanban](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/native-tracker.md), [shared editor](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/config-share.md).

**Tools to debug and recover work.** Inspect events, outputs and diffs, then use supported file snapshots, rewind and verifiable postconditions to investigate or repair a run. Restoring workflow or file state does not reverse every external side effect. [Workspace versioning](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/workspace-versioning.md), [verified actions](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/adr/044-adaptive-recovery-for-deterministic-action-nodes.md).

**Native multi-tenant operation.** The self-hosted platform separates resources by organization and provides teams, roles, credentials, quotas and audit. These controls are included in the platform; compare the [team boundaries and edition requirements](feature-matrix.md#team-access) of each alternative. Developers can start with the same engine through the CLI, Studio or desktop app before adopting shared infrastructure. Validate deployment and isolation settings for your environment. [Iterion platform](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/cloud-overview.md).

## 🤝 Compare the full agent lifecycle

Agent-oriented frameworks also combine deterministic and adaptive execution. Assess the concrete controls: execution backend, session handoff, live supervision, asynchronous human input, reusable memory, recovery and tenant-scoped operation. The [agent capability comparison](feature-matrix.md#agent-depth) makes those dimensions explicit.

For persistence, inspect **what is saved, where execution resumes and which side effects can repeat**. LangGraph and CrewAI also document state persistence; evaluate recovery behavior on your own failure cases. [LangGraph persistence](https://docs.langchain.com/oss/python/langgraph/persistence), [CrewAI persistence](https://docs.crewai.com/en/concepts/flows#flow-persistence).

## 🧭 Check fit with your architecture

- **Application integrations:** list required actions and events. Iterion provides [local deterministic connector actions and OpenAPI/Swagger package generation](feature-inventory.md#x09), alongside tools, MCP and APIs. Check available packages and the work to supply authentication, overlays and missing actions; broader catalog coverage and cloud transport remain outside this delivered scope.
- **Knowledge applications:** validate source ingestion, retrieval and the conversation interface. Include the actions and deliverables your assistant needs to produce.
- **Custom agent applications:** prototype your hardest routing, state and tool requirements in `.bot`, including dynamic work inside a node. Compare the resulting code, deployment and debugging workflow with your existing runtime.
- **Operations and support:** define your deployment, isolation, availability and support requirements. **Managed service on request — subject to assessment**; scope and commitments are agreed during that assessment. Iterion remains experimental and has no standard SLA. [Iterion status](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/README.md).

## 💰 Measure cost per accepted result

Iterion's MIT license lets you self-host and adapt the engine without an engine license fee. Budget for models, infrastructure, integration, maintenance and human review. **Managed service on request — subject to assessment**, with scope and pricing defined for your needs. [Iterion license](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/LICENSE).

Billing units differ: n8n paid plans use workflow executions, Make uses credits and Zapier uses tasks, with action-specific rules. Normalize these units against the same mission and acceptance criteria. [Recorded billing conditions](methodology.md#plan-conditions).

Measure **total cost divided by accepted results** across a representative batch. Include unsuccessful runs, retries, supervision, child bots and review time. Record quality, turnaround time and operator interventions alongside cost; these determine whether automation improves your delivery process.

For an embedded or customer-facing service, check redistribution and hosting rights. Iterion is MIT; n8n uses the Sustainable Use License; Activepieces separates its MIT core from Enterprise directories; Dify adds Apache conditions, including multi-tenancy and branding. [n8n license](https://raw.githubusercontent.com/n8n-io/n8n/master/LICENSE.md), [Activepieces license](https://raw.githubusercontent.com/activepieces/activepieces/main/LICENSE), [Dify license](https://raw.githubusercontent.com/langgenius/dify/main/LICENSE).

## 🚀 Try Iterion on your project {#try-iterion}

Pick a representative mission. For a knowledge application: gather sources, produce a structured analysis, ask for missing information and deliver a reviewed report. For software delivery: analyze a request, change the repository, run tests, review and correct the result. Include your own tools, state and acceptance criteria.

**Evaluate the outcome and the developer experience.** Inspect the deliverable, validation evidence, cost and human interventions. Exercise an interruption and recovery. Check how easily you can change the workflow, debug failures and connect it to your existing CI or application.

Existing application automations can call Iterion for a specialized mission. Define the request, run ID, terminal state and result links through the CLI, API/SDK or webhooks. This integration needs implementation and testing. [Iterion APIs and webhooks](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/webhooks.md).

<div class="comparison-cta">

**Turn your agent workflow into a working pilot.**

[Get started with Iterion](../quickstart.md) · [Explore the bot catalog](../examples.md) · [Open the visual editor guide](../visual-editor.md)

</div>

**Build custom agent workflows on an AI-centric engine, with native controls from mission launch to result validation.**

---

Scope: official alternative documentation reviewed on September 11, 2026, and Iterion at commit `bbc1ddc7844b81582b6dfc4d0b7f381680bb284b`, with the local connector foundation separately reviewed at `603d2a1e3314252dd2995e3bfaa4a7394e13093b`. Evidence combines documentation and targeted code inspection; fit should be validated in your pilot. [Inventory evidence and scope](feature-inventory.md#evidence).

</div>
