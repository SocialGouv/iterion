# Cloud bot bundle snapshots

A cloud launch resolved through the server's bot authority freezes one bundle
collection before compilation: the parent workflow, its skills, prompts,
presets, devbox and helper files, plus every transitively named subbot bundle.
Child paths retain their relative layout. Child bundles resolve through the
same server authority (eligible team tier, platform override, baked catalog),
so a platform child override is visible at the same launch boundary as its
parent. No child or resource is resolved from the runner image for that run.

The launch and its queued redeliveries use these bytes even if a stored row is
edited or the runner catalog comes from a different release. A missing child,
unreadable file, symlink, escape from the collection, or size limit is an
explicit error; the runner never substitutes its own catalog. Collection size
is bounded to 32 MiB of file bytes/paths and 4,096 files. Generated directories
(`.git`, `.devbox`, `.iterion`, `node_modules`, `__pycache__`) and Go tests are
excluded. Binary contents and executable bits are preserved.

`BotBundle` carries the snapshot JSON and its SHA-256 digest. If it would make
the queue message too large, the publisher uses the existing out-of-band JSON
blob transport, under `ir/<run-id>-bundle-<digest>.json`; the workflow's IR
remains a separate object. A content-specific key prevents a resumed attempt
from overwriting the snapshot referenced by an older queued message. The runner
checks the digest and paths before materializing a private temporary collection,
then removes the entire collection when the attempt ends. Bundle prompts merge
into the serialized workflow AST, just as they do during server compilation, and
every `{{include}}` in a prompt — in `main.bot` or in `prompts/*.md` — is
resolved into the prompt body on the server: the AST that travels is
self-contained, so the runner compiles it without the files beside the source.
An inline source with an include and no bundle to resolve it from is refused at
publish rather than queued to die on the runner.

An explicit or automatic resume re-resolves and freezes the current collection,
following the existing resume contract for stored bots. An inline source supplied
on resume determines the child graph to capture. The existing workflow hash and
`force` checks remain in force; a snapshot is not a promise that resumes always
reuse the initial resources. Inline/loose launches that bypass catalog resolution
retain their existing behavior. Legacy queued messages without a snapshot retain
the old version-checked stored-bundle/catalog path.

## Deployment

The queue schema is **v13**. Older runners must reject this new intent, since
ignoring the payload would silently mix code versions again; new runners retain
the existing v10–v13 acceptance window. Follow the cloud runbook's rollout/epoch
procedure and align server and runner builds. A bundle snapshot fixes catalog
drift, not binary/DSL incompatibility between releases.

The filesystem/local runtime does not need a blob backend because it does not
cross the cloud queue. Cloud uses the existing Mongo/S3-backed `IRBlobStore`
transport, with immutable bundle keys. No per-replica registry or additional
database collection is introduced.

## Proof

- `TestSnapshotPreservesBundleCollection`: original child, skills/devbox, binary
  bytes and executable mode survive capture, subsequent source edits and decode.
- `TestSnapshotCatalogLaunchAndResume`: catalog launch freezes siblings; a new
  resume captures updated resources and an inline resume's new child graph.
- `TestSnapshotChildUsesServerAuthorityAndRefusesMissingChild`: server platform
  child overrides apply; unresolved children fail at launch.
- `TestSnapshotRunnerIgnoresDivergentCatalog`: the real runner executes the
  snapshotted child despite a different runner catalog, and refuses a missing
  child even when the runner image could supply one.
- `TestSnapshotOffloadIsImmutableAcrossResume`: large snapshots fit the queue
  through object references without overwriting the preceding attempt's bytes.

These are deterministic tests; a coordinated live server/runner rollout remains
the operational verification for a release carrying the schema bump.
