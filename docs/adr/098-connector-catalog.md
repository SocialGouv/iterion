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

Four constraints the existing code imposes, each of which a naive
generalization would break:

- **The OAuth-app identity is `(tenant, provider, base_url, owner_login,
  security_read_only)`** — per owning account *and per role*, not per host.
  The per-host index was deliberately dropped
  ([oauth_app_store.go:266](../../pkg/forge/oauth_app_store.go)) because a
  tenant legitimately holds one App per GitHub org and one org legitimately
  hosts both a runtime and a watch-only App. A `(tenant, connector, host)`
  key would re-impose exactly the constraint that was lifted.
- **Refresh needs a fenced claim.** `pkg/forge/refresh.go` scans and rewrites
  with no ownership claim, and a server replica each runs one
  ([server_lifecycle.go:193](../../pkg/server/server_lifecycle.go)); two
  replicas exchanging the same rotating refresh token can invalidate the
  token family or overwrite each other's whole record. The precedent to reuse
  is `pkg/secrets/oauth_refresh_worker.go`, which claims per credential and
  commits only while the claim holds.
- **Forge consumers are a class, not two stores.** Repository launch, review
  publication, SSO ownership proofs and team-deletion blockers all select
  connections; a non-forge connection on a shared host must not become
  eligible for a forge fallback. The generalization introduces explicit
  connection **capabilities**, and lands as expand-and-contract preserving
  ids, the `forge_conn:` sealing AAD and existing callback URLs.
- **A managed secret is a workflow secret**, materialised into the sandbox as
  a file ([run_secrets.go](../../pkg/secrets/run_secrets.go),
  [loop_secrets.go](../../pkg/runner/loop_secrets.go)) — which is how forge
  bots reach `git` and `glab`. Reusing that path "as is" would contradict
  decision 4, so a connector credential is an **execution-only capability**
  that cannot become a workflow secret, an env var, a mounted file or a
  prompt materialization. Legacy forge delivery stays explicitly separate.

### 4. A credential is used by the server or the runner, never inside the sandbox

For an **agent**: the call goes to the MCP facade over HTTP with a per-run
token, and the **server** resolves the connection and executes. For a
**node**: the **runner** executes, reading the credential at call time through
the refs it already carries.

An injected agent cannot exfiltrate a token it never holds; mid-run refresh
comes for free; and every backend is treated alike instead of only the ones
whose CLI can be handed a header.

Splitting execution across two process classes is what makes the following
contracts prerequisites rather than details, and each is a P1 deliverable:

- **The grant is the authorization, and it carries a tenant.** The board MCP
  handler's own invariant says its grant model is safe *only* because its
  store is single-tenant
  ([mcp_board_handler.go:204](../../pkg/server/mcp_board_handler.go)), and
  `ConnectionStore.Get` filters by `_id` alone
  ([connection_store.go:141](../../pkg/forge/connection_store.go)). Copying
  the board grant would let any valid run token resolve any tenant's
  connection. A connector grant carries tenant, run/attempt, node, the
  authorized alias→connection mapping, the package digest, the permitted
  operations and an expiry; aliases resolve **only** through it, against a
  tenant-scoped connection API.
- **Every outbound call goes through the guarded dialer.** A connector's whole
  point is calling operator-supplied hosts, so `pkg/secure/httpdial`'s
  public-unicast pinning and no-redirect policy is mandatory on every
  connector path — the call, pagination links, OAuth endpoints, spec fetches.
  A tenant-supplied URL must not be its own justification for reaching a
  private address; a self-hosted endpoint needs a deployment-controlled
  exception.
- **Rate limits need one authority.** Five server replicas and twenty runners
  each enforcing "one request per second" locally send twenty-five. Buckets
  are shared (`pkg/valkey`, with an in-process implementation for local runs)
  and keyed by what the vendor actually meters, not by connection id.
- **Transport bootstrap is specified per environment.** The board endpoint is
  injected only when a sandbox, an endpoint and a registration callback all
  exist ([executor_build_task.go:1396](../../pkg/backend/model/executor_build_task.go)),
  which is why a CLI run without a server silently has no board. A **required**
  connector must fail admission when its transport cannot be brought up —
  board-style silent disablement is the wrong default for a capability a
  workflow declared.

### 5. Distribution mirrors bots: baked → platform → team, plus git

