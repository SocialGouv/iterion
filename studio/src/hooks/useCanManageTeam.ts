import { useAuth } from "@/auth/AuthContext";
import { managesTeam } from "@/hooks/useTenantSubject";

// useCanManageTeam centralises the "can administer this team's resources" rule
// so the team-scoped settings pages share one definition instead of
// re-deriving it inline.
//
// It follows the server's `canManageTeam` (pkg/server/auth_authz.go) — a
// super-admin, OR admin/owner on the team, OR admin/owner of the team's ORG
// (`orgAdminOfTeam`) — as far as the identity tree can express it. All three
// arms are load-bearing: reading the team role alone looks equivalent only
// because `buildOrgTree` synthesises `admin` for an org admin, and it does
// that ONLY where no explicit grant exists (auth_views.go:
// `role, granted := teamRole[t.ID]`). An org admin who also holds an explicit
// lower row on that team therefore arrives as `viewer`, and a UI reading the
// team role alone hides every control the server would accept.
//
// NOT an exact mirror, and the gap is known: the server's arms read the
// membership store, while this reads `buildOrgTree`, which enumerates orgs
// from `ListOrgMembershipsByUser`. A team grant whose org membership is
// missing is therefore absent from the tree entirely, and this answers false
// on a team the server would let the caller administer. That state is the
// `orphan_grant` the admin console exists to surface — a drift to repair,
// not a permission to widen from the client.
//
// `teamID` is REQUIRED. It used to read the role on the ACTIVE team, which is
// a different team whenever a page was reached by URL. Making the subject
// implicit is what let that spread to six sites, so the parameter is not
// optional — every caller already holds the id.
//
// The rule itself is managesTeam (useTenantSubject), which a reader that may
// hold no session — or already holds the identity — applies directly.
export function useCanManageTeam(teamID: string): boolean {
  const { orgs, user } = useAuth();
  return managesTeam(orgs, !!user?.is_super_admin, teamID);
}
