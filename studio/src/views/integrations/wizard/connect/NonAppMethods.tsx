// Extracted from ConnectRepoWizard.tsx to keep that file focused.
// Shared "other methods" disclosure used by both authorize variants.

import { useState } from "react";

import type {
  ForgeConnection,
  ForgeOAuthApp,
  ForgeProvider,
} from "@/api/forgeConnections";

import PatFallback from "./PatFallback";

export interface NonAppMethodsProps {
  teamID: string;
  provider: ForgeProvider;
  baseURL: string;
  oauthApps: ForgeOAuthApp[];
  hideOAuth?: boolean;
  onError: (m: string) => void;
  onPatConnected: (conn: ForgeConnection) => void;
}

export default function NonAppMethods({
  teamID,
  provider,
  baseURL,
  oauthApps,
  hideOAuth = false,
  onError,
  onPatConnected,
}: NonAppMethodsProps) {
  const [expanded, setExpanded] = useState(false);
  return (
    <div className="border-t border-border-subtle pt-3">
      <button
        type="button"
        className="text-caption text-fg-muted hover:text-fg-default"
        aria-expanded={expanded}
        onClick={() => setExpanded((v) => !v)}
      >
        {expanded ? "Hide other methods" : "Other methods (personal token)"}
      </button>
      {expanded && (
        <div className="mt-2">
          <PatFallback
            teamID={teamID}
            provider={provider}
            baseURL={baseURL}
            oauthApps={hideOAuth ? [] : oauthApps}
            onError={onError}
            onConnected={onPatConnected}
            bare
          />
        </div>
      )}
    </div>
  );
}
