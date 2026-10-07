# Sovereign runner pools

A team whose code must never reach a public LLM vendor runs on a **sovereign
runner pool**: a dedicated set of runner pods whose only LLM credential is the
team's own OpenAI-compatible gateway, and whose runs are isolated from the
other tenants by code — routing, admission and credentials, not network
fencing (the deployment stays the shared iterion.cloud install; there is no
dedicated namespace and no egress filtering).

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
   (operator decision: the shared install, isolation by code). The
   per-run credential bundle seals under a fresh per-run DEK that
   travels in the run's own queue message (ADR-123) — a runner pod
   holds no platform key material at all — and binds to tenant, pool
   and run: a bundle served outside its own identity context refuses
   to decrypt.
7. **Server-side auxiliary surfaces**: the merge-conflict resolver,
   declared supervisors and the session board send content derived from a
   run to a model resolved outside the run's own execution. On a run
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

## Operations

- `GET|PUT /api/admin/runner-pools` — the registry (super-admin; wholesale
  replace, validated: pool name grammar, duplicate names, lifecycle
  states; audited with the previous state).
- `PUT /api/admin/teams/{id}/runner-pool` — the mapping (super-admin;
  refuses pools the registry does not know).
- `GET /api/admin/dlq?pool=<p>` and the peek/replay/discard routes take the
  same `?pool=` parameter — each pool parks on its own DLQ stream, and the
  depth gauge sums the shared DLQ with every registry-known pool's.
- The pool refusal is typed (`runview.ErrPoolContentRefused`): pin the
  surface's model to the pool's `openai_compatible/*` gateway and provide
  that gateway credential to the process, or run the surface on the pool.
- The orphan sweeper's queued-pass skip counts pool consumers' backlogs;
  an unreadable pool backlog DEFERS the orphan verdict rather than
  flipping a run that is waiting its turn.

## Deliberate residuals

- A pool deleted from the registry leaves its streams and consumers
  behind — reclamation is an operator action (no auto-teardown of a
  boundary's topology).
- `providers_served` is recorded but consumed only by the credential
  planning slice (P4).
- The pool DLQ API surface (list/peek/replay/discard per pool) is wired;
  the per-pool labeled depth gauge and the `admin dlq` CLI pool flag are
  follow-ups.
