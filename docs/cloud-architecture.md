# Cloud architecture

**Audience.** Anyone who needs the mental model behind the
multi-tenant platform — to debug a stuck run, design an integration,
or convince a security review that "yes, tenancy is enforced at the
store layer, not in the UI". Every component below has a real file in
[pkg/](../pkg/); the link beside each piece points there.

This page supersedes the old [cloud.md](cloud.md) (now a 20-line front
door) and complements [cloud-deployment.md](cloud-deployment.md) (the
operator runbook).

Diagram: [diagrams/README.md](diagrams/README.md) — the data-model pages and the access-control matrix.

## Control plane vs data plane

```mermaid
flowchart LR
  subgraph "Control plane"
    SRV["iterion server<br/>REST + WS<br/>(pkg/server)"]
    MONGO[("MongoDB<br/>runs / events / identity<br/>secrets / audit<br/>pkg/store/mongo")]
  end

  subgraph "Data plane"
    NATS[("NATS JetStream<br/>iterion.queue.runs<br/>iterion.queue.runs.dlq<br/>KV iterion-run-locks<br/>pkg/queue/nats")]
    RUN["iterion runner pool<br/>KEDA-scaled<br/>(pkg/runner)"]
    SBX["Per-run sandbox pod<br/>(pkg/sandbox<br/>kubernetes driver)"]
    S3[("S3-compatible blob<br/>artifacts<br/>pkg/store")]
  end

  CLIENT[Client<br/>studio / SDK / curl / PAT] -- "REST + WS<br/>(JWT / PAT / cookie)" --> SRV
  FORGE[Forge<br/>GitLab / GitHub<br/>Forgejo / Gitea] -- "POST /api/webhooks/&lt;provider&gt;/&lt;id&gt;<br/>iwh_ token / HMAC" --> SRV

  SRV -- "publish RunMessage<br/>+ SecretsRef" --> NATS
  SRV -- "persist / read" --> MONGO
  SRV -- "change-stream<br/>(events)" --> MONGO

  NATS -- "pull (durable<br/>iterion-runners,<br/>MaxAckPending=256)" --> RUN
  RUN -- "claim KV lease<br/>+ heartbeat (~20s)" --> NATS
  RUN -- "execute" --> SBX
  RUN -- "write events / status" --> MONGO
  RUN -- "write artifact bodies" --> S3
```

The two halves are kept deliberately separate:

- The **control plane** (server + Mongo) is the source of truth for
  identity, multi-tenancy, secrets, audit, and run metadata.
- The **data plane** (NATS + runner pods + sandbox pods + S3) does the
  expensive work — LLM calls, tool execution, file mutation.

