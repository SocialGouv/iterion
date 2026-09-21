import { hasOrgRole, useAuth } from "@/auth/AuthContext";
import { findTeamGrant } from "@/hooks/useTenantSubject";

// useCanManageTeam centralises the "can administer this team's resources" rule
// so the team-scoped settings pages share one definition instead of
// re-deriving it inline.
//
// It MIRRORS the server's `canManageTeam` (pkg/server/auth_authz.go), which is
// a super-admin, OR admin/owner on the team, OR admin/owner of the team's ORG
// (`orgAdminOfTeam`). All three arms are load-bearing: reading the team role
// alone looks equivalent only because `buildOrgTree` synthesises `admin` for an
// org admin — and it does that ONLY where no explicit grant exists
// (auth_views.go: `role, granted := teamRole[t.ID]`). An org admin who also
// holds an explicit lower row on that team therefore arrives as `viewer`, and
// a UI reading the team role alone hides every control the server would accept.
//
// `teamID` is REQUIRED. It used to read the role on the ACTIVE team, which is
// a different team whenever a page was reached by URL. Making the subject
// implicit is what let that spread to six sites, so the parameter is not
// optional — every caller already holds the id.
export function useCanManageTeam(teamID: string): boolean {
  const { orgs, user } = useAuth();
  if (user?.is_super_admin) return true;
  // findTeamGrant is the one lookup useTeamSubject uses too: "may I see it"
  // and "may I manage it" must not answer from two pictures of the caller. It
  // returns the owning ORG as well, which is what makes the third arm free.
  const grant = findTeamGrant(orgs, teamID);
  if (!grant) return false;
  if (grant.team.role === "admin" || grant.team.role === "owner") return true;
  return hasOrgRole(grant.org.org_role || null, "admin");
}
