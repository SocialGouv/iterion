# ADR-078 — Config-editor role: SSO-team-scoped access to config-share

Status: accepted · 2026-07-18

Extends [ADR-076](076-scoped-config-share-editor.md) (scoped config-share
editor) and [ADR-077](077-declarative-per-bot-config-share-surface.md). ADR-076
shipped the editor as a **bearer capability URL** (`iws_` token, synthetic
`KindShare` principal). This ADR adds a second, complementary access path: a
**real user, authenticated via org SSO, holding a least-privilege `config_editor`
capability on a team**, who reaches only the config-share editor views for that
team's bots — nothing else in the studio.

## Context

The capability-URL model (ADR-076) is right for a **truly external** editor
(no account, no SSO): you hand them a link, zero onboarding. But for the actual
first user — internal design/a11y colleagues who curate the veille config — it
has real weaknesses the operator felt in practice:

- it is a **bearer secret** (leaks if the URL is pasted/forwarded/synced);
- **no identity or attribution** — the delivery audit records IP/UA, not *who*;
- **no revoke-by-person** — you rotate the token (breaking everyone's link) or
  delete the share;
- keeping a standing link alive needed the **"never expires"** opt-out
  (ADR-077 follow-up) — a permanent credential, the classic smell.

For colleagues who are (or can be) in the org's SSO, the better model is: access
is a property of **team membership + a scoped role**. In the team with the
config-editor capability → you can edit that team's bots' config; removed from
the team → access is gone. No token, no expiry, real identity.

The two models are **complementary, not competing**: capability-URL for the
account-less external case, SSO-team-scoped for the internal case.

## Decision

Add a `config_editor` capability and an SSO-authenticated editor surface, built
so it does **not** weaken the ADR-076 token path.

1. **`config_editor` is an ORTHOGONAL capability, not a rank on the role
   ladder.** The team role ladder is strictly linear (`viewer < member < admin
   < owner`, compared by `AtLeast`), and — critically — `canViewTeam` admits any
   `Role.Valid()` principal, *not* `AtLeast(RoleViewer)`. So a naive "below
   viewer" linear role would silently gain broad team read. Instead,
   `config_editor` is a distinct role value that the **standard gates
   (`canViewTeam`/`canManageTeam`/`canViewOrg`) explicitly reject**, and that
   only a **new `canEditConfigShares(team)` gate** accepts. It grants exactly one
   thing: edit this team's config-shares. Nothing else.

2. **A SEPARATE authenticated endpoint — the token path stays cookie-less.**
   The public editor routes (`/api/config-share/{id}/…`) keep their Bearer-only,
   `credentials:"omit"`, structurally-CSRF-immune contract (ADR-076). The
   real-user path is a new group under the normal session auth:
   `GET /api/teams/{id}/config-shares` (already exists, now also readable by a
   config-editor), `GET|PATCH /api/teams/{id}/config-shares/{sid}/config` behind
   `requireAuth + canEditConfigShares`, reusing the SAME
   `configShareSvc.ProjectedRead`/`ApplyEdit` the public handlers call. The
   real user is `KindUser` (not synthetic), so it passes the `IsSynthetic()`
   guard cleanly — ADR-076's synthetic rejection is untouched and is *not* a
   blocker here.

3. **A role-keyed limited studio shell.** `RestrictedShell` today triggers on
   "member of zero teams" and shows the marketplace. A config-editor *is* a team
   member, so a **new branch in `AuthGate`** — keyed on the active role being
   `config_editor` — renders a sibling limited shell that mounts ONLY the
   config-share editors for the team's bots, reached through the **normal cookie
   session client** (`@/api/client`), not the `iws_`-hardwired `configShare.ts` /
   `ConfigShareView`.

4. **No org over-grant.** A team membership normally mirrors up to an
   `OrgRoleMember`, which passes `canViewOrg` (org roster/usage/settings read).
   A `config_editor` must **not** mirror to org membership — it stays purely
   team-and-config-scoped.

5. **Attribution.** The `configshare.Delivery` audit row gains an `actor` field,
   so a real-user edit is attributable to the user (the token path keeps its
   `share:<id>` actor).

6. **SSO assignment.** `config_editor` is assignable through the existing SSO
   role plumbing — a GitHub "config editors" team → `GitHubTeamGrant{Role:
   config_editor}`, or an OIDC provider `DefaultRole: config_editor` — once the
   role value is `Valid()`. The operator UI adds `config_editor` to the team
   members role picker (invite + change-role), which validates only
   `role.Valid()`, so no member-management handler changes.

## Scope (MVP) + follow-ups

- **MVP: team-scoped.** A `config_editor` can edit **all** of their team's
  config-shares. There is no member→bot binding today; per-bot / per-share
  narrowing (a config-editor limited to `feed-watch/a11y` only) is a follow-up
  (natural home: a scoped field on `Membership` or a join consulted by
  `canEditConfigShares`).
- Follow-ups: per-bot/per-share member binding; an org-level "config editor only"
  concept if org context is ever needed; email-invite copy tailored to the
  config-editor role.

## Consequences

- The operator gets a **no-token, revoke-by-membership** way to delegate config
  editing to colleagues, with real identity + attribution — the durable-access
  need that previously forced a never-expiring token.
- The capability-URL path is **unchanged and still the right tool** for
  account-less external editors. `never_expires` remains, but is no longer the
  *only* way to grant standing access.
- One new low-privilege capability enters the auth model. It is deliberately
  **out of the linear ladder** so it can never be mistaken for "viewer+" by a
  gate that checks `Valid()` — the single biggest correctness hazard, closed by
  a golden test asserting `config_editor` is rejected by every standard gate and
  admitted by `canEditConfigShares` alone.

Reference: [docs/config-share.md](../config-share.md).
