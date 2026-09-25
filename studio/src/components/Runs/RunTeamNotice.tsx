import { useEffect, useState } from "react";

import { useAuth } from "@/auth/AuthContext";
import { getTeam } from "@/api/orgs";

// RunTeamNotice discloses, per ADR-103, that the open run belongs to a
// team other than the caller's active one: by-id reads are served from the
// RUN's team, so actions taken here apply to a team the sidebar does not
// show as active. Hidden when the run's team is the active team (the
// common case), unknown, or the run carries no team at all (local mode).
export function RunTeamNotice({ tenantId }: { tenantId?: string }) {
  const { activeTeamID } = useAuth();
  const [name, setName] = useState<string | null>(null);

  useEffect(() => {
    if (!tenantId || tenantId === activeTeamID) return;
    let alive = true;
    getTeam(tenantId)
      .then((team) => {
        if (alive) setName(team.name || tenantId);
      })
      .catch(() => {
        if (alive) setName(tenantId);
      });
    return () => {
      alive = false;
    };
  }, [tenantId, activeTeamID]);

  if (!tenantId || !activeTeamID || tenantId === activeTeamID) return null;
  return (
    <div
      data-testid="cross-team-run-notice"
      className="border-b border-border-default bg-bg-subtle px-4 py-1.5 text-xs text-fg-muted"
    >
      This run belongs to another team
      {name ? ` — ${name}` : ""}. Actions here apply to that team.
    </div>
  );
}
