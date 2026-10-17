// Who may write a roster row, and which roles they may write there.
//
// The server answers this on every roster write — `requireRankOverMember`
// on the org PATCH/DELETE and the team PATCH/DELETE/PUT, plus the cap on the
// role WRITTEN (the writer's org standing for an org role, their team role
// for a team one) — and both pages that render a roster mirror it from HERE
// rather than each deriving it. The org page and the team page apply the same
// two ladders; a second copy drifts the day a rung moves.
//
// This is a mirror, not the gate: the server reads the membership rows at
// request time and refuses whatever the UI offers. What it buys is a control
// that carries the reason instead of a 403 — so every input must be the row
// the server's guard reads (the ROSTER row, refetched after a write), never a
// token or an identity tree loaded once at sign-in.
//
// The rules, as the server enforces them:
//
//   A. A platform admin is exempt from all of them.
//   B. Org scope: a write does not reach a member whose org role is above the
//      writer's own standing, and the role WRITTEN stays at or below it.
//   C. Team scope, team with a parent org: the org ladder binds the same way
//      (C.1). When the writer's org standing is below admin — their authority
//      over the team is their own grant — the team ladder binds too: not over
//      a grant above their own (C.2.a), and not writing a team role above
//      their own (C.2.b), config_editor excepted since it is off the ladder.
//      An admin or owner of the parent org answers to the org ladder alone.
//   D. A team with no parent org has no org ladder: C.2 alone, on the grant.
//   E. Equal rank passes, which is what leaves a member their own row.
//
// A role the ladder cannot place is refused rather than written over, and the
// reason then names the only remedy that works, a platform admin.

import { ApiError } from "@/api/client";

import { ORG_ROLES, TEAM_ROLES, outranksViewer, roleLabel } from "./roles";

/** The writer, as the ladders read them. */
export interface ViewerStanding {
  /** is_super_admin — exempt from every rule below. */
  platformAdmin: boolean;
  /**
   * Their own org role, from the org ROSTER row the server's guard reads;
   * null when that roster carries no row for them, which is the standing the
   * server reads there too (ErrNotFound is rank 0, readOwnOrgStanding). A
   * roster that could NOT be read is not this value's business: it answers
   * `orgStandingBlock` below, which refuses the control rather than deciding
   * it at rank 0.
   */
  orgRole: string | null;
  /** Their own grant on the team, from the team roster row; null when none. */
  teamRole: string | null;
}

/** The row being written. */
export interface MemberRow {
  /** The member's org role, null when they hold no row (or none is known). */
  orgRole: string | null;
  /** The member's grant on the team, null when they hold none. */
  teamRole: string | null;
}

/**
 * The route the write takes. It decides which ladders bind and which remedy
 * a refusal may name: the org routes are gated on canManageOrg, so a member
 * holding the rank refused can make the write; the team routes are gated on
 * canManageTeam, which refuses a plain org member — naming one there would
 * send the operator to an actor who cannot do it.
 */
export type WriteScope =
  | { kind: "org" }
  /** `hasParentOrg: false` is a team whose org is gone or never was (D). */
  | { kind: "team"; hasParentOrg: boolean };

export interface WriteDecision {
  /** True when the write on this row is the viewer's to make. */
  allowed: boolean;
  /** Why it is refused, naming a remedy that works. Null when allowed. */
  reason: string | null;
}

const ALLOWED: WriteDecision = { allowed: true, reason: null };

/**
 * How a roster the decision reads answered.
 *
 * Two of the five are not an absence of knowledge. `absent`: there is no such
 * roster to read here — a team with no parent org has no org roster, and the
 * query that would load it is not asked (D). `refused`: the server answered,
 * and what it answered is that this viewer may not read it — the org roster is
 * gated on `canViewOrg`, which admits every member of the org (and every
 * platform admin), so a refusal states that they hold no membership in that
 * org. `loading` and `failed` are the unknowns, and an unknown refuses rather
 * than decides: the rows this page shows are the server's own, and a ladder
 * answered at "no row" while the roster is in flight, or after it failed,
 * offers a control the server refuses.
 */
export type RosterRead = "ready" | "loading" | "failed" | "refused" | "absent";

/**
 * How a roster query answered, in the one place that decides it: the pages
 * that read a roster mirror the same ladders from here, so they must classify
 * the same failure the same way.
 *
 * The error is read BEFORE the data, since react-query keeps the rows through
 * a failed refetch: a page holding stale rows and no current read of them
 * decides nothing from them. A 403 is kept apart from the other failures
 * because it is not an outage — see `RosterRead` — and in TEAM scope the
 * difference is a behaviour (`canManageFromRoster`).
 */
