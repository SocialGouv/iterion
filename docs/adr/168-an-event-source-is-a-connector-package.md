# ADR-168 — An event source is a connector package: a pinned verification contract, a sanitized event, a durable intake, and a subscription that maps the event onto a bot's vars

- **Status**: Proposed — the operator set its direction on 2026-11-14: native connector packages rather than an app aggregator at runtime; a first wave of Slack, Mattermost, Jira, Linear, Sentry, Alertmanager and polled RSS/Atom and Jira; delivery to a bot's parked run in scope, its semantics left to #3135. Plan review by codex gpt-6-astra (xhigh), two rounds of 22 findings each, dispositions at the end. Acceptance is the operator's.
- **Date**: 2026-11-14
- **Deciders**: jo (direction, the three arbitrations), Claude (the design), codex gpt-6-astra (the plan review)
- **Extends**: [ADR-098](098-connector-catalog.md) — this is the "trigger ingress family" its `triggers.yaml` section defers to a later lot of #1072: event filters, payload mapping, durable polling cursors and the studio surfaces. [ADR-046](046-event-driven-runs-trigger-spine.md) — the spine's envelope, subscriptions and evaluator are the binding this record extends.
- **Relates to**: [ADR-094](094-trigger-effect-outbox.md) (effects durable before the cursor moves) · [ADR-117](117-server-side-leader-lease-for-idempotent-nets.md) (an elected replica per net, which is not mutual exclusion) · [ADR-034](034-webhook-dual-mode-auth-token-header-vs-body-hmac.md) (webhook authentication) · [ADR-138](138-forge-provisioning-immutable-identities.md) (provisioned hook identities and epochs) · [ADR-051](051-in-bot-event-driven-primitives.md) and [ADR-081](081-async-human-interaction.md) (the parked run an event will reach) · [ADR-139](139-language-stays-go-sidecars-for-the-rest.md) (no code loaded in process)
- **Serves**: [#3269](https://github.com/SocialGouv/iterion/issues/3269) (the event-source catalog) · #1072 (its P2 inbound half and its P3 polling cursors) · #3133 (standing crews) · [the live-agents study](../references/live-agents-openmausbot.md)

## Context

An operator wants to connect event sources — a chat tool, a ticket tracker, a monitor, a feed — and plug one into a bot in one click, from a catalog where each source carries its logo. OpenMausBot offers this by delegating its "connected apps" to Composio, a hosted aggregator that handles the OAuth and relays the events of hundreds of applications.

iterion has the pieces of this and not the whole:

- **One inbound lane, one event.** ADR-098's connector lane (#2761, `pkg/server/webhooks_connector.go`) believes a delivery through a connection's `trigger` grant and the package's declared events. It is entered from the Forgejo route only, and it parses one payload into a Go struct (`forgejoIssueComment`) whose fields become hardcoded vars (`forgejo_event`, `issue_number`, `comment_id`…).
- **Closed sets where the next variant should be data.** `trigger.Source` (`pkg/trigger/event.go`), `webhooks.Provider` (`pkg/webhooks/types.go`) and the manifest's `invocations.kind` (`pkg/bundle/manifest.go`, its DTOs and `studio/src/api/triggers.ts`) are each a closed list — the smell philosophy §2 names.
- **Five binding stores.** A bot is bound to an event through `webhooks.Config` (and its `BotRules`), `cloudsched.ScheduledBot`, `trigger.Subscription`, `schedules.yaml` or `iterion.dispatcher.yaml`. The connector lane picks its bot from the webhook config's default, so one webhook launches one bot. The local `trigger.Subscription` store is in memory and is rebuilt from manifests with fresh ids at every start (`pkg/cli/trigger.go`).
- **Nothing to show.** `connector.yaml` has no icon or category, no route lists the packages or their triggers, and the studio types a connector's name as free text.
- **No filter, mapping or polling.** `triggers.yaml` carries `id`, `event` and `description`; ADR-098 left the rest deferred on purpose, each being a field invented before its reader.
- **Admission before authentication.** On an HMAC-signed webhook, the middleware charged the per-webhook rate bucket and the tenant's durable monthly counter before the provider handler verified the signature, so anyone holding a hook's URL could spend a tenant's quota. #3276 fixes the existing lanes; this record states the rule for the new one.

This record is the reader ADR-098 waited for.

## Decision

### 1. A source plugin is a connector package

The catalog's unit is an ADR-098 package and nothing else: no new artifact kind, no `plugin.yaml` contribution (ADR-098 refused that manifest's lifecycle and tenancy for connectors). A package that declares triggers **is** a source, resolved through the same tiers — baked, platform, team, git — under the same maturity rule: a `spotted` package is listed and inert.

