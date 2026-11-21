# Retained filesystem payload access

Migrated protocol-one payload clients keep a checked filesystem capability
for each complete operation. The selected path is an identity/cache key, never authority to reopen
a replacement directory. [Run birth](run-birth.md) defines the staged protocol
and its remaining activation gates; production admission is still closed.

## Operation lifetimes

Checked ancestors are opened relative to retained parents and matched to their
observed directory identity. Creation synchronizes each parent, including retry
through an existing directory. Constructors recheck cancellation after namespace
IO and close refused capabilities. File snapshots stage, sync and rename within
one retained parent, then synchronize that parent; failures are returned even
when rename already made new bytes visible.

The retained clients cover inboxes/counters, tags and Git metadata, diff blobs,
tool blobs, events/checkpoints, attempts, backend sessions, notes/plans,
artifacts/interactions, route decisions, logs, attachments, scratch banks, turns
and run-file readers. Composite lifetimes matter:

- Artifact fallback holds its directory through scan and decode; interaction
  CAS holds one through load, identity validation and answer publication.
- Attachment metadata/body Open, body/sidecar Write and listing's child reads
  use the same retained ancestry. Cleanup after a failed sidecar ignores the
  canceled context so the newly published body can still be removed.
- A turn operation owns a tree of checked node/iteration roots. Latest and
  AtIndex reuse that tree through their internal load/list helpers. Supersession
  validates the incumbent's four coordinates and uses the requested coordinates
  for IO. Unencodable incoming checkpoints are refused before supersession;
  message sidecars remain opaque bytes.
- Run-file readers retain their artifact root. Unix openat borrows a directory
  descriptor and refuses symlinks along the requested path. Listing keeps child
  roots while obtaining metadata; it may list a symlink but never descends into
  it. Tool names such as `part..name` remain legal.

Ordinary payload file opens are confined by `os.Root`, but may follow a final
symlink inside that root. They do not claim the run-file reader’s stricter
Unix NOFOLLOW policy.

Open descriptors may outlive a directory capability: they still refer to the
selected inode. Log creation and Append acknowledgements synchronize file and
parent; subsequent tee log lines gain no per-line fsync promise. Multi-file
operations are not transactions, and directory walks are not point-in-time
snapshots. Root Close errors are not uniformly propagated by deferred cleanup.

CLI run/resume and dispatcher logging pass the original invocation context to
`TeeRunLogContext`; a retained strict binding cannot adopt a newer birth. Strict
setup errors propagate before engine execution. For recognized filesystem IO
failures, historical invocations keep a per-run redactor fork of their existing
logger.
Unknown or semantic errors, invalid arguments and cancellation remain fatal;
all leaves of a joined error must be recognized IO for that fallback. Windows
recognizes an explicit set of Win32 IO codes alongside portable error mappings.
The context-free `TeeRunLog` API remains a historical compatibility wrapper.
This local tee qualification does not cover Mongo log streaming or brokers.

## Streaming and global maintenance

Strict attachment/scratch staging retains the source FD and its checked root
from creation through publication. Arbitrary readers run outside metadata
locks; after revalidation, only the owned file is copied through the retained
destination. Copy starts at offset zero, then file sync, cancellation check,
rename and parent sync precede success. The copy costs O(n) under the store
mutex. A two-phase copy would need separate deletion/recreation and cleanup
qualification; no size cap substitutes for that work. Legacy staging retains
its historical closed-temporary pathname rename and does not gain the strict
source-identity guarantee.

Global note-author erasure enumerates physical format/run/birth namespaces,
including retired or orphaned births without headers. Checked roots remain open
through each visit; namespace-directory symlinks and noncanonical format
directories are errors,
not silently skipped subjects. Completed siblings are not rolled back if a
later namespace fails. Recursive payload removals operate through their retained
parent and synchronize even an already-absent entry on retry.

## Boundary still open

For protocol one, `OpenRunFilesGrant` retains the original data binding and
checked directory until its owner closes the grant. `Bind` rejects another run
or birth before restoring the numeric format; `OpenRoot` returns an independently
owned duplicate after checking the original metadata. A grant is a data
capability, not execution authority. Closing it does not upload or finalize a run.
FS `EnsureRunFilesDir` uses the same checked creation and permission path but
returns an ordinary pathname after closing its temporary grant.

Protocol-one Mongo scratch selection includes numeric format, run ID and birth
beneath a physically disjoint sibling root. Managed creation retains checked
ancestors. `UploadRunFilesFrom` requires a grant issued by that Store, binds it
before selecting objects, and walks its retained root and child roots. Regular
files open with NOFOLLOW and NONBLOCK on Unix; a substituted FIFO cannot block
opening. Upload errors preserve source files for retry. Successful cleanup uses
the retained parent and compares the leaf identity before removal; this is not
an atomic inode comparison-and-swap against a same-UID host actor. Delete retains
the same namespace across remote cleanup. When its parent exists, an absent leaf still retains that
parent so a retry can synchronize an earlier removal.

The existing Ensure/Upload entry points, when serving protocol-one runs, acquire
only operation-scoped capabilities;
they cannot retain the directory selected by an earlier string-returning call.
Execution, managed import and final upload share the invocation's initial grant.
`BuildExecutor` transfers ownership only on successful construction; its failure
paths close the grant. The executor lends it to the engine and closes it after
handoff cleanup. In runview launches, CLI run/resume and the four subbot hosts
(runview, runner, dispatcher and CLI), owned executor cleanup runs while the
acquired run lock or lease is still held, before release or park. Pre-launch
failures close the owned executor; release of acquired ownership remains
deferred through cleanup, including a cleanup panic. Local close-once guards
prevent a second attempt during unwinding. A panic inside executor Close can
interrupt its internal cleanup, so this does not promise that every internal
resource closes. Caller-supplied CLI resume executors remain borrowed; these
qualified paths do not establish ownership rules for arbitrary custom hosts. The runner uploads before closing its owned executor, retaining
identity through `WithoutCancel` and the existing bounded upload budget. The
grant's `Bind` runs before upload dispatch, including when the caller context
has no binding; a conflicting binding refuses. Stores implementing only the
new upload interface work without implementing the legacy interface. A store
with neither interface still needs no upload.

Custom executors can lend the same capability through `RunFilesGrant()`. If
none is lent, the engine owns its fallback for the sandbox lifetime and closes
it on failure or after cleanup. That fallback is not a post-run upload grant;
a custom caller needing a later upload must retain its own initial grant.
Kubernetes readback duplicates a retained root instead of reopening `HostDir`,
including after a rename. Its cleanup context retains the invocation's values
while detaching cancellation. Docker checks host-path identity at its actual
bind boundary, before legacy path preparation could mutate a replacement.
This check does not pass a local descriptor to a remote Docker daemon.

A child adopting its parent's sandbox inherits the parent's artifact mount.
It cannot serve the child's grant, so protocol-one adoption is typed-refused
before adoption effects. Historical adoption and children taking their own
sandbox path retain their existing behavior.

This contract isolates distinct births. It does not add execution-epoch
fencing between resumes of one birth, or a terminal finalization receipt.
FS live reads, Mongo final upload visibility and existing terminal timing stay
unchanged. A same-UID actor controlling host directories can still redirect
path-bearing tools; identity checks at a driver boundary are not revocation.

Replacement, restart, cancellation, durability and refused-operation witnesses
are exercised with admission-only test overlays while the protocol is closed.
They do not authorize activation before the remaining cleanup, runtime,
reference/URL and cloud-twin obligations are satisfied.
