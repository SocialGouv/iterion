# ADR-150: attachments reach cloud runs — a staging seam on the store, a copy where no bind exists, a typed refusal where neither

Status: accepted (ARB-111, session-vagues46, 2026-10-29)
Date: 2026-10-29
Issue: SocialGouv/iterion#2086

## Context

An attachment is a file the operator gives a run: picked in the launch
form, uploaded as it is picked, named by the launch, promoted to the run,
bind-mounted read-only into the sandbox at `/run/iterion/attachments`,
and handed to nodes as `{{attachments.<name>.path}}`. Every link of that
chain was written for a server and a runner that share a filesystem. In
a cloud deployment they share nothing, and the chain broke twice, on two
independent links, with two different kinds of silence:

- **The upload answered 503.** The server staged an upload in a directory
  under the run store's root — `<root>/uploads/<upload_id>/` — and the
  cloud store has no root by construction. `POST /api/runs/uploads`
  refused with "store has no filesystem root"; the launch form could not
  attach anything.
- **The sandbox dropped the bind and said so to nobody who reads runs.**
  The kubernetes driver has no host filesystem, so the runtime removed
  every host bind mount it had declared — the attachments among them —
  with a warning on the runner's log and nothing on the run's stream. The
  nodes were then handed an empty path (a store with no root has no host
  path to offer either), a tool ran `cat ""`, an agent was told a file was
  at "", and both improvised. The ticket's own probe read ABSENT/ABSENT
  on an up-to-date cloud instance.

Three outcomes were on the table for the second gap, and the ticket named
the worst: *the silent skip*. A run that goes on without what it was given
reads, afterwards, as a run that never had it.

## Options considered

**Where the staging seam lives.**

- *A — methods on `store.RunStore`.* Every implementation and every test
  fake grows four methods it may not want. Rejected: staging is a
  capability a store has or has not, exactly like the run-files area.
- *B — an optional narrow interface, `store.StagingStore`, probed with
  `store.AsStagingStore` (the `AsRunFilesStore` pattern).* The filesystem
  store keeps its directory layout — its tests and its reaper unchanged in
  substance — and the Mongo store gains a twin. The server never touches a
  disk for uploads; a store that stages nothing is refused typed.
  **Chosen.**
- *C — stage through the artifact API (runner → server).* Covers a tool
  producing a file during a run, not the launch-time upload that was dead.
  Rejected for this ticket.

**What a dropped bind owes the run.**

