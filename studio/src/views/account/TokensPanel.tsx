import { errorMessage } from "@/lib/errorHints";
import { formatDateTime } from "@/lib/format";
import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { InlineBanner } from "@/components/ui/InlineBanner";

import {
  type PersonalAccessToken,
  type TokenRevocation,
  type TokenStatus,
  FeatureUnavailableError,
  createMyToken,
  disableMyToken,
  enableMyToken,
  listMyTokens,
  purgeMyToken,
  regenerateMyToken,
  revokeMyToken,
  tokenStatus,
} from "@/api/pats";
import { useAuth } from "@/auth/AuthContext";

import { Badge, type BadgeVariant } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { Checkbox } from "@/components/ui/Checkbox";
import { CopyButton } from "@/components/ui/CopyButton";
import { Dialog } from "@/components/ui/Dialog";
import { EmptyState } from "@/components/ui/EmptyState";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";
import { Table, THead, Th, TBody, Tr, Td, TableSkeleton } from "@/components/ui/Table";
import ConfirmDialog from "@/components/shared/ConfirmDialog";
import { useAsyncAction } from "@/hooks/useAsyncAction";

const statusBadge: Record<TokenStatus, BadgeVariant> = {
  active: "success",
  disabled: "warning",
  expired: "neutral",
  revoked: "danger",
};

// The acts that end a token (or its row) ask first; each names what it ends.
type Confirm = { kind: "revoke" | "regenerate" | "purge"; token: PersonalAccessToken };

const confirmCopy: Record<
  Confirm["kind"],
  { title: string; message: string; label: string }
> = {
  revoke: {
    title: "Revoke",
    message:
      "Every connection that uses this token will fail immediately, and the pending invitations it sent by email end with it.",
    label: "Revoke",
  },
  regenerate: {
    title: "Regenerate",
    message:
      "A new token with the same name, team and expiry replaces this one, shown once. This token stops working immediately, and the pending invitations it sent by email end with it.",
    label: "Regenerate",
  },
  purge: {
    title: "Delete",
    message: "The revoked token leaves this list for good. Its audit trail stays.",
    label: "Delete",
  },
};

