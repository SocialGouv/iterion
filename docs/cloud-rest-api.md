# Cloud REST API reference

**Audience.** Anyone calling iterion programmatically — a CI job, an
SDK author, an operator writing curl runbooks. Every endpoint listed
here exists in [pkg/server/](../pkg/server/), grouped by domain.

Diagram: [diagrams/access-control.md](diagrams/access-control.md) — the access-control matrix (principals, roles, the authorization decision points).

This page is **curated, not exhaustive**: it covers the cloud- and
team-facing surface, and leaves out the local-studio-only routes
(`/api/local/*`, `/api/files`, `/api/projects`, `/api/examples`,
`/api/filesystem/*`) along with much of the run-console read surface.
A few team-facing families have their own reference instead of a table
here: `/api/admin/llm/*` ([cloud-llm-credentials.md](cloud-llm-credentials.md)),
`/api/admin/settings/usage-caps` ([usage-caps.md](usage-caps.md)),
the notification routes ([notifications.md](notifications.md)), the
config-share routes ([config-share.md](config-share.md)), and
`/api/teams/{id}/plugin-sources*` ([plugins.md](plugins.md)).
For the complete inventory of the build you are actually running, use
the generated spec — every route is recorded by the server's
`recordingMux`, so it cannot drift:

```bash
iterion openapi                 # OpenAPI 3.1 for this binary, offline, to stdout
curl .../api/openapi.json       # the same, from a live instance
curl .../api/routes             # just the method+pattern inventory
```

(The `/api/v1/native`, `/api/v1/dispatcher`, `/api/v1/mcp/board` and
`/api/v1/mcp/runs` sub-trees register on the *same* mux but bypass the
route recorder — they are handed the bare `ServeMux` rather than the recording
wrapper — so they are deliberately absent from that spec, and so are the three
run-token forge routes (`POST /api/v1/forge/publish-review`,
`GET /api/v1/forge/pull-request`, `POST /api/v1/forge/delivery-preflight`).
The exception worth knowing:
the forge-facing `/api/v1/native/issues/*` routes and the dependency
graph go through the recorded path and therefore **do** appear.)

Authentication. Most routes accept any of:

- **Cookie**: `__Host-iterion_auth` (access JWT) + `__Host-iterion_refresh`
  (rotation). The `__Host-` prefix is dropped — leaving the bare
  `iterion_auth` / `iterion_refresh` — when the deployment cannot meet its
  terms (no TLS, or an explicit cookie domain); a browser would discard the
  prefixed form there. A deployment reads only the spelling it sets.
- **Bearer JWT**: `Authorization: Bearer <access-jwt>` issued by login
  / refresh.
- **Bearer PAT**: `Authorization: Bearer iap_…` — long-lived personal
  access token; authenticates **as** the issuing user with that user's
  role + super-admin flag
  ([pkg/server/pat_routes.go:identityFromPAT](../pkg/server/pat_routes.go)).
  Up to four rows are resolved on every request — the token, its owner,
  the owner's membership of the token's team, and that team (a
  super-admin's token with no team scope reads the first two). A refusal
  answers `401` with its reason: no such token or one that does not verify,
  a revoked or expired token, an owner gone or not active, no team scope, a
  membership or team gone; a row the store could not read answers
  `500 {"error":"identity unavailable"}` with the cause logged, so a
  client never throws away a live credential over a store outage.
- **WS ticket**: `?ticket=<t>` on `/api/ws[/…]` and on a run's
  `browser/cdp` — a single-use ticket minted by an authenticated
  `POST /api/ws/ticket` from a live access token or a PAT (a ticket
  presented there mints nothing: `403`), valid 60 seconds and never past
  the credential that minted it; it is resolved and consumed before any
  JWT/PAT verification, and its redemption checks that credential again —
  past its expiry, once its session is signed out, its account disabled or
  the personal access token that minted it revoked, the ticket is refused.
  A ticket the store refuses — unknown, expired or already redeemed —
  answers `401 {"error":"invalid or expired ws ticket"}` and never falls
  through to the other credentials; a redemption the store could not
  complete answers `500 {"error":"identity unavailable"}` with the cause
  logged, like a PAT row the server could not read.
- **WS query**: `?t=<access-jwt>` — or an `iap_` PAT — on `/api/ws` and `/api/ws/*` alone, for a WebSocket client
  that can set neither header nor cookie.

Where a route says "team member", "team admin" or "super-admin", the
guard maps to `canViewTeam` / `canManageTeam` / `requireSuperAdmin`.
**"team admin" therefore means admin/owner of that team OR an admin/owner
of its ORG** — `canManageTeam` carries that third arm everywhere, reads
included. On the routes that LAUNCH a run — `POST /api/runs`,
`POST /api/runs/{id}/resume`, `POST /api/v1/triggers/emit`,
`POST /api/v1/pipeline-board/tasks/{id}/launch` — "member" is enforced by
the launch gate's own rung
([pkg/server/launch_gate.go](../pkg/server/launch_gate.go), `gateLaunch`),
which refuses a `viewer` and the orthogonal `config_editor` capability with
`member_role_required` (403); the run-addressed mutations additionally pass
the ADR-103 ladder. The same rung guards every state-changing board and
trigger route at its mount (`boardWriteAuth`, `triggerWriteAuth`), because
a card written ready or a subscription installed enabled is a launch the
platform performs later.
Webhook delivery URLs (`POST /api/webhooks/<provider>/<id>`) use their
own auth (token bearer or HMAC body signature) and are public to the
JWT layer.

## Authentication + identity

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `POST` | `/api/auth/login` | public | Email + password login |
| `POST` | `/api/auth/refresh` | refresh cookie or `X-Iterion-Refresh` | Rotate access JWT |
| `POST` | `/api/auth/logout` | public | Sign the presented refresh token's session out + clear the cookies; `?lineage=<id>` names the session meant (another one is left signed in, `204` with `X-Iterion-Logout: other-session`) |
| `POST` | `/api/auth/register` | public (when `signup_mode=open` or with invite) | Create account |
| `POST` | `/api/auth/password/change` | public (legacy) | First-login password rotation for `pending_password_change` users (`409` below) |
| `POST` | `/api/auth/password/reset/request` | public | Mint + email a reset token (always 200, no enumeration) |
| `POST` | `/api/auth/password/reset/confirm` | public | Redeem `iar_…`, set new password, issue fresh session (`409` below) |
| `GET` | `/api/auth/providers` | public | List configured OIDC connectors + `signup_mode` |
| `GET` | `/api/auth/oidc/{provider}/start` | public | Start OIDC dance |
| `GET` | `/api/auth/oidc/{provider}/callback` | public | OIDC redirect target |
| `GET` | `/api/auth/invitations/lookup` | public | Resolve invitation token → email + team |
| `POST` | `/api/auth/invitations/accept` | session JWT (never a PAT) | Accept an invitation while logged in. The path is public to the middleware, so the handler verifies the cookie or `Bearer` JWT itself — an `iap_…` PAT never resolves here |
| `GET` | `/api/auth/me` | authenticated (no role) | Current user, org→teams tree and active context, and `session_lineage` — the session the access token belongs to (absent for a PAT) — with `expires_at`, that session's own bound (a pending sign-out's record expires at it; absent when the session is gone). `/api/auth/*` is tenant-free, so an account with no team at all reads it |
| `POST` | `/api/auth/me/team/{team_id}` | member, session access token (a PAT answers `403`) | Switch active team |
| `POST` | `/api/auth/me/org/{org_id}` | member, session access token (a PAT answers `403`) | Switch active org (re-issues the JWT; validates org then team) |
| `POST` | `/api/me/password` | member, a live session (below); a personal access token answers `403` | Self-service password change (`409` below); personal access tokens and connected SSO identities are kept. The current-password proof is throttled like the sign-in (`429`) and feeds the account's lockout |
| `POST` | `/api/me/sessions/revoke-all` | member, a live session (below) | Sign out every device, revoke every personal access token of the account and remove every SSO identity a signed-in session connected (the one the account was created or first signed in with stays; a password-less account whose every remaining identity was session-written keeps its oldest). A cleanup step failing after the record answers `503 signout_cleanup_failed` — sign in again and repeat |

Source: [pkg/server/auth_routes.go](../pkg/server/auth_routes.go) +
[pkg/server/password_routes.go](../pkg/server/password_routes.go).

