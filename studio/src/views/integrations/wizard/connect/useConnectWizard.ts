// Extracted from ConnectRepoWizard.tsx to keep that file focused.
// useConnectWizard owns the wizard's data + navigation state: the
// team's forge connections and OAuth apps, the URL-derived current
// step, the returnTo round-trip mirror, and the repo-capable bot list
// feeding EnableRepoPanel.

import { useCallback, useEffect, useMemo, useState } from "react";

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

export function useConnectWizard(
  teamID: string,
  q: URLSearchParams,
  navigate: ConnectWizardNavigate,
) {
  const [connections, setConnections] = useState<ForgeConnection[]>([]);
  const [oauthApps, setOAuthApps] = useState<ForgeOAuthApp[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const reload = useCallback(async () => {
    setError(null);
    try {
      const [conns, apps] = await Promise.all([
        listForgeConnections(teamID),
        listForgeOAuthApps(teamID),
      ]);
      setConnections(conns);
      setOAuthApps(apps);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setLoading(false);
    }
  }, [teamID]);

  useEffect(() => {
    void reload();
  }, [reload]);

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
    setError,
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
