# Sovereign runner pools

A team whose code must never reach a public LLM vendor runs on a **sovereign
runner pool**: a dedicated set of runner pods whose only LLM credential is the
team's own OpenAI-compatible gateway, and whose runs are isolated from the
other tenants by code — routing, admission and credentials, not network
fencing (the deployment stays the shared iterion.cloud install; there is no
dedicated namespace and no egress filtering in this implementation). The
operator-selected next deployment boundary is pool-specific RBAC and
NetworkPolicy in the shared namespace; those separate changes must be
deployed before claiming network enforcement.

The isolation chain, in the order a run meets it:

1. **Mapping** (super-admin): `PUT /api/admin/teams/{id}/runner-pool` maps a
   team to a pool. The mapping is written only through that audited route
   (every other team writer is a patch), and the launch resolver reads it
   FRESH — a store error refuses the launch rather than guessing.
2. **Registry** (`PUT|GET /api/admin/runner-pools`): which pools exist and
   their lifecycle — `provisioning` (topology up, routing refused),
   `active` (the pool's runners are deployed and consuming: routing flows),
   `draining`, `disabled`. A mapped team whose pool is missing or not
   active has its launches and resumes REFUSED, naming the pool and the
   state — never routed onto the shared pool. The mapping enforces the
   one-team-per-pool invariant (D13): a second team onto a held pool is
   refused with the holder named — two teams on one pool would let each
   team's runs carry the other's credentials to the same pods.
   A pool team's launch also consults no shared credential tier — org,
   contributor pool and platform credentials never enter its bundle, so
   the pod holds no stranger's key; a route nothing of the team's own
   funds refuses the launch with the remedy named.
3. **The frozen stamp**: the run document and the wire message carry the
   pool from launch. Resume follows the frozen stamp and refuses a
   re-mapping (`ErrPoolRemapped` — a pool move is a new launch). Forks and
   subbot children inherit the stamp.
4. **Per-pool topology**: one run stream + one DLQ stream + one durable
   consumer per pool (`ITERION_RUNS_POOL_<p>` / `iterion-runners-pool-<p>`),
   created and reconciled by the server (immediate pass + a 30 s tick,
   idempotent, self-healing). `PublishRun` derives the subject from the
   frozen stamp: a pool run cannot land on the shared stream.
5. **Admission** (the runner): a pool pod attaches the consumer its server
   reconciler created and admits only runs whose message, frozen document
   and pod pool AGREE — a disagreement parks the payload on the run's
   pool's DLQ and flips the document with the named reason. The pod's
   identity is announced at boot; an unstamped message on the shared pod
   stays admitted (the default).
6. **Credentials**: a pool pod carries only its pool's gateway secret — no
   vendor keys are mounted. A mis-routed vendor call fails at
   authentication, not at egress; the network is not the enforcement layer
   (the current shared install enforces this boundary in code). The
   per-run credential bundle seals under a fresh per-run DEK that
   travels in the run's own queue message (ADR-123) — a runner pod
   holds no platform key material at all — and binds to tenant, pool
   and run: a bundle served outside its own identity context refuses
   to decrypt.
7. **Server-side auxiliary surfaces**: the merge-conflict resolver,
   declared supervisors and the session board send content derived from a
   run to a model resolved outside the run's own execution. All three take
   their model registry from one credential channel
   (`pkg/backend/auxllm.Registry` — the server process env today). The
   resolver and the session board resolve through plain `Resolve`, so the
   run's own gateway credential, once #2005 PR-C' reopens the sealed bundle
   server-side, funds them from that one place (#2645); declared
   supervisors resolve through `ResolveWithContext` on the run's ctx, whose
   sealed credentials already fund them where the ctx carries them — the
   PR-C' change must touch that call site too (`pkg/supervise/bot.go`). On a run
   stamped to a pool they consult the run document's frozen stamp and
   refuse with `ErrPoolContentRefused` (HTTP 403 on the resolver route),
   naming the remedy — unless the surface's model is itself
   `openai_compatible/*` (the operator provided the pool's gateway
   credential to the calling process). The refusal holds where the run
   actually executes: the runner pod filters its declared supervisors with
   the message's frozen stamp; the server filters the in-process
   coordinators. An unreadable stamp leaves the surface off. Runs that
   predate the stamp carry none — the resolver refuses them when their
   tenant is CURRENTLY mapped (read fresh through the identity seam); the
   ambiguous case refuses, it never guesses. Unmapping a team never
   declassifies its past runs: the historical stamp is the boundary, not
   the current mapping.
   Operator-local surfaces — `iterion run`, `iterion supervise`, and the
   dispatcher's first-party runs — are unstamped documents on machines the
   operator already trusts with vendor credentials; they are out of the
   pool boundary by construction.
8. **Object storage reads are tenant-mediated**: an IR blob (or bundle
   snapshot) read resolves the run id embedded in its key and loads that
   run under the caller's tenant filter before touching the bucket — a
   foreign tenant's key refuses with the same not-found a missing blob
   would return, and an unattributed caller fails closed. The S3
   credential remains deployment-wide (the accepted residual: the pods
   sit inside the install's network boundary); what the code guarantees
   is that the STORE opens no object for a tenant that does not own
   its run.
9. **The run lease carries the run's admitted identity**: the
   distributed lease a runner takes on a run (`pkg/queue/nats`) is
   written with the tenant — and runner pool, when stamped — of the
   message THAT pod admitted, taken from the delivery context
   (`store.WithLeaseIdentity`). An acquire without an admitted identity
   fails closed (`ErrLeaseUnattributed`): an unattributable lease never
   comes to exist. Before a holder refreshes or releases the lease, it
   re-reads the stored body — on the failure path of the revision CAS,
   never in the nominal path — and refuses on any disagreement
   (`ErrLeaseIdentityMismatch`): a lease rewritten under another
   admission is never extended and never deleted by a holder of a
   different identity; a takeover under the SAME identity (a sibling of
   the same team) is caught by the revision itself. The guard is
   against honest confusion, not an active forger: a pod forging the
   whole protocol is beyond the code's reach (the same accepted
   residual as the rest of the boundary — ADR-123's rest point).
10. **Control-plane commands carry the run's admitted identity**: the
   cancel and steer commands are stamped — in their NATS headers
   (`iterion-admitted-tenant` / `iterion-admitted-pool`, exact
   lower-case spelling; the vendored client's header map is
   case-sensitive) — with the tenant (and pool) from the run
   document's frozen stamp by the server that emits them, and the pod
   holding the run verifies the stamp against the message it admitted
   before acting; a command of another tenant or pool, or with no
   stamp at all, is ignored with an error log. The subjects stay
   shared (`iterion.cancel.<run_id>` / `iterion.steer.<run_id>`): the
   guard lives at the pod, the one place that knows what it actually
   admitted, so a shared subject can deliver a foreign command but
   cannot make another pool's pod act on it. Fleet-mix window: a
   not-yet-upgraded server emits unstamped commands and the upgraded
   pods refuse them — a mid-rollout cancel or steer does not take
   until both sides run this build (deploy server and runners
   together). Headers are broker-visible and operator-writable — a
   guard against misrouting and confusion, not an active forger (the
   boundary's accepted residual).
11. **The tenant-filter opt-outs are audited, by family**: the 62
    call sites of `WithoutTenantFilter`/`TeamBlind` (tests excluded)
    fall into six families, each audited against one question — does
    the site act on an already-admitted run, or enumerate for a user?
    - *System post-execution* (outcome router, schedule outcome, the
      trigger relay): a platform consumer loads ONE run by id to
      decide its follow-up. The run is already admitted; nothing
      enumerates.
    - *Cluster-wide sweeps* (the lifecycle reconcilers, the queue/
      retry/gate sweepers, the reaper, the alert and usernotify
      sweeps): the sweep IS a cluster operation — it discovers its
      own work. A tenant filter would make it blind by construction.
    - *Forge webhooks* (the gate autofix/reconcile/relaunch family):
      the webhook is signature-verified, and the run id it names is
      bound to that repository at launch — a foreign team's run can
      never match another org's webhook. The blind load is by id;
      nothing is searchable across tenants through it.
    - *Already-mediated control* (the cloud publisher's cancel/steer):
      the caller's tenant filter already gated the same read; the
      blind load is the second, explicit one — commented at each
      site.
    - *User-facing reads* (artifacts, issue watchers, fork grants):
      the scoped variants (`ListArtifactsCtx`, …) serve the HTTP
      handlers; the opt-out sites are the explicit LOCAL variants.
    - *Mediation itself* (`GetIRBlob` — P5-b item 8 —, `runTenant`,
      the launch id-uniqueness check): the blind read is how the
      tenant is resolved or the boundary enforced.
    No site was found acting as a cross-tenant enumeration for a
    caller. The families stay auditable by grepping the two helpers;
    a new site outside these shapes owes its own justification in its
    comment.
12. **A team's LLM fallback policy closes the platform fallback at the
    root**: `Team.LLMFallback` (the super-admin route
    `PUT /api/admin/teams/{id}/llm-fallback`, the only writer — the same
    boundary discipline as the pool mapping) names how the credential
    walk falls back for the team's runs. Empty or `platform` keeps the
    shared tiers as always. `none` is the sovereign posture: the walk
    consults only the team's own credentials (its BYOK keys, its OAuth
    forfaits), no shared tier (org, pool, platform) ever seals into the
    team's bundle, and an LLM route nothing of the team's funds REFUSES
    the launch by name — every pinned provider must hold a credential
    of the team's own, and a workflow whose routes cannot be attributed
    still refuses when the team holds NO credential at all (the pod's
    platform env would be the only funding left). Read FRESH at every
    resolution; a store error or an unknown value refuses (fail closed).
    Env-funded runs (every route on the runner's gateway) answer to the
    read's fail-closed gate only; a tool-only run spends nothing.
    Deployment order matters: a publisher that predates the field
    ignores it, so set the policy AFTER the release is everywhere; and
    loosening `none` back to `platform` lets a resumed run re-seal
    shared credentials — audited, and by design.

## Routing dials: the pool's env block is pinned

A pool's models, provider hints, backends and rescue routes are its own env
block (`runnerPools[].env` in the chart). The chart renders every
`ITERION_*` name of that block into `ITERION_BOT_VARS_ENV_PINS` — the union
with a list the block spells itself, for an install that pinned by hand
before the chart rendered it — and the runner reads a pinned name from the
pod env **ahead of** the platform bot-vars record (`PUT
/api/admin/settings/bot-vars`), whose precedence elsewhere is setting > pod
env > .bot default ([ADR-093](adr/093-bot-vars-runtime-settings.md), its
2026-10-22 addendum).

The pin exists because the record is a fleet-wide dial written for the
shared runners: a model id pinned there for the shared fleet's vendor would
otherwise reach a pool pod whose provider hint and rescue model name its
gateway — every route of the chain then crosses a gateway id with a vendor
hint, the dispatch refuses each at build (`refuseGatewayCrossing`), and the
pool runs nothing. A pinned name the pod leaves empty resolves to the .bot
default, not to the record.

**The record still reaches every name the block does not spell.** That is
the point of the pin, and its limit: a platform-wide effort dial tunes a
pool too, and so does a platform-wide model pin on a slot the block forgot
— on a node with no gateway hint the record's vendor id is not refused, it
dispatches as a vendor route, and the pool then leaks only if its pod holds
a vendor credential (it holds none of the platform's; the team's own BYOK
keys travel in the run bundle, see the residuals). So a pool block spells
**every dial the bots it runs read**, not the three the incident named.
For review-pr (its `lib/nodes.bot`): `ITERION_VIBE_MODEL_` and
`ITERION_VIBE_BACKEND_` (`claw`: the one backend that serves a gateway
route) for each of `CLAUDE`, `CLAUDE_GLANCE`, `GPT`, `GPT_GLANCE`, `EMIT`,
`EMIT_GPT`; `ITERION_VIBE_PROVIDER_` (`openai_compatible`) for `CLAUDE`,
`GPT`, `EMIT`, `EMIT_GPT` (a glance node reads its full slot's hint); the
supervisors' `ITERION_VIBE_MODEL_SUPERVISOR` and `_SUPERVISOR_GPT`; and the
rescue dial `ITERION_VIBE_MODEL_CLAUDE_FALLBACK`. Two consequences the
bot's own comments spell out:

- `ITERION_VIBE_BACKEND_EMIT=claw` is required on a pool — the merge step
  defaults to `claude_code`, which cannot serve a gateway model — and on
  claw the merge node has no tools: it returns its merged findings (what
  the publish step consumes) but writes no report file under
  `report_path` and files no board issue.
- The authored rescue routes (`glm`, `provider: zai`) are refused on a pool
  whatever the rescue dial says — a gateway id under a vendor hint — which
  is the design: a pool must fail rather than leak. A pool has no rescue
  route; a gateway outage fails the node.

Operator notes:

- Tune a pool in GitOps (its env block), never through the record — the
  record cannot reach a pinned name, by design.
- The runner says what it pinned at boot (`platformcfg: bot_vars: N name(s)
  pinned to the process environment …`) and warns on a token that is not an
  `ITERION_` name; a pool whose model still reads as the record's value has
  no pin rendered — check the Deployment's env.

## Operations

The super-admin CLI uses the same audited routes as the API:

```sh
iterion remote admin runner-pools list
iterion remote admin runner-pools set --data @pools.json
iterion remote admin teams runner-pool TEAM --pool alpha
iterion remote admin teams runner-pool TEAM --clear
iterion remote admin teams llm-fallback TEAM --policy none
iterion remote admin teams llm-fallback TEAM --clear
iterion remote admin dlq list --pool alpha
iterion remote admin dlq show 42 --pool alpha
iterion remote admin dlq replay 42 --pool alpha
iterion remote admin dlq delete 42 --pool alpha
```

`runner-pools set` replaces the whole registry: read it first and retain the
other pools. Mapping requires exactly one of `--pool` and `--clear`;
fallback requires exactly one of `--policy none|platform` and `--clear`.
Clearing fallback restores the default platform policy. DLQ sequence IDs
are positive integers; omitting `--pool` addresses the shared DLQ.

### Moving a team between pools

Changing or clearing a mapping checks every queued or running run of that
team, including running runs idle between model calls. A known nonempty
answer refuses with HTTP 409, the run IDs and the drain remedy. Missing
store capability or unreadable/incomplete run metadata refuses with 503,
including with `--force`. A no-op mapping does not need this read.

Stop new submissions **and resumes** for the team first. If its old pool is
dedicated, put that registry entry in `draining`, then wait for queued and
running work to finish before changing the mapping. A shared source has no
registry entry to drain: stop the team's ingress and wait; disabling the
destination does not drain the shared source. Parked runs do not execute,
and their later resume still refuses a changed immutable pool stamp.

This read is a cutover backstop, not a distributed launch lock: a new launch
after the read can race the mapping write. `--force` deliberately bypasses a
known nonempty answer (`?force=1`), and both audit scopes retain `forced`
and `alive_runs`. It never invents an empty answer after a failed read.

### Registry host projection

An entry may record `gateway_base_url` and `forge_hosts` alongside its
lifecycle. The gateway is an HTTP(S) URL with a host and valid port, without
credentials, query or fragment. Forge entries are bare hosts with optional
ports. Validation makes no DNS request and preserves the operator's literal
values. Private, loopback and cluster-local destinations remain accepted;
GET/PUT responses derive `C213` warnings rather than persisting a second copy.

At pool-runner boot, the registry gateway host and effective port are compared
with `OPENAI_COMPATIBLE_BASE_URL`. Case, a trailing DNS dot and default ports
do not create false mismatches. A missing/unreadable projection or mismatch
emits one warning and boot continues. This projection neither rewrites the
pod gateway nor supplies execution credentials or a run content boundary.

### DLQ and boundary readback

`iterion_dlq_depth` retains the total shared-plus-pools depth.
`iterion_dlq_depth_by_pool{pool="alpha"}` exposes each enabled registry pool,
including its real zero; it does not add a shared-pool label. A complete
sample removes retired labels. Any failed read retains the previous complete
sample for both metrics rather than publishing a partial total. The pool-labelled
family is published by one atomic swap of an immutable sample. A collection
that overlaps a refresh retains its complete previous sample, including the
values Prometheus reads after collection; it cannot observe the reset between
polls or a partially populated set of pool labels.

The rendered-pod witness checks pool containers, init containers and their
referenced configuration for platform sealing-key names and test key
material. JWT and authenticated Mongo key references are explicitly allowed,
as is the pool's own gateway credential. The operator-managed storage Secret
is an opaque approved input; a render cannot inspect its deployed contents
or establish that namespace RBAC prevents a pod from reading other Secrets.

The pool content refusal is typed (`runview.ErrPoolContentRefused`): pin the
surface's model to the pool's `openai_compatible/*` gateway and provide that
gateway credential to the process, or run the surface on the pool. The orphan
sweeper includes pool consumers' backlogs; an unreadable backlog defers its
verdict instead of failing queued work.

## Deliberate residuals

- A pool deleted from the registry leaves its streams and consumers
  behind — reclamation is an operator action (no auto-teardown of a
  boundary's topology).
- `providers_served` is recorded and read by nothing: the credential walk
  answers "is this route env-funded?" from the route's own provider, never
  from the registry entry. A guard reports every reader of the field in the
  module's own production Go, so one cannot land one without saying what it
  does; a test fixture, a bson projection on the column or a studio client
  reading the JSON field is outside its reach.
