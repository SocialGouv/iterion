// Page hook for the org-administration console: the orgs-list query, the
// shared busy/error slot every mutation rides, the refresh-after-mutation
// sequencing, and the drawer selection. Gating (super-admin, cloud mode)
// stays in the page component — it arrives here only as `enabled`, so
// local mode never fires a doomed 404.

import { useCallback, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";

import { listOrgs, type OrgView } from "@/api/orgs";
import { FeatureUnavailableError } from "@/api/usage";
import { useAsyncAction } from "@/hooks/useAsyncAction";

export interface UseOrgsAdminResult {
  orgs: OrgView[];
  /** False only while the initial fetch is pending (drives the skeleton). */
  loaded: boolean;
  busy: boolean;
  bannerErr: string | null;
  /** The org whose drawer is open; null when closed. */
  active: OrgView | null;
  setActive: (o: OrgView | null) => void;
  refresh: () => Promise<void>;
  run: (fn: () => Promise<unknown>) => Promise<void>;
}

export function useOrgsAdmin(enabled: boolean): UseOrgsAdminResult {
  const queryClient = useQueryClient();
  const orgsQuery = useQuery<OrgView[]>({
    queryKey: ["admin-orgs"],
    queryFn: () => listOrgs(),
    enabled,
  });
  const orgs = orgsQuery.data ?? [];
  // Drives the initial TableSkeleton only — post-mutation refetches keep
  // the table on screen (isPending stays false once data exists).
  const loaded = !orgsQuery.isPending;
  const [active, setActive] = useState<OrgView | null>(null);
  // Single useAsyncAction underpins every mutation (create, update,
  // status); the list fetch error shares its banner slot, with the
  // FeatureUnavailable exception mapped to a friendlier line.
  const { busy, error, run: actionRun } = useAsyncAction();
  const fetchErr = orgsQuery.error
    ? orgsQuery.error instanceof FeatureUnavailableError
      ? "Organization administration is a cloud-mode feature — it isn't available on this server (local/desktop mode)."
      : String(orgsQuery.error)
    : null;
  const bannerErr = error ?? fetchErr;

  // Refetch the list; awaited by callers that sequence identity reloads
  // after it, exactly like the manual refresh was.
  const refresh = useCallback(
    () => queryClient.invalidateQueries({ queryKey: ["admin-orgs"] }),
    [queryClient],
  );

  // Wrap a mutation in the shared busy/error slot, then refresh the
  // list once it settles successfully. useAsyncAction maps the thrown
  // error to its hook's errorMessage().
  const run = useCallback(
    async (fn: () => Promise<unknown>) => {
      const ok = await actionRun(async () => {
        await fn();
        return true;
      });
      if (ok) await refresh();
    },
    [actionRun, refresh],
  );

  return { orgs, loaded, busy, bannerErr, active, setActive, refresh, run };
}
