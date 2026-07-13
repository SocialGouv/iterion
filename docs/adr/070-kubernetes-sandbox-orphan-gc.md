# ADR-070 — Garbage-collect orphaned kubernetes sandbox pods and their plaintext-credential Secrets

- Status: accepted
- Date: 2026-07-12
- Deciders: jo (direction), Claude (analysis + implementation)

## Context

The kubernetes sandbox driver (`pkg/sandbox/kubernetes`) cleaned up its
per-run resources — the sandbox pod, the egress-CA Secret, the
**file-secrets Secret holding materialised plaintext BYOK/forge
credentials**, and the NetworkPolicy — **only** on a graceful `Run.Cleanup`,
which fires only when the engine exits normally. A runner pod that is
SIGKILLed, OOM-killed, or node-evicted mid-run never runs `Cleanup`, so:

- the pod, its two Secrets, and the NetworkPolicy **leak indefinitely**;
- the Secret holding plaintext credentials had no TTL and no owner — a
  **credential-at-rest leak**, not merely a resource leak.

There was no kubernetes orphan reaper (only the docker driver's
`ReapOrphanContainers`), and the manifests carried the `iterion.io/managed`
labels but **no `ownerReference`, no `activeDeadlineSeconds`, no
`ttlSecondsAfterFinished`**, so the cluster itself would never GC them.
The old driver comment openly deferred a GC CronJob to "V2".

### Alternatives considered

1. **A cluster-side GC CronJob** (the original "V2" plan). Rejected as the
   primary mechanism: it is an out-of-tree Helm artifact with its own RBAC,
   invisible to the Go test suite, and adds an operational moving part that
   drifts from the driver that creates the resources. Kept as a possible
   future belt-and-braces, not the shipped fix.

2. **Own the Secrets/NetworkPolicy by the *sandbox pod*** (the literal
   reading of "make the Secrets ownerReference the pod"). This gives the
   cleanest cascade — delete the pod, the cluster deletes its Secrets — but
   the sandbox pod *mounts* those Secrets, so they must exist *before* the
   pod is Ready, while an ownerReference needs the pod's UID, which only
   exists *after* the pod is created. Resolving the chicken-and-egg means
   either (a) creating the pod first and letting kubelet retry the volume
   mount with exponential backoff until the Secrets appear (tens of seconds
   of added start latency), or (b) re-applying the Secrets with the UID
   patched in afterwards (extra `secrets/patch` RBAC we don't want to
   assume). Both add risk to the hot start path for a cascade the label
   reaper already provides.

3. **`ttlSecondsAfterFinished` via a Job wrapper.** Rejected: it would
   restructure the pod-as-`sleep infinity` exec target into a Job, a large
   change to the exec/populate/postCreate lifecycle for a TTL that only
   applies once the pod *finishes* — which a `sleep infinity` pod never does
   on its own without `activeDeadlineSeconds` anyway.

4. **Defense in depth: self-terminating manifests + a label reaper, both
   owned by the *runner* pod.** Chosen.

## Decision

Three cooperating mechanisms, none of which depends on `Run.Cleanup`
firing:

1. **`spec.activeDeadlineSeconds` on the sandbox pod**, derived from the
   run's budgeted `max_duration` + a 30-minute margin (`RunInfo.MaxDurationSeconds`
   → `activeDeadlineFor`). A pod leaked by a killed runner self-fails once
   it exceeds the deadline, so it stops consuming compute deterministically
   instead of idling on `sleep infinity` forever. Runs with no duration
   budget get no deadline (0 = unbounded) — we never invent a cap the
   operator did not ask for; the reaper is the backstop there.

2. **An `ownerReference` on *every* per-run resource (pod, both Secrets,
   NetworkPolicy) pointing at the *runner* pod**, read best-effort from the
   downward-API env vars `ITERION_RUNNER_POD_NAME` / `ITERION_RUNNER_POD_UID`
   (the Helm chart wires them; nil when absent). When a runner pod is
   removed — deployment rollout, node drain, scale-down — the cluster
   cascade-GCs its entire sandbox footprint, including the
   plaintext-credential Secret, with no reaper round-trip.

   We deliberately own the resources by the **runner** pod, not the sandbox
   pod (alternative 2). This trades the sandbox-pod cascade (which would let
   deleting the pod delete its Secrets) for no added start latency and no
   extra RBAC. The consequence — the label reaper must delete all three
   kinds explicitly, since deleting the sandbox pod does *not* cascade
   runner-owned Secrets — is accepted and encoded in `reapKinds`.

