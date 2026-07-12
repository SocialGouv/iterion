# ADR-068 — Mid-run file-secret refresh for sandboxed runs

- Status: accepted
- Date: 2026-07-12
- Deciders: jo (direction), Claude (analysis + implementation)

## Context

#99 fixed a real correctness bug: a workflow's `as: file` secret (e.g. a
GitHub App installation token, which lives ~1h) is a **launch-time
snapshot**, so a long run that pushes/comments after the token expires
uses a dead credential. The fix re-reads the store record on a cadence
and rewrites the materialised file, so `cat <path>` at use time reads a
live token.

But that refresh loop starts **only for the no-sandbox in-pod path**
(`pkg/runner/loop.go`): it keys on `materializeFileSecretsNoSandbox`,
which returns `nil` whenever the run resolves to an active sandbox
(docker or k8s). A sandbox driver mounts the secret at container start —
docker as a host **bind-mount**, k8s as a mounted **Secret** — and the
loop never fires. So a long `sandbox: auto` run (the **common cloud
case** — the whole Vetty/Billy dogfood fleet runs sandboxed) still
pushes with a dead token: #99, still open for the path most runs take.

The two drivers mount the secret through very different mechanisms, and
the credential material (sealed generic secret → plaintext, store
access, tenant scope, sealer) lives **runner-side**, not in the sandbox
package. So the refresh needs runner-owned re-reads driving a
driver-specific propagation, across a layer boundary — the runner starts
the engine, and the engine (not the runner) creates and owns the sandbox
`Run`.

Alternatives considered:

1. **Pre-materialise secrets to a runner-controlled host path and have
   the sandbox bind-mount that** (so the runner owns the file it later
   rewrites). Rejected: it only helps docker (k8s pods have no host
   filesystem to bind), and it inverts the existing spec model where the
   driver owns materialisation — a larger, driver-asymmetric change.
2. **Run the refresh loop inside the runtime/sandbox layer**, which holds
   the `Run` and the host paths. Rejected: that layer has no access to
   the generic-secret store, the sealer, or the tenant identity — all
   runner config. Threading those down inverts the dependency direction
   and coupling the runtime to secret storage.
3. **Expose a driver-agnostic refresh operation on the sandbox `Run`, and
   let the runner drive it** via an engine callback. Chosen.

## Decision

Add an optional `sandbox.SecretFileRefresher` interface —
`RefreshSecretFile(ctx, name, value)` — implemented per driver:

- **docker** — record each mounted file secret's host bind-mount source
  at `Start`. On refresh, rewrite it: a directory-mounted secret (the
  default `/run/iterion/secrets/*` case) is replaced atomically via
  temp-write + rename inside the mounted dir; a single-file custom mount
  is rewritten **in place** (a rename would swap the container-pinned
  inode). Docker bind-mounts follow the host inode, so a subsequent
  in-container `cat` reads the new value.
- **kubernetes** — re-apply the per-run Secret with the rotated key's
  value updated, keeping an ordered snapshot so a later refresh of a
  different key doesn't revert an earlier one. kubelet propagates the
  Secret update to the projected volume within ~1min.

The engine gains `WithSandboxRunObserver(func(sandbox.Run))`, invoked with
the live `Run` right after start (nil default = no-op, so non-cloud
engines are unchanged). The runner registers an observer that, when the
run has refreshable file secrets, spawns a refresh loop (the sandboxed
counterpart of `refreshFileSecretsLoop`) re-reading each secret's store
record on the same 5-min cadence and handing rotations to the driver's
`SecretFileRefresher`. Reads stay tenant-scoped; the value is never
logged; failures are loud and retried next tick, leaving the last good
value in place.

## Consequences

- A sandboxed run that converges >1h after launch pushes/comments with a
  **live** token — #99 now holds for the common cloud path.
- The refresh contract (cadence, tenant scoping, no-value-logging,
  loud-failure) is shared with the no-sandbox path via `readFreshSecret`;
  the propagation mechanism is the only per-driver difference.
- **k8s custom mount paths are a known gap**: a file secret with an
  absolute `mount_path` outside `/run/iterion/secrets` is projected via
  `subPath`, and kubelet does **not** auto-update subPath mounts. The
  Secret is still refreshed, but that projection stays stale until pod
  restart. The default (directory-mounted) secrets — including the
  `forge_token` case this ADR targets — do update. Tracked as a board
  finding; closing it needs a mount-model change (project the custom
  path without subPath, or a sidecar re-reader).
- A driver that doesn't implement `SecretFileRefresher` degrades loudly
  (a warning that a long run may push with a stale token) rather than
  silently — the noop driver rejects file secrets outright, so this only
  ever affects a future driver.
