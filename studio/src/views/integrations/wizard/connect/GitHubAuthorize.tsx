// Extracted from ConnectRepoWizard.tsx to keep that file focused.
// GitHub authorize variant: the least-privilege App install is the
// primary path; the manifest-created App card covers the cold start.

import { useMemo, useState } from "react";

import {
  type ForgeConnection,
  type ForgeOAuthApp,
  connectForge,
} from "@/api/forgeConnections";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { errorMessage } from "@/lib/errorHints";
import { useServerInfoStore } from "@/store/serverInfo";
import {
  DEFAULT_BASE,
  canonicalBase,
} from "@/views/teams/tabs/integrations/forgeShared";

import CreateGitHubAppCard from "./CreateGitHubAppCard";
import { RETURN_PATH } from "./model";
import NonAppMethods from "./NonAppMethods";

export interface GitHubAuthorizeProps {
  teamID: string;
  baseURL: string;
  oauthApps: ForgeOAuthApp[];
  installedAppID: string;
  onBack: () => void;
  onError: (m: string) => void;
  onPatConnected: (conn: ForgeConnection) => void;
}

export default function GitHubAuthorize({
  teamID,
  baseURL,
  oauthApps,
  installedAppID,
  onBack,
  onError,
  onPatConnected,
}: GitHubAuthorizeProps) {
  const platformAppConfigured = useServerInfoStore(
    (s) => s.info?.forge_github_app_configured ?? false,
  );
  const [busy, setBusy] = useState(false);

  // A team-installable GitHub App exists when a manifest-created app is
  // registered for the (github, base) tuple with installable: true.
  const teamInstallable = useMemo(() => {
    const base = canonicalBase("github", baseURL);
    return oauthApps.find(
      (a) =>
        a.provider === "github" &&
        (a.forge_base_url ?? DEFAULT_BASE.github) === base &&
        a.installable === true,
    );
  }, [oauthApps, baseURL]);

  const appAvailable = platformAppConfigured || !!teamInstallable;

  const installApp = async () => {
    setBusy(true);
    try {
      const res = await connectForge(teamID, {
        provider: "github",
        mode: "app",
        forge_base_url: baseURL.trim() || undefined,
        next: RETURN_PATH,
      });
      if (res.install_url) {
        window.location.href = res.install_url;
        return;
      }
      if (res.authorize_url) {
        window.location.href = res.authorize_url;
        return;
      }
      if (res.connection) {
        onPatConnected(res.connection);
      }
    } catch (e) {
      onError(errorMessage(e));
      setBusy(false);
    }
  };

  return (
    <div className="space-y-4">
      <header className="space-y-1">
        <h2 className="text-headline font-semibold">Authorize GitHub</h2>
        <p className="text-xs text-fg-muted">
          {baseURL.trim()
            ? `Base: ${baseURL.trim()}`
            : "github.com — least-privilege GitHub App is the recommended path."}
        </p>
      </header>

      {installedAppID && (
        <InlineBanner tone="success" layout="inline" title="GitHub App created">
          The app is registered for this team. Install it on GitHub next to
          pick which repositories it can act on.
        </InlineBanner>
      )}

      {appAvailable ? (
        <div className="rounded-[var(--radius-lg)] border border-accent/40 bg-accent-soft p-4 space-y-2">
          <div className="flex items-center gap-2">
            <span className="font-medium">Install the GitHub App</span>
            <Badge variant="accent" size="sm">
              Recommended
            </Badge>
          </div>
          <p className="text-caption text-fg-muted">
            Least privilege — the App gets only the repositories you select
            and the minimal permission set (contents, pull requests,
            webhooks). No account-wide <code>repo</code> scope.
          </p>
          <div>
            <Button
              variant="primary"
              onClick={() => void installApp()}
              disabled={busy}
              loading={busy}
            >
              {installedAppID ? "Install it now on GitHub" : "Install the GitHub App"}
            </Button>
          </div>
        </div>
      ) : (
        <CreateGitHubAppCard
          teamID={teamID}
          baseURL={baseURL}
          onError={onError}
        />
      )}

      <NonAppMethods
        teamID={teamID}
        provider="github"
        baseURL={baseURL}
        oauthApps={oauthApps}
        onError={onError}
        onPatConnected={onPatConnected}
      />

      <div className="flex items-center justify-between pt-2">
        <Button variant="ghost" size="sm" onClick={onBack}>
          ← Back
        </Button>
        <span className="text-caption text-fg-subtle">
          Waiting on a redirect from GitHub after you authorize.
        </span>
      </div>
    </div>
  );
}
