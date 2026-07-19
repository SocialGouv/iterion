import { errorMessage } from "@/lib/errorHints";
import { type ReactNode, useEffect, useMemo, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useLocation, useSearch } from "wouter";
import { CheckIcon } from "@radix-ui/react-icons";
import { ChevronRight } from "lucide-react";

import { FeatureUnavailableError } from "@/api/client";
import {
  listForgeConnections,
  listForgeIntegrations,
  listForgeOAuthApps,
} from "@/api/forgeConnections";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { EmptyState } from "@/components/ui/EmptyState";
import { useConfirm } from "@/hooks/useConfirm";
import { isRepoCapable } from "@/lib/triggers";
import { useBotsStore } from "@/store/bots";

import { ConnectForm } from "./integrations/ConnectForm";
import { connectionStatusLabel } from "./integrations/connectionLabels";
import { ConnectionCard } from "./integrations/ConnectionCard";
import { OAuthAppsSection } from "./integrations/OAuthAppsSection";
import SchedulesSection from "./integrations/SchedulesSection";

export default function IntegrationsTab({
  teamID,
  canManage,
}: {
  teamID: string;
  canManage: boolean;
}) {
  const queryClient = useQueryClient();
  // Mutations in the child sections report through setActionErr; load
  // failures surface from the queries. One banner shows whichever is
  // current.
  const [actionErr, setActionErr] = useState<string | null>(null);
  const { confirm, dialog } = useConfirm();

  // The connections + OAuth-apps keys are shared with the connect wizard
  // and RepoTargetSection, so a connection added there is fresh here
  // without a manual bridge.
  const connectionsQuery = useQuery({
    queryKey: ["forge-connections", teamID],
    queryFn: () => listForgeConnections(teamID),
  });
  const integrationsQuery = useQuery({
    queryKey: ["forge-integrations", teamID],
    queryFn: () => listForgeIntegrations(teamID),
  });
  const oauthAppsQuery = useQuery({
    queryKey: ["forge-oauth-apps", teamID],
    queryFn: () => listForgeOAuthApps(teamID),
  });
  const connections = connectionsQuery.data ?? [];
  const integrations = integrationsQuery.data ?? [];
  const oauthApps = oauthAppsQuery.data ?? [];
  const fetching =
    connectionsQuery.isFetching || integrationsQuery.isFetching || oauthAppsQuery.isFetching;
  const fetchError =
    connectionsQuery.error ?? integrationsQuery.error ?? oauthAppsQuery.error;
  const unavailable =
    connectionsQuery.error instanceof FeatureUnavailableError ||
    integrationsQuery.error instanceof FeatureUnavailableError ||
    oauthAppsQuery.error instanceof FeatureUnavailableError;
  const err =
    actionErr ??
    (fetchError && !unavailable && !fetching ? errorMessage(fetchError) : null);
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
  const search = useSearch();
  const preselectBot = new URLSearchParams(search).get("bot") ?? undefined;
  const [, navigate] = useLocation();

  // "Manual setup (advanced)" folds the legacy Connect-a-forge form + the
  // OAuth-apps registry away — the /integrations/connect wizard is the
  // recommended path. Closed by default, but auto-opened when the URL says
  // the operator is mid-manual-flow: an OAuth / GitHub-App return lands here
  // with ?connected= / ?installed= (the manual forms are where the result
  // shows), and a #connect-forge-form anchor deep-links straight to it.
  const oauthReturn = useMemo(() => {
    const q = new URLSearchParams(search);
    return q.has("connected") || q.has("installed");
  }, [search]);
  const [manualOpen, setManualOpen] = useState(
    () => oauthReturn || window.location.hash === "#connect-forge-form",
  );
  useEffect(() => {
    if (oauthReturn) setManualOpen(true);
  }, [oauthReturn]);
  useEffect(() => {
    if (window.location.hash === "#connect-forge-form") {
      setManualOpen(true);
      document
        .getElementById("connect-forge-form")
        ?.scrollIntoView({ behavior: "smooth", block: "center" });
    }
  }, []);

  const reload = () => {
    setActionErr(null);
    void queryClient.invalidateQueries({ queryKey: ["forge-connections", teamID] });
    void queryClient.invalidateQueries({ queryKey: ["forge-integrations", teamID] });
    void queryClient.invalidateQueries({ queryKey: ["forge-oauth-apps", teamID] });
  };

  useEffect(() => {
    void fetchBots();
  }, [fetchBots]);

  // Re-fetch when the tab/window regains focus. The GitHub App-Manifest flow
  // (and any OAuth-app registration done in another tab) navigates away and
  // returns here WITHOUT a teamID change, and the app-wide query client has
  // refetchOnWindowFocus off — without this the Connect form keeps showing a
  // stale "OAuth (no app)" for an app that was just registered, until a hard
  // reload.
  useEffect(() => {
    const refetch = () => {
      if (document.visibilityState === "visible") reload();
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

      <WiringGuide
        forgeConnected={connections.length > 0}
        repoEnabled={integrations.length > 0}
        canManage={canManage}
      />

      {/* Repo-centric summary first: repositories are the operator's
          mental model; connections are plumbing one section below. */}
      <div>
        <div className="mb-1 flex items-center justify-between gap-2">
          <h3 className="font-medium">Repositories</h3>
          {canManage && (
            <Button variant="primary" size="sm" onClick={() => navigate("/integrations/connect")}>
              + Connect a repository
            </Button>
          )}
        </div>
        <p className="text-xs text-fg-muted mb-3">
          Each connected repo carries its enabled bots — webhook, token and schedules are
          provisioned automatically.
        </p>
        {integrations.length === 0 ? (
          <EmptyState
            title="No repository connected yet"
            message="The guided flow connects your forge account and enables bots on a repo in one pass."
            action={
              canManage ? (
                <Button variant="primary" size="sm" onClick={() => navigate("/integrations/connect")}>
                  Connect a repository
                </Button>
              ) : undefined
            }
          />
        ) : (
          <ul className="divide-y divide-border-subtle rounded border border-border-subtle bg-surface-0">
            {integrations.map((i) => {
              const conn = connections.find((c) => c.id === i.connection_id);
              return (
                <li key={i.id} className="flex flex-wrap items-center gap-2 px-3 py-2">
                  <span className="min-w-0 flex-1 truncate text-sm font-medium">
                    {i.repo_full_name}
                  </span>
                  <span className="text-caption text-fg-muted">
                    {i.bot_ids.length > 0
                      ? `${i.bot_ids.length} bot${i.bot_ids.length > 1 ? "s" : ""}`
                      : "no bots"}
                  </span>
                  <span className="rounded bg-surface-2 px-1.5 py-0.5 text-caption text-fg-muted">
                    {i.provider}
                    {conn?.status && conn.status !== "active"
                      ? ` · ${connectionStatusLabel(conn.status)}`
                      : ""}
                  </span>
                </li>
              );
            })}
          </ul>
        )}
      </div>

      <SchedulesSection teamID={teamID} canManage={canManage} />

      <div>
        <h3 className="font-medium mb-1">Connected accounts</h3>
        <p className="text-xs text-fg-muted mb-3">
          The forge accounts behind those repositories. Connect once, then enable bots per
          repository — iterion creates the webhook on the forge and wires the bot's token
          for you.
        </p>
        {connections.length === 0 ? (
          <EmptyState
            title="No forge connected yet"
            message="Connect a GitLab, GitHub or Forgejo account to let bots act on your repositories."
            action={
              canManage ? (
                <Button variant="primary" size="sm" onClick={() => navigate("/integrations/connect")}>
                  Connect a forge
                </Button>
              ) : undefined
            }
          />
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
                onError={setActionErr}
                confirm={confirm}
                preselectBot={preselectBot}
                autoOpenEnable={!!preselectBot && connections.length === 1}
              />
            ))}
          </div>
        )}
      </div>

      <div id="connect-forge-form">
        <button
          type="button"
          onClick={() => setManualOpen((o) => !o)}
          aria-expanded={manualOpen}
          className="inline-flex items-center gap-1.5 text-sm font-medium text-fg-default hover:text-accent-text"
        >
          <ChevronRight
            size={14}
            className={`shrink-0 text-fg-subtle transition-transform duration-[var(--motion-fast)] ${
              manualOpen ? "rotate-90" : ""
            }`}
            aria-hidden
          />
          Manual setup (advanced)
        </button>
        <p className="mt-0.5 text-xs text-fg-muted">
          The guided “Connect a repository” flow above is the recommended path. Use these
          forms to register a forge OAuth app or connect an account by hand.
        </p>
        {manualOpen && (
          <div className="mt-3 space-y-6">
            <OAuthAppsSection
              teamID={teamID}
              apps={oauthApps}
              connections={connections}
              canManage={canManage}
              onChanged={reload}
              onError={setActionErr}
              confirm={confirm}
            />
            {canManage && (
              <ConnectForm
                teamID={teamID}
                oauthApps={oauthApps}
                onConnected={reload}
                onError={setActionErr}
              />
            )}
          </div>
        )}
      </div>
    </div>
  );
}

