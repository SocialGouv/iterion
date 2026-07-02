import { errorMessage } from "@/lib/errorHints";
import { useEffect, useMemo, useRef, useState } from "react";

import {
  type ForgeOAuthApp,
  type ForgeProvider,
  connectForge,
} from "@/api/forgeConnections";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { Input } from "@/components/ui/Input";
import { Radio } from "@/components/ui/Radio";
import { useServerInfoStore } from "@/store/serverInfo";

import { CONNECTABLE, DEFAULT_BASE, canonicalBase } from "./forgeShared";

export function ConnectForm({
  teamID,
  oauthApps,
  onConnected,
  onError,
}: {
  teamID: string;
  oauthApps: ForgeOAuthApp[];
  onConnected: () => void;
  onError: (m: string) => void;
}) {
  const [provider, setProvider] = useState<ForgeProvider>("gitlab");
  const [baseURL, setBaseURL] = useState("");
  const [mode, setMode] = useState<"oauth" | "pat" | "app">("oauth");
  const [pat, setPat] = useState("");
  const [busy, setBusy] = useState(false);
  // Once the user picks a mode explicitly, stop auto-steering it.
  const modeTouched = useRef(false);

  // The "Install GitHub App" mode only works when the server actually has a
  // GitHub App configured (ITERION_FORGE_GITHUB_APP_*). When it doesn't,
  // offering the option just dead-ends on a 400; hide it and let the user use
  // OAuth or a PAT (the GitHub App is GitHub-only regardless).
  const githubAppConfigured = useServerInfoStore(
    (s) => s.info?.forge_github_app_configured ?? false,
  );

  // OAuth is offered for a (provider, instance) only when a matching OAuth app
  // is registered for this team; otherwise the PAT fallback.
  const appExists = (p: ForgeProvider, base: string) =>
    oauthApps.some(
      (a) => a.provider === p && (a.forge_base_url ?? DEFAULT_BASE[p]) === canonicalBase(p, base),
    );
  const oauthAvailable = appExists(provider, baseURL);

  // The least-privilege GitHub-App path (operator-selected repos, minimal fixed
  // permissions) is the recommended default for GitHub when the server has an
  // App configured; otherwise steer to OAuth when an app exists, else PAT.
  const bestMode = (p: ForgeProvider, base: string): "oauth" | "pat" | "app" => {
    if (p === "github" && githubAppConfigured) return "app";
    return appExists(p, base) ? "oauth" : "pat";
  };

  // Re-steer to the best default when the inputs flip, unless the user overrode
  // it.
  useEffect(() => {
    if (modeTouched.current) return;
    setMode(bestMode(provider, baseURL));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [oauthAvailable, provider, githubAppConfigured]);

  const pickMode = (m: "oauth" | "pat" | "app") => {
    modeTouched.current = true;
    setMode(m);
  };

  const pickProvider = (p: ForgeProvider) => {
    setProvider(p);
    // Re-steer to the new provider's best default (also clears a stale,
    // github-only "app" mode when switching away).
    modeTouched.current = false;
    setMode(bestMode(p, baseURL));
  };

  const connect = async () => {
    setBusy(true);
    try {
      const res = await connectForge(teamID, {
        provider,
        mode,
        forge_base_url: baseURL.trim() || undefined,
        pat: mode === "pat" ? pat : undefined,
        next: window.location.pathname,
      });
      // full-page round-trip for OAuth / App install.
      if (res.authorize_url || res.install_url) {
        window.location.href = (res.authorize_url ?? res.install_url) as string;
        return;
      }
      setPat("");
      onConnected();
    } catch (e) {
      const msg = errorMessage(e);
      // Self-hosted forges (e.g. a private GitLab) usually have no OAuth app
      // registered on this server. Rather than dead-ending on the raw 400,
      // steer the user to the PAT path (which is always available).
      if (mode === "oauth" && /no oauth app is registered|oauth is not configured/i.test(msg)) {
        modeTouched.current = true;
        setMode("pat");
        onError(
          "No OAuth app is registered for this instance — register one above, or paste a personal access token instead (now selected below).",
        );
      } else {
        onError(msg);
      }
    } finally {
      setBusy(false);
    }
  };

  const redirectHint = useMemo(() => {
    if (mode === "oauth") return "You'll be redirected to authorize iterion, then back here.";
    if (mode === "app") return "You'll be redirected to GitHub to install the app, then back here.";
    return "";
  }, [mode]);

  return (
    <section className="bg-surface-1 border border-border-subtle rounded p-4 space-y-3">
      <h3 className="font-medium">Connect a forge</h3>
      <div className="flex gap-2 flex-wrap">
        {(["gitlab", "github", "forgejo"] as ForgeProvider[]).map((p) => (
          <Button
            key={p}
            variant={provider === p ? "secondary" : "ghost"}
            size="sm"
            aria-pressed={provider === p}
            disabled={!CONNECTABLE.includes(p)}
            onClick={() => pickProvider(p)}
            title={CONNECTABLE.includes(p) ? "" : "Coming in a later phase"}
          >
            {p}
            {CONNECTABLE.includes(p) ? "" : " (soon)"}
          </Button>
        ))}
      </div>

      <div>
        <label htmlFor="forge-base-url" className="sr-only">
          Forge base URL
        </label>
        <Input
          size="md"
          id="forge-base-url"
          name="forge-base-url"
          placeholder="Forge base URL (optional — for self-hosted, e.g. https://gitlab.example.com)"
          value={baseURL}
          onChange={(e) => setBaseURL(e.target.value)}
          autoComplete="off"
        />
      </div>

      <div className="flex gap-3 text-sm items-center flex-wrap">
        {/* Least-privilege path first + flagged recommended (GitHub only). */}
        {provider === "github" && githubAppConfigured && (
          <label className="flex items-center gap-1">
            <Radio checked={mode === "app"} onChange={() => pickMode("app")} />
            Install GitHub App
            <Badge variant="accent" size="sm">
              Recommended
            </Badge>
          </label>
        )}
        <label
          className={`flex items-center gap-1 ${oauthAvailable ? "" : "opacity-50"}`}
          title={
            oauthAvailable
              ? ""
              : `No OAuth app registered for ${provider} on this instance — register one above, or paste a token`
          }
        >
          <Radio
            checked={mode === "oauth"}
            onChange={() => pickMode("oauth")}
            disabled={!oauthAvailable}
          />
          Use OAuth{oauthAvailable ? "" : " (no app)"}
        </label>
        <label className="flex items-center gap-1">
          <Radio
            checked={mode === "pat"}
            onChange={() => pickMode("pat")}
          />
          Paste a token
        </label>
      </div>

      {provider === "github" && githubAppConfigured && mode === "app" && (
        <p className="text-caption text-fg-muted">
          Least privilege: the GitHub App gets only the repositories you select
          and a minimal fixed permission set (contents, pull requests,
          webhooks) — not your account's full <code>repo</code> scope.
        </p>
      )}

      {mode === "pat" && (
        <div>
          <label htmlFor="forge-pat" className="sr-only">
            Personal access token
          </label>
          <Input
            size="md"
            type="password"
            id="forge-pat"
            name="forge-pat"
            placeholder="Personal access token (api / repo + hook-admin scope)"
            value={pat}
            onChange={(e) => setPat(e.target.value)}
            // "new-password" reliably suppresses saved-login autofill that
            // browsers force into password fields even with autoComplete="off".
            autoComplete="new-password"
          />
        </div>
      )}
      {redirectHint && <p className="text-caption text-fg-muted">{redirectHint}</p>}

      <Button
        variant="primary"
        onClick={() => void connect()}
        disabled={busy || (mode === "pat" && pat.trim() === "")}
        loading={busy}
      >
        {busy ? "Connecting…" : "Connect"}
      </Button>
    </section>
  );
}
