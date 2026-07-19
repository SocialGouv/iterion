import { errorMessage } from "@/lib/errorHints";
import { formatDateTime } from "@/lib/format";
import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { InlineBanner } from "@/components/ui/InlineBanner";

import {
  type PersonalAccessToken,
  FeatureUnavailableError,
  createMyToken,
  listMyTokens,
  revokeMyToken,
} from "@/api/pats";
import { useAuth } from "@/auth/AuthContext";

import { Button } from "@/components/ui/Button";
import { CopyButton } from "@/components/ui/CopyButton";
import { Dialog } from "@/components/ui/Dialog";
import { EmptyState } from "@/components/ui/EmptyState";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";
import { Table, THead, Th, TBody, Tr, Td, TableSkeleton } from "@/components/ui/Table";
import ConfirmDialog from "@/components/shared/ConfirmDialog";
import { useAsyncAction } from "@/hooks/useAsyncAction";

export default function TokensPanel() {
  const { teams } = useAuth();
  const queryClient = useQueryClient();
  const query = useQuery<PersonalAccessToken[]>({
    queryKey: ["my-pats"],
    queryFn: () => listMyTokens(),
  });
  const tokens = query.data ?? [];
  // The manual reload skeletoned on every pass (create/revoke included) —
  // isFetching keeps that visible.
  const loading = query.isFetching;
  const unavailable = query.error instanceof FeatureUnavailableError;
  // Mutation failures share the banner with the fetch error (mutation wins,
  // like the old single slot); the fetch error hides while a reload is in
  // flight, which the manual reload achieved by clearing it up front.
  const [mutErr, setMutErr] = useState<string | null>(null);
  const err =
    mutErr ??
    (query.error && !unavailable && !loading ? errorMessage(query.error) : null);
  const [creating, setCreating] = useState(false);
  const [issued, setIssued] = useState<{ pat: PersonalAccessToken; token: string } | null>(null);
  const [deleting, setDeleting] = useState<PersonalAccessToken | null>(null);

  // Post-mutation refresh: clear the shared error slot and refetch the list.
  const reload = () => {
    setMutErr(null);
    void queryClient.invalidateQueries({ queryKey: ["my-pats"] });
  };

  // Tokens store only the team_id — resolve it to the friendly team name the
  // create dialog shows, falling back to the raw id for a team this account
  // no longer sees.
  const teamNameByID = new Map(teams.map((t) => [t.team_id, t.team_name]));

  const doDelete = async () => {
    if (!deleting) return;
    try {
      await revokeMyToken(deleting.id);
      setDeleting(null);
      reload();
    } catch (e) {
      setMutErr(errorMessage(e));
    }
  };

  if (unavailable) {
    return (
      <EmptyState
        title="Personal access tokens not enabled"
        message="The PAT service requires a cloud-mode server with the pat store wired."
      />
    );
  }

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-lg font-semibold">Personal access tokens</h2>
          <p className="text-xs text-fg-subtle mt-0.5">
            Long-lived tokens for the iterion CLI / SDK. Inherits your role at the active team.
          </p>
        </div>
        <Button size="sm" variant="primary" onClick={() => setCreating(true)}>
          New token
        </Button>
      </div>

      {err && (
        <InlineBanner tone="danger" layout="inline">
          {err}
        </InlineBanner>
      )}

      {loading ? (
        <TableSkeleton rows={4} cols={7} />
      ) : tokens.length === 0 ? (
        <EmptyState message="No tokens yet." />
      ) : (
        <Table caption="Personal access tokens">
          <THead>
            <Th>Name</Th>
            <Th>Team</Th>
            <Th>Last4</Th>
            <Th>Created</Th>
            <Th>Expires</Th>
            <Th>Last used</Th>
            <Th align="right">Actions</Th>
          </THead>
          <TBody>
            {tokens.map((t) => (
              <Tr key={t.id}>
                <Td>{t.name}</Td>
                <Td className="text-xs text-fg-muted">
                  {t.team_id ? (teamNameByID.get(t.team_id) ?? t.team_id) : "(default)"}
                </Td>
                <Td className="font-mono text-xs text-fg-muted">…{t.token_last4}</Td>
                <Td className="text-xs text-fg-muted">
                  {formatDateTime(t.created_at)}
                </Td>
                <Td className="text-xs text-fg-muted">
                  {t.expires_at ? formatDateTime(t.expires_at) : "never"}
                </Td>
                <Td className="text-xs text-fg-muted">
                  {formatDateTime(t.last_used_at)}
                </Td>
                <Td align="right">
                  {t.revoked_at ? (
                    <span className="text-xs text-danger">revoked</span>
                  ) : (
                    <Button
                      size="sm"
                      variant="ghost"
                      className="text-danger"
                      onClick={() => setDeleting(t)}
                    >
                      Revoke
                    </Button>
                  )}
                </Td>
              </Tr>
            ))}
          </TBody>
        </Table>
      )}

      {creating && (
        <CreateTokenDialog
          teams={teams.map((t) => ({ id: t.team_id, name: t.team_name }))}
          onClose={() => setCreating(false)}
          onCreated={(r) => {
            setCreating(false);
            setIssued(r);
            reload();
          }}
        />
      )}

      {issued && (
        <Dialog
          open
          onOpenChange={(v) => {
            if (!v) setIssued(null);
          }}
          title={`Token for ${issued.pat.name}`}
          description="Copy now — it cannot be retrieved later."
          footer={
            <Button variant="primary" onClick={() => setIssued(null)}>
              Done — hide token
            </Button>
          }
        >
          <div className="flex items-center gap-2 bg-surface-0 border border-border-subtle rounded p-2 font-mono text-xs break-all">
            <span className="flex-1">{issued.token}</span>
            <CopyButton value={issued.token} variant="icon" />
          </div>
        </Dialog>
      )}

      <ConfirmDialog
        open={deleting !== null}
        title={`Revoke ${deleting?.name ?? ""}?`}
        message="Every connection that uses this token will fail immediately."
        confirmLabel="Revoke"
        confirmVariant="danger"
        onConfirm={() => void doDelete()}
        onCancel={() => setDeleting(null)}
      />
    </div>
  );
}

