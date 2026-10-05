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
   state — never routed onto the shared pool.
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
   (operator decision: the shared install, isolation by code).
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