export function rosterReadOf(o: {
  /** False when there is no such roster here — a team with no parent org. */
  present: boolean;
  error: unknown;
  loaded: boolean;
}): RosterRead {
  if (!o.present) return "absent";
  if (o.error != null) {
    return o.error instanceof ApiError && o.error.status === 403 ? "refused" : "failed";
  }
  return o.loaded ? "ready" : "loading";
}

/** The viewer's own row in one roster, with how that roster answered. */
export interface OwnStanding {
  read: RosterRead;
  /**
   * Their role in it, null when it carries no row for them — which is a
   * standing of none, not an unknown: these rosters are whole (the server
   * lists every membership of the org / the team, unpaginated).
   */
  role: string | null;
}

/**
 * The team scope, derived from the one fact the page holds: whether the org
 * roster exists to be read. Deriving `hasParentOrg` from anything else is how
 * the ladder selection and the roster state start disagreeing. A roster this
 * viewer is refused is a roster that EXISTS, so the org ladder still binds the
 * scope — what answers that state is the gate below.
 */
export function teamScopeFor(org: RosterRead): WriteScope {
  return { kind: "team", hasParentOrg: org !== "absent" };
}

/**
 * orgStandingBlock answers why nothing the org ladder decides on a TEAM roster
 * is offered when the org roster that ladder reads did not give its rows — the
 * fail-closed half of the mirror: the ladder's inputs are rows, and a row that
 * is not there yet is not a row that says "no rank".
 *
 * Null when the ladders may decide: a platform admin (exempt from both, so no
 * roster decides anything for them), a team with no parent org (no org ladder
 * at all — D), or a roster that gave its rows (`ready` — a `refused` one
 * answered too, and gave none).
 *
 * `refused` is the one of the three whose refusal is the whole surface's and
 * not a row's: the gate (`canManageFromRoster`) closes there, so no row
 * renders a control to describe, and these words are what the page says
 * instead. They name the state — no membership in the team's organization —
 * and the remedy that repairs it, never a reload: the read is a standing
 * refusal, and re-issuing it answers 403 again.
 *
 * The ORG page needs no twin: there, the rows and the viewer's own standing
 * come from ONE read, so a roster that did not answer renders no row to
 * decide about.
 */
export function orgStandingBlock(
  viewer: ViewerStanding,
  org: RosterRead,
): WriteDecision | null {
  if (viewer.platformAdmin || org === "ready" || org === "absent") return null;
  return { allowed: false, reason: orgRosterBlockReason(org) };
}

/**
 * The deletion's own phrasing of the whole-surface unknowns: the row-shaped
 * sentence `orgStandingBlock` gives names "this row", and no row is the
 * referent of a control that deletes the team. The refused state keeps the
 * org block's sentence — it is already about the team's access.
 */
export function deleteStandingBlock(
  viewer: ViewerStanding,
  org: RosterRead,
): WriteDecision | null {
  if (viewer.platformAdmin || org === "ready" || org === "absent") return null;
  if (org === "loading") {
    return {
      allowed: false,
      reason:
        "Your standing in this team's organization is still loading: the team cannot be deleted until it is read.",
    };
  }
  if (org === "failed") {
    return {
      allowed: false,
      reason:
        "This team's organization roster could not be read: the team cannot be deleted until it is.",
    };
  }
  return orgStandingBlock(viewer, org);
}

function orgRosterBlockReason(org: "loading" | "failed" | "refused"): string {
  switch (org) {
    case "loading":
      return "Your standing in this team's organization is still loading: nothing on this row is offered until it is read.";
    case "failed":
      return "This team's organization roster could not be read, so nothing on this row is offered: reload the page, or ask an admin or owner of that organization, or a platform admin.";
    case "refused":
      return "You hold no membership in this team's organization, so no change to this team's access is offered here: an admin or owner of that organization, or a platform admin, adds you to it.";
  }
}

