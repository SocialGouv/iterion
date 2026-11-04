# Account erasure — by anonymization

An account is erased by anonymization, never hard-deleted
([ADR-159](../../adr/159-identity-data-lifecycle-erasure-by-anonymization.md)):
its id stays the pseudonym every `owner_id`, `actor_id`, `target_id`,
`invited_by`, `created_by`, `submitted_by` and `reviewed_by` keeps naming —
and a run note's `author`, rewritten from the address; everything that
names or lets in the person goes. Operator
procedure: [cloud-admin-guide.md §1.11](../../cloud-admin-guide.md).

## The status

`identity.UserStatusErased` is terminal: only `identity.Store.MarkUserErased`
(the erasure's first write, address kept) and `EraseUser` (the
anonymization) write it. `PatchUser` refuses a patch naming it (`ErrErasedStatusReserved`)
and refuses every patch on an erased row, preconditions or not
(`UserPatch.refusedStatuses` always holds `erased`, in the Mongo filter
too) — the one choke point a re-enable, the bootstrap reconcile or a racing
sign-in meets. The console's `PATCH` answers `409 user_erased`. `UserStatus.SignInRefused` and
`SignInRefusingStatuses` are the one reading of "no sign-in on this status"
— disabled and erased together — and every gate in `pkg/auth` asks them
(`accountRefusal`, `sessionRefusal`, `ssoRefusal`, every `IfNotStatus`
precondition) and in `pkg/server` (`grantRefusal`): a new status that
refuses sign-ins joins there, not at a site. `auth.ErrAccountErased` wraps
`auth.ErrAccountDisabled`, so `errors.Is(err, ErrAccountDisabled)` holds
wherever it held, with a message that says no re-enable brings it back.

No membership lands on an erased account: `auth.Service` writes through
`grantGuard`, its identity store (also `Service.Store()`, the one
`pkg/server` writes through), which refuses `UpsertMembership` and
`UpsertOrgMembership` for an erased account before the write and reads the
status again after it — a grant that finds the mark then is taken back,
`ErrGrantRefused`, and its caller's undo runs. Only the erasure's own
`restoreOwnerships` passes, under `withErasureCompensation`.

The email becomes `identity.ErasedEmail(id)` — `erased-<id>@erased.invalid`
(RFC 2606 domain) — unique because the id is, so the unique index on
`users.email` admits every erasure; both stores refuse to CREATE an account
under `ErasedEmailDomain` (`ErrReservedEmail`), so no registration can take
a marker first.

## The seams, each half in its own store

