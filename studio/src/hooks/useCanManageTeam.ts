import { useAuth } from "@/auth/AuthContext";

// useCanManageTeam centralises the "can administer this team's resources" rule
// (admin/owner role, or a super-admin) so the team-scoped settings pages share
// one definition instead of re-deriving it inline.
//
// Pass `teamID` when the page addresses a team by id rather than working on
// the active one. Without it the answer comes from `activeRole`, which is the
// role on the ACTIVE team — right for the surfaces that operate there
// (integrations, triggers, schedules, repo detail) and wrong for a page
// reached by URL: an org admin opening a team of their org that is not the
// active one would see no controls, though the server accepts their writes.
export function useCanManageTeam(teamID?: string): boolean {
  const { activeRole, activeTeamID, orgs, user } = useAuth();
  if (user?.is_super_admin) return true;

  if (teamID && teamID !== activeTeamID) {
    // The identity tree already resolves the org-admin case: buildOrgTree
    // lists every team of an org the caller administers, with an implied
    // admin role. Reading it here keeps ONE definition of "may manage"
    // rather than a second ladder that would drift from the server's.
    for (const org of orgs) {
      const grant = org.teams.find((t) => t.team_id === teamID);
      if (grant) return grant.role === "admin" || grant.role === "owner";
    }
    // Not in the tree at all: no grant to read, and we are not a
    // super-admin. Refuse rather than fall through to the active team's
    // role, which describes a different team.
    return false;
  }

  return activeRole === "admin" || activeRole === "owner";
}
