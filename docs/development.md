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

## Build and checks

```bash
task build             # build studio, sync embedded dispatcher bots, then ./iterion
task lint              # gofmt + go vet + golangci-lint
task test              # all Go unit tests
task test:e2e          # deterministic/stub E2E suite
task test:goldens      # recorded bot-schema/invariant replays; no credentials
task studio:check      # ESLint + TypeScript + Vitest
task check             # lint + unit + goldens + studio:check
```

Useful narrower gates:

```bash
task test:race
task test:bundle
task test:live:compile     # compile every -tags=live test without running/cost
task openapi:check         # regenerate OpenAPI + studio types, fail on diff
task sdk:ts:check          # TypeScript SDK build/typecheck/tests
task desktop:test
task chart:lint
```

`task test:live` and the narrower `test:live:*` tasks call real backends and require the credentials named in each task description. `test:goldens:record` also calls a real LLM and rewrites fixtures; ordinary verification should use `test:goldens`.

The direct Go commands work when generated assets are already materialised:

```bash
go build -mod=vendor -o iterion ./cmd/iterion
go test ./...
```

Prefer `task build` after editing studio assets or any of the nine embedded dispatcher bots because it runs `studio:build` and `templates:dispatch-bots` first.

## Running the Mongo conformance harness locally

`pkg/store/mongo` (and every other suite gated on `ITERION_TEST_MONGO_URI` —
`grep -rl ITERION_TEST_MONGO_URI --include='*_test.go' pkg/`) skips under a
plain `go test ./...`; CI's `mongo-conformance` job is what enforces the
contract. A change to a store twin or a conformance row is therefore
unverified until CI runs it — unless you reproduce the job. The recipe that
matches the job (`mongo:8.0`, a one-member replica set for change streams and
transactions), on a port that leaves a studio's own Mongo alone:

```bash
docker run --rm -d --name iterion-mongo-conf --ulimit nofile=131072:131072 \
  -p 27018:27017 mongo:8.0 --replSet rs0 --bind_ip_all
docker exec iterion-mongo-conf mongosh --quiet \
  --eval 'rs.initiate({ _id: "rs0", members: [{ _id: 0, host: "localhost:27018" }] })'
# wait for PRIMARY:
docker exec iterion-mongo-conf mongosh --quiet --eval 'rs.status().members[0].stateStr'

ITERION_TEST_MONGO_URI='mongodb://localhost:27018/?replicaSet=rs0' \
  devbox run -- go test ./pkg/store/mongo/ -run 'TestConformance_Mongo' -v
docker rm -f iterion-mongo-conf
```

Two details that cost a session each: the member must be advertised on the
published port (`localhost:27018`), or the driver's topology resolves back
onto whatever listens on 27017; and the `--ulimit nofile` is not optional —
the whole suite creates a fresh database with ~10 indexes per subtest, and a
container's default file-descriptor limit makes `mongod` panic with
`Too many open files` a few minutes in, which reads as `connection refused`
from the test.

Both store twins start a run differently on purpose — the filesystem store
goes straight to `running`, the cloud store to `queued`
(`storetest.Opts.InitialStatus` models it). A conformance row that
compares-and-sets from a status it did not set itself passes on one twin by
accident and can never match on the other; state the status the row tests
from.

## Frontend and local services

```bash
task studio:dev              # backend + Vite HMR
task studio:dev:backend
task studio:dev:frontend
task cloud:up                # local Mongo/NATS/MinIO/server stack
task cloud:logs
task cloud:down              # also removes compose volumes
```

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

Before opening a change, run the smallest relevant gates and finish with `devbox run -- task check` when practical. Changes to OpenAPI, the SDK, Helm, desktop, or live-test declarations also need their domain-specific checks above.