| Store seam | Erasure method | What it does |
|---|---|---|
| `identity.Store` | `MarkUserErased(ctx, id)` | Status erased, everything else kept — the commit point no sign-in or patch passes. |
| `identity.Store` | `EraseUser(ctx, id)` | One write: marker email, status erased, `updated_at`; `name`, `password_hash`, `github_orgs`, `last_login_at`, lockout, defaults and super-admin flag unset (`eraseUserUpdate` ↔ `erasedUser`). A row already under its marker answers unchanged. |
| `identity.Store` | `DeleteInvitationsByEmail(ctx, email)` | The invitations addressed to the row's address, in every team, matched as `Invitation.AddressedTo` does (case, surrounding whitespace). |
| `identity.Store` | `DeleteInvitationsByInviter(ctx, userID)` | The invitations the account created (`invited_by`), in every team: accepting one reads nothing of its creator. |
| `auth.SessionStore` | `DeleteAccountSessions(ctx, userID)` | DELETES the rows (they carry the sign-in's IP and user agent); the account's revocation records stay. |
| `audit.Store` | `ScrubActor(ctx, userID, email)` | Unsets `ip`, `user_agent` and `meta.<ScrubbedMetaKeys>` (`email`, `subject_hash`) on every row whose `actor_id` is the account, and the meta keys alone on every other row whose `target_id` is the account or whose `meta.email` is its address (an admin's act on it, an invitation; their IP is the actor's and stays); ids, action and the rest of `meta` stay. A writer putting a direct identifier in `meta` uses one of those keys. |
| `secrets.ApiKeyStore`, `GenericSecretStore` | `DeleteByScopeUser(ctx, userID)` | Every user-scoped key or secret, in every tenant; team-wide rows never (an empty id is refused). |
| `store.RunNoteStore` | `ReattributeRunNotes(ctx, from, to)` | Every note authored by the address (case, blanks ignored), every run and tenant, rewritten to the id. |
| `configshare.Store` | `ScrubActor(ctx, actor)` | `source_ip` and `user_agent` cleared on every delivery of `ActorUser(id)`. |
| `webpush.SubscriptionStore` | `DeleteByUser(ctx, userID)` | Every browser push registration of the account. |
| `pat.Store` | `RevokeAllByUser` (existing) | Tokens revoked; rows kept (hash only) for the audit of the mint. |
| `secrets.OAuthStore`, `credpool.PledgeStore` | `ListByUser` + `Delete` (existing) | The person's own forfaits (owner key = the id, never `org:`/platform) and pool pledges. |

Memory and Mongo twins of each seam stay in lock-step through the shared
conformance suites (`runEraseSuite`, `runSessionStoreSuite`,
`runStoreSuite`'s "scrub", `runDeleteByUserSuite`); the Mongo legs run under
`ITERION_TEST_MONGO_URI`, and `TestEraseUserUpdateKeysMatchTheBSONTags`
checks the `$unset` keys against the bson tags without a server.

## The orchestration

`auth.Service.EraseUser(ctx, userID, EraseOptions)` runs the identity
half, in this order, every step idempotent so an interrupted erasure
completes on the next call: refuse the last owner of a shared org
(`ErrLastOrgOwner`; a personal org or one pending deletion does not
count); release the shared ownerships and recount — none left (a racing
erasure or demotion) puts them back and refuses; `MarkUserErased` (no
sign-in opens from here); mark the personal orgs for deletion (the nightly
`orgsweep` purge removes them with everything in them); record the account
revocation; sweep the invitations by address, the team and org
memberships and the SSO links (`sweepAccountGrants` — a shared ownership
granted since the first read is released with the same recount, and
refuses the erasure when none is left); run
`EraseOptions.BeforeAnonymize` with the address; anonymize the row; sweep
again (what a sign-in in flight wrote) and delete the sessions.
`EraseResult` carries the counts, `AlreadyErased` and `Marked` (answered
with an error too).

`pkg/server.handleAdminEraseUser` (`POST /api/admin/users/{id}/erase`,
super-admin, refused on the caller's own account) runs that, handing it as
`BeforeAnonymize` the stores it owns (`eraseAccountHoldings`): personal
access tokens, pledges then forfaits, personal keys and secrets, push
subscriptions, notes (through the run service, the store notes are
written to), config-share deliveries, audit rows (the refused sign-ins
by address found by `auth.Service.SubjectDigests`, ADR-157; scrubbed
twice — once, then again past the bound every background audit insert is
held to from its queueing (`waitOutAuditInserts`), so a line queued on any
replica before the first scrub cannot outlive the second) — then writes the one platform
audit line of the act, `user.erased`, under the admin's identity with the
counts, synchronously and detached from the request (a client gone does
not cancel it): the line a replay reads after a restoration, so its
loss answers `500` and a repeat writes it. Written too, `incomplete: true`,
when the request fails once `Marked`. A failure answers `500`; the request
is repeated, with the address still on the row when the failure came
before the anonymization — the studio's drawer reads such an account as
an incomplete erasure and keeps « Relancer l'effacement » on every erased
account.
`orgDeletionGrace` is the personal org's grace, the same as an admin's org
deletion. `auth.Service.CreateOrgFor` refuses an owner whose status
refuses sign-ins (`ErrGrantRefused`, 422).

## What stays, and why

The id, as a pseudonym, in every reference above — the studio's "launched
by", the audit's "who did this", the marketplace's hidden
`submitted_by`/`reviewed_by`. The runs, notes, secrets and objects the
account created inside its organizations: they belong to the tenant.
`memory_spaces` keyed by user id: left to the tenant under the pseudonym
(the retention gap revisits it). `notification_prefs`, `model_prefs`: a
choice keyed by (tenant, id), purged with the org. The audit TTL stays one 400-day index
(`audit.RetentionDays`), the question of a shorter retention for
connection events being the DPO's. Backups: propagation by expiry, no
re-erasure inside dumps, erasures replayed after a restoration from the
`user.erased` lines read on the LIVE deployment (restore drill, step 6),
with the DPO's register.

Witnesses: `pkg/identity/erase_conformance_test.go`,
`pkg/auth/erase_test.go` (mutations: drop the session deletion, the
recount, the second sweep; move the status after the hook),
`pkg/server/admin_users_erase_test.go`.