/**
 * canManageFromRoster answers the gate the server answers with canManageOrg /
 * canManageTeam — whether any write control belongs on this surface at all —
 * from the same rows the ladders read.
 *
 * It exists because moving the LADDER to the roster row and leaving the GATE
 * on the identity tree keeps both failures the move was for: the tree is
 * fetched once at sign-in and no roster write rewrites it, so a promotion
 * lands on controls that stayed dead and a self-demotion leaves controls the
 * server has started refusing.
 *
 * The arms are the server's, and all of them are rows: the viewer's own grant
 * on the team (team scope), an admin or owner of the org (orgAdminOfTeam,
 * which a team with no parent org does not have), plus the platform admin —
 * `is_super_admin`, the one viewer a roster cannot answer for, since they
 * hold no membership row. A roster that answered and carries no row for them
 * is a standing of NONE (these rosters are whole), and a roster that has not
 * answered grants nothing: no control is offered on a guess, and none on an
 * identity tree either. The tree says what the viewer held at sign-in, so on
 * a roster that has since dropped their row it arms a form the server answers
 * 403 to, beside controls that same roster has already disarmed.
 *
 * The grant arm has one state the server's own canManageTeam would take and
 * this does not: a team whose ORG roster the viewer is REFUSED. That refusal
 * says they hold no membership in the team's organization, which makes their
 * grant the `orphan_grant` state `useCanManageTeam` already answers false on
 * — a drift to repair, not a permission to widen from the client — and the
 * org ladder every row of such a team answers to has no rows to read. So the
 * surface is read-only there, with the reason `orgStandingBlock` gives, rather
 * than a form and rows whose every control is refused.
 */
export function canManageFromRoster(o: {
  scope: WriteScope;
  platformAdmin: boolean;
  /** Their row in the org roster — `absent` on a team with no parent org. */
  org: OwnStanding;
  /** Their row in the team roster; unused in org scope. */
  team: OwnStanding;
}): boolean {
  if (o.platformAdmin) return true;
  if (arm(o.org, (role) => covers(role, "admin", ORG_ROLES))) return true;
  // The grant is an arm of the TEAM gate only: it manages no org — and it is
  // no arm at all on a team whose org roster this viewer is refused, the
  // orphan grant above.
  if (
    o.scope.kind === "team" &&
    o.org.read !== "refused" &&
    arm(o.team, (role) => covers(role, "admin", TEAM_ROLES))
  ) {
    return true;
  }
  return false;
}

/**
 * One arm of the gate: what the row says, or no — never a guess. A roster with
 * no such row to give (`absent` — a team with no parent org has no org
 * roster), one that has not answered (`loading`, `failed`) and one this viewer
 * is refused all grant nothing here, which is why this arm is two-valued;
 * where the difference between them IS a behaviour, it is read from
 * `RosterRead` itself (the reason `orgStandingBlock` gives, the ladder
 * `teamScopeFor` selects, the grant arm `canManageFromRoster` withholds).
 */
function arm(s: OwnStanding, manages: (role: string) => boolean): boolean {
  return s.read === "ready" && s.role != null && manages(s.role);
}

/**
 * The org standing that binds a write in this scope. A team with no parent
 * org has none at all, whatever the viewer holds elsewhere (D).
 */
function orgStandingIn(viewer: ViewerStanding, scope: WriteScope): string | null {
  if (scope.kind === "team" && !scope.hasParentOrg) return null;
  return viewer.orgRole;
}

/** Whether a standing reaches a rung of its ladder — the server's AtLeast. */
function covers(standing: string | null, rung: string, ladder: readonly string[]): boolean {
  return !outranksViewer(rung, standing, ladder);
}

/** A rung this build cannot place is refused, and only a platform admin writes it. */
function unplaceable(role: string, ladder: readonly string[]): boolean {
  return !ladder.includes(role);
}

function unplaceableReason(role: string): string {
  return `This build cannot place the role ${roleLabel(role)} on its ladder, so nothing is written over it: a platform admin writes this row.`;
}

/** The deletion's own phrasing: the act is a delete, and the actor deletes. */
function unplaceableDeleteReason(role: string): string {
  return `This build cannot place the role ${roleLabel(role)} on its ladder: a platform admin deletes this team.`;
}

function orgLadderReason(role: string, teamScope: boolean): string {
  if (unplaceable(role, ORG_ROLES)) return unplaceableReason(role);
  return teamScope
    ? `Their role in this team's organization is ${role}, above your own standing there: an admin or owner of that organization, or a platform admin, writes this row.`
    : `Their org role is ${role}, above your own: a member of this org who already holds ${role}, or a platform admin, writes this row.`;
}

function teamLadderReason(role: string, hasParentOrg: boolean): string {
  if (unplaceable(role, TEAM_ROLES)) return unplaceableReason(role);
  const label = roleLabel(role);
  return hasParentOrg
    ? `Their team role is ${label}, above your own: a member of this team who already holds ${label}, an admin or owner of the team's organization, or a platform admin, writes this row.`
    : `Their team role is ${label}, above your own: a member of this team who already holds ${label}, or a platform admin, writes this row. This team has no organization to appeal to.`;
}

