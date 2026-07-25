import { useEffect, useMemo } from "react";

import { useQuery } from "@tanstack/react-query";

import {
  forgeTeamRepoKey,
  listTeamForgeRepos,
  type ForgeTeamRepo,
} from "@/api/forgeConnections";
import { useAuth } from "@/auth/AuthContext";
import { useActiveRepoStore } from "@/store/activeRepo";
import { useServerInfoStore } from "@/store/serverInfo";

// Stable empty fallback so the undefined→loaded transition doesn't hand
// consumers a fresh [] reference each render.
const EMPTY: ForgeTeamRepo[] = [];

export interface UseActiveRepoResult {
  /** The active repo row, or null (overview mode / nothing connected). */
  activeRepo: ForgeTeamRepo | null;
  /** True when the operator explicitly chose "All repos" (overview). */
  overview: boolean;
  repos: ForgeTeamRepo[];
  loading: boolean;
  /** Cloud mode with a team context — false hides every repo affordance. */
  enabled: boolean;
  /** Pick a repo key (forgeTeamRepoKey), or null for "All repos". */
  choose: (key: string | null) => void;
  teamID: string | null;
}

// The studio's repo-first context: most views scope to one connected
// repo; actions that target a repo pre-fill from it. Fetches the team's
// connected repos (30s lazy poll, like useRunRepos) and reconciles the
// persisted per-(user,team) choice — auto-selecting the first connected
// repo when there is no valid stored one.
export function useActiveRepo(): UseActiveRepoResult {
  const { user, activeTeam } = useAuth();
  const isCloud = useServerInfoStore((s) => s.info?.mode === "cloud");
  const userID = user?.id ?? null;
  const teamID = activeTeam?.team_id ?? null;
  const enabled = isCloud && !!teamID && !!userID;

  const query = useQuery<ForgeTeamRepo[]>({
    queryKey: ["team-forge-repos", teamID],
    queryFn: () => listTeamForgeRepos(teamID ?? ""),
    enabled,
    refetchInterval: 30_000,
    refetchIntervalInBackground: false,
  });
  const repos = query.data ?? EMPTY;

  const selection = useActiveRepoStore((s) => s.selection);
  const hydrate = useActiveRepoStore((s) => s.hydrate);
  const reconcile = useActiveRepoStore((s) => s.reconcile);
  const choose = useActiveRepoStore((s) => s.choose);

  // (Re)hydrate whenever the (user, team) identity changes.
  useEffect(() => {
    if (userID && teamID) hydrate(userID, teamID);
  }, [userID, teamID, hydrate]);

  // Repo-first reconciliation once the list lands (and on refetches, so
  // a disconnected repo clears itself).
  useEffect(() => {
    if (enabled && query.data) reconcile(query.data);
  }, [enabled, query.data, reconcile]);

  const activeRepo = useMemo(() => {
    if (selection.kind !== "repo") return null;
    return repos.find((r) => forgeTeamRepoKey(r) === selection.key) ?? null;
  }, [selection, repos]);

  return {
    activeRepo,
    overview: selection.kind === "all",
    repos,
    loading: query.isLoading,
    enabled,
    choose,
    teamID,
  };
}
