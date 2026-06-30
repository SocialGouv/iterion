import { useAuth } from "@/auth/AuthContext";

// useCanManageTeam centralises the "can administer this team's resources" rule
// (admin/owner role, or a super-admin) so the team-scoped settings pages share
// one definition instead of re-deriving it inline.
export function useCanManageTeam(): boolean {
  const { activeRole, user } = useAuth();
  return (
    activeRole === "admin" || activeRole === "owner" || (user?.is_super_admin ?? false)
  );
}
