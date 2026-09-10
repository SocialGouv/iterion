# ADR-098 — The connector catalog: MCP for agents, deterministic nodes for workflows

- Status: proposed (2026-09-10)
- Serves: epic [#1072](https://github.com/SocialGouv/iterion/issues/1072) — an
  autonomous, redistributable connector catalog; lot P0 is
  [#1073](https://github.com/SocialGouv/iterion/issues/1073)
- Relates to: ADR-046 (trigger spine), ADR-069 (mid-run secret refresh),
  ADR-079/080 (cloud-portable plugin sources), ADR-090 (DB-backed runtime
  settings), ADR-094 (durable effect outbox),
  [docs/plugins.md](../plugins.md), [docs/bundles.md](../bundles.md),
  the study in `etudes/catalogue-connecteurs` (v1→v4 + an adversarial review)

## Context

iterion reaches third-party systems through one-off code today: `pkg/forge`
knows three git forges, `pkg/backend/mcp` boots whatever MCP servers a
workflow or a plugin declares, and everything else is a `tool` node shelling
out to a CLI. There is no catalog, no connection model outside the forges, and
no way to say "this team may call Slack" without writing Go.

Two offers are required, and neither substitutes for the other:

- **MCP for agents and bots** — capabilities attachable to a bot, available
  across its agents, independent of the model and of the backend.
- **Deterministic nodes** — a fixed operation with typed parameters, a typed
  result and typed errors, with **no LLM** choosing the operation, building
  the arguments or reading the answer.

A catalog listing an MCP server does not deliver the second: an agent that
*can* call an API is not a workflow step that *will* call it the same way
twice.

### What the study established, and what the code said back

The study (v1→v4, plus an adversarial review by Claude Code Opus 5) landed on
the right diagnosis — "the missing piece is a connector contract and its
lifecycle" — but proposed to build three things the repository already has in
a more mature form, and to execute connector logic in a bundled Node runtime
(Activepieces pieces behind a worker). Reading the code changed three
conclusions and the licence survey changed a fourth:

- `pkg/forge` is already a cloud-grade connection model: per-tenant OAuth apps
  with sealed client secrets, `oauth_app` / `github_app` / `pat` kinds,
  self-hosted instance URLs, a refresh worker, `needs_reauth` / `revoked` /
  `degraded` statuses, a managed secret injected into runs, and mid-run
  rotation (ADR-069). What it lacks is generality, not maturity. Adding a
  fourth credential system beside it, the forge one, the LLM-forfait one
  (`pkg/secrets/oauth*`) and the local MCP OAuth cache would be the mistake.
- iterion already ships MCP servers **inside its own binary** on three
  transports: `__mcp-board` (stdio), `/api/v1/mcp/board` (HTTP + a per-run
  `X-Iterion-Run` token), and in-process claw tools — all three dispatching
  into one `boardops` package. That is the template for a connector facade,
  with no Node and no Python.
- Bot distribution already resolves **team → platform → baked** through one
  authority (`pkg/server/bot_resolver.go`), materialises by reference on the
  runner with a version-drift guard, and can be pushed without an image
  rollout. That is the template for distributing connector packages.
- **A vendor's API description is not automatically redistributable.** Of the
  five specs surveyed on 2026-09-10, four are permissive and one — Mattermost,
  which the study had picked as the pilot on an assumption of Apache-2.0 — is
  **CC BY-NC-SA 3.0**, non-commercial. Nothing about an endpoint list reveals
  this.

## Decision

### 1. The deterministic substrate is declarative, generated from vendor OpenAPI

A connector's operations are **data**: a method, a path, flat typed
parameters, a typed result, a closed set of error classes. They are
**generated** from the vendor's own description and refined by a hand-authored
**overlay**. No Go per service, no bundled runtime.

Completeness is the reason. A vendor spec covers the whole API and the vendor
maintains it; the marginal cost of a service becomes an overlay. Measured:
Forgejo yields **506 operations** and GitHub **1225** from one command each,
where the Activepieces piece for GitLab has **one** action and Mattermost
**one**.

Activepieces stays useful as an **offline mining source** (MIT, notices kept)
for what a spec cannot express — curated operation sets, auth wording, trigger
recipes — seeding overlays. It is never a runtime.

**The generator ingests Swagger 2.0 and OpenAPI 3.x natively**, because two of
the four permissive specs (Slack, Forgejo) are 2.0, and because the
install-time generation lane below runs on an operator's instance where an
out-of-band Node converter is not available.

### 2. The MCP offer is a generated facade over the same package

The same operations are exposed to agents through an iterion-owned MCP facade
on the board's three transports. A **curated** subset is listed as tools; the
long tail is reached through a `search_operations` / `call_operation` pair, so
a 1225-operation API does not bury an agent's context.

Third-party and vendor MCP servers remain first-class **catalog entries** —
they often expose agent-shaped tools a mechanical facade cannot invent. What
they never become is a deterministic node: the facade is what carries that
promise.

### 3. Connections generalize `pkg/forge`, they do not sit beside it

`pkg/forge`'s connection model becomes `pkg/connection`: the provider enum
opens into a connector id, auth schemes come from the package rather than a
hardcoded list, and the forges become connectors #1–3. One OAuth-app store
(with a **platform tier** for iterion-managed cloud apps and a **team tier**
for an operator's own — the hybrid path already decided), one refresh worker,
one status vocabulary, one managed-secret injection.

### 4. A credential is used by the server or the runner, never inside the sandbox

For an **agent**: the call goes to the MCP facade over HTTP with a per-run
token, and the **server** resolves the connection and executes. For a
**node**: the **runner** executes, reading the credential at call time through
the refs it already carries.

An injected agent cannot exfiltrate a token it never holds; mid-run refresh
comes for free; and every backend is treated alike instead of only the ones
whose CLI can be handed a header.

### 5. Distribution mirrors bots: baked → platform → team, plus git

Connector packages resolve through the same three tiers as bots, with git
sources for the external case and the marketplace as an **index** — never a
cloud installer. One resolver, guarded by a sweep test, so no launch surface
can read a different tier from the one that serves it.

### 6. `tool` gains a third recipe rather than a new node type

```iter fragment
tool comment:
  action: forgejo.issue.create_comment
  connection: forge_main
  params:
    owner: "{{vars.owner}}"
    repo: "{{vars.repo}}"
    index: "{{outputs.pick.issue}}"
    body: "{{outputs.draft.text}}"
  timeout: 30s
```

`tool` is already *the* deterministic node — it carries `publish`, `needs`,
`await`, `permission`, `postcondition`. A `call` node would duplicate all of
that in the parser, the AST, the IR, the unparser, the diagram and the studio.

**ADR-044's recovery ladder is refused on an action node** (a compile-time
diagnostic): an LLM repairing a deterministic call is exactly what the offer
promises does not happen.

### 7. Logic escapes are declared, never implicit

Parameter and response mapping uses `pkg/dsl/expr` (total, no recursion).
Composite operations that no declaration can express — Slack's three-step
upload, Jira's ADF conversion — get a bounded `goja` transform (already
vendored, pure Go) in a later lot. Anything an operation needs beyond that is
a missing feature, not a hook.

### 8. Generate ≠ redistribute

`iterion connectors gen` is a **tool**: anyone may run it locally on any
description. What iterion **redistributes** in its baked catalog is only
packages whose spec licence permits it, asserted explicitly per package
(`provenance.redistributable`, defaulting to **false**).

For a non-permissive spec, iterion ships the **overlay alone** — its own
authored work — plus a `spec_source:` URL, and the operator's instance
generates the operations at install time. That is licit, it keeps operations
fresh, and it lets an operator generate against **their own** self-hosted
instance's description.

## What the P0 prototype measured

The generator ([pkg/connector/gen](../../pkg/connector/gen)) and the
declarative model ([pkg/connector/spec](../../pkg/connector/spec)) were built
and run against two real descriptions. Reproduce with:

```sh
ITERION_CONNECTOR_SPEC=/tmp/forgejo_swagger.json ITERION_CONNECTOR_ID=forgejo \
  go test ./pkg/connector/gen -run Measure -v
```

| | Forgejo | GitHub |
|---|---|---|
| Format | Swagger 2.0 | OpenAPI 3.0.3 |
| Licence | **MIT**, "for the purpose of interoperability" | **MIT** |
| Operations / domains | 506 / 10 | 1225 / 47 |
| Schemas | 246 | 967 |
| Effects (read/create/update/delete) | 261 / 102 / 58 / 85 | 644 / 190 / 204 / 187 |
| Package size | **615 KiB** (ops 499, schemas 115) | **4.74 MiB** (ops 1.68 MiB, schemas 3.06 MiB) |
| Auth derived | token (header `Authorization`, prefix `token `), basic, +3 | **none — the spec declares no security scheme** |

Licences verified the same day, per artifact: GitHub MIT, Forgejo MIT, Slack
MIT (Swagger 2.0), Jira Cloud `info.license` Apache 2.0 (developer ToS still
to read), Mattermost **CC BY-NC-SA 3.0**. GitLab's published path 404s and is
still to be located.

Four findings changed the design:

1. **A first-rate permissive spec can omit auth entirely.** GitHub's declares
   no security scheme anywhere — not at the root, not per operation, not in
   `components`; authentication lives in prose. So validation is split:
   `ValidateGenerated` is what a generator can guarantee, `Validate` is the
   complete check a launch requires. Demanding auth of the generator would
   have made the most important connector ungeneratable.
2. **Schemas dominate a large package** (3.06 of GitHub's 4.74 MiB). They are
   shared, not inlined per operation, and pruned to what iterion needs to
   validate an argument, render a form and type a result.
3. **Embedding the catalog in the binary is out.** At GitHub's scale a handful
   of connectors would add tens of megabytes to every binary — CLI, desktop
   and runner alike. The catalog is `COPY`'d into the image behind
   `ITERION_CONNECTORS_PATH`, like the bot catalog. It also means a connector
   package **exceeds `botsource.MaxBundleBytes` (6 MiB) territory**: the
   platform/team tiers need their own limit, or a curation pass, decided in P1.
4. **Operation ids need the vendor's abbreviation stripped.** 214 of Forgejo's
   506 operations name their resource in short form (`orgCreateTeam` under tag
   `organization`), against 187 that spell it out. Without the strip every
   call site reads `organization.org_create_team`.

## Consequences

- A new service costs an overlay, not an engine PR — the Nth-variant test the
  repository's philosophy sets for a seam.
- The two offers share one operation model, so a package cannot claim coverage
  on the agent side while lacking it on the deterministic side.
- `pkg/forge` gains a generalization it will pay for once: the provider enum
  opens, and the forges become the first three catalog entries.
- A catalog of hundreds of entries is honest, because `spotted` is inert
  server-side rather than a label.
- **Not covered here**: the executor's wire behaviour (retries, rate limits,
  the unknown-outcome contract), the trigger ingress family, the per-process
  devbox profile, and the studio surfaces. Each is a later lot of #1072.

## Alternatives rejected

- **An Activepieces worker at execution time.** Node on every runner and
  sandbox, an SDK shim to reimplement (`auth`, dynamic properties, storage,
  files, callbacks, pause/resume), `workspace:*` packaging, and coverage that
  is thin exactly where iterion needs it (GitLab: 1 action; Mattermost: 1).
  Dynamic properties are executed code, so they cannot be deterministic.
- **Porting the n8n catalog.** `LicenseRef-n8n-sustainable-use` restricts
  redistribution; rewriting does not launder the rights.
- **Go code per service.** One engine PR per connector — the anti-pattern the
  repository's modularity principle names outright.
- **Carrying connectors in `plugin.yaml`.** That manifest is `UnmarshalStrict`
  before its version check, and installed plugins are host-global and
  super-admin-gated: the wrong lifecycle and the wrong tenancy.
- **Marketplace install in cloud.** Files on an ephemeral pod — the lesson
  ADR-079/080 already paid for.
- **Requiring OpenAPI 3.x and converting 2.0 out of band.** Adds a Node
  toolchain to a project that has just refused Node at execution time, and it
  is unavailable in the install-time generation lane.
