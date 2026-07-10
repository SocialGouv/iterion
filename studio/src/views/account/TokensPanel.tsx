import { errorMessage } from "@/lib/errorHints";
import { useEffect, useState } from "react";
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
import ConfirmDialog from "@/components/shared/ConfirmDialog";
import { useAsyncAction } from "@/hooks/useAsyncAction";

export default function TokensPanel() {
  const { teams } = useAuth();
  const [tokens, setTokens] = useState<PersonalAccessToken[]>([]);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState<string | null>(null);
  const [unavailable, setUnavailable] = useState(false);
  const [creating, setCreating] = useState(false);
  const [issued, setIssued] = useState<{ pat: PersonalAccessToken; token: string } | null>(null);
  const [deleting, setDeleting] = useState<PersonalAccessToken | null>(null);

  const reload = async () => {
    setLoading(true);
    setErr(null);
    try {
      setTokens(await listMyTokens());
      setUnavailable(false);
    } catch (e) {
      if (e instanceof FeatureUnavailableError) setUnavailable(true);
      else setErr(errorMessage(e));
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    void reload();
  }, []);

  const doDelete = async () => {
    if (!deleting) return;
    try {
      await revokeMyToken(deleting.id);
      setDeleting(null);
      void reload();
    } catch (e) {
      setErr(errorMessage(e));
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
        <EmptyState message="Loading…" />
      ) : tokens.length === 0 ? (
        <EmptyState message="No tokens yet." />
      ) : (
        <div className="overflow-x-auto"><table className="w-full text-sm">
          <thead className="text-xs uppercase tracking-wider text-fg-muted text-left">
            <tr>
              <th className="px-2 py-1">Name</th>
              <th className="px-2 py-1">Team</th>
              <th className="px-2 py-1">Last4</th>
              <th className="px-2 py-1">Created</th>
              <th className="px-2 py-1">Expires</th>
              <th className="px-2 py-1">Last used</th>
              <th className="px-2 py-1 text-right">Actions</th>
            </tr>
          </thead>
          <tbody>
            {tokens.map((t) => (
              <tr key={t.id} className="border-t border-border-subtle">
                <td className="px-2 py-2">{t.name}</td>
                <td className="px-2 py-2 text-xs text-fg-muted">
                  {t.team_id ?? "(default)"}
                </td>
                <td className="px-2 py-2 font-mono text-xs text-fg-muted">…{t.token_last4}</td>
                <td className="px-2 py-2 text-xs text-fg-muted">
                  {new Date(t.created_at).toLocaleString()}
                </td>
                <td className="px-2 py-2 text-xs text-fg-muted">
                  {t.expires_at ? new Date(t.expires_at).toLocaleString() : "never"}
                </td>
                <td className="px-2 py-2 text-xs text-fg-muted">
                  {t.last_used_at ? new Date(t.last_used_at).toLocaleString() : "—"}
                </td>
                <td className="px-2 py-2 text-right">
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
                </td>
              </tr>
            ))}
          </tbody>
        </table></div>
      )}

      {creating && (
        <CreateTokenDialog
          teams={teams.map((t) => ({ id: t.team_id, name: t.team_name }))}
          onClose={() => setCreating(false)}
          onCreated={(r) => {
            setCreating(false);
            setIssued(r);
            void reload();
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
