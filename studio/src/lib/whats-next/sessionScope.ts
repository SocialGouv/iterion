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
  // True once the key has SETTLED, i.e. every input it is derived from has
  // resolved. It is false during a cold load, when the key is still walking
  // its way to the real value.
  //
  // A consumer that treats every key change as a scope SWITCH needs this:
  // the key is not stable at mount. Locally it is `projectId`, null until
  // server-info lands. In cloud it also waits on auth, the active team, and
  // the team-repos query, so it walks `null -> "team:all" -> "team:<repo>"`
  // — two changes that are resolution, not a switch, and are indistinguishable
  // from one by comparing keys alone.
  ready: boolean;
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
    loading: reposLoading,
    teamID,
  } = useActiveRepo();
  const serverInfoLoaded = useServerInfoStore((s) => s.info !== null);
  const isCloud = useServerInfoStore((s) => s.info?.mode === "cloud");
  const scopeKey = repoScopeEnabled
    ? `${teamID ?? "_team"}:${activeRepo ? forgeTeamRepoKey(activeRepo) : "all"}`
    : projectId;

  return {
    scopeKey,
    // In cloud the key is only settled once repo scoping is actually on
    // (auth + team resolved) AND the repos query has landed, because both
    // move the key. Locally, server-info alone decides it.
    ready: serverInfoLoaded && (!isCloud || (repoScopeEnabled && !reposLoading)),
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
