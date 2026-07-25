import { useCallback, useState } from "react";
import { LockClosedIcon } from "@radix-ui/react-icons";
import { useQuery, useQueryClient } from "@tanstack/react-query";

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
import { useConfirm } from "@/hooks/useConfirm";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { Dialog } from "@/components/ui/Dialog";
import { EmptyState } from "@/components/ui/EmptyState";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Input } from "@/components/ui/Input";
import { PageHeader } from "@/components/ui/PageHeader";
import { Select } from "@/components/ui/Select";
import { Table, THead, Th, TBody, Tr, Td, TableSkeleton } from "@/components/ui/Table";

// Secrets manages the local (non-cloud) sealed secret store: machine-global
// ~/.iterion/secrets.json plus an optional per-project override. Values are
// AES-GCM sealed at rest, injected into runs at tool/shell exec time, and
// never enter the agent's context. Backed by /api/local/secrets
// (server_info.secrets_enabled).
// Stable empty fallback for the errored state, so renders don't hand the
// table a fresh [] reference each time.
const EMPTY_SECRETS: LocalSecretView[] = [];

export default function Secrets() {
  const queryClient = useQueryClient();
  const [creating, setCreating] = useState(false);
  const [rotating, setRotating] = useState<LocalSecretView | null>(null);
  const { confirm, dialog } = useConfirm();

  const secretsQuery = useQuery<LocalSecretView[]>({
    queryKey: ["local-secrets"],
    queryFn: listLocalSecrets,
  });
  // On a fetch error the list reads as empty (banner over the empty
  // state, not a stale table or an endless skeleton).
  const secrets = secretsQuery.isError
    ? EMPTY_SECRETS
    : (secretsQuery.data ?? null);
  // Delete failures share the fetch error's banner; any reload clears
  // them (the fetch side clears itself on refetch).
  const [actionError, setActionError] = useState<string | null>(null);
  const error =
    actionError ??
    (secretsQuery.error && !secretsQuery.isFetching
      ? errorMessage(secretsQuery.error)
      : null);

  // Post-mutation reload (delete / create / rotate): invalidate so the
  // list refetches.
  const reload = useCallback(() => {
    setActionError(null);
    void queryClient.invalidateQueries({ queryKey: ["local-secrets"] });
  }, [queryClient]);

  const doDelete = async (rec: LocalSecretView) => {
    const ok = await confirm({
      title: `Delete ${rec.name}?`,
      message:
        "Bots that reference this secret by name will fail to resolve it until you add it again.",
      confirmLabel: "Delete",
      confirmVariant: "danger",
    });
    if (!ok) return;
    try {
      await deleteLocalSecret(rec.id);
      reload();
    } catch (e) {
      setActionError(errorMessage(e));
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
            <Button
              variant="secondary"
              size="sm"
              onClick={() => {
                setActionError(null);
                void secretsQuery.refetch();
              }}
            >
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
          <TableSkeleton rows={4} cols={5} />
        ) : secrets.length === 0 ? (
          <EmptyState
            title="No secrets yet"
            message="Add a secret, then reference it from a bot's secrets: block by name."
          />
        ) : (
          <SecretsTable
            secrets={secrets}
            onRotate={setRotating}
            onDelete={(rec) => void doDelete(rec)}
          />
        )}
      </div>

      {creating && (
        <UpsertSecretDialog
          onClose={() => setCreating(false)}
          onDone={() => {
            setCreating(false);
            reload();
          }}
        />
      )}

      {rotating && (
        <UpsertSecretDialog
          rec={rotating}
          onClose={() => setRotating(null)}
          onDone={() => {
            setRotating(null);
            reload();
          }}
        />
      )}

      {dialog}
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
    <Table caption="Local secrets">
      <THead>
        <Th>Name</Th>
        <Th>Scope</Th>
        <Th>Last4</Th>
        <Th>Hosts</Th>
        <Th align="right">Actions</Th>
      </THead>
      <TBody>
        {secrets.map((s) => (
          <Tr key={`${s.scope}:${s.id}`}>
            <Td className="font-mono">{s.name}</Td>
            <Td>
              <Badge variant={s.scope === "project" ? "info" : "neutral"}>{s.scope}</Badge>
            </Td>
            <Td className="font-mono text-fg-muted">…{s.last4 ?? "????"}</Td>
            <Td className="text-fg-muted text-xs break-all">
              {s.allowed_hosts && s.allowed_hosts.length > 0 ? s.allowed_hosts.join(", ") : "—"}
            </Td>
            <Td align="right" className="space-x-1 whitespace-nowrap">
              <Button size="sm" variant="ghost" onClick={() => onRotate(s)}>
                Rotate
              </Button>
              <Button size="sm" variant="ghost" className="text-danger" onClick={() => onDelete(s)}>
                Delete
              </Button>
            </Td>
          </Tr>
        ))}
      </TBody>
    </Table>
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
