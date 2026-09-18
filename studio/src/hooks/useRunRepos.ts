import { errorMessage } from "@/lib/errorHints";
import { useQuery } from "@tanstack/react-query";

import { listRunRepos, type RunRepo } from "@/api/runs";
import { useAuth } from "@/auth/AuthContext";

// Stable empty fallback so the undefined→loaded transition doesn't hand
// consumers a fresh [] reference each render.
const EMPTY: RunRepo[] = [];

export interface UseRunReposResult {
  repos: RunRepo[];
  loading: boolean;
  error: string | null;
}

// Fetches the distinct repositories (cloud project_path) that have runs,
// with counts — feeds the run-list "by repo" filter chips. Decoupled
// from the runs list itself so selecting a repo (which narrows the list)
// doesn't make the other repo chips vanish.
//
// Cloud-mode only: pass `enabled: false` in local/desktop mode (where
// runs carry no project_path and folder chips are derived client-side).
// Polls lazily — the repo set changes far slower than the runs list, so
// a 30s refetch keeps new repos appearing without hammering the server.
export function useRunRepos(enabled: boolean): UseRunReposResult {
  // The distinct-repos set is server-scoped to the active team (cloud
  // only — this hook is disabled in local mode). Keying by team means a
  // team switch invalidates the chips instead of showing the previous
  // team's repos. `enabled` already gates this to cloud, so activeTeam
  // is always present when the query runs.
  const { activeTeam } = useAuth();
  const teamID = activeTeam?.team_id ?? null;
  const query = useQuery<RunRepo[]>({
    queryKey: ["run-repos", teamID],
    queryFn: () => listRunRepos(),
    enabled,
    refetchInterval: 30_000,
    refetchIntervalInBackground: false,
  });

  return {
    repos: query.data ?? EMPTY,
    loading: query.isLoading,
    error: query.error ? errorMessage(query.error) : null,
  };
}
