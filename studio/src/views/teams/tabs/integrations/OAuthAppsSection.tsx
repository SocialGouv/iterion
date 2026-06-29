import {
  type ForgeConnection,
  type ForgeOAuthApp,
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
                <Button
                  variant="danger"
                  size="sm"
                  className="shrink-0"
                  onClick={() => void remove(a)}
                >
                  Delete
                </Button>
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