/**
 * canWriteMember answers whether a role change or a removal on this row is
 * the viewer's to make, and why not when it is not.
 *
 * An allowed row always carries its own current role among `assignableRoles`
 * below — both answers are the same comparison against the same standing —
 * so a select rendered for a writable row can offer the filtered ladder
 * without misrepresenting what is stored.
 */
export function canWriteMember(
  viewer: ViewerStanding,
  row: MemberRow,
  scope: WriteScope,
): WriteDecision {
  if (viewer.platformAdmin) return ALLOWED;
  const teamScope = scope.kind === "team";
  const standing = orgStandingIn(viewer, scope);
  // B / C.1 — the org ladder binds every write whose org is known, team-scope
  // ones included. A member with no org row stands below every rung, so the
  // ladder has nothing to refuse there.
  if (
    (!teamScope || scope.hasParentOrg) &&
    row.orgRole != null &&
    outranksViewer(row.orgRole, standing, ORG_ROLES)
  ) {
    return { allowed: false, reason: orgLadderReason(row.orgRole, teamScope) };
  }
  // C.2.a / D — the team ladder binds when the viewer's authority over the
  // team is their own grant. An admin or owner of the parent org manages
  // every team in it and answers to the org ladder alone.
  if (
    scope.kind === "team" &&
    !covers(standing, "admin", ORG_ROLES) &&
    row.teamRole != null &&
    outranksViewer(row.teamRole, viewer.teamRole, TEAM_ROLES)
  ) {
    return {
      allowed: false,
      reason: teamLadderReason(row.teamRole, scope.hasParentOrg),
    };
  }
  // E — equal rank, a lower one, and the viewer's own row.
  return ALLOWED;
}

/**
 * assignableRoles names the roles the viewer may write in this scope, in
 * ladder order — the cap on the role WRITTEN, which is a separate refusal
 * from the one above: it applies to every row the viewer CAN write, their own
 * included (a team admin writes no owner, on anyone).
 */
export function assignableRoles(
  viewer: ViewerStanding,
  scope: WriteScope,
): readonly string[] {
  if (scope.kind === "org") {
    if (viewer.platformAdmin) return ORG_ROLES;
    // B — up to their own standing.
    return ORG_ROLES.filter((r) => !outranksViewer(r, viewer.orgRole, ORG_ROLES));
  }
  const standing = orgStandingIn(viewer, scope);
  if (viewer.platformAdmin || covers(standing, "admin", ORG_ROLES)) return TEAM_ROLES;
  // C.2.b — up to their own grant. config_editor survives the filter
  // because it is off the ladder and above nothing on it (ADR-078), which is
  // the exception the server makes for it rather than a case stated twice.
  return TEAM_ROLES.filter((r) => !outranksViewer(r, viewer.teamRole, TEAM_ROLES));
}

/**
 * roleOptions are the options a select on one row must carry: the roles the
 * viewer may write, plus the row's own role when that list does not hold it.
 *
 * The ladders never hide the stored role of a row they let the viewer write —
 * both answers are the same comparison — but a platform admin is exempt from
 * them and reaches a row whose role no rung carries (an org row left holding
 * `config_editor`, a value this build cannot place). A `<select>` whose value
 * matches no option renders the FIRST one, i.e. shows a role the member does
 * not hold.
 */
export function roleOptions(
  offered: readonly string[],
  current: string,
): readonly string[] {
  return offered.includes(current) ? offered : [...offered, current];
}

/**
 * reconcileRole keeps a form's held role consistent with the list it is
 * actually offered, for the surfaces that GRANT a role rather than show the
 * one a row holds: a `<select>` whose value matches no option renders the
 * FIRST one, so a list that shrinks under a held pick — the cap narrowing when
 * the roster it reads lands — would submit a role the operator never saw
 * selected.
 *
 * The pick when it is still offered; otherwise the HIGHEST role that is (the
 * lists arrive in ladder order, least-privileged first), which is the nearest
 * thing to the gesture — falling back to the first would hand them
 * config_editor, a different capability rather than a lower rank, on every
 * list that still carries a rung. A list of ONLY config_editor answers it
 * either way, and that is not this rule being wrong: it is the whole
 * invitation an org-less minter — a team admin or owner whose parent org
 * carries no row for them — may mint, the one cell the server accepts from
 * them (`team / org:none / team:admin / config_editor` → 200, every rung →
 * 403). The select shows it, so nothing is submitted unseen. Null when
 * nothing is offered, which is a form with nothing to submit.
 */
