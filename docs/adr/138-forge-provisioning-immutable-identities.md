# ADR-138: immutable webhook identities and hybrid reconciliation

Status: accepted. Scope: forge-write-integrity (#2452, #2475).

## Decision

Provisioning remains a synchronous operator action, with durable retry state.
A shared `pkg/lease` lease per `(tenant, connection, repo)` reduces overlap;
`ProvisionLeaseTTL` overrides its one-minute default. Expiry never proves that
a remote HTTP request stopped. A local optimistic version cannot fence an API
that does not accept conditional writes.

Each webhook epoch therefore has one immutable secret, sealed on its durable
integration reservation before any config or hook write. Every retry of that
epoch sends that same secret. `HookSpec.Version` identifies the local attempt;
providers do not pretend to enforce it remotely. Both config and integration
snapshot writes compare their version atomically. A rejected settings restore
cannot overwrite a later operator PATCH. Memory stores own deep snapshots;
Mongo preserves independently stamped usage/sync metadata in the atomic write.

An existing config whose secret is not recoverable migrates through a new
candidate webhook ID and URL. The previous hook and its verification material
remain unchanged until the integration CAS selects the new active ID. Inbound
admission checks that authoritative integration, not just `Config.Enabled`.
Candidate-only reservations and retired rows are hidden from operational
integration reads. Lifecycle reads retain them for retry and disconnect.

The migration source is frozen through its config version before the candidate
is built. Ordinary PATCHes conflict during handover instead of succeeding on a
policy the activation would discard. Retry the migration, then edit the new
active endpoint. Retirement can disable a frozen endpoint through an
authority-scoped update that changes no policy fields.

Disable retires the integration before remote cleanup. Its verification configs
and monotonic integration version remain as tombstones. Re-enable waits for
cleanup to finish, then reserves a distinct webhook ID and secret. Cleanup
deletes captured schedule identities, never successor schedules selected after
a slow remote call. Project-bot rotation records its result through a separate
credential-identity CAS so an unrelated operator edit cannot lose a token the
forge has already issued.

Schedule projection runs under the integration version: a mutex in memory, a
conditional integration write and schedule batch in the same Mongo transaction.
The projection advances the version. Retirement captures its rows under the
retired version before slow remote cleanup, so neither a late projection nor
a late cleanup can replace a successor's schedules. Rotation compensates an
unrecorded issued token on both successful and failed secret writes when its
owning integration has retired or changed credential identity.

All forge write redirects (301, 302, 303, 307, 308) are refused before Go can
rewrite the method or replay the body. The typed error names the status and
sanitized target. OAuth and manifest exchanges redact their known credentials;
hook provisioning redacts its secret. GET/HEAD retain their client's redirect
policy. A redirect is an **indeterminate** original write: a proxy can apply
the first request before returning it. Retry state and verification material
therefore survive.

## Hybrid reconciler boundary

The synchronous path reserves, attempts, and activates. Re-running the same
request repairs a pending reservation; disconnect includes pending credentials.
This change does not install a periodic provisioning worker.

A future elected sweep must use the same identity/CAS seams. It must persist
the complete desired request (including operator overrides and schedule
choices), inspect both active and pending epochs, and retry uncertain creates
by their exact URL. Its garbage collector must retain retired epoch URLs and
captured hook IDs, because an old remote create can finish after local cleanup.
It must never delete all hooks for a repo or infer that a timeout prevented a
write. Finalization must compare the authoritative integration version.

Until that worker ships, abandoned candidates and late duplicate/orphan hooks
can require an operator retry or forge-side cleanup. URL/event settings written
late within the same epoch may drift until retry; their immutable secret still
verifies. There is no claim of linearizable remote settings or automatic garbage
collection. An already admitted delivery is not cancelled by later retirement.

## Proofs

Deterministic tests hold the first hook write, expire its lease, complete a
successor, then let the first write apply last. The oracle verifies the forge's
actual final secret against the stored hash, for fresh and existing hooks.
Other tests cover legacy migration, retired admission, stale operator settings,
credential recovery, and memory/Mongo version parity. Redirect matrices cover
every write method and redirect status, including missing or malformed Location.