// WiringGuide is the sober "how it fits together" overview + status checklist
// at the top of the forges tab. It guides the operator through the connect
// pipeline (forge → repo+bot → webhook+token auto-wired) and collapses to a
// single line once a forge is connected and a repo is enabled, so it stays
// out of the way for established teams.
function WiringGuide({
  forgeConnected,
  repoEnabled,
  canManage,
}: {
  forgeConnected: boolean;
  repoEnabled: boolean;
  canManage: boolean;
}) {
  const [, navigate] = useLocation();
  const wired = forgeConnected && repoEnabled;
  // Default state follows `wired` (collapsed once a forge + repo are wired),
  // but a manual expand/collapse wins. Tracking an override rather than seeding
  // useState(!wired) matters because the forge data loads async: at first
  // render `wired` is always false, so a plain initial state would never
  // collapse for an already-wired team once its data arrives.
  const [override, setOverride] = useState<boolean | null>(null);
  const open = override ?? !wired;

  if (wired && !open) {
    return (
      <div className="flex items-center justify-between gap-2 bg-surface-1 border border-border-subtle rounded px-4 py-2">
        <span className="inline-flex items-center gap-2 text-xs text-fg-muted">
          <Badge variant="success" leadingIcon={<CheckIcon className="w-3 h-3" />}>
            Wired
          </Badge>
          Forge connected and a repo is enabled — webhooks and tokens are managed for you.
        </span>
        <button
          type="button"
          onClick={() => setOverride(true)}
          className="text-caption text-fg-subtle hover:text-fg-default shrink-0"
        >
          How it works
        </button>
      </div>
    );
  }

  return (
    <div className="bg-surface-1 border border-border-subtle rounded p-4 space-y-3">
      <div className="flex items-start justify-between gap-2">
        <div>
          <h3 className="font-medium">How integrations fit together</h3>
          <p className="text-xs text-fg-muted">
            Connect a forge, enable a bot on a repo — iterion wires the rest.
          </p>
        </div>
        {wired && (
          <button
            type="button"
            onClick={() => setOverride(false)}
            className="text-caption text-fg-subtle hover:text-fg-default shrink-0"
          >
            Hide
          </button>
        )}
      </div>
      <ol className="space-y-2">
        <GuideStep
          n={1}
          done={forgeConnected}
          label="Connect a forge"
          hint="GitLab, GitHub or Forgejo — once per account. The guided flow connects it and enables a repo in one pass."
          action={
            !forgeConnected && canManage ? (
              <button
                type="button"
                onClick={() => navigate("/integrations/connect")}
                className="text-accent-text hover:underline"
              >
                Connect →
              </button>
            ) : undefined
          }
        />
        <GuideStep
          n={2}
          done={repoEnabled}
          label="Enable a bot on a repo"
          hint={
            forgeConnected
              ? "Use “Enable a repo” on a connected forge below."
              : "Available once a forge is connected."
          }
        />
        <GuideStep
          n={3}
          // Informational step: the webhook + token are provisioned as part of
          // enabling a repo, so it tracks step 2 rather than a state of its own.
          done={repoEnabled}
          label="Webhook + token"
          hint="Created automatically on the forge — nothing to do."
        />
      </ol>
      <p className="text-caption text-fg-subtle">
        Need to give a bot a credential? Add it under <strong>Secrets</strong>, then map it in{" "}
        <strong>Bot bindings</strong>.
      </p>
    </div>
  );
}

function GuideStep({
  n,
  done,
  label,
  hint,
  action,
}: {
  n: number;
  done: boolean;
  label: string;
  hint: string;
  action?: ReactNode;
}) {
  return (
    <li className="flex items-start gap-2.5">
      <span
        className={`mt-0.5 inline-flex items-center justify-center w-4 h-4 rounded-full border text-caption shrink-0 ${
          done
            ? "bg-success-soft text-success-fg border-success/40"
            : "bg-surface-2 text-fg-muted border-border-default"
        }`}
        aria-hidden
      >
        {done ? <CheckIcon className="w-3 h-3" /> : n}
      </span>
      <div className="min-w-0">
        <div className="text-sm text-fg-default">
          {label}
          {action && <span className="ml-2 text-xs">{action}</span>}
        </div>
        <div className="text-caption text-fg-subtle">{hint}</div>
      </div>
    </li>
  );
}
