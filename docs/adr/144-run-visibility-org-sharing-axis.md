# ADR-144: run visibility — an org sharing axis, not a second tenancy

Status: accepted (plan reviewed by codex gpt-6-astra xhigh, 9 findings, 2026-10-27)
Date: 2026-10-27
Issue: SocialGouv/iterion#2744

## Context

Every run a bot publishes outside the deployment names the run that wrote it —
the forge footer, the Mattermost digest footer (#2430). The link is the
reader's way to the evidence: steps, models, cost. Today that way answers 404
for anyone who is not a member of the run's team: the run-by-id choke point
(ADR-103) admits a standing IN the run's team, plus super-admins and
org-admin lineage, and nothing else. The evidence chain breaks exactly where
a colleague first meets the fleet.

The ask: a SocialGouv colleague, admitted manually, can READ the runs of the
veille teams (Vigie) — and, where the operator accepts the exposure, the
review teams (Revi).

## Options considered

**A second org ("SocialGouvPublic").** Rejected. An org is a tenancy: its own
teams, credentials, quotas, board. Run authorization resolves a standing in
the RUN's team and the org arm follows THAT team's org — a principal of
another org reaches nothing. The org would have to be populated with
membership rows in the production teams to grant anything, at which point it
granted nothing an org is for. Duplication is not a sharing axis.

**Viewer memberships in bulk in the ops teams.** Rejected: far wider than
run reads (team lists, board reads, stats), unrevokable per purpose, and it
conflates "works here" with "may see this team's evidence".

**Signed per-run share links (WS-ticket style).** Rejected as the system
answer: a bearer token in a forge comment is precisely what the run-link rule
refuses to publish, and per-link sharing is unmanageable at fleet scale.

**Chosen — a per-team sharing axis.** `Team.RunVisibility` (`""` team-private
default, `"org"` shared) read LIVE at the run-by-id choke point: an
org-membership of the run team's parent org reads as a `viewer`, marked
`Via: org-shared`. The audience is the ORG ROSTER (not a curated sub-team —
the review showed every team-role mirror and SSO grant already lands in the
org roster, so a curated team would promise a revocation guarantee it cannot
keep). Admission is the org roster's existing manual flows; revocation is
removing the org-membership.

## Decision

1. `identityInTeamOn`'s no-membership arm gains, on the SAME store reads the
   org-admin-lineage arm already makes, one more grant: a plain org member of
   the team's parent org, on a team whose `RunVisibility == "org"`, reads as
   `RoleViewer` with `Via: "org-shared"`. Precedence is unchanged: a team
   membership row still wins (a `config_editor` still sees no runs —
   ADR-078), and org-admin lineage keeps its own mark.
2. The axis grants READ only. `runScopeLadder` is untouched: every action,
   `browser/cdp` and the post-mortem shell included, stays behind the member
   rung.
3. The field is written ONLY through `PUT /api/teams/{id}/run-visibility`,
   gated `orgAdminOfTeam` (exposure is a governance act, like suspension)
   and audited to BOTH scopes (team + org), the `handleSetTeamStatus`
   precedent. `""` is an explicit reset; an absent field is refused; an
   unknown value is refused at the route and read as team-private at the
   choke point.
4. Passive run sockets re-derive their standing: `subscribe`/`subscribe_logs`
   carry no envelope of their own, so a subscribed connection re-asks, every
   `ITERION_RUN_WS_STANDING_TICK` (default 5m), that its credential stands
   and a viewer standing in the run's team still holds — the revocation the
   acceptance criterion promises covers the transports that never speak.
5. The run payloads (GET + WS snapshot) carry the caller's resolved standing
   (`viewer: {role, via}`), attached after the deterministic build; the
   console derives its action affordances from it instead of offering
   commands the ladder will refuse.

## Consequences

- **The shared run is shared WHOLE.** Artifacts are raw by design
  (docs/secrets.md), worktree files and logs carry what the run carried. The
  flag is all-or-nothing per team, so each activation is preceded by a
  written exposure inventory for that team (runbook); Vigie's is trivial,
  Revi's is the operator's explicit decision (private-repo diffs).
- **Presigned attachment URLs outlive revocation** by their TTL (≤ 24h) — a
  power every viewer already had for their own teams; documented here as a
  known exception. A behavioral test awaits a blob-store test harness
  (deferred follow-up).
- **The audience is the org roster.** Granting someone "the veilles" means
  granting them every org-shared team at once; the operator curates the
  roster, the flags curate the surface.
- **Rollout is fail-closed**: an old replica does not know the axis and
  refuses (404) — deploy every replica before opening flags; rollback is
  closing the flags first.
- No board/list access, no read audit (the `Via` mark is stamped on the
  resolved identity for the future), no write path, no self-service
  admission: all deliberate V1 non-goals.
