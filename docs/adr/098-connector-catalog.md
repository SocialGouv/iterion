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

### Fixed in a second pass (F3, F5, F7, F12, F14, F15, F19)

Seven of the tracked findings turned out to be DEFECTS rather than design
lots, and were closed the same way as the first batch — falsified before being
trusted.

**F5 was the one that mattered most**, because it made a headline claim false:
`ITERION_LLM_CLASSIFIER_MODEL` chained an LLM classifier over the shared
tool-node policy check that every recipe passes, so a deployment setting it put
a model call in front of every action node, on the path documented as having
none. Closed with `PolicyContext.Deterministic`, derived from the node inside
that single check and honoured by the classifier falling through to its
deterministic base — the operator's own allow/deny rules still apply in full.
**The behavioural test this ADR owed since round one is now written**, and
deliberately runs with the classifier ENABLED: testing the default
configuration would have passed throughout, since it is off by default.

**F3** — a name granted retry safety. `getOrCreateLease` snake-cases to
`get_or_create_lease`, matched the `get_` prefix, and a POST that allocates a
lease was classified read, hence blindly repeatable. Effects now come from HTTP
semantics alone. Measured on the shipped Forgejo package: the heuristic never
fired once (261 GET→read, 102 POST→create, 85 DELETE→delete, 58 PUT/PATCH→
update). It bought nothing and risked everything.

**F7** — an unknown grant defeated the scheme conjunction: the escape accepted
any requirement holding a term with a matching scheme, so `A AND B` admitted a
connection holding only A. Now one shared definition (`spec.SatisfiableBy`)
used by both the executor and the connection layer, since two copies would
drift into a silent authorisation difference.

**F12** — four silent corruptions of an argument's value: an embedded
reference JSON-quoted (`hello "Alice"`), a large integer losing digits to
float64, an empty string read as absent, and a wrong-typed value sent
unchanged. All four now reach the vendor as written, or are refused.

**F14** — what could not be derived was erased and reported as nothing: a
required parameter in an unbuildable location, a request body whose `$ref` is
undefined. Both now route through the skip mechanism with a reason, and a
failed generation returns its report instead of a nil one.

**F15** — regeneration was not idempotent: `gen` wrote the MERGED package, so
the next `validate` looked up pre-rename ids and failed on thirteen unmatched
entries. The overlay is now checked against a separate copy and the pure
package is written.

**F19** — every document is version-probed and carries a connector-identity
check, not just `connector.yaml`.

### The three that are BLOCKED, with the contract they must satisfy

F16, F20 and F21 are not oversights and not quick fixes: each is a decision the
cloud lot has to make, and none can be made honestly before `pkg/connection`
has its Mongo twin and a runner path. What CAN be written now is the contract,
so the lot is judged against something rather than re-argued.

**F16 — credential-execution eligibility, stated per profile.** The reason
first given here was wrong (see the correction above); the positive rule is
owed. It must be expressed in terms of what a deployment's sandbox actually
ISOLATES, never the name of a process:

- Where the workload runs in a sibling pod or container the credential holder
  does not share (the kubernetes driver, the docker driver), a node's
  credential may be resolved by the process driving that workload.
- Where the workload shares the process or the host — the `noop` driver, and a
  runner configured as its own sandbox — an agent's commands execute exactly
  where the credential lives, and the two are the same trust domain. That is
  acceptable for a LOCAL run, where the OS user boundary is the operator's own
  and `iterion secret` already sits behind it. It is NOT acceptable on a
  multi-tenant deployment, and the resolver must refuse it there rather than
  rely on nobody having configured it.
- The refusal needs an explicit, greppable escape hatch, per this repository's
  own doctrine on load-bearing limits.

