// Extracted from ConnectRepoWizard.tsx to keep that file focused.
// useConnectWizard owns the wizard's data + navigation state: the
// team's forge connections and OAuth apps, the URL-derived current
// step, the returnTo round-trip mirror, and the repo-capable bot list
// feeding EnableRepoPanel.

import { useCallback, useEffect, useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";

import {
  type ForgeConnection,
  type ForgeOAuthApp,
  type ForgeProvider,
  listForgeConnections,
  listForgeOAuthApps,
} from "@/api/forgeConnections";
import { errorMessage } from "@/lib/errorHints";
import { isRepoCapable } from "@/lib/triggers";
import { useBotsStore } from "@/store/bots";
import { sanitizeReturnTo } from "@/views/integrations/wizard/bindModel";

import { CONNECT_RETURN_KEY, STEP_ORDER, withQuery, type Step } from "./model";

export type ConnectWizardNavigate = (
  to: string,
  opts?: { replace?: boolean },
) => void;

const NO_CONNECTIONS: ForgeConnection[] = [];
const NO_OAUTH_APPS: ForgeOAuthApp[] = [];

export function useConnectWizard(
  teamID: string,
  q: URLSearchParams,
  navigate: ConnectWizardNavigate,
) {
  // Step mutations (PAT connect, App install, …) report their failures
  // through setError; fetch failures surface from the queries. One error
  // slot is exposed — a step error wins, and reload clears it.
  const [actionError, setActionError] = useState<string | null>(null);

  // The connections key is shared with RepoTargetSection, so a connection
  // added here is fresh there (and vice versa) without a manual bridge.
  const connectionsQuery = useQuery({
    queryKey: ["forge-connections", teamID],
    queryFn: () => listForgeConnections(teamID),
  });
  const oauthAppsQuery = useQuery({
    queryKey: ["forge-oauth-apps", teamID],
    queryFn: () => listForgeOAuthApps(teamID),
  });
  const connections = connectionsQuery.data ?? NO_CONNECTIONS;
  const oauthApps = oauthAppsQuery.data ?? NO_OAUTH_APPS;
  const loading = connectionsQuery.isLoading || oauthAppsQuery.isLoading;
  const fetching = connectionsQuery.isFetching || oauthAppsQuery.isFetching;
  const fetchError = connectionsQuery.error ?? oauthAppsQuery.error;
  const error =
    actionError ?? (fetchError && !fetching ? errorMessage(fetchError) : null);

  const { refetch: refetchConnections } = connectionsQuery;
  const { refetch: refetchOAuthApps } = oauthAppsQuery;
  const reload = useCallback(async () => {
    setActionError(null);
    await Promise.all([refetchConnections(), refetchOAuthApps()]);
  }, [refetchConnections, refetchOAuthApps]);

  const connectedID = q.get("connected") ?? "";
  const installedAppID = q.get("installed") ?? "";
  const explicitStep = q.get("step") as Step | null;
  const providerParam = (q.get("provider") as ForgeProvider | null) ?? "github";
  const baseParam = q.get("base") ?? "";

  // A returning App-install / OAuth round-trip forces the step; otherwise
  // the URL's ?step= wins, falling back to "provider".
  const step: Step = connectedID
    ? "repos"
    : explicitStep && STEP_ORDER.includes(explicitStep)
      ? explicitStep
      : "provider";

  // The bind wizard (and any caller) can hand us a ?returnTo= in-app path
  // to land on after the connect flow. It must survive both SPA step
  // navigation (kept in the URL by gotoStep) and the OAuth/App round-trip
  // whose `next` URL we don't control fully — hence the sessionStorage
  // mirror, restored when the operator comes back with ?connected=.
  const returnTo =
    sanitizeReturnTo(q.get("returnTo")) ??
    sanitizeReturnTo(sessionStorage.getItem(CONNECT_RETURN_KEY));
  useEffect(() => {
    const fromURL = sanitizeReturnTo(q.get("returnTo"));
    if (fromURL) sessionStorage.setItem(CONNECT_RETURN_KEY, fromURL);
  }, [q]);
  const clearReturnTo = () => sessionStorage.removeItem(CONNECT_RETURN_KEY);

  const gotoStep = (next: Step, extra?: Record<string, string>) => {
    const p = new URLSearchParams();
    p.set("step", next);
    if (returnTo) p.set("returnTo", returnTo);
    if (extra) for (const [k, v] of Object.entries(extra)) if (v) p.set(k, v);
    navigate(withQuery(p), { replace: false });
  };

  const restart = () => {
    clearReturnTo();
    navigate(withQuery(new URLSearchParams()));
  };

  // Bot catalog — repo-capable bots feed EnableRepoPanel just like the
  // existing IntegrationsTab does.
  const allBots = useBotsStore((s) => s.bots);
  const botsWarning = useBotsStore((s) => s.error);
  const fetchBots = useBotsStore((s) => s.fetch);
  useEffect(() => {
    void fetchBots();
  }, [fetchBots]);
  const repoBots = useMemo(() => (allBots ?? []).filter(isRepoCapable), [allBots]);

  return {
    connections,
    oauthApps,
    loading,
    error,
    setError: setActionError,
    reload,
    connectedID,
    installedAppID,
    providerParam,
    baseParam,
    step,
    returnTo,
    clearReturnTo,
    gotoStep,
    restart,
    repoBots,
    botsWarning,
  };
}
