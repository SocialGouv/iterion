import { useState } from "react";
import { useQuery } from "@tanstack/react-query";

import {
  type ForgeConnection,
  type ForgeProvider,
  type RegisterForgeOAuthAppInput,
  getMyGitHubOrgs,
  registerForgeOAuthApp,
  startGitHubManifest,
  startGitHubOrgsPicker,
} from "@/api/forgeConnections";
import { Button } from "@/components/ui/Button";
import { Chip } from "@/components/ui/Chip";
import { Input } from "@/components/ui/Input";
import { Radio } from "@/components/ui/Radio";
import { Select } from "@/components/ui/Select";

import { DEFAULT_OAUTH_SCOPES } from "./forgeShared";

type RegisterMode = "auto" | "auto_from_connection" | "manual";

export function RegisterOAuthAppForm({
  teamID,
  connections,
  onRegistered,
  onError,
}: {
  teamID: string;
  connections: ForgeConnection[];
  onRegistered: () => void;
  onError: (m: string) => void;
}) {
  const [show, setShow] = useState(false);
  const [provider, setProvider] = useState<ForgeProvider>("gitlab");
  const [baseURL, setBaseURL] = useState("");
  const [mode, setMode] = useState<RegisterMode>("auto");
  const [adminToken, setAdminToken] = useState("");
  const [connectionID, setConnectionID] = useState("");
  const [clientID, setClientID] = useState("");
  const [clientSecret, setClientSecret] = useState("");
  const [githubOrg, setGithubOrg] = useState("");
  const [orgIsCustom, setOrgIsCustom] = useState(false);
  const [busy, setBusy] = useState(false);

  // Load the user's GitHub orgs once the github provider is selected, to offer a
  // dropdown instead of free-text. Empty until they grant read:org via the picker
  // (failures deliberately leave the list empty).
  const orgsQuery = useQuery({
    queryKey: ["my-github-orgs"],
    queryFn: () => getMyGitHubOrgs(),
    enabled: show && provider === "github",
  });
  const myOrgs = orgsQuery.data ?? [];

  const pickOrgs = async () => {
    try {
      const { authorize_url } = await startGitHubOrgsPicker(
        window.location.pathname + window.location.search,
      );
      window.location.href = authorize_url;
    } catch (e) {
      onError((e as Error).message);
    }
  };

  const redirectURI = `${window.location.origin}/api/forge/oauth/callback`;
  // GitHub has no create-app REST API (only the interactive App-Manifest flow),
  // so token-based auto-create isn't available for it yet — nudge to manual.
  const autoSupported = provider !== "github";
  const usableConns = connections.filter((c) => c.provider === provider);

  const pickProvider = (p: ForgeProvider) => {
    setProvider(p);
    if (p === "github" && mode !== "manual") setMode("manual");
  };

  const submit = async () => {
    setBusy(true);
    try {
      const input: RegisterForgeOAuthAppInput = {
        provider,
        forge_base_url: baseURL.trim() || undefined,
        mode,
      };
      if (mode === "manual") {
        input.client_id = clientID.trim();
        input.client_secret = clientSecret.trim();
      } else if (mode === "auto") {
        input.admin_token = adminToken.trim();
      } else {
        input.connection_id = connectionID;
      }
      await registerForgeOAuthApp(teamID, input);
      setAdminToken("");
      setConnectionID("");
      setClientID("");
      setClientSecret("");
      setBaseURL("");
      setShow(false);
      onRegistered();
    } catch (e) {
      onError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  // GitHub has no create-app API: instead iterion hands GitHub a pre-filled App
  // manifest the browser POSTs; GitHub creates the App and redirects back to
  // iterion's callback, which stores the credentials. One click, no admin token.
  const launchGitHubManifest = async () => {
    setBusy(true);
    try {
      const { post_url, manifest } = await startGitHubManifest(teamID, {
        forge_base_url: baseURL.trim() || undefined,
        github_org: githubOrg.trim() || undefined,
        next: window.location.pathname + window.location.search,
      });
      const form = document.createElement("form");
      form.method = "POST";
      form.action = post_url;
      const field = document.createElement("input");
      field.type = "hidden";
      field.name = "manifest";
      field.value = JSON.stringify(manifest);
      form.appendChild(field);
      document.body.appendChild(form);
      form.submit(); // navigates to GitHub; the callback brings us back
    } catch (e) {
      onError((e as Error).message);
      setBusy(false);
    }
  };

  const canSubmit =
    mode === "manual"
      ? !!clientID.trim() && !!clientSecret.trim()
      : mode === "auto"
        ? autoSupported && !!adminToken.trim()
        : !!connectionID;

  if (!show) {
    return (
      <Button
        variant="ghost"
        size="sm"
        className="mt-3"
        onClick={() => setShow(true)}
      >
        + Register an OAuth app
      </Button>
    );
  }

  return (
    <section className="mt-3 bg-surface-1 border border-border-subtle rounded p-4 space-y-3">
      <h4 className="font-medium text-sm">Register an OAuth app</h4>
      <div className="flex gap-2 flex-wrap">
        {(["gitlab", "github", "forgejo"] as ForgeProvider[]).map((p) => (
          <Button
            key={p}
            variant={provider === p ? "secondary" : "ghost"}
            size="sm"
            aria-pressed={provider === p}
            onClick={() => pickProvider(p)}
          >
            {p}
          </Button>
        ))}
      </div>

      {/* Show operators exactly what OAuth scope(s) the app will request before
          they create/authorize it (mirrors the Go DefaultScopes). */}
      <div className="flex items-center gap-1.5 flex-wrap text-caption text-fg-muted">
        <span>
          Requests scope{DEFAULT_OAUTH_SCOPES[provider].length > 1 ? "s" : ""}:
        </span>
        {DEFAULT_OAUTH_SCOPES[provider].map((s) => (
          <Chip key={s}>
            <code>{s}</code>
          </Chip>
        ))}
        {provider === "github" && (
          <span>
            — broad; prefer installing a GitHub App (least privilege) over an
            OAuth app where possible.
          </span>
        )}
      </div>

      {provider === "github" && (
        <div className="rounded border border-accent/40 bg-accent/5 p-3 space-y-2">
          <div className="space-y-1">
            <label htmlFor="gh-manifest-org" className="block text-xs text-fg-subtle">
              Create under
            </label>
            <div className="flex items-center gap-2">
              <Select
                size="md"
                id="gh-manifest-org"
                value={orgIsCustom ? "__other__" : githubOrg}
                onChange={(e) => {
                  if (e.target.value === "__other__") {
                    setOrgIsCustom(true);
                    setGithubOrg("");
                  } else {
                    setOrgIsCustom(false);
                    setGithubOrg(e.target.value);
                  }
                }}
              >
                <option value="">Your personal account</option>
                {myOrgs.map((o) => (
                  <option key={o} value={o}>
                    {o}
                  </option>
                ))}
                <option value="__other__">Other organization…</option>
              </Select>
              <button
                type="button"
                className="text-caption text-accent hover:underline shrink-0"
                onClick={() => void pickOrgs()}
              >
                {myOrgs.length > 0 ? "Refresh from GitHub" : "List my GitHub orgs"}
              </button>
            </div>
            {orgIsCustom && (
              <Input
                size="md"
                placeholder="Org login (e.g. SocialGouv)"
                value={githubOrg}
                onChange={(e) => setGithubOrg(e.target.value)}
                autoComplete="off"
              />
            )}
          </div>
          <Button
            variant="primary"
            onClick={() => void launchGitHubManifest()}
            disabled={busy}
            loading={busy}
          >
            {busy ? "Opening GitHub…" : "Create a GitHub App"}
          </Button>
          <p className="text-caption text-fg-muted">
            Creating under an org makes the App installable org-wide (personal account = installable
            only there). Only orgs that allow iterion's OAuth app appear in the list — GitHub hides
            orgs with third-party-app restrictions; for one of those pick “Other organization…” and
            type its login (you can still create the App there with the right perms). GitHub
            Enterprise: set the base URL below first.
          </p>
        </div>
      )}

      <div className="flex flex-wrap gap-3 text-sm">
        {/* Token-based auto-create is GitLab/Forgejo-only — GitHub's auto-create
            is the App-Manifest button above, so we hide this option for GitHub
            rather than show a dead-end disabled radio. */}
        {autoSupported && (
          <label className="flex items-center gap-1">
            <Radio checked={mode === "auto"} onChange={() => setMode("auto")} />
            Auto-create (admin token)
          </label>
        )}
        <label className="flex items-center gap-1">
          <Radio
            checked={mode === "auto_from_connection"}
            onChange={() => setMode("auto_from_connection")}
          />
          Reuse a connection
        </label>
        <label className="flex items-center gap-1">
          <Radio checked={mode === "manual"} onChange={() => setMode("manual")} />
          Paste credentials
        </label>
      </div>

      <div>
        <label htmlFor="oauth-app-base-url" className="sr-only">
          Forge base URL
        </label>
        <Input
          size="md"
          id="oauth-app-base-url"
          name="oauth-app-base-url"
          placeholder="Forge base URL (optional — for self-hosted, e.g. https://gitlab.example.com)"
          value={baseURL}
          onChange={(e) => setBaseURL(e.target.value)}
          autoComplete="off"
        />
      </div>

      {mode === "auto" && (
        <>
          <div>
            <label htmlFor="oauth-admin-token" className="sr-only">
              Admin token
            </label>
            <Input
              size="md"
              type="password"
              id="oauth-admin-token"
              name="oauth-admin-token"
              placeholder="Admin token (GitLab: instance-admin PAT with api scope)"
              value={adminToken}
              onChange={(e) => setAdminToken(e.target.value)}
              autoComplete="new-password"
            />
          </div>
          <p className="text-caption text-fg-muted">
            iterion creates the OAuth app on the forge for you (redirect URI + scope set
            automatically) and stores its credentials sealed. The admin token is used once and never
            stored.
          </p>
        </>
      )}

      {mode === "auto_from_connection" && (
        <>
          <div>
            <label htmlFor="oauth-conn-pick" className="sr-only">
              Connection
            </label>
            <Select
              size="md"
              id="oauth-conn-pick"
              value={connectionID}
              onChange={(e) => setConnectionID(e.target.value)}
            >
              <option value="">Select a {provider} connection…</option>
              {usableConns.map((c) => (
                <option key={c.id} value={c.id}>
                  {c.account_login ?? c.id} · {c.forge_base_url ?? c.provider}
                </option>
              ))}
            </Select>
          </div>
          <p className="text-caption text-fg-muted">
            Reuses an existing {provider} connection's token to create the app — no admin token to
            paste. The connection's owner needs create-app rights on the instance.
          </p>
        </>
      )}

      {mode === "manual" && (
        <>
          <div>
            <label htmlFor="oauth-client-id" className="sr-only">
              Client ID
            </label>
            <Input
              size="md"
              id="oauth-client-id"
              placeholder="Client ID (Application ID)"
              value={clientID}
              onChange={(e) => setClientID(e.target.value)}
              autoComplete="off"
            />
          </div>
          <div>
            <label htmlFor="oauth-client-secret" className="sr-only">
              Client secret
            </label>
            <Input
              size="md"
              type="password"
              id="oauth-client-secret"
              name="oauth-client-secret"
              placeholder="Client secret"
              value={clientSecret}
              onChange={(e) => setClientSecret(e.target.value)}
              autoComplete="new-password"
            />
          </div>
          <p className="text-caption text-fg-muted">
            Create the app on the forge with redirect URI{" "}
            <span className="font-mono break-all">{redirectURI}</span> and the scope it needs
            (GitLab: <span className="font-mono">api</span>), then paste its credentials here.
          </p>
        </>
      )}

      <div className="flex items-center gap-2">
        <Button
          variant="primary"
          onClick={() => void submit()}
          disabled={busy || !canSubmit}
          loading={busy}
        >
          {busy ? "Registering…" : "Register"}
        </Button>
        <Button variant="ghost" onClick={() => setShow(false)}>
          Cancel
        </Button>
      </div>
    </section>
  );
}
