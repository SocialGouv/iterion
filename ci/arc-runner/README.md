# ARC CI runner with a C compiler, Go and Node

SocialGouv/iterion#981 distinguishes two failures: the race detector really
needs a C compiler, while early Mongo service initialization raced dind startup.
This image solves the compiler part. It preserves the upstream runner user,
groups, entrypoint and command; Docker access is covered by the separate dind
gate in SocialGouv/infra-apps#56.

It also bakes the Go of `go.mod` and Node 24, so the jobs' `setup-go` and
`setup-node` steps find them instead of downloading them — about 30 s and
12 s a job, through each node's ~20 MB/s share of the cluster's egress. They
sit in the runner's tool cache, `/opt/hostedtoolcache` (`RUNNER_TOOL_CACHE`),
in the layout those actions read (`<tool>/<version>/x64` and an
`x64.complete` marker): the runner's default tool cache lives under
`/home/runner/_work`, which ARC mounts a volume over. Each archive is checked
against its published sha256 at build time, and
`$RUNNER_TOOL_CACHE/iterion-ci-runner.env` records what was baked.

Builds live in Iterion because its Actions are enabled. The shared runner image
has its own lifecycle: `ARC CI runner image` runs only for this directory or its
workflow, and publishes only validated main builds. Its immutable version is
`1.<workflow-run-number>.<attempt>`, independent of the Iterion product release.
Every rebuild/retry gets a distinct version; no `latest` or `edge` is published.
The OCI revision label records the source commit. Renovate's existing Dockerfile
manager updates the upstream version and digest; no frozen organisation runner
repository or additional Actions allowlist entry is required.

```sh
docker build -t iterion-ci-runner:test ci/arc-runner
bash ci/arc-runner/verify.sh iterion-ci-runner:test
```

The verifier runs the image as UID 1001, with no network and a read-only root
filesystem: it checks the tool cache (go.mod's Go and Node 24, their markers
and versions, `RUNNER_TOOL_CACHE`, all of it owned by the runner so setup-go
can still add another version), then compiles a real cgo call to libc with
`go test -race` using the image's own Go, gcc and headers. The negative
control for the compiler is this image without gcc: `runtime/cgo` fails with
`C compiler "gcc" not found`.

**Bumping Go** is one change: go.mod (its `toolchain` line when it has one,
its `go` line otherwise — what setup-go reads), and in the Dockerfile
`GO_VERSION` with `GO_SHA256` (https://go.dev/dl/?mode=json lists it) —
`internal/ciguard` fails the build when the two versions differ. Once merged,
the image is published; bump its pin in infra-apps and sync. Until then
setup-go downloads the new Go, and the `test` job says so in a warning.
Node follows Renovate (`NODE_VERSION`); its PR fails the image build until
`NODE_SHA256` moves with it.

**For every repository on the scale set:** `setup-go` and `setup-node` take a
version already in the tool cache before downloading. A job that asks for a
range the baked version satisfies (`go-version: 1.26`, `1.x`,
`node-version: 24`) gets the baked one, not the newest patch — ask for an
exact version, or set `check-latest: true`, when the newest patch matters.

Local validation on 2026-09-13 used the exact upstream digest in the Dockerfile
as a negative control: `runtime/cgo` failed because `gcc` was not found. The
same verifier passed against the derived image (`ok .../arc-runner-smoke
1.011s`). This executes the test binary as well as compiling and linking it.
The writable temporary mount explicitly permits execution; the rest of the
container stays read-only. Actionlint and Bash syntax validation also pass.

Before changing ARC, publish a validated version, verify anonymous pull access
(the first GHCR package may need its owner to set visibility to public), and
pin both that version and its digest in infra-apps. Let Renovate update the
versioned image. All image fields referring to the runner distribution must
stay aligned, including init-dind-externals. The inotify initializer may keep
the upstream image because it copies no runner distribution files.

Only after approved image/dind activation and actual ARC runs should the
`race`, `mongo-conformance` and `cloud-e2e` routing overrides change. A green
local smoke check is not a full `go test -race ./...`, not a `services:` job,
and does not establish the cause of cloud-e2e's historical failure. Record each
of those proofs on #981; this image PR alone does not close it.
