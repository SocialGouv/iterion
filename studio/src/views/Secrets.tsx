import { useCallback, useEffect, useState } from "react";
import { LockClosedIcon } from "@radix-ui/react-icons";

import {
  type LocalSecretView,
  createLocalSecret,
  deleteLocalSecret,
  isValidSecretName,
  listLocalSecrets,
  updateLocalSecret,
} from "@/api/secrets";
import { errorMessage } from "@/lib/errorHints";
import { useAsyncAction } from "@/hooks/useAsyncAction";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { Dialog } from "@/components/ui/Dialog";
import { EmptyState } from "@/components/ui/EmptyState";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Input } from "@/components/ui/Input";
import { PageHeader } from "@/components/ui/PageHeader";
import { Select } from "@/components/ui/Select";
import ConfirmDialog from "@/components/shared/ConfirmDialog";

// Secrets manages the local (non-cloud) sealed secret store: machine-global
// ~/.iterion/secrets.json plus an optional per-project override. Values are
// AES-GCM sealed at rest, injected into runs at tool/shell exec time, and
// never enter the agent's context. Backed by /api/local/secrets
// (server_info.secrets_enabled).
export default function Secrets() {
  const [secrets, setSecrets] = useState<LocalSecretView[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);
  const [rotating, setRotating] = useState<LocalSecretView | null>(null);
  const [deleting, setDeleting] = useState<LocalSecretView | null>(null);

  const reload = useCallback(async () => {
    setError(null);
    try {
      setSecrets(await listLocalSecrets());
    } catch (e) {
      setError(errorMessage(e));
      setSecrets([]);
    }
  }, []);

  useEffect(() => {
    void reload();
  }, [reload]);

  const doDelete = async () => {
    if (!deleting) return;
    try {
      await deleteLocalSecret(deleting.id);
      setDeleting(null);
      void reload();
    } catch (e) {
      setError(errorMessage(e));
    }
  };

  return (
    <div className="flex h-full min-h-0 flex-col bg-surface-0 text-fg-default">
      <PageHeader
        icon={<LockClosedIcon className="h-5 w-5" />}
        title="Secrets"
        description={
          <>
            Store credentials for your bots, sealed at rest. A bot declares what it
            needs in its <code>secrets:</code> block and references{" "}
            <code>{"{{secrets.NAME}}"}</code>; the value is injected at exec time and
            never enters the agent's context. Project secrets override global ones by
            name.
          </>
        }
        actions={
          <div className="flex items-center gap-2">
            <Button variant="secondary" size="sm" onClick={() => void reload()}>
              Refresh
            </Button>
            <Button variant="primary" size="sm" onClick={() => setCreating(true)}>
              Add secret
            </Button>
          </div>
        }
      />

      <div className="mx-auto flex w-full max-w-3xl flex-1 flex-col gap-3 overflow-y-auto p-6">
        {error && (
          <InlineBanner tone="danger" title="Secrets error" layout="inline">
            {error}
          </InlineBanner>
        )}

        {secrets === null ? (
          <EmptyState message="Loading…" />
        ) : secrets.length === 0 ? (
          <EmptyState
            title="No secrets yet"
            message="Add a secret, then reference it from a bot's secrets: block by name."
          />
        ) : (
          <SecretsTable
            secrets={secrets}
            onRotate={setRotating}
            onDelete={setDeleting}
          />
        )}
      </div>

      {creating && (
        <UpsertSecretDialog
          onClose={() => setCreating(false)}
          onDone={() => {
            setCreating(false);
            void reload();
          }}
        />
      )}

      {rotating && (
        <UpsertSecretDialog
          rec={rotating}
          onClose={() => setRotating(null)}
          onDone={() => {
            setRotating(null);
            void reload();
          }}
        />
      )}

      <ConfirmDialog
        open={deleting !== null}
        title={`Delete ${deleting?.name ?? ""}?`}
        message="Bots that reference this secret by name will fail to resolve it until you add it again."
        confirmLabel="Delete"
        confirmVariant="danger"
        onConfirm={() => void doDelete()}
        onCancel={() => setDeleting(null)}
      />
    </div>
  );
}

