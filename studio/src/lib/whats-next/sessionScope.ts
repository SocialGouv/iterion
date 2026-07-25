// Repo-first scope resolution for a whats-next session.
//
// The scope key is what a session is bound to: the local project dir in
// local/desktop mode, and `(team, active repo)` in cloud — switching
// the sidebar repo must switch conversations, not silently resurface a
// session bound to another repo. The key participates in both the
// localStorage session memory and the startup-discovery filtering.

import { forgeTeamRepoKey, type ForgeTeamRepo } from "@/api/forgeConnections";
import { useActiveRepo } from "@/hooks/useActiveRepo";
import { useServerInfoStore } from "@/store/serverInfo";

export interface SessionScope {
  // Storage/discovery scope key. Null only in local mode before the
  // server info resolves a project id.
  scopeKey: string | null;
  // Cloud mode with a team context — repo-first scoping active.
  repoScopeEnabled: boolean;
  // True when the operator chose "All repos" (overview).
  overview: boolean;
  // The sidebar's active repo row, or null.
  activeRepo: ForgeTeamRepo | null;
  // Local-mode project id (kept around for effect dep parity).
  projectId: string | null;
  // The repo the NEXT launch will operate on (cloud: the sidebar's
  // active repo). Null = board-only launch.
  launchRepo: string | null;
}

export function useSessionScope(): SessionScope {
  const projectId = useServerInfoStore(
    (s) => s.info?.current_project_id ?? null,
  );
  // Repo-first scope (cloud): the launch targets the sidebar's active
  // repo, the session key includes it, and discovery filters by it —
  // so switching repos switches conversations instead of silently
  // resurfacing a session bound to another repo. Inert outside cloud.
  const {
    activeRepo,
    overview,
    enabled: repoScopeEnabled,
    teamID,
  } = useActiveRepo();
  const scopeKey = repoScopeEnabled
    ? `${teamID ?? "_team"}:${activeRepo ? forgeTeamRepoKey(activeRepo) : "all"}`
    : projectId;

  return {
    scopeKey,
    repoScopeEnabled,
    overview,
    activeRepo,
    projectId,
    launchRepo:
      repoScopeEnabled && activeRepo?.clone_url
        ? activeRepo.repo_full_name
        : null,
  };
}
