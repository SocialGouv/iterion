# ADR-159: Identity data lifecycle — an account is erased by anonymization, its id stays a pseudonym

- **Status**: Accepted — the operator's arbitration of 2026-11-03 on the
  DA↔repository gap 3 ("anonymization, administration only; self-service
  after the terms of service"); this record is the ADR, the change that
  carries it is the implementation. The number is provisional until the
  final rebase.
- **Date**: 2026-11-03
- **Authors**: devthejo (session with Claude), for the security
  qualification note's gap 3 (right to erasure, GDPR art. 17)
- **Landed by this change**: `identity.UserStatusErased`,
  `identity.Store.MarkUserErased` / `EraseUser` / `DeleteInvitationsByEmail` /
  `DeleteInvitationsByInviter`,
  `auth.Service.EraseUser`, `auth.SessionStore.DeleteAccountSessions`,
  `audit.Store.ScrubActor`, `webpush.SubscriptionStore.DeleteByUser`,
  `secrets.ApiKeyStore` / `GenericSecretStore.DeleteByScopeUser`,
  `store.RunNoteStore.ReattributeRunNotes`, `configshare.Store.ScrubActor`,
  `auth.ErrGrantRefused`, `POST /api/admin/users/{id}/erase`,
  `iterion remote admin users erase --yes`, the studio's "Effacer le compte
  (anonymisation)" action. Witnesses: `pkg/identity/erase_conformance_test.go`,
  `pkg/auth/erase_test.go`, the audit, session, secrets, run-note and
  config-share conformance suites, `pkg/server/admin_users_erase_test.go`.
- **Relates to**: [ADR-048](048-org-team-hierarchy.md) (the identity
  domain) · [cloud-admin-guide.md §1.11](../cloud-admin-guide.md) (the
  procedure) · [cloud-backup.md](../cloud-backup.md) (the remanence this
  record rules on) · the account-erasure leaf of
  [docs/agents/engine](../agents/engine/account-erasure.md)

## Context

Until this change nothing erased an account. Disabling one (`status:
"disabled"`) refused its sign-ins and ended its sessions while keeping
every datum: the row with its email, name, password hash and GitHub orgs;
the SSO links; the sessions, which carry the IP and user agent of each
sign-in; the memberships; the invitations addressed to it; the push
subscriptions; the person's own LLM subscription credentials (sealed
tokens, the provider account's address), credential-pool pledges and
user-scoped API keys and secrets — which a resumed run of theirs is still
sealed with; the run notes, whose author defaults to the writer's address;
the config-share edits of their config-editor sessions, with IP and user
agent; the audit rows naming it, with their IP, user agent and the
addresses some actions put in `meta.email`. The right to erasure (GDPR
art. 17) had no path, and the qualification note said so.

The account's id, on the other hand, is woven through the data the
platform keeps for other reasons: `runs.owner_id`, `audit_events.actor_id`
and `target_id` (a 400-day compliance trail),
`memberships.invited_by`, `invitations.invited_by`, every `created_by` of
a secret, key, binding, bot source, plugin source, schedule, webhook or
forge connection, `marketplace.submitted_by` and `reviewed_by`. Deleting
the row would leave those references pointing at nothing — the studio's
"launched by", the audit's "who did this".

## Options considered

1. **Hard delete of the `users` row.** Removes the most. Breaks every
   referential read above and loses the audit's proof of who acted (the
   trail the DPO and an incident review expect). Both options free the
   address: a later registration under it is a new account either way,
   and neither links it to the erased one.
2. **Anonymization: the row stays under a marker, everything that names or
   lets in the person goes** (chosen). The id — a UUID that identifies nobody
   without the row — stays a pseudonym every reference keeps naming;
   every direct identifier is removed or replaced.
3. **Self-service `DELETE /api/me` beside the admin route.** Deferred: it
   needs terms of service and a privacy policy that name it (gap 9), and a
   rule for an account that owns organizations; the admin route is the one
   a DPO request reaches today.

## Decision

1. **An account is erased by anonymization, by a super-admin**, through
   `POST /api/admin/users/{id}/erase` (studio: the account file's
   "Effacer le compte (anonymisation)"; CLI: `remote admin users erase
   --yes`, refused without the flag).
   The caller's own account is refused: an erasure is a request another
   person acts on.
