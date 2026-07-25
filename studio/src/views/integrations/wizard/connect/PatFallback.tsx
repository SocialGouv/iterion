// Extracted from ConnectRepoWizard.tsx to keep that file focused.

import { useMemo, useState } from "react";

import {
  type ConnectForgeInput,
  type ForgeConnection,
  type ForgeOAuthApp,
  type ForgeProvider,
  connectForge,
} from "@/api/forgeConnections";
import { Button } from "@/components/ui/Button";
import { Textarea } from "@/components/ui/Textarea";
import { errorMessage } from "@/lib/errorHints";
import {
  DEFAULT_BASE,
  canonicalBase,
} from "@/views/teams/tabs/integrations/forgeShared";

import { RETURN_PATH } from "./model";

export interface PatFallbackProps {
  teamID: string;
  provider: ForgeProvider;
  baseURL: string;
  oauthApps: ForgeOAuthApp[];
  onError: (m: string) => void;
  onConnected: (conn: ForgeConnection) => void;
  // bare renders the OAuth/PAT blocks directly, without PatFallback's own
  // disclosure — for callers (NonAppMethods) that already gate visibility
  // behind their own "Other methods" toggle. One toggle, not two.
  bare?: boolean;
}

// PatFallback keeps the PAT path always reachable (self-hosted forges,
// no OAuth app registered, or the operator preferring a token). It also
// offers the OAuth "Use OAuth" secondary path when a matching OAuth app
// is registered — this is the "Other methods" pocket described in the
// wizard brief.
export default function PatFallback({
  teamID,
  provider,
  baseURL,
  oauthApps,
  onError,
  onConnected,
  bare = false,
}: PatFallbackProps) {
  const [expanded, setExpanded] = useState(false);
  const open = bare || expanded;
  const [busy, setBusy] = useState(false);
  const [pat, setPat] = useState("");

  const oauthAvailable = useMemo(() => {
    return oauthApps.some(
      (a) =>
        a.provider === provider &&
        (a.forge_base_url ?? DEFAULT_BASE[provider]) ===
          canonicalBase(provider, baseURL),
    );
  }, [oauthApps, provider, baseURL]);

  const kickOff = async (mode: ConnectForgeInput["mode"]) => {
    setBusy(true);
    try {
      const res = await connectForge(teamID, {
        provider,
        mode,
        forge_base_url: baseURL.trim() || undefined,
        pat: mode === "pat" ? pat.trim() : undefined,
        next: RETURN_PATH,
      });
      if (res.authorize_url) {
        window.location.href = res.authorize_url;
        return;
      }
      if (res.connection) {
        setPat("");
        onConnected(res.connection);
      }
    } catch (e) {
      onError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className={bare ? "" : "border-t border-border-subtle pt-3"}>
      {!bare && (
        <button
          type="button"
          className="text-caption text-fg-muted hover:text-fg-default"
          aria-expanded={expanded}
          onClick={() => setExpanded((v) => !v)}
        >
          {expanded ? "Hide other methods" : "Other methods (OAuth, personal token)"}
        </button>
      )}
      {open && (
        <div className="mt-2 space-y-3 rounded border border-border-subtle bg-surface-1 p-3">
          {oauthAvailable && (
            <div>
              <div className="text-xs font-medium text-fg-default mb-1">
                Use OAuth
              </div>
              <p className="text-caption text-fg-muted mb-2">
                Authorize the OAuth app registered for this instance. You'll
                be redirected to {provider} and back here.
              </p>
              <Button
                variant="secondary"
                size="sm"
                onClick={() => void kickOff("oauth")}
                disabled={busy}
                loading={busy}
              >
                Continue with OAuth
              </Button>
            </div>
          )}
          <div>
            <div className="text-xs font-medium text-fg-default mb-1">
              Personal access token
            </div>
            <p className="text-caption text-fg-muted mb-2">
              Paste a token with the scopes your bots need. The token is
              sealed at rest and used only for this connection.
            </p>
            <Textarea
              value={pat}
              onChange={(e) => setPat(e.target.value)}
              placeholder="Personal access token"
              rows={2}
              className="w-full font-mono text-xs"
              autoComplete="off"
            />
            <div className="mt-2">
              <Button
                variant="secondary"
                size="sm"
                onClick={() => void kickOff("pat")}
                disabled={busy || pat.trim() === ""}
                loading={busy}
              >
                Connect with token
              </Button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
