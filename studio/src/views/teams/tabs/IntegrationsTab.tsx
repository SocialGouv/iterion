import { errorMessage } from "@/lib/errorHints";
import { useEffect, useMemo, useState } from "react";
import { useSearch } from "wouter";

import { FeatureUnavailableError } from "@/api/client";
import {
  type ForgeConnection,
  type ForgeIntegration,
  type ForgeOAuthApp,
  listForgeConnections,
  listForgeIntegrations,
  listForgeOAuthApps,
} from "@/api/forgeConnections";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { useConfirm } from "@/hooks/useConfirm";
import { isRepoCapable } from "@/lib/triggers";
import { useBotsStore } from "@/store/bots";
import OAuthConnections from "@/views/settings/OAuthConnections";

import { ConnectForm } from "./integrations/ConnectForm";
import { ConnectionCard } from "./integrations/ConnectionCard";
import { OAuthAppsSection } from "./integrations/OAuthAppsSection";

export default function IntegrationsTab({
  teamID,
  canManage,
}: {
  teamID: string;
  canManage: boolean;
}) {
  const [connections, setConnections] = useState<ForgeConnection[]>([]);
  const [integrations, setIntegrations] = useState<ForgeIntegration[]>([]);
  const [oauthApps, setOAuthApps] = useState<ForgeOAuthApp[]>([]);
  const [unavailable, setUnavailable] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const { confirm, dialog } = useConfirm();
  // Bots come from the shared catalog cache so a metadata edit (e.g. in
  // the Bot panel) re-renders the connection cards immediately. We surface
  // every repo-capable bot — one that declares an invocations: block (forge
  // event / slash-command / schedule / board) or a legacy forge: block — not
  // just the two Revi bots. See lib/triggers.isRepoCapable.
  const allBots = useBotsStore((s) => s.bots);
  const botsWarning = useBotsStore((s) => s.error);
  const fetchBots = useBotsStore((s) => s.fetch);
  const repoCapableBots = useMemo(
    () => (allBots ?? []).filter(isRepoCapable),
    [allBots],
  );
  // ?bot=<name> (set by the catalog's "Connect to a repo" affordance) pre-checks
  // that bot in the enable dialog and auto-opens it when there's one connection.
  const preselectBot = new URLSearchParams(useSearch()).get("bot") ?? undefined;

  const reload = async () => {
    setErr(null);
    try {
      const [conns, ints, apps] = await Promise.all([
        listForgeConnections(teamID),
        listForgeIntegrations(teamID),
        listForgeOAuthApps(teamID),
      ]);
      setConnections(conns);
      setIntegrations(ints);
      setOAuthApps(apps);
    } catch (e) {
      if (e instanceof FeatureUnavailableError) {
        setUnavailable(true);
        return;
      }
      setErr(errorMessage(e));
    }
  };

  useEffect(() => {
    void reload();
    void fetchBots();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [teamID]);

  // Re-fetch when the tab/window regains focus. The GitHub App-Manifest flow
  // (and any OAuth-app registration done in another tab) navigates away and
  // returns here WITHOUT a teamID change, so the [teamID] effect above never
  // re-runs — without this the Connect form keeps showing a stale
  // "OAuth (no app)" for an app that was just registered, until a hard reload.
  useEffect(() => {
    const refetch = () => {
      if (document.visibilityState === "visible") void reload();
    };
    window.addEventListener("focus", refetch);
    document.addEventListener("visibilitychange", refetch);
    return () => {
      window.removeEventListener("focus", refetch);
      document.removeEventListener("visibilitychange", refetch);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [teamID]);

  if (unavailable) {
    return (
      <InlineBanner tone="info" layout="inline">
        Forge integrations are not enabled on this server. They require the cloud control
        plane (Mongo-backed connection + webhook stores).
      </InlineBanner>
    );
  }

  return (
    <div className="space-y-6">
      {dialog}
      {err && (
        <InlineBanner tone="danger" layout="inline">
          {err}
        </InlineBanner>
      )}
      {botsWarning && (
        <InlineBanner tone="warning" layout="inline">
          {botsWarning}
        </InlineBanner>
      )}

      <div>
        <h3 className="font-medium mb-1">Connected forges</h3>
        <p className="text-xs text-fg-muted mb-3">
          Connect a GitLab/GitHub/Forgejo account once, then enable a bot on a repo — iterion
          creates the webhook on the forge and wires the bot's token for you.
        </p>
        {connections.length === 0 ? (
          <div className="text-fg-muted text-sm">No forge connected yet.</div>
        ) : (
          <div className="space-y-3">
            {connections.map((c) => (
              <ConnectionCard
                key={c.id}
                teamID={teamID}
                conn={c}
                integrations={integrations.filter((i) => i.connection_id === c.id)}
                repoBots={repoCapableBots}
                canManage={canManage}
                onChanged={reload}
                onError={setErr}
                confirm={confirm}
                preselectBot={preselectBot}
                autoOpenEnable={!!preselectBot && connections.length === 1}
              />
            ))}
          </div>
        )}
      </div>

      <OAuthAppsSection
        teamID={teamID}
        apps={oauthApps}
        connections={connections}
        canManage={canManage}
        onChanged={reload}
        onError={setErr}
        confirm={confirm}
      />

      {canManage && (
        <ConnectForm
          teamID={teamID}
          oauthApps={oauthApps}
          onConnected={reload}
          onError={setErr}
        />
      )}

      {canManage && (
        <div className="pt-4 border-t border-border-subtle">
          <OAuthConnections scope={{ teamId: teamID }} org />
        </div>
      )}
    </div>
  );
}