export function reconcileRole(
  offered: readonly string[],
  picked: string,
): string | null {
  if (offered.includes(picked)) return picked;
  return offered[offered.length - 1] ?? null;
}

/**
 * assignableRolesNote explains a ladder the viewer sees only part of, for the
 * roster whose select offers it — a role missing with no word is read as a
 * deployment that lost it. Null when the whole ladder is offered.
 */
export function assignableRolesNote(
  viewer: ViewerStanding,
  scope: WriteScope,
): string | null {
  const offered = assignableRoles(viewer, scope);
  const ladder: readonly string[] = scope.kind === "org" ? ORG_ROLES : TEAM_ROLES;
  // Only rungs can be missing: the role off the ladder is never capped, so
  // the list never needs the label that renders it.
  const missing = ladder.filter((r) => !offered.includes(r));
  if (missing.length === 0) return null;
  const list = missing.join(", ");
  if (scope.kind === "org") {
    return `Roles above your own are not offered (${list}): writing one takes that standing in this org, or a platform admin.`;
  }
  return scope.hasParentOrg
    ? `Roles above your own are not offered (${list}): writing one takes that role on this team, an admin or owner of the team's organization, or a platform admin.`
    : `Roles above your own are not offered (${list}): writing one takes that role on this team, or a platform admin.`;
}

/**
 * canDeleteTeam answers whether DELETING this team (DELETE /api/teams/{id})
 * is the viewer's to make, and why not when it is not. The server's rule is
 * the org ladder over every grant the deletion clears
 * (requireRankOverTeamMembers): the route's gate is canManageOrg — a team
 * admin's grant reaches no deletion — and no member of the team may stand
 * above the author on the org ladder. The TEAM ladder does not apply: the
 * deletion is an org-scoped act over every grant at once, not a write whose
 * author's authority is their own grant on one of them.
 *
 * The rows are the ones the page already joins for the ladder: each member's
 * org role from the org roster (null where they hold none, which stands below
 * every rung — an orphan grant blocks no deletion), and each member's team
 * grant from the team roster, whose own read is this function's first
 * refusal. A role the build cannot place is refused rather than deleted over.
 * The org roster's whole-surface unknowns — in flight, failed, or refused —
 * are `orgStandingBlock`'s answers on the page, composed before this one is
 * asked (the same pattern as canWriteMember), so this function decides only
 * from rows that were read.
 *
 * Only the team scope reaches here — the org page has no team of its own to
 * delete — and a team with no parent org is refused for EVERYONE: the
 * server's gate refuses before it reads any org, a platform admin included,
 * whose exemption lives only where that gate can read an org. An orphan team
 * joins an organization through `migrate orgs` before any delete reaches it;
 * this surface offers no delete on one.
 */
export function canDeleteTeam(
  viewer: ViewerStanding,
  memberOrgRoles: readonly (string | null)[],
  scope: WriteScope,
  /** The team roster's own read: the deletion decides from its rows, so an
   * unread one refuses the control before any rank is compared. */
  teamRead: RosterRead = "ready",
): WriteDecision {
  // The page's own uncertainty comes first: rows the page has not read are
  // not rows the rule decides from.
  if (teamRead === "loading" || teamRead === "failed" || teamRead === "refused") {
    return {
      allowed: false,
      reason:
        teamRead === "loading"
          ? "The team's roster has not finished loading: the team cannot be deleted until it is read."
          : "The team's roster could not be read: the team cannot be deleted until it is.",
    };
  }
  // The route's first refusal, mirrored: a team with no parent org answers
  // at the server's gate — the explicit OrgID disjunct refuses EVERYONE
  // there, before canManageOrg runs and before the platform-admin exemption
  // is reached.
  if (scope.kind !== "team" || !scope.hasParentOrg) {
    return {
      allowed: false,
      reason:
        "This team has no organization: nobody deletes it from this surface — an orphan team joins an organization through `migrate orgs` before any delete reaches it.",
    };
  }
  if (viewer.platformAdmin) return ALLOWED;
  // The route's gate, mirrored: canManageOrg reads an org admin or owner.
  // A viewer below that rung holds no deletion to be refused FROM — the
  // remedy names the standing that has it.
  if (!covers(viewer.orgRole, "admin", ORG_ROLES)) {
    return {
      allowed: false,
      reason:
        "Deleting a team takes an admin or owner standing in its organization: an admin or owner of that organization, or a platform admin, deletes this team.",
    };
  }
  for (const role of memberOrgRoles) {
    if (role == null) continue; // no org row: below every rung (B)
    if (unplaceable(role, ORG_ROLES)) {
      return { allowed: false, reason: unplaceableDeleteReason(role) };
    }
    if (outranksViewer(role, viewer.orgRole, ORG_ROLES)) {
      return {
        allowed: false,
        reason: `Their role in this team's organization is ${role}, above your own standing there: an owner of this organization, or a platform admin, deletes this team.`,
      };
    }
  }
  return ALLOWED;
}