export default function TokensPanel() {
  const { teams } = useAuth();
  const queryClient = useQueryClient();
  // Revoked and expired tokens never authenticate again: the server lists
  // them on request only.
  const [showRevoked, setShowRevoked] = useState(false);
  const query = useQuery<PersonalAccessToken[]>({
    queryKey: ["my-pats", showRevoked],
    queryFn: () => listMyTokens({ includeRevoked: showRevoked }),
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
  // A freshly minted plaintext — from a create or a regenerate — shown once.
  // revokeError is set when a regenerate could not revoke the token it
  // replaced: that one still authenticates.
  const [issued, setIssued] = useState<{
    pat: PersonalAccessToken;
    token: string;
    revokeError?: string;
  } | null>(null);
  const [confirming, setConfirming] = useState<Confirm | null>(null);
  // The confirmed act is in flight: the confirm button takes no second click
  // (a second regenerate would be refused, a second revoke is wasted).
  const [acting, setActing] = useState(false);
  // The last act that ended invitations, so the operator sees who to invite
  // again: a revoke, a regenerate or a delete is never a silent end of them.
  const [ended, setEnded] = useState<{
    verb: string;
    name: string;
    revocation: TokenRevocation;
  } | null>(null);

  // Post-mutation refresh: clear the shared error slot and refetch the list.
  const reload = () => {
    setMutErr(null);
    void queryClient.invalidateQueries({ queryKey: ["my-pats"] });
  };

  // Tokens store only the team_id — resolve it to the friendly team name the
  // create dialog shows, falling back to the raw id for a team this account
  // no longer sees.
  const teamNameByID = new Map(teams.map((t) => [t.team_id, t.team_name]));

  // A sweep that ended nothing leaves an earlier list up until it is
  // dismissed: the operator may not have noted who to invite again yet.
  const noteEnded = (verb: string, name: string, revocation: TokenRevocation) => {
    if (
      revocation.invitations_revoked.length > 0 ||
      (revocation.invitations_revoked_elsewhere ?? 0) > 0 ||
      revocation.invitations_error
    ) {
      setEnded({ verb, name, revocation });
    }
  };

  const doConfirmed = async () => {
    if (!confirming) return;
    const { kind, token } = confirming;
    setActing(true);
    try {
      if (kind === "revoke") {
        noteEnded("Revoking", token.name, await revokeMyToken(token.id));
      } else if (kind === "purge") {
        noteEnded("Deleting", token.name, await purgeMyToken(token.id));
      } else {
        const r = await regenerateMyToken(token.id);
        setIssued({ pat: r.pat, token: r.token, revokeError: r.revoke_error });
        noteEnded("Regenerating", token.name, {
          invitations_revoked: r.invitations_revoked,
          invitations_revoked_elsewhere: r.invitations_revoked_elsewhere,
          invitations_error: r.invitations_error,
        });
      }
      setConfirming(null);
      reload();
    } catch (e) {
      setConfirming(null);
      setMutErr(errorMessage(e));
    } finally {
      setActing(false);
    }
  };

  const toggleDisabled = async (t: PersonalAccessToken) => {
    try {
      if (t.disabled_at) await enableMyToken(t.id);
      else await disableMyToken(t.id);
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

  const copy = confirming ? confirmCopy[confirming.kind] : null;
  const endedElsewhere = ended?.revocation.invitations_revoked_elsewhere ?? 0;

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-lg font-semibold">Personal access tokens</h2>
          <p className="text-xs text-fg-subtle mt-0.5">
            Long-lived tokens for the iterion CLI / SDK. Inherits your role at the active team.
          </p>
        </div>
        <div className="flex items-center gap-3">
          <Checkbox
            label="Show revoked"
            checked={showRevoked}
            onChange={(e) => setShowRevoked(e.target.checked)}
          />
          <Button size="sm" variant="primary" onClick={() => setCreating(true)}>
            New token
          </Button>
        </div>
      </div>

      {err && (
        <InlineBanner tone="danger" layout="inline">
          {err}
        </InlineBanner>
      )}

      {ended && (
        <InlineBanner
          tone="warning"
          layout="inline"
          title={
            ended.revocation.invitations_revoked.length > 0 || endedElsewhere > 0
              ? `${ended.verb} ${ended.name} ended the invitations it sent`
              : `${ended.verb} ${ended.name} could not end every invitation it sent`
          }
          onDismiss={() => setEnded(null)}
        >
          {ended.revocation.invitations_revoked.length > 0 && (
            <>
              <p>Invite them again:</p>
              <ul className="list-disc pl-5">
                {ended.revocation.invitations_revoked.map((inv) => (
                  <li key={inv.id}>
                    {inv.email} — {teamNameByID.get(inv.team_id) ?? inv.team_id}, {inv.role}
                  </li>
                ))}
              </ul>
            </>
          )}
          {endedElsewhere > 0 && (
            <p>
              {endedElsewhere} more in teams this session does not reach: their team&apos;s audit
              log names them.
            </p>
          )}
          {ended.revocation.invitations_error && <p>{ended.revocation.invitations_error}</p>}
        </InlineBanner>
      )}

      {loading ? (
        <TableSkeleton rows={4} cols={8} />
      ) : tokens.length === 0 ? (
        <EmptyState message="No tokens yet." />
      ) : (
        <Table caption="Personal access tokens">
          <THead>
            <Th>Name</Th>
            <Th>Team</Th>
            <Th>Last4</Th>
            <Th>Status</Th>
            <Th>Created</Th>
            <Th>Expires</Th>
            <Th>Last used</Th>
            <Th align="right">Actions</Th>
          </THead>
          <TBody>
            {tokens.map((t) => {
              const status = tokenStatus(t);
              return (
                <Tr key={t.id}>
                  <Td>{t.name}</Td>
                  <Td className="text-xs text-fg-muted">
                    {t.team_id ? (teamNameByID.get(t.team_id) ?? t.team_id) : "(default)"}
                  </Td>
                  <Td className="font-mono text-xs text-fg-muted">…{t.token_last4}</Td>
                  <Td>
                    <Badge variant={statusBadge[status]}>{status}</Badge>
                  </Td>
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
                    <div className="flex justify-end gap-1">
                      {(status === "active" || status === "disabled") && (
                        <>
                          <Button
                            size="sm"
                            variant="ghost"
                            aria-label={`Regenerate ${t.name}`}
                            onClick={() => setConfirming({ kind: "regenerate", token: t })}
                          >
                            Regenerate
                          </Button>
                          <Button
                            size="sm"
                            variant="ghost"
                            aria-label={`${status === "disabled" ? "Enable" : "Disable"} ${t.name}`}
                            onClick={() => void toggleDisabled(t)}
                          >
                            {status === "disabled" ? "Enable" : "Disable"}
                          </Button>
                        </>
                      )}
                      {status === "revoked" ? (
                        <Button
                          size="sm"
                          variant="ghost"
                          className="text-danger"
                          aria-label={`Delete ${t.name}`}
                          onClick={() => setConfirming({ kind: "purge", token: t })}
                        >
                          Delete
                        </Button>
                      ) : (
                        <Button
                          size="sm"
                          variant="ghost"
                          className="text-danger"
                          aria-label={`Revoke ${t.name}`}
                          onClick={() => setConfirming({ kind: "revoke", token: t })}
                        >
                          Revoke
                        </Button>
                      )}
                    </div>
                  </Td>
                </Tr>
              );
            })}
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
          {issued.revokeError && (
            <InlineBanner tone="danger" layout="inline" className="mb-3">
              {issued.revokeError}
            </InlineBanner>
          )}
          <div className="flex items-center gap-2 bg-surface-0 border border-border-subtle rounded p-2 font-mono text-xs break-all">
            <span className="flex-1">{issued.token}</span>
            <CopyButton value={issued.token} variant="icon" />
          </div>
        </Dialog>
      )}

      <ConfirmDialog
        open={confirming !== null}
        title={`${copy?.title ?? ""} ${confirming?.token.name ?? ""}?`}
        message={copy?.message ?? ""}
        confirmLabel={copy?.label ?? "Confirm"}
        confirmVariant="danger"
        busy={acting}
        onConfirm={() => void doConfirmed()}
        onCancel={() => setConfirming(null)}
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