3. **A labelled-resource reaper** (`ReapOrphanResources`) — the kubernetes
   counterpart to docker's `ReapOrphanContainers`. It lists managed pods,
   Secrets and NetworkPolicies (`iterion.io/managed=true`) and force-deletes
   those whose owning run an `isTerminal` predicate marks terminal or absent
   from the store. Wired into the runview service at boot and on the
   periodic reconcile tick, **gated on `store.Capabilities().CrossProcessLock`**
   and reusing the liveness-first `sandboxContainerReapable` predicate — so a
   lock-less store can never reap an in-flight run's sandbox.

   **Two homes for this reaper:**
   - **Self-hosted filesystem-store-in-k8s** — wired into the `runview.Service`
     at boot and on the periodic reconcile tick, gated on
     `store.Capabilities().CrossProcessLock` (the filesystem flock is the
     liveness authority) and reusing the `sandboxContainerReapable` predicate.
   - **Managed cloud** — wired into the **runner** claim-loop
     ([pkg/runner/reaper.go](../../pkg/runner/reaper.go)), boot + a ticker over
     the runner loop's lifetime. The managed-cloud runview server runs on the
     lock-less Mongo store (`CrossProcessLock == false`), so its gate skips the
     reaper, and the cloud runner never constructs a `runview.Service` — so the
     runview reaper never fires in cloud. The runner IS in-cluster and DOES have
     liveness authority via its **NATS KV lease**, so its reap predicate
     (`sandboxResourceReapable`) is liveness-first on `IsRunLocked` — the exact
     signal the queue sweeper already trusts — with the store status as a
     backstop (reap only when the lease is absent AND the run is terminal or
     gone). A healthy **sibling** runner thus reaps a dead runner's orphaned
     sandbox on the next tick. This closes the OOM-with-surviving-pod window the
     `ownerReference` cascade misses: the cascade fires only on runner-pod
     **deletion** (rollout, drain, scale-down), NOT on an in-place container
     OOM/SIGKILL restart (the pod UID survives, so nothing cascades).

   The runner reaper is off when not in-cluster (`kubernetes.Detect` fails) and
   when the runner has no NATS connection (the lease is the authority). Reap
   cadence is `ITERION_SANDBOX_REAP_INTERVAL` (default 60s; `0` keeps only the
   boot scan).

The docker driver path is untouched.

## Consequences

- A killed runner's sandbox pod + both Secrets + NetworkPolicy are GC'd by
  the cluster on runner-pod deletion (ownerReference cascade), and — in every
  topology — by a reaper's boot/periodic sweep once the run is terminal (the
  `runview.Service` reaper self-hosted, the runner reaper in managed cloud).
  `activeDeadlineSeconds` bounds the pod's compute consumption in the meantime
  (it fails the pod, but does not delete the pod or its Secrets — that is the
  cascade's / reaper's job).
- **Managed-cloud OOM-with-surviving-pod is now closed** (was tracked as a
  residual): the runner reaper fires on a container OOM/SIGKILL where the
  runner *pod* survives — a sibling runner deletes the orphaned sandbox pod +
  both Secrets + NetworkPolicy within one tick, so the plaintext-credential
  Secret no longer persists until the next runner-pod deletion.
- The ownerReference is best-effort: a cluster whose Helm chart does not
  wire the downward-API env vars gets `activeDeadlineSeconds` + the reaper
  only, which still satisfy the acceptance bar (bounded window, no manual
  prune). The env-var seam is the documented upgrade to full cascade GC.
- The reaper is off on the lock-less cloud *server* (by the same gate that
  protects the run and docker reapers) — but ON in the cloud *runner*, which
  has its own NATS-lease liveness authority. So in a multi-tenant cloud the
  runner reaper + cascade-on-runner-deletion + `activeDeadlineSeconds` do the
  work; a future in-cluster controller/CronJob can still adopt
  `ReapOrphanResources` behind its own lease if a server-side sweep is ever
  wanted.

## 2026-07-13 — reap predicate fails safe on a transient store error

The store-status backstop originally collapsed **any** `LoadRun` error into
"the run is gone → reap". That is wrong for a *transient* error (Mongo outage,
decode failure, context deadline): NATS KV and Mongo are independent, so a
lease-absent run hitting a Mongo blip would be force-deleted mid-flight —
sandbox pod **plus its plaintext-credential Secret and NetworkPolicy** — the
exact leak this ADR closes, re-opened as a data-destruction bug. It also
inverted the fail-safe direction the lease check two lines up already takes
("unknown liveness → keep") and the sibling `runview.sandboxContainerReapable`.

Fix: a shared `store.ErrRunNotFound` sentinel wrapped by both the Mongo and
filesystem `LoadRun` not-found paths. The reap predicate now reaps only on a
provable `errors.Is(err, store.ErrRunNotFound)` and **keeps** (fail safe,
retry next tick) on any other error — so "gone" means *provably* gone, never
"the store didn't answer". Only when the lease is absent AND the store proves
the run terminal-or-not-found is a sandbox reaped.