/**
 * The org role an accepted invitation at this TEAM role mirrors up to — the
 * studio twin of identity.OrgRoleForTeamRole. Null where there is no mirror:
 * config_editor (rank 0, off the ladder — ADR-078), which is why no invitation
 * cap touches it, and any value this build cannot place, which invitableRoles
 * refuses outright rather than minting uncapped.
 */
export function orgMirrorOfTeamRole(role: string): string | null {
  if (unplaceable(role, TEAM_ROLES) || role === "config_editor") return null;
  return outranksViewer(role, "member", TEAM_ROLES) ? "admin" : "member";
}

/**
 * invitableRoles filters an invitation role picker's ladder down to the roles
 * this viewer's invitation may carry — the two caps the minting routes apply
 * (K), which are NOT the cap on a roster write:
 *
 *  1. the ORG cap (requireOrgStandingToInvite): accepting an invitation writes
 *     the team grant AND, for a role on the ladder, an org membership at the
 *     role's org MIRROR — so the mirror must not be above the minter's own org
 *     standing. It applies where the team has a parent org to read it in.
 *  2. the TEAM cap: where the minter's authority over the team is their own
 *     grant — no parent org, or an org standing below admin — the minted team
 *     role must not be above their own, the same bound as the roster write.
 *
 * `offered` is the picker's own ladder: the team pickers offer TEAM_ROLES, the
 * org page's offers the three org-ladder names, which are team roles too (an
 * org invitation joins a team in the org). A platform admin is exempt, and
 * config_editor — no org mirror, off the team ladder — is capped by neither.
 */
export function invitableRoles(
  viewer: ViewerStanding,
  scope: WriteScope,
  offered: readonly string[],
): readonly string[] {
  if (viewer.platformAdmin) return offered;
  const standing = orgStandingIn(viewer, scope);
  // There is an org to read the mirror in: the team has a parent org, or the
  // invitation is minted from the org route, which joins a team of that org.
  const orgCap = scope.kind === "org" || scope.hasParentOrg;
  // requireRankToInvite's own shape: the grant is measured unless an org
  // standing of admin or above answered for the team, whatever the route. On
  // the org route that standing is what the gate reads, so this cap binds
  // nothing there (measured: every org-route cell below admin is refused by
  // the gate) — but it is written from the standing, not from the route, so
  // the exemption rests on the row rather than on which form was used.
  const teamCap = !orgCap || !covers(standing, "admin", ORG_ROLES);
  return offered.filter((role) => {
    // A value no rung of the team ladder carries is not minted from here: the
    // server refuses it as invalid, and a cap read off a rank of zero would
    // let it through.
    if (unplaceable(role, TEAM_ROLES)) return false;
    const mirror = orgMirrorOfTeamRole(role);
    if (mirror == null) return true;
    if (orgCap && !covers(standing, mirror, ORG_ROLES)) return false;
    if (teamCap && outranksViewer(role, viewer.teamRole, TEAM_ROLES)) return false;
    return true;
  });
}

/**
 * invitableRolesNote explains an invitation ladder the viewer sees only part
 * of, for the same reason assignableRolesNote does: a role missing with no
 * word reads as a deployment that lost it. Null when nothing is capped.
 */
export function invitableRolesNote(
  viewer: ViewerStanding,
  scope: WriteScope,
  offered: readonly string[],
): string | null {
  const allowed = invitableRoles(viewer, scope, offered);
  const missing = offered.filter((r) => !allowed.includes(r));
  if (missing.length === 0) return null;
  const list = missing.map(roleLabel).join(", ");
  return `Roles an invitation of yours cannot carry are not offered (${list}): accepting one grants that role on the team and mirrors it up to an org membership, and neither may be above your own standing.`;
}