Connector packages resolve through the same three tiers as bots, with git
sources for the external case and the marketplace as an **index** — never a
cloud installer. One resolver, guarded by a sweep test, so no launch surface
can read a different tier from the one that serves it.

What it mirrors is the bot transport's CURRENT shape, not its legacy one:
`materializeBotBundle` prioritises an **immutable snapshot** (inline bytes, a
digest, or a blob ref) and treats fetching the stored row plus a version check
as the legacy path ([botbundle.go:14](../../pkg/runner/botbundle.go)). A
reference-plus-drift-guard is not enough for a connector: a package replaced
after a run is queued cannot be *retrieved* for that run's resume, so the
guard turns a resumable run into a permanently failed one. Packages are
therefore **content-addressed and immutable**, retained while any queued or
resumable run references them, with connector-specific blob offload beside
the IR's (ADR-075). And an image `COPY` serves the server and runner but
gives a standalone release binary nothing — CLI and desktop need their own
asset delivery.

### 6. `tool` gains a third recipe rather than a new node type

Proposed syntax — the grammar does not accept it yet, which is why the fence
below is untagged (an `iter` fence is compiled by the docs guard against the
CURRENT grammar, and the docs must never show something the engine cannot run):

```
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

That refusal is necessary and **not sufficient**, because two other LLM paths
reach a `tool` node today:

- **The tool-policy classifier.** When `ITERION_LLM_CLASSIFIER_MODEL` is set,
  `runview` chains an `LLMClassifier` over the policy checker
  ([executor.go:428](../../pkg/runview/executor.go)), and *every* recipe calls
  `checkToolNodePolicy`. A deployment with that variable set would put a model
  call in front of every action — and fail the node outright when the
  classifier's own credential is missing. An action node resolves its policy
  through a deterministic path with static rules; an incompatible effective
  configuration is refused loudly rather than silently honoured.
- **Postconditions.** They run before recipe dispatch, can skip execution, and
  can turn a failed recipe into a success
  ([executor_verified_action.go](../../pkg/backend/model/executor_verified_action.go)).
  An inherited postcondition could mark an unknown remote mutation successful.
  P1 forbids postconditions on an action node; a later lot may define one that
  reads the typed result instead of a shell exit code.

The acceptance test is behavioural, not structural: **an action-only workflow
makes zero model requests with `ITERION_LLM_CLASSIFIER_MODEL` set** — not
merely under the default configuration, which is where a structural test would
stop.

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
authored work — plus a `spec_source:` URL, and the generation happens at
install time. That keeps operations fresh and lets an operator generate
against **their own** self-hosted instance's description.

**Moving the generation is not a licence clearance, and this ADR does not
claim it is.** CC BY-NC-SA restricts exercising the licensed rights for
commercial advantage, not only redistributing — so a commercial hosted
deployment fetching and transforming a non-commercial description *on a
tenant's behalf* is a different act from a self-hosted operator doing it for
themselves, and only the second is clearly covered. The install-time lane is
therefore **rights-dependent**: the package records the source artifact and
the permission relied on, retains the required notices, and distinguishes
operator-side generation from generation performed by iterion's own service.
Where the permission is unclear, the connector does not ship.

A licence is not a boolean, which is why `redistributable` is an explicit
assertion rather than a lookup on `spec_license`: GitLab's description is
CC BY-SA 4.0 — commercial use is fine, but attribution and share-alike attach
to the derived files, and even MIT requires the notice to travel. So a
redistributed package carries its provenance in every generated file, and P1
owes the notice header that makes that true on disk (recorded here rather
than discovered at the twentieth connector).

## What the P0 prototype measured

The generator ([pkg/connector/gen](../../pkg/connector/gen)) and the
declarative model ([pkg/connector/spec](../../pkg/connector/spec)) were built
and run against two real descriptions. Reproduce with:

```sh
ITERION_CONNECTOR_SPEC=/tmp/forgejo_swagger.json ITERION_CONNECTOR_ID=forgejo \
  go test ./pkg/connector/gen -run Measure -v
