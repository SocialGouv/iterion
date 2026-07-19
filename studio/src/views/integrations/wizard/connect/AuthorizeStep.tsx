// Extracted from ConnectRepoWizard.tsx to keep that file focused.
// Step 2 — authorize the chosen forge. Dispatches to the GitHub variant
// (App install / manifest creation) or the generic OAuth/PAT variant.

import type {
  ForgeConnection,
  ForgeOAuthApp,
  ForgeProvider,
} from "@/api/forgeConnections";

import GitHubAuthorize from "./GitHubAuthorize";
import NonGitHubAuthorize from "./NonGitHubAuthorize";

export interface AuthorizeStepProps {
  teamID: string;
  provider: ForgeProvider;
  baseURL: string;
  oauthApps: ForgeOAuthApp[];
  installedAppID: string;
  onBack: () => void;
  onError: (m: string) => void;
  onPatConnected: (conn: ForgeConnection) => void;
}

export default function AuthorizeStep({
  teamID,
  provider,
  baseURL,
  oauthApps,
  installedAppID,
  onBack,
  onError,
  onPatConnected,
}: AuthorizeStepProps) {
  if (provider === "github") {
    return (
      <GitHubAuthorize
        teamID={teamID}
        baseURL={baseURL}
        oauthApps={oauthApps}
        installedAppID={installedAppID}
        onBack={onBack}
        onError={onError}
        onPatConnected={onPatConnected}
      />
    );
  }
  return (
    <NonGitHubAuthorize
      teamID={teamID}
      provider={provider}
      baseURL={baseURL}
      oauthApps={oauthApps}
      onBack={onBack}
      onError={onError}
      onPatConnected={onPatConnected}
    />
  );
}
