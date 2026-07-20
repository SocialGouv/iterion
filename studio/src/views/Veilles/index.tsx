import { useAuth, canEditConfigShares } from "@/auth/AuthContext";
import { InlineBanner } from "@/components/ui";
import { Workspace } from "@/views/configEditorShell/Workspace";

/**
 * VeillesView — the config-share editor as a first-class route in the FULL
 * studio (with sidebar), for accounts that can edit config-shares but aren't
 * boxed into the minimal ConfigEditorShell (admins/owners/super-admins, and a
 * config_editor whose access was later broadened).
 *
 * It renders the SAME Workspace as the minimal shell, so the editing UX is
 * identical — the veille editor simply "grows a home" in the full app instead
 * of only existing as a whole-shell landing. Gated at the nav/hub level by
 * canEditConfigShares; the friendly guard here covers a stray deep-link.
 */
export default function VeillesView() {
  const { activeTeamID, activeTeam, activeRole, user } = useAuth();

  if (!canEditConfigShares(activeRole, !!user?.is_super_admin)) {
    return (
      <div className="p-4 sm:p-6">
        <div className="mx-auto max-w-3xl">
          <InlineBanner tone="warning" layout="inline" title="Not available">
            You don't have access to edit veille config-shares on this team.
          </InlineBanner>
        </div>
      </div>
    );
  }

  return (
    <div className="h-full overflow-auto p-4 sm:p-6">
      <div className="mx-auto max-w-5xl">
        {activeTeamID ? (
          <Workspace teamID={activeTeamID} teamName={activeTeam?.team_name} />
        ) : (
          <InlineBanner tone="warning" layout="inline" title="No active team">
            Select a team to edit its veille config-shares.
          </InlineBanner>
        )}
      </div>
    </div>
  );
}
