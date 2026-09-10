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
import { Select } from "@/components/ui/Select";
import { errorMessage } from "@/lib/errorHints";
import { useServerInfoStore } from "@/store/serverInfo";
import {
  DEFAULT_BASE,
  canonicalBase,
} from "@/views/teams/tabs/integrations/forgeShared";
import { GitHubAppLogoHint } from "@/views/teams/tabs/integrations/GitHubAppLogoHint";

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

  // A team may hold SEVERAL installable Apps on one host — one per owning org,
  // since a private App is only installable on its owner. Keep them all: the
  // operator picks which org to install into.
  const teamApps = useMemo(() => {
    const base = canonicalBase("github", baseURL);
    return oauthApps.filter(
      (a) =>
        a.provider === "github" &&
        (a.forge_base_url ?? DEFAULT_BASE.github) === base &&
        a.installable === true,
    );
  }, [oauthApps, baseURL]);

  const [selectedAppID, setSelectedAppID] = useState("");
  const appAvailable = platformAppConfigured || teamApps.length > 0;
  const appLabel = (a: ForgeOAuthApp) => a.owner_login || a.app_slug || a.id;

  const installApp = async () => {
    setBusy(true);
    try {
      const res = await connectForge(teamID, {
        provider: "github",
        mode: "app",
        forge_base_url: baseURL.trim() || undefined,
        // Pin WHICH app when the team has several, so the install callback
        // records it on the connection instead of guessing by host.
        oauth_app_id: selectedAppID || teamApps[0]?.id,
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
        <>
          <InlineBanner tone="success" layout="inline" title="GitHub App created">
            The app is registered for this team. Install it on GitHub next to
            pick which repositories it can act on.
          </InlineBanner>
          <GitHubAppLogoHint
            logoUploadURL={oauthApps.find((a) => a.id === installedAppID)?.logo_upload_url}
          />
        </>
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
          {teamApps.length > 1 && (
            <label className="block space-y-1">
              <span className="text-caption text-fg-muted">
                Install which App? Each one belongs to a different GitHub
                organisation.
              </span>
              <Select
                value={selectedAppID || teamApps[0]?.id}
                onChange={(e) => setSelectedAppID(e.target.value)}
              >
                {teamApps.map((a) => (
                  <option key={a.id} value={a.id}>
                    {appLabel(a)}
                  </option>
                ))}
              </Select>
            </label>
          )}
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
      ) : null}

      {/* Always reachable, not only on a cold start: a team that already has
          an App still needs a second one to operate on another GitHub org (a
          private App installs solely on its owner). When an App exists this is
          the secondary path, collapsed behind a summary. */}
      {appAvailable ? (
        <details className="rounded-[var(--radius-lg)] border border-border-default p-3">
          <summary className="cursor-pointer text-sm text-fg-muted">
            Work with another GitHub organisation? Create a second App
          </summary>
          <div className="pt-3">
            <CreateGitHubAppCard
              teamID={teamID}
              baseURL={baseURL}
              onError={onError}
            />
          </div>
        </details>
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
