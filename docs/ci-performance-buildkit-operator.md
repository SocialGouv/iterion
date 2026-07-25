# CI performance — buildkit-operator migration (measured)

Evidence file for the `image.yml` migration to
[buildkit-operator](https://github.com/SocialGouv/buildkit-operator): amd64 builds
on the in-house buildkit-operator warm daemon, arm64 on a native GitHub arm runner,
merged into one multi-arch index. Goal: prove or disprove, with real numbers, the
speed gain vs the previous `docker buildx` + QEMU multi-arch path. All numbers below
are measured from real GitHub Actions runs on `SocialGouv/iterion`.

## What changed

| | OLD (pre-migration) | NEW (this migration) |
|---|---|---|
| amd64 | `docker buildx` on the GH runner | buildkit-operator warm daemon (persistent cache mounts + S3 cold cache, GitHub-OIDC mTLS) |
| arm64 | **QEMU emulation** on the amd64 GH runner | **native** `ubuntu-24.04-arm` runner |
| cache | GitHub Actions cache (`type=gha`), layers only | daemon PVC: layers **+** `RUN --mount=type=cache` mounts + S3 cold cache |
| supply chain | cosign + syft per image | provenance/SBOM per-arch (operator), cosign-signed index once at merge |

The Dockerfiles also gained `RUN --mount=type=cache` mounts (pnpm store, go-build,
npm, apt, pip, go module cache) — warm on the operator daemon.

## Headline — full image.yml chain wall-clock

Source: `gh run view`. Reproduce: `scripts/bench/ci-build-bench.sh ci-durations --old-run <id> --new-run <id>`.

| run | path | chain wall-clock | speedup |
|---|---|---|---|
| `28290258656` | OLD — QEMU multi-arch | **62.3 min** | 1.00× |
| `28293580991` | NEW — operator amd64 + native arm64, **cold** daemons (first build, S3 seed) | **37.9 min** | **1.64×** |
| `28294577915` | NEW — operator amd64 + native arm64, **warm** daemons (steady state) | **26.7 min** | **2.33×** |

Steady-state CI (warm daemons) is the number that matters day-to-day: **62.3 → 26.7 min (2.33×)**.
The cold run is the honest first-build cost (daemon provision + S3 cache seed).

## Per-image, per-leg (minutes)

OLD is a single multi-arch job (amd64+arm64 under QEMU in one number). NEW runs
amd64 (operator) and arm64 (native) in parallel, then a merge job.

| image | OLD (QEMU multi-arch) | NEW amd64 cold | NEW amd64 warm | NEW arm64 native | NEW merge |
|---|---|---|---|---|---|
| iterion (`build`) | **33.3** | 8.6 | **0.8** | 2.8–6.7 | 1.0 |
| sandbox-slim | 2.6 | 5.5 | 3.1 | 1.9–2.3 | 1.4 |
| sandbox-full | 4.4 | 5.9 | 3.6 | 2.1–2.4 | 1.7 |
| sandbox-sec | **21.9** | 9.1 | 6.3 | 3.8–3.9 | 2.3 |

Per-image NEW wall-clock = max(amd64, arm64) + merge (legs run in parallel).

## Dockerfile cache mounts — measured effect

The `RUN --mount=type=cache` mounts (Part A) pay off on the warm operator daemon.
Evidence from the `build / amd64` operator log:

- The mounts run as authored:
  `#27 [studio-builder] RUN --mount=type=cache,target=/pnpm-store … pnpm install --store-dir=/pnpm-store …`
  and `#26 [llm-clis] RUN --mount=type=cache,target=/root/.npm npm install …`.
- The daemon **persists** them: its cache config exports `type==exec.cachemount`
  (`"Cache export": true … "type==exec.cachemount"`).
- S3 cold cache is active: `#14 importing cache manifest from s3:…`.

Result on the iterion image (amd64 operator leg):

| | cold daemon (first build + S3 seed) | warm daemon |
|---|---|---|
| amd64 build | **8.6 min** | **0.8 min** (~10×) |

The lockfile-first layer ordering already gated dep re-download on lockfile changes
(`COPY package.json pnpm-lock.yaml` → `pnpm install --frozen-lockfile` → `COPY studio`,
Go `-mod=vendor`); the cache mounts add warm-store reuse of the compile/install cost on top.

## Upstream-referenced figures (operator docs, for context)

From the operator's `docs/performance.md` / `docs/storage-and-cold-cache.md` — not our
measurement, cited for orientation:

- S3 cold rehydrate: ~41.8 s → ~4.5 s (~9×) on a fresh daemon (layers only).
- Daemon cold-start: ~19.5 s p50 PVC attach; ~90 s full provision — masked in steady
  state by scale-to-zero + PVC retention + `/prewarm` (we set `BUILDKIT_OPERATOR_WAIT_WARM=1`).

## Conclusion (honest, per axis)

- **The QEMU-bound heavy images are crushed.** The two offenders that were 88% of the
  OLD chain — iterion (33.3 min) and sandbox-sec (21.9 min) — drop to ~7.7 and ~8.6 min
  warm wall-clock. Eliminating QEMU arm64 (native arm runner) is the single biggest win.
- **Warm operator amd64 is excellent.** iterion amd64 8.6 → 0.8 min once the daemon is
  warm (cache mounts + S3) — a ~10× drop, exactly the Dockerfile-cache-mount payoff.
- **Cold-start has a real one-time cost.** First build on a fresh daemon (provision +
  S3 seed) makes the cold chain 37.9 min vs 26.7 warm — still well under OLD's 62.3.
- **Light images gain the least, proportionally.** sandbox-slim/full were already cheap
  under QEMU (2.6 / 4.4 min); the operator + per-image merge overhead (~1.4–1.8 min)
  means they're roughly flat-to-slightly-worse cold and modestly better warm. The net
  chain win comes from the heavy images, not these.
- **arm64-native is fast but gha-cache-variable.** Native arm64 ranges 1.9–6.7 min
  depending on the gha layer-cache state of the run (cache mounts don't persist on the
  gha leg). It is never the QEMU 20–33 min anymore, but it is now often the per-image
  critical path — a future lever (registry cache / a second operator arch) if needed.
- **Net, factual:** steady-state CI **62.3 → 26.7 min, 2.33×**; first-cold **1.64×**.
  The migration is a clear win, dominated by killing QEMU and warming the heavy-image
  amd64 builds on our own buildkit-operator (a successful dogfood of the in-house service).

## Reproduce

```sh
# Full chain comparison (needs gh):
scripts/bench/ci-build-bench.sh ci-durations --old-run 28290258656 --new-run 28294577915

# Per-state micro-bench (needs docker; operator states need mTLS + BUILDD_URL):
export BUILDKIT_OPERATOR_BUILDD_URL=https://buildd.bko.fabrique.social.gouv.fr
export BUILDKIT_OPERATOR_GATEWAY_HOST=bkod.fabrique.social.gouv.fr   # + mTLS certs on the buildx remote
scripts/bench/ci-build-bench.sh build --image bench --runs 3
scripts/bench/ci-build-bench.sh build --image sec  --runs 3   # the worst QEMU offender
```

## Operational notes (provisioning that made it work)

- buildd `/route` API: `https://buildd.bko.fabrique.social.gouv.fr` (publicly reachable).
- Daemon gateway (off-cluster, from GitHub-hosted runners):
  `gateway-host: bkod.fabrique.social.gouv.fr` — the public wildcard LB
  (`*.bkod.fabrique.social.gouv.fr` → 79.137.120.122:443). The internal `bko.*` host is
  filtered off-cluster; using it yields `context deadline exceeded`.
- Auth: GitHub OIDC (audience `buildkit-operator`) — no static token needed.
- `BUILDKIT_OPERATOR_WAIT_WARM=1` on the amd64 job: wait for the daemon to be Ready
  before pointing buildx at it (a cold-start daemon otherwise refuses the gRPC connection).
- Org vars/secrets (visibility ALL): `BUILDKIT_OPERATOR_BUILDD_URL`,
  `BUILDKIT_OPERATOR_GATEWAY_HOST`, `BUILDKIT_OPERATOR_{CA,CERT,KEY}`.

## Addendum 2026-07-22 — decoupled image pipeline + operator-side tuning

The serial slim→full→sec sandbox chain (the ~26 min above) no longer runs on
app pushes. The pipeline was restructured (see `.github/workflows/`):

- **Layer inversion**: the sandbox images became tool-only `*-base` images
  (rebuilt only when `sandbox/**` changes, own workflow `sandbox-images.yml`,
  own concurrency queue) + a cheap per-push finalize (`_finalize.yml`) that
  stamps the freshly-built iterion binary onto the published bases — all four
  variants in parallel, so `iterion-sandbox-*:edge` never lags the app image.
- **Shared operator daemons per family**: the four bases route to ONE
  buildkit-operator name (`iterion-sandbox-base`, shared content store + apt
  cache mounts), the four finalizes to another (`iterion-sandbox-finalize`,
  the app image is pulled once, not four times).
- **Operator-side** (buildkit-operator ≥ v0.13): adaptive keep-warm (idle
  window scales with observed build cadence) and bounded cache-volume
  auto-grow replace hand-tuned per-project CRs; the remaining declared tuning
  lives in the chart's `projectDefaults` (infra-apps).

Net effect on the day-to-day loop: a Go-only (or go.mod) push pays the app
build + parallel finalizes — no sandbox chain, no queueing behind it.
