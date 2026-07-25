// Extracted from ConnectRepoWizard.tsx to keep that file focused.
// GitLab / Forgejo authorize variant: OAuth when an app is registered
// for the instance, PAT always reachable below.

import { useMemo, useState } from "react";

import {
  type ForgeConnection,
  type ForgeOAuthApp,
  type ForgeProvider,
  connectForge,
} from "@/api/forgeConnections";
import { Button } from "@/components/ui/Button";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { errorMessage } from "@/lib/errorHints";
import {
  DEFAULT_BASE,
  canonicalBase,
} from "@/views/teams/tabs/integrations/forgeShared";

import { RETURN_PATH } from "./model";
import NonAppMethods from "./NonAppMethods";

export interface NonGitHubAuthorizeProps {
  teamID: string;
  provider: ForgeProvider;
  baseURL: string;
  oauthApps: ForgeOAuthApp[];
  onBack: () => void;
  onError: (m: string) => void;
  onPatConnected: (conn: ForgeConnection) => void;
}

export default function NonGitHubAuthorize({
  teamID,
  provider,
  baseURL,
  oauthApps,
  onBack,
  onError,
  onPatConnected,
}: NonGitHubAuthorizeProps) {
  const oauthAvailable = useMemo(() => {
    const base = canonicalBase(provider, baseURL);
    return oauthApps.some(
      (a) =>
        a.provider === provider &&
        (a.forge_base_url ?? DEFAULT_BASE[provider]) === base,
    );
  }, [oauthApps, provider, baseURL]);

  const [busy, setBusy] = useState(false);

  const kickOffOAuth = async () => {
    setBusy(true);
    try {
      const res = await connectForge(teamID, {
        provider,
        mode: "oauth",
        forge_base_url: baseURL.trim() || undefined,
        next: RETURN_PATH,
      });
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

  const label = provider === "gitlab" ? "GitLab" : "Forgejo";

  return (
    <div className="space-y-4">
      <header className="space-y-1">
        <h2 className="text-headline font-semibold">Authorize {label}</h2>
        <p className="text-xs text-fg-muted">
          {baseURL.trim() ? `Base: ${baseURL.trim()}` : DEFAULT_BASE[provider]}
        </p>
      </header>

      {oauthAvailable ? (
        <div className="rounded-[var(--radius-lg)] border border-accent/40 bg-accent-soft p-4 space-y-2">
          <div className="font-medium">Continue with OAuth</div>
          <p className="text-caption text-fg-muted">
            Authorize the OAuth app registered for this instance. You'll be
            redirected to {label} and back here.
          </p>
          <div>
            <Button
              variant="primary"
              onClick={() => void kickOffOAuth()}
              disabled={busy}
              loading={busy}
            >
              Continue with OAuth
            </Button>
          </div>
        </div>
      ) : (
        <InlineBanner tone="info" layout="inline">
          No OAuth app is registered for this instance. You can still connect
          with a personal access token below, or register an OAuth app first
          from the Integrations page.
        </InlineBanner>
      )}

      <NonAppMethods
        teamID={teamID}
        provider={provider}
        baseURL={baseURL}
        oauthApps={oauthApps}
        // NonGitHubAuthorize primary flow is OAuth, so hide the redundant
        // "Use OAuth" secondary block; keep the PAT one always visible.
        hideOAuth
        onError={onError}
        onPatConnected={onPatConnected}
      />

      <div className="flex items-center justify-between pt-2">
        <Button variant="ghost" size="sm" onClick={onBack}>
          ← Back
        </Button>
        <span className="text-caption text-fg-subtle">
          Waiting on a redirect from {label} after you authorize.
        </span>
      </div>
    </div>
  );
}
