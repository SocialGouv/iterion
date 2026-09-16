# Connector identities across regeneration

`iterion connectors gen` maintains `identity.lock.yaml` beside `connector.yaml`.
Commit it with the generated package and authored overlay. The generator first
reconciles its proposed names against this history, then checks the effective
public names after applying the overlay to a separate copy. Generated `ops/`
files never contain the applied overlay.

For example, `GET /issues` owns `probe.issue.list_issues`. Adding an endpoint
that sorts earlier and derives that name gives the newcomer a deterministic
hash suffix. Changing the old endpoint's vendor `operationId` or tag keeps
its existing generated name. Removing it retains its reserved names; a later
endpoint cannot inherit them. The identity is the uppercase HTTP method and
the exact templated path, including parameter names.

The lock records:

- `operations`: method/path → generated id and every public id that identity
  has held. An explicit overlay rename may add an unused public name. An
  overlay cannot transfer a name already held by another method/path.
- `generated_auth`: a fingerprint → generated scheme id and its readable
  shape. The shape includes kind, placement, value prefix, OAuth endpoints
  and PKCE. Vendor labels, descriptions and advertised/default scopes do not
  determine identity. Header names compare case-insensitively. Internal
  operation security references follow any restored scheme ids.
- `public_auth`: effective scheme id → shape, after authored auth corrections.
  An overlay that changes an existing public id's placement or issuer is
  refused; introduce a new id for a different credential destination.

Two generated auth schemes with the same identity are ambiguous and refused.
The generator cannot infer that two indistinguishable declarations represent
different accounts. Give their semantics an explicit distinct representation
in the source before generating. This is separate from the existing
connection's pinned credential placement at runtime.

The lock's schema version is read before strict decoding. A future version
reports an upgrade remedy; malformed fields, duplicate keys, conflicting ids
and foreign connector identities fail before the package is written. This
lock belongs to the generation command; the low-level `gen.Generate` API
remains a stateless proposal of names. Library callers that regenerate a
published package must use `identity.Lock.Reconcile` and `ObservePublic` too.
An older CLI that predates this mechanism does not enforce the lock: use the
updated generator when regenerating a protected package.

## Existing packages and explicit migrations

When no lock exists but a generated package does, the command seeds the lock
from that existing package and its current overlay before reconciling the new
specification. Do not delete `ops/` first: doing so discards the only available
identity history. A directory containing only an overlay is a new package;
its first generation establishes the history.

The shipped Forgejo lock contains **503 operation identities**, matching the
package actually delivered on main and ADR-098's final measurement. The
earlier PoC's 506 count is historical. Adding its lock changes no generated
operation bytes or effective public names.

To rename a public operation, edit its overlay `id:` and regenerate. The
previous name stays reserved to that same HTTP identity. This is an explicit
breaking rename for callers of the old name, not an alias offered at runtime.
To change a canonical HTTP identity or intentionally transfer a retired name,
review and edit the lock entry alongside the relevant source/overlay and
caller migration. Removing a history entry deliberately removes that
protection; the diff must expose the decision. Do not recreate the whole lock
as a routine response to an identity conflict.

`--keep-overlay=false` is accepted when no authored overlay exists. With an
existing overlay, identity checking requires `--keep-overlay`; remove the
overlay explicitly if the intended package no longer uses it. Skipping its
identity check while leaving the same file active for runtime would protect
a different package from the one workflows execute.

## Writing and recovery

One advisory OS file lock serializes cooperating generation commands for a
destination. Its persistent sibling file is
`.<package>.connector-generation.lock`; its OS lock is released on exit. Do
not delete that file while a generator may hold it. This is a local tool lock,
with the same network-filesystem limitations as the existing store lock.

The complete replacement is staged in a sibling directory. Authored regular
files, directories and permission bits are preserved. Symlinks and other
non-regular entries are explicitly refused before copying; the standard
filesystem copy would otherwise follow a link and replace it with its target.
Validation and staging errors leave the existing package bytes intact.

Publishing uses two directory renames with rollback on a reported installation
error. It is **not** a crash-atomic directory exchange: concurrent readers may
see a short absence. A process death between renames can leave a sibling
`.<package>.connector-backup-*`. The next generation refuses and names it.
Inspect the destination, stage and backup; restore the backup when the new
destination was not installed, or retain the installed candidate and remove
the verified obsolete backup. Then regenerate. A backup is never silently
used as permission to discard the previous identity history.

This slice changes no DSL syntax, queue schema, cloud resolver, OAuth store or
runtime catalog. Cloud package snapshots, grants and action-attempt durability
remain later work in [#1072](https://github.com/SocialGouv/iterion/issues/1072).
