# ADR-157: Sign-ins join the audit log — platform-scoped refusals, a peppered subject, no refresh row

- **Status**: Accepted — the operator picked this option on 2026-11-03 (DA ↔ repository gap 1, "option A: sign-ins and run launches, two PRs"); this record is the ADR of the first PR, the sign-ins. The run launches follow in their own change and amend this record.
- **Date**: 2026-11-03
- **Authors**: devthejo (session with Claude), for the MTSSF security qualification of iterion
- **Landed by this change**: `auth.login.succeeded` / `auth.login.failed` written by `auditLoginSucceeded` / `auditLoginFailed` ([`pkg/server/audit_login.go`](../../pkg/server/audit_login.go)) at every HTTP site that mints or refuses a session; the explicit-actor writer `auditWriteActor` and the single insert `auditInsert` ([`pkg/server/audit_helper.go`](../../pkg/server/audit_helper.go)); the typed refusal reasons and the peppered digest `Service.SubjectDigest` ([`pkg/auth/login_audit.go`](../../pkg/auth/login_audit.go)). Witnesses: `pkg/server/audit_login_test.go` (one scenario per site) and `pkg/server/audit_login_sites_test.go` (the choke point).
- **Relates to**: [cloud-admin-guide.md §1.5](../cloud-admin-guide.md) (the operator's view of the log) · [identity-data-lifecycle.md](../agents/engine/identity-data-lifecycle.md) (the doctrine page) · [ADR-048](048-org-team-hierarchy.md) (the org a row is keyed on) · the security qualification note (DA cadre 4, Traçabilité 3)

## Context

The audit log (`pkg/audit`) records control-plane mutations — org and team
lifecycle, members, secrets, webhooks, tokens — with actor, IP, user-agent
and a 400-day TTL, readable by org admins (`/api/{teams,orgs}/{id}/audit`)
and platform admins (`/api/admin/audit`). It recorded no sign-in: a
connection, accepted or refused, left only `users.last_login_at`, the
lockout counter, the `sessions` row of an accepted one and a Prometheus
counter. The deployment's own checklist asked for login coverage
([cloud-public-exposure-checklist.md](../cloud-public-exposure-checklist.md))
and the security qualification rates traceability on "who did what, from
where, when": the one scenario it retains — a diverted agent spending a
forge token — is reconstructed from "who signed in" and "who launched this
run". The first half is this record.

Seven things had to be decided, and each is a contract a later reader may
want to argue with.

## Decision

**One event per attempt, at the HTTP site.** Every handler that mints or
refuses a session writes `auth.login.succeeded` or `auth.login.failed`:
password login, the forced rotation of a pending account, registration, the
browser and desktop branches of the OIDC callback, the desktop ticket's
redemption, the password-reset redemption and the self-service password
change. The site is the HTTP handler and not `commitLogin` in `pkg/auth`,
because IP, user-agent, the method and the typed refusal coexist only there;
a refusal never reaches `commitLogin`. What keeps a ninth site from drifting
is `TestEverySessionMintingHandlerIsAudited`: it reads the session-minting
methods of `auth.Service` from the source of `pkg/auth` and refuses any
function of `pkg/server` that calls one without writing both events. An
OIDC callback refused before an identity resolves — an expired or replayed
state, a failed code exchange, an error the IdP returned — writes no row:
it names no subject and no account, and the server's `Warn` log is its
trace. The one such refusal that is recorded is the login-CSRF guard
firing (the user-agent binding cookie disagrees with the flow's state,
reason `agent_binding`), because it is an attack signal rather than a
failed round trip.

**A success is a tenant row on the ORG the session is active in** — never
on a team: the org purge sweeps `audit_events` by org id, and a row keyed on
a team would survive the org. Org admins read it at
`GET /api/orgs/{id}/audit`. The actor is the account; the row carries the
method (`password|github|google|oidc|org_sso|desktop` — `oidc` is every
deployment-wide connector but the two built-in ones, the one the deployment
names `sso` included, and `org_sso` an org's own IdP), the flow when the
session is the side effect of another act (`register`, `password_change`,
`password_reset`), the connector slug — a desktop sign-in's too, carried by
its ticket from the callback to the redemption — and the active team. An
account with no active org writes a platform row rather than a tenant row
nobody can list.

**A refusal is a platform row with no tenant.** A refused attempt resolved
no session and may name no account, so there is no org to attribute it to,
and a tenant row on a guessed org would let an org admin read the attempts
of addresses that are not their members. The reason is typed
(`unknown_account`, `bad_password`, `locked`, `disabled`,
`password_change_required`, `link_required`, `sso_restricted`,
`ticket_invalid`, …), never the error message: the wire deliberately
collapses several of these into one answer, and the audit trail — platform
admins only — is where they are told apart. The actor is the request's
identity when the route is authenticated (the self-service password change
proves the current password from a live session) and `anonymous` otherwise.
The target is that identity, or the account the SERVER resolved for the
attempt — a desktop ticket's account, the account whose new password was
committed without a session (`password_changed_sign_in`, written beside the
`user.password_reset` / `user.password_changed` row the change owes) — and
none for an attempt by address: what a client typed is never promoted to an
account id.

**The identifier attempted is never stored in clear.** A refusal carries
`meta.subject_hash`: HMAC-SHA256 of the normalised address under a key
derived from the active signing key with the label
`iterion/audit/subject/v1` (`JWTSigner.derivedKey`), truncated to 16 hex
characters. An e-mail address is enumerable, so a bare SHA-256 — truncated
or not — is reversible by dictionary; the pepper is what makes the digest
useless to a reader of the collection or of a dump. The digest correlates
the attempts of one campaign on one address and nothing else; a key
rotation starts a new series, which the trail tolerates. Where the attempt
names no address (a reset link, a desktop ticket) the row carries no
digest. A success carries no digest either: its actor is the account.

**A refresh is not audited.** The access token expires every 15 minutes, so
a refresh row per rotation is ~100 rows per active user per day for a fact
the session lineage already records (`sessions.rotated_from`, `lineage`,
the rotation's user-agent and IP). `Refresh` is the one session-minting
method the choke-point test exempts, by name and with its reason.

**The flood is bounded by the limiter, not by the log — and the limiter
is weaker than its name today.** One refusal is one row, and a throttled
request (429) reaches no sign-in and writes no row; the log adds no bound
of its own. What the limiter bounds depends on the route, and on the
client identity it keys on. That identity is `clientIP`, which reads a
forwarded header only from a peer in `TrustedProxyCIDRs` — a field no chart
value or environment variable sets yet — and otherwise answers
`r.RemoteAddr` with its ephemeral port: the per-IP bucket is per TCP
connection, and behind an ingress every row records the ingress pod's
address and port. The edge-trust change (#2673) makes it a port-free,
per-client address under an operator setting; until it lands:

- `POST /api/auth/login` and `POST /api/auth/password/change` also limit
  per attempted address (burst 5, 5/min): one address cannot be hammered
  faster, but attempts spread over many addresses and fresh connections are
  unbounded, one 400-day platform row each;
- `POST /api/auth/password/reset/confirm` and `POST /api/auth/desktop/exchange`
  have the per-IP bucket only (burst 5, 5/min);
- `POST /api/auth/register` has its own per-IP bucket (burst 3);
- the OIDC callback has no limiter: each refusal row costs a full IdP
  round trip, which is the bound.

The insert is detached and best-effort, as every audit write is, so a store
that lags slows no sign-in.
`TestAuditLogin_TheAddressTierBoundsAFloodOnOneAddressInTheShippedConfiguration`
witnesses the per-address tier in the configuration that ships (no trusted
proxy, one fresh connection per attempt); the per-client bound is #2673's
to witness.

**One TTL, 400 days, submitted to the DPO.** The collection has one TTL
index on `created_at`; sign-in rows share it. The CNIL recommends six to
twelve months for connection logs, so this retention is above the
recommendation and is submitted to the DPO as one question with the backup
retention. If refused, a per-action `expires_at` is the prepared change
(half a day); the single index is not a doctrine.

## Options considered

- *A hook in `commitLogin`* (`LoginRecorder` beside `TeamGrantSweep`): one
  site for every success, but refusals never reach it, IP and user-agent
  would travel through the context, and the method would have to be added to
  `loginContext` or passed by seven callers. Kept as the fallback if a mint
  path appears that no HTTP handler owns.
- *A bare truncated SHA-256 of the address*: rejected — an e-mail address is
  enumerable (gap 2's doctrine: a bare hash is for CSPRNG tokens of ≥128
  bits, never for a human input).
- *Storing nothing of the subject*: acceptable, and what the row does where
  the derivation is unavailable; but a campaign on one address is then
  invisible, which is the one question a brute-force investigation asks.
- *Auditing the refresh*: rejected on volume for a fact already recorded.
- *A tenant row for a refusal of an org SSO provider* (the tenant is known):
  rejected — one rule for every refusal, or two readers argue about the
  third case.

## Consequences

- `/api/admin/audit` without `?action=` lists more rows; the studio's audit
  tab needs no change (free-text filter on the action).
- The `sessions` collection keeps its own trace (IP, user-agent, lineage)
  and its own purge; the audit row does not replace it.
- `audit.Event.ActorKind` gains `anonymous` for a refused anonymous attempt.
- Run launches (`run.launched`, one row per run, actor user | webhook |
  system) are the second PR of the same decision; it amends this record.
- The account-erasure change (gap 3) must scrub `{meta.subject_hash, ip,
  user_agent}` of two sets of rows, in `audit.Store`, Memory and Mongo in
  lock-step: those whose actor or target is the erased account, and the
  refusals that carry no account id — every attempt by address — found by
  recomputing `SubjectDigest(email)` under every key still in the signing
  key set. A digest under a retired key is unlinkable to the address
  without that key, which is what erasure asks.
