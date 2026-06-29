import { useState } from "react";
import { useSearch } from "wouter";

import { createPAT } from "@/api/tokens";
import { useAuth } from "@/auth/AuthContext";
import { Button } from "@/components/ui/Button";
import { InlineBanner } from "@/components/ui/InlineBanner";

// CliAuthPage is the browser half of `iterion remote login`: the signed-in user
// approves, a personal access token is minted, and the page redirects to the
// CLI's local loopback listener with the token. Standalone (no AppShell).
//
// Security: the redirect target MUST be loopback (http://127.0.0.1|localhost) so
// the minted token can only reach a process on this machine.
export default function CliAuthPage() {
  const search = useSearch();
  const { user } = useAuth();
  const params = new URLSearchParams(search);
  const redirectURI = params.get("redirect_uri") ?? "";
  const state = params.get("state") ?? "";
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const loopback = isLoopback(redirectURI);

  const finish = (extra: Record<string, string>) => {
    const u = new URL(redirectURI);
    for (const [k, v] of Object.entries(extra)) u.searchParams.set(k, v);
    if (state) u.searchParams.set("state", state);
    window.location.href = u.toString();
  };

  const authorize = async () => {
    setBusy(true);
    setError(null);
    try {
      const { token } = await createPAT("iterion CLI");
      finish({ token });
    } catch (e) {
      setError((e as Error).message);
      setBusy(false);
    }
  };

  return (
    <div className="min-h-screen flex items-center justify-center bg-surface-0 p-4">
      <div className="w-full max-w-md bg-surface-1 border border-border-subtle rounded-lg p-6 space-y-4">
        <h1 className="text-lg font-semibold">Authorize the iterion CLI</h1>
        {!loopback ? (
          <InlineBanner tone="danger" layout="inline">
            Invalid request — the CLI redirect target must be a local address
            (http://127.0.0.1). Re-run `iterion remote login` from your terminal.
          </InlineBanner>
        ) : (
          <>
            <p className="text-sm text-fg-muted">
              A command-line tool on this machine is requesting access to your iterion
              account{user?.email ? ` (${user.email})` : ""}. Approving creates a personal
              access token and hands it to the CLI.
            </p>
            {error && (
              <InlineBanner tone="danger" layout="inline">
                {error}
              </InlineBanner>
            )}
            <div className="flex gap-2">
              <Button variant="primary" loading={busy} onClick={() => void authorize()}>
                Authorize
              </Button>
              <Button variant="ghost" disabled={busy} onClick={() => finish({ error: "cancelled" })}>
                Cancel
              </Button>
            </div>
            <p className="text-caption text-fg-subtle">
              You can revoke this token any time from Account → Access tokens.
            </p>
          </>
        )}
      </div>
    </div>
  );
}

function isLoopback(uri: string): boolean {
  try {
    const u = new URL(uri);
    return u.protocol === "http:" && (u.hostname === "127.0.0.1" || u.hostname === "localhost");
  } catch {
    return false;
  }
}
