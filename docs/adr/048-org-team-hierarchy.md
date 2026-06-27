# ADR-048 — Org → Teams two-level hierarchy

Status: **Proposed** (scoping — not yet implemented)
Date: 2026-06-27

## Context

Today iterion's tenancy model is **flat: a "team" IS the tenant** — one
`identity.Team` type (id / name / slug / role / quotas), no separate "org"
concept. A user belongs to many teams and switches the *active* team (stored on
the JWT as `id.TeamID`); the studio's `UserTeamChip` already exposes a team
switcher. Every tenant-scoped store keys on this single id: `forge.Connection`,
`boardmongo` (board per team), `secrets`/OAuth, `marketplace` viewer, `orgusage`
quotas, `audit`, `pat`, webhooks.

The flat model conflates two things operators want to separate:
- **Org** = the billing/identity boundary: SSO, member roster, plan + quotas,
  the unit you'd call "SocialGouv".
- **Team** = a working sub-unit inside an org: its own board, forge
  connections, enabled bots, runs — e.g. "platform", "data", "revi-squad".

The confusing left-nav chip ("SocialGouv" with `SocialGouv/socialgouv` on the
team page) is a symptom: it's really the *tenant* switcher, but there's no level
above it, so an org with several squads can't model them without creating
several unrelated top-level "teams" that don't share SSO/billing/members.

## Decision (proposed)

Introduce a **two-level hierarchy: Org → Teams**.

- **Org** owns: members (with org-roles: owner/admin/member), SSO config, plan +
  quotas/cost-cap, billing, the marketplace-submitter identity, audit.
- **Team** (belongs to exactly one Org) owns: board, forge connections + repo
  integrations, enabled bots, runs, team-scoped secrets. A team-role
  (admin/member/viewer) is granted per (user, team).
- A user is a **member of an Org** and is granted access to **0..N teams**
  within it. Active context = **(org_id, team_id)**; both ride the JWT.
- Personal org/team auto-created on signup (preserves today's single-user UX).

### Key design decisions to lock

1. **Tenant key.** Two options for the stores currently keyed by `tenant=team_id`:
   - (A) **Keep `team_id` as the tenant key** for resource stores (board, forge,
     secrets, runs); add `org_id` only where org-level (members, SSO, quotas,
     billing, marketplace). *Smallest migration* — existing team-scoped data is
     untouched; we only add an Org layer above + an `org_id` column on teams.
   - (B) Re-key everything on `(org_id, team_id)`. Cleaner long-term, **large
     migration** of every collection + index.
   → **Recommend (A)**: org is additive; resources stay team-keyed. Quotas/usage
     move to org-level (sum across its teams).

2. **Quotas/usage.** Move `orgusage` + cost-cap + run-quota from per-team to
   **per-org** (the launch gate sums a team's run against its org budget). Teams
   can optionally carry a sub-cap.

3. **SSO + members.** `auth`/`orgSSO` + the member roster move to **org** scope;
   team membership becomes a grant within the org (you must be an org member to
   be added to its teams).

4. **JWT + active context.** Add `org_id` alongside `team_id`; `/api/auth/me`
   returns the org tree (orgs → teams the user can see); two switchers (org, then
   team) — or one combined "Org / Team" picker.

5. **Naming/UX.** Left-nav chip becomes an explicit **Org picker** (chevron +
   "Organisation"), with the Team picker nested or adjacent. Resolves the "c'est
   le bazar" confusion.

## Migration (existing flat teams)

Each current `Team` becomes an **Org with one default Team** carrying the same
id (so every team-keyed resource — board/forge/secrets/runs — keeps working
unchanged under option A). The Org gets the team's name/slug/quotas/SSO/members;
the default Team inherits the resources. Idempotent backfill: for each team,
create `org_id=<new>`, set `team.org_id`, move org-level fields up. Personal
teams → personal orgs.

## Phased rollout

- **P0 — model + migration (no UX change):** add `identity.Org` + `org_id` on
  Team + the Org store + the backfill; JWT carries `org_id` (= the team's new
  org); quotas/usage/SSO/members re-pointed to org. Everything else unchanged —
  one org per team, invisible to users.
- **P1 — multiple teams per org:** allow creating Teams within an Org; team
  switcher scoped to the active org; team-role grants.
- **P2 — studio UX:** Org picker + Team picker; org settings (members/SSO/
  billing) vs team settings (board/forge/bots) split; the chip cleanup.
- **P3 — org admin:** invite to org, manage team grants, per-team sub-caps.

## Risks / open questions

- Migration is the crux — every tenant-scoped store + the JWT + the auth gates
  must agree on the new context. Option (A) bounds it (resources stay
  team-keyed) but the auth/usage/SSO re-point is still broad and needs a careful,
  reversible, well-tested backfill (mirror the cloud Mongo migration pattern).
- Forge/board/marketplace are team-scoped (good) but **marketplace submitter** +
  **audit** are arguably org-scoped — confirm per-surface.
- Do we need cross-team resource sharing within an org (shared forge connection,
  shared secret)? Defer to post-P2.

## Alternatives considered

- **Keep flat (status quo).** Multi-team already works via the switcher; the only
  real gap is no shared SSO/billing/members across a user's teams. Cheapest, but
  doesn't satisfy "an org with several squads".
- **Re-key everything on (org, team)** (option B). Cleanest, highest migration
  cost/risk. Rejected for now in favour of additive option (A).