2. **What goes.** The `users` row keeps its `_id`, `created_at` and the
   write's `updated_at`; its `email` becomes `erased-<id>@erased.invalid`
   — unique by construction, under a domain RFC 2606 reserves and both
   stores refuse at account creation (`identity.ErrReservedEmail`) — and
   its status becomes `erased`, a terminal status only `Store.MarkUserErased`
   (the erasure's first write, which keeps the address) and
   `Store.EraseUser` (the anonymization) write — `Store.PatchUser` refuses a patch that names it
   (`identity.ErrErasedStatusReserved`) and refuses every patch on an
   erased row whatever its preconditions (`ErrUserStatusChanged`), in the
   same filter as the write, so a re-enable, a bootstrap reconcile or a
   sign-in racing the erasure cannot revive it; `name`, `password_hash`, `github_orgs`, `last_login_at`, the
   lockout pair, the default team and org and the super-admin flag are
   unset. Every `oidc_links` row of the account, every team and org
   membership, every invitation addressed to its address (matched as
   `Invitation.AddressedTo` does) and every invitation it created
   (`invited_by`; accepting one reads nothing of its creator, so it would
   still seat its invitee — an org offboarding revokes them too), every
   `push_subscriptions` row go. The
   person's own credentials go: every `oauth_credentials` record under
   their personal owner key (never an org's or the platform's), every
   `cred_pledges` row, every user-scoped `api_keys` and `generic_secrets`
   row in every team (`DeleteByScopeUser`) — a resumed run of the
   pseudonym's resolves its owner's credentials, and the refresh worker
   renews every record, so a credential kept would keep serving. Every
   `run_notes.author` that is the account's address — the author a note
   defaults to — becomes the account's id. Every
   `config_share_deliveries` row of the account's config-editor sessions
   (`actor: user:<id>`) loses its `source_ip` and `user_agent`. Every
   `sessions` row of the account is **deleted**, not revoked — a session
   row carries the IP and user agent of its sign-in; the account's
   revocation records (`session_revocations`, ids and a generation) stay,
   and the one landed before the delete keeps refusing an access token
   minted before it. Every personal access token is revoked (rows kept for
   the audit of the mint, hash only). The account's **personal
   organization** is marked for deletion and the nightly purge removes it
   with everything in it.
3. **What stays, as a pseudonym.** The id, wherever it is referenced:
   `owner_id`, `actor_id`, `target_id`, `invited_by`, `created_by`,
   `submitted_by`, `reviewed_by`, a run note's `author` once rewritten,
   a config-share delivery's `actor`. The marketplace's public view
   already hides `submitted_by` / `reviewed_by` (`ForViewer`); an erased
   submitter's id is a pseudonym there too. A note author typed by hand
   (the route accepts one) is free text the erasure does not read.
4. **The audit log is scrubbed, not cut.** `audit.Store.ScrubActor`
   removes `ip`, `user_agent` and the `audit.ScrubbedMetaKeys` of `meta`
   (`email`, `subject_hash`) from every row whose `actor_id` is the
   account — the IP and user agent are the person's — and from the
   refused sign-ins [ADR-157](157-authentication-events-join-the-audit-log.md)
   records: an attempt with no actor whose `target_id` is the account, and
   an attempt by address naming no account, found by its
   `meta.subject_hash` among the address's digests under every key still
   in the signing key set (`auth.Service.SubjectDigests`); and the
   `ScrubbedMetaKeys` alone from every other row whose `target_id` is the
   account or whose `meta.email` is its address (case and surrounding
   blanks ignored): an administrator's act on the account, an invitation
   addressed to it. The IP and user agent of those rows are their actor's
   and stay, as the forensic trace of that act.
   Both twins (Memory and Mongo) run one conformance suite. The ids, the
   action and the rest of `meta` stay: the row still proves what was
   done. A writer that puts a new
   direct identifier in `meta` uses a key in `ScrubbedMetaKeys`, not one
   of its own. Audit lines are inserted in the background, on every
   replica, each bounded from its queueing (5 s): a line naming the person
   queued before the scrub could land after it, keyed by an invitation id
   and an address the row no longer carries. So the erasure scrubs, waits
   out that bound (plus a margin), and scrubs again while it still knows
   the address — which holds across replicas, sharing only the store. The erasure itself is one
   platform audit line, `user.erased`, under the admin's identity,
   carrying the counts of what was removed — written synchronously, since
   a replay after a restoration reads it: its loss answers `500`, and the
   repeat (idempotent) writes it. Its insert is detached from the request,
   under the same bound: an admin's client gone once the account carries
   the erased status takes nothing of the line with it.
5. **Refusals.** An account that is the last owner of a shared (non
   personal, not pending deletion) organization is refused (`409
   last_org_owner`, naming the organization): ownership is transferred or
   the organization deleted first — never an orphaned organization. The
   refusal holds under a race: the erasure releases its ownership first
   and recounts the owners once its membership is gone, so of two
   erasures (or an erasure and a demotion) of the last two owners, the one
   whose write lands second sees none left, puts its membership back and
   is refused. An ownership granted DURING the erasure — an organization
   the account creates in a request that read it before the mark — is
   recounted the same way by the sweep that finds it: none left, the
   membership is put back and the erasure refused before the
   anonymization, naming the organization (the account stays marked; the
   repeat goes through the transfer or the deletion like any last owner). An erased account admits no console write (`409
   user_erased`), no password reset (`422`), no org or team grant
   (`422`), owns no new organization (`422`, `auth.ErrGrantRefused`,
   checked in `auth.Service.CreateOrgFor`, the one gate of the console and
   of self-service), and refuses every sign-in, refresh, switch and credential
   derivation as a disabled one does (`auth.ErrAccountErased` *is*
   `auth.ErrAccountDisabled`, with its own message).
6. **Idempotent, resumable, closed from the first write.** Every step is
   idempotent and ordered so an erasure a failure interrupts completes on
   the next request: the refusals; the release of shared ownerships under
   the recount; the **erased status, address kept** — the commit point
   past which no sign-in, refresh, switch or patch opens on the account,
   so nothing below is re-created by the person and a failure leaves no
   account that can sign in with its grants half gone; the personal org's
   mark, the revocation record, the invitations, the memberships, the
   links; the server's half (`auth.EraseOptions.BeforeAnonymize`: personal
   access tokens, credentials, push subscriptions, notes, config-share
   deliveries, audit scrub) while the row still carries the address the
   invitation, note and audit steps match on; the anonymization; a second
   sweep of the links, memberships and invitations — what a sign-in that
   read the account before the status and wrote after the first sweep left
   — and the deletion of the session rows. A failure answers `500`; the
   request is repeated, and finds the address again when the failure came
   before the anonymization (`erased` status, address still on the row). A
   second erasure answers `200` with `already_erased: true` and zero
   counts.
