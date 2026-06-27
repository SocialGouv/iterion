# CI performance — buildkit-operator migration (factual comparison)

This is the evidence file for the `image.yml` migration to
[buildkit-operator](https://github.com/SocialGouv/buildkit-operator). The goal is
to **prove or disprove**, with real numbers, the speed gain of building the
iterion container images on the in-house buildkit-operator (amd64) + a native
arm64 GitHub runner, vs the previous `docker buildx` + QEMU multi-arch path.

Be factual. If a state shows no gain, it is recorded as-is.

## What changed

| | OLD (pre-migration) | NEW (this migration) |
|---|---|---|
| amd64 | `docker buildx` on the GH runner | buildkit-operator warm daemon (persistent cache mounts + S3 cold cache) |
| arm64 | **QEMU emulation** on the amd64 GH runner | **native** `ubuntu-24.04-arm` runner |
| cache | GitHub Actions cache (`type=gha`), layers only | daemon-local PVC (layers **+** `RUN --mount=type=cache` mounts) + S3 cold cache |
| supply chain | cosign + syft per image | provenance/SBOM per-arch (operator), cosign-signed index once at merge |

The Dockerfiles also gained `RUN --mount=type=cache` mounts (pnpm store, go-build,
npm, apt, pip, go module cache) — warm on the operator daemon, they cut
compile/install cost even when a source-only change invalidates the layer.

## Cache states measured

| state | how it's forced | what it represents |
|---|---|---|
| `baseline-cold` | `docker buildx --no-cache` | worst case, no operator |
| `baseline-warm` | `docker buildx` reusing local layer cache | best case without the operator |
| `operator-cold` | operator `untrusted=true` (ephemeral daemon, `cache:null`) | true cold on the operator — no mounts, no S3 |
| `operator-warm` | operator, 2nd build on the same routing key | warm PVC: cache mounts + layers |
| `s3-cold-rehydrate` | fresh daemon after a seed (k8s-side, see below) | cross-daemon / scale-to-zero recovery |
| `daemon-cold-start` | first `/route` to a scaled-to-zero project | PVC attach (~19.5s p50, masked by prewarm) |

`s3-cold-rehydrate` and `daemon-cold-start` are **not** forceable from a pure CI
client (they need k8s-side BuildProject deletion); they are read from the
operator's Prometheus metrics and its `TestS3ColdCache` e2e, and cited below.

## Headline: full image.yml chain wall-clock

Source of truth: `gh run view <id>` (job `startedAt`/`completedAt`).
Reproduce: `scripts/bench/ci-build-bench.sh ci-durations --old-run <id> --new-run <id>`.

### OLD — run `28290258656` (main @ `722d8f80`, QEMU multi-arch) — MEASURED

| job | wall-clock | note |
|---|---|---|
| build (iterion) | **33.3 min** | Go compile + studio + tools, **arm64 under QEMU** |
| build-sandbox-slim | 2.6 min | |
| build-sandbox-full | 4.4 min | Go tarball + tool installs, arm64 under QEMU |
| build-sandbox-sec | **21.9 min** | semgrep/gosec/trivy installs **under QEMU** |
| **chain total** | **62.3 min** | sequential `needs:` chain |

The two QEMU-bound steps (main image 33 min, sec 22 min) are **88% of the chain**.

### NEW — run `<TBD>` (operator amd64 + native arm64) — TO FILL

> Fill after the first migrated run (workflow_dispatch on the branch, or first
> push to main). Command:
> `scripts/bench/ci-build-bench.sh ci-durations --old-run 28290258656 --new-run <new-id>`

| job | amd64 (operator) | arm64 (native) | merge+sign | note |
|---|---|---|---|---|
| build (iterion) | _TBD_ | _TBD_ | _TBD_ | |
| build-sandbox-slim | _TBD_ | _TBD_ | _TBD_ | |
| build-sandbox-full | _TBD_ | _TBD_ | _TBD_ | |
| build-sandbox-sec | _TBD_ | _TBD_ | _TBD_ | |
| **chain total** | | | **_TBD_** | warm-daemon run |

| | OLD | NEW (cold daemon) | NEW (warm daemon) | speedup |
|---|---|---|---|---|
| chain total (min) | 62.3 | _TBD_ | _TBD_ | _TBD_× |

> Record **two** NEW numbers: the first migrated run (cold daemon, one-time PVC
> attach + cache seed) and a re-run (warm daemon). The warm number is the
> steady-state CI experience; the cold number is the honest first-run cost.

## Per-build micro-benchmark (cache states)

Source of truth: `scripts/bench/ci-build-bench.sh build --image <bench|main|slim|full|sec> --runs 3`.
Run it where the operator is reachable (mTLS certs + `BUILDKIT_OPERATOR_BUILDD_URL`).

### image `bench` (representative, cheap) — TO FILL

| cache state | min (s) | median (s) | max (s) | CACHED steps |
|---|---|---|---|---|
| baseline-cold | _TBD_ | | | |
| baseline-warm | _TBD_ | | | |
| operator-cold | _TBD_ | | | |
| operator-warm | _TBD_ | | | |

(Repeat per real image with `--image main|slim|full|sec` for production fidelity.)

## Upstream-referenced figures (not our measurement)

From the operator's `docs/performance.md` / `docs/storage-and-cold-cache.md`,
cited for context — to be corroborated by our own runs above where possible:

- S3 cold rehydrate: **~41.8 s → ~4.5 s (~9×)** on a fresh daemon (layers only;
  cache mounts are per-daemon, not exported).
- Daemon cold-start: **~19.5 s p50** PVC attach; **~90 s** full provision —
  masked in steady state by scale-to-zero + PVC retention + `/prewarm`.
- Warm dedicated daemon vs shared pool: **~9.6 s vs ~18.3 s**.

## Conclusion — TO WRITE after NEW numbers land

State plainly, per axis:
- Did the chain total drop, and by how much (cold and warm)?
- Did native arm64 remove the QEMU penalty on `build` + `sec` specifically?
- Did the cache mounts help the warm re-run (CACHED-step count, compile time)?
- Any axis with **no** gain or a regression (e.g. cold-daemon first run slower
  than QEMU baseline) — record it; that is the honest pilot result of running our
  own buildkit-operator service.

## Reproduce

```sh
# Full chain comparison (needs gh):
scripts/bench/ci-build-bench.sh ci-durations            # lists recent main runs
scripts/bench/ci-build-bench.sh ci-durations --old-run 28290258656 --new-run <new-id>

# Per-state micro-bench (needs docker; operator states need mTLS + BUILDD_URL):
export BUILDKIT_OPERATOR_BUILDD_URL=...   # + BUILDKIT_OPERATOR_GATEWAY_HOST, mTLS certs on the buildx remote
scripts/bench/ci-build-bench.sh build --image bench --runs 3
scripts/bench/ci-build-bench.sh build --image sec  --runs 3   # the worst QEMU offender
```
