# ADR-066 — Runner build-cache architecture: shared RWX Go caches now, Nix binary-cache substituter next

- Status: accepted
- Date: 2026-07-10
- Deciders: jo (direction), Claude (analysis + implementation)

## Context

Cloud runner pods (devbox-first-class image, ADR-lineage: the 2026-07-08
devbox runner) execute bot verify gates by cloning the target repo into an
ephemeral workdir (`/tmp/iterion/repos/<run-id>`) and running the repo's own
toolchain via `devbox run`. Three cache layers determine how cold that is:

1. **Nix store (`/nix`)** — the toolchain binaries (go, task, node…).
   Already mitigated: the runner image bakes `devbox global add go@1.26
   go-task` at build time, so the dominant toolchain is warm in the image
   layer itself.
2. **Go module cache (`GOMODCACHE`)** — the target repo's dependency
   downloads. Cold on every pod start (lives in the pod filesystem).
3. **Go build cache (`GOCACHE`)** — compiled package objects. Cold on
   every pod start AND unshared between runs on the same pod (per-run
   workdirs don't reset it, but pod churn does).

Live evidence (2026-07-09, run `019f47b8` on ovh-prod): a Billy verify pass
spent most of its wall-clock recompiling vendored dependencies from scratch —
a single `modernc.org/sqlite` compile took ~44 s × 3 concurrent test
invocations; full `go test ./pkg/...` passes ran 5+ minutes each, repeated
every campaign iteration. With KEDA autoscaling (2→50 pods), every scaled-up
pod pays the full cold cost, multiplying the waste exactly when the queue is
deepest. Slow verifies also widen the NATS AckWait window, feeding the
fail→redeliver loops observed on stalled runs.

## Decision

**Phase 1 (this ADR, shipped): opt-in shared RWX PVC for the Go caches.**
The chart gains a `runner.cache` block (`charts/iterion/templates/
runner-cache-pvc.yaml` + runner-deployment wiring): a ReadWriteMany PVC
mounted at `/cache` on every runner pod, with `GOMODCACHE=/cache/go/pkg/mod`
and `GOCACHE=/cache/go/build` exported pod-wide. `devbox run -- go …`
inherits the env, so no image or bot change is needed. Off by default; on
OVH prod the storage class is `openebs-nfs-hspeed` (the RWX-capable NFS
provisioner available in the cluster).

Why sharing one volume across the fleet is safe *for these two caches*:
- `GOMODCACHE` is read-mostly, content-addressed by module version, and
  protected by go's own lockfile discipline (`go mod download` uses lock
  files within the cache).
- `GOCACHE` is content-addressed (action IDs) and explicitly designed for
  concurrent access; a corrupted/missing entry degrades to a recompile,
  never to wrong output.

**Phase 2 (follow-on, not in this change): Nix binary-cache substituter for
the long tail of toolchains.** For target repos whose `devbox.json` needs
tools NOT baked in the image (python, rust, node versions…), the fix is a
shared substituter (attic or S3-backed nix cache) that pods hit over HTTP,
not a shared `/nix` mount.

## Alternatives considered

- **Shared RWX PVC for the live `/nix` store — REJECTED.** The Nix store
  has a SQLite metadata DB (`/nix/var/nix/db`) that assumes a single
  local-filesystem writer. Many pods mutating one store over NFS risks
  store-DB corruption fleet-wide — one corrupted shared volume takes down
  every runner (blast radius maximal, on the component meant to *improve*
  reliability). The image-baked store + substituter pattern gets the same
  warmth with zero shared mutable state.
- **Per-pod RWO PVC (StatefulSet-style) — REJECTED.** Keeps caches warm
  across restarts of the *same* pod but leaves every KEDA scale-up pod cold
  (the moment that matters most), multiplies storage cost by maxReplicas,
  and would require converting the Deployment to a StatefulSet.
- **hostPath node-local cache — REJECTED.** Warm only per node, breaks the
  no-node-affinity assumption, and is a tenant-isolation smell on shared
  nodes.
- **Remote GOCACHE (`GOCACHEPROG`, e.g. a bazel-remote/S3 adapter) —
  DEFERRED.** Strictly better isolation than NFS (HTTP protocol, no shared
  filesystem semantics), but needs Go ≥1.24 plumbing in every target repo's
  toolchain and an extra service. Revisit if NFS small-file latency on
  GOCACHE proves worse than recompiling (escape hatch documented in
  values.yaml: point GOCACHE at an emptyDir, keep GOMODCACHE shared).

## Consequences

- Fresh and scaled-up runner pods reuse downloaded modules + compiled
  objects; verify passes on Go repos drop from minutes to seconds of
  compile time after first warm-up.
- The volume grows unbounded (Go prunes GOCACHE at ~trim intervals, never
  GOMODCACHE). 20Gi default + `AllowVolumeExpansion` on the class; a
  periodic prune (cron `go clean -modcache` equivalent or size-triggered
  recreation) is acceptable ops debt for now — the volume is a cache, so
  deleting it is always safe.
- Non-Go ecosystems get nothing yet. The same pattern (shared
  content-addressed cache dirs: npm cache, pip cache, cargo registry)
  can extend the same PVC later; each needs its own concurrency-safety
  review before enabling.
- The GOCACHE/GOMODCACHE env is pod-wide, so *sandboxed* runs (sibling
  containers) do NOT inherit it — this covers the in-pod execution path
  (`ITERION_SANDBOX_OVERRIDE=none`), which is the cloud-runner production
  path today.
