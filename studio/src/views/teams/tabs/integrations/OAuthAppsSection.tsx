import {
  type ForgeConnection,
  type ForgeOAuthApp,
  connectForge,
  deleteForgeOAuthApp,
} from "@/api/forgeConnections";

import type { ConfirmFn } from "./forgeShared";
import { RegisterOAuthAppForm } from "./RegisterOAuthAppForm";
import { Button } from "@/components/ui/Button";

export function OAuthAppsSection({
  teamID,
  apps,
  connections,
  canManage,
  onChanged,
  onError,
  confirm,
}: {
  teamID: string;
  apps: ForgeOAuthApp[];
  connections: ForgeConnection[];
  canManage: boolean;
  onChanged: () => void;
  onError: (m: string) => void;
  confirm: ConfirmFn;
}) {
  const remove = async (a: ForgeOAuthApp) => {
    const ok = await confirm({
      title: "Delete OAuth app?",
      message: (
        <span>
          Connections that authenticate via this {a.provider} app (
          {a.forge_base_url ?? a.provider}) will no longer be able to OAuth-refresh.
          Existing connections keep working until their token expires.
          {a.app_manage_url && (
            <>
              {" "}
              This only removes it from iterion — {a.provider} has no app-deletion API,
              so delete the app on the forge too:{" "}
              <a
                href={a.app_manage_url}
                target="_blank"
                rel="noreferrer"
                className="text-accent hover:underline"
              >
                open its settings ↗
              </a>
              .
            </>
          )}
        </span>
      ),
      confirmLabel: "Delete",
      confirmVariant: "danger",
    });
    if (!ok) return;
    try {
      await deleteForgeOAuthApp(teamID, a.id);
      onChanged();
    } catch (e) {
      onError((e as Error).message);
    }
  };

  // Install a GitHub App (least-privilege github_app): redirects to GitHub to
  // pick the repos, then GitHub calls back and iterion creates a github_app
  // connection (the bot acts as the App, not as the authorizing user).
  const install = async () => {
    try {
      const res = await connectForge(teamID, {
        provider: "github",
        mode: "app",
        next: window.location.pathname + window.location.search,
      });
      // Open install in a NEW tab: after picking repos GitHub redirects to the
      // App's setup_url (which creates the github_app connection), so the new
      // tab lands back on Integrations while this tab stays put.
      if (res.install_url) {
        const w = window.open(res.install_url, "_blank");
        if (!w) window.location.href = res.install_url; // popup blocked → same tab
      } else onError("no install URL returned for this app");
    } catch (e) {
      onError((e as Error).message);
    }
  };

  return (
    <div>
      <h3 className="font-medium mb-1">Forge OAuth apps</h3>
      <p className="text-xs text-fg-muted mb-3">
        Register an OAuth application per forge instance to connect over OAuth instead of a personal
        access token. Scoped to this team — each forge and self-hosted instance can have its own app.
      </p>
      {apps.length === 0 ? (
        <div className="text-fg-muted text-sm">No OAuth app registered yet.</div>
      ) : (
        <ul className="space-y-2">
          {apps.map((a) => (
            <li
              key={a.id}
              className="flex items-center justify-between gap-2 bg-surface-1 border border-border-subtle rounded px-3 py-2 text-sm"
            >
              <div className="min-w-0">
                <div className="font-medium">
                  {a.provider} · {a.forge_base_url ?? "—"}
                  <span className="ml-2 rounded bg-surface-2 px-1 text-caption text-fg-subtle">
                    {a.auto_created ? "auto" : "manual"}
                  </span>
                </div>
                <div className="text-caption text-fg-muted font-mono truncate">
                  client_id: {a.client_id}
                </div>
              </div>
              {canManage && (
                <div className="flex shrink-0 items-center gap-2">
                  {a.installable && a.provider === "github" && (
                    <Button
                      variant="primary"
                      size="sm"
                      title="Install this App on GitHub (least-privilege bot identity) and connect"
                      onClick={() => void install()}
                    >
                      Install
                    </Button>
                  )}
                  <Button variant="danger" size="sm" onClick={() => void remove(a)}>
                    Delete
                  </Button>
                </div>
              )}
            </li>
          ))}
        </ul>
      )}
      {canManage && (
        <RegisterOAuthAppForm
          teamID={teamID}
          connections={connections}
          onRegistered={onChanged}
          onError={onError}
        />
      )}
    </div>
  );
}
