import { useAuth } from "@/auth/AuthContext";
import { findTeamGrant } from "@/hooks/useTenantSubject";

// useCanManageTeam centralises the "can administer this team's resources" rule
// (admin/owner role, or a super-admin) so the team-scoped settings pages share
// one definition instead of re-deriving it inline.
//
// `teamID` is REQUIRED. It used to read the role on the ACTIVE team, which is
// a different team whenever a page was reached by URL: an org admin opening a
// team of their org would see no controls, though the server accepts their
// writes. Making the subject implicit is what let that spread to six sites, so
// the parameter is not optional — every caller already holds the id.
export function useCanManageTeam(teamID: string): boolean {
  const { orgs, user } = useAuth();
  if (user?.is_super_admin) return true;
  // findTeamGrant is the one lookup useTeamSubject uses too: "may I see it"
  // and "may I manage it" must not answer from two pictures of the caller.
  const role = findTeamGrant(orgs, teamID)?.team.role;
  return role === "admin" || role === "owner";
}
