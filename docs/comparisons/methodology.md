---
title: "Methodology and sources"
description: "Scope, evidence and proposed integrations behind the product comparison."
aside: false
pageClass: "comparison-page"
---

<div lang="en">

# 🔎 Comparison methodology and sources

September 11, 2026 — companion to the [feature matrix](feature-matrix.md).

The matrix compares documented capabilities, access conditions and integration paths supported by official extension points. Sources were reviewed on September 11, 2026. This is a documentation review, without competitive execution tests.

A 🛠 cell describes an implementation using documented extensions. It does not rule out another native solution or imply that the proposed integration has been built and validated. Deployment and edition conditions remain explicit.

## What was verified

- **Documented capabilities:** official product documentation, licenses and dated edition records support the comparison cells. References are attached to product rows and numbered conditions.
- **Implementation evidence:** Iterion's inventory links to pinned documentation and targeted code inspections. The [evidence table](feature-inventory.md#evidence) identifies the mechanisms inspected; this is not an independent execution test of every criterion.
- **Proposed integrations:** 🛠 identifies architectures inferred from documented extension points, with implementation and testing still required.
- **Product availability:** Iterion's on-request managed-service assessment is the project owner's stated position; it does not establish an existing standard managed offering.

The review checked source relevance and scope, including associated components and edition boundaries. No cross-product runtime benchmark or tenant-isolation certification was performed.

## Examples of confirmed capabilities

<div class="comparison-table" role="region" aria-label="Methodology and sources — table 1" tabindex="0">

