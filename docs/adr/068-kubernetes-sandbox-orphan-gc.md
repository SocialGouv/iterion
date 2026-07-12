# ADR-068 — Garbage-collect orphaned kubernetes sandbox pods and their plaintext-credential Secrets

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
   lock-less cloud server sharing a namespace with live runner pods can never
   reap an in-flight run's sandbox. It runs on the tick (unlike the docker
   reaper, which is boot-only) because a runner OOM-killed while the server
   stays up leaks a plaintext-credential Secret that boot-only reaping would
   miss until the next restart.

The docker driver path is untouched.

## Consequences

- A killed runner's sandbox pod + both Secrets + NetworkPolicy are GC'd by
  the cluster within a bounded window with no manual `kubectl delete`:
  immediately on runner-pod deletion (ownerReference cascade), and
  otherwise by the reaper's boot/periodic sweep once the run is terminal.
  `activeDeadlineSeconds` bounds the pod's compute consumption in the
  meantime.
- No plaintext-credential Secret outlives its run once either the reaper
  runs or the runner pod is deleted.
- The ownerReference is best-effort: a cluster whose Helm chart does not
  wire the downward-API env vars gets `activeDeadlineSeconds` + the reaper
  only, which still satisfy the acceptance bar (bounded window, no manual
  prune). The env-var seam is the documented upgrade to full cascade GC.
- The reaper is off on the lock-less cloud server (by the same gate that
  protects the run and docker reapers), so in a multi-tenant cloud the
  cascade-on-runner-deletion + `activeDeadlineSeconds` do the work, and a
  future in-cluster controller/CronJob can adopt `ReapOrphanResources`
  behind its own lease if a server-side sweep is ever wanted.