The active team rides the access credential, and the credential lives in a
browser-wide cookie — every tab of a browser shares it. A caller that needs
its requests to act on another team of its own standing — a second studio
tab that kept its team while the cookie moved, a CLI whose base URL is
assembled for it — sends `X-Iterion-Team: <team_id>` on any tenant-scoped
route (where a route also reads `?team_id=`, the query keeps precedence).
The header re-scopes the request to that team at the auth choke point: the
standing it costs is the one a team switch would demand (the role is
recomputed in the named team, so the header never carries the active team's
role across), a team the caller has no standing in answers `403` naming the
header, routes that scope by the team in their path ignore it (the path is
the more local scope), run-addressed routes ignore it (ADR-103: a run by id
is served from the run's team), and a credential without an active team is
not rescued by one. Absent, it changes nothing.

Every route that mints a credential — login, register, refresh, the
password change and reset confirm, the desktop exchange, the team and org
switches — answers the session alone (`AuthSessionResponse`: `user`, the
`active_*` context, `expires_at` — the access credential's own expiry, not
`GET /api/auth/me`'s session bound — and `access_token` for a non-browser
client). The OIDC callback mints one too and answers a `302`: in a browser
it sets the session cookies; for a desktop SSO it sets none and redirects a
single-use ticket to the desktop's loopback, which the desktop redeems at
`POST /api/auth/desktop/exchange`. Go's redirect writes a short HTML link
as the body of either. None answers the org tree: `GET
/api/auth/me` is the one route that builds it, and when it cannot read the
tree — one org's teams included — it answers `500 {"error":"identity
unavailable"}`, never an empty or partial tree. Every 5xx a store or
server failure causes on the routes of this section — `GET
/api/auth/oidc/github/callback/orgs` included, the forge feature that lives
under the auth prefix for its redirect URI, its upstream GitHub exchange
too — and on `GET` and `POST /api/teams` and `GET /api/teams/{id}/members`
below carries that same body, and the cause goes to the server log; a
deployment with no auth service answers `auth not configured`. A refusal
keeps its own status and message. On `GET /api/teams/{id}/members` and
`POST /api/teams` it rests only on rows the store returned — the member
list's `403 not a member`, the team creation's `403 org admin or owner
required` — and a row the store could not read answers the 500 above. The
SSO routes refuse on a failed read too, and say so in their flow
([flow-auth-login.md](diagrams/flow-auth-login.md)): `GET
/api/auth/oidc/{provider}/start` answers `404 unknown provider` whatever
stopped the provider's resolution, and its callback redirects with
`unknown_provider` likewise, or with `state_expired` when the sign-in state
cannot be taken. Any
authenticated route reached with an `iap_` PAT answers that same 500 when
one of the rows the middleware resolves for it — the token, its owner, the
owner's membership of the token's team, and that team — cannot be read. The team-management routes below keep 500 bodies that name the row a
failure may have left standing.

> **Migration (since the release that includes #1668).** The nine
> credential routes — `POST /api/auth/login`, `POST /api/auth/register`,
> `POST /api/auth/refresh`, `POST /api/auth/password/change`,
> `POST /api/auth/password/reset/confirm`, `POST /api/auth/desktop/exchange`,
> `POST /api/me/password`, `POST /api/auth/me/team/{team_id}` and
> `POST /api/auth/me/org/{org_id}` — no longer carry `orgs`: a client that
> read the org→teams tree from their answer reads it from
> `GET /api/auth/me`.

`POST /api/auth/refresh` answers `401` when the presented credential is
absent or dead, with a body that names which: `no refresh token` (nothing
was presented, and the cookies are left alone), `auth: session revoked`,
`auth: session expired`, `auth: session not found` (an unknown token —
Mongo's TTL index reaps expired sessions — or, followed by `: its account
no longer exists`, a session whose account is gone), `auth: account
disabled` and `auth: session revoked: the account must change its
password` (an account an administrator reset or set pending a password
change); each of the last five clears the cookies. A `5xx` leaves the
cookies alone: the client keeps the token it presented and presents it
again, promptly — a failure before the rotation rotated nothing, and a
rotation whose successor was handed to nobody (never written, or written
and never claimed, whatever step failed after the rotation) is completed
by the token's next presentation, if it comes within five minutes of the
rotation (or within `ITERION_REFRESH_REUSE_GRACE` when that is longer) and
before the token's own expiry; later, the token reads as abandoned and its
presentation as reuse. A rotated token whose successor was handed out,
presented again within `ITERION_REFRESH_REUSE_GRACE` (30 s by default,
measured on the database's clock), receives the same successor — unless
that successor was itself rotated or revoked since, which is reuse. Reuse
revokes the token's lineage — every session rotated from the same login,
whoever holds it — and the account's other logins, its other devices,
keep their sessions; the reuse of a token whose session was retired before
lineages were recorded (by a release before #1668) names no lineage and
revokes every session of the account
([cloud-admin.md](cloud-admin.md#2-required-secrets-at-boot)). A reuse
whose revocation could not be completed answers `500` and keeps the
cookies: the token presented again is reuse again and retries it. A
sign-out (`POST /api/auth/logout`) revokes the presented token's lineage
the same way and answers `204`, clearing the cookies, and a later
presentation of that token is reuse; a sign-out whose session read or
revocation fails answers `500 {"error":"identity unavailable"}` and leaves
the cookies, for the client to present the token again. A sign-out may
name the session it means, `POST /api/auth/logout?lineage=<id>` with the
`session_lineage` `GET /api/auth/me` answered: a presented token of another
lineage — a session the client opened since — signs nothing out and keeps
its cookies, answering `204` with `X-Iterion-Logout: other-session`; every
other `204` means the presented session is signed out, or there was none.
"Sign out everywhere" (`POST /api/me/sessions/revoke-all`), a password
change (`POST /api/me/password`), a reset (`POST
/api/auth/password/reset/confirm` or an administrator's `POST
/api/admin/users/{id}/reset-password`) and the rotation an administrator
forces (`POST /api/auth/password/change`) end every session of the account
before the new credential lands — and the credential changes again once
it landed. A revocation answers from its record, the one write it waits
for; its pass over the live sessions runs after the answer. A failure
before the new credential is committed answers `500` and leaves the
password, and a reset's link, as they were — the sessions may already
have ended. A failure once the new password is committed answers `409
{"error":"password changed, but no session could be opened: sign in with
the new password","error_code":"password_changed_sign_in"}` and clears the
cookies: the password is changed and a reset's link spent, so presenting
the same request again answers `invalid credentials`. One credential
change runs at a time per account: a second waits up to 20 seconds, then
answers `409` with `error_code` `credential_change_busy`, nothing
changed. A commit whose answer is lost may have landed: the revocation
that follows a commit runs anyway, the request answers `500`, and a reset's
link stays spent. A change an administrator's disable lands under answers
the `409` above and opens no session. "Sign out everywhere" also revokes
every personal access token of the account and removes every SSO identity
a signed-in session connected (`GET /api/me/sso/{provider}/link/start`) —
the one the account was created or first signed in with through SSO stays;
a password change and the resets keep both. An administrator's disable
(`PATCH /api/admin/users/{id}`) ends the sessions, revokes the personal
access tokens and removes those SSO identities once the status landed, and
answers `500` with the account disabled when that revocation fails. The
refresh cookie's `Expires` is the session's expiry rounded up to the hour.

The team and org switches (`POST /api/auth/me/team/{team_id}`, `POST
/api/auth/me/org/{org_id}`) re-scope the presented access token without
extending it: the token they answer expires when the presented one does,
and names the same session (its `sid` and `sgen` claims). They answer
`401` once that session is signed out, the account signed out everywhere
(any of the revocations above), disabled (`auth: account disabled`) or
pending a password change, and `403 {"error":"a personal access token
cannot mint another credential: sign in to mint one"}` to a personal
access token: a token is pinned to its team and its own expiry. An access
token minted before access tokens named their session switches too,
bounded by its own expiry alone. `POST /api/me/tokens` and `GET
/api/me/sso/{provider}/link/start` refuse the same credentials the same
way — a personal access token mints no other token and links no SSO
identity — the link start also answers `403` to an access token that names
no session, and the link's callback asks again once the link is written,
removing it (`?sso_link_error=failed`) when the session ended meanwhile;
`POST /api/ws/ticket` answers `401` to the access token of a revoked
session, `403` to a ticket presented to mint another, and an `expires_in`
no longer than the presenting credential has left; the ticket's redemption
asks again, so a ticket opens no WebSocket once the credential that minted
it expired, its session ended or the personal access token that minted it
was revoked.

The routes that change or remove a credential — `POST /api/me/password`,
`POST /api/me/sessions/revoke-all`, `DELETE /api/me/tokens/{token_id}`,
`DELETE /api/me/sso/links/{provider}/{subject}` — and the writes of an
org's sign-in configuration (`POST`/`PATCH`/`DELETE
/api/orgs/{id}/sso/providers…`, `POST`/`DELETE /api/orgs/{id}/sso/domains…`
and `…/verify`) answer `401` to the access token of a revoked session, of a
disabled or pending account, before they act; a live personal access token
passes them (it is refused `403` at `POST /api/me/password`: that route
answers with a session). Every platform-admin route (`/api/admin/…`, the
plugin enable, disable, install and uninstall routes, a marketplace
plugin's install and uninstall, and the marketplace moderation of a public
or instance-wide entry) answers `401` to the same tokens and `403
super-admin only` once the account is no longer a super-admin, whatever
the token says; the same two platform acts outside the admin prefix do the
same — `PUT /api/orgs/{id}/members/{user_id}`, the instance-wide (`global`)
memory space writes, and revoking another account's personal access
token. A super-admin's claim is re-read from the account where every
request's identity is established, so a demotion takes the platform
authority away at once everywhere — the team and org routes the flag
could reach included — while the token's ordinary access persists to its
expiry. A personal access token passes the platform-admin routes, but
`POST /api/admin/users/{id}/reset-password` answers it `403` for its own
account.
`POST /api/auth/desktop/exchange` answers `401` (`auth: session revoked`,
`auth: account disabled`) to a ticket whose session was revoked, or whose
account was disabled or set pending, since the IdP callback minted it.

## Teams + members + invitations

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/teams` | member | List the caller's teams |
| `POST` | `/api/teams` | org admin | Create a team in the `org_id` the body names, else in the caller's active org (`canManageOrg`) |
| `GET` | `/api/teams/{id}/members` | team member | List members. `403 not a member` means the authorization reads answered; one that could not be completed answers the identity `500` above, logged — unless another row already grants access (an org admin of the team's org), which no failed read takes away |
| `PUT` | `/api/teams/{id}/members/{user_id}` | team admin | Place an account that ALREADY exists, idempotently (`{"role":…}`). Refuses **422** when the user is not yet a member of the team's ORG — that membership is the identity boundary a team grant sits inside, so the order is `PUT /api/orgs/…` first. The membership is re-read AFTER the grant is written — on a context detached from the request, and on both outcomes of that write, since a store that reports an error may have applied it anyway: **409** when a concurrent `DELETE /api/orgs/{id}/members/{user_id}` took it away in between and the grant was revoked — with the pending invitations the holder addressed to their own address in this team, which that grant was the premise of (`invitation.deleted`, reason `creator_team_grant_unwound`), a sweep that fails being named in the same body for a revoke by hand; **500** when the revoke itself failed (the body names the grant that may still stand), or when an ACKNOWLEDGED write's membership cannot be re-confirmed (the grant is revoked too — a boundary nothing can certify does not hold one up). A write that FAILED with the membership unreadable answers that write error and keeps the grant: one store fails both calls, and the grant left standing is the one the member already held |
| `PATCH` | `/api/teams/{id}/members/{user_id}` | team admin | Change role. A role change WRITES the grant row back, so for a role that HAS a boundary it owes the same guards as the `PUT`: **422** when the user is not (or is no longer) a member of the team's org — an existing grant is refused, never revoked, since that drift is what `orphan_grant` reports — and **409**/**500** at the re-read that follows the write, under the `PUT`'s rule. A role OUTSIDE the ladder (`config_editor`) is mirrored up to no org membership (ADR-078), so writing it reads no boundary when the grant is ALREADY off the ladder too — the state a `config_editor` invitation creates, answered **200** for an account that holds no org membership. Taking a LADDER grant off the ladder still reads the boundary: otherwise the holder of an orphan `admin` grant, who passes the team-admin check on that grant alone, would re-role themselves and keep the grant in one request. The same holder still reaches that grant state in two, by minting an invitation to their own address and accepting it, but only for a role mirrored up to NO org membership: a mint is capped at its minter's own org standing (`POST /api/teams/{id}/invitations`), so that pair of requests no longer re-admits them to the ORG — which is why `orphan_grant` reports a missing org membership for ANY role and `off_ladder` names the sanctioned cause beside it rather than inside it |
| `DELETE` | `/api/teams/{id}/members/{user_id}` | team admin | Remove a member. A concurrent `PATCH …/members/{user_id}` that read the grant before this removal writes that row back and re-creates the grant at the role it asked for: the store has no conditional write, so the role change cannot tell its own update from a resurrection. Both calls answer success; the removal is idempotent, so re-issue it. This delete also WITHDRAWS a premise: the mint's own confirmation keeps a self-addressed invitation on its creator's grant in the team when they hold no org membership row in its org (`POST /api/teams/{id}/invitations`), so once the grant is taken those rows are swept, one audit line each (`invitation.deleted`, reason `creator_team_grant_removed`) — a row left standing would be a re-entry ticket into the team the grant was just taken from. It takes only what that grant was the premise of: a row addressed to SOMEBODY ELSE stands, and so does everything a holder who still HAS an org membership row minted, those resting on that row — read ONCE, before the invitations are selected, so a row written back in between (a placement, an accept, a role change) is read as absent and what rests on it is swept too: that window costs an invitation that stops working, with nothing granted and no org rank written, each row taken named in the team's audit trail and re-mintable. **500** when the team's org, that row or the team's invitations cannot be read — a selection that could not be made is not one that found nothing — naming the row that may stand and asking for the removal again |
| `GET` | `/api/teams/{id}/invitations` | team admin | List pending invitations |
| `POST` | `/api/teams/{id}/invitations` | team admin | Mint a token (shown once). The role it carries is capped at the minter's OWN org standing, since accepting an invitation on the org ladder mirrors the grant up to an org membership (`pkg/auth` `grantOrgMembershipForTeam`; `admin`/`owner` → org **admin**, the others → org **member**): a role whose mirror the minter does not reach answers **403** naming the role, that mirror and their own org role. That is what keeps a team admin who is a plain org member — or the off-boarded holder of an orphan `admin` grant, who passes the team-admin check on that grant alone — from minting one for their own address and coming back an org admin. A role mirrored up to NOTHING (`config_editor`, ADR-078) carries no standing to exceed, so no RANK measures it: no cap at the mint, and no sweep of the MINTED half when its creator's own standing drops — a removal included, which hands such an invitation over rather than withdrawing it. A removal still takes one addressed to its own creator: the cascade's addressed-to half takes it whatever its role (`DELETE /api/orgs/{id}/members/{user_id}`), so the confirmation after the mint takes it too once that creator can no longer REACH the team it joins — no org membership row and no grant in that team — **403** naming the address rather than a rank. A creator who still HOLDS that grant keeps the invitation, since accepting it gains them nothing the team-admin check did not already admit the mint on — the orphan-grant state above, reported rather than revoked. That precondition is the confirmation's OWN: the removal's cascade takes such a row whatever grant its creator holds, because it revokes that grant in the same pass, so the two sides answer differently on exactly that one state, by design and with no race, and re-issuing the removal takes a row a mint kept. What answers the grant going away WITHOUT the row is the removal's order: its LAST write is the invitation sweep (`DELETE /api/orgs/{id}/members/{user_id}`), so no row the mint kept on a grant that cascade revoked outlives it. A grant revoked with NO removal in progress, in a team that stays, carries that row with it too, by the same rule read from the other end — whoever withdraws the premise sweeps the rows kept on it: `DELETE /api/teams/{id}/members/{user_id}`, the grant compensation of the team-member `PUT`/`PATCH` and the GitHub SSO reconciliation of a login the allow-list no longer grants that team revoke them with the grant (reasons `creator_team_grant_removed`, `creator_team_grant_unwound`, `creator_team_grant_sso_revoked`; the SSO one runs on the login path, and a sweep that fails there fails the login, since the row it could not take is a re-entry ticket into the team the grant just left). The compensation sweeps on the exit where that org membership row is definitively GONE: on the one where the row could not be READ the rule has nothing to decide on, so the grant is taken back with no sweep and the **500** names the row that may stand (listed by `GET /api/teams/{team}/invitations`, revoked by hand). So a grant written BACK after a removal's last sweep — by a straddling accept, a team admin's `PATCH`, a placement — re-opens the window, and the row goes with that grant when it goes. That is what such a mint owes reads for: its creator's standing, then — only where that membership row is gone — their grant in the team and the address their own account answers at. A team with no parent org has no boundary to read; a super-admin passes. The standing is read from the store rather than from the token, so a membership removed mid-request refuses the mint and one that cannot be read answers **500**. The team names the org that standing is read in, so for a caller the team-admin check admitted on their OWN grant — or a platform admin — a team that does not exist answers **404** `auth: team not found` and one that cannot be READ answers **500**: a cap that cannot be applied does not mint. An ORG admin meets that check's own **403** on both instead, since it resolves the team to find the parent org they manage. It is read at the MINT and CONFIRMED once the row has landed, since those writes revoke only the rows their own listing reaches: a mint whose cap was read before such a write and whose row lands after its sweep is refused by that confirmation — **403** in the cap's words, or in the address's for the role with no mirror addressed to its own creator, with the invitation it minted revoked (one audit line, `invitation.deleted`, reason `creator_standing_dropped`), or **500** when one of those three states cannot be read, which takes the invitation back too, under the reason of THAT read (`creator_standing_unconfirmed`, `creator_team_grant_unconfirmed`, `creator_address_unconfirmed`) and with the body naming it — a standing that could not be READ is not one that dropped, one cause for three reads answers neither question, and the trail is where they are asked (a revoke that fails leaves the row and the refusal names it, to be revoked by hand). The rank is otherwise kept in step at the ROW rather than re-read at acceptance (the accept reads nothing about an invitation's creator): a pending invitation carries the role it was minted with until it is revoked — by hand (`DELETE …/invitations/{invite_id}`), or by the writes that lower its creator's own standing, which revoke what they minted above what they are left with (`PATCH /api/orgs/{id}/members/{user_id}`, `PUT /api/orgs/{id}/members/{user_id}`, and the org-member `DELETE` cascade, where what they are left with is no standing at all) |
| `DELETE` | `/api/teams/{id}/invitations/{invite_id}` | team admin | Revoke |
| `GET` | `/api/orgs/{id}/usage` | org member | Org-member mirror of the admin usage view (see below) |
| `GET` | `/api/teams/{id}/audit` | team admin | Tenant audit log |

## Organisations — self-serve (org members, teams, SSO)

The org-admin self-serve mirror of the super-admin org views (two-level
tenancy, ADR-048). All routes are `requireAuth` and the Auth column below is
the per-route truth: mutations need org **admin/owner** (`canManageOrg`) and
most reads org **membership** (`canViewOrg`), but the invitation list and the
audit log are reads behind `canManageOrg` — they expose invitees and actor
emails/IPs — and `PUT …/members/{user_id}` takes the platform super-admin flag
*before* any org role is consulted. Sources:
[pkg/server/orgs_routes.go](../pkg/server/orgs_routes.go),
[pkg/server/org_sso_routes.go](../pkg/server/org_sso_routes.go),
[pkg/server/org_sso_domain_routes.go](../pkg/server/org_sso_domain_routes.go),
[pkg/server/team_lifecycle_routes.go](../pkg/server/team_lifecycle_routes.go)
(`PUT …/members/{user_id}`) and
[pkg/server/audit_helper.go](../pkg/server/audit_helper.go) (the audit log).

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/orgs/{id}` | org member | One org — name, slug, status, quotas. The twin of `GET /api/teams/{id}`, so a console can resolve an org by id instead of by the caller's own membership tree |
| `GET` | `/api/orgs/{id}/members` | org member | List org members + roles |
| `PUT` | `/api/orgs/{id}/members/{user_id}` | **super-admin** | Place an account that ALREADY exists, idempotently (`{"role":…}`). Creates or updates, where `PATCH` requires an existing membership. One issued while a `DELETE` of the same member is in flight answers 200 and leaves NO row: the removal's last delete takes it — re-issue the placement once the removal has returned. Super-admin because it names an account id rather than an address its owner answers at: under org admin it absorbs any account, discloses its email through the roster, and turns the 404/200 split into an existence oracle. An org admin adds by `POST …/invitations`. Being idempotent on an EXISTING membership, it is also a way an org role goes DOWN, so it reconciles what its subject minted the way the `PATCH` does: the pending invitations whose org mirror is above the role written are revoked, and a sweep that cannot read them answers **500** with the role already written, naming the act to re-issue (the placement). The rank cap of the `PATCH` is not read here — this route refuses every non-super-admin, whom the cap exempts |
| `PATCH` | `/api/orgs/{id}/members/{user_id}` | org admin | Change a member's org role (`member\|admin\|owner`). The role written is capped at the WRITER's own org standing, read from their membership row: an org admin writing `owner` — on anybody's row, their own included — answers **403** naming the role, that standing and who can write it: a member who already holds that rank, or a platform admin through the `PUT`. Lowering a role is never capped by the rank written, whatever the row's own rank, so an org admin still lowers an OWNER — and that write carries the sweep below, which takes what the owner minted above the role written; an owner still appoints an owner, and a super-admin passes (`IsSuperAdmin` is the escape hatch, and they hold no org membership for a cap to read). An org whose roster holds NO owner therefore takes a platform admin to appoint one — the state the teams→orgs backfill leaves, where the mirror `identity.OrgRoleForTeamRole` tops out at org admin, and the one an owner's own self-demotion or removal leaves behind. **500** when the writer's own standing cannot be READ: a cap that cannot be applied does not write. A writer whose own membership row goes away between the gate and this read holds no standing at all, so that interleaving refuses every write they make, a lowering included. The role written is also the standing the pending invitations that member MINTED are measured against, since accepting one mirrors its role up to an org membership (`pkg/auth` `grantOrgMembershipForTeam`): the ones above it are revoked across the org's teams, one audit line each (`invitation.deleted`, reason `org_member_demoted`), while the ones at or below it, the ones mirrored up to NOTHING (`config_editor`) and the ones somebody ELSE minted stand — including one minted FOR that member, since the sweep selects on who minted a row and is blind to the address it names, so one they minted for their OWN address goes with the rest. That sweep runs AFTER the write, on a context detached from the request, and reconciles against the role WRITTEN rather than the one it replaced, so **500** `revoking the pending invitations they created` leaves the role changed with such an invitation standing, and the re-issue it asks for — the same role again — finishes it. One residual it does not reach: an accept already in flight. The accept CLAIMS its invitation before writing the org membership its role mirrors up to (`pkg/auth`), and the sweep leaves a claimed row alone, so one claimed before the sweep's listing still seats its holder at the mirrored rank; re-issuing the role change does not undo that, the roster's own write does |
| `DELETE` | `/api/orgs/{id}/members/{user_id}` | org admin | Remove a member (cascades to every team grant inside the org, every not-yet-accepted invitation that member MINTED in those teams whose role carries an org role (`identity.OrgRoleForTeamRole`) — the demotion's own rule measured against the standing a removal leaves, which is none: every role on the ladder is above it and goes, while one mirrored up to NOTHING (`config_editor`, ADR-078) outranks no standing and stands. They are selected by the account id the path names, so they are swept whether or not an account row answers to it: a hand-over on the ladder hands over BEFORE leaving, since the invitation an off-boarded owner minted for their successor goes with them — AND, where their account row names an address, every not-yet-accepted invitation addressed to them in those teams, twice — the second sweep runs once the membership row is gone, on a context detached from the request, so a placement, a role change or a mint that happened while the membership was still live is swept; the invitations go first in each pass because an accept CLAIMS the invitation before it writes any access (`pkg/auth`), and two sides ordered alike leave an accept only one way through — claiming before a pass's first delete and writing its grant after its last). The membership row is then deleted once MORE, after the second sweep and on its own budget: an accept that straddled the sweeps writes the boundary back before its grant, so that delete is what takes it — and it is blind to who wrote the row, so a placement (`PUT …/members/{user_id}`) issued inside the window answers 200 and leaves nothing behind; re-issue the placement once the removal has returned. The invitations are then swept once MORE, last of everything this removal writes and on its own budget: inside a pass that sweep precedes the grant revoke, and the mint's own confirmation KEEPS a self-addressed row from a creator who still holds a grant in the team (`POST /api/teams/{id}/invitations`), so a row minted between the two is seen by neither sweep and kept on a grant the pass takes one call later — the off-boarded member's own re-entry ticket behind a 204. With that selection last, a 204 means no invitation row this rule takes stands behind a premise THIS removal withdrew; a failure of it answers **500** in a clause of its own, naming the row rather than the grants the second pass revoked. What it does not bound is another writer: a grant or a boundary row written back AFTER that selection's listing re-opens the window, and what closes it then is the write that withdraws the premise next — the team-member `DELETE` and the grant compensation sweep the rows kept on a grant they revoke, and re-issuing the removal is the repair for a boundary row. What that last delete of the membership row bounds is likewise the WINDOW, not the race: an accept that claimed its invitation before the first sweep and lands BOTH its access writes after the last delete leaves the member an org member holding the team grant, with `orphan_grant` quiet because the boundary is there, so the console's row for that grant carries no drift flag at all, and re-issuing the removal is the repair. The cascade acts on the account id the path names — this route neither requires it to be a member nor refuses an unknown one — so a target with NO account row is still off-boarded: there is no address its invitations could be addressed to, which is logged and leaves them standing (listed by `GET /api/orgs/{id}/invitations`, revoked by hand), while the grants and the membership row are taken; a user read that fails for any OTHER reason stops the pass. **500** after the first pass leaves the org membership in place, but the pass has no transaction behind it: the grants and invitations it revoked before failing stay revoked (each named in its team's audit trail); **500** on the membership row's own delete means the first pass is through — the grants revoked, and the invitations under the condition above — and that row may or may not be gone (`DeleteOne` answers nil when nothing matched, so the error can be a lost ack); **500** after the second means the member IS removed — the last delete runs on that path too, so the boundary row is taken either way — and may still hold a grant or a pending invitation, which the body names; **500** on the last delete means an org membership written inside the window may stand, so they would be an org member again, and when the second sweep failed too one body names both halves. Each of those bodies says which state it left, and the removal is idempotent, so re-issue it to finish the sweep |
| `GET` | `/api/orgs/{id}/invitations` | org admin | List pending org invitations |
| `POST` | `/api/orgs/{id}/invitations` | org admin | Mint an org invitation token. There is no org-only invitation: it is a TEAM invitation into `team_id`, or into the org's **primary team** when the body names none — its first non-personal team, else its first team, so an org that holds only personal teams defaults into one of those, and where that org is itself personal the invitee becomes a second member of an org whose own `PUT …/members/{user_id}` answers **422** to one. Accepting it writes a team grant AT THE REQUESTED ROLE in that team — `owner` with no `team_id` makes the invitee owner of the org's primary team — besides the org membership the ladder mirrors up. **404** `team not in org` when that `team_id` is a team of ANOTHER org, and in the same words when it is a team that exists nowhere: `canManageOrg` authorized the org of the path and nothing about another one, accepting such an invitation would write a grant, and on the ladder an org membership, in a tenant the caller does not manage — and telling the two apart would tell an org admin which team ids exist outside their own tenant, which its siblings (the caps `PATCH`, the invitation `DELETE`) do not. **500** when that team cannot be read, or when the org's teams cannot be LISTED on the default-team path — a store that failed is not an org without a team, which stays **400**. The **403** role cap of the team route applies here too, on a read of its own — the gate read the standing before the body named the team — and so does its confirmation once the row has landed, with the same **403**/**500** |
| `DELETE` | `/api/orgs/{id}/invitations/{invite_id}` | org admin | Revoke |
| `GET` | `/api/orgs/{id}/teams` | org member | List the org's teams |
| `POST` | `/api/orgs/{id}/teams` | org admin | Create a team in the org |
| `GET` | `/api/orgs/{id}/audit` | org admin | Org audit log |
| `GET` | `/api/orgs/{id}/sso/providers` | org member | List SSO providers |
| `POST` | `/api/orgs/{id}/sso/providers` | org admin | Add an SSO provider (OIDC) |
| `PATCH` | `/api/orgs/{id}/sso/providers/{provider_id}` | org admin | Update a provider |
| `DELETE` | `/api/orgs/{id}/sso/providers/{provider_id}` | org admin | Remove a provider |
| `POST` | `/api/orgs/{id}/sso/providers/{provider_id}/test` | org admin | Test a provider's config |
| `GET` | `/api/orgs/{id}/sso/domains` | org member | List claimed SSO domains |
| `POST` | `/api/orgs/{id}/sso/domains` | org admin | Claim a domain |
| `POST` | `/api/orgs/{id}/sso/domains/{domain_id}/verify` | org admin | Verify a claimed domain |
| `DELETE` | `/api/orgs/{id}/sso/domains/{domain_id}` | org admin | Release a domain |

## BYOK LLM keys + generic secrets + bindings

User-scoped, team-scoped and org-tier (`/api/orgs/{id}/api-keys`,
`/api/orgs/{id}/oauth/…`) flavours share the same payload shape. All three
return metadata only — the plaintext is **write-only**.

Revoking takes the same right as creating (`canManageTeam` on both for a
team-scoped record, `canManageOrg` for an org-tier one), so an
org admin cannot be left holding a credential they installed in a team of
their org and cannot pull back. A **user-scoped** record is exempt: it is
personal, and only its owner or a super-admin may touch it.

⚠️ **`PATCH`/`DELETE` on `/api/me/...` is the same handler as the team
route, with no `{id}` to re-scope from** — so the store tenant stays the
caller's ACTIVE team, a TEAM-scoped record of that team is reachable there,
and the rule follows the **record's** scope rather than the path. The
sibling `GET /api/me/api-keys` lists user-scoped rows only, which hides the
asymmetry: not listable there, still mutable there.

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/teams/{id}/api-keys` | team member | List team's BYOK keys |
| `POST` | `/api/teams/{id}/api-keys` | team admin | Create — `base_url` required for `openai_compatible` (https, canonical; literal non-public addresses and reserved names refused) and refused elsewhere; `repos` restricts the key to repositories (full forge paths, no URL or glob); unknown fields and a second JSON value are refused (strict) |
| `PATCH` | `/api/teams/{id}/api-keys/{key_id}` | team admin | Rename, toggle default, ROTATE the sealed secret (same shape gate as create, new `last4` + fingerprint), set `max_concurrent_runs` (0 = uncapped), replace the `bots` or `repos` audience, move a gateway key's `base_url` — a rotation: `secret` is required in the same request and is sealed under the new URL, a move without it is `400` (the stored secret is never re-sealed for a host its holder did not prove it for), re-sending the stored URL is a no-op — and every rotation of a gateway key answers `409` when another writer rotated or moved the key between the read and the write; `base_url` on a vendor key is `400`, blank included (a create ignores an empty one as absent); unknown fields and a second JSON value are refused (strict) — an absent field is left alone, `"bots": []` clears it. The write is a `$set` of the named fields only, never a replace of the row. A row whose `provider` this server build does not know (written by a newer server during a rolling update, or left by a rollback) is refused with `409`, audited as `refused`: update it from a current server, or delete it |
| `DELETE` | `/api/teams/{id}/api-keys/{key_id}` | team admin | Delete |
| `GET` | `/api/me/api-keys` | member | List own user-scoped keys |
| `POST` | `/api/me/api-keys` | member | Create personal key (same fields and rules as the team route) |
| `PATCH` | `/api/me/api-keys/{key_id}` | per record scope | Update — same write semantics and `409` on a row from a newer build as the team route |
| `DELETE` | `/api/me/api-keys/{key_id}` | per record scope | Delete |
| `GET` | `/api/teams/{id}/secrets` | team member | List team's generic secrets |
| `POST` | `/api/teams/{id}/secrets` | team admin | Create |
| `PATCH` | `/api/teams/{id}/secrets/{secret_id}` | team admin | Update |
| `DELETE` | `/api/teams/{id}/secrets/{secret_id}` | team admin | Delete |
| `GET` | `/api/me/secrets` | member | Personal secrets |
| `POST` | `/api/me/secrets` | member | Create |
| `PATCH` | `/api/me/secrets/{secret_id}` | per record scope | Update |
| `DELETE` | `/api/me/secrets/{secret_id}` | per record scope | Delete |
| `GET` | `/api/teams/{id}/bots/{bot_id}/bindings` | team member | List bot bindings |
| `POST` | `/api/teams/{id}/bots/{bot_id}/bindings` | team admin | Create binding |
| `PATCH` | `/api/teams/{id}/bots/{bot_id}/bindings/{binding_id}` | team admin | Update |
| `DELETE` | `/api/teams/{id}/bots/{bot_id}/bindings/{binding_id}` | team admin | Delete |

Sources:
[pkg/server/byok_routes.go](../pkg/server/byok_routes.go),
[pkg/server/generic_secrets_routes.go](../pkg/server/generic_secrets_routes.go),
[pkg/server/bot_bindings_routes.go](../pkg/server/bot_bindings_routes.go).
Full semantics in [secrets-reference.md](secrets-reference.md).

## Team bot sources (cloud bot editing)

Team-authored bot bundles — the writable, tenant-scoped store the studio
editor saves into (cloud pods bake the catalog read-only). Each source is a
multi-file bundle (`main.bot` + `manifest.yaml` + `skills/`…). Edit rights =
the `config_editor` capability on the team, team admin or owner, an org
admin/owner of its parent org, or a super-admin (`canEditBots`) — a plain
viewer or member is not an editor. If the
server has no bot-source store wired every route returns `501 bot editing is
not enabled on this server`.

| Method | Path | Access | Purpose |
|---|---|---|---|
| `GET` | `/api/teams/{id}/bot-sources` | bot editor | List the team's bots (metadata only — file bodies omitted) |
| `GET` | `/api/teams/{id}/bot-sources/{slug}` | bot editor | One bot with its full file map |
| `PUT` | `/api/teams/{id}/bot-sources/{slug}` | bot editor | Create or replace the whole bundle (`{files, version?, id?, stored_sha256?}`) |
| `PUT` | `/api/teams/{id}/bot-sources/{slug}/files/{path...}` | bot editor | Per-file save (`{content, version?, id?, stored_sha256?}`) |
| `DELETE` | `/api/teams/{id}/bot-sources/{slug}/files/{path...}` | bot editor | Delete one file (never `main.bot`) |
| `DELETE` | `/api/teams/{id}/bot-sources/{slug}` | bot editor | Delete the bot |
| `POST` | `/api/teams/{id}/bot-sources/{slug}/fork` | bot editor | Fork a baked catalog bot (`{from}`) into an editable copy |

Every write validates the whole file map. A key ending in `.bot.yaml` — an
author document, a draft of a `.bot`, never what launches — is refused with
`400` and `error_code: author_document`; the store never holds one. A bot that
holds such a key from before this rule is written again by deleting that file
(the per-file `DELETE` validates the map after the removal) or by a whole-bundle
`PUT` without it. A fork reads the catalog bundle without its drafts.

Every write **compiles the bundle before it persists** — a bot that fails to
parse/compile is rejected `400 bot does not compile: <diagnostics>`, never
left to fail at launch. A non-zero `version` is an optimistic if-match token
(`409` if a concurrent editor advanced it); slug collisions are `409`.

### The digest rule — what a read decided, a write carries

`GET` on one bot answers, beside the files, `id`, `version` and
`file_sha256` — per file, the digest of the bytes as answered. A write of an
existing file presents back what that read issued: `version` and `id` name
the bot (a bot deleted and re-created under the same slug is a NEW id, and a
write pinned to the old one is `409`, never a replacement), and
`stored_sha256` — per path on the whole-bundle `PUT`, the single file's own
on the per-file `PUT` — must still be the digest of the stored file the
write REPLACES or OMITS (an omission deletes), for EVERY file the write
touches or drops — `manifest.yaml` and the bundle's other non-`.bot` files
included, so an omission without that file's digest is a `409` naming it,
never a silent deletion. A digest of other bytes, a
missing one, or a missing `id` on an update is `409` naming what changed; a
creation (no `version`) decides on the bot's absence and carries none; the
per-file `DELETE` removes by name and needs no digest — pinned to a
`version`, it presents `bot_id` like the `PUT`s do (a bot deleted and
re-created takes no old client's delete); presented without a `version`,
the id is still honored — the identity is judged and the freshest version
is deleted — and with neither `version` nor `bot_id`, the delete stays
last-write-wins. A write that deletes a manifest which declared
`requires.iterion` is `409` too: the requirement dies with the file, and
the push is told to push it with the manifest or force.

Migration note: the version number used to be the whole claim, and
`stored_sha256` was optional — a write was decided on a fresh fetch of the
bundle, so two editors could still silently overwrite one another, and a bot
deleted and re-created at the same version took an old client's write. The
studio and `iterion remote admin bots push` were migrated in the same
change; a custom client starts sending `id` and `stored_sha256` (both come
from the same `GET` that already handed it the files) or receives `409` on
every update of an existing bot — every file of it, the bundle's
non-`.bot` files included.

The same rule holds the workspace's own doors: a save of an existing file
through `POST /api/files/save` (disk studio) presents `digest`, the sha256
its open issued in the open response, and an open that did not read a file
whole — past the read budget or the size cap, a file that is not UTF-8 —
issues none, so no save decided from such an open can present one. The file
door holds a save to the file's own whole read alone; a save through the
bot's unit document is held to the read budget the bot's files share — a
fragment whose own read passes while the shared budget refuses it lands
through the file door, and the landed state is visible: the bot opens
short, and the files the budget cut answer no digest. A
document the server's reader refused opens with its `document` marked
`{"salvage": "not read whole: <code>"}`, which every door that decodes a
document refuses (`400`, `ast.ErrSalvage`) wherever it runs. `salvage` is
reserved: a document carrying any key that case-folds to it at top level is
refused as the display of a load not read whole (the match is Go's
case-insensitive one, `Salvage` and unicode folds included); below the top
level the word is a name like any other.

Source:
[pkg/server/bot_sources_routes.go](../pkg/server/bot_sources_routes.go),
store [pkg/botsource/](../pkg/botsource/botsource.go).

## Forge integrations (connections, OAuth apps, repo-bots)

The team-scoped, **outbound** forge layer behind the studio's repo-first shell
([docs/repo-scope.md](repo-scope.md)): connect a forge, hold an OAuth app /
GitHub App credential, and provision a set of bots onto a repo (webhook + hook
+ managed secret + bindings). All routes are `requireAuth` and the Auth column
below is the per-route truth: most reads need team **membership**
(`canViewTeam`) and mutations team **admin/owner** (`canManageTeam`), with
exceptions both ways — the connection's repo list, the repo-bots preview and an
integration's hook list are reads behind `canManageTeam`, while the GitHub-App
connection refresh is a write that takes membership, like connection health.
The card-scoped `/api/v1/native/issues/*` routes of `board_forge.go` — push,
list pulls, open a pull, its CI, merge it — take the board family's write rung
(`boardWriteAuth`): a state-changing call needs the member role in the
caller's ACTIVE team, a read checks no role, and neither consults the
card's own team beyond that. Sources:
[pkg/server/forge_routes.go](../pkg/server/forge_routes.go),
[forge_refresh_route.go](../pkg/server/forge_refresh_route.go),
[forge_connect_routes.go](../pkg/server/forge_connect_routes.go),
[forge_oauth_app_routes.go](../pkg/server/forge_oauth_app_routes.go),
[forge_github_manifest_routes.go](../pkg/server/forge_github_manifest_routes.go),
[forge_provisioning_routes.go](../pkg/server/forge_provisioning_routes.go),
[board_forge.go](../pkg/server/board_forge.go).

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/teams/{id}/forge/connections` | team member | List forge connections |
| `POST` | `/api/teams/{id}/forge/connections` | team admin | Connect a forge (PAT / OAuth / GitHub-App install) |
| `DELETE` | `/api/teams/{id}/forge/connections/{conn_id}` | team admin | Remove a connection |
| `GET` | `/api/teams/{id}/forge/connections/{conn_id}/health` | team member | Connection health / token probe |
| `POST` | `/api/teams/{id}/forge/connections/{conn_id}/refresh` | team member | GitHub App only: re-probe live installation grants, persist them, and force a fresh token mint |
| `POST` | `/api/teams/{id}/forge/connections/{conn_id}/avatar` | team admin | Upload the iterion-bot avatar onto the account behind a PAT connection (`{variant?, force?}`); 422 on OAuth (a person), on GitHub (no API — names the App's settings page) and on a token the forge rejects (the connection is marked `revoked`; reconnect first), 409 `needs_force` on an account the forge does not flag as a bot or would not describe at all, 502 when the forge did not answer the identity read or refused the upload. See [brand.md](brand.md) |
| `GET` | `/api/teams/{id}/forge/connections/{conn_id}/repos` | team admin | Repos visible to the connection |
| `GET` | `/api/teams/{id}/forge/repos` | team member | Team's forge-linked repos |
| `POST` | `/api/teams/{id}/forge/repos` | team admin | Create a repo (opt-in `RepoCreator` capability) |
| `GET` | `/api/teams/{id}/forge/oauth-apps` | team member | List per-tenant OAuth apps |
| `POST` | `/api/teams/{id}/forge/oauth-apps` | team admin | Register an OAuth app (`manual` / `auto` / `auto_from_connection`) |
| `DELETE` | `/api/teams/{id}/forge/oauth-apps/{app_id}` | team admin | Remove an OAuth app |
| `POST` | `/api/teams/{id}/forge/oauth-apps/github-manifest` | team admin | Start the GitHub App-manifest auto-create flow |
| `GET` | `/api/teams/{id}/forge/repo-bots` | team member | List repo→bot provisionings (integrations) |
| `GET` | `/api/teams/{id}/forge/repo-bots/preview` | team admin | Preview what enabling a bot set subscribes to (no forge writes) |
| `POST` | `/api/teams/{id}/forge/repo-bots` | team admin | Enable bots on a repo (provision webhook + hook + secret + bindings) |
| `PATCH` | `/api/teams/{id}/forge/repo-bots/{integration_id}` | team admin | Set the bot set and the repo's operator settings. A PRESENT `bot_ids` replaces the stored set; an ABSENT one keeps it, so a settings-only patch need not echo it; an explicitly empty one is a `400` — `DELETE` removes the integration. In an org that requires provision approval, a patch that widens the repo's automation — adding a bot, turning auto-fix on or sending `schedule_crons`, for example — answers `202 pending_approval` (`409` while another request for the repo is pending) and changes nothing until approved; a caller who manages the org is not parked, nor, under the `shared_credentials` scope, a team that funds its own runs |
| `DELETE` | `/api/teams/{id}/forge/repo-bots/{integration_id}` | team admin | Disable / deprovision (tears down webhook + hook) |
| `PATCH` | `/api/teams/{id}/forge/integrations/{iid}` | team admin | Update an integration (incl. `sync_issues_enabled`) |
| `POST` | `/api/teams/{id}/forge/integrations/{iid}/sync` | team admin | Run the forge→board issue sync now (one-way, forge is source) |
| `GET` | `/api/teams/{id}/forge/integrations/{iid}/hooks` | team admin | List the webhooks on an integration |

The OAuth handshake completes on public callbacks the SPA is redirected to:
`GET /api/forge/oauth/callback`, `GET /api/forge/github/app/callback`, and
`GET /api/forge/github/app-manifest/callback`.

## Project-board binding (GitHub Projects v2)

One board per team (ADR-097). Binding it makes the board's `Status` column
two-way with the native board's columns and imports `Area`/`Mode`/`Priority`
onto cards as `area:`/`mode:`/`prio:` labels. Runbook:
[docs/github-board-sync.md](github-board-sync.md).

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/teams/{id}/board-binding` | team member | The binding, incl. the effective status map and the last sync |
| `PUT` | `/api/teams/{id}/board-binding` | team admin | Bind / re-bind (resolves the board and caches its ids) |
| `DELETE` | `/api/teams/{id}/board-binding` | team admin | Unbind |

`PUT` takes the board's **address**, never its ids — every id in the stored
binding comes from reading the board with the team's own credential, so a
caller cannot point a team at a board it has no rights on:

```jsonc
{
  "owner": "SocialGouv",
  "number": 203,
  "owner_kind": "org",              // or "user"; default "org"
  "connection_id": "conn_123",
  "status_map": {                   // optional; default = the shipped five
    "Todo": "ready",
    "Doing": "in_progress",
    "Shipped": "done"
  },
  "sync_every_seconds": 120         // absent = default (120); 0 = off; floor 60
}
```

Every rejection is a `400` naming the cause: a malformed ref, a non-injective
`status_map` (two columns on one state), an interval under the floor, a board
the credential cannot resolve, or a map matching none of the board's columns
(the real column names are listed in the message). `GET`/`DELETE` on an unbound
team are `404`, never a silent success.

## Inbound webhooks

CRUD (operator-side) plus per-provider delivery URLs.

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/teams/{id}/webhooks` | team member | List |
| `POST` | `/api/teams/{id}/webhooks` | team admin | Create + mint `iwh_` token (shown once) |
| `GET` | `/api/teams/{id}/webhooks/{webhook_id}` | team member | Get one |
| `PATCH` | `/api/teams/{id}/webhooks/{webhook_id}` | team admin | Update |
| `DELETE` | `/api/teams/{id}/webhooks/{webhook_id}` | team admin | Delete |
| `POST` | `/api/teams/{id}/webhooks/{webhook_id}/rotate` | team admin | Rotate token + re-seal HMAC |
| `GET` | `/api/teams/{id}/webhooks/{webhook_id}/deliveries` | team member | Last ~100 deliveries |
| `POST` | `/api/webhooks/gitlab/{id}` | webhook token | Inbound delivery (MR + `/revi`) |
| `POST` | `/api/webhooks/github/{id}` | webhook HMAC | Inbound PR delivery |
| `POST` | `/api/webhooks/forgejo/{id}` | webhook HMAC | Inbound PR delivery (also Gitea headers) |
| `POST` | `/api/webhooks/generic/{id}` | webhook token (or HMAC opt-in) | Bot-agnostic JSON delivery |

Source: [pkg/server/webhooks_routes.go](../pkg/server/webhooks_routes.go).
Full reference: [webhooks.md](webhooks.md).

## OAuth-forfait (Claude / Codex)

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/me/oauth/connections` | member | List configured forfait kinds + expiry |
| `POST` | `/api/me/oauth/{kind}/authorize/start` | member | Begin the browser authorize handshake — the primary way a user connects |
| `POST` | `/api/me/oauth/{kind}/authorize/complete` | member | Finish that handshake |
| `POST` | `/api/me/oauth/{kind}/credentials` | member | Upload pasted `credentials.json` / `auth.json` |
| `POST` | `/api/me/oauth/{kind}/refresh` | member | Refresh stored access token against the IdP |
| `PATCH` | `/api/me/oauth/{kind}` | member | Name the account behind the credential (`{"account_label": "…"}`, `""` clears; ≤ 120 characters) — metadata only, the sealed credential is untouched |
| `DELETE` | `/api/me/oauth/{kind}` | member | Disconnect |

`authorize/complete` and `credentials` take an optional `?account_label=`
query parameter to name the account at connect time. The listing exposes
`account_label` beside `fingerprint`, the same string the publisher logs
when it picks a credential (see
[cloud-llm-credentials.md](cloud-llm-credentials.md#name-the-account-behind-every-credential)).

An owner may hold a CHAIN of credentials per kind — rank 0 the primary, 1 and
up the fallbacks tried in order — so the completion and the upload, which write a link, and `refresh`, `PATCH`
and `DELETE`, which address one, take a `?rank=` query parameter. Absent means rank 0, so
a call meant for a fallback lands on the primary and reports success. The
listing returns each record's `rank`.

Every route above has a team-scoped mirror at
`/api/teams/{id}/oauth/…` (`connections`, `{kind}/authorize/start`,
`{kind}/authorize/complete`, `{kind}/credentials`, `{kind}/refresh`,
`PATCH {kind}`, and `DELETE {kind}`) for a forfait the whole team draws
on rather than one operator — `GET connections` takes team **membership**
(`canViewTeam`), the six mutations team **admin/owner** (`canManageTeam`) —
and a platform mirror at `/api/admin/llm/oauth/…` (super-admin).

Source: [pkg/server/oauth_routes.go](../pkg/server/oauth_routes.go),
[pkg/server/oauth_team_routes.go](../pkg/server/oauth_team_routes.go).

## Credential pool

Lending your own subscription or personal metered key to the shared
pool — full model in [credential-pool.md](credential-pool.md).

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/me/pool` | member | Your pledges and what they have served |
| `PUT` | `/api/me/pool/{source}/{ref}` | member | Create or update a pledge (ceilings, sharing window, bot allow-list) |
| `DELETE` | `/api/me/pool/{source}/{ref}` | member | Withdraw a pledge |
| `GET` | `/api/me/pool/history` | member | The runs your quota served |
| `GET` | `/api/teams/{id}/pool` | team member | The pool's policy. The donor roster rides along only for an org admin/owner (`canManageOrg`) — who lends, and how much they have given, is the contributors' own business |
| `PUT` | `/api/teams/{id}/pool` | org admin | Set the audience policy deciding who may draw. The pool document is keyed by ORG, so a team admin/owner who is not also an org admin/owner is refused `403` |

Source: [pkg/server/credpool_routes.go](../pkg/server/credpool_routes.go).

## Cloud schedules

Recurring bots, the cloud counterpart of `iterion schedule` —
[scheduling.md](scheduling.md).

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/teams/{id}/schedules` | team member | List the team's cron-scheduled bots |
| `POST` | `/api/teams/{id}/schedules` | team admin | Create a schedule |
| `PATCH` | `/api/teams/{id}/schedules/{sid}` | team admin | Update one |
| `DELETE` | `/api/teams/{id}/schedules/{sid}` | team admin | Remove one |

A `config_editor` reaches the same schedule store by another door:
`PATCH /api/teams/{id}/config-editor/shares/{sid}/schedule` edits the cron of
the oldest schedule tied to its share's (bot, category) — an enabled, non-read-only share, and
only when such a schedule exists; the schedule's own flag, disabled or not, does not block the
edit — gated on
`canEditConfigShares`, which admits a `config_editor` as well as anyone
`canManageTeam` admits
([pkg/server/config_editor_routes.go](../pkg/server/config_editor_routes.go)).

Source: [pkg/server/schedules_routes.go](../pkg/server/schedules_routes.go).

## Triggers (event-driven runs)

The subscription registry binding (event filter) → (bot launch), gated
on `server_info.triggers_enabled` —
[ADR-046](adr/046-event-driven-runs-trigger-spine.md).

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/v1/triggers` | authenticated (no role) | List subscriptions (team-scoped in cloud) |
| `POST` | `/api/v1/triggers` | member | Create one |
| `GET` | `/api/v1/triggers/{id}` | authenticated (no role) | Read one |
| `PUT` | `/api/v1/triggers/{id}` | member | Replace one |
| `DELETE` | `/api/v1/triggers/{id}` | member | Remove one |
| `POST` | `/api/v1/triggers/emit` | member | Publish a custom event onto the bus |
| `GET` | `/api/v1/triggers/health` | authenticated (no role) | The in-process schedule scheduler's liveness — a local-mode surface: cloud fires schedules through cloudsched, so there it answers `scheduler_running: false` |
| `POST` | `/api/v1/bots/{name}/triggers/from-invocation` | member | Generate a subscription from a bot manifest's `invocations:` block |

Every state-changing route takes the member rung at the mount
(`triggerWriteAuth` — the same decision as the board family's write rung), so
a `viewer` lists subscriptions, reads one, and reads the health but cannot create, replace,
delete or generate one; `emit` additionally passes the launch gate, which
enforces the rung a second time. The reads carry `requireAuth`; the
subscription reads also scope by the request's tenant.

Source: [pkg/server/triggers_routes.go](../pkg/server/triggers_routes.go).

## Personal access tokens (PATs)

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/me/tokens` | member | List own PATs (no plaintext) |
| `POST` | `/api/me/tokens` | member | Mint a PAT (`iap_…` shown once) |
| `DELETE` | `/api/me/tokens/{token_id}` | member (owner) or super-admin | Revoke |

Source: [pkg/server/pat_routes.go](../pkg/server/pat_routes.go).

## Memory + knowledge

Spaces are addressed by query params (`?name=`, `?visibility=`, plus
`?bot=` for `visibility=bot` and `?project=` for project/bot scopes).
`/api/memory/*` is tenant-scoped, so a credential carrying no active team is
refused `403` before any `visibility` arm is evaluated — super-admins
included.

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/memory/usage` | member | `{used_bytes, quota_bytes}` for one space |
| `GET` | `/api/memory/docs` | member | List documents in a space (optional `?dir=`) |
| `GET` | `/api/memory/doc` | member | Read a document (`?path=`) |
| `PUT` | `/api/memory/doc` | member (super-admin for `visibility=global`) | Write |
| `DELETE` | `/api/memory/doc` | member (super-admin for global) | Delete |
| `GET` | `/api/memory/export` | member | Tarball export of the space |
| `POST` | `/api/memory/import` | member (super-admin for global) | Import a tarball |

Source: [pkg/server/memory_routes.go](../pkg/server/memory_routes.go).
Full reference: [memory-and-knowledge.md](memory-and-knowledge.md).

## Runs surface

Read-only views plus the launch / resume mutations the studio drives.
On every route that addresses a run by id — `/api/runs/{id}/…` and
`/api/ws/runs/{id}…` — the class below is the caller's standing in the RUN's
team (ADR-103), not in their active one: reads take a viewer, actions a member
— the `browser/cdp` and `shell` GETs count as actions, since they hand the
caller the run's browser and terminal — and an admin/owner of the team's org
who is not a member of that team enters as a viewer.

### The id-addressed routes outside the by-id class

Every route the mux serves under an id wildcard is either in the class above
or governed by another stated rule — the enumeration is pinned by
`TestEveryIDWildcardRouteIsClassifiedOrReasoned` in
[pkg/server/id_route_enumeration_test.go](../pkg/server/id_route_enumeration_test.go),
so a new id-addressed route cannot land unclassified:

| Family | Rule |
|---|---|
| `/api/teams/{id}/…` | the path team IS the scope (`canViewTeam` / `canManageTeam`) |
| `/api/admin/…` | super-admin over the platform sentinel tenant |
| `/api/auth/…`, `/api/me/…`, `/api/orgs/…` | identity surfaces, user-scoped, org-scoped by path |
| `/api/v1/native/issues/{id}…`, `/api/v1/pipeline-board/tasks/{id}…` | the board store resolves per ACTIVE team (cloud) or the one local store; a cross-team id answers `404` emergently — the ADR-103 by-id rule for issues is the pending #1849 slice |
| `/api/v1/native/labels/{label}`, `/api/v1/native/board/…` | board vocabulary and config of the active team's store |
| `/api/v1/bots/{name}…` | the dispatch catalog: a tiered namespace by design (team > platform > baked), not an authored-resource id |
| `/api/assistant-watches/{watchID}` | fixed compatibility sibling of the run-addressed `DELETE /api/runs/{id}/assistant-watches/{watchID}`: the stop resolves the watch's target run under the caller's active team |
| `/api/v1/marketplace/…` | public marketplace family (anonymous reads, repo-coordinate installs) |
| `/api/v1/mcp/…` | per-run token over single-tenant store surfaces |
| `/api/webhooks/…` | signature-authenticated deliveries |
| `/api/examples/{name…}`, `/api/local/…`, `/api/projects/{id}`, `/api/v1/pipeline-board/workspace-images/{path…}`, `/api/v1/plugins/{name}…`, `/brand/{file}` | deployment-local or static surfaces with no tenant dimension (local registries, workdir files, the plugin tree, brand assets) |

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/runs` | authenticated (no role) — another team than the active one, through `?team_id=` or `X-Iterion-Team`, needs `canViewTeam` | List runs |
| `GET` | `/api/runs/global-active` | authenticated (no role) | Active runs — running, queued, paused or `failed_resumable`; a `running` run quiet past 15 minutes reads as an orphan and is dropped — across the **local** stores on this machine — a desktop-daemon affordance walking the local iterion home. In cloud mode it returns `{"runs":[]}` unconditionally: the pod's `$HOME` is shared infrastructure, so walking it could leak across tenants. |
| `POST` | `/api/runs` | member | Launch a workflow |
| `POST` | `/api/v1/pipeline-board/tasks/{id}/launch` | member | Launch the bot a pipeline-board task names, bypassing the ready-state ranking — the same `gateLaunch` admission as `POST /api/runs` |
| `POST` | `/api/runs/preview-cost` | authenticated (no role) | Estimate cost before launch |
| `POST` | `/api/runs/uploads` | authenticated (no role) | Upload an attachment |
| `GET` | `/api/runs/{id}` | viewer (run tenant) | Run state |
| `GET` | `/api/runs/{id}/events` | viewer | Event log |
| `GET` | `/api/runs/{id}/workflow` | viewer | Workflow source attached to the run |
| `GET` | `/api/runs/{id}/artifacts/{node}` | viewer | Artifact versions for a node |
| `GET` | `/api/runs/{id}/artifacts/{node}/{version}` | viewer | One artifact |
| `GET` | `/api/runs/{id}/files` / `…/files/content` / `…/files/diff` | viewer | Working-tree views |
| `GET` | `/api/runs/{id}/commits` etc. | viewer | Worktree commit history |
| `GET` | `/api/runs/{id}/attachments/{name}` | viewer | Download an attachment |
| `GET` | `/api/runs/{id}/attachments/{name}/url` | viewer | Pre-signed S3 URL |
| `POST` | `/api/runs/{id}/cancel` | member | Cancel a running run |
| `POST` | `/api/runs/{id}/pause` | member | Pause |
| `POST` | `/api/runs/{id}/resume` | member | Resume (re-publishes through the queue) |
| `POST` | `/api/runs/{id}/fork` | member | Fork at a prior turn |
| `POST` | `/api/runs/{id}/merge` | member | Land the run's storage branch on a target branch. For a **repo-targeted** run — whose workspace is gone by the time it returns — the server materialises its own clone of the launch ref, runs the same merge pipeline, and pushes the advanced target back to the forge (never force). The merge is persisted as merged only once the forge has it; a refused push records `merge_status=failed`. |
| `GET` | `/api/runs/{id}/merge/conflicts` | viewer | Conflicting paths left by a merge attempt |
| `POST` | `/api/runs/{id}/merge/conflicts/resolve` | member | Resolve one conflicting path with supplied content |
| `POST` | `/api/runs/{id}/merge/conflicts/resolve-with-agent` | member | Delegate one conflict to the resolver agent (`ITERION_CONFLICT_RESOLVER_MODEL`) |
| `POST` | `/api/runs/{id}/merge/conflicts/finalize` | member | Commit the resolved merge and continue the pipeline |
| `POST` | `/api/runs/{id}/merge/conflicts/abort` | member | Abandon the merge; the storage branch is preserved |
| `POST` | `/api/runs/{id}/commit-and-finalize` | member | Commit pending work and finalise |
| `POST` | `/api/runs/{id}/rename` | member | Rename a run |
| `GET` | `/api/runs/{id}/children` | viewer | Child (subbot) runs |
| `GET` | `/api/runs/{id}/review/scope` / `…/review/diff` | viewer | Human-gate review scope and its diff ([review-scope.md](review-scope.md)) |
| `GET` | `/api/runs/{id}/session-board` | viewer | Session-board widgets ([session-board.md](session-board.md)) |
| `GET` | `/api/runs/{id}/interactions/pending` | viewer | Unanswered `ask_user_async` questions |
| `GET`/`POST`/`DELETE` | `/api/runs/{id}/queue-messages`, `…/queue-message`, `…/queue-message/{msgID}` | viewer (GET) · member (POST, DELETE) | Pending steering messages for the run |
| `GET` | `/api/runs/{id}/log` | viewer | Streamed run log |
| `GET` | `/api/runs/{id}/preview` | viewer | Preview proxy (SSRF-guarded) |
| `GET` | `/api/ws/runs/{id}/shell` | member | Post-mortem PTY in a preserved worktree — a local-mode operation: cloud answers `403` before the upgrade ([post-mortem-shell.md](post-mortem-shell.md)); while open, it asks again every 30 s whether its credential still stands |
| `GET` | `/api/ws/runs/{id}` | viewer | Live run-console WebSocket. The upgrade and `subscribe` are a read; the mutating envelopes (`cancel`, `pause`, `answer`, `queue_message`, `cancel_queued_message`, `bump_loop`, `raise_budget`) take a member on the same connection, and each asks again whether the socket's credential still stands (a refusal: an `error` envelope `credential_refused`, then close `1008` with a reason starting `credential_refused`) AND whether the caller still holds the run's scope (a lost scope — a membership removed, a demotion — answers an `error` envelope `forbidden` on that envelope and the socket stays; a read that does not complete refuses that envelope with `standing_unavailable` or `scope_unavailable` and the socket stays); the connection stops at 2 h (`ITERION_RUN_WS_MAX_LIFETIME`, close `1000` with a reason starting `lifetime_reached`) |
| `GET` | `/api/v1/runs/stats` | authenticated (no role) — another team than the active one, through `?team_id=` or `X-Iterion-Team`, needs `canViewTeam` | Rolling stats (for the studio) |
| `GET` | `/api/v1/limits/cost` | authenticated (no role) | Cost-cap status |
| `POST` | `/api/v1/limits/cost/override` | authenticated (no role) | Grant or revoke the day's cost-cap override. **Any authenticated caller with a team** — the handler performs no role check, and the route sits outside the run-scope prefixes, so the ADR-103 ladder does not apply either: a `viewer` passes. The audit actor is recorded as `operator`. A local-mode feature: cloud stores keep no spend ledger, so there the status reads `enabled: false` and the override answers `409`. |

Source: [pkg/server/runs.go](../pkg/server/runs.go).

## Super-admin (organisations + users + DLQ + audit)

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/admin/orgs` | super-admin | List orgs — the first 500, unpaginated (no `?offset=` / `?limit=`) |
| `POST` | `/api/admin/orgs` | super-admin | Create org |
| `GET` | `/api/admin/orgs/{id}` | super-admin | Read |
| `PATCH` | `/api/admin/orgs/{id}` | super-admin | Update name / slug / quotas |
| `DELETE` | `/api/admin/orgs/{id}` | super-admin | Schedule org deletion — 24h grace, restorable until the sweeper purges it. `409` on the caller's own ACTIVE org: switch org first |
| `POST` | `/api/admin/orgs/{id}/restore` | super-admin | Cancel a scheduled deletion |
| `POST` | `/api/admin/orgs/{id}/status` | super-admin | Suspend / read-only / activate |
| `GET` | `/api/admin/orgs/{id}/usage` | super-admin | Usage snapshot |
| `GET` | `/api/admin/orgs/{id}/teams` | super-admin | List the org's teams |
| `GET` | `/api/admin/users` | super-admin | List users (`?offset=&limit=` pagination, limit default 50 / max 200; `?q=` matches an email PREFIX or an exact user id — not a substring, so the match rides the unique index on `email`) |
| `GET` | `/api/admin/users/{id}` | super-admin | One account's file: status, last sign-in, whether a password sign-in is possible at all, its SSO links, and the orgs/teams it was **granted** — not what it could reach. See [administering accounts](ticket-context.md#an-account-signs-in-and-sees-nothing) |
| `PATCH` | `/api/admin/users/{id}` | super-admin | Status / super-admin flag |
| `POST` | `/api/admin/users/{id}/reset-password` | super-admin | Force a user's password reset; disabled accounts return 422 and must be explicitly re-enabled first; a personal access token answers `403` for its own account |
| `GET` | `/api/admin/audit` | super-admin | Platform audit log (filters: `action`, `actor`, `from`, `to`, `offset`, `limit`) |
| `GET` | `/api/admin/runs` | super-admin | Cross-org runs view (#2148): every org's runs in one list — the filters combine (`?org_id=`, `?team_id=`, `?status=`, `?since=`), pagination is server-side across orgs (`?limit=` 1..500, default 50; pass the answered `next_cursor` back as `?cursor=` — a malformed token is a 400), and each row names its team and org and carries the run's sovereign runner-pool stamp. Read-only: opening ONE run rides the ADR-103 choke point; every super-admin cross-team grant — this list included — is audited at platform scope through one named predicate |
| `GET` | `/api/admin/dlq` | super-admin | List parked messages |
| `GET` | `/api/admin/dlq/{seq}` | super-admin | Peek payload |
| `POST` | `/api/admin/dlq/{seq}/replay` | super-admin | Re-publish onto the live subject |
| `DELETE` | `/api/admin/dlq/{seq}` | super-admin | Discard |
| `GET` | `/api/admin/bots` | super-admin | List platform bot overrides (metadata + content digest) |
| `GET` | `/api/admin/bots/{slug}` | super-admin | One override incl. its file map |
| `PUT` | `/api/admin/bots/{slug}` | super-admin | Push/replace an override (`{files, version?}`; compiled before persisting; 413 over the body cap) |
| `DELETE` | `/api/admin/bots/{slug}` | super-admin | Remove the override (baked catalog serves again) |
| `PUT/DELETE` | `/api/admin/bots/{slug}/files/{path...}` | super-admin | Single-file edit of an override — `{path...}` is the multi-segment wildcard, so `skills/x/SKILL.md` matches |
| `POST` | `/api/admin/bots/{slug}/fork` | super-admin | Seed the override from the baked bundle (`{from}`) |
| `GET/PUT` | `/api/admin/settings/bot-roles` | super-admin | Webhook role→bot bindings (merge semantics; `null` clears a field) |
| `GET/PUT` | `/api/admin/settings/sandbox` | super-admin | `sandbox: auto` fallback image override |

Sources: [pkg/server/admin_orgs_routes.go](../pkg/server/admin_orgs_routes.go),
[pkg/server/admin_bots_routes.go](../pkg/server/admin_bots_routes.go),
[pkg/server/admin_runs_routes.go](../pkg/server/admin_runs_routes.go),
[pkg/server/platform_settings.go](../pkg/server/platform_settings.go),
[pkg/server/queue_sweeper.go](../pkg/server/queue_sweeper.go); the four user
rows are registered in [pkg/server/auth_routes.go](../pkg/server/auth_routes.go)
and handled in [pkg/server/auth_admin.go](../pkg/server/auth_admin.go) +
[pkg/server/admin_users_routes.go](../pkg/server/admin_users_routes.go), the
audit row in [pkg/server/audit_helper.go](../pkg/server/audit_helper.go).

## Server info + health

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/server/info` | public | Mode, version, `auth_required`, `email_enabled`, per-feature enablement flags, upload limits |
| `GET` | `/healthz` | public | Liveness — HTTP listener up |
| `GET` | `/readyz` | public | Readiness — every dependency the deployment wired (`Config.ReadinessChecks`), pinged concurrently under a per-check deadline (1s by default). A failing CRITICAL check is `503`; a non-critical one is reported `degraded` and still answers `200`. Draining or superseded is `503` without touching a dependency; no checks wired (local mode) is `200` |
| `GET` | `/metrics` | public on the metrics port (ClusterIP-only by design) | Prometheus scrape |

## Non-obvious JSON shapes

### `POST /api/teams/{id}/webhooks` — create response (token-once)

```json
{
  "config": {
    "id": "8e2…",
    "tenant_id": "team_acme",
    "name": "GitLab MR review",
    "provider": "gitlab",
    "sign_mode": "",
    "enabled": true,
    "token_last4": "Vp3a",
    "fingerprint": "sha256:…",
    "bot_ids": ["review-pr"],
    "wildcard_bots": false,
    "project_allowlist": ["acme/*"],
    "event_allowlist": [],
    "rate_limit": { "rate": 1.0, "burst": 10 },
    "monthly_call_limit": 0,
    "launch_vars": {},
    "key_overrides": {},
    "created_by": "user_…",
    "created_at": "2026-06-11T10:11:12Z",
    "updated_at": "2026-06-11T10:11:12Z"
  },
  "token": "iwh_…"
}
```

The `token` field is the **only** way to recover the plaintext. The
same shape comes back from the rotate endpoint.

### Launch-denial envelope

Every gate refusal — REST launch, resume, the custom-event emit, the
pipeline board's launch-now, webhook publication — uses the same shape
([pkg/server/launch_gate.go](../pkg/server/launch_gate.go)):

```jsonc
{
  "error":    "monthly_run_quota_exceeded",   // stable token
  "detail":   "monthly run quota (1000) exhausted",
  "reset_at": "2026-07-01T00:00:00Z"          // monthly quotas
}
```

Plus the header `Retry-After: <seconds>` on `concurrency_cap_exceeded`
and `launch_rate_limited`. The authorization refusal uses the same
envelope with `"error": "member_role_required"` and no `reset_at` —
nothing resets, the caller's role does; a refused board or trigger write
answers the same envelope without counting as a denied launch. Token list
and HTTP semantics in [quotas-and-limits.md](quotas-and-limits.md).

### `GET /api/orgs/{id}/usage` (also `/api/admin/orgs/{id}/usage`)

See [quotas-and-limits.md → Reading usage](quotas-and-limits.md#reading-usage)
for the full `orgUsageView` schema. Same shape on both routes — the
admin endpoint is super-admin only, the member endpoint is any org member.

### `POST /api/me/tokens` — create PAT

Request:

```json
{ "name": "github-actions", "team_id": "team_…", "expires_in_days": 90 }
```

Response (plaintext shown once):

```json
{
  "pat":    { "id": "…", "name": "github-actions", "token_last4": "Q9k2", "expires_at": "…", … },
  "token":  "iap_…",
  "org_id": "org_…"
}
```

`expires_in_days` is clamped down to `ITERION_PAT_MAX_TTL` when the
platform sets one. `org_id` is the pinned team's parent org, omitted for example when the
token carries no team or the org could not be resolved. `team_id` is optional
in form only: without it the mint pins the caller's ACTIVE team, and answers
`400` when the caller has none — an unpinned token could never authenticate.
A super-admin keeps unpinned tokens, which resolve to the owner's default team
at every use, or to no team. Membership and the team's existence are re-checked
on every call that resolves a team.

A token is minted from a live session only: the route answers `403` to a
personal access token (a token never mints another) and `401` to the
access token of a revoked session or of a disabled or pending account. It
asks again once the token's row is written, and revokes the token — never
answering its plaintext — when a revocation landed meanwhile. A token
lives until its own expiry or its revocation: by hand
(`DELETE /api/me/tokens/{token_id}`), by "sign out everywhere", or by an
administrator's disable; a password change and a reset keep it.