function SecretsTable({
  secrets,
  onRotate,
  onDelete,
}: {
  secrets: LocalSecretView[];
  onRotate: (rec: LocalSecretView) => void;
  onDelete: (rec: LocalSecretView) => void;
}) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-sm">
        <thead className="text-xs uppercase tracking-wider text-fg-muted text-left">
          <tr>
            <th className="px-2 py-1">Name</th>
            <th className="px-2 py-1">Scope</th>
            <th className="px-2 py-1">Last4</th>
            <th className="px-2 py-1">Hosts</th>
            <th className="px-2 py-1 text-right">Actions</th>
          </tr>
        </thead>
        <tbody>
          {secrets.map((s) => (
            <tr key={`${s.scope}:${s.id}`} className="border-t border-border-subtle">
              <td className="px-2 py-2 font-mono">{s.name}</td>
              <td className="px-2 py-2">
                <Badge variant={s.scope === "project" ? "info" : "neutral"}>{s.scope}</Badge>
              </td>
              <td className="px-2 py-2 font-mono text-fg-muted">…{s.last4 ?? "????"}</td>
              <td className="px-2 py-2 text-fg-muted text-xs break-all">
                {s.allowed_hosts && s.allowed_hosts.length > 0 ? s.allowed_hosts.join(", ") : "—"}
              </td>
              <td className="px-2 py-2 text-right space-x-1 whitespace-nowrap">
                <Button size="sm" variant="ghost" onClick={() => onRotate(s)}>
                  Rotate
                </Button>
                <Button size="sm" variant="ghost" className="text-danger" onClick={() => onDelete(s)}>
                  Delete
                </Button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// UpsertSecretDialog creates a new secret or rotates an existing one (when rec
// is set). Name + scope are fixed on rotate; only the value changes.
function UpsertSecretDialog({
  rec,
  onClose,
  onDone,
}: {
  rec?: LocalSecretView;
  onClose: () => void;
  onDone: () => void;
}) {
  const rotate = rec !== undefined;
  const [name, setName] = useState(rec?.name ?? "");
  const [secret, setSecret] = useState("");
  const [scope, setScope] = useState<"global" | "project">(rec?.scope ?? "global");
  const [hosts, setHosts] = useState((rec?.allowed_hosts ?? []).join(", "));
  const { busy, error: err, run } = useAsyncAction();

  const v = isValidSecretName(name);
  const canSubmit = (rotate || v.ok) && secret !== "";

  const submit = () => {
    if (!canSubmit) return;
    const allowedHosts = hosts
      .split(",")
      .map((h) => h.trim())
      .filter(Boolean);
    return run(async () => {
      if (rotate) {
        await updateLocalSecret(rec.id, { secret, allowed_hosts: allowedHosts });
      } else {
        await createLocalSecret({ name, secret, scope, allowed_hosts: allowedHosts });
      }
      onDone();
    });
  };

  return (
    <Dialog
      open
      onOpenChange={(o) => {
        if (!o) onClose();
      }}
      title={rotate ? `Rotate ${rec.name}` : "Add secret"}
      description="The value is stored sealed; only the last4 + fingerprint are ever returned."
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" loading={busy} disabled={!canSubmit} onClick={() => void submit()}>
            {rotate ? "Rotate" : "Create"}
          </Button>
        </>
      }
    >
      {err && (
        <InlineBanner tone="danger" layout="inline" className="mb-3">
          {err}
        </InlineBanner>
      )}
      <div className="space-y-3 text-sm">
        {!rotate && (
          <>
            <label className="block">
              <div className="text-xs text-fg-muted mb-1">Name</div>
              <Input
                value={name}
                onChange={(e) => setName(e.target.value)}
                error={name !== "" && !v.ok}
                placeholder="GITHUB_TOKEN"
                autoFocus
              />
              <div className={`text-xs mt-1 ${v.ok ? "text-fg-muted" : "text-danger"}`}>
                {v.ok ? "OK" : v.error ?? "—"}
              </div>
            </label>
            <label className="block">
              <div className="text-xs text-fg-muted mb-1">Scope</div>
              <Select
                size="md"
                value={scope}
                onChange={(e) => setScope(e.target.value as "global" | "project")}
              >
                <option value="global">Global (all projects on this machine)</option>
                <option value="project">Project (this workspace only, overrides global)</option>
              </Select>
            </label>
          </>
        )}
        <label className="block">
          <div className="text-xs text-fg-muted mb-1">{rotate ? "New value" : "Secret value"}</div>
          <Input
            type="password"
            value={secret}
            onChange={(e) => setSecret(e.target.value)}
            placeholder="paste here — never shown again"
            autoFocus={rotate}
          />
        </label>
        <label className="block">
          <div className="text-xs text-fg-muted mb-1">Egress hosts (optional, comma-separated)</div>
          <Input
            value={hosts}
            onChange={(e) => setHosts(e.target.value)}
            placeholder="github.com, api.github.com"
          />
          <div className="text-xs text-fg-muted mt-1">
            Restricts where this secret may egress (sandboxed runs). Empty = unrestricted.
          </div>
        </label>
      </div>
    </Dialog>
  );
}
