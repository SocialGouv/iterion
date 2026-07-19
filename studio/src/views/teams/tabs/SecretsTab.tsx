import { errorMessage } from "@/lib/errorHints";
import { formatDateTime } from "@/lib/format";
import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useLocation } from "wouter";
import { InlineBanner } from "@/components/ui/InlineBanner";

import {
  FeatureUnavailableError,
  type GenericSecretView,
  createMySecret,
  createTeamSecret,
  deleteMySecret,
  deleteTeamSecret,
  isValidSecretName,
  listMySecrets,
  listTeamSecrets,
  updateMySecret,
  updateTeamSecret,
} from "@/api/secrets";

import { Button } from "@/components/ui/Button";
import { Dialog } from "@/components/ui/Dialog";
import { EmptyState } from "@/components/ui/EmptyState";
import { Input } from "@/components/ui/Input";
import { Table, THead, Th, TBody, Tr, Td, TableSkeleton } from "@/components/ui/Table";
import { useAsyncAction } from "@/hooks/useAsyncAction";
import { useConfirm } from "@/hooks/useConfirm";

interface Props {
  teamID: string;
  canManage: boolean;
}

export default function SecretsTab({ teamID, canManage }: Props) {
  const [, navigate] = useLocation();
  const queryClient = useQueryClient();
  // Mutation failures report through setActionErr; load failures surface
  // from the team-secrets query. One banner shows whichever is current.
  const [actionErr, setActionErr] = useState<string | null>(null);
  const [creating, setCreating] = useState<null | "team" | "me">(null);
  const [rotating, setRotating] = useState<{ scope: "team" | "me"; rec: GenericSecretView } | null>(
    null,
  );
  const { confirm, dialog } = useConfirm();

  const teamSecretsQuery = useQuery({
    queryKey: ["team-secrets", teamID],
    queryFn: () => listTeamSecrets(teamID),
  });
  // Personal secrets are best-effort: a failure just renders an empty
  // list (the team list is the tab's primary content).
  const mySecretsQuery = useQuery({
    queryKey: ["my-secrets"],
    queryFn: () => listMySecrets().catch(() => [] as GenericSecretView[]),
  });
  const teamSecrets = teamSecretsQuery.data ?? [];
  const mySecrets = mySecretsQuery.data ?? [];
  // isFetching (not isLoading): the pre-query code skeletoned both tables
  // on every reload, including post-mutation refreshes.
  const loading = teamSecretsQuery.isFetching || mySecretsQuery.isFetching;
  const unavailable = teamSecretsQuery.error instanceof FeatureUnavailableError;
  const err =
    actionErr ??
    (teamSecretsQuery.error && !unavailable && !loading
      ? errorMessage(teamSecretsQuery.error)
      : null);

  const reload = () => {
    setActionErr(null);
    void queryClient.invalidateQueries({ queryKey: ["team-secrets", teamID] });
    void queryClient.invalidateQueries({ queryKey: ["my-secrets"] });
  };

  const doDelete = async (scope: "team" | "me", rec: GenericSecretView) => {
    const ok = await confirm({
      title: `Delete ${rec.name}?`,
      message:
        "Bot bindings that reference this secret will stop resolving immediately. Workflows that need it will fail until you add a new secret with the same workflow name.",
      confirmLabel: "Delete",
      confirmVariant: "danger",
    });
    if (!ok) return;
    try {
      if (scope === "team") await deleteTeamSecret(teamID, rec.id);
      else await deleteMySecret(rec.id);
      reload();
    } catch (e) {
      setActionErr(errorMessage(e));
    }
  };

  if (unavailable) {
    return (
      <EmptyState
        title="Secrets not enabled on this server"
        message="Generic secrets require a multi-tenant deployment."
      />
    );
  }

  return (
    <div className="space-y-6">
      {err && (
        <InlineBanner tone="danger" layout="inline">
          {err}
        </InlineBanner>
      )}

      <p className="text-caption text-fg-subtle -mb-2">
        Values injected into bots via{" "}
        <button
          type="button"
          className="text-accent-text hover:underline"
          onClick={() => navigate("/integrations?tab=bindings")}
        >
          Bot bindings
        </button>
        .
      </p>

      <section>
        <SecretsSectionHeader
          title="Team secrets"
          description="Org-wide credentials available to every bot the org runs. Admin-managed."
          onCreate={canManage ? () => setCreating("team") : undefined}
        />
        <SecretsTable
          caption="Team secrets"
          secrets={teamSecrets}
          loading={loading}
          emptyText={
            canManage
              ? "No team secrets yet. Use them in bot bindings to expose them to a workflow under a chosen name."
              : "No team secrets yet. Ask an admin to add them."
          }
          canManage={canManage}
          onRotate={(rec) => setRotating({ scope: "team", rec })}
          onDelete={(rec) => void doDelete("team", rec)}
        />
      </section>

      <section>
        <SecretsSectionHeader
          title="My secrets"
          description="Personal credentials scoped to your user. Useful when running bots interactively."
          onCreate={() => setCreating("me")}
        />
        <SecretsTable
          caption="My secrets"
          secrets={mySecrets}
          loading={loading}
          emptyText="No personal secrets yet."
          canManage
          onRotate={(rec) => setRotating({ scope: "me", rec })}
          onDelete={(rec) => void doDelete("me", rec)}
        />
      </section>

      {creating && (
        <CreateSecretDialog
          scope={creating}
          teamID={teamID}
          onClose={() => setCreating(null)}
          onCreated={() => {
            setCreating(null);
            reload();
          }}
        />
      )}

      {rotating && (
        <RotateSecretDialog
          scope={rotating.scope}
          teamID={teamID}
          rec={rotating.rec}
          onClose={() => setRotating(null)}
          onRotated={() => {
            setRotating(null);
            reload();
          }}
        />
      )}

      {dialog}
    </div>
  );
}