A failure in the data plane (a runner OOMs, NATS reboots, S3 is slow)
must not lose the run; the control plane keeps the canonical state and
the orphan sweeper closes the gap when the runner dies between claiming
a run and writing its terminal status (see
[Iterion Cloud admin guide → DLQ + orphan
sweeper](cloud-admin-guide.md#17-dlq-triage)).

## The run lifecycle

| Status | Meaning |
|---|---|
| `queued` | Waiting to start. In cloud mode the run document in Mongo's `runs` collection reads `queued` until something settles it — the engine starting on the runner that claimed it, or a launch rollback or a cancel, for example; locally, the pipeline-concurrency cap parked the launch. |
| `running` | A runner pod has claimed the KV lease, opened the bundle, and is executing nodes. Heartbeats refresh the lease ~every 20s. |
| `paused_waiting_human` | A human node awaits input (`POST /api/runs/{id}/resume` with answers). Resumable. |
| `paused_operator` | Operator paused via the studio. Resumable. |
| `failed_resumable` | Transient failure (LLM rate limit, timeout, runner crash) — the runner Naks and JetStream redelivers for an auto-retry — **or** budget exceeded, which the runner Acks (no auto-redelivery: the same message carries the same spent budget, so it would re-fail instantly). Either way the checkpoint is preserved; `iterion resume` / studio Retry brings it back, and for budget exceeded you raise the cap first (`iterion resume --max-cost-usd/--max-duration …`). |
| `failed` | Definitive — `FailNode` reached, or first node failed before any checkpoint existed. |
| `cancelled` | Cancelled **by the operator**. Checkpoint preserved but terminal: a redelivered launch is dropped, so continuing needs an explicit resume (this deliberate anti-resurrection guard is why a runner-shutdown interruption is promoted to `failed_resumable` instead — see "Rolling deploys" below). |
| `finished` | Terminal success. |

Statuses are pinned in
[pkg/store/run.go:RunStatus](../pkg/store/run.go).

## Sealed credentials bundle lifecycle

Each cloud run carries its credentials through the queue in a sealed
envelope so the runner pod gets exactly what it needs and nothing else
([pkg/secrets/run_secrets.go](../pkg/secrets/run_secrets.go)):

```
   server (publisher)                              runner pod
   ──────────────────                              ──────────
   1. walk the credential tiers: BYOK,             5. fetch RunSecretsRecord by ref
      generic secrets, OAuth, org, pool,              from Mongo
      platform, restore, then the
      fallback door
   2. assemble RunBundle{                          6. build the AES-GCM sealer from
        APIKeys, GenericSecrets,                      RunMessage.BundleDEK, then
        GenericSecretHosts, OAuthCredentials, ...     OpenRunBundle under the same AAD
      }
   3. NewRunBundleDEK → a fresh 32-byte            7. inject into engine ctx
      per-run key, then SealRunBundle →            8. on success or a pause for
      sealed_bundle (AES-GCM, key_id "dek",           human input: Delete(ref)
      AAD run_secrets:<tenant_id>/<pool>/<run_id>)
   4. write RunSecretsRecord{                      Mongo TTL: 24h on the record so
        _id = NewSecretsRef(),                      a bundle the runner keeps (a failed,
        key_id = "dek", sealed_bundle,              cancelled or operator-paused run's,
        expires_at = now+24h, ...                   for example) never lingers (Resume
      }                                             re-publishes and re-resolves).
      then publish RunMessage{ ..., SecretsRef,
      BundleDEK }
```

The sealed payload never reaches NATS — the queue carries the ref and
the run's own key, the ciphertext stays in Mongo, and opening a current
bundle needs no platform key on the pod. The AAD binds the bundle to
its full identity (`run_secrets:<tenant_id>/<pool>/<run_id>`), so a
record served for another tenant, pool or run cannot open. A `dek`-stamped record whose
message carries no `BundleDEK` is refused with
`ErrBundleDEKMissing`, and a key that does not authenticate fails at
`OpenRunBundle` with `secrets: authentication failed`; either way the
runner Nak's and the message redelivers. The runner's
`ITERION_SECRETS_KEY` ring opens only the two older cohorts — a record
with no key id (sealed under the run-only AAD) or one stamped with a
ring key id — which the 24h bundle TTL retires.

## Queue internals

Conventions are pinned in
[pkg/queue/nats/nats.go](../pkg/queue/nats/nats.go) — every constant
matches plan §C.2:

| Resource | Default name | Purpose |
|---|---|---|
| Stream | `ITERION_RUNS` | Live runs queue |
| DLQ stream | `ITERION_RUNS_DLQ` | Parked messages (max-deliver-exhausted) |
| Subject | `iterion.queue.runs` | Where the publisher writes |
| DLQ subject | `iterion.queue.runs.dlq` | DLQ park subject |
| KV bucket | `iterion-run-locks` | Distributed lease per run id |
| Durable consumer | `iterion-runners` | The pull-consumer the runner pool drains |

Pinned semantics:

- **`MaxAckPending = 256`** on the consumer — a fleet-wide ceiling on
  in-flight (delivered-unacked) runs across the shared durable consumer,
  not a per-pod cap. It MUST stay ≥ the max runner-pod count KEDA scales
  to (the historic value of `1` pinned the whole fleet to a single
  concurrent run). Per-pod serialization comes instead from each runner
  holding one in-flight run via its serial loop. Horizontal scale is
  "more pods" via KEDA.
- **`AckWait = 10min`** with periodic `InProgress()` heartbeats so a
  long LLM step doesn't trigger redelivery while it's still healthy.
- **KV lease**: TTL 60s, refreshed every 20s by the runner's
  `heartbeat` goroutine
  ([pkg/runner/loop_lock.go](../pkg/runner/loop_lock.go)) — through the
  engine's teardown and the runner's post-engine steps, within the runner's
  lease ceiling once the run can be resumed. If a single lease
  refresh fails, the runner self-cancels its own run to avoid split-brain
  (`iterion_runner_heartbeat_errors_total` bumps).
- **`MaxDeliver = 8`** — the eighth NAK parks a copy on the DLQ stream
  (header `Iterion-DLQ-Reason: <err>`) and the runner CAS-flips the
  run to `failed_resumable`
  ([pkg/runner/loop.go](../pkg/runner/loop.go), look for "parking on
  DLQ"). The original NATS message is Term'd; the DLQ copy is the
  recoverable artifact.
- **Delayed re-offers.** Two outcomes hand the message back with a
  delay instead of at once, so the redelivery budget is not burnt inside
  a condition that needs wall-clock time to clear: a run whose sandbox
  setup phase timed out (`SANDBOX_SETUP_TIMEOUT`, a stuck workspace copy)
  is re-offered to a fresh pod after 2 minutes, and the run's timeline
  carries a `run_redelivery_deferred` event naming the reason, the delay
  and the attempt's rank — the DLQ park still applies on the last
  delivery; a `running` doc written more recently than the runner's
  adoption floor (a lapsed-but-alive pod may still be unwinding) is
  re-offered after the floor's remainder. On the LAST permitted delivery
  that young doc is Term'd instead and the log says so — JetStream would
  not re-offer it anyway — and the orphan sweeper below owns it from
  there; nothing is written over a possibly-live writer.
- **DLQ retention**: 7 days
  ([pkg/queue/nats/nats.go:DefaultDLQMaxAge](../pkg/queue/nats/nats.go)).
  An operator triages via the admin endpoints
  ([Iterion Cloud admin guide
  §1.7](cloud-admin-guide.md#17-dlq-triage)).

The **orphan sweeper** runs on the server side
([pkg/server/queue_sweeper.go](../pkg/server/queue_sweeper.go)) and
catches the failure mode the runner can't — the pod that died before
even claiming the run, or before its first status write. It scans
every 60s for `queued` past the redelivery window + margin (~2h40 with
the defaults: the window described below + 10min) or `running > 10min` AND no
current NATS-KV lease, then CAS-flips matched rows to `failed_resumable`.
Bumps `iterion_runs_orphan_recovered_total`.

The same sweeper also polls `DLQDepth()` so
`iterion_dlq_depth` is kept fresh — that's what the
`IterionDLQNotEmpty` alert in the starter pack fires on.

A delivery that cannot take the run lock is retried after one lease interval
— the configured `ITERION_LOCK_TTL` / `runner.lock_ttl`, 60 seconds by
default — which is how long a lease nobody refreshes takes to evaporate, so
the retry either finds the run free or meets a live owner. One that finds the
lease held by another runner backs off towards the longest a lease is held
once its run can be resumed: the engine's teardown, cancelled or parked, then
the runner's post-engine steps — the git snapshot, the bank, the artifact
upload — within the runner's lease ceiling (about 56 minutes, most of it the
bank's own budget), then the lease's lapse. Its retries double from one lease
interval to a last one that waits that whole hold (1 min, 1 min 48, 3 min 35
… 57 min with the defaults), so a resume delivered while the previous
execution still unwinds outlives it, whichever delivery first met the lock —
unless no retry is left. The queue's worst-case redelivery window counts each
of the `MaxDeliver` deliveries at the longest delay its rank can take —
`AckWait`, the schema and epoch delays, the lease TTL, or that back-off —
about 2 h 30 with the defaults, and the orphan sweeper's queued-staleness
cutoff tracks it. On its last allowed attempt the original message is archived on
the DLQ and `run_delivery_exhausted` lands on the run's timeline. Either way
the run itself is untouched — without the lock no writer may change its
outcome or its continuation — so this records a *delivery* failure, never an
execution one.

Two different failures reach that path, and the DLQ reason names which:

- **`run lock held by another runner`** — confirmed contention
  (`jetstream.ErrKeyExists` on the lease). A sibling owns the run; inspect
  that owner's outcome, and discarding the duplicate is safe once it
  completed.
- **`run lock acquisition failed`** — every other lock error (KV bucket
  missing, a broker blip on the create). Ownership **could not be
  confirmed**, which is not the same as no owner: a sibling may hold the
  lease and its collision simply never got reported. Replay only after
  checking the run itself *and* a healthy lock service — a blind replay
  duplicates a live run. A run left `queued`/`running` with no lease is the
  orphan sweeper's, above.

`parked: false` on the event means the archive was **not acknowledged**, not
that no copy exists: the publish waits for a JetStream ack, so a lost ack can
hide a copy that landed. Read the DLQ before either replaying (which may
duplicate the run) or discarding (which may destroy its last copy) — neither
is safe blind.

## Multitenancy enforcement layers

Four boundaries:

1. **HTTP middleware** validates the credential and stamps an
   `auth.Identity{UserID, Email, OrgID, OrgRole, TeamID, Role,
   IsSuperAdmin, Kind, JTI, Via}` on the ctx — `OrgRole` is the org tier
   the claim carries, and `Kind` marks a principal nobody signed in
   as: `webhook` / `share` (synthetic, which the operator RBAC gates
   refuse) or `internal` (a daemon of this process, carrying no role to
   judge). JWT, PAT and the single-use WS ticket converge here; the
   self-authenticating surfaces are exempt from this gate — among them
   inbound webhooks and config-share editor links, which mint their own
   identity, and the per-run `X-Iterion-Run` tokens of the board- and
   runs-MCP routes and of the forge-publish routes, which mint none
   ([pkg/server/middleware.go](../pkg/server/middleware.go),
   [pkg/server/middleware_webhook.go](../pkg/server/middleware_webhook.go),
   [pkg/server/middleware_configshare.go](../pkg/server/middleware_configshare.go)).
2. **Route guards**: among them `canViewTeam` / `canManageTeam` /
   `canViewOrg` / `canManageOrg` / `canEditConfigShares` / `canEditBots`,
   which refuse a synthetic principal, pass a super-admin, and otherwise
   cross-check the URL's `{id}` against the principal's team and org
   memberships — as stored, and for the view and config-editor gates
   also as its credential carries them
   ([pkg/server/auth_authz.go](../pkg/server/auth_authz.go));
   `requireSuperAdmin` fronts the admin routes, and a record's own
   mutation right goes through `canMutateScopedRecord` behind
   `canMutateApiKey` / `canMutateGenericSecret` — these are
   [diagrams/access-control.md](diagrams/access-control.md)'s decision points.
3. **Store ctx**: `store.WithIdentity(ctx, tenantID, userID)` is
   re-stamped onto the ctx so the Mongo adapters that read it add
   `tenant_id = ...` to their filters.
4. **Mongo adapter**: `runs`, `events`, `api_keys`, `generic_secrets`,
   `bot_secret_bindings`, `audit_events`, `webhook_configs`,
   `webhook_deliveries`, `memory_spaces` and `memory_docs` carry a
   `tenant_id` field that some rows omit — for example a `global`
   memory space's documents, a platform audit row, or an event
   appended under a tenant-less context. In `pkg/store/mongo`, `withTenantFilter` panics
   on a ctx without a tenant unless the caller opts out through
   `store.WithoutTenantFilter`, and the API-key, binding and
   generic-secret stores' filters return `ErrApiKeyTenantMissing`,
   `ErrBindingTenantMissing` and friends: they
   **fail closed**, not "show everything". The audit,
   webhook and memory stores read no tenant from the ctx; where they
   scope by tenant, the caller passes it.
   A compound index starting with `tenant_id` backs
   the per-tenant queries, so it exists where the reads are per-tenant
   and not where a row is reached by another key — `api_keys` and
   `generic_secrets` index `scope_team` first, a run's children (`interactions`,
   `user_messages`, `run_gitmeta`, `run_tags`) and `memory_docs` index
   from their parent's id instead, and `org_usage` keys its
   (subject, month) in `_id` with no `tenant_id` at all.
   The credential lookups are the deliberate exception: for example
   `password_resets` and `personal_access_tokens` hold no `tenant_id`
   and are read by token hash (a PAT also by its `_id` and its owner's
   `user_id`) — resolving the bearer is what
   yields the identity a tenant could be read from — as are the forge
   stores' reads by `_id`, where
   the callback resolves the tenant from signed state and the HTTP layer
   asserts tenant ownership before mutating.

One deliberate cross-tenant case is `visibility=global` memory: the
FS adapter / Mongo adapter both treat it as untenanted.

## Where each metric is emitted

| Metric | Emitter | When |
|---|---|---|
| `iterion_runs_created_total{status}` | server | At every Launch/Resume publish |
| `iterion_runs_active{status="running"}` | runner | Sum across pods = in-flight runs |
| `iterion_run_duration_seconds{status}` | runner | On terminal status |
| `iterion_ws_connections` | server | WS open / close |
| `iterion_mongo_change_stream_lag_seconds` | server | Per event delivered |
| `iterion_nats_pending_messages` | runner | Polled every 15s |
| `iterion_workspace_clone_duration_seconds` | runner | Time spent in `prepareRepoWorkspace` (clone + checkout) before engine start, observed once per repo-bound run |
| `iterion_llm_tokens_total{backend,model,direction}` | runner | Per LLM call |
| `iterion_llm_cost_usd_total{backend,model}` | runner | Per claw-priced call (delegate calls don't carry a price table) |
| `iterion_runner_heartbeat_errors_total` | runner | Per KV refresh failure |
| `iterion_webhook_deliveries_total{provider,status}` | server | Per delivery (terminal status) |
| `iterion_webhook_throttled_total{provider,reason}` | server | Pre-handler throttle (rate / quota) |
| `iterion_auth_logins_total{result}` | server | Login attempts |
| `iterion_auth_password_resets_total{step}` | server | Reset flow (`requested`, `confirmed`) |
| `iterion_launch_denied_total{reason}` | server | Launch gate refusals |
| `iterion_runs_orphan_recovered_total` | server | Sweeper flips |
| `iterion_dlq_depth` | server | Sweeper poll of NATS state |

All from a shared registry in
[pkg/cloud/metrics/metrics.go](../pkg/cloud/metrics/metrics.go) so the
PodMonitor scrapes both binaries with identical metric names.
Tenant labels are deliberately absent on every counter (cardinality
discipline; per-org accounting lives in Mongo).

## Run-completion callbacks vs inbound webhooks

The two webhook directions are intentionally separate features:

- **Inbound** (this whole doc): the forge / CI / custom caller fires an
  iterion run. See [webhooks.md](webhooks.md).
- **Outbound** (run-completion callbacks): the run, when it terminates,
  POSTs back to a URL the launcher supplied. See
  [outbound-callbacks.md](outbound-callbacks.md). Used to deliver the
  final answer to a chat bot without polling, with optional HMAC
  signing via `ITERION_COMPLETION_WEBHOOK_SECRET`.

The runner emits the callback after every terminal status transition,
firing once. Webhook URLs are SSRF-vetted (loopback / RFC-1918 /
metadata-IP refused by default — see the outbound doc for the
self-hosted relaxation).
