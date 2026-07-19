import { useMemo } from "react";
import { useLocation, useSearch } from "wouter";
import { useQueryClient } from "@tanstack/react-query";

import { forgeTeamRepoKey } from "@/api/forgeConnections";
import { CloudOnlyNotice } from "@/components/shared/CloudOnlyNotice";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { useAuth } from "@/auth/AuthContext";
import { useActiveRepoStore } from "@/store/activeRepo";
import { useServerInfoStore } from "@/store/serverInfo";

import { StepIndicator } from "@/views/integrations/wizard/StepIndicator";
import AuthorizeStep from "@/views/integrations/wizard/connect/AuthorizeStep";
import DoneStep from "@/views/integrations/wizard/connect/DoneStep";
import {
  STEP_LABEL,
  STEP_ORDER,
  readQuery,
  withQuery,
} from "@/views/integrations/wizard/connect/model";
import ProviderStep from "@/views/integrations/wizard/connect/ProviderStep";
import ReposStep from "@/views/integrations/wizard/connect/ReposStep";
import {
  useConnectWizard,
  type ConnectWizardNavigate,
} from "@/views/integrations/wizard/connect/useConnectWizard";

// The wizard is the centrepiece of the cloud connect-repo UX redesign:
// four short steps (Provider → Authorize → Repositories → Done), driven
// straight off the URL query so a back-nav or an OAuth/App-install
// round-trip resumes exactly where it left off. Everything the wizard
// needs already lives in @/api/forgeConnections + EnableRepoPanel; this
// component only orchestrates them behind a single guided flow — the
// per-step components and the state hook live in wizard/connect/.

export default function ConnectRepoWizard() {
  const { activeTeam } = useAuth();
  const teamID = activeTeam?.team_id ?? "";
  const serverInfo = useServerInfoStore((s) => s.info);
  const isCloud = serverInfo?.mode === "cloud";
  const search = useSearch();
  const [, navigate] = useLocation();
  const q = useMemo(() => readQuery(search), [search]);

  useHeaderSlot({
    left: <span className="text-sm font-semibold">Connect a repository</span>,
    right: activeTeam ? (
      <span className="text-xs text-fg-muted">{activeTeam.team_name}</span>
    ) : null,
  });

  // Cloud-only: repositories, forge connections and OAuth apps are all
  // team-scoped resources with no local-mode equivalent.
  if (serverInfo && !isCloud) {
    return (
      <div className="h-full overflow-auto">
        <div className="max-w-2xl mx-auto p-3 sm:p-6">
          <CloudOnlyNotice
            title="Connect a repository"
            feature="Repository connection"
          />
        </div>
      </div>
    );
  }
  if (!teamID) {
    return (
      <div className="h-full overflow-auto">
        <div className="max-w-2xl mx-auto p-3 sm:p-6">
          <InlineBanner tone="info" layout="inline">
            Select a team to connect a repository. Forge connections are
            team-scoped.
          </InlineBanner>
        </div>
      </div>
    );
  }

  return <WizardInner teamID={teamID} q={q} navigate={navigate} />;
}

interface WizardInnerProps {
  teamID: string;
  q: URLSearchParams;
  navigate: ConnectWizardNavigate;
}

function WizardInner({ teamID, q, navigate }: WizardInnerProps) {
  const queryClient = useQueryClient();
  const choose = useActiveRepoStore((s) => s.choose);

  const {
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
  } = useConnectWizard(teamID, q, navigate);

  return (
    <div className="h-full overflow-auto">
      <div className="max-w-2xl mx-auto p-3 sm:p-6 space-y-5">
        <StepIndicator
          steps={STEP_ORDER.map((s) => ({ id: s, label: STEP_LABEL[s] }))}
          current={step}
          ariaLabel="Connect a repository — progress"
        />

        {error && (
          <InlineBanner tone="danger" layout="inline">
            {error}
          </InlineBanner>
        )}
        {botsWarning && step === "repos" && (
          <InlineBanner tone="warning" layout="inline">
            {botsWarning}
          </InlineBanner>
        )}

        {step === "provider" && (
          <ProviderStep
            teamID={teamID}
            oauthApps={oauthApps}
            initialProvider={providerParam}
            initialBase={baseParam}
            onError={setError}
            onNextAuthorize={(provider, base) =>
              gotoStep("authorize", {
                provider,
                base: base ?? "",
              })
            }
            onPatConnected={(conn) =>
              navigate(
                withQuery(new URLSearchParams({ connected: conn.id })),
              )
            }
          />
        )}

        {step === "authorize" && (
          <AuthorizeStep
            teamID={teamID}
            provider={providerParam}
            baseURL={baseParam}
            oauthApps={oauthApps}
            installedAppID={installedAppID}
            onBack={() =>
              gotoStep("provider", {
                provider: providerParam,
                base: baseParam,
              })
            }
            onError={setError}
            onPatConnected={(conn) =>
              navigate(
                withQuery(new URLSearchParams({ connected: conn.id })),
              )
            }
          />
        )}

        {step === "repos" && (
          <ReposStep
            teamID={teamID}
            loading={loading}
            connections={connections}
            connectionID={connectedID}
            repoBots={repoBots}
            reloadConnections={reload}
            onError={setError}
            onDone={(enabled) => {
              if (enabled) {
                choose(
                  forgeTeamRepoKey({
                    connection_id: enabled.connectionID,
                    repo_full_name: enabled.repo,
                  }),
                );
              }
              queryClient.invalidateQueries({
                queryKey: ["team-forge-repos", teamID],
              });
              const p = new URLSearchParams({ step: "done" });
              if (enabled) {
                p.set("connected", enabled.connectionID);
                p.set("repo", enabled.repo);
              }
              navigate(withQuery(p));
            }}
          />
        )}

        {step === "done" && (
          <DoneStep
            connectionID={connectedID}
            repo={q.get("repo") ?? ""}
            returnTo={returnTo}
            onReturn={
              returnTo
                ? () => {
                    clearReturnTo();
                    navigate(returnTo);
                  }
                : undefined
            }
            onGoToRepos={() => navigate("/integrations?tab=forges")}
            onOpenBoard={() => navigate("/board")}
            onLaunchBot={() => navigate("/bots")}
            onConnectAnother={restart}
          />
        )}
      </div>
    </div>
  );
}