| Product and criterion | Status | Evidence and scope |
|---|---|---|
| Zapier — workflow file | 🟡 | JSON import/export on Team and Enterprise; unavailable on Free/Professional. [Official guide](https://help.zapier.com/hc/en-us/articles/8496308481933-Import-and-export-Zap-workflows-in-your-Team-or-Enterprise-account). |
| Flowise — workflow file | ✅ | Migration guide documents JSON export and import; credentials are handled separately. [Cloud migration](https://docs.flowiseai.com/migration-guide/cloud-migration). |
| Make — MCP | ✅ | AI Agents accepts MCP servers; Make also supplies its own server. [Client](https://help.make.com/make-ai-agents-new-mcp-tools-are-now-available), [server](https://help.make.com/introduction-to-mcp). |
| Zapier — persisted human wait | 🟡 | Request Approval waits for a decision with expiration and history, from Professional. This does not establish universal crash recovery. [Human in the Loop](https://help.zapier.com/hc/en-us/articles/38731463206029-Request-approval-to-keep-your-workflow-running-with-Human-in-the-Loop). |
| Make — code sandbox | 🟡 | Make Code isolates JS/Python on paid plans with resource limits; custom libraries require Enterprise. [Make Code](https://help.make.com/the-make-code-app-is-available). |
| Zapier — code sandbox | 🟡 | Isolated JavaScript runtime with plan-dependent time/memory limits. [Code by Zapier](https://help.zapier.com/hc/en-us/articles/8496310939021-Use-JavaScript-code-in-Zap-workflows). |
| Flowise — code sandbox | 🟡 | Code Interpreter calls an associated E2B sandbox; this does not establish an integrated project shell. [E2B tool](https://docs.flowiseai.com/integrations/langchain/tools/python-interpreter). |
| Dify — agent shell and files | 🟡 | The new Agent has a sandbox in beta, separate from the Code node. Workflow nodes have separate sandbox state. [Agent overview](https://docs.dify.ai/en/cloud/use-dify/build/new-agent/overview), [node scope](https://docs.dify.ai/en/cloud/use-dify/nodes/agent). |
| LangGraph — agent shell and files | 🟡 | Associated Deep Agents provides sandbox backends and execution/file tools. This requires a configured backend and is separate from core graph execution. [Sandbox backends](https://docs.langchain.com/oss/python/deepagents/sandboxes). |
| LangGraph — visual authoring | 🟡 | Associated Fleet builds agents without code; arbitrary LangGraph graph structure remains code-defined. [Fleet documentation](https://docs.langchain.com/langsmith/fleet). |
| CrewAI — agent shell and files | 🟡 | Associated E2B tools provide execution and file access with an external sandbox backend. [E2B tools](https://docs.crewai.com/v1.15.21/en/tools/ai-ml/e2bsandboxtools). |
| CrewAI — persisted human wait | ✅ | A pending async feedback request persists flow state automatically; the application supplies notification/callback transport and invokes resume. [Human feedback](https://docs.crewai.com/v1.15.21/en/learn/human-feedback-in-flows#async-human-feedback-non-blocking). |
| Iterion — local connector actions | 🟡 | Typed deterministic actions and package generation are delivered for local in-process runs, with a separate commit pin and explicit scope. [Connector evidence](feature-inventory.md#x09). |
| Iterion — output validation | Qualified | Static diagnostics and typed `compute` conformance are supplied. General node-output validation exists as an engine option but is not enabled by product entry points at the reviewed commit. [Pinned code evidence](feature-inventory.md#a08). |
| Dify — own workers | 🟡 | Version 1.13 describes Celery for streaming workflows and resumes; non-streaming remains in the API within this scope. [Maintainer announcement](https://github.com/langgenius/dify/discussions/32245). |
| CrewAI — own workers | 🟡 | Private-platform chart configures worker replicas and placement; Enterprise scope, separate from the framework alone. [Worker configuration](https://enterprise-docs.crewai.com/reference/chart-values/worker). |
| Dify — SSO | 🟡 | SSO, RBAC and audit are listed for Enterprise. [Recorded access conditions](#conditions-dify). |
| Flowise — SSO | 🟡 | OIDC is documented for Enterprise; the product's end-of-life condition also applies. [SSO](https://docs.flowiseai.com/configuration/sso). |

</div>


## Proposed integration paths

The sources establish the ability to call code, an API or a tool. The work required to deliver the target behavior is marked **to develop**.

<div class="comparison-table" role="region" aria-label="Methodology and sources — table 2" tabindex="0">

| Criterion | Products | Proposed implementation and limitation |
|---|---|---|
| Human intervention / persisted wait | Make | Store business state in a Data Store, request a decision and trigger continuation through a webhook. Two scenarios can implement one mission; the same run is not natively suspended. Design response authentication, expiration and deduplication. [Data Stores](https://help.make.com/data-stores), [Webhooks](https://help.make.com/webhooks). |
| Cumulative token / estimated AI cost budget | n8n, Make, Zapier, Activepieces, Dify, Flowise, CrewAI, Windmill | Route relevant AI calls through a shared controller, propagate a run ID, aggregate usage and reject later calls. Handle pricing, returned usage, concurrency and recovery. A built-in AI step may need replacement. A check after an agent step cannot control its internal loop. |
| Worktree / result merge policy | n8n, Make, Zapier, Activepieces, Dify, Flowise, Windmill | Build a Git executor for worktree/branch creation, commands/tests, result collection and policy-based integration. Scripts/APIs supply extension points; workflow export into Git does not implement this lifecycle. |
| Agent sandbox with shell and files | n8n, Make, Zapier, Activepieces, Flowise | An external service creates the sandbox, exposes mission commands/files and manages lifecycle and recovery. Isolation remains that service's responsibility; HTTP, SSH or a Code node alone does not provide it. |

</div>


### Extension points by product

<div class="comparison-table" role="region" aria-label="Methodology and sources — table 3" tabindex="0">

| Product | Documented building blocks | Integration responsibility |
|---|---|---|
| n8n | [Execute Command](https://docs.n8n.io/integrations/builtin/core-nodes/n8n-nodes-base.executecommand) and [HTTP Request](https://docs.n8n.io/integrations/builtin/core-nodes/n8n-nodes-base.httprequest), also usable as an agent tool | Self-hosted Git commands or a remote executor. Execute Command is disabled by default since n8n 2.0 and absent from n8n Cloud. For budgets, the HTTP AI service owns accounting and enforcement. |
| Make | [SSH](https://apps.make.com/ssh), [HTTP](https://apps.make.com/http), scenarios and [Data Stores](https://help.make.com/data-stores) | Git/sandbox executor through SSH or HTTP; budget controller in the AI service; scenario correlation for human decisions. |
| Zapier | [Outbound Webhooks](https://help.zapier.com/hc/en-us/articles/8496326446989-Send-webhooks-in-Zap-workflows) | Integrate a controlled Git/sandbox or AI API into Zaps. Mission duration may require asynchronous launch and later result delivery. |
| Activepieces | [Piece HTTP client](https://github.com/activepieces/activepieces/blob/main/.agents/skills/piece-builder/common-patterns.md), [AI providers and gateways](https://www.activepieces.com/docs/admin-guide/guides/setup-ai-providers) | Execution API and file management; AI gateway or dedicated service with per-run budget correlation. The gateway guide alone does not establish that budget scope. |
| Dify | [HTTP Request](https://docs.dify.ai/en/cloud/use-dify/nodes/http-request), [model interface and LLMUsage](https://docs.dify.ai/en/develop-plugin/features-and-specs/plugin-types/model-schema) | Define Git tools and result integration using the new Agent sandbox or a tool/API; an AI plugin/service can aggregate usage and cost. Counting after workflow completion cannot block in-progress calls. |
| Flowise | [Custom Tool](https://docs.flowiseai.com/integrations/langchain/tools/custom-tool) and [AgentFlow V2](https://docs.flowiseai.com/using-flowise/agentflowv2) | Tool calling an external executor, with AI controls in that service. Hosting, maintenance and extensions must account for Flowise's end of life. |
| CrewAI | [LLM hooks](https://docs.crewai.com/v1.15.21/en/learn/llm-hooks) and Python tools | Hooks/adapter with shared counters and explicit over-budget handling. A hook alone does not guarantee coverage of every call path. Git tools also require implementation. |
| Windmill | [Scripts and flows](https://www.windmill.dev/docs/intro), [AI Sandbox](https://www.windmill.dev/docs/core_concepts/ai_sandbox) | Write Git scripts and AI controls. Its sandbox supplies coding tools and files; define your own Git integration policy. |
| LangGraph | [Graph API](https://docs.langchain.com/oss/python/langgraph/graph-api), shared state and associated [Deep Agents sandboxes](https://docs.langchain.com/oss/python/deepagents/sandboxes) | Implement Git finalization, counters and call enforcement using framework primitives; configure the sandbox backend if using the associated harness. |

</div>


Iteration limits, per-call output caps and account quotas serve different scopes from cumulative run budgets. **Zapier pauses automatically above 75 tasks per run**; the detailed tables retain this separate safeguard. [AI by Zapier migration](https://help.zapier.com/hc/en-us/articles/47402591569805-Migrating-from-Agents-to-AI-by-Zapier).

## Hosting conditions

- **Iterion: managed service on request — subject to assessment.** Self-hosting is available. Managed scope, terms and support commitments are defined during assessment; no standard SLA is specified.
- **Flowise: Cloud still listed after announced end of life.** The [product-site record](#conditions-flowise) retains its Cloud listing; the [maintainer announcement](https://github.com/FlowiseAI/Flowise/discussions/6727) dates repository archiving to August 13 and the end of the core team's official Discord/GitHub presence to August 31, 2026. The conditional cell reflects this availability risk.

## Recorded access conditions {#plan-conditions}

The edition, hosting and billing conditions below come from official pages reviewed on **September 11, 2026**. Each commercial source is identified by its exact title and URL in plain code, followed by the relevant facts observed; technical documentation is linked directly. These are dated observations, not archived copies or contractual commitments. Unversioned pages may change.

### n8n {#conditions-n8n}

**Source record:** “n8n Plans and Pricing - n8n.io” · `https://n8n.io/pricing/` · reviewed September 11, 2026.

Paid plans use workflow executions; Git, environments, SSO and governance vary by edition. The Community edition supports user accounts but excludes projects and workflow/credential sharing: a workflow or credential is accessible to its creator and the instance owner. Iterion's self-hosted platform instead provides native organizations, teams and tenant-scoped resources. See [Community feature scope](https://github.com/n8n-io/n8n-docs/blob/main/docs/deploy/host-n8n/community-edition-features.md), [user management](https://github.com/n8n-io/n8n-docs/blob/main/docs/administer/manage-users-and-access/README.md) and [Iterion multi-tenancy](feature-inventory.md#e01).

### Make {#conditions-make}

**Source record:** “Pricing & Subscription Packages | Make” · `https://www.make.com/en/pricing` · reviewed September 11, 2026.

Hosted service with credit-based consumption. [Multiple teams and team roles](https://help.make.com/teams) require Teams or above. [Make Code](https://help.make.com/the-make-code-app-is-available) requires a paid plan; [SSO](https://help.make.com/single-sign-on) and [audit logs](https://help.make.com/audit-logs) require Enterprise. The [on-premise agent](https://help.make.com/on-premise-agent) connects the service to a local network; its documented architecture does not supply a self-hosted workflow engine.

### Zapier {#conditions-zapier}

**Source record:** “Plans & Pricing | Zapier” · `https://zapier.com/pricing` · reviewed September 11, 2026.

Hosted service with task-based consumption and action-specific rules. Tool-using AI by Zapier requires Professional or above and an Advanced/Premium model, including with BYOK; human approval requires Professional or above; JSON export/import requires Team/Enterprise; governance and SSO depend on plan. Shared folders require Team/Enterprise, while multiple workspaces require Enterprise. [Workspace scope](https://help.zapier.com/hc/en-us/articles/34713530114573-Zapier-account-organization-and-workspaces); [feature-specific technical sources](feature-matrix.md#sources).

### Activepieces {#conditions-activepieces}

**Source record:** “Pricing | Activepieces” · `https://www.activepieces.com/pricing` · reviewed September 11, 2026.

Projects, standard roles and SSO start at Team; audit logs, external secrets and Git releases are listed for Ultimate. Community is the automation core. The standalone Agents/Chat product must be distinguished from Run Agent steps inside flows; its commercial Free plan is also distinct from Community Edition. This distinction does not justify claiming that Community cannot run any AI agent. [Project scope](https://www.activepieces.com/docs/admin-guide/guides/structure-projects), [agent product changes](https://www.activepieces.com/docs/about/changelog).

### Dify {#conditions-dify}

**Source record:** “Dify Enterprise - Infrastructure for Building Agentic AI at Enterprise Scale” · `https://dify.ai/dify-enterprise` · reviewed September 11, 2026.

The Community/Enterprise comparison lists one workspace for self-hosted Community; multiple workspaces and enterprise management require Enterprise. SSO, RBAC and audit logging are listed in the Enterprise section. Basic roles within one workspace do not establish multi-workspace availability.

### CrewAI {#conditions-crewai}

**Source record:** “Pricing | CrewAI” · `https://crewai.com/pricing` · reviewed September 11, 2026.

Basic is free and includes Studio's visual editor on CrewAI infrastructure. SSO, RBAC and customer-infrastructure deployment are listed for Enterprise. The platform and Python framework are separate scopes.

### Flowise {#conditions-flowise}

**Source record:** “Flowise - Build AI Agents, Visually” · `https://flowiseai.com/` · reviewed September 11, 2026.

The site displays a sunsetting banner alongside Cloud plans. The [maintainer announcement](https://github.com/FlowiseAI/Flowise/discussions/6727) dates repository archival to August 13, 2026 and the end of the core team's official Discord/GitHub presence to August 31. This does not establish a shutdown date for every hosted service or availability of new contracts. Evaluate existing deployments with this lifecycle constraint.

### Windmill {#conditions-windmill}

Sources: official [roles documentation](https://www.windmill.dev/docs/core_concepts/roles_and_permissions) and [deployment stages](https://www.windmill.dev/docs/advanced/canonical_deployment_setups). Community provides workspace roles and resource access controls, with a three-workspace limit. Git sync is available in Community for workspaces with up to two users; broader deployment controls depend on edition.

### LangGraph and associated LangSmith {#conditions-langgraph}

LangGraph's runtime is separate from the platform that operates it. [LangSmith RBAC](https://docs.langchain.com/langsmith/user-management) and [self-hosted LangSmith](https://docs.langchain.com/langsmith/self-hosted) require Enterprise. These operator controls are not the same as authorization implemented inside an application using LangGraph.

## Criterion scope

Criteria describe target behavior; the legend separates supplied capabilities from integration work. Git features concern the repository an agent works on, separately from workflow versioning. The self-hosted team-access row checks supplied resource access controls for multiple teams/projects; it is not a certification of tenant, network or process isolation. The [ten-product tenancy table](feature-matrix.md#team-access) states the actual boundary and edition instead of treating every workspace as equivalent. Managed hosting distinguishes an available offering from an on-request assessment. The availability grid covers **21 features and 10 products**. A separate [12-dimension agent workflow table](feature-matrix.md#agent-depth) examines the mechanisms behind agent support; it is not an additional set of competitive checkmarks. The [140-criterion inventory](feature-inventory.md) includes interfaces and bot methods as well as engine primitives, so its total is not a competitive score.

**Compare equivalent scopes.** Separate core engines, associated harnesses, hosted platforms and paid editions. Native agent support is shared by several products; compare session control, supervision, memory, recovery and integration work instead of assigning whole use cases to a product family. Iterion supports repository-optional missions and highly customized agent workflows; Git delivery is one capability within that scope.

Model settings, platform cost policies and cumulative run budgets have different scopes. Sources and limitations remain in the [detailed tables](feature-matrix.md#orchestration).

</div>