function CreateTokenDialog({
  teams,
  onClose,
  onCreated,
}: {
  teams: Array<{ id: string; name: string }>;
  onClose: () => void;
  onCreated: (r: { pat: PersonalAccessToken; token: string }) => void;
}) {
  const [name, setName] = useState("");
  const [teamID, setTeamID] = useState("");
  const [days, setDays] = useState<number>(90);
  const { busy, error: err, run } = useAsyncAction();

  const submit = () => {
    if (!name.trim()) return;
    return run(async () => {
      const r = await createMyToken({
        name: name.trim(),
        team_id: teamID || undefined,
        expires_in_days: days > 0 ? days : undefined,
      });
      onCreated(r);
    });
  };

  return (
    <Dialog
      open
      onOpenChange={(v) => {
        if (!v) onClose();
      }}
      title="New personal access token"
      description="The plaintext is shown ONCE on create."
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" loading={busy} disabled={!name.trim()} onClick={() => void submit()}>
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
            placeholder="CI bot"
            autoFocus
          />
        </label>
        <label className="block">
          <div className="text-xs text-fg-muted mb-1">Pin to a team (optional)</div>
          <Select value={teamID} onChange={(e) => setTeamID(e.target.value)}>
            <option value="">— default team —</option>
            {teams.map((t) => (
              <option key={t.id} value={t.id}>
                {t.name}
              </option>
            ))}
          </Select>
        </label>
        <label className="block">
          <div className="text-xs text-fg-muted mb-1">Expires in (days, 0 = no expiry)</div>
          <Input
            type="number"
            min={0}
            value={String(days)}
            onChange={(e) => setDays(Number(e.target.value))}
          />
        </label>
      </div>
    </Dialog>
  );
}