**F20 — the per-request accounting contract.** A first half shipped: a walk now
reports how many HTTP requests it made (`Result.Requests`, surfaced on the
node's output and its finish event when it is more than one), so twenty
rate-limit slots spent behind one node are no longer invisible. What the cloud
lot owes is the RECORD: one entry per physical request carrying the tenant, the
connection, the operation, the vendor's request id where it gives one, and the
outcome — enough to answer "what did this integration cost, and which calls
completed before the run was cancelled". It belongs beside `pkg/credusage`,
which answers the same question for LLM spend, and for the same reason: a
per-run total belongs to nobody when one run spends two credentials.

**F21 — wire authentication is not credential acquisition.** `pkg/connection`
models the first: which scheme, which placement, which value. It does not model
the second — how a credential is OBTAINED and RENEWED — and the two are
different enough that GitHub Apps need an installation identity, an app-key
linkage and a permission set, none of which is a "scheme". The lot needs a
lifecycle adapter seam (authorization-code OAuth, client credentials, App
installation minting, a static PAT) and a migration mapping for each existing
forge kind, decided before any store changes.

### Tracked, not yet fixed

- **F13 (part)** — response-schema validation. A 2xx whose body does not match
  the declared schema is accepted as data. The status and redirect halves are
  fixed; this one needs a schema validator the package does not have.
- **The operation identity lock** — the overlay's `id:` pins match DERIVED ids,
  not canonical method+path identities, so they cannot actually prevent a
  vendor's next release from moving an id onto another operation. A committed
  method+path → id mapping is what would.

### What is still only prose

The reviewer's most valuable output: a table separating what this ADR *claims*
from what the code *does*. It is kept here, and kept CURRENT, because the
failure it names is the one this document is most prone to — a control adopted
into the plan reading, months later, as a control that ships.

Its state after `pkg/connection` and the two fix passes:

| Control | State |
|---|---|
| Tenant-carrying connector grant | **Shipped.** Every `connection.Store` read takes the tenant as a positional argument; another tenant's record is `ErrNotFound` |
| Execution-only credential capability | **Shipped, structurally.** `openCredential` is unexported and its one caller is the resolver, inside a call |
| Zero-LLM action policy | **Shipped**, with the behavioural test that runs with the classifier ENABLED (F5) |
| Guarded dialer on every connector path | **Local path shipped** (`connection.LocalHTTPClient` is `httpdial.SafeClient`), **with the deployment-controlled exception this ADR owes it**: `ITERION_CONNECTOR_ALLOW_PRIVATE=1` opens the guard for a self-hosted instance, the refusal names it, and `connections add` warns when a base URL will be refused. Local tier only — a cloud tier must not read it, since there the base URL is tenant-supplied. The executor still accepts any non-nil client, so a future wiring could hand it an unguarded one |
| Fenced refresh claim | Not implemented — `pkg/forge/refresh.go` still scans without a claim, and no connector refresh worker exists yet |
| Immutable retained packages / `ConnectorRefs` | Not implemented; queue versions remain 14/10 |
| A cloud (Mongo) `connection.Store` | **Not implemented.** The interface and its conformance suite exist and the memory/file twins pass it; until the Mongo one lands, connectors are LOCAL-ONLY — a cloud hole by this repository's own doctrine, not a limitation |

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

## Adversarial review disposition, round three (codex `gpt-6-astra`, xhigh — 20 findings)

Probe-driven again, against the worktree. Twenty findings, one critical,
thirteen high. Its lesson is narrower and sharper than round two's: **most of
what it found was a class I had left half-closed, or a regression one of my own
fixes had created.** Round two's dispositions were not wrong; they were
incomplete in a way only a second adversary noticed.

### The two that mattered

**F2 — the runtime never applied the authored half.** `spec.Load` reads ops/
and knows nothing about overlays, so the catalog served EXECUTION the generated
package while `iterion connectors validate` reported on the merged one. The
shipped Forgejo connector validated with two auth schemes and ran with five —
three of which are not credentials at all — answered to its pinned
`forgejo.issue.comment` in validation and to nothing in a run, and lost every
declared pagination. An operator's green validate described a package no run
ever saw. One loader now does generated → overlay → complete check, and both
surfaces call it.

The end-to-end test could not have caught it: its fixture has no overlay, so
the two loaders are indistinguishable there. That is worth remembering about
end-to-end tests generally — this one proved the path worked for the shape it
happened to use.

**F1 — the engine still retried mutations the connector refuses to repeat.**
Round two closed `unknown_outcome`, the answer that never arrived. The CLASS is
wider: a POST answered 500 is `upstream` like any other 500, and the write may
have committed before the server failed. A 201 whose body will not decode is
worse — the mutation certainly happened and only its answer is lost. Both were
retried two seconds later. Decided now where the facts are (status plus the
operation's effect), and only for a mutation: a 4xx is a refusal, nothing
happened, and parking those would be the opposite defect.

This is the repository's own grep-la-classe rule, paid again: the first fix
treated the site the report named.

### Also fixed

- **F9** — an alias was unique only at creation, so a rename left two
  connections answering one name and `ByAlias` returned whichever the map
  reached first: identical workflow input selecting a different credential
  between two runs.
- **F10** — two more pagination truncations. A cursor walk ended on an empty
  page (Slack documents exactly that shape: no items, and a next cursor), and
  `asPositiveInt` did not know `json.Number` — **the type my own coercion fix
  had just introduced**, so a caller's `limit: 2` stopped being recognised as a
  size at all.
- **F12** — the vendor's own error text could carry the token. Redaction
  covered the transport path and stopped there; a gateway answering 403
  routinely echoes the credential it rejected. Scrubbed in place, so the typed
  error's retry disposition survives.
- **F13** — the file refusal knew one spelling of a file. Swagger 2 says `type:
  file`, OpenAPI 3 says `type: string, format: binary`; every OpenAPI 3 upload
  went through the refusal written to catch it.
- **F15** — a tier that could not LOAD a package was treated as one that did
  not have it, so a refused package was silently replaced by a different one.
  Its falsifier then exposed that absence was never recognised at all:
  `os.IsNotExist` does not unwrap, and the loader wraps with `%w`.
- **F20** — authored text was read as JSON: a literal kept no whitespace, and a
  literal `null` made the argument vanish.
- **F6** — the resolver read only the operation's own security, so a
  connector-wide requirement was never checked before a credential was handed
  over.
- The mid-flight probe that caught a required whole-body parameter left
  optional — a defect introduced by the `WholeBody` marker two commits earlier.

### Tracked, with what each needs

- **F3** — the F16 contract this ADR states is not yet sufficient. Under the
  docker driver with `host_state: auto`, the sandbox mounts the iterion data
  directory, which holds the connection store AND (on a keyfile fallback) the
  master key. Being a sibling container did not put the credential outside the
  workload's reach. Eligibility must be judged from the RESOLVED mounts and
  privileges, not from the driver's name — a correction to the contract, before
  the cloud lot builds against it.
- **F4** — a connection created without `--base-url` resolves the package's
  default at call time, so replacing the package redirects an existing
  credential to another origin. The authorized origin should be pinned at
  creation.
- **F5** — the "tenant-carrying grant" this ADR records as shipped is tenant
  OWNERSHIP, not run AUTHORIZATION: a node may name any alias its tenant holds.
  A run-scoped grant (connection id, operation allowlist, expiry) is still
  owed, and the claim is corrected in the table below.
- **F7** — `ResolveAction` returns plaintext once per node and the walk reuses
  it, so a revocation mid-walk is not seen. An opaque handle re-resolved per
  dispatch belongs in F21's lifecycle.
- **F8** — whole-record updates have no expected revision, so a stale writer
  can resurrect a revoked connection. Revision-based CAS must be defined before
  Mongo, not after.
- **F14** — the deterministic policy bypass works, but the classifier is
  CONSTRUCTED eagerly and fails on a missing credential before any action runs.
  The regression test injects an already-built classifier and so cannot see it.
- **F16** — accounting drops failed and retried requests, and the record this
  ADR specifies carries no run/attempt/node identity.
- **F17/F18/F19** — the file store accepts a zero-byte file as an empty
  database and does not validate what it loads; a lost sealing key is silently
  replaced beside existing ciphertext; the memory store returns shared slices,
  so a caller can mutate stored capabilities without an Update.

### The honesty table, corrected again

| Control | State |
|---|---|
| Tenant-scoped reads | **Shipped** — positional tenant, another tenant's record is ErrNotFound |
| Run-scoped GRANT | **Not shipped** (F5). Tenant ownership is not run authorization, and this row previously claimed otherwise |
| Execution-only credential | **Partial** — unexported opening, but plaintext once per node and no re-resolution mid-walk (F7) |
| Zero-LLM action policy | **Shipped after construction**; eager classifier construction still fails an action-only run (F14) |
| Guarded dialer | **Local path shipped, with its deployment-controlled exception** (`ITERION_CONNECTOR_ALLOW_PRIVATE`, round four); the executor still accepts any non-nil client |
| Credential never in an error | **Shipped for every shape one travels in** — the token, what the run materialised into a parameter (round four), and both halves of a basic credential plus their base64 blob (round five) |
| Effective package at runtime | **Shipped** (F2) — generated + overlay, one loader for validation and execution |
| Fenced refresh claim / revision CAS | Not implemented (F8) |
| A cloud (Mongo) `connection.Store` | Not implemented — connectors remain LOCAL-ONLY |

**The single most dangerous thing**, in the reviewer's framing and accepted
here: nothing yet binds a run to the connections it may use. The tenant check
is real and the credential is sealed, but a node names an alias and gets it.
That is tolerable while connectors are local-only and the operator is the
tenant; it is the first thing the cloud lot must close.

## Adversarial review disposition, round four (the PR merge gate — 5 findings)

Five findings, one high. The shape differs from round three's: none of these
were regressions from an earlier fix, and none were in the executor's wire
behaviour, which three rounds have now worked over. Four of the five are
places where something **reads as configured and is not** — the class this
ADR keeps rediscovering one layer out each time.

### The one that held the gate

**A self-hosted instance was the case the guard refused.** Every local
connector call went out on `httpdial.SafeClient(true, …)`, and nothing in the
lot lifted it — no flag, no env var, no per-connection field. The single
connector this catalog ships is Forgejo, which is overwhelmingly self-hosted:
`connections add --base-url http://localhost:3000` was accepted without a
word, and then every action node failed with "resolved address 127.0.0.1 is
not a public unicast IP" — a message naming neither the connection nor a way
to permit it.

This ADR already owed the remedy in writing (*"a self-hosted endpoint needs a
deployment-controlled exception"*), and by this repository's own test the
limit was artificial rather than load-bearing: it existed because nobody had
wired the override. `ITERION_CONNECTOR_ALLOW_PRIVATE=1` is it, spelled like
the `ITERION_RUNNER_CLONE_ALLOW_PRIVATE` precedent it mirrors, and LOCAL-tier
only — a cloud tier builds its own client and must not read it, since there
the base URL is tenant-supplied.

Two things were added with it, because the refusal arriving a layer away from
the mistake is half the defect: the refusal now carries its own remedy, and
only when the guard is genuinely the cause (the hint re-asks with the policy
off, so a typo or a dead DNS is never told to open a security guard), and
`connections add` says the same thing at the moment the base URL is typed.

Verified end to end against the shipped Forgejo package and a local server:
refused with the remedy when closed, `status: 200` with the `token ` prefix
applied when open.

### The other four

- **The parser concatenated two bare words.** The unit-join exists for
  `timeout: 30s`, which the lexer splits in two — but it was unconditional,
  and every `params:` value goes through it. `body: hello world` reached the
  vendor as `helloworld`, and a third word was read as the next parameter
  name. Only a NUMBER takes a unit now; anything else left on the line is
  diagnosed by name, with the value echoed back **from the source** in the
  form it has to take. `pkg/dsl/parser` had no test for this file at all.
- **Redaction followed the package, not the run.** `renderActionParams`
  materialises a `{{secrets.X}}` into any parameter, while `exec.secretValues`
  collects only the credential plus params the package marked `Secret: true` —
  a flag generated packages inherit from descriptions that rarely set it. A
  transport failure copies the whole request URL into `Error.Message`, so a
  second credential passed as an argument left with it. Scrubbed through the
  run's own Guard, in place on the typed error so `errors.As` still reaches
  `*exec.Error` and the ambiguous-effect guarantee survives.
- **`timeout:` was inert on `command:`/`script:`.** The property now parses on
  any tool node and only the action path reads it, so `command: go test ./...`
  with `timeout: 30s` compiled clean and ran unbounded. Added to C266's orphan
  list. Wiring it into the shell recipes is the more generous answer and
  belongs in its own change.
- **Retries had no delay when the vendor named none.** Measured: 657µs between
  attempts. Not inventing a backoff is defensible about the LENGTH of a delay;
  the alternative chosen was zero. 500ms doubling to a 5s ceiling with the
  jitter shape `RetryPolicy.backoff` already uses, and the vendor's own
  `Retry-After` still wins.

### Found while verifying those, and fixed with them

- The property registry taught a `retry:` form the compiler refuses ("attempt
  count **or duration**; empty takes **the package default**") — a duration is
  a C265 error and there is no package default. It renders into three surfaces
  an author reads before writing a line.
- `FileStore.read` claimed to run "under the same lock" as a write. It takes
  only the in-process mutex; the safety rests on `WriteFileAtomic`'s rename.
  Stated as the contract it is, since the Mongo twin's conformance suite is
  the next reader of that sentence.
- The EBNF declared `retry:`, `timeout:` and every `params:` value as
  `STRING_LIT`, while every example in this ADR and in `docs/dsl.md` writes
  `timeout: 30s` unquoted.
- `connections add` printed the capability FLAG rather than the grant it
  stored, so it said `capabilities:` where `connections list` said `action`
  for the same record. `pkg/cli` had no test for these commands.

### Answered, and not defects

- `AMBIGUOUS_EFFECT` does reach `failed_resumable` with its checkpoint, and is
  genuinely excluded from `--auto-resume`, which admits only
  `DispositionTransient`. The path is **`failRunErrWithCheckpoint`**, which
  persists the classified `rtErr.Code` — not the sibling
  `failRunWithCheckpoint`, which hardcodes `ErrCodeExecutionFailed`, a code
  that IS on the auto-resume allow-list. The two names differ by three letters
  and by the entire guarantee, which is why the chain is now pinned by a test
  rather than by reading:
  `TestAnUndecidedMutationParksTheRunForAnOperator` runs it on the real engine
  with the recovery dispatch every production host wires, and asserts the
  persisted code, the status, the production `AutoResumable` predicate and a
  single request to the vendor. Falsified by making the engine drop the
  classified code: the run reverts to `EXECUTION_FAILED` *and* becomes
  auto-resumable.
- `ClassifierChecker` is the only checker on the tool-node path that can
  consult a model; `Policy` and `RulePolicy` are pure. A `permission: ask`
  gate cannot park an action node, because a tool node's `permission:` is
  parsed and not enforced (C112).
- `Result.Requests` does accumulate across a successful walk (`n + 1` per
  page). It is the per-call value on a failure path, where the node fails and
  there is no output to carry it.
- `Connection.ExpiresAt` is read by the **resolver**, which refuses a
  connection whose expiry has passed. No surface sets a non-zero value yet
  (the CLI seals a zero expiry) and no refresh worker exists — both of which
  the row above records — but the field is public on this struct and
  serialised, so any writer reaching it finds the refusal already in place,
  and iterion no longer spends a call to have the vendor answer 401 about an
  expiry it already holds. On a mutating operation that call also buys an
  effect to reason about. What made this a defect rather than an honest gap
  was its doc comment, which said *"Read by the refresh worker to decide what
  is due"* — naming machinery this lot does not ship. A public field described
  as load-bearing and inert in fact is worse than an absent one; the comment
  now states what is true.

### The studio, deliberately still unwired

`Connectors` is set by `iterion run` and `iterion resume` and by nothing else.
That is this lot's stated boundary — *"Not covered here: … and the studio
surfaces"* — and a node that reaches an unwired surface fails explicitly
rather than reporting a success it never performed. Named here because the
gap is invisible from the CLI, which is where the feature was exercised.

## Adversarial review disposition, round five

**The other half of a basic credential was outside the redaction net.**
`exec.secretValues` listed `cred.Value` and the params a package marked
`Secret: true`. `Credential.Username`/`Password` — documented in the same file
as *"serve AuthBasic"* — were never in it, while `applyCredential` sends
exactly those bytes as `Basic base64(user:pass)`. So the layer transmitted a
credential its own redaction could not recognise coming back, which is the
failure mode round three closed for token-style credentials (F12: a vendor 4xx
echoes what it rejected) left open for the one scheme the shipped Forgejo
package declares (`connector.yaml`, `kind: basic`). Within a single function,
`cred.Value` had the guarantee and `cred.Password` did not.

Latent, and fixed anyway. No shipped surface stores a basic credential today —
the CLI writer only seals tokens — but `SealBasic` is exported in this same
lot for exactly that purpose and `resolve.go` already maps both halves onto the
wire credential. The boundary belongs to the layer that owns the guarantee,
not to the memory of whoever writes the first caller.

All three shapes are redacted, because a vendor can echo any of them: the
base64 blob it received, or either half once it decoded one. The username is
in the set although RFC 7617 calls it a user-id and not a secret — a catalog
accepts whatever vendor a package describes, and `<api key>:` with an empty
password is a widespread convention, so this layer cannot tell which half a
given vendor made secret. Over-redacting costs a marker in an error message;
under-redacting costs a key in the run's events, the tool hooks and error
tracking. The test asserts the bytes really travelled before asserting they
came back redacted, so it cannot pass by sending nothing.

### Seven more, found by re-reading the lot rather than the review

The gate's finding above was the one handed over. Re-reading the diff on
the axes four rounds had not walked — the DSL's own readers, the CLI's
refusals, the reading of a vendor's answer — turned up seven more. The
shape is the round-four shape again: something that **reads as configured
and is not**, one layer further out each time.

**A trailing comment was not a line ending.** `scanComment` consumes the
newline and emits `TokenComment` in its place — which is why
`skipToNewline` and `skipNewlinesFrom` both stop on one — and `atLineEnd`
listed only Newline/Dedent/EOF. So `timeout: 30s   # keep it short` was an
E020 error on a valid line, and then `restOfLineText` ran past the comment
into the NEXT line and deleted that property from the node. Every property
of this recipe reads through that path.

**The unit join was licensed by the head being numeric**, not by adjacency.
`body: 2 failures` was still joined into `2failures`, silently, because
after the join the line IS ended and the refusal added in round four never
fires. Two tokens join only when the second begins exactly where the first
ended, which is what `30s` actually is.

**`params:` hand-rolled its own block reader.** It tested for an INDENT and
consumed the rest of the line otherwise, which reads as defensive and is
the opposite: a bare `params:` followed by a sibling property swallowed
that property's line, and `params: { owner: "acme" }` dropped every
argument — both with no diagnostic, on the one recipe whose promise is that
the request is what was declared. It goes through `blockBodyAfter` now, the
reader every other block uses.

**A path argument could be path STRUCTURE.** The `/` case was closed by
escaping each segment; `.` and `..` are unreserved, so `url.PathEscape`
returns them unchanged and Go sends `EscapedPath()` verbatim. Measured:
`repo: ".."` sent `/api/v1/repos/acme/../issues/1`, which the vendor — or
any proxy — resolves to `/api/v1/repos/issues/1`, and `repo: ""` sent `//`.
A path argument comes from `{{...}}`: an issue title, a branch name, a
model's output. The call addressed a resource the workflow never named and
that answer was checkpointed as the declared call's; on a mutation it is a
write to the wrong place.

**`connections add` wrote three connections no call could use** — a
self-hosted connector with no `--base-url` (the shipped Forgejo package is
`operator_supplied` with no default, and `add` printed "(the package
default)" for a default that does not exist), a base URL with no scheme,
and `--scheme basic`, which this writer cannot seal since it has no
`--username-env`. Each was stored, listed and reported as connected, then
failed at the first action node. It was also the one caller passing a nil
warning sink to `NewLocalSealer` — so the command that first mints the
master key for a connector-only operator was the one that did not say it
had written it to disk.

**A walk with no position parameter re-sends one request.**
`validatePagination` demands `cursor_field` "so the walk could never
advance past page one" and never demanded the parameter the walk advances
THROUGH; `CallPaged` writes the position only when it is named. A package
declaring `style: cursor` with no `cursor_param` sent `max_pages` identical
requests and appended page one's items that many times.

**A 303 on a mutation was read as "it never happened".** The 3xx branch was
the only error path not asking `markAmbiguous`. iterion does not follow a
redirect, so the call did not reach the redirect TARGET — but 303 is the
canonical answer to a POST whose effect already happened, and 302 is used
the same way. 307/308/301 stay unambiguous, deliberately: each says the
vendor did not process the call.

Four more corrections found while verifying those: an unmapped vendor error
code silently restored the status range over the class the package declared
for that status (`403 → rate_limited` downgraded to a non-retryable
`forbidden`); a Swagger `consumes` naming only unbuildable media types read
as JSON, so a `text/plain` body went out as `json.Marshal("# Hello")` —
quotes included — under a JSON content type, where the OpenAPI 3 arm of the
same generator refuses and reports a coverage gap; the C265 remedy in the diagnostic catalogue and the reference
still taught `retry: 1m` — the form C265 refuses — after the property
registry had been corrected for it in round four; Swagger 2 dropped
`required` on a whole-body parameter where the OpenAPI 3 arm of the same
function propagates it; and a required parameter delivered as a `$ref` was
dropped with no skip reported, because `required` was read off the `{"$ref":
…}` wrapper, which carries nothing else — so the two cases that check names
in its own comment were the two it could never see.

## Adversarial review disposition, round six (the PR merge gate — 10 findings)

Ten findings, two high, and both of the high ones were the same defect class
the whole lot is built to prevent: **a promise wired on one path and not on
its siblings**.

### The two that held the gate

**A 2xx whose outcome predicate could not be EVALUATED was not marked
ambiguous.** `readResponse` asks `markAmbiguous` on every other undecided path
— the 4xx/5xx, the redirect a mutation may already have performed, the 2xx
whose body does not decode. The predicate branch did not, and it is the same
case as the one ten lines above it: the vendor answered 2xx, so the mutation
CERTAINLY landed and only the reading of its answer failed. Without the mark,
`AmbiguousEffect()` stays false, the run is classified `EXECUTION_FAILED`
rather than `AMBIGUOUS_EFFECT`, and the auto-resume path may re-issue the exact
mutating call whose effect already happened. Reachable with nothing exotic:
`validateOutcome` only checks that `success_when` PARSES, so any predicate
yielding a non-boolean at eval time takes it.

**A vendor's parameter name had no written form, in either direction.**
`spec.Param.Key` is the VENDOR's wire name — the generator carries it through
unchanged and `exec` looks the argument up by it — and the reader accepted only
a Go identifier: `tokenAsIdent` returns `""` for a quoted string, and a dashed
key does not lex as one token. The shipped Forgejo package has 22 such keys;
`activity-id` and `user-id` are REQUIRED path parameters, so every
`forgejo.activitypub.*` operation was uncallable from any workflow. The writer
had the mirror defect on the same round trip (only the VALUE was quoted), which
bites through the AST JSON transport whatever the parser accepts, so both sides
moved together. `retry:`/`timeout:` were the third instance of the class,
found beside it.

### The promise, completed on its remaining paths

The ambiguity fix above is the node's half. Two paths reach the run's failure
code **without** passing through the classification at all, and both were found
by tracing the guarantee rather than the diff:

- **A branch.** `processConvergence` builds ONE error for the whole
  convergence, and its untyped fallback flattens the chain to a string;
  `commonBranchFailureCode` structurally cannot rescue an executor failure,
  since it reads a `*RuntimeError`. So an undecided mutation under a
  `fan_out_all` was failed `EXECUTION_FAILED` — on the auto-resume allow-list
  — and the resume re-ran every branch. One action node in a fan-out, or a
  `fan_out_each` over N items, is the whole recipe.
- **A teardown.** The `ctx.Err()` short-circuit in `executeNode` precedes
  recovery dispatch by design (a cancelled run must not retry), but every code
  it can reach is one something re-drives: a drain's `interrupted` is
  redelivered to a fresh pod, `--timeout`'s `TIMEOUT` is on the auto-resume
  allow-list. A drain landing mid-call produced the ambiguity and then threw it
  away.

Both now keep the classification, and the e2e test for the first was verified
failing against the previous code.

### The same shape, one layer out: a capability wired on ONE surface

`ExecutorSpec.Connectors` was populated at two of the nine construction sites.
The studio, the launch API, a board card and a subbot built their executor
without it, so the same `.bot`, on the same machine, with the same catalog and
the same sealer, ran from the CLI and failed at its first action node
everywhere else. Four surfaces gain it through one shared function; the cloud
runner stays out (it needs a cloud connection store that does not exist yet)
and so does the golden recorder, which must not reach a vendor at all.

Found with it: the CLI returned a typed nil `*connection.Resolver` for
"nothing wired", and a typed nil in an interface is not nil — so the
executor's own "no connector catalog wired" diagnostic was unreachable and the
run got a message pointing at the connector instead of at the install.

### Found by reading the slices the review had not

A second pass over the generator, the executor and the CLI, each against its
own oracle rather than against the diff:

- **A bodyless Swagger operation inherited the root's `consumes`.** The
  encoding was asked for unconditionally, and an unsupported one is
  deliberately not cleared — so a GET under a root `consumes:
  [application/xml]` was published as `unsupported:application/xml` and refused
  by validation. An API whose root advertises only a media type iterion cannot
  build lost its ENTIRE read surface: measured at 0 operations derived, 3
  skipped, with a diagnostic about a request body none of them has. This is the
  over-correction of round five's own fix, on the arm where `consumes` is
  inherited.
- **A YAML key that is not a string was dropped.** "No API description has one"
  is wrong about the only syntax that can produce one: YAML resolves an
  unquoted `200:` as an int, so the ordinary hand-written responses block lost
  every case — silently, with no skip — and with it `validatePagination`'s
  array check. The JSON twin of the same document was read correctly. Its
  sibling one function over: `enumStrings` switched on `float64` only, which is
  what JSON gives; yaml.v2 gives `int`, so an int enum survived one syntax and
  not the other.
- **A secret was JSON-decoded on its way to the vendor.** A whole-value
  reference renders as a JSON literal, and the secret was materialised INSIDE
  that literal — after the encoder, before the coercion that decodes it. So the
  credential's own bytes were read as syntax: measured, `back\slash` and
  `has"quote` reached the vendor WITH their surrounding quotes and a literal
  `\n` arrived as a newline. Four of five shapes corrupted; each is a 401 on a
  valid credential, which is the symptom materialising was added to remove.
- **`connectors gen` wrote a credential to disk.** The fetch URL went into the
  provenance verbatim, and `spec.Write` puts that at 0644 in a directory whose
  whole point is to be committed — on the lane that exists precisely for a
  description an operator may not redistribute, i.e. the one behind auth. Go
  redacts userinfo when it prints a URL in an error; only what iterion
  persisted kept it in the clear.
- **A paginated walk had no bound of its own.** `max_pages` is taken verbatim
  from the package and nothing validates that number, while each page reads up
  to the 32 MiB limit and every page's items are held until the call returns.
  At the DEFAULT twenty pages that is already ~640 MiB. Clamped and given a
  byte budget, both ending the walk the way running out of pages already does —
  `complete=false`, which is what that flag means.
- **An update wiped the sealed credential of the connection it edited**, since
  `SealedPayload` is `json:"-"` and any caller rebuilding the record from the
  transport shape carries none. BOTH store twins had the identical omission,
  which is exactly why the conformance suite could not see it.
- **A catalog root that cannot be read was treated as absent**, so the project
  tier was dropped whole and the home tier served a different package for the
  same connector id — the defect `layeredCatalog.Package` refuses by name one
  file over, inverted at the site that decides which tiers exist at all.

Also fixed, each named in its own commit: a second `params:` block replacing
the first in silence; a templated `connection:` compiling clean and dying
mid-run (refused, not rendered — an alias built from an output would let an
upstream node choose which credential a call carries); `RawPath` built from the
DECODED base path, and a substituted value re-read as syntax by the next
replacement, which together sent a path argument as path STRUCTURE; an
unbounded `Retry-After`; a request count that reported the last attempt only;
`effect`/`maturity` typos landing on the inert side of every predicate; a
media type outliving its operation; and `connections add` sealing a credential
with a trailing newline.

### Open, with what each needs

- **The generator resolves a `$ref` one hop.** A vendor definition that is
  itself an alias (`CreatePullReviewCommentOptions: {$ref:
  CreatePullReviewComment}`) resolves to a map whose only key is `$ref`, so
  the body is declared a whole-body blob and its five members are not
  addressable from a `.bot`. Visible in the committed package
  (`ops/repository.yaml`, `create_pull_review_comment`). Needs a
  cycle-guarded resolve loop AND a regeneration of the shipped package,
  which needs the vendor description this repo does not commit.
- **The shipped package still carries three generator defects that are FIXED
  in the generator**: Swagger 2 dropping `required` on a whole-body parameter
  (the OpenAPI 3 arm of the same function propagated it; `checkParams` then
  treated the mandatory body as absent and the POST went out with none), a
  required parameter behind a `$ref` dropped without a skip, and a `consumes`
  naming only media types iterion cannot build read as JSON (wrong bytes,
  wrong header) where the OpenAPI 3 arm makes it a coverage gap. The code is
  corrected and tested; `connectors/forgejo/**` shows the old output until it
  is regenerated, with the same caveat as above.
- **The sealed credential's own expiry — CLOSED.** The observation was right:
  `checkUsable` read the PLAINTEXT record field, which the AAD does not cover,
  and runs before `openCredential`, so it structurally could not consult the
  authenticated copy. Both are read now, and the rule is fail-closed on
  either: the record check stays (cheap, refuses before anything is unsealed,
  and it is what an operator sees), and the blob's copy gets the last word
  once it can be read. `TestASealedExpiryOutranksTheRecord` drives the case the
  plaintext check cannot reach — a record claiming no expiry over a blob that
  has one — and falsifying the new guard reddens that test alone.
- **A response's integers are `float64`.** The REQUEST direction was fixed
  in this lot (`asPositiveInt`'s `json.Number` arm, "so a large id survives
  to the wire exactly"); `decodeJSON` still unmarshals into `any`, so an id
  above 2^53 read back from a vendor and fed into a follow-up call
  addresses a neighbouring row. `UseNumber` is the one-line change, but
  `Result.Data` flows into node outputs and from there into the expression
  evaluator, so it needs that path verified rather than assumed.

## Adversarial review disposition, round seven (the PR merge gate — 3 findings)

### The one that held the gate: a repository could redefine what an operation does

Rounds four and five pinned two of the three ways a shadowing package can abuse
a connection: the **origin** is recorded at `connections add` with no fallback
to the package's, and the **auth placement** is recorded there and compared in
`checkUsable`. The third was open, and it is the one that matters most:
**nothing pins the OPERATION.**

`<workspace>/connectors` was the FIRST tier, and the workspace is the
repository a run acts on — a checkout this engine treats as untrusted
everywhere else (the author-trust gate, the permission gate, memory-as-data). A
repository shipping `connectors/forgejo/connector.yaml` + `ops/` that keeps the
connector id, the scheme id and the placement passes every check in
`checkUsable` while redefining `forgejo.issue.comment` to `DELETE
/api/v1/repos/{owner}/{repo}` — against the operator's pinned origin, with the
operator's credential.

Two remedies were weighed and both cost more than they buy. A **content digest
of the operation set** pinned on the Connection inverts `checkUsable`'s
deliberate rule (*refuse on mismatch, not on any change*) and would force
re-consenting after every ordinary `iterion connectors gen`. **Pinning the tier
a package came from** breaks under `worktree: auto`, whose workspace path is new
on every run.

So the project tier became a **deliberate grant**:
`ITERION_CONNECTOR_PROJECT_CATALOG=1`, spelled and read exactly like the
`ITERION_CONNECTOR_ALLOW_PRIVATE` precedent one screen above it in the same
file. Closed, `<workspace>/connectors` is not consulted at all; an operator who
generated a connector into their own project (`connectors gen` writes
`connectors/<id>` by default) says so once. The tier is skipped in
`LocalCatalogs` — the one place that decides which roots are catalogs — so
`connections add` and a run still refuse identically.

A tier that is present and ungranted does **not** vanish, because a silently
inert capability is this repo's own definition of a defect: it becomes a tier
that reports why it did not answer, inside the layered lookup's own "no such
connector" text, so every surface says the same thing without any plumbing.
`connections add`'s "no connector catalog found" stopped naming a project root
it would not have read. And an ungranted root that cannot be STATTED is no
longer an error — that refusal was bought for a tier nobody consults, which
would have let a stranger's repository fail every run on the machine by
shipping a `connectors` directory iterion may not read.

| Control | State |
|---|---|
| Pinned origin (which host a credential may reach) | **Shipped** (round four) — recorded at `connections add`, no package fallback |
| Pinned placement (where in the request it goes) | **Shipped** (round five) — `PlacementOf` is the one definition, written and compared |
| Pinned operation identity (what the call DOES) | **Not pinned, and now unreachable by default** — the untrusted tier that could redefine it is a grant (`ITERION_CONNECTOR_PROJECT_CATALOG`). The identity lock this ADR has owed since round two is still owed, and is what a granted project tier or a future team/marketplace tier will need |

### A call that never LEFT was parked as an undecided mutation

The ambiguity class took six rounds and four sites to hold (the tally at the
end of this document). This is the fifth, and it is the mirror image of all of
them: not a failure wrongly read as decided, but a **non-failure wrongly read
as undecided**.

`transportError` classified every `Client.Do` failure on a mutation with no
idempotency key as `unknown_outcome`. `Do` also fails before a byte is written:
the SSRF guard refusing a private host — the default for the self-hosted
Forgejo that is the one connector this catalog ships — a name that does not
resolve, a connection refused, or a header value net/http declines to send
(a pasted credential's trailing newline). Every one of those was reported as
*"the request was sent and no answer came back … reconcile before retrying"*,
refused any retry, classified `AMBIGUOUS_EFFECT` and parked the run terminally,
off the auto-resume list. The very first mutating action against a self-hosted
instance with the guard closed was that.

The default is unchanged — only a cause that **proves** nothing was sent
downgrades, because guessing in that direction is how a duplicate mutation
ships. Three qualify: an `ErrNotSent`-marked cause, a `*net.DNSError`, and a
**dial-stage** `*net.OpError` (deliberately not `read`/`write`, which happen on
an established connection where the request may well have gone out).

The guard's own refusal is an opaque `fmt.Errorf` string, so it is marked where
it is KNOWN rather than matched by text: the local tier wraps its guarded
`DialContext`, and a dialer that returned an error returned no connection.
`Error.NotSent` carries the fact onward, which is also what makes such a
mutation retryable again — a call the vendor never received duplicates nothing.

The header case is fixed at the other end: `buildRequest` now refuses a value
net/http would reject, naming the header and the offending byte and **never**
the value, since for the auth header the value is the credential. A local
refusal never reaches the transport classification at all.

### A paginated walk that failed reported one request instead of the pages it spent

`Result.Requests`' own doc comment names the defect it exists to end — *"a
paginated one that died on page 18 and then walked 20 reported 20 while the
vendor had served 38"* — and it was still live on the failure path, because the
cumulative assignment sat **after** the two failure returns. A walk that died
on page 18 handed back the failing page's own count: 1 for an HTTP error, 0 for
a transport failure, which erased the whole walk. `res.Bytes` went the same
way.

`executeToolNodeAction` adds `res.Requests` per attempt, so a `retry: 3` node
that burned eighteen pages, failed, and then walked twenty reported 21 for 38
served — and that accounting is the only signal an operator has that one node
spent dozens of rate-limit slots. Accumulated before the returns now, with a
test per failure shape (HTTP and transport), each falsified against the old
placement.

### Found beside it: a cursor that does not advance

A cursor walk read termination off the cursor being EMPTY and nothing else, so
a vendor that echoes the same cursor every page — on its last page, or because
the package named a field that happens to be constant — was re-sent it up to
`maxWalkPages` times. 500 of the vendor's rate-limit slots, and 500 copies of
one page returned as the collection. The duplicated items are the worse half: a
workflow acts on them.

A cursor repeated verbatim is the protocol saying nothing, so the walk stops
and reports `complete=false` — the signal `CallPaged`'s contract already tells
a caller to read. The three fixtures that modelled an endless collection with a
constant cursor were modelling an endless *collection* through a degenerate
*protocol*; they advance it now, which is what a real vendor does.


## The ambiguity class, counted across the rounds

Worth stating once, because no single round can see it and each one read like
the last site: **"a mutation whose effect is undecided must never be replayed"
took six rounds and four distinct sites to hold.**

| Round | The site |
|---|---|
| two | the answer LOST after the request left (transport, timeout) |
| three | a 5xx on a mutation, and a 2xx whose body will not decode |
| three | the engine flattened the typed error with `%s`, so the seam never saw it |
| six | a 2xx whose outcome predicate cannot be EVALUATED |
| six | two paths reaching the run's failure code without passing the classifier at all |

Each fix was correct, tested and falsified, and each was the site the report
named. What the tally shows is that the site is not the class: the promise
lives wherever a failure becomes a code, and that is a set you enumerate by
grep, not by fixing what was reported. The one measure that would have closed
it earlier is the one the corpus already prescribes — ask "who ELSE turns a
failure into a classification?" and answer with a grep of the constructors,
before declaring the round done.

The composition test (`TestAnUndecidedMutationParksTheRunForAnOperator`) exists
for the same reason: both ENDS of the chain had tests through five rounds while
the middle — the engine actually persisting the classified code — had none, and
a sibling helper one refactor away writes `EXECUTION_FAILED`, which is on the
auto-resume allow-list.