function SecretsSectionHeader({
  title,
  description,
  onCreate,
}: {
  title: string;
  description: string;
  onCreate?: () => void;
}) {
  return (
    <div className="flex items-start justify-between mb-2">
      <div>
        <h3 className="font-medium">{title}</h3>
        <p className="text-xs text-fg-subtle mt-0.5">{description}</p>
      </div>
      {onCreate && (
        <Button size="sm" variant="primary" onClick={onCreate}>
          Add secret
        </Button>
      )}
    </div>
  );
}

function SecretsTable({
  caption,
  secrets,
  loading,
  emptyText,
  canManage,
  onRotate,
  onDelete,
}: {
  caption: string;
  secrets: GenericSecretView[];
  loading: boolean;
  emptyText: string;
  canManage: boolean;
  onRotate: (rec: GenericSecretView) => void;
  onDelete: (rec: GenericSecretView) => void;
}) {
  if (loading) return <TableSkeleton rows={4} cols={6} />;
  if (secrets.length === 0) return <EmptyState message={emptyText} />;
  return (
    <Table caption={caption}>
      <THead>
        <Th>Name</Th>
        <Th>Last4</Th>
        <Th>Fingerprint</Th>
        <Th>Created</Th>
        <Th>Last used</Th>
        <Th align="right">Actions</Th>
      </THead>
      <TBody>
        {secrets.map((s) => (
          <Tr key={s.id}>
            <Td className="font-mono">{s.name}</Td>
            <Td className="font-mono text-fg-muted">…{s.last4 ?? "????"}</Td>
            <Td className="font-mono text-fg-muted text-xs break-all">
              {s.fingerprint ? s.fingerprint.slice(0, 12) : "—"}
            </Td>
            <Td className="text-fg-muted text-xs">
              {formatDateTime(s.created_at)}
            </Td>
            <Td className="text-fg-muted text-xs">
              {formatDateTime(s.last_used_at)}
            </Td>
            <Td align="right" className="space-x-1 whitespace-nowrap">
              {canManage && (
                <>
                  <Button size="sm" variant="ghost" onClick={() => onRotate(s)}>
                    Rotate
                  </Button>
                  <Button
                    size="sm"
                    variant="ghost"
                    className="text-danger"
                    onClick={() => onDelete(s)}
                  >
                    Delete
                  </Button>
                </>
              )}
            </Td>
          </Tr>
        ))}
      </TBody>
    </Table>
  );
}

function CreateSecretDialog({
  scope,
  teamID,
  onClose,
  onCreated,
}: {
  scope: "team" | "me";
  teamID: string;
  onClose: () => void;
  onCreated: () => void;
}) {
  const [name, setName] = useState("");
  const [secret, setSecret] = useState("");
  const { busy, error: err, run } = useAsyncAction();

  const v = isValidSecretName(name);

  const submit = () => {
    if (!v.ok || !secret) return;
    return run(async () => {
      if (scope === "team") await createTeamSecret(teamID, { name, secret });
      else await createMySecret({ name, secret });
      onCreated();
    });
  };

  return (
    <Dialog
      open
      onOpenChange={(v) => {
        if (!v) onClose();
      }}
      title={scope === "team" ? "Add team secret" : "Add personal secret"}
      description="The secret value is stored sealed; only the last4 + fingerprint are returned by the API."
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            loading={busy}
            disabled={!v.ok || !secret}
            onClick={() => void submit()}
          >
            Create
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
        <label className="block">
          <div className="text-xs text-fg-muted mb-1">Name</div>
          <Input
            value={name}
            onChange={(e) => setName(e.target.value)}
            error={name !== "" && !v.ok}
            placeholder="GITLAB_TOKEN"
            autoFocus
          />
          <div className={`text-xs mt-1 ${v.ok ? "text-fg-muted" : "text-danger"}`}>
            {v.ok ? "OK" : v.error ?? "—"}
          </div>
        </label>
        <label className="block">
          <div className="text-xs text-fg-muted mb-1">Secret value</div>
          <Input
            type="password"
            value={secret}
            onChange={(e) => setSecret(e.target.value)}
            placeholder="paste here — never shown again"
          />
        </label>
      </div>
    </Dialog>
  );
}

function RotateSecretDialog({
  scope,
  teamID,
  rec,
  onClose,
  onRotated,
}: {
  scope: "team" | "me";
  teamID: string;
  rec: GenericSecretView;
  onClose: () => void;
  onRotated: () => void;
}) {
  const [secret, setSecret] = useState("");
  const { busy, error: err, run } = useAsyncAction();

  const submit = () => {
    if (!secret) return;
    return run(async () => {
      if (scope === "team") await updateTeamSecret(teamID, rec.id, { secret });
      else await updateMySecret(rec.id, { secret });
      onRotated();
    });
  };

  return (
    <Dialog
      open
      onOpenChange={(v) => {
        if (!v) onClose();
      }}
      title={`Rotate ${rec.name}`}
      description="The previous value is replaced atomically. Workflows currently running continue with the old value until they finish."
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" loading={busy} disabled={!secret} onClick={() => void submit()}>
            Rotate
          </Button>
        </>
      }
    >
      {err && (
        <InlineBanner tone="danger" layout="inline" className="mb-3">
          {err}
        </InlineBanner>
      )}
      <label className="block text-sm">
        <div className="text-xs text-fg-muted mb-1">New value</div>
        <Input
          type="password"
          value={secret}
          onChange={(e) => setSecret(e.target.value)}
          placeholder="paste new value"
          autoFocus
        />
      </label>
    </Dialog>
  );
}