- *Keep warning on the runner's log.* Rejected: measured twice already
  (#815, #834) as the shape of incident nobody can diagnose from the run.
- *Refuse every run that holds attachments on a driver with no host
  filesystem.* Rejected: it would also refuse the consumers a URL serves —
  vision inlining in cloud reads the presigned URL today and works.
- *A run event for every drop; a typed refusal when the run holds
  attachments and the driver can neither bind nor copy them; a typed
  node failure when a node names a path the run cannot hand it.*
  **Chosen.** The refusal is the Nth-variant guard: once the copy seam
  below lands, no shipped driver falls in it (docker binds, kubernetes
  copies, noop is the host).

**How a driver with no host filesystem receives the files.**

- *Hand the nodes the URL only.* Rejected: a tool that needs a file gets a
  URL it must fetch with credentials it does not have.
- *A capability and a seed list — `sandbox.Capabilities.SupportsHostDirCopy`
  and `sandbox.RunInfo.SeedDirs`: directories the driver copies into the
  container at start, after the workspace, the way kubernetes already
  copies the workspace (tar over `kubectl exec`).* The runtime seeds only
  what it cannot bind: a driver that binds, binds; one that copies, copies;
  one that does neither meets the refusal above. **Chosen.**
- *Where the bytes come from on a store with no root:* the engine
  materialises the run's attachments into a per-run temporary directory
  before the sandbox starts (list, open, write, read-only files), removes it
  with the sandbox, and fails the setup typed when a fetch fails. The
  attachment path handed to nodes is then the container path when the
  sandbox mounted or copied the directory, the local copy when the run
  executes in the runner's own process (cloud `sandbox: none`), and the
  URL alone otherwise.

## Decision

1. **`store.StagingStore`** is the seam between the upload and the run:
   `StageUpload`, `OpenStagedUpload`, `DiscardStagedUpload`,
   `ReapStagedUploads`. `FilesystemRunStore` implements it under
   `<root>/uploads/<upload_id>/{meta.json,<filename>}`. `mongo.Store`
   implements it with a record in the `staged_uploads` collection — `_id`
   the upload id, stamped with the tenant and the user that staged it,
   read under the tenant filter — and the bytes in the blob backend under
   **`uploads/<upload_id>/<filename>`** (`blob.Client.PutUpload`,
   `GetUpload`, `DeleteUpload`). Nothing presigns an upload; the record is
   the only way to its bytes. The reaper runs on every replica as a
   platform sweep (no tenant), deletes the bytes before the record, and
   there is no Mongo TTL index — it would drop the record and leak the
   bytes. Upload ids carry 128 bits of randomness under the `up_<unix>_`
   prefix clients already treat as opaque. The wire format of
   `POST /api/runs/uploads` is unchanged.
2. **Every dropped host bind is a run event**, `sandbox_host_binds_dropped`
   (driver, the targets the dropped binds served, the variables withdrawn
   with them, any value handed back).
3. **A run that holds attachments on a driver that can neither bind nor
   copy them is refused** at sandbox start, before any node:
   `ATTACHMENTS_UNDELIVERABLE`, said on the run's stream as
   `sandbox_skipped` with `refused: true` and the names.
4. **A node that names `{{attachments.<name>.path}}` (or the bare form)
   while the run holds the attachment with no path it can open fails
   before it runs**, `ATTACHMENT_PATH_UNAVAILABLE`, at the one boundary
   every execution path crosses (`executeWithResources`). `.url` serves
   anywhere; an attachment the run does not hold still renders empty — a
   declared optional attachment is the bot's to test for.
5. **`sandbox.Capabilities.SupportsHostDirCopy` and
   `sandbox.RunInfo.SeedDirs`** carry the attachments to a driver with no
   host filesystem. The kubernetes driver declares the capability, mounts
   one `emptyDir` per seed, streams the directory in with the workspace's
   tar pipeline under the workspace copy's budget, and makes the files
   read-only (the directory itself stays writable: an `emptyDir` is
   root-owned and group-writable through `fsGroup`, and the sandbox user
   cannot change its mode). On a store with no root the engine
   materialises the attachments first, so the same seed serves cloud.

## Consequences

- Attachments work in a cloud deployment on both paths the ticket named,
  with one contract for the two store twins (the `StagingStore` case of
  the shared conformance suite runs against both).
- A new blob family, `uploads/<upload_id>/`, keyed by something other
  than a run id: documented in the public-exposure checklist with its
  scoping rule (the tenant-filtered record), and in the backup page as
  transient. A new collection, `staged_uploads`, exempt from the per-run
  sweep guard because no run owns it.
- Two failure codes join the reserved set; one event type joins the
  sandbox family; the studio's hints and event union carry them.
- The runtime's attachments directory is a fact in one more case: a
  copied directory counts as landed, so `attachmentPath` keys off it as it
  keys off a bind.
- What is deliberately left: the resume path's forecast of a local copy
  (before materialisation it answers the URL, as before), and read-only
  on the pod being per-file rather than per-mount.

## Witnesses

`pkg/runtime`: `TestSandboxRefusesARunWhoseAttachmentsNoDriverCanDeliver`,
`TestANodeNamingAnAttachmentPathRunsWithAFileOrNotAtAll`,
`TestFinalizeMountsForDriver`. `pkg/store/storetest`: the `StagingStore`
case, run by the filesystem suite and the Mongo suite. `pkg/store/mongo`:
`TestStagedUploadsAreScopedToTheTenantThatStagedThem` (a real replica
set). `pkg/store/blob`: the prefix-guard sweeps, `DeleteUpload` among them.
`pkg/server`: `TestUploadAttachment_StagesThroughTheStoreWhenItHasNoFilesystemRoot`,
`TestUploadAttachment_IsRefusedTypedWhenTheStoreStagesNothing`.
`pkg/sandbox/kubernetes` and `pkg/runtime`: the seed copy and the
materialisation witnesses of the third unit. Each saw its defect put back
and reddened on its own assertion.