```

| | Forgejo | Slack | GitHub | GitLab |
|---|---|---|---|---|
| Format | Swagger 2.0 | Swagger 2.0 | OpenAPI 3.0.3 | OpenAPI 3.0.0 (YAML, 3.6 MB) |
| Licence | **MIT** ("for the purpose of interoperability") | **MIT** | **MIT** | **CC BY-SA 4.0** (`info.license`) |
| Operations / domains | 506 / 10 | 174 / 55 | 1225 / 47 | 1844 / 170 |
| Schemas | 246 | 48 | 967 | 889 |
| Package size | **615 KiB** | **186 KiB** | **4.74 MiB** | **3.58 MiB** |
| Auth derived | token (`Authorization`, prefix `token `), basic, +3 | oauth2 | **none** | bearer, oauth2 |
| Operations skipped | 0 | 0 | 0 | 2 |
| Ids needing a counter | 0 | 0 | 0 | 91 (4.9 %) |
| Complete without an overlay | no (auth) | yes | no (auth) | yes |

Licences verified the same day, per artifact — and they fall into **three**
groups, not two: permissive (GitHub, Forgejo, Slack: MIT; Jira Cloud:
`info.license` Apache 2.0, developer ToS still to read), **copyleft but
commercial** (GitLab: CC BY-SA 4.0 — redistributable *with* attribution and
share-alike on the derived files), and **non-commercial** (Mattermost:
CC BY-NC-SA 3.0 — the install-time lane, never the shipped catalog).

Seven findings changed the design:

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
5. **One malformed operation must not fail the generation.** GitLab's
   auto-generated description declares a path parameter `issue_id` on a path
   templated `{epic_issue_id}` (and one more like it). Failing there would
   lose its other 1842 operations to those two — the shape of the broken
   manifest that failed every launch of a team for 2h22 (ADR-080's
   amendment). So each operation is validated as it is derived, the bad ones
   are skipped, and the skips come back to the caller in a `Report`: a
   catalog with invisible holes is the other way to be wrong.
6. **A collision must be disambiguated by the PATH, not by a counter.** A
   large description collides constantly — GitLab derives the same
   `boards.create_lists` for the group-scoped and the project-scoped endpoint
   — and `create_3` becomes `create_4` the day the vendor adds an operation
   that sorts earlier, silently breaking every `.bot` that quoted it. The
   path is the operation's real identity, so the suffix comes from it.
   Measured after the change: 0 counters on Forgejo, GitHub and Slack; 91 of
   1844 on GitLab, which the overlay must pin. And GitLab's own operation ids
   are machine-generated from the path
   (`postApiV4GroupsIdDashEpicsEpicIidIssuesIssueId`), so they are detected
   and discarded in favour of the path derivation.

   **This is not yet identity stability, and the ADR does not claim it.** The
   walk is over sorted paths and the first arrival takes the unsuffixed name,
   so a vendor adding an endpoint that derives the same name and sorts
   *earlier* would take that name and push the existing operation onto the
   suffixed form — an unchanged `.bot` would then address a different
   operation. Zero counters measures collisions, not identity. P1 owes an
   **identity lock**: a committed mapping from an operation's canonical
   identity (method + path) to its public id, which regeneration compares
   against and refuses to reassign. The same rule applies to the derived
   auth-scheme ids a stored connection keeps.
7. **A path item's parameters must not override the operation's own.** Both
   formats give the operation precedence (OpenAPI 3.0.3, Path Item Object:
   they "can be overridden at the operation level"), and the first
   implementation had it backwards — the shared declaration's type and
   default won over the override written to correct them, silently. Fixed and
   pinned by a test that was checked to fail against the old order.

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

## What an overlay has to carry

P0 asked what an overlay must supply that a description cannot. The answer is
measured rather than guessed — it is what four real descriptions turned out to
be unable to state, and the shipped Forgejo overlay
([connectors/forgejo/overlay.yaml](../../connectors/forgejo/overlay.yaml), 5.6
KiB against 651 KiB of generated operations) is the reference:

- **Auth, sometimes entirely.** GitHub declares no security scheme at all.
  Forgejo declares *five*, and only two are credentials a connection can hold:
  `Sudo` is an admin impersonation modifier and `X-FORGEJO-OTP` a second
  factor, so offering them in a connection wizard would ask an operator to
  authenticate with something that is not an identity. Slack declares OAuth
  but passes the credential as an ordinary parameter, which must be marked
  secret or it reaches a model's context.
- **Pagination.** A description shows a `page` parameter exists; it never
  shows the response is a page OF something, nor where the walk should stop.
- **The curated MCP set.** 15 of Forgejo's 506 operations are worth offering
  an agent by name. Which fifteen is a product judgement.
- **Identity pins.** The overlay states an id; the derivation only proposes
  one. This is F13's identity lock in its per-operation form.
- **Outcome.** That Slack signals failure as `{"ok": false}` inside a 200 is
  prose.
- **Corrections** a human sees and a derivation cannot: a POST that searches,
  an endpoint that runs a model, an operation to drop outright.

Two properties make the split hold. An overlay entry that matches **nothing**
is an error, not a no-op: the usual cause is a regeneration that moved a
derived id, and ignoring it would bring the operation back *uncorrected* while
the overlay still looked applied. And `deterministic` is a **one-way door** —
an overlay may remove the claim (this endpoint runs a model) but never assert
it, because a human overruling a structural refusal with a promise is exactly
what this catalog must not accept on trust.

`iterion connectors gen|validate` is what makes a committed package
reproducible; a generated package nobody can regenerate is a blob, and the
whole split rests on the generated half being disposable. It is also the
install-time lane, which is why `--license` and `--redistributable` are
explicit inputs recorded in the provenance rather than inferred.

## The execution profile — what schema v1 actually promises

The review's F14/F15/F16 said the supported request/response semantics must be
settled *before* `spec.SchemaVersion` 1 is frozen, because a reduced model
publishes operations that look executable and are not. They are settled here,
and measured on the same four descriptions.

**Requests.** A body's encoding is declared (`json` | `form` | `multipart`),
never guessed — the same field set means different bytes in each. Non-scalar
parameters carry their serialization (`style` + `explode`, mapped from
Swagger's `collectionFormat`), because `labels=[a,b]` reaches a vendor as
`a,b`, as `a&labels=b` or as `a%20b` and only one is what it parses. Each
parameter has a public **key** distinct from its wire **name**, since an
operation legitimately carries `name` in its path and another in its body and
one flat `params:` map must hold both.

**Security.** Requirements are per operation: alternatives of conjunctions,
naming a scheme and the scopes *that operation* needs. `security: []` is
explicit anonymity, distinct from declaring nothing. And the three scope sets
stay apart — what a vendor **advertises**, what a connection **requests**, what
it was **granted** — because collapsing them is how an integration that reads
one channel asks for every scope the API offers. `Operation.SatisfiedBy`
answers a binding check at launch instead of a vendor 403 mid-run.

**Responses.** Every 2xx variant is kept, with 202 marked *pending*: a workflow
that reads "accepted" as "done" acts on work that has not happened. And an
`OutcomePolicy` expresses an API that signals failure **inside** a success
status — Slack answers 200 with `{"ok": false, "error": …}`, so a status-only
model would checkpoint a failure as a success. An unmapped vendor code inside a
2xx classifies as `bad_request`, never as success.

What this cost and recovered, measured:

- **Slack: 91 of its 174 operations gained a body.** They previously had *no
  arguments at all* — Swagger's `formData` was being read as a location iterion
  did not send, so `chat.postMessage` was published with nothing to post.
- **GitHub dropped 1225 → 1223 operations**, and that is the mechanism working:
  `POST /markdown/raw` (`text/plain`) and the release-asset upload
  (`application/octet-stream`) are raw-body operations. They were previously
  published as executable with an empty body; they are now named coverage gaps.
- Packages grew ~7 % (Forgejo 615 → 660 KiB, GitLab 3.58 → 3.81 MiB).

**Known gap, named rather than discovered later**: there is no `raw` body
encoding, so uploading a release asset and rendering raw markdown are out of
reach for now. Adding one needs a "the body IS this value" parameter shape,
which is a deliberate later decision, not an oversight.

## Adversarial review disposition (codex `gpt-6-astra`, xhigh — 22 findings)

Reviewed at commit `4b8a9bd3c`, read-only against the worktree. 4 critical,
15 high, 3 medium. Each finding was checked against the code before being
adopted or set aside — a finding is a hypothesis until reproduced, in both
directions.

**Adopted into the ADR above** — F1 (a grant carries a tenant; the board grant
model does not, and `ConnectionStore.Get` filters by `_id` alone) · F2 (every
connector path uses the guarded dialer; the ADR was silent about SSRF for a
feature whose purpose is calling operator-supplied hosts) · F5 (a connector
credential is an execution-only capability, because the managed-secret path
materialises a file *into* the sandbox) · F6 (fenced refresh claim; the forge
worker has none where `pkg/secrets/oauth_refresh_worker.go` does) · F8
(transport bootstrap specified per environment; a required connector fails
admission rather than degrading silently) · F10 (the OAuth-app key is per
owning account *and role* — the ADR's `(tenant, connector, host)` would have
re-imposed a constraint the repo deliberately lifted; plus the
expand-and-contract inventory) · F11 (immutable content-addressed packages
with retention — the ADR described the *legacy* bot transport; and its size
premise was wrong: 4.74 MiB is **below** the 6 MiB limit, so the real defect
is retention, not size) · F12 (`ITERION_LLM_CLASSIFIER_MODEL` puts a model in
front of every tool node, and postconditions can turn a failed recipe into a
success — forbidding ADR-044 recovery was necessary and not sufficient) · F13
(zero counters measures collisions, not identity: P1 owes an identity lock) ·
F14 (**a real bug in the committed generator** — a path item's parameters
overrode the operation's own, backwards from both formats; fixed, with a test
verified to fail against the old order) · F18 (one shared rate-limit
authority, since two process classes call the same connection) · F22 (moving
generation is not licence clearance — the install-time lane is
rights-dependent, and the blanket claim of legality is withdrawn).

**Adjusted** — F4: the action-attempt state machine (invocation identity,
request digest, idempotency key, outcome) becomes a P1 *prerequisite* rather
than a deferred contract, but `unknown_outcome` stays in the vocabulary now;
the resume path it must survive is `pkg/runtime/resume.go`'s node
re-execution. F9: real, and the repository already answers it —
`docs/cloud-queue-schema-rollout.md` carries the ordering policy and a
per-bump checklist, so the ADR references it and P1 adds the v14 → v15 entry
instead of inventing a procedure. F17: adopted as a contract to write, minus
the premise — ADR-094 is cited as the *pattern* for durable materialization,
never as a ready-made generic inbox, and connector ingress owes its own
identity, replay checks and acknowledgement timing.

**Adopted, and they move schema v1** — F14, F15 (per-operation security
requirements; supported vs requested vs granted scopes) and F16 (Slack
signals failure as `{"ok": false}` inside a **200**, so a status-only error
model breaks on a pilot service) together say the execution profile must be
defined *before* `spec.SchemaVersion` 1 is frozen. That is now P1's first
task, not P0's closing one.

**Deferred, documented as follow-ups** — F3 (binding platform OAuth apps to
trusted issuer origins so a team package cannot redefine `token_url`: real,
and it only bites once the platform tier exists — P2, with the team tier
refused platform-app consumption until then) · F19 (goja's interrupt does not
preempt native calls and bounds no heap: transforms stay refused until the
limits are enforceable, which was already P3) · F20 (third-party MCP
credential adapter and structured-result carriage across backends) · F21
(qualification keyed by environment/backend/auth rather than one scalar).

**Not contested** — the review's own closing note lists three objections it
checked and found false (Valkey board tokens already exist, both large
packages are under 6 MiB, the plan does mention the studio and MCP OAuth
compatibility). Recorded here so they are not re-raised.

**The reviewer's simpler alternative** — one trusted Go executor on the run
host, with the MCP facade as its narrow gateway — is **partly adopted**: its
shared-contract core (one credential resolution, one request construction,
one quota path, one attempt record) is exactly F4/F18's requirement and is
now in the ADR. Its proposal to execute agent calls on the *runner* rather
than the server is not adopted. Its scope advice — qualify a small Forgejo
operation set, keep the rest as unqualified catalog data — matches the
maturity model already specified.

> **The reason first given here was wrong** (round-two F16). It said runner-side
> execution "would put the credential back inside the pod the agent runs in".
> That conflates a process CLASS with an isolation BOUNDARY: under the
> kubernetes driver the sandbox is a sibling pod, explicitly distinct from the
> runner ([pkg/sandbox/kubernetes/manifest.go](../../pkg/sandbox/kubernetes/manifest.go)),
> so the runner is not the agent's pod. The premise is also false in the other
> direction — under the noop driver, and where a runner is configured as its own
> sandbox, agent commands execute exactly where the plan's own deterministic
> nodes hold credentials.
>
> The decision stands on the argument that survives: the boundary is a
> deployment's actual isolation capability, not the name of the process. What
> is owed is a stated rule for credential-execution eligibility per profile,
> refusing the combinations that do not isolate — not a claim that "runner"
> means "unsafe".

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

## Adversarial review disposition, round two (codex `gpt-6-astra`, xhigh — 21 findings)

Run against the implementation, not the prose: the reviewer had read access to
the worktree and drove the executor, the compiler, the AST transport and the
real recovery dispatcher with isolated probes. Twenty-one findings — one
critical, fourteen high. Its most useful contribution was not any single
finding but a table separating what this ADR *claims* from what the code
*does*, reproduced under "What is still only prose" below.

### Fixed in this branch

**F1 (critical) — an ambiguous mutation was retried by the engine.** The
executor classified a lost-answer POST as `unknown_outcome` and said "reconcile
before retrying"; the node boundary formatted that typed error into a string
with `%s`, so the recovery dispatcher — which classifies with `errors.As` — saw
plain text, bucketed it as `EXECUTION_FAILED`, and retried two seconds later.
Every component was individually correct and the property was false anyway,
which is the shape of defect only a composition test finds. Fixed with a new
seam: `runtime.AmbiguousEffect`, an interface an error implements to declare its
remote effect undecided, checked FIRST in the classifier (a lost-answer error
matches the network-transient needles almost by definition, so any later
position would hand it to the exponential backoff) and answered by a recipe that
never retries. New reserved code `AMBIGUOUS_EFFECT`, classified
`DispositionDeterministic` for the opposite reason to its neighbours — not "a
second attempt would reach the same verdict" but "a second attempt might
duplicate what already landed".

**F2 — a declared idempotency key licensed a retry.** Both readers took
`op.IdempotencyKeyParam != ""` as proof, so an OPTIONAL key the caller omitted
earned the retry it was meant to prevent. One shared predicate over the actual
arguments now answers it, rejecting empty and blank values too.

**F6 — the runtime enforces what the compiler promises.** A hand-built action
node carrying a postcondition took the ADR-044 ladder and reported success from
its idempotent-skip rung with no HTTP request made. The invariants are checked
at the executor before anything can claim the node, and the resolver's answer is
checked at the point of use: a non-deterministic operation, or one from a
`spotted` package, is refused rather than trusted from whoever produced it.

**F8 — pagination reported truncation as completeness**, in four ways. The
extraction returned an empty slice when it could not FIND the collection, which
reads exactly like a short page; the shipped Forgejo `repository.search` had
that shape and returned zero repositories with `complete = true`. Extraction is
now fallible, `Validate` refuses pagination over a non-array response with no
`items_field` (the generator already recorded the shape in `ResultCase.Array` —
the data was there all along), a cursor walk ends on the CURSOR rather than on a
short page, page sizes are measured against what was actually requested rather
than the package default, and a hand-picked page is never "complete".

**F13 (part) — a redirect was a success.** The reader refused only statuses
≥400, while the guarded client deliberately does not follow redirects, so a 302
returned OK with an empty body. Also: 202 is now pending by DEFAULT, since
generators only see what a vendor documented and most do not document their
202s. *Response-schema validation remains unimplemented — see below.*

**F17 — two passes did not know the third recipe existed.** Ref validation
collected command and script refs but not action params, so a typo'd reference
rendered empty and was SENT instead of raising C029; group expansion
substituted every scalar field and left the params alone, so they reached the
vendor as literal `{{params.repo}}`. The second needed a DEEP copy — the
shallow one shared the slice with the group template, so the first
instantiation's values would have leaked into every later `use`.

**F18 — `retry:` was dead config**, compiled and stored and read by nobody. It
now performs N extra attempts gated by the same safety predicate, so `retry: 5`
re-drives a throttled read and performs a lost-answer POST exactly once. The
duration form is refused: it gave the field two readings, and the delay is not
the workflow's to guess.

**F4 — secret handling, two opposite bugs.** The `{{secrets.NAME}}` placeholder
was sent to the vendor instead of the value (every other recipe materialises;
this one did not), and the credential appeared verbatim in transport errors,
which travel to the run's events and error tracking — `*url.Error` prints the
full URL, and an `in: query` scheme puts the token in it. Redaction now happens
where the secret bytes are still identifiable, over raw and URL-escaped forms.

Every fix above was **falsified before being trusted**: the change was reverted
and the test watched to fail on the reviewer's exact reported output.

### Tracked, not yet fixed

These are real and reproduced; they are execution-profile completeness rather
than safety, and each one is a lot of its own.

- **F3** — the generator infers `EffectRead` from a derived verb prefix, so a
  POST named `getOrCreateLease` is classified as a safe repeat. Effects must
  come from HTTP semantics, with an overlay correction required to call a POST
  read-only.
- **F5** — `ITERION_LLM_CLASSIFIER_MODEL` still chains an LLM classifier over
  the shared tool-node policy check, so a deployment setting it puts a model in
  front of every action. The promised behavioural test (zero model requests
  with the variable SET) is still unwritten, and until it exists the
  no-LLM claim is a claim.
- **F7** — generation silently drops security schemes it cannot resolve, and
  execution accepts any matching term when scopes are unknown, defeating
  `SatisfiedBy`'s conjunction refusal. Needs an explicit "scopes unknown" state
  distinct from "no scopes".
- **F9/F10/F11** — the body model cannot express a root array (it invents a
  `body` member), vendor `+json` media types collapse to `json`, multipart file
  parts are written as text fields, and `deepObject` / path-array styles are not
  serialized as declared. Each publishes an operation as executable that sends
  the wrong bytes; the honest interim is to REFUSE these shapes at generation.
- **F12** — parameter coercion parses every value as JSON first, so
  `9007199254740993` loses precision, `hello {{input.who}}` becomes `hello
  "Alice"`, and an empty string is dropped. Whole-value references should
  resolve as typed values and strings interpolate as text.
- **F14** — generation erases what it cannot derive (unresolved body refs,
  unsupported parameters) and reports zero skips, and `Validate` does not check
  outcome expressions. Derivation errors should propagate rather than vanish.
- **F15** — `connectors gen` writes the MERGED package and `validate` applies
  the overlay again over already-renamed ids, so regenerating Forgejo and
  re-validating fails on thirteen missing operations. The generated half must
  be persisted unmerged.
- **F16** — the ADR rejects runner-side agent execution on a pod-isolation
  premise that is false for Kubernetes sibling sandboxes and for the supported
  noop / runner-as-sandbox profiles. **The rejection stands on other grounds
  but its stated reason is wrong and must be rewritten** in terms of isolation
  capability per deployment profile, not process class.
- **F19** — the version probe guards `connector.yaml` only; ops and schema
  documents are decoded without a version or identity check.
- **F20** — no per-request accounting contract: a paginated action makes twenty
  billable calls that contribute nothing to the run's budget and leave no record
  linking quota, vendor request id and credential.
- **F21** — the connection generalization has no credential-LIFECYCLE model.
  Wire authentication and credential acquisition/renewal are different things,
  and GitHub App installation tokens need the second.

### What is still only prose

The reviewer's most valuable output. Of the round-one controls this ADR records
as adopted, these exist **in the plan only** — they were adopted into lots not
yet built, which is honest, but nothing in this document should be read as
describing shipped behaviour:

| Control | State |
|---|---|
| Tenant-carrying connector grant | Prose only — no grant, no production `ResolveAction` |
| Guarded dialer on every connector path | Only for CLI spec fetching; execution accepts any non-nil client |
| Execution-only credential capability | Not implemented; the executor takes a plaintext `Credential` |
| Fenced refresh claim | Not implemented — `pkg/forge/refresh.go` still scans without a claim |
| Immutable retained packages / `ConnectorRefs` | Not implemented; queue versions remain 14/10 |
| Zero-LLM action policy | Not implemented (F5) |

Round-one dispositions the reviewer judged wrong, and which are accepted as
wrong: **F4's ordering** (building the node path through generic retry recovery
before the attempt state machine existed is what produced F1); **F12's claimed
closure** (compiler rejection is not a runtime invariant — now fixed as F6);
**the "execution profile settled" conclusion** (parameter precedence was fixed,
but security, body shapes, serialization and response interpretation are not
ready to freeze as v1); and **treating overlay id pins as identity protection**
(they match derived ids, not canonical method/path identities — the identity
lock is still owed).

The single most dangerous thing the reviewer named — "the executor says
reconcile before retrying; the engine retries two seconds later" — is fixed.
