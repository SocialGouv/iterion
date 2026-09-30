# ADR-105 — CI build cache on the org's runners: cold, measured, and never a shared writable store

- Status: proposed
- Date: 2026-09-30
- Deciders: jo (direction), Claude (analysis)
- Relates to: [066-runner-build-cache-architecture.md](066-runner-build-cache-architecture.md)
  (the product's cloud runners — a shared writable RWX cache, off by default;
  the same write-trust question applies if it is re-enabled, see below);
  [docs/merge-policy.md](../merge-policy.md) (the merge queue this serves)

## Context

Since #1955, every required check from `tests.yml` runs on the organisation's
self-hosted scale set `arc-runners` (ovh-dev), and a merge-queue entry is a
full CI cycle. Measured on 2026-09-29, after #1955:

| What | Time |
|---|---|
| `test` without its `setup-go` step (five runs) | 1 009-1 167 s |
| `race` (five runs; built cold, cache off since #1955) | 796-1 039 s |
| `setup-go` in `test`: toolchain, then restoring GitHub's Actions cache | 98-432 s (**432 s** in main run 36625201544) |
| `setup-go` in `vendor-check`, same run | **447 s**, for 11 s of work |
| `fmt-check`, cache off: compile `cmd/iterion` and its dependencies cold, run it | **66 s** (+31 s toolchain) |

The archive those restores fetch is keyed on the image OS, the Go version and
`go.sum`, and the one on the scale set (409 MB, created 2026-09-29 10:25:18Z)
was saved by `vendor-check`'s post step — a job that downloads modules and
compiles nothing. `test` builds with `-mod=vendor`: **it restores 400 MB it
does not use, and has always compiled cold** (its `go run ./cmd/iterion
openapi` step takes 56-64 s after a restore, fmt-check's cold 66 s). The
restore ran at ~1 MB/s in the 432 s case.

Three facts bound any cache design:

1. **Every job owns its node.** `dind` runs privileged on an org-scoped scale
   set (infra-apps `arc-runners/README.md`: "each owns the node it lands on"),
   the organisation is on GitHub's Free plan (no separate runner groups), and
   `merge_group` runs pull-request code on the scale set. A cache *writer* on
   these runners can be tampered with by whatever shares its node; so can a
   reader's copy of anything on that node.
2. **Go trusts its cache, test results included.** Build outputs are reused
   after a size and build-ID check (`cmd/go/internal/work/buildid.go`), and a
   cached test result is replayed when the test binary and the files *it*
   opened are unchanged — files read by a child process are not tracked.
   iterion's e2e tests build `./cmd/iterion` in a child `go build`
   (`e2e/cli_server_boot_test.go`, `e2e/mcp_server_test.go`): with a shared
   `GOCACHE`, a change confined to `cmd/iterion` replays `ok (cached)` for
   them. Reproduced on 2026-09-29 — a test building its app in a child
   process reports `ok (cached)` after the app broke, `FAIL` with `-count=1`.
3. **A cache Go uses is a cache Go rewrites.** It trims entries unused for five
   days and re-timestamps those it touches; since Go 1.24 it also caches
   `go run` executables (~94 MB for `cmd/iterion`). A restored-and-resaved
   archive only grows; a read-only baked one decays.

## Decision

1. **CI's required test steps run with `-count=1`**, so CI never replays a test
   result from a cache (fact 2), whatever cache it restores. The local
   `task test`/`task check` keep Go's test cache for speed; a developer who
   changed only `cmd/iterion` should run the e2e package with `-count=1` — CI
   will.
2. **On the scale set, Go jobs build cold: `setup-go`'s cache off**
   (`cache: ${{ runner.environment == 'github-hosted' }}`: fork and
   dependency-bot pull requests keep GitHub's cache at GitHub's speed). For
   `test`, that removes the restore — 1.5 to 7 minutes of the queue's critical
   path — at no compile cost, since it builds cold already. `vendor-check` and
   `golangci` download their modules instead of restoring them (~205 MB for
   `vendor-check`'s `go mod tidy`). Every job it touches gets a
   `timeout-minutes` (the go command's module fetches have no timeout of their
   own). Measured on the pull request and over the following days: job time
   and, for those two, download time. On the scale set `vendor-check`'s
   `go mod verify` then checks an empty module cache — it still means
   something on the GitHub-hosted path.
3. **Modules, if downloads prove to matter: an in-cluster GOPROXY.** Unlike
   `GOCACHE`, a module proxy is safe to share organisation-wide for
   *integrity* — `go.sum` and the checksum database verify every module it
   serves — provided its store is unwritable from runner pods: a corrupted
   module fails every Go job after the fetch, which is an availability hole,
   not a quiet one.

## Alternatives considered

- **Keep GitHub's Actions cache on the scale set — REJECTED** by the numbers
  above: for `test` it restores an archive with no build in it, and under load
  the restore costs more than compiling.
- **A warm `GOCACHE` baked into the runner image — DEFERRED.** It would save
  roughly two minutes of compiling in `test` (77 s cold vs 11 s warm for its
  test binaries on 8 local CPUs, ~140 s vs ~20 s scaled to a pod), but `race`
  — cold, and sharing no action ID with `test` — is the queue's floor at
  13-17 minutes: until `race` is sharded, no lever on `test` alone shortens
  the cycle by more than that gap. Done right it needs, each of them
  load-bearing:
  - every entry's mtime set in the future when the layer is sealed —
    otherwise Go trims the baked entries after ~5 days and copies up those it
    re-timestamps;
  - each target job's environment and commands replayed verbatim
    (`CGO_ENABLED=0` for `test` and `golangci` — the image ships gcc; tags;
    test variants), with Go at the scale set's `GOROOT` (#1964's tool cache)
    and at its checkout path (`/home/runner/_work/iterion/iterion`, not a
    GitHub-hosted runner's `/home/runner/work/…`);
  - `go clean -testcache` before sealing;
  - an image pipeline whose publish job runs only on `main` and alone holds
    `packages: write` (`ci-runner-image.yml` grants it at workflow level today,
    pull-request runs included), with the source in its build context and a
    rebuild-and-pin cadence under five days.
  And it is only as trustworthy as each node (fact 1): its advantage over a
  store is a blast radius of one node, not immunity.
- **A writable cache store in the cluster (S3 — SeaweedFS or MinIO — written
  by `main` only, read elsewhere) — REJECTED on this scale set.** Its
  write-once boundary does not survive fact 1: the writer runs where any
  organisation job can own its node, and a poisoned entry would persist and
  spread to every node. It becomes possible only with a writer off the scale
  set whose entries readers verify (a signature bound to the writer's
  workflow), or with node isolation — Kata, which infra-apps runs only on
  ovh-prod's build pool: a new pool on ovh-dev. As infra-apps deploys
  SeaweedFS (without its security configuration), its master, volume and filer
  APIs accept writes from any pod that reaches them, so runner pods would have
  to reach the S3 gateway alone. MinIO's upstream repository is archived and
  its images are no longer distributed (infra-apps records it).
- **`GOCACHEPROG` against such a store — DEFERRED**, for the same trust
  reason. It would upload only what a build produced and fetch only what a
  build needs; revisit with the store.
- **A self-hosted Actions cache server** (e.g. falcondev-oss's
  github-actions-cache-server) — **DEFERRED**: a patched runner binary in the
  organisation-wide image, an organisation-wide service, and `main`'s scope
  writable by any run on `main` — which runs on the scale set, fact 1 again.
- **S3-backed drop-in cache actions** (`runs-on/cache`, `tespkg/actions-cache`)
  — the same store, the same trust problem; they save writing a protocol, not
  a boundary.
- **A shared ReadWriteMany volume or `hostPath` per node — REJECTED**: a mount
  every job can write is a cache every job can poison.

## Consequences

- Decision 1 closes fact 2 for the gate before any cache work.
- Decision 2 is expected to take 1.5-7 minutes off `test` and most of
  `vendor-check`'s ~7, with nothing to operate or trust; the pull request
  implementing it carries the before/after numbers.
- After it, `test` (~17-19 min) and `race` (13-17 min) are close: the next
  lever on the cycle is sharding `race`, then splitting `test`. Each new job
  pays its own checkout (50-77 s, internet-bound) and toolchain (~30 s,
  unless #1964's baked one), which that design must count.
- ADR-066's shared writable RWX cache (off by default and disabled in prod)
  runs target repositories' own code against one `GOCACHE`; if it is
  re-enabled, facts 1-2 apply there, as they do to the GOPROXY +
  `GOCACHEPROG` follow-up the infra-apps values plan for the product's
  runners. ADR-066's "never to wrong output" does not hold for test results
  (fact 2). The rule for both: nothing untrusted writes a cache that
  something else trusts.
