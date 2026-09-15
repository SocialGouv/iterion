# ARC CI runner with a C compiler

SocialGouv/iterion#981 distinguishes two failures: the race detector really
needs a C compiler, while early Mongo service initialization raced dind startup.
This image solves the compiler part. It preserves the upstream runner user,
groups, entrypoint and command; Docker access is covered by the separate dind
gate in SocialGouv/infra-apps#56.

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
devbox run -- bash ci/arc-runner/verify.sh iterion-ci-runner:test
```

The verifier mounts the pinned Go toolchain read-only and compiles a real cgo
call to libc with `go test -race`, as UID 1001, with no network and a read-only
root filesystem. It uses the image's gcc/headers, not the developer's. The
upstream image is the negative control. The final image includes no Go SDK;
workflows retain their existing setup-go step.

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
