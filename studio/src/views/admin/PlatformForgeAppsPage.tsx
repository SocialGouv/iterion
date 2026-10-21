import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import {
  type ForgeOAuthApp,
  type ForgeProvider,
  deletePlatformForgeOAuthApp,
  listPlatformForgeOAuthApps,
  registerPlatformForgeOAuthApp,
} from "@/api/forgeConnections";
import { useAuth } from "@/auth/AuthContext";
import { CloudOnlyNotice } from "@/components/shared/CloudOnlyNotice";
import ConfirmDialog from "@/components/shared/ConfirmDialog";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import { Button } from "@/components/ui/Button";
import { EmptyState } from "@/components/ui/EmptyState";
import { Input } from "@/components/ui/Input";
import { Table, TableSkeleton, TBody, Td, Th, THead, Tr } from "@/components/ui/Table";
import { errorMessage } from "@/lib/errorHints";
import { useServerInfoStore } from "@/store/serverInfo";
import { useUIStore } from "@/store/ui";
import { DEFAULT_BASE } from "@/views/teams/tabs/integrations/forgeShared";

import AdminNav from "./AdminNav";

// Platform forge OAuth apps console: a forge instance's OAuth application
// registered ONCE for every team of the deployment. The canonical case is a
// GitLab instance whose applications are created at group level by its
// administrators (one application, pasted here by whoever created it), after
// which every team's connect wizard offers "Continue with OAuth" on that
// instance — and, on GitLab, posts as a per-repository bot iterion mints.
// The hook base is said here too, for an instance that only delivers to
// allowlisted hosts: every connection made through the app inherits it.
export default function PlatformForgeAppsPage() {
  const { user: me } = useAuth();
  const isSuper = me?.is_super_admin ?? false;
  const serverInfo = useServerInfoStore((s) => s.info);
  const isCloud = serverInfo?.mode === "cloud";
  const addToast = useUIStore((s) => s.addToast);
  const qc = useQueryClient();
  const [pendingDelete, setPendingDelete] = useState<ForgeOAuthApp | null>(null);

  useHeaderSlot({
    left: <span className="text-sm font-semibold">Platform forge OAuth apps</span>,
  });

  const query = useQuery<ForgeOAuthApp[]>({
    queryKey: ["admin-forge-oauth-apps"],
    queryFn: listPlatformForgeOAuthApps,
    enabled: isSuper && isCloud,
  });

  const del = useMutation({
    mutationFn: (id: string) => deletePlatformForgeOAuthApp(id),
    onSuccess: () => {
      addToast("Platform OAuth app removed.", "success");
      void qc.invalidateQueries({ queryKey: ["admin-forge-oauth-apps"] });
    },
    onError: (err) => addToast(errorMessage(err), "error"),
  });

  if (!isSuper) {
    return (
      <div className="p-6">
        <p className="text-sm text-fg-muted">Super-admin only.</p>
      </div>
    );
  }
  if (serverInfo && !isCloud) {
    return (
      <div className="h-full overflow-auto">
        <div className="max-w-5xl mx-auto p-3 sm:p-6">
          <CloudOnlyNotice feature="Platform forge OAuth apps" />
        </div>
      </div>
    );
  }

  const apps = query.data ?? [];

  return (
    <div className="h-full overflow-auto">
      <div className="max-w-5xl mx-auto p-3 sm:p-6 space-y-6">
        <AdminNav />
        <p className="text-sm text-fg-muted">
          An OAuth application registered here serves every team: their connect wizard offers
          “Continue with OAuth” on the instance, and a GitLab connection made this way posts as a
          per-repository bot iterion creates at enable — no token to create or paste. A team's own
          registration on the same instance takes precedence over the platform one.
        </p>

        <section aria-labelledby="platform-forge-apps-list">
          <h2 id="platform-forge-apps-list" className="text-sm font-semibold mb-2">
            Registered apps
          </h2>
          {query.isPending ? (
            <TableSkeleton rows={2} cols={5} />
          ) : query.isError ? (
            <p className="text-sm text-danger-fg">{errorMessage(query.error)}</p>
          ) : apps.length === 0 ? (
            <EmptyState
              title="No platform OAuth app yet"
              message="Register the application created on the forge below."
            />
          ) : (
            <Table caption="Platform forge OAuth apps">
              <THead>
                <Tr>
                  <Th>Provider</Th>
                  <Th>Instance</Th>
                  <Th>Client ID</Th>
                  <Th>Webhook base</Th>
                  <Th>
                    <span className="sr-only">Actions</span>
                  </Th>
                </Tr>
              </THead>
              <TBody>
                {apps.map((a) => (
                  <Tr key={a.id}>
                    <Td>{a.provider}</Td>
                    <Td className="font-mono text-xs">
                      {a.forge_base_url ?? DEFAULT_BASE[a.provider]}
                    </Td>
                    <Td className="font-mono text-xs break-all">{a.client_id}</Td>
                    <Td className="font-mono text-xs">
                      {a.webhook_base_url ?? (
                        <span className="text-fg-muted">this deployment's public URL</span>
                      )}
                    </Td>
                    <Td>
                      <Button
                        variant="danger"
                        size="sm"
                        onClick={() => setPendingDelete(a)}
                        disabled={del.isPending}
                      >
                        Delete
                      </Button>
                    </Td>
                  </Tr>
                ))}
              </TBody>
            </Table>
          )}
        </section>

        <RegisterPlatformAppForm
          onRegistered={() => {
            addToast("Platform OAuth app registered.", "success");
            void qc.invalidateQueries({ queryKey: ["admin-forge-oauth-apps"] });
          }}
          onError={(m) => addToast(m, "error")}
        />

        <ConfirmDialog
          open={pendingDelete !== null}
          title="Delete this platform OAuth app?"
          message={
            pendingDelete
              ? `Teams will no longer be offered OAuth on ${
                  pendingDelete.forge_base_url ?? DEFAULT_BASE[pendingDelete.provider]
                } through this app. Deletion is refused while a connection still authenticates through it.`
              : ""
          }
          confirmLabel="Delete"
          confirmVariant="danger"
          onConfirm={() => {
            if (pendingDelete) del.mutate(pendingDelete.id);
            setPendingDelete(null);
          }}
          onCancel={() => setPendingDelete(null)}
        />
      </div>
    </div>
  );
}