7. **One audit retention, submitted to the DPO.** The audit TTL stays the
   single 400-day index (`audit.RetentionDays`): the row an erasure
   scrubs expires with every other. Whether connection events need a
   shorter retention (the CNIL's 6–12 months for connection logs) is the
   one question put to the DPO with this record; a per-action
   `expires_at` is the prepared answer if the single index is refused.
8. **Remanence in backups.** A dump or a mirror taken before an erasure
   holds the data erased after it, for as long as the backup's own
   retention. The doctrine: **propagation by expiry** — the erasure
   reaches the backups when they age out, which [cloud-backup.md](../cloud-backup.md)
   bounds; **no re-erasure inside dumps** (a dump is immutable evidence
   of a point in time; editing one would forge it); **after a restoration,
   the erasures recorded since the restored point are replayed**. The
   restored audit log ends at the restored point, so the list to replay is
   read on the LIVE deployment — the `user.erased` lines since the dump
   (`GET /api/admin/audit?action=user.erased&from=`),
   kept with the DPO's request register of decision 9, which is the
   authoritative list. The line is written as soon as the account carries
   the erased status, with `incomplete: true` when the request failed past
   it, so an act begun is on the list too. The route is idempotent: the
   replay is one request per account. The restoration drill
   ([cloud-backup.md](../cloud-backup.md#restore-drill), step 6) carries
   it, with every other action recorded since the dump; when the live side
   is lost too, the register alone names the erasures to ask again.
9. **The manual procedure, until self-service exists.** A request reaches
   the DPO or the platform operator; the operator verifies the identity
   of the requester out of band, runs the erasure (studio, CLI or API),
   and keeps the `user.erased` audit line as the trace of the act, with
   the request's reference in the ticket that tracked it. Self-service
   follows the terms of service and the privacy policy (gap 9).

## Consequences

- The DA's retention line can say "accounts: disabled on request, erased
  by anonymization through the administrator" with a mechanism behind it.
- `identity.UserStatus.SignInRefused` and `SignInRefusingStatuses` are the
  one reading of "no sign-in on this status": the `erased` status is
  refused at every site the `disabled` one is, in `pkg/auth` and
  `pkg/server`, and a future status joins there, not at N sites.
- An access token already minted is stateless until its own expiry (at
  most the access TTL), as after a disable; what reads the account —
  a mint, a switch, a refresh, a WebSocket ticket — refuses it at once.
  A write that would store personal data again under the pseudonym reads
  the account too: a push subscription is refused for an account that
  refuses sign-ins, checked before and after its write (one landing behind
  the erasure's deletion is taken back), and an invitation to an address
  whose account is under erasure is refused the same way, so none outlives
  the sweep by address that a retry, once the row is anonymized, could no
  longer run. Every team and org membership write of `pkg/auth` and
  `pkg/server` goes through the service's identity store, which refuses
  one for an erased account before the write and reads the status again
  after it (`auth.grantGuard`): a grant that finds the mark then is taken
  back and its caller compensates what it provisioned around it (an
  organization created across the mark is deleted). One of the two reads
  — the grant's after its write, the sweep's after the mark — always sees
  the other write.
- Orphan references are not a new class: the id still resolves to a row,
  whose email reads as a marker. A console or a report that renders an
  account by email shows `erased-<id>@erased.invalid`; one that wants
  prose renders the `erased` status.
- What this record does not erase, deliberately: the runs, notes, team
  secrets and objects the account created inside its organizations, which
  belong to the tenant and outlive the person; `memory_spaces` keyed by
  user id, left to the tenant with the same pseudonym (to revisit with
  the retention gap); `notification_prefs` and `model_prefs`, a scope
  and a model choice keyed by (tenant, user id), no address in them,
  removed with their organization's purge.
- A sign-in that read the account before the erased status landed and
  writes after the second sweep — a request in flight across the whole
  erasure — can still leave a link or a session row; repeating the
  erasure removes it.
- Landed beside it, the sibling change of gap 3 (#3004): an
  organization's purge deletes every run with its S3 objects, and its
  audit rows under both keys (org id and team ids).

## Witnesses

- `pkg/identity/erase_conformance_test.go` — the row after `EraseUser` on
  both twins, `MarkUserErased` then `EraseUser`, the marker, the reserved
  domain, the invitations by address, and the `$unset` keys against the
  bson tags without a Mongo, on a seed whose every `User` field is set (a
  new field reddens until the erasure names it).
- `pkg/auth/erase_test.go` — collection by collection on the memory twins,
  the last-owner refusal (and under a racing release: drop the recount and
  it reddens), sessions deleted not marked, idempotence, the server's half
  run before the row loses its address and not at all on a refusal, a
  failed hook leaving an erased row no password or refresh opens (move the
  status after the hook and it reddens), the sign-ins and grants written
  during the erasure refused or swept (drop the second sweep and it
  reddens).
- `pkg/auth/session_store_conformance_test.go` ("deleting an account's
  sessions…"), `pkg/audit/audit_test.go` ("scrub"),
  `pkg/usernotify/webpush/store_test.go`,
  `pkg/secrets/scope_user_delete_test.go`, `pkg/store/notes_test.go` and
  `pkg/store/mongo/notes_test.go`, `pkg/configshare/scrub_test.go` — both
  twins; the Mongo legs run in the CI `mongo-conformance` job.
- `pkg/identity/erase_conformance_test.go` ("no patch reaches an erased
  row…") and `user_patch_mongo_test.go` — the terminal status on both
  twins and in the filter Mongo is asked.
- `pkg/server/admin_users_erase_test.go` — the route end to end: self,
  last owner, 404, the stores (credentials, notes, config-share edits
  included), the audit line, the second erasure at zero counts, the
  refused console writes and org creation, the `incomplete` line of an
  erasure that fails past the status.
