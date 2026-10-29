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

It also bakes **the actions the workflows pin**, where the runner looks for
them before it reaches the network. An ephemeral runner starts with an empty
`_actions`, so every job downloads every action again from
codeload.github.com — which answers `429` under load: on 2026-10-15
`test-unit`, `vendor-check` and `fmt-check` failed before any step ran and the
merge queue ejected the pull request, the fourth time (SocialGouv/iterion#2435),
and a third party measured the same throttling with no ARC involved. Baking
hardens against a service nobody here controls; a mirror or a shared mutable
cache would not.

The mechanism is the runner's own, not an entrypoint of ours:
`ACTIONS_RUNNER_ACTION_ARCHIVE_CACHE` (actions/runner 2.337.0,
`src/Runner.Common/Constants.cs`, `src/Runner.Worker/ActionManager.cs`). For
`uses: <owner>/<repo>/<path>@<sha>` the worker looks for
`<cache>/<owner>_<repo>/<sha>.tar.gz`, copies it into the job's temp directory
and extracts it exactly as it would a download — no symlink, nothing written
into the cache, upstream's entrypoint and command untouched. The cache lives at
`/opt/actions-archive-cache` for the same reason the tool cache lives in
`/opt`: anything baked under `/home/runner/_work` is hidden by the volume ARC
mounts over it. The key is the sha the Actions service resolves, which is the
pin itself — every `uses:` in this repository is sha-pinned, so a tag would
silently miss the cache and the generator refuses one.

`baked-actions.txt` is **generated**, never hand-kept — an image baking a set
the workflows no longer ask for looks healthy and quietly downloads again:

```sh
bash scripts/ci/baked-actions-manifest.sh > ci/arc-runner/baked-actions.txt
```

`internal/ciguard` runs that generator against `.github/workflows` and fails
when the committed manifest differs, holds the cache out of `_work`, and drives
`scripts/ci/verify-baked-actions.sh` over crafted caches. The verifier also
runs inside the image, offline, as part of `verify.sh`: it proves every `uses:`
of the tree resolves from the baked archives (the archive exists, unpacks to
the one nested directory the runner requires, and carries the action's own
manifest at that subpath) and that the image baked from the manifest this tree
ships. The negative control is the README's usual kind: remove one archive, or
one line of the manifest, and the verification fails naming the action. The
whole set measured 27,515,970 bytes over 24 entries on 2026-10-29 — a rounding
error next to the baked Go and Node.

One honest residual: the image BUILD downloads those archives from the very
host that throttles, so a build started during a codeload squall fails (with
`curl --retry 5`) instead of shipping a cache with holes. That is the trade the
baking makes — one failure per build, loudly, against one download per action
per job, silently.

**Rolling it out** is tracked as iterion#2820, and needs the same two external
gestures as any change to this image, and `arc-runners` has no `syncPolicy.automated`: publish a validated
version from main, pin that version AND its digest in infra-apps, then sync
Argo by hand. Until that happens the jobs keep downloading their actions and
nothing regresses.

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

`race` and `mongo-conformance` moved on 2026-09-29 (#1955). `cloud-e2e` has
not, and the reason is still a measurement nobody has taken — the image's
docker access is not it (two sibling jobs use it). Take it before touching
anything:

```
gh workflow run arc-kind-probe.yml   # then read the run's summary
```

[`.github/workflows/arc-kind-probe.yml`](../../.github/workflows/arc-kind-probe.yml)
brings a kind cluster up on an `arc-runners` pod through the same pinned
action `cloud-e2e` uses, captures what kind wrote into the kubeconfig and what
the runner container can actually reach — including whether an egress proxy
takes a loopback API server with it, which `kubectl` would honour and the
docker CLI would not — and names the hypothesis those facts support
(`scripts/ci/arc-kind-probe.sh`, exit 0 only on `usable`). Its routing is a
plain label on purpose: a probe of the scale set must not follow the
`CI_SELF_HOSTED` lever away from it.
