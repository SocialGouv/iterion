# Identity data lifecycle — audit of sign-ins and launches, retention, erasure

What the platform records about a person's account beyond the account row
itself, how long it keeps it, and how it goes away. The package invariants
of identity, auth and audit stay in
[tenancy-credentials-spend.md](tenancy-credentials-spend.md); this page is
the doctrine across them. Decision records:
[ADR-157](../../adr/157-authentication-events-join-the-audit-log.md).

## Sign-ins are audited at the HTTP site, one row per attempt

- Every handler of `pkg/server` that mints or refuses a session writes
  `auth.login.succeeded` or `auth.login.failed` through `auditLoginSucceeded`
  / `auditLoginFailed` (`audit_login.go`): password login, the forced
  rotation of a pending account, registration, both branches of the OIDC
  callback, the desktop ticket's redemption, the reset redemption and the
  self-service password change. `TestEverySessionMintingHandlerIsAudited`
  reads the session-minting methods of `auth.Service` from source and
  refuses a function that calls one without both events — a ninth mint
  path is red until it is audited or exempted by name with its reason.
- An OIDC callback refused before an identity resolves (expired state,
  failed exchange, IdP `error=`) writes no row — no subject; the `Warn` log
  is its trace. The login-CSRF guard (`agent_binding`) is recorded.
- `Refresh` is the exemption: ~100 rows per active user per day for a fact
  the session lineage already records.
- `/cli-auth` mints a personal access token, not a session: its row is
  `pat.created`.

## What a row carries, and what it never does

- A success is a tenant row keyed on the **org** the session is active in
  (never a team: the org purge sweeps `audit_events` by org id), actor = the
  account, `meta.method` (`password|github|google|oidc|org_sso|desktop`;
  `oidc` includes the deployment connector named `sso`, `org_sso` is an
  org's own IdP — `ssoLoginMethod`), `meta.flow` when the session is a side
  effect (`register`, `password_change`, `password_reset`),
  `meta.provider` (connector slug; a desktop ticket carries it to the
  redemption), `meta.team_id`. No active org → a platform row. Read at
  `GET /api/orgs/{id}/audit`, not the team route.
- A refusal is a **platform** row with no tenant, actor `anonymous` unless
  the route is authenticated, `meta.reason` typed (`unknown_account`,
  `bad_password`, `locked`, `disabled`, `password_change_required`,
  `link_required`, `sso_restricted`, `ticket_invalid`, `reset_token_invalid`,
  `agent_binding`, `password_changed_sign_in`, `error` …), never the
  message — the wire collapses several of these into one answer on purpose
  (`auth.ReasonOf` is the typed side). `target_id` is set only for an
  account the server resolved (`loginAttempt.account`), never from what
  the client typed.
- The identifier attempted is **never stored in clear**:
  `meta.subject_hash` = HMAC-SHA256 under `derivedKey(activeKID,
  "iterion/audit/subject/v1")`, 16 hex (`auth.Service.SubjectDigest`). A
  bare hash of an e-mail is a dictionary lookup; the pepper is the signing
  key. A key rotation starts a new series. No address → no digest.
- IP is `s.clientIP(r)`: a forwarded header only from a peer in
  `TrustedProxyCIDRs`, which no setting fills yet (#2673) — so today the
  peer's `ip:port`, the ingress pod's behind an ingress. User-agent is the
  request's.
- A 429 writes no row; the log adds no bound. Today's bounds: login and
  forced rotation per attempted address (burst 5); the per-IP tier is per
  connection until #2673; register burst 3; the OIDC callback none (an IdP
  round trip per row). ADR-157 lists them.

## Retention

- One TTL index on `audit_events.created_at`, 400 days
  (`audit.RetentionDays`), sign-in rows included; submitted to the DPO as
  one question with the backup retention. If the DPO asks for the CNIL's
  6–12 months on connection logs, the prepared change is a per-action
  `expires_at` — never a second collection.
- `sessions` keep their own IP/user-agent/lineage and their own purge
  (`DeleteExpired`); the audit row does not replace them.

## Launches — the second PR of ADR-157

- `run.launched`, one row per run, through one wrapper around
  `s.runs.Launch` on every launcher (API, studio, webhook, schedule, board
  dispatcher, pipeline admission), actor user | webhook | system; a test
  refuses a `Launch` outside the wrapper. To be written with that change.

## Erasure — to be completed by gap 3

- The account-erasure change scrubs `{meta.subject_hash, ip, user_agent}` of
  the rows whose actor or target is the erased account AND of the refusals
  by address, which carry no account id: matched on `SubjectDigest(email)`
  recomputed under every key still in the signing key set (a retired key's
  digest is unlinkable). One method of `audit.Store` (Memory and Mongo in
  lock-step); it also deletes the account's `sessions` rows (revocation
  alone keeps them). Section owned by that change.