function RegisterPlatformAppForm({
  onRegistered,
  onError,
}: {
  onRegistered: () => void;
  onError: (m: string) => void;
}) {
  const [provider, setProvider] = useState<ForgeProvider>("gitlab");
  const [baseURL, setBaseURL] = useState("");
  const [clientID, setClientID] = useState("");
  const [clientSecret, setClientSecret] = useState("");
  const [webhookBase, setWebhookBase] = useState("");
  const [busy, setBusy] = useState(false);

  const redirectURI = `${window.location.origin}/api/forge/oauth/callback`;
  const canSubmit = clientID.trim() !== "" && clientSecret.trim() !== "";

  const submit = async () => {
    setBusy(true);
    try {
      await registerPlatformForgeOAuthApp({
        provider,
        forge_base_url: baseURL.trim() || undefined,
        client_id: clientID.trim(),
        client_secret: clientSecret.trim(),
        webhook_base_url: webhookBase.trim() || undefined,
      });
      setClientID("");
      setClientSecret("");
      setBaseURL("");
      setWebhookBase("");
      onRegistered();
    } catch (e) {
      onError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <section
      aria-labelledby="platform-forge-app-register"
      className="bg-surface-1 border border-border-subtle rounded p-4 space-y-3"
    >
      <h2 id="platform-forge-app-register" className="font-medium text-sm">
        Register a platform OAuth app
      </h2>
      <div className="flex gap-2 flex-wrap">
        {(["gitlab", "github", "forgejo"] as ForgeProvider[]).map((p) => (
          <Button
            key={p}
            variant={provider === p ? "secondary" : "ghost"}
            size="sm"
            aria-pressed={provider === p}
            onClick={() => setProvider(p)}
          >
            {p}
          </Button>
        ))}
      </div>
      <p className="text-caption text-fg-muted">
        Create the application on the forge first (GitLab: the group's Settings → Applications,
        confidential, scope <code>api</code>) with this redirect URI:{" "}
        <code className="break-all">{redirectURI}</code>
      </p>
      <div>
        <label htmlFor="platform-app-base-url" className="sr-only">
          Forge base URL
        </label>
        <Input
          size="md"
          id="platform-app-base-url"
          placeholder="Forge base URL (optional — for self-hosted, e.g. https://gitlab.example.com)"
          value={baseURL}
          onChange={(e) => setBaseURL(e.target.value)}
          autoComplete="off"
        />
      </div>
      <div>
        <label htmlFor="platform-app-client-id" className="sr-only">
          Client ID
        </label>
        <Input
          size="md"
          id="platform-app-client-id"
          placeholder="Client ID (application id)"
          value={clientID}
          onChange={(e) => setClientID(e.target.value)}
          autoComplete="off"
        />
      </div>
      <div>
        <label htmlFor="platform-app-client-secret" className="sr-only">
          Client secret
        </label>
        <Input
          size="md"
          type="password"
          id="platform-app-client-secret"
          placeholder="Client secret"
          value={clientSecret}
          onChange={(e) => setClientSecret(e.target.value)}
          autoComplete="new-password"
        />
      </div>
      <div>
        <label htmlFor="platform-app-webhook-base" className="sr-only">
          Webhook base URL
        </label>
        <Input
          size="md"
          id="platform-app-webhook-base"
          placeholder="Webhook base URL (optional — a host this forge is allowed to deliver to, e.g. https://iterion.example.gouv.fr)"
          value={webhookBase}
          onChange={(e) => setWebhookBase(e.target.value)}
          autoComplete="off"
        />
      </div>
      <p className="text-caption text-fg-muted">
        The webhook base is inherited by every connection made through this app; leave it empty
        when the forge can reach this deployment's public URL.
      </p>
      <Button
        variant="primary"
        onClick={() => void submit()}
        disabled={busy || !canSubmit}
        loading={busy}
      >
        {busy ? "Registering…" : "Register"}
      </Button>
    </section>
  );
}
