# Development

This page is for contributors to Iterion itself. The repository's reproducible toolchain is the supported path; CI and bot verification use the same task entry points.

## Toolchain

Install [Devbox](https://www.jetify.com/devbox). The checked-in `devbox.json` currently provides Go 1.26, Node 24, Go Task, golangci-lint, Helm, kubectl/kind, desktop build libraries, and supporting tools.

```bash
devbox shell
# or run one command without entering a shell:
devbox run -- task check
```

Optional [direnv](https://direnv.net/) integration activates the environment on entry:

```bash
eval "$(direnv hook bash)"   # use the equivalent hook for your shell
direnv allow
```

The repository also ships `.devcontainer/` for VS Code/Codespaces. Task automatically reads a root `.env` when present; it is gitignored and intended for local credential/config overrides.

**pnpm via corepack:** the `studio/` workspace is locked to a specific
pnpm version through `package.json`'s `packageManager` field. The
Taskfile invokes pnpm as `corepack pnpm …` so the version is
auto-dispatched without polluting the host install. Corepack ships
with the `nodejs_24` package devbox already provides — no extra
install. Don't run `corepack enable` inside devbox: the Nix store is
read-only, the global symlink fails, and you don't need it (`corepack
pnpm` works without enable).

## Build and checks

```bash
task build             # build studio, sync embedded dispatcher bots, then ./iterion
task lint              # gofmt + go vet + golangci-lint
task test              # all Go unit tests
task test:e2e          # deterministic/stub E2E suite
task test:goldens      # recorded bot-schema/invariant replays; no credentials
task studio:check      # ESLint + TypeScript + Vitest
task check             # lint + the free layer: unit, goldens, studio, pi extension, brand, DSL and map checks
```

`task --list` prints every task with its description. `task lint` runs a
curated golangci-lint (`.golangci.yml`: errcheck, govet, ineffassign,
staticcheck, unconvert, unused; misspell is off because it flags French
comments; tests skip errcheck and SA1012; `cmd/iterion-desktop` is excluded as
cgo/build-tagged). Binaries build with `CGO_ENABLED=0`; version and commit come
in through ldflags from `package.json` and git.

Useful narrower gates:

```bash
task check:fast           # the fast inner-iteration layer: fmt + vet + golangci --fast-only + bot fmt + dsl + map + positioning (a red here is worth fixing first, but `task check` stays owed)
task test:race
task test:bundle
task test:live:compile     # compile every -tags=live test in the whole repo (`./...`, so any live-tagged file outside ./e2e is covered too) without running/cost; also runs in CI as a step of the required `test` job, so a refactor that breaks the live layer's compilation reddens the PR
task test:contract:rgaa    # the rgaa-audit engine contract against the REAL pinned ultra11y (offline, no LLM): `npm ci` from scripts/ultra11y-engine/package-lock.json on first use, entry file checked against bots/rgaa-audit/ultra11y.pin; CI runs it in the test and race jobs
task openapi:check         # regenerate OpenAPI + studio types, fail on diff
task docs:links            # every relative link and #anchor of the tracked markdown resolves, GitHub-style (docs:links:new = only what a change adds)
task sdk:ts:check          # TypeScript SDK build/typecheck/tests
task desktop:test
task chart:lint
task ci:shards -- -n 3 -i 0  # print one deterministic test shard — the mechanism the CI race-shard matrix legs (the #1977 split) divide ./... with (bare, or `-all`, dumps every shard)
```

`task test:live` and the narrower `test:live:*` tasks call real backends and require the credentials named in each task description. `test:goldens:record` also calls a real LLM and rewrites fixtures; ordinary verification should use `test:goldens`.

The direct Go commands work when generated assets are already materialised:

```bash
go build -mod=vendor -o iterion ./cmd/iterion
go test ./...
```

Prefer `task build` after editing studio assets or any of the nine embedded dispatcher bots because it runs `studio:build` and `templates:dispatch-bots` first.

Targeted Go tests (the CI suite at full size is minutes; a change usually
concerns one tree):

```bash
go test -mod=vendor -count=1 ./bots/                                  # the unit-suite floor — almost ten minutes, everything else finishes long before it
go test -mod=vendor -count=1 -run 'TestName' ./pkg/<tree>/...          # one test in one tree
go test -mod=vendor -count=1 -v ./e2e/...                              # the e2e suite, verbose
go test -race -mod=vendor -count=1 ./pkg/<tree>/...                    # one tree under the race detector
```

`-count=1` matters whenever a child process builds code (e.g. `cmd/iterion`
e2e binaries): Go replays cached `ok` results for inputs it cannot see.

## Running the Mongo conformance harness locally

Every suite gated on `ITERION_TEST_MONGO_URI` skips under a plain `go test
./...`; CI's `mongo-conformance` job is what enforces the contract, over the
package trees listed in its `go test` line — and `TestMongoGatedPackagesAreInTheCIJob`
(`pkg/store/mongo/ci_mongo_gate_test.go`) fails the build when a package whose
tests actually READ the variable is missing from that list (a package that
merely names it in a comment does not count). A change to a store twin or a
conformance row is therefore unverified until it ran against a real replica
set. The recipe below mirrors the job (`mongo:8.0`, one-member replica set for
change streams and transactions) and was run verbatim on 2026-09-06: every
listed tree green in 95 s.

```bash
# mongod listens on 27018 INSIDE the container too (--port), so the member
# it advertises resolves to itself; 27017 is left to a studio's own Mongo.
docker run --rm -d --name iterion-mongo-conf --ulimit nofile=131072:131072 \
  -p 27018:27018 mongo:8.0 --replSet rs0 --bind_ip_all --port 27018
docker exec iterion-mongo-conf mongosh --port 27018 --quiet \
  --eval 'rs.initiate({ _id: "rs0", members: [{ _id: 0, host: "localhost:27018" }] })'
# wait for the election — a single status read is not a wait:
for i in $(seq 1 30); do
  st=$(docker exec iterion-mongo-conf mongosh --port 27018 --quiet \
        --eval 'try { print(rs.status().members[0].stateStr) } catch (e) { print("NOTREADY") }' | tail -1)
  [ "$st" = "PRIMARY" ] && break; sleep 2
done; [ "$st" = "PRIMARY" ] || { echo "replica set never became PRIMARY"; exit 1; }

# the same trees the CI job runs, read from the job itself:
pkgs=$(sed -n '/^  mongo-conformance:/,/^  [a-z-]*:$/p' .github/workflows/tests.yml | grep -oE '\./pkg/[a-z/]+/\.\.\.' | tr '\n' ' ')
ITERION_TEST_MONGO_URI='mongodb://localhost:27018/?replicaSet=rs0' \
  devbox run -- go test -count=1 $pkgs
docker rm -f iterion-mongo-conf
```

Three details that each cost a session: `-p 27018:27017` with a member
advertised as `localhost:27018` never initiates (docker's port mapping is
host-side; inside the container nothing listens on 27018, so
`replSetInitiate` finds no member that is itself) — bind mongod on the
published port; poll for `PRIMARY`, or `go test` connects mid-election and a
`ReplicaSetNoPrimary` reads as a store regression; and `--ulimit nofile` is
not optional — the suites create a fresh database with ~10 indexes per
subtest, and a container's default file-descriptor limit makes `mongod` panic
with `Too many open files` minutes in, which the test reports as
`connection refused`.

Both store twins start a run differently on purpose — the filesystem store
goes straight to `running`, the cloud store to `queued`
(`storetest.Opts.InitialStatus` models it). A conformance row that
compares-and-sets from a status it did not set itself passes on one twin by
accident and can never match on the other; state the status the row tests
from.

## The same replica set from an agent's Bash sandbox

The recipe above assumes the test process and docker share a network
namespace. An agent's Bash sandbox (Claude Code's bubblewrap one) does not:
its namespace holds `lo` and nothing else, which `ip -o addr` prints in two
lines, and docker's port publication is host-side — so `127.0.0.1:<published>`
answers `connection refused` inside the sandbox while `docker port` shows the
mapping, and the container's bridge address is `network unreachable`. The
symptom is not an error, it is a wait: `mongostore.New` validates the
connection through `pingPrimary`, which retries as long as its context lives
(`pkg/store/mongo/ping.go`), and the context comes from `t.Deadline()`, so the
test burns the whole `-timeout` before reporting — measured against a dead
port, `-timeout 45s` reports after 40 s, which makes the documented
`-timeout 1200s` a twenty-minute silence that reads as a store regression.

A unix socket crosses the namespace boundary, being a filesystem object rather
than a route. Run mongod on one in a bind-mounted scratch dir outside the
repo, initiate the single-member set as above so transactions are available,
and keep `directConnection=true`: the driver then connects only to the host in
the URI and never discovers the `host:port` the member advertises, which would
be a TCP route again (`SetDirect`, the driver's `clientoptions.go`). Run
verbatim on 2026-11-01, where it carried a transaction-replay suite through
the real `mongostore.SaveRun`.

```bash
name=iterion-mongo-sock                    # your own name: never a running container's
sockdir=${TMPDIR:-/tmp}/$name              # outside the repo; mongod creates the socket here
mkdir -p "$sockdir" && chmod 0777 "$sockdir"   # the image's mongod is not your uid
docker run --rm -d --name "$name" --ulimit nofile=131072:131072 \
  -v "$sockdir":/sock mongo:8.0 --replSet rs0 \
  --unixSocketPrefix /sock --filePermissions 0777
# mongod answers a few seconds after the container exists — initiating before
# that is ECONNREFUSED, and the set then stays unconfigured for good:
for i in $(seq 1 30); do
  docker exec "$name" mongosh --quiet --eval 'db.adminCommand({ ping: 1 })' >/dev/null 2>&1 && break
  sleep 1
done
docker exec "$name" mongosh --quiet \
  --eval 'rs.initiate({ _id: "rs0", members: [{ _id: 0, host: "localhost:27017" }] })'
for i in $(seq 1 30); do
  st=$(docker exec "$name" mongosh --quiet \
        --eval 'try { print(rs.status().members[0].stateStr) } catch (e) { print("NOTREADY") }' | tail -1)
  [ "$st" = "PRIMARY" ] && break; sleep 2
done; [ "$st" = "PRIMARY" ] || { echo "replica set never became PRIMARY"; exit 1; }

# the socket path is the URI's host, percent-encoded, and must end in .sock
uri="mongodb://$(printf %s "$sockdir/mongodb-27017.sock" | sed 's|/|%2F|g')/?directConnection=true"
# devbox run starts in the devbox project dir, not the cwd: name the tree you mean
root=$(dirname "$(git rev-parse --git-common-dir)")
ITERION_TEST_MONGO_URI="$uri" devbox run -c "$root" -- \
  bash -c "cd $PWD && go test -count=1 -timeout 1200s ./pkg/server/cloudpublisher/"
docker logs --tail 50 "$name"              # capture before removing: the container is --rm
docker rm -f "$name"
```

`--bind_ip_all` is absent on purpose: nothing dials this server over TCP, and
`mongosh` inside the container reaches it on loopback. Both permissions are
needed, and each is missed differently: a mount dir left at your own `0775`
makes mongod exit 48 with the dir still empty — it does not run as your uid
and has to create the socket there — while without `--filePermissions 0777`
the socket appears as `srwx------` owned by the image's user and the host side
gets `permission denied`. The socket inode outlives `docker rm` in the scratch
dir; it is inert, and a rerun replaces it.

## Frontend and local services

```bash
task studio:dev              # backend + Vite HMR
task studio:dev:backend
task studio:dev:frontend
task cloud:up                # local Mongo/NATS/SeaweedFS/server stack
task cloud:test:s3           # S3 compatibility bench against its SeaweedFS, in its own bucket
task cloud:logs
task cloud:down              # also removes compose volumes
```

A stack started before SeaweedFS replaced MinIO leaves two volumes that
`cloud:down` no longer knows; remove them once with
`docker volume rm -f iterion-cloud_minio-data iterion-cloud_runner-work` (`-f`
ignores one that is not there).

Desktop, chart, image, and cross-platform packaging tasks are listed by `task --list-all`; use their dedicated runbooks before releasing: [desktop build](desktop-build.md), [desktop release](desktop-release-checklist.md), and [cloud deployment](cloud-deployment.md).

## Repository structure

```text
iterion/
├── cmd/
│   ├── iterion/             # Cobra entrypoint
│   └── iterion-desktop/     # Wails desktop wrapper
├── pkg/
│   ├── cli/                 # public CLI command implementations/templates
│   ├── dsl/                 # lexer/parser, AST, expressions, IR/compiler, unparser
│   ├── runtime/             # graph engine, routing, loops, budgets, recovery, worktrees
│   ├── backend/             # model/delegated executors, MCP, tools, cost, secret guard
│   ├── bundle/              # .botz loading; bundlelint holds C200–C234 checks
│   ├── sandbox/             # Docker/Podman/Kubernetes isolation and egress controls
│   ├── store/               # local and cloud persistence abstractions
│   ├── server/ + runview/   # studio/run/cloud HTTP and streaming surfaces
│   ├── dispatcher/          # tracker polling, leases, hooks, issue-to-bot dispatch
│   ├── schedule-related     # cloudsched, schedgate, trigger
│   ├── cloud-related        # queue, runner, auth, identity, orgusage, forge, webhooks
│   └── extensions/state     # plugin, skilllib, memory, secrets, marketplace, supervise
├── studio/                  # React/Vite/TypeScript UI
├── bots/                    # maintained bot catalogue (main.bot + manifest/resources)
├── examples/                # focused DSL/integration demonstrations
├── e2e/                     # deterministic and build-tagged live E2E tests
├── sdks/typescript/         # @iterion/sdk CLI wrapper
├── charts/iterion/          # Helm chart and tests
├── docker/ + sandbox/       # container images/helpers and sandbox fixtures
├── docs/                    # living guides plus explicitly dated records
├── scripts/ + tooling/      # generation, release, and verification helpers
├── internal/httpx/          # module-private HTTP utility
├── third_party/             # checked-in third-party source/assets
└── vendor/                  # vendored Go modules, including claw-code-go
```

The labels `schedule-related`, `cloud-related`, and `extensions/state` above are conceptual groupings, not literal directories; inspect `pkg/` for the individual packages. This keeps the map useful as packages evolve.

## Key contracts

- DSL syntax lives in `pkg/dsl/parser`; compilation/semantic validation lives in the split files under `pkg/dsl/ir`. Diagnostics use sparse DSL ranges C001–C199 and async C240–C242; bundle checks use C200–C234.
- `pkg/server` registers the HTTP route table that generates `openapi.json`; `task openapi:check` guards the committed spec and studio types.
- `bots/` is the editable full catalogue. `pkg/cli/templates/dispatch_bots/` is generated for the embedded zero-config subset; do not hand-maintain the copies.
- Studio's production build is copied into `pkg/server/static` and embedded into the Go binary.
- The module vendors [`claw-code-go`](https://github.com/SocialGouv/claw-code-go) for in-process multi-provider execution. Keep `go.mod`, `go.sum`, and `vendor/` consistent.
- Bump the claw-code-go pin **only** with [`scripts/bump-claw.sh`](../scripts/bump-claw.sh): it pushes the claw commit if needed, then runs `go get @<sha>`, tidy, vendor, verify and commits. Never hand-write the pseudo-version — a locally computed, non-UTC timestamp fails `go mod verify` ("does not match version-control timestamp") and turns `vendor-check` red on `main` and on every PR merge ref (three times on 2026-07-11). claw-code-go is developed by this team (sibling worktrees under `.works/`): improving it while touching a seam that crosses it is in scope.

Before opening a change, run the gates your diff concerns (the narrower block above; `task check:fast` covers the cheap cross-cutting layer) and finish with `devbox run -- task check` when practical. Changes to OpenAPI, the SDK, Helm, desktop, or live-test declarations also need their domain-specific checks above.