**The measure of the seam.** Once this record is implemented, adding a source whose wire contract fits the declared vocabulary is files under `connectors/<id>/` and no Go. Each package of the first wave is checked for it — an empty `git diff --stat` outside `connectors/`, the package's own tests aside. A vendor that needs a new verification scheme, body encoding or response format adds it in Go, argued on its own: those are the vocabulary, not a source.

### 2. What a package shows: `connector.yaml`'s presentation fields

Optional fields at the package's constant schema version (ADR-098's growth rule):

- `icon:` — a file of the package (`icon.svg`), sanitized once at load: `script`, `foreignObject`, every `on*` attribute, every external reference (`href`/`xlink:href` and `url()` outside a `#fragment`) and anything that is not SVG are removed. A file above `ITERION_CONNECTOR_ICON_MAX_BYTES` (64 KiB by default; 0 removes the cap) or that the sanitizer cannot parse fails the package's validation, naming the file.
- `brand_color:` — a `#rrggbb` for the monogram fallback and the tile's accent, contrast-checked against the surface tokens.
- `category:` — an open vocabulary (`chat`, `tickets`, `monitoring`, `forge`, `feeds`, `calendar`, …); an unknown value is a C1xx warning, never a refusal (philosophy §1).
- `homepage:`, `docs_url:`, and `provenance.icon:` — where the logo comes from (the vendor's press kit, or simple-icons under CC0). A brand's mark is used nominatively, to name the service.

`GET /api/v1/connectors/{id}/icon` serves the sanitized bytes as `image/svg+xml` with `Content-Security-Policy: default-src 'none'; style-src 'unsafe-inline'; sandbox`, `X-Content-Type-Options: nosniff` and an ETag equal to their digest. The studio renders a logo as an `<img>`, never inline: the sanitizer and the image context are two defences, so neither has to be perfect alone. A package with no icon gets a monogram tile.

### 3. The verification contract: `triggers.yaml`'s `ingress:` block

A document-level block, optional, at `schema_version: 1`. Every reader of a delivery for this package reads it, so the lane holds no vendor's name:

```yaml
ingress:
  body: json                       # json | form — decoded once, under the body cap
  credential:                      # what proves the sender
    scheme: hmac-sha256            # hmac-sha256 | hmac-sha1 | token | bearer | basic
    in: header                     # header | body | query
    name: X-Forgejo-Signature
    encoding: hex                  # hmac only: hex | base64
    prefix: ""                     # Slack: "v0="
    signed: "{body}"               # hmac only; Slack: "v0:{timestamp}:{body}"
    secret: minted                 # minted (iterion generates it) | vendor (the operator pastes the vendor's)
  timestamp:                       # optional replay window
    in: header                     # header (Slack) | body (Linear: webhookTimestamp)
    name: X-Slack-Request-Timestamp
    unit: s                        # s | ms
    tolerance: 5m
  installation: 'payload.team_id'  # optional: the authenticated identity a delivery must carry (§6)
  event: { header: X-Forgejo-Event }          # or { field: type }
  delivery_id: { header: X-Forgejo-Delivery } # metadata only (see "identity")
  headers: [X-Forgejo-Event, X-Forgejo-Delivery]   # the only headers an expression may read
  redact: [token]                             # body fields removed from the event
  handshake:
    when: 'payload.type == "url_verification"'
    respond: 'payload.challenge'
  ack: { status: 200, body: "" }
```

- **Bounded extraction, then verification, then nothing else.** The lane reads at most the body cap, decodes it once when a credential, a timestamp or an installation lives in the body, extracts exactly the declared fields — refusing a body where one is missing, repeated or not a scalar — and verifies the signature over the raw bytes and the timestamp within its tolerance. A delivery that fails answers 401, is logged, and changes no durable state: no intake row, no monthly counter, no per-webhook rate bucket. Before verification only a per-source-IP limit applies.
- **The signed-string template is validated.** `signed` must contain `{body}`. It must contain `{timestamp}` exactly when the timestamp is read from a header — a timestamp in the body is already authenticated by `{body}`, as Linear signs it. A template that drops a required component fails the package's validation.
- **Identity comes from authenticated material.** The intake's dedup identity is the digest of the signed body, plus the timestamp when it is signed separately. A vendor's delivery-id header is unsigned, so it is kept as metadata and never trusted for dedup: changing it on a captured request must not open a second intake. A captured request replayed within the dedup retention is absorbed; a vendor without a signed timestamp gets no stronger replay protection than that, and the setup matrix (§6) says which ones.
- **The installation is authenticated, not routed.** A vendor whose one endpoint serves several installations (a Slack app's single request URL) declares `installation:`, an expression over the authenticated body. Binding records the value the operator's connection belongs to, and a delivery carrying another value is refused. The URL says which ingress was hit; only the signed body says whose event it is.
- **The consented contract is pinned.** Binding an ingress records the digest of the package's `ingress:` block as the operator consented to it. A delivery whose package now resolves to a different contract — a team override of a baked package, a new version — is refused with a typed error naming both digests until the operator re-binds. This is the inbound twin of the outbound resolver's refusal of a changed credential placement (`pkg/connection/resolve.go`).
- **The secret's origin is declared.** `minted`: iterion generates the secret, shows it once and gives it to the vendor (or provisions it, §6). `vendor`: the vendor generates it (a Slack app's signing secret, a Mattermost outgoing webhook's token) and the operator pastes it; the ingress route accepts it on creation and on rotation. It is sealed like every webhook secret, and rotation opens an ADR-138 epoch.
- **The answer is declared, and admission precedes it.** An authenticated delivery is admitted against the tenant's quota and against a per-tenant cap on unprocessed intake (`ITERION_CONNECTOR_INTAKE_BACKLOG_MAX`, 0 removes it): over the cap it answers 503 so the vendor retries later. Admitted, it is answered with the package's `ack` (200 and an empty body by default) as soon as its intake row is durable (§8), never after matching: a vendor's deadline (Slack's three seconds) does not wait for a run to start, and a vendor that reads any other status as a failure (Linear) gets exactly what it expects.

### 4. The sanitized event

What an expression, a preview, the intake store, the bus and a launched run see is the **event**, never the request: `event.<output>` (the declared outputs, §5), `event.payload` (the decoded body without the `redact` fields and the credential field) and `event.headers` (only the headers listed in `headers:`; `Authorization`, `Cookie` and the credential header are never listable). The raw body and the full headers exist only inside verification and dedup, and are never persisted, logged, published or passed on. An event's text reaches a prompt as untrusted input, in the same class as a forge comment today.

### 5. What a trigger declares

Per trigger, every field optional beside ADR-098's `id`, `event` and `description`:

- `display_name:` — the catalog's label.
- `delivery: webhook | poll` — `webhook` by default.
- `outputs:` — `{ name, from: <expr>, description, type: string | int | bool | list | map }`, what the studio offers for mapping and what a run receives by default. Reserved names, each with a fixed type, fill the `trigger.Event` envelope and the launch: `subject`, `subject_state`, `actor`, `repo`, `thread` (strings), `labels` (list of strings), and `repo_url`, `repo_ref` (strings), which become the launch's clone target. A `repo_url` is believed only when its host is the connection's base URL host or is in the ingress's repository allowlist; otherwise the delivery is refused, naming the host. `thread` keys the conversation a delivery belongs to (a Slack `channel` and `thread_ts`, a Mattermost `root_id`, an issue key), which the `deliver` mode reads.
- `filters:` — `{ id, from: <expr>, type: string | enum | bool, options, default, selector }`, predicates over the event. A subscription stores the chosen values; matching compares them by equality or membership. `selector:` names an operation of the same package whose result fills a picker.
- `ignore:` — an expression that drops an event before matching: the loop guard (a bot's own Slack message), stated by the package because only the package knows the vendor's markers.
- `sample:` — a file under `samples/`, a real delivery with its secrets removed, which the preview and the package's tests read.
- `provision:` — §6.
- `poll:` — §7.

### 6. The binding is a `trigger.Subscription`

No sixth store. The ingress and the binding are split along what each owns.

**The ingress** is a `webhooks.Config` with `Provider: connector`, the last value of the closed provider set; the vendor is read from the connection, the route is `POST /api/webhooks/c/{ingress_id}`. An ingress is one per (connection, registration scope): a Forgejo connection that serves three repositories has three ingresses, each with its own hook and secret, because Forgejo registers hooks per repository (`pkg/forge/admin.go`). An ingress holds the sealed secret and its origin, the pinned contract digest, the bound installation value, the operator's allowlists and hold-label veto, and its provisioning state.

**The binding** is a `trigger.Subscription`:

- **The event carries where it came from.** `trigger.Event` gains `ConnectionID` and `IngressID` (or `PollerID`), set by the server, never read from a payload. A connector subscription stores a scalar `connector_key` (`<connection id>/<trigger id>`), and `ListCandidates` filters on tenant, enabled and `connector_key` over a Mongo index on those three scalar fields — two array fields cannot share one compound index — so two connections to one vendor never fire each other's bots.
- **`trigger.SourceConnector`** is the last constant of the closed source set: what follows is `Kind`, the trigger id, which is data.
- **One discriminator, refused where it is unsupported.** The existing `Mode` stays the only field that says what a match does, and gains `deliver`. Every constructor and dispatcher that reads it — the `From*Invocation` builders (which today force `direct` for schedule and keepalive, and default board invocations to `board`), `EffectiveMode`, the evaluator, the forge dispatch — refuses a kind and mode it cannot honour, with a typed `unsupported` result, before any subscription is built; nothing defaults an unknown mode to a launch. `deliver` stays refused until #3136 ships it. `board` is refused on a connector subscription: the board effect promotes an **existing** native card (`pkg/server/trigger_cloud.go`), and turning an external event into a card is a contract of its own.
- **The rest of the binding**: `filters` (chosen values, plus an optional `when:` expression), `var_map` (a var name to an expression over the event), for a polled trigger the `poller` it listens to (§7), and `no_live_run: launch | drop`, stored now and read by #3136. With no `var_map`, each output named like a declared var of the bot is passed under that name. Saving a subscription validates the map against the bot's declared vars (`botregistry.EntryWithSchema.Vars`) and answers 400 on an undeclared target or an unfilled required var.
- **Durable everywhere, bootstrapped idempotently.** The local subscription store becomes persistent under the run store (`.iterion/`), beside the Mongo store and a memory store kept for tests; intake, pairs and poll state follow the same three-way rule. Manifest-derived subscriptions get stable ids (from bot, kind, trigger and connection) and are reconciled, not regenerated: an operator's disable or delete leaves a tombstone the next start honours. The workers start from durable state whether or not the manifest scan finds anything.
- **The manifest suggests, the operator binds.** `invocations` accepts `kind: connector` with `{ trigger, mode, filters, vars }`. A manifest-derived subscription's identity is (bot, kind, trigger id, connection), so a bot written for Slack and Jira gets two; the `from-invocation` route gains a connector builder and stops refusing a second subscription of one kind. Every consumer of the kind enumeration — manifest validation, the registry DTOs, the studio types — accepts it. The `.bot` text gains nothing (ADR-046).

**The routes the wizard needs**, team-scoped in cloud; locally the same handlers under `/api/v1/` on the local tenant, as `/api/v1/triggers` already is, because a local process has no team (`pkg/server/server_routes.go`):

| Route | What it does |
|---|---|
| `GET …/sources` | the catalog (§10) |
| `POST …/connections` | a connection; accepts `auth kind: none` and `basic`, and the `trigger` + `action` grants |
| `POST …/connections/{cid}/selectors/{filter}` | runs a filter's `selector` operation through the resolver |
| `POST …/ingresses` | an ingress for a connection and a registration scope; mints a secret or accepts the vendor's |
| `POST …/ingresses/{iid}/provision` · `DELETE …` | the provisioning lifecycle below |
| `GET/POST/PATCH/DELETE …/triggers*` | subscriptions |
| `POST …/triggers/preview` | the filter verdict and the vars a run would receive, against a pasted payload or the sample; launches nothing |
| `GET …/intakes?state=dead` · `POST …/intakes/{id}/replay` | the dead-letter view and replay (§8) |

**Provisioning** follows ADR-138's discipline. A package that provisions declares `create`, `delete` and `list` operations, a `match:` expression that recognises its own hook in a `list` result, and `id_from:`. Their params see a registration context: the ingress URL and secret, the epoch, a marker unique to the epoch, the scope, and on `delete` the remote hook id. The ingress records the desired state, the epoch, the remote id once known, and the cleanup owed by a retired epoch. A create whose response is lost is resolved by `list`: one match is adopted; none means the create may still land, so the epoch stays pending and the reconciler lists again before it creates; several are an error naming the remote ids. A late create of a retired epoch is deleted by its cleanup obligation. A package without `list` cannot provision: its setup is manual.

**Which first-wave source is one click**:

| Source | Credential | Replay protection | Hook setup |
|---|---|---|---|
| Forgejo | HMAC header, minted | body digest | provisioned per repository |
| Jira | HMAC header (`X-Hub-Signature`), minted | body digest | provisioned through the webhook REST API |
| Slack | HMAC with header timestamp, vendor | timestamp window + digest | manual (`setup.md`): one app per connection, its `team_id`/`api_app_id` bound as the installation |
| Mattermost | token in the body, vendor | body digest | manual in the first wave |
| Linear | HMAC header, body timestamp in ms, minted | timestamp window + digest | manual in the first wave: its hook API is GraphQL, which the executor does not speak yet |
| Sentry | HMAC header, vendor | body digest | manual (`setup.md`) |
| Alertmanager | bearer or basic, minted | body digest | manual (`setup.md`) |
| RSS/Atom, Jira JQL | none, or the connection's | — | polling |

### 7. Polling

A trigger with `delivery: poll` declares a query and how to read it:

```yaml
poll:
  operation: jira.search.issues          # paginated by the operation's own contract
  params: { jql: 'concat(query.jql, " AND updated >= \"", cursor.value, "\" ORDER BY updated ASC")' }
  query: [ { id: jql, type: string } ]   # what an operator writes; part of the poller's identity
  items: 'response.issues'
  item_id: 'item.key'
  item_version: 'item.fields.updated'
  cursor: { from: 'item.fields.updated', kind: timestamp, overlap: 1m }
  interval: { default: 5m, min: 1m }
```

- **A poller is a query.** Its identity is (tenant, connection, trigger, the digest of its `query` values). A subscription to a polled trigger names its poller, and an intake from a poller is matched only against that poller's subscriptions — two overlapping queries never deliver one item twice to one subscription. `query` values shape the request; `filters` stay predicates over each item.
- **Pages follow the operation's contract.** The scan uses the operation's declared pagination (`CursorParam`/`PageParam`, `pkg/connector/spec/package.go`) through `exec.CallPaged`. A scan that ends incomplete — a page error, a repeated token, `complete=false` — records its progress for the next tick and never moves the checkpoint.
- **What polling promises: the revisions it observes.** A tick re-reads `overlap` before the checkpoint so equal timestamps and late-visible records are seen again, and a seen set of (`item_id`, `item_version`) removes what was already accepted. An entity changed twice between two ticks yields one event, its observed revision; a record that becomes visible later than `overlap` is missed. That is the honest contract of snapshot polling, and the catalog states it per trigger. Completeness needs a change-history source (see "Not decided here").
- **Feeds.** An RSS or Atom item's identity falls back from `guid` to `link` to the digest of its canonical content; its version is that content digest. The seen set retains an identity while its entry is still in the feed, plus `ITERION_CONNECTOR_POLL_SEEN_RETENTION` after it leaves, so an item is seen once for as long as it is observable.
- **Accept, then checkpoint, under a fence.** Every observed item becomes an intake row, idempotent on (poller, `item_id`, `item_version`), before the checkpoint moves (ADR-094's order). `pkg/lease` elects a holder but issues no fencing token, so the poller's own record carries one: a holder that takes over increments a generation by compare-and-swap, every checkpoint write carries it, and a write from an older generation — or a lower checkpoint — is refused. Two holders overlapping produce duplicate intake attempts, which the identity absorbs.
- **Eligibility is iterion's sequence, not the vendor's clock.** Intake rows carry a per-poller sequence number. A subscription records the sequence at its activation and receives only intakes above it, so a late-visible item whose source timestamp predates the activation is still delivered. A new poller's first scan records its intakes as primed — durable, matched to nothing — so enabling a source never replays a backlog. `backfill: N` on a subscription makes the last N primed or earlier intakes of its poller eligible for it alone.
- **Outbound authority.** Polling, provisioning and selectors spend the connection's credential, which the `trigger` grant does not authorize (`pkg/connection/connection.go`). They go through the connection resolver like an `action:` node — status, expiry, maturity, origin and placement checked — and need the `action` grant; the wizard asks for both in one consent.
- **Guards and bounds.** The operation runs server-side through `pkg/connector/exec` behind the cloud client's SSRF-guarded dialer, which refuses a private address. `exec` gains a `format: feed` decoder (RSS 2.0, Atom 1.0, `encoding/xml`, no new dependency). `interval.min` holds unless `ITERION_CONNECTOR_POLL_MIN_INTERVAL` lowers it.

### 8. From intake to effect

1. **Accepted means durable.** After verification and admission (§3) the lane answers a handshake if it is one, applies `ignore`, extracts the outputs and writes an **intake row** — the sanitized event, its connection, ingress or poller, trigger id, dedup identity and sequence — then answers `ack`. A redelivery with the same dedup identity meets the existing row and is answered `ack` without a second one.
2. **Matching runs after the answer, and its outcome is stored.** A worker claims an intake, lists its candidate subscriptions in a stable order (by subscription id), evaluates each one's filters and `when`, and writes one **pair** per candidate into a pair store of its own, keyed uniquely on (intake id, subscription id): matched, not matched, or refused with its reason — an expression that exhausted its budget is a refused pair, visible and replayable, never a silent omission. The intake is marked matched only when every candidate has its pair. A fan-out of any width is so many rows, not one growing document.
3. **A pair's launch identity is its run id.** A matched pair launches with a run id derived from (intake id, subscription id), which `LaunchSpec.RunID` already accepts (`pkg/runview/service_launch.go`). The launch path owes one property in return: submitting a run id that already exists **completes** that run's submission — publishes a persisted run that was never published — instead of failing or creating another. A pair is `launched` only once the submission is acknowledged; a run record that exists proves nothing more than a submission to complete.
4. **Transient failures retry; exhausted ones are visible.** A pair moves `pending → claimed(owner, generation, expiry) → launched | refused | failed`, and `failed` re-enters `pending` with backoff (`ITERION_CONNECTOR_PAIR_MAX_ATTEMPTS`, `…_BACKOFF`) until its attempts are spent, as `trigger.EffectOutbox`'s `MarkRetry` already does for board effects. Every write is fenced by the claim's generation. A claim past its expiry is recovered by a reconciler under a lease (ADR-117) through the same submission. A pair out of attempts, and an intake whose matching keeps failing, land in the dead-letter view and are replayable from it.
5. **The bus hears it, observationally — readers first.** The lane publishes each event with an explicit `observational` marker, and the evaluator skips marked events whatever their payload. Rollout is reader-first: a release whose evaluators honour the marker ships before any publisher emits an event that carries the marker without a launched run id; until then publishers keep the run id and do not publish filtered or zero-success events.
6. **Modes.** `direct` launches through the shared launch path, with its quota, routing, trust and var layers. `board` and `deliver` are refused (§6).

### 9. Expressions: one language, a context per site

Every expression is `pkg/dsl/expr`, under its existing per-expression work budget (`ITERION_DSL_EVAL_BUDGET`). The candidates a delivery is evaluated against are the tenant's own subscriptions, so a hostile payload controls the input, not the count; an exhausted budget is a per-pair verdict (§8.2). `expr.Context` gains named callbacks for the namespaces below, and an unknown root is an error at validation time, never the evaluator's implicit `input` fallback.

| Site | Namespaces | Validated at |
|---|---|---|
| `ingress.installation`, `event`, `handshake.when`/`respond` | `payload`, `headers` (listed) | package load |
| `outputs[].from`, `filters[].from`, `ignore` | `payload`, `headers` (listed) | package load |
| `poll.params` | `query`, `cursor` | package load |
| `poll.items`, `item_id`, `item_version`, `cursor.from` | `response`, `item` | package load |
| `provision.*.params`, `match`, `id_from` | `registration` (url, secret, epoch, marker, scope, remote id), `response` (`match`, `id_from`) | package load |
| subscription `when`, `var_map` | `event` (outputs, payload, headers) | subscription save |

An absent path evaluates to nil, and a nil output is absent: the var it maps to is not set, never set to an empty string. Values reach a launch as strings by one conversion — a string as is, a number in canonical decimal, a bool as `true`/`false`, a list or map as compact JSON. The preview and the lane call the same evaluator and conversion.

### 10. The catalog is a read model

`GET /api/teams/{id}/sources` (and `/api/v1/sources` locally) answers the packages that declare triggers across the tenant's tiers: presentation fields, maturity and tier, push or poll and what polling promises, the grants a source needs, its setup mode and replay protection, the tenant's connections, ingresses and pollers for it, and the subscriptions on each. Beside them it lists the built-in sources — schedule, board, run outcome, emit — as descriptors the studio routes to their existing dialogs. It stores nothing (philosophy §5).

### 11. The Forgejo lane migrates under one authority at a time

The connector-bound Forgejo configs of #2761 are live, and a rolling deploy runs old and new replicas together. Two launchers that each believe they own a delivery would launch twice — the legacy lane keys its idempotency on the delivery record and lets the launch mint its own run id — so the migration never has two authorities:

- **Release A (expand).** The decoders accept every new field, and no shipped package uses one: an older replica must never meet a package it cannot decode (`pkg/connector/spec/triggers.go` decodes strictly). Every replica of A reads, per delivery, an `authority` field on the legacy config row: unset means the legacy lane, `subscriptions` means the new one. A reconciler under a lease creates, for each connector-bound config, its ingress and subscription with deterministic ids, disabled. Nothing switches.
- **The switch, once the whole fleet runs A.** One compare-and-swap writes `authority: subscriptions` on the legacy row and enables the migrated subscription. Every replica reads the field per delivery, so exactly one lane owns each delivery at every instant. From then on the subscription is the record an operator edits, disables or deletes; the legacy row keeps only the pointer.
- **Rollback.** To A: A reads the field and keeps the new lane. Below A, after the switch: the old code reads the legacy row as it was at the switch and never sees the intake rows, so rolling below A requires draining the intake first. That is an operator procedure the release notes state, and the dead-letter view's undrained count is what the operator checks before it.
- **Release B (contract).** The `forgejoIssueComment` struct and the legacy branch are removed once no row lacks the switch.

**Parity is the whole launch, without a Forgejo branch.** The witness compares the complete launch request of a migrated delivery with today's — each var with its presence or absence (`issue_number` and `comment_id` are set only when non-zero), the operator's var layers and their precedence (`applyWebhookVarLayers`), the clone target now carried by the `repo_url` output under its trust rule, the run's trust class, routing, credential pins and launch policies — and the new lane reaches it through the package's declarations alone.

## Consequences

- A new source whose wire contract fits the vocabulary costs a package: a `connector.yaml`, a `triggers.yaml`, a logo, a sample, and provisioning operations or a `setup.md`.
- The connector lane loses its last vendor name, under a single authority throughout its migration.
- One delivery can serve several bots, each with its own filters, mapping and connection, where a connector webhook launched exactly one.
- Three closed sets each gain one last member — `trigger.SourceConnector`, `webhooks.ProviderConnector`, the invocation kind `connector` — and `Mode` gains `deliver`, refused until #3136.
- The launch path gains an idempotent submission by caller-chosen run id, which any other at-least-once launcher can reuse.
- New durable state, each with its Mongo twin, its persistent local twin and its memory twin in the change that introduces it: the extended subscription and its tombstones, the ingress with its pinned contract, installation and provisioning state, the intake rows, the pairs, the pollers with their checkpoint, fence and seen set.
- The studio gains a catalog, a plug wizard, a dead-letter view and a logo component; the local server gains the connection, ingress and source routes.

## Alternatives rejected

- **An app aggregator at runtime** (Composio, Pipedream, Nango). It puts a third party in every event's path and a vendor's catalog in place of the packages' — the opposite of #1072's decisions, which already refused an Activepieces worker and Pipedream at execution time. Declined by the operator on 2026-11-14. Reopens on a named deployment that needs a source no package can describe and accepts the third party.
- **Binding in `webhooks.Config.BotRules`.** A second spine, out of reach of the board and schedule sources, the Automations view and the cloud subscription store.
- **Triggers declared in the `.bot` text.** A connection, a channel and a tenant are a deployment's facts.
- **A brand-icon package in the studio** (simple-icons on npm). The logo would belong to the studio, so a team's own package could not bring its own.
- **Matching inside the request.** A vendor's acknowledgement deadline does not wait for N subscriptions, and a crash between the answer and the effects would lose what the vendor believes delivered.
- **Two authorities sharing an idempotency key during the migration.** The legacy launcher keys idempotency on its delivery record, not on the run, so a shared key would not stop two launches; one authority field read per delivery does.
- **The lease as a fence.** `pkg/lease` elects and expires; it does not order writes. The poller's own generation does.
- **A separate `effect` field beside `Mode`.** Two fields saying what a match does would disagree, and today's dispatch defaults the unknown to a launch.
- **An aggregate evaluation budget per delivery.** The candidate count is the tenant's own; a per-pair verdict keeps every subscription's outcome visible instead of letting an early one starve the rest.
- **A JSONPath library for extraction.** A second expression language beside the DSL's.

## Not decided here

- **The `deliver` mode's semantics** — the standing-bot address, the host door, the queue, the no-live-run policy, the successor when a run's budget is spent: #3135, then #3136.
- **Turning an external event into a board card** — its title and body, its identity across updates, grouping, the dispatcher staying the launch authority. Until then `board` is refused on connector subscriptions.
- **Complete change capture** — a vendor's change-history or audit source (Jira's changelog) behind a periodic reconciliation, for a subscription that must see every revision rather than the observed ones.
- **Streaming sources** (Slack Socket Mode, a Mattermost websocket): an elected holder of a long-lived connection per tenant. They reopen on a source that offers neither a webhook nor a poll.
- **Slack distributed apps** (one app, many workspaces, OAuth installs): #1072's P2 managed OAuth apps. The first wave binds one app to one connection.
- **GraphQL operations in the executor**, which Linear's hook provisioning and selectors would need.
- **Distributing packages through the marketplace**: #1072 refused marketplace installs in cloud, and that stands.

## Witnesses this record owes

- **Verification.** For each scheme and credential location, with real vendor signature fixtures (Slack, Linear, Forgejo, Jira, Sentry): a wrong secret, a missing or repeated credential field, a timestamp outside the window in seconds and in milliseconds answer 401 and leave the intake store, the monthly counter and the per-webhook bucket unchanged. Skip the verification, and each test fails.
- **Identity and installation.** A captured signed request with a changed delivery-id header opens no second intake; a Slack delivery signed by the right app for another `team_id` is refused.
- **The pin.** A team override that weakens a baked package's verification is refused until re-bound.
- **The canary.** A unique value in the credential field and in an unlisted header reaches no expression, preview, intake row, bus event or run.
- **Isolation.** Two connections to one vendor in one tenant, and two overlapping pollers: each delivery reaches only its own subscriptions, once — checked against a real Mongo, its index created and its query plan read.
- **The pair machine.** A crash before the launch, after the run is persisted but before it is published, and after publication but before the pair is marked, each end with exactly one published run. A returned launch error retries with backoff and lands in the dead-letter view when its attempts are spent; replay launches it once. Each built in both orders.
- **The answer and the backlog.** An accepted delivery is answered with the package's exact `ack` while matching is pending; over the backlog cap the answer is 503 and nothing is written.
- **Polling.** An older generation's checkpoint write and a lower value are refused immediately after a takeover; a crash between intake and checkpoint loses nothing; an incomplete scan does not advance; equal timestamps across a page boundary are neither skipped nor doubled; a late-visible item older than a subscription's activation still reaches it; an RSS item without `guid` or `link` is seen once while it stays in the feed; a private feed URL is refused.
- **Modes.** `deliver` through every `From*Invocation` builder and the forge dispatch, and `board` on a connector subscription, answer `unsupported` and launch nothing. An observational event is never re-evaluated, including by a reader of the previous release.
- **Invocations and restarts.** One bot with a Slack and a Jira invocation gets two subscriptions; repeated local restarts keep their ids and honour a tombstone; a restart with an empty manifest scan still drains saved intake.
- **Migration.** A migrated Forgejo delivery produces the same complete launch request as today; across the switch on a mixed fleet, one delivery launches one run.
- **The seam.** Each first-wave package lands with an empty diff outside `connectors/`.
- **The logo.** An `icon.svg` carrying `<script>` and an external `href` is served without them, with the CSP header.

## Adversarial review disposition

Each finding was checked against the code before its disposition; the ones with the widest blast radius were reproduced by reading the cited lines.

### Round one (codex gpt-6-astra, xhigh — 22 findings)

- **Adopted** — **F1** the pair state machine with a recoverable launch identity · **F2** credential and timestamp locations, units and body encoding · **F3** the sanitized event and its canary · **F4** admission after verification, for every lane (the existing lanes are fixed by #3276) · **F5** the pinned contract · **F6** the connection on the event and the index · **F7** entity versus version · **F8** complete scans, overlap and the seen set · **F9** accept before checkpoint, a fenced monotonic checkpoint · **F10** a beginning per subscription · **F11** a migration in stages · **F12** parity on the whole launch request · **F13** persistent local stores · **F14** provisioning on ADR-138's discipline · **F15** outbound operations through the resolver under the `action` grant · **F17** the observational marker · **F18** `Mode` as the one discriminator · **F19** the declared `ack` after a durable intake · **F21** invocation identity and the connector builder · **F22** a context per expression site.
- **Adjusted** — **F16** the board effect needs an existing card: `board` is refused on connector subscriptions and materialization is named as undecided · **F20** the wizard's routes and the setup matrix are adopted; Linear's setup is manual because the executor speaks no GraphQL yet.

### Round two (codex gpt-6-astra, xhigh — 22 findings)

- **Adopted** — **F1, F4** one authority field read per delivery, switched by compare-and-swap once the fleet is homogeneous, with rollback and drain rules (§11) · **F2** a pair is launched only once its submission is acknowledged; the launch path completes an existing run id's submission · **F3** retries with backoff, attempt limits, a dead-letter view and replay · **F5** unsupported kinds and modes refused in every builder and dispatcher · **F6** `{timestamp}` required only for a header timestamp · **F7** a poller is a query; intakes match only its subscriptions · **F8** eligibility by iterion's intake sequence; durable priming · **F9** one ingress per (connection, registration scope) · **F10** the authenticated installation · **F11** dedup on signed material; vendor delivery ids are metadata · **F12** `repo_url`/`repo_ref` outputs under a host trust rule · **F13** reader-first rollout of the marker · **F14** a scalar `connector_key` index · **F15** pagination by the operation's contract; an incomplete scan never advances · **F17** feed identity by content digest, retention while the entry is observable · **F18** a fence the poller record issues, since `pkg/lease` has none · **F19** `list`, `match` and the registration context; zero, one and several matches · **F21** a pair store of its own; a backlog cap before the answer · **F22** stable ids, tombstones, workers started from durable state.
- **Adjusted** — **F16** polling promises the revisions it observes, stated per trigger; complete capture is named as undecided rather than promised · **F20** the aggregate budget is removed rather than given progress semantics: the candidate count is the tenant's own, and an exhausted per-expression budget is a visible, replayable pair verdict.
