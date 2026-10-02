# ADR-117 — A server-side leader lease for idempotent nets

- Status: accepted
- Date: 2026-09-30
- Deciders: jo (direction), Claude (analysis)
- Relates to: [070-kubernetes-sandbox-orphan-gc.md](070-kubernetes-sandbox-orphan-gc.md)
  (a server-side sweep "would need its own lease"); [096-board-claim-lease-and-watchdog.md](096-board-claim-lease-and-watchdog.md)
  (a fenced per-card lease — a different guarantee); [097-github-projects-v2-board-sync.md](097-github-projects-v2-board-sync.md)
  (a per-tenant watermark lease, not reusable); issue #2002

## Context

The server's background nets run on every replica, and the house doctrine
makes that safe: each item they touch is claimed (a CAS, a leased row), or the
work re-reads before it writes. Safe is not cheap. The merge-gate sweeper
(`pkg/server/forge_gate_sweeper.go`) re-offers every gating run of the last
hour once a minute, and every gating run of the last 192 hours every thirty
passes; each offer costs a forge read (`GET pull`, often `GET status`) against
the GitHub App installation's hourly REST budget. It had no claim at all — its
safety came from the reconciler's live re-read — so its cost scaled with the
replica count.

Measured on 2026-09-30 (#1995): with the server HPA at its maximum of ten
replicas, the modelled sweep demand was 11.7k–19.4k requests an hour against a
budget of about 11.4k–12.5k. The budget ran out 31–43 minutes after every
hourly reset from 09Z to 14Z. Twelve pull requests lost their `revi/review`
verdict to the rate limit, 23 synthetic "review died" failures followed, and 17
paid review relaunches.

Nothing existing elected one replica for a periodic loop:

- `BoardSyncWorker.ClaimSync` elects per pass, but is hard-wired to the
  per-tenant `board_bindings` rows.
- The NATS-KV run lock is out of the server's reach (`QueueBackend` exposes no
  KV), its TTL is the whole bucket's, and it has no in-memory twin.
- Valkey is optional and has no lock primitive in this code base.

## Decision

1. **A generic seam, `pkg/lease`.** A named lease one owner holds until an
   expiry: `Store.Acquire(name, owner, now, ttl)` takes it when free, expired,
   or already the caller's; `Store.Renew` only extends the caller's own lease
   and never creates one, so a renewal that lands after its holder released
   cannot resurrect the lease; `Store.Release` gives it up, refusing with
   `ErrLost` when it moved on. Two twins, one conformance suite: `MongoStore`
   (collection `leases`, one document per name, a conditional upsert whose
   duplicate-key refusal *is* "held by another") and `MemoryStore`. The clock
   is the caller's, passed explicitly as `ClaimSync` does, and both twins keep
   it to the millisecond, as BSON does.
2. **`lease.Run` serves a term.** It campaigns every `Retry` (each attempt
   bounded by TTL/3, never by `Retry`), runs the work while the lease is held,
   renews every TTL/3, and **steps down fail-closed**: the term ends when a
   renewal is refused, or when no renewal it can prove has succeeded within
   TTL − TTL/6 of being sent — before the store could hand the lease to a
   successor. When the work returns, the lease is released on a detached
   context, so a rollout hands over within one retry instead of a TTL. A store
   that cannot answer elects nobody: the work waits. An answer can be lost
   after its write landed, so an unanswered attempt is followed by a release;
   the write landing after that release is the one residual case, and it keeps
   the lease one TTL — the bound of a crash.
3. **The lease is sticky.** The holder keeps its term for as long as it is
   healthy, so the elected replica keeps its in-memory state (the sweeper's
   deep cursor and pass counter) across passes, and each term opens as a fresh
   start would (the sweeper's first pass of a term is deep).
4. **For load, not for exclusion.** Clocks, a stalled holder and a store
   round-trip bound how exclusive a lease is: a holder that stalls past its TTL
   overlaps its successor for the length of the stall. So a lease elects nets
   that are **already idempotent** and whose *cost* scales with replicas. Work
   that must never run twice keeps a claim on the item itself (ADR-094's
   outbox, ADR-096's fenced card lease), which this does not replace.
5. **The merge-gate sweeper is its first user**: lease `merge-gate-sweeper`,
   TTL three sweep intervals, retry one interval. Lease names live in
   `pkg/server/leases.go`, so two nets never share one by accident. The owner
   is the host name plus a random suffix.
6. **Its cost is measured, not modelled.** The forge HTTP client's transport
   counts every request it sends — attempts as the transport sees them, each
   redirect hop and each answer whatever its status — by host, API
   (REST/GraphQL) and lane. Each replica logs one line per hour it sent
   requests in, `forge HTTP: N requests in the hour ending
   2026-09-30T15:00Z — …`, when its next request arrives, and flushes the hour
   it was counting when it stops; a partial hour says so. The sweep's requests are
   charged to the `merge-gate-sweeper` lane.

## Consequences

- The sweep's forge cost is one replica's, whatever the HPA does: ÷10 at the
  current maximum, with no change to what the gate decides.
- Takeover delay is bounded: at most one retry interval after a clean stop;
  after a crash, the TTL plus one retry — four minutes for the sweeper — and
  its first pass one tick later. The net is idempotent, so a gap only delays a
  repair.
- Every other offer path is unchanged: the event path still runs on whichever
  replica its queue group picks, so the guards written for racing offers (the
  UUIDv5 escalation card, `relaunchStillRunning`) stay.
- The next net to elect costs one `lease.Run` call and one name — the forge
  board issue sync (`board_forge.go`), which also spends the installation's
  REST budget on every replica, is the obvious second.
- A deployment without Mongo runs one process, and the in-memory store elects
  it.

## Alternatives considered

- **Shard by `hash(run) mod N`.** Needs a stable replica index and count, which
  an HPA does not give; a rescale reshuffles every shard mid-pass.
- **Extend `ClaimSync`.** Its lease is fused with the per-tenant watermark; a
  singleton would need a fake tenant row and inherit a schema it does not use.
- **NATS-KV lease.** The doctrine's first example, but the server has no KV
  handle and the bucket's TTL is shared with the run locks; a new bucket means
  schema work on two connect paths, and still no memory twin for tests.
- **Leave the sweep unelected, and only forget settled runs.** That cuts reads
  per offer (a separate change, #2002's second part), but leaves the cost
  proportional to the replica count, and the reset-instant races between
  replicas in place.
