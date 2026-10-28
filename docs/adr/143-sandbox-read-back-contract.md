# ADR-143: A sandbox path the run reads back is declared, served by storage the driver owns, and drained at teardown

- **Status**: Accepted
- **Date**: 2026-10-28
- **Authors**: devthejo (session with Claude), operator-arbitrated — the #2668 lane, arbitration of 2026-10-28: "2668.3 = ADR d'abord, le canari RÉÉCRIT pour le nouveau contrat"
- **Landed by this change**: the contract and the rewritten canary. Owed by the slices behind it: the `WorkspaceExporter` widening, the kubernetes drain, the collector, and the `pkg/store/mongo` twin — in that order, each with its own witness.
- **Relates to**: [082-sandbox-by-default.md](082-sandbox-by-default.md) and [031-sandbox-one-long-lived-sleep-infinity-container-per-run.md](031-sandbox-one-long-lived-sleep-infinity-container-per-run.md) (this names a new clause of the driver contract they set) · [sandbox.md](../sandbox.md#a-promise-the-driver-drops-takes-its-variable-with-it) (its "Known gap — run-files on pods" is what this closes) · [attachments.md](../attachments.md) (`required=true`, the loud half of #2668)

## Context

The runtime hands a container paths to write into — the run-files
directory an in-sandbox tool drops artifacts in
(`ITERION_ARTIFACT_FILES_DIR`), the attachments directory, the bundle.
Every one of them is a HOST BIND, and a driver with no host filesystem
(kubernetes) drops the lot. The rule that followed is sound and was paid
for twice: **a path the runtime NAMES to the container is served by a
mount the driver keeps** — when the bind goes, the promise goes with it,
and the variable stays honestly unset rather than naming a directory
nobody reads (#815: a gate wrapper redirecting its report into it died
on "Directory nonexistent", and four lots read an oracle verdict out of
an environment failure).

Honest, and empty. On a pod the artifact-files panel has nothing in it,
`deliverables:` has nowhere to collect from, and the collector reads a
host directory the pod never wrote to. The loss is now at least
VISIBLE (`run_attachment_lost`, 503d9917e) and fatal when the author
asks for it (`attachment=… required=true`), but a loud loss is still a
loss: no mechanism carries a pod's bytes back.

What is missing is not a bind. It is the OTHER half of the promise: the
run does not only need the path to exist while the container runs, it
needs the CONTENT afterwards, when the pod is gone.

## Decision

1. **A read-back path is DECLARED, not inferred.** The spec carries the
   container paths whose content the run must recover, as data —
   alongside the mounts, never derived from them. A driver that cannot
   serve one refuses at admission with its own error, the way the
   kubernetes driver already refuses `type=bind`; it never serves it
   silently half-way.

2. **The driver serves it with storage it owns.** Where a host
   filesystem exists the bind is that storage and nothing changes. On
   kubernetes it is an `emptyDir` at the declared container path: the
   pod writes to a real directory for its whole life, and the path the
   runtime names is served by a mount the driver KEEPS. The promise rule
   is therefore **generalized, not weakened** — it was never about
   bind-ness, it was about a kept mount — and the implementation already
   reads that way (`dropUnsupportedHostBinds` keys on the surviving
   mounts, not on who set the variable).

3. **The driver DRAINS it at teardown, into the run's file store.**
   That drain is the second clause of the promise, and the one that
   makes naming the path legal on a pod: the variable is set *because*
   the content is read back, not merely because a directory exists.
   `WorkspaceExporter` is widened past the workspace to carry it — one
   capability exporting a declared SET of paths — rather than a second,
   parallel export seam (modularity: the next variant would need an
   engine `if`).

4. **A drain that fails is an event, never an empty set.** The precedent
   is exact and already in the tree: `sandbox_workspace_export_failed`
   says "the copy failed, the run's work is about to be lost", non-fatal
   and visible. A read-back that returns nothing because the drain
   failed must be distinguishable from one that returns nothing because
   the tool wrote nothing — a reader cannot tell those apart from an
   empty list, and #2668 is the ticket about exactly that confusion.

5. **The durable state ships its cloud twin in the same slice**
   (cloud-native rule 3): the drained set lands through the run file
   store, so the `pkg/store/mongo` twin is part of the slice that
   introduces it, not a follow-up.

6. **The canary is rewritten, never removed.**
   `TestSandboxSpec_NoPromiseSurvivesTheBindThatServedIt`
   ([pkg/runtime](../../pkg/runtime/sandbox_bind_promise_canary_test.go))
   encoded "no promise survives a dropped bind". Under this contract the
   rule it pins is the generalized one — *named ⇒ served by a mount the
   driver keeps* — plus its converse for the read-back path: the
   run-files variable is named EXACTLY when a kept mount serves it. Both
   directions are measured on the real wiring, so the seam landing turns
   the canary green on the new behaviour without an edit, and a variable
   naming a path no mount serves still fails in CI rather than in a
   campaign.

## What is not promised

- **A pod that dies before teardown.** A drain runs at teardown; a pod
  OOM-killed or evicted mid-run takes its emptyDir with it. Bytes that
  must survive an abrupt end are published as the run goes
  (`attachment=`, which persists each file as it is declared), not left
  for the drain. `required=true` makes that choice loud rather than
  silent, which is why it lands first.
- **Unbounded size.** An emptyDir spends the node's ephemeral storage
  and the drain spends the store's: the read-back set is capped, and the
  cap is the slice's to name and to meter — never "whatever the pod
  wrote".
- **Streaming.** The content is readable after teardown, not live
  during the run. A panel that wants progress watches events, which is
  what events are for.
- **Host-path semantics.** The declared path is a CONTAINER path. The
  host directory a bind-capable driver happens to use is an
  implementation detail of that driver, and no consumer may read the
  drained set by host path (the collector's current shape, `pkg/runner`
  reading `RunFilesHostDir`, is exactly what the collector slice
  replaces).
- **The attachments and bundle directories.** They are binds with their
  own contracts (one is written by the host and read by the container,
  the other is provisioning input). This contract is about paths whose
  content travels the other way; widening it to them is a separate
  decision.

## Consequences

- `ITERION_ARTIFACT_FILES_DIR` becomes set on a pod run once the seam
  lands — the point of the change, and a behaviour change for any bot
  that branches on its absence to pick a temp dir. The fallback stays
  legal: a tool that finds the variable set writes there, and one that
  does not keeps its own directory.
- The "Known gap — run-files on pods" paragraph in
  [sandbox.md](../sandbox.md) is replaced by this contract when the
  seam lands, not before: until the drain exists, the honest statement
  is still that nothing collects a pod's run files.
- The `deliverables:` axis (#2690) and rgaa-audit's outputs (#2701)
  stop depending on a host filesystem, which is what made them
  cloud-only promises on paper.
- One seam, one shape, three sandboxes: a capability wired for the
  kubernetes driver is wired, or typed-refused, for docker and
  podman — the backend-parity rule applied to sandboxes.
